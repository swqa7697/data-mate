package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
	"github.com/swqa7697/data-mate/internal/database/postgres"
	relay "github.com/swqa7697/data-mate/internal/mcp"
	"golang.org/x/sys/unix"
)

type sessionDriver struct {
	database.Driver
	entered chan struct{}
	exited  chan struct{}
}

func (d *sessionDriver) Query(ctx context.Context, a database.Access, q database.QueryRequest) (database.QueryResult, error) {
	if q.SQL == "wait" {
		d.entered <- struct{}{}
		<-ctx.Done()
		d.exited <- struct{}{}
		return database.QueryResult{}, ctx.Err()
	}
	if q.SQL == "full" {
		d.entered <- struct{}{}
		return database.QueryResult{Connection: a.Profile.Alias, Columns: []database.ResultColumn{{Name: "value", Type: "text"}}, Rows: [][]any{{strings.Repeat("x", 200000)}}, RowCount: 1}, nil
	}
	if q.SQL == "large" {
		return database.QueryResult{Connection: a.Profile.Alias, Columns: []database.ResultColumn{{Name: "value", Type: "text"}}, Rows: [][]any{{strings.Repeat("\\\"", 180000)}}, RowCount: 1}, nil
	}
	if q.SQL == "upstream" {
		return database.QueryResult{}, errors.New("synthetic-password postgres://secret SQL detail")
	}
	return d.Driver.Query(ctx, a, q)
}
func sessionClient(t *testing.T, c *Controller) (*sdk.ClientSession, *net.UnixConn) {
	t.Helper()
	conn, err := c.OpenSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "fixture", Version: "1"}, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	session, err := client.Connect(ctx, &sdk.IOTransport{Reader: conn, Writer: conn}, nil)
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session, conn
}
func callTool(t *testing.T, s *sdk.ClientSession, name string, args any) (*sdk.CallToolResult, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	r, err := s.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(r.StructuredContent)
	if err != nil || len(r.Content) != 1 {
		t.Fatal("missing compatibility content", err)
	}
	text, ok := r.Content[0].(*sdk.TextContent)
	if !ok || !bytes.Equal(b, compactJSON(t, text.Text)) {
		t.Fatal("content representations differ")
	}
	return r, b
}
func compactJSON(t *testing.T, s string) []byte {
	t.Helper()
	var value any
	if err := json.Unmarshal([]byte(s), &value); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(value)
	return b
}
func awaitSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("operation did not reach expected state")
	}
}

