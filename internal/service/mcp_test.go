package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net"
	"os"
	"path/filepath"
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
	release chan struct{}
}

func (d *sessionDriver) ListTables(ctx context.Context, a database.Access, q database.PageRequest) (database.TablePage, error) {
	if q.Schema == "fault" {
		return database.TablePage{}, database.Fail(contracts.ResourceLimit, "synthetic metadata failure", true)
	}
	return d.Driver.ListTables(ctx, a, q)
}
func (d *sessionDriver) ListObjects(ctx context.Context, a database.Access, q database.ObjectPageRequest) (database.ObjectPage, error) {
	if q.Schema == "fault" {
		return database.ObjectPage{}, database.Fail(contracts.StaleCursor, "synthetic metadata failure", false)
	}
	return d.Driver.ListObjects(ctx, a, q)
}
func (d *sessionDriver) DescribeTable(ctx context.Context, a database.Access, q config.Table) (database.Description, error) {
	if q.Name == "fault" {
		return database.Description{}, database.Fail(contracts.PermissionDenied, "synthetic metadata failure", false)
	}
	return d.Driver.DescribeTable(ctx, a, q)
}
func (d *sessionDriver) DescribeObject(ctx context.Context, a database.Access, q database.ObjectRequest) (database.ObjectDescription, error) {
	if q.Name == "fault" {
		return database.ObjectDescription{}, database.Fail(contracts.QueryUnsupported, "synthetic metadata failure", false)
	}
	return d.Driver.DescribeObject(ctx, a, q)
}