// Regression ladder 3: lifecycle/admission tests have no initialized SDK peers,
// wire dispatch, cancellation or relay. This scenario owns that new boundary.
func TestMCPSessions(t *testing.T) {
	c, f, store := controllerFixture(t)
	pg, err := postgres.New()
	if err != nil {
		t.Fatal(err)
	}
	d := &sessionDriver{Driver: pg, entered: make(chan struct{}, 16), exited: make(chan struct{}, 16)}
	f.driver = d
	p := fixtureProfile()
	p.Connection.Host = "synthetic-host"
	p.Connection.Username = "synthetic-username"
	putProfiles(t, store, config.Profiles{Version: 1, Connections: []config.Profile{p}})
	if _, err = c.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	a, _ := sessionClient(t, c)
	b, _ := sessionClient(t, c)
	listed, err := a.ListTools(t.Context(), nil)
	if err != nil || len(listed.Tools) != 6 {
		t.Fatal("tools", err)
	}
	names := map[string]bool{}
	for _, tool := range listed.Tools {
		names[tool.Name] = true
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Fatal("missing read-only annotation")
		}
		raw, _ := json.Marshal(tool.InputSchema)
		var schema map[string]any
		_ = json.Unmarshal(raw, &schema)
		if schema["additionalProperties"] != false || tool.OutputSchema == nil {
			t.Fatal("loose schema")
		}
	}
	for _, name := range []string{"list_connections", "list_tables", "describe_table", "list_objects", "describe_object", "query"} {
		if !names[name] {
			t.Fatal("missing tool", name)
		}
	}
	r, raw := callTool(t, a, "list_connections", map[string]any{})
	if r.IsError || contracts.Validate("list_connections.output", raw) != nil || bytes.Contains(raw, []byte("synthetic-host")) || bytes.Contains(raw, []byte("synthetic-username")) || bytes.Contains(raw, []byte(p.ID)) {
		t.Fatal("connection redaction")
	}
	for _, args := range []map[string]any{{"connection": "fixture", "sql": "select 1", "password": "synthetic-password"}, {"connection": "fixture", "sql": "select 1", "row_limit": 0}} {
		_, err := a.CallTool(t.Context(), &sdk.CallToolParams{Name: "query", Arguments: args})
		if err == nil || strings.Contains(err.Error(), "synthetic-password") {
			t.Fatal("invalid input diagnostic", err)
		}
	}
	for _, test := range []struct {
		sql, alias string
		code       contracts.Code
	}{{"delete from app.items", "fixture", contracts.ReadOnlyViolation}, {"select 1", "missing", contracts.ConnectionNotFound}, {"large", "fixture", contracts.ResourceLimit}, {"upstream", "fixture", contracts.ServiceUnavailable}} {
		r, raw = callTool(t, a, "query", map[string]any{"connection": test.alias, "sql": test.sql})
		var body struct {
			Error contracts.Failure `json:"error"`
		}
		_ = json.Unmarshal(raw, &body)
		if !r.IsError || body.Error.Code != test.code || bytes.Contains(raw, []byte("synthetic-password")) {
			t.Fatalf("failure propagation %s: %s", test.sql, raw)
		}
	}
	// SDK cancellation releases work; a second session continues independently.
	cancelCtx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := a.CallTool(cancelCtx, &sdk.CallToolParams{Name: "query", Arguments: map[string]any{"connection": "fixture", "sql": "wait"}})
		done <- err
	}()
	awaitSignal(t, d.entered)
	callTool(t, b, "list_connections", map[string]any{})
	cancel()
	if err := <-done; err == nil {
		t.Fatal("cancelled call succeeded")
	}
	awaitSignal(t, d.exited)
	// EOF cancels an active operation even when the client did not notify.
	disconnected, wire := sessionClient(t, c)
	go func() {
		_, _ = disconnected.CallTool(t.Context(), &sdk.CallToolParams{Name: "query", Arguments: map[string]any{"connection": "fixture", "sql": "wait"}})
	}()
	awaitSignal(t, d.entered)
	wire.Close()
	awaitSignal(t, d.exited)
	// Sixteen accepted calls consume the session; the seventeenth closes it promptly.
	saturated, _ := sessionClient(t, c)
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			_, _ = saturated.CallTool(t.Context(), &sdk.CallToolParams{Name: "query", Arguments: map[string]any{"connection": "fixture", "sql": "wait"}})
		})
	}
	for range 16 {
		awaitSignal(t, d.entered)
	}
	_, err = saturated.CallTool(t.Context(), &sdk.CallToolParams{Name: "list_connections", Arguments: map[string]any{}})
	if err == nil {
		t.Fatal("seventeenth call admitted")
	}
	wg.Wait()
	for range 16 {
		awaitSignal(t, d.exited)
	}
	callTool(t, b, "list_connections", map[string]any{})
	// Invalid peers never reach a handler or poison healthy sessions.
	for _, bad := range []string{"{broken}\n", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n", `{"jsonrpc":"2.0","id":1,"id":2,"method":"initialize"}` + "\n", strings.Repeat("x", (256<<10)+1) + "\n"} {
		conn, err := c.OpenSession(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		_, _ = io.WriteString(conn, bad)
		var b [1]byte
		_, err = conn.Read(b[:])
		conn.Close()
		var timeout net.Error
		if err == nil || errors.As(err, &timeout) && timeout.Timeout() {
			t.Fatal("invalid frame not closed", err)
		}
	}
	// Real stdio pipes ensure the preamble stays off stdout and EOF joins relay work.
	conn, err := c.OpenSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	input, writer := io.Pipe()
	reader, output := io.Pipe()
	bridged := make(chan error, 1)
	go func() { bridged <- relay.Bridge(t.Context(), conn, input, output) }()
	client := sdk.NewClient(&sdk.Implementation{Name: "bridge", Version: "1"}, nil)
	session, err := client.Connect(t.Context(), &sdk.IOTransport{Reader: reader, Writer: writer}, nil)
	if err != nil {
		t.Fatal("bridge protocol stdout", err)
	}
	callTool(t, session, "list_connections", map[string]any{})
	session.Close()
	select {
	case <-bridged:
	case <-time.After(3 * time.Second):
		t.Fatal("bridge EOF leaked")
	}
	sessionDeadlines(t, store, d)
}

// Extend the same owning wire scenario with real production deadlines. net.Pipe
// provides deterministic backpressure without depending on kernel buffer sizes.
func sessionDeadlines(t *testing.T, store *config.Store, d *sessionDriver) {
	t.Helper()
	m := newManager(t.Context(), store, noKeys{}, d)
	defer m.Close()
	if err := m.initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	serve := func() (net.Conn, <-chan error) {
		peer, server := net.Pipe()
		done := make(chan error, 1)
		go func() { done <- relay.Serve(t.Context(), server, m, "fixture") }()
		t.Cleanup(func() { peer.Close() })
		return peer, done
	}
	idle, idleDone := serve()
	defer idle.Close()

	type blocked struct {
		done       <-chan error
		serverDone <-chan error
	}
	var blockedPeers []blocked
	for _, bridge := range []bool{false, true} {
		socket, done := serve()
		var peer io.ReadWriter = socket
		var serverDone <-chan error

		if bridge {
			input, stdin := io.Pipe()
			stdout, output, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			// Reproduce an inherited blocking stdout, not an os.Pipe poller.
			fd, err := unix.Dup(int(output.Fd()))
			if err != nil {
				t.Fatal(err)
			}
			output.Close()
			inherited := os.NewFile(uintptr(fd), "fixture-stdout")
			bridgeDone := make(chan error, 1)
			go func() { bridgeDone <- relay.Bridge(t.Context(), socket, input, inherited) }()
			peer = struct {
				io.Reader
				io.Writer
			}{stdout, stdin}
			serverDone = done
			done = bridgeDone
			t.Cleanup(func() { stdout.Close(); stdin.Close() })
		} else {
			_ = socket.SetDeadline(time.Now().Add(12 * time.Second))
		}

		_, err := io.WriteString(peer, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"fixture","version":"1"}}}`+"\n")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := bufio.NewReader(peer).ReadBytes('\n'); err != nil {
			t.Fatal(err)
		}
		_, err = io.WriteString(peer, `{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`+"\n"+`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"query","arguments":{"connection":"fixture","sql":"full"}}}`+"\n")
		if err != nil {
			t.Fatal(err)
		}
		awaitSignal(t, d.entered)
		// Query has entered with the shared lease. A writer must acquire it even
		// though neither the socket peer nor the bridge stdout consumes the result.
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		lease, err := store.WriteLease(ctx)
		cancel()
		if err != nil {
			t.Fatal("slow output retained state lease", err)
		}
		lease.Release()
		blockedPeers = append(blockedPeers, blocked{done, serverDone})
	}
	for _, p := range blockedPeers {
		select {
		case <-p.done:
		case <-time.After(7 * time.Second):
			t.Fatal("stalled output session survived")
		}
		if p.serverDone != nil {
			select {
			case <-p.serverDone:
			case <-time.After(time.Second):
				t.Fatal("bridge server survived disconnect")
			}
		}
	}
	select {
	case <-idleDone:
	case <-time.After(7 * time.Second):
		t.Fatal("uninitialized session survived")
	}
}