func (d *sessionDriver) Query(ctx context.Context, a database.Access, q database.QueryRequest) (database.QueryResult, error) {
	if strings.HasPrefix(q.SQL, "fail:") {
		return database.QueryResult{}, database.Fail(contracts.Code(strings.TrimPrefix(q.SQL, "fail:")), "synthetic failure", true)
	}
	if q.SQL == "panic" {
		panic("synthetic-password postgres://secret")
	}
	if q.SQL == "encode" {
		return database.QueryResult{Rows: [][]any{{math.NaN()}}}, nil
	}
	if q.SQL == "hold" {
		d.entered <- struct{}{}
		select {
		case <-ctx.Done():
			return database.QueryResult{}, ctx.Err()
		case <-d.release:
		}
	}
	if strings.HasPrefix(q.SQL, "select 1") || q.SQL == "hold" {
		return database.QueryResult{Connection: a.Profile.Alias, Columns: []database.ResultColumn{{Name: "value", Type: "int4"}}, Rows: [][]any{{1}}, RowCount: 1}, nil
	}
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
	input, writer := io.Pipe()
	reader, output := io.Pipe()
	bridged := make(chan error, 1)
	go func() { bridged <- relay.Bridge(t.Context(), conn, input, output) }()
	session, err := client.Connect(ctx, &sdk.IOTransport{Reader: reader, Writer: writer, MaxLineLength: -1}, nil)
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		session.Close()
		reader.Close()
		writer.Close()
		conn.Close()
		select {
		case <-bridged:
		case <-time.After(3 * time.Second):
			t.Error("session bridge leaked")
		}
	})
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
	d := &sessionDriver{Driver: pg, entered: make(chan struct{}, 16), exited: make(chan struct{}, 16), release: make(chan struct{})}
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
	if bytes.Contains(raw, []byte("synthetic-host")) || bytes.Contains(raw, []byte("synthetic-username")) || bytes.Contains(raw, []byte(p.ID)) {
		t.Fatal("connection redaction")
	}
	// Ladder 2: extend the live connection-list scenario. Agents must see finite
	// budgets before querying, omit disabled budgets and refresh them on this peer.
	checkLimits := func(result *sdk.CallToolResult, raw []byte, name string, want map[string]int) {
		t.Helper()
		var body struct {
			Connections []struct {
				Limits map[string]int `json:"limits"`
			} `json:"connections"`
		}
		if result.IsError || contracts.Validate("list_connections.output", raw) != nil || json.Unmarshal(raw, &body) != nil || len(body.Connections) != 1 {
			t.Fatalf("%s: invalid connection limits: %.300s", name, raw)
		}
		if got := body.Connections[0].Limits; !maps.Equal(got, want) {
			t.Fatalf("%s: connection limits = %v, want %v", name, got, want)
		}
	}
	checkLimits(r, raw, "defaults", map[string]int{"max_rows": 50000})
	for _, test := range []struct {
		name   string
		limits config.Limits
		want   map[string]int
	}{
		{"bounded", config.Limits{QueryTimeoutMS: 750, MaxRows: 12, MaxResultBytes: 4096}, map[string]int{"max_rows": 12, "max_result_bytes": 4096, "query_timeout_ms": 750}},
		{"timeout removed", config.Limits{MaxRows: 12, MaxResultBytes: 4096}, map[string]int{"max_rows": 12, "max_result_bytes": 4096}},
		{"bytes removed", config.Limits{QueryTimeoutMS: 750, MaxRows: 12}, map[string]int{"max_rows": 12, "query_timeout_ms": 750}},
		{"defaults restored", config.DefaultLimits(), map[string]int{"max_rows": 50000}},
	} {
		p.Limits = &test.limits
		putProfiles(t, store, config.Profiles{Version: 1, Connections: []config.Profile{p}})
		r, raw = callTool(t, a, "list_connections", map[string]any{})
		checkLimits(r, raw, test.name, test.want)
	}
	for _, args := range []map[string]any{{"connection": "fixture", "sql": "select 1", "password": "synthetic-password"}, {"connection": "fixture", "sql": "select 1", "row_limit": 0}} {
		_, err := a.CallTool(t.Context(), &sdk.CallToolParams{Name: "query", Arguments: args})
		if err == nil || strings.Contains(err.Error(), "synthetic-password") {
			t.Fatal("invalid input diagnostic", err)
		}
		callTool(t, a, "query", map[string]any{"connection": "fixture", "sql": "select 1"})
	}
	for _, test := range []struct {
		sql, alias string
		code       contracts.Code
	}{{"delete from app.items", "fixture", contracts.ReadOnlyViolation}, {"select 1", "missing", contracts.ConnectionNotFound}, {"panic", "fixture", contracts.ServiceUnavailable}, {"encode", "fixture", contracts.ServiceUnavailable}, {"upstream", "fixture", contracts.ServiceUnavailable}} {
		r, raw = callTool(t, a, "query", map[string]any{"connection": test.alias, "sql": test.sql})
		var body struct {
			Error contracts.Failure `json:"error"`
		}
		_ = json.Unmarshal(raw, &body)
		if !r.IsError || body.Error.Code != test.code || bytes.Contains(raw, []byte("synthetic-password")) {
			t.Fatalf("failure propagation %s: %.200s", test.sql, raw)
		}
		callTool(t, a, "query", map[string]any{"connection": "fixture", "sql": "select 1"})
	}
	// Ladder 2: each public failure code must leave this same bridged client usable.
	for _, code := range []contracts.Code{contracts.ConfigInvalid, contracts.ConnectionNotFound, contracts.CredentialMissing, contracts.VaultUnavailable, contracts.ConnectFailed, contracts.QueryUnsupported, contracts.QueryTimeout, contracts.ResourceLimit, contracts.ReadOnlyViolation, contracts.PermissionDenied, contracts.QueryFailed, contracts.StaleCursor, contracts.InvalidArgument, contracts.ServiceUnavailable, contracts.Cancelled} {
		r, raw = callTool(t, a, "query", map[string]any{"connection": "fixture", "sql": "fail:" + string(code)})
		var body struct {
			Error contracts.Failure `json:"error"`
		}
		if json.Unmarshal(raw, &body) != nil || !r.IsError || body.Error.Code != code {
			t.Fatalf("lost failure %s: %.200s", code, raw)
		}
		probe, _ := callTool(t, a, "query", map[string]any{"connection": "fixture", "sql": "select 1"})
		if probe.IsError {
			t.Fatal("failure poisoned original session", code)
		}
	}
	// All four metadata dispatch paths release their work on errors, too.
	for _, test := range []struct {
		name string
		args map[string]any
		code contracts.Code
	}{
		{"list_tables", map[string]any{"connection": "fixture", "schema": "fault"}, contracts.ResourceLimit},
		{"list_objects", map[string]any{"connection": "fixture", "schema": "fault"}, contracts.StaleCursor},
		{"describe_table", map[string]any{"connection": "fixture", "schema": "app", "table": "fault"}, contracts.PermissionDenied},
		{"describe_object", map[string]any{"connection": "fixture", "schema": "app", "name": "fault", "kind": "type"}, contracts.QueryUnsupported},
	} {
		r, raw = callTool(t, a, test.name, test.args)
		var body struct {
			Error contracts.Failure `json:"error"`
		}
		if json.Unmarshal(raw, &body) != nil || !r.IsError || body.Error.Code != test.code {
			t.Fatal("metadata failure lost", test.name)
		}
		callTool(t, a, "query", map[string]any{"connection": "fixture", "sql": "select 1"})
	}
	// A genuine broken configuration must recover after repair on this session,
	// including list_connections, without reopening the bridge or service.
	databasePath := filepath.Join(c.Root.Path, "data-mate.db")
	if err := os.Chmod(databasePath, 0644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(databasePath, 0600) })
	r, _ = callTool(t, a, "list_connections", map[string]any{})
	if !r.IsError {
		t.Fatal("invalid configuration accepted")
	}
	if err := os.Chmod(databasePath, 0600); err != nil {
		t.Fatal(err)
	}
	r, _ = callTool(t, a, "list_connections", map[string]any{})
	if r.IsError {
		t.Fatal("configuration repair required a new session")
	}
	callTool(t, a, "query", map[string]any{"connection": "fixture", "sql": "select 1"})
	r, raw = callTool(t, a, "query", map[string]any{"connection": "fixture", "sql": "large"})
	if r.IsError || len(raw) < 1<<19 {
		t.Fatal("large escaped result rejected")
	}
	r, _ = callTool(t, a, "query", map[string]any{"connection": "fixture", "sql": "select 1 /*" + strings.Repeat("x", 1<<20) + "*/"})
	if r.IsError {
		t.Fatal("large inbound query rejected")
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
	callTool(t, a, "query", map[string]any{"connection": "fixture", "sql": "select 1"})
	// EOF cancels an active operation even when the client did not notify.
	disconnected, wire := sessionClient(t, c)
	go func() {
		_, _ = disconnected.CallTool(t.Context(), &sdk.CallToolParams{Name: "query", Arguments: map[string]any{"connection": "fixture", "sql": "wait"}})
	}()
	awaitSignal(t, d.entered)
	wire.Close()
	awaitSignal(t, d.exited)
	// Saturation rejects only the extra call; admitted work and its client survive.
	saturated, _ := sessionClient(t, c)
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			result, err := saturated.CallTool(t.Context(), &sdk.CallToolParams{Name: "query", Arguments: map[string]any{"connection": "fixture", "sql": "hold"}})
			if err != nil || result.IsError {
				t.Error("admitted call interrupted", err)
			}
		})
	}
	for range 16 {
		awaitSignal(t, d.entered)
	}
	r, raw = callTool(t, saturated, "list_connections", map[string]any{})
	var busy struct {
		Error contracts.Failure `json:"error"`
	}
	if json.Unmarshal(raw, &busy) != nil || !r.IsError || busy.Error.Code != contracts.ResourceLimit || !busy.Error.Retryable {
		t.Fatal("seventeenth call was not rejected safely")
	}
	close(d.release)
	wg.Wait()
	callTool(t, saturated, "query", map[string]any{"connection": "fixture", "sql": "select 1"})
	callTool(t, b, "list_connections", map[string]any{})
	sessionProtocolRecovery(t, c, d)
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

// Extend the owning wire scenario with malformed frames and protocol recovery
// through the same bridge. Raw peers can exercise input that SDK clients cannot send.
func sessionProtocolRecovery(t *testing.T, c *Controller, d *sessionDriver) {
	t.Helper()
	socket, err := c.OpenSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	peer, pipes := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- relay.Bridge(t.Context(), socket, pipes, pipes) }()
	defer func() {
		peer.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("raw bridge leaked")
		}
	}()
	reader := bufio.NewReader(peer)
	type reply struct {
		ID     json.RawMessage `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code int64 `json:"code"`
		} `json:"error"`
	}
	send := func(s string) {
		t.Helper()
		_ = peer.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.WriteString(peer, s+"\n"); err != nil {
			t.Fatal(err)
		}
	}
	receive := func() reply {
		t.Helper()
		_ = peer.SetReadDeadline(time.Now().Add(5 * time.Second))
		line, err := reader.ReadBytes('\n')
		if err != nil {
			t.Fatal("same MCP connection became unavailable", err)
		}
		if bytes.Contains(line, []byte("synthetic-password")) || bytes.Contains(line, []byte("postgres://secret")) {
			t.Fatal("protocol diagnostic leaked")
		}
		var r reply
		if json.Unmarshal(line, &r) != nil {
			t.Fatal("invalid MCP response")
		}
		return r
	}
	probe := func(id string) {
		t.Helper()
		send(`{"jsonrpc":"2.0","id":` + id + `,"method":"tools/call","params":{"name":"query","arguments":{"connection":"fixture","sql":"select 1"}}}`)
		r := receive()
		var result struct {
			IsError    bool                 `json:"isError"`
			Structured database.QueryResult `json:"structuredContent"`
		}
		if r.Error != nil || string(r.ID) != id || json.Unmarshal(r.Result, &result) != nil || result.IsError || result.Structured.RowCount != 1 {
			t.Fatal("recovery query failed", string(r.ID), r.Error)
		}
	}
	// Pre-initialization errors must allow a corrected initialize on this peer.
	for _, input := range []string{`{broken}`, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":[]}`} {
		send(input)
		if receive().Error == nil {
			t.Fatal("invalid handshake accepted")
		}
	}
	send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"raw","version":"1"}}}`)
	if receive().Error != nil {
		t.Fatal("corrected initialization failed")
	}
	send(`{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`)
	for _, test := range []struct {
		input string
		code  int64
	}{
		{`{broken}`, -32700},
		{strings.Repeat("x", (256<<10)+1), -32700},
		{`{"jsonrpc":"2.0","id":1,"id":2,"method":"tools/list"}`, -32600},
		{`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"query","arguments":{"connection":"fixture","sql":"select 1","sql":"panic"}}}`, -32600},
		{`[]`, -32600},
		{`{"jsonrpc":"2.0"}`, -32600},
		{`{"jsonrpc":"1.0","id":1,"method":"tools/list"}`, -32600},
		{`{"jsonrpc":"2.0","id":null,"method":"tools/list"}`, -32600},
		{`{"jsonrpc":"2.0","id":true,"method":"tools/list"}`, -32600},
		{`{"jsonrpc":"2.0","id":1.5,"method":"tools/list"}`, -32600},
		{`{"jsonrpc":"2.0","id":1,"method":42}`, -32600},
		{`{"jsonrpc":"2.0","id":1,"method":"unknown"}`, -32601},
		{`{"jsonrpc":"2.0","id":1,"method":"server/discover"}`, -32601},
		{`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`, -32600},
		{`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"query","arguments":{"connection":"fixture","sql":"select 1","row_limit":0}}}`, -32602},
		{`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"unknown","arguments":{}}}`, -32602},
	} {
		send(test.input)
		r := receive()
		if r.Error == nil || r.Error.Code != test.code {
			t.Fatalf("protocol failure %.100s: %+v", test.input, r.Error)
		}
		probe(`"after-error"`)
	}
	// Ignored notifications/responses cannot introduce a reply or change state.
	for _, ignored := range []string{
		`{"jsonrpc":"2.0","method":"notifications/unknown"}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`,
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":true}}`,
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"missing"}}`,
		`{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"ignored"}}`,
	} {
		send(ignored)
		probe(`"after-notification"`)
	}
	// Integer IDs must not round through floating point; string and number IDs
	// that look alike must remain distinct. They may be reused after a response.
	for _, id := range []string{`9007199254740993`, `9223372036854775807`, `"1"`, `1`, `""`, `1`} {
		probe(id)
	}
	send(`{"jsonrpc":"2.0","id":"duplicate","method":"tools/call","params":{"name":"query","arguments":{"connection":"fixture","sql":"wait"}}}`)
	awaitSignal(t, d.entered)
	send(`{"jsonrpc":"2.0","id":"duplicate","method":"tools/call","params":{"name":"query","arguments":{"connection":"fixture","sql":"panic"}}}`)
	send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"duplicate"}}`)
	awaitSignal(t, d.exited)
	if string(receive().ID) != `"duplicate"` {
		t.Fatal("duplicate replaced original request")
	}
	probe(`"duplicate"`)
	// Control saturation is also a per-request error. Cancellation notifications
	// still pass while all request slots are occupied.
	for i := 0; i < 16; i++ {
		send(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"query","arguments":{"connection":"fixture","sql":"wait"}}}`, i))
		awaitSignal(t, d.entered)
	}
	send(`{"jsonrpc":"2.0","id":"busy","method":"tools/list"}`)
	if r := receive(); r.Error == nil || r.Error.Code != -32000 {
		t.Fatal("control saturation disconnected or succeeded")
	}
	for i := 0; i < 16; i++ {
		send(fmt.Sprintf(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":%d}}`, i))
	}
	for i := 0; i < 16; i++ {
		receive()
		awaitSignal(t, d.exited)
	}
	probe(`"after-capacity"`)
}

// Real pipes retain no state lease during backpressure. Explicit transport
// cancellation must release blocked writes and uninitialized peers promptly.
func sessionDeadlines(t *testing.T, store *config.Store, d *sessionDriver) {
	t.Helper()
	m := newManager(t.Context(), store, noKeys{}, d)
	defer m.Close()
	if err := m.initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	serve := func() (net.Conn, <-chan error, context.CancelFunc) {
		peer, server := net.Pipe()
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- relay.Serve(ctx, server, m, "fixture") }()
		t.Cleanup(func() { cancel(); peer.Close() })
		return peer, done, cancel
	}
	idle, idleDone, stopIdle := serve()
	defer idle.Close()
	stopIdle()
	select {
	case <-idleDone:
	case <-time.After(3 * time.Second):
		t.Fatal("idle session did not cancel")
	}
	for _, bridge := range []bool{false, true} {
		socket, serverDone, cancel := serve()
		var peer io.ReadWriter = socket
		done := serverDone
		if bridge {
			input, stdin := io.Pipe()
			stdout, output, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
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
			done = bridgeDone
			t.Cleanup(func() { stdout.Close(); stdin.Close() })
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
		ctx, finish := context.WithTimeout(t.Context(), 2*time.Second)
		lease, err := store.WriteLease(ctx)
		finish()
		if err != nil {
			t.Fatal("slow output retained state lease", err)
		}
		lease.Release()
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("blocked output did not cancel")
		}
		if bridge {
			select {
			case <-serverDone:
			case <-time.After(3 * time.Second):
				t.Fatal("bridge server survived cancellation")
			}
		}
	}
}
