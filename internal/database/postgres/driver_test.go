package postgres

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
	"github.com/swqa7697/data-mate/internal/database/postgres/sqlpolicy"
)

// The subprocess enters through the same startup sanitization as the application.
// A real protocol peer observes destination, password and session settings; hostile
// PG files must neither redirect a connection nor supply a fallback credential.
func TestConnectionBoundary(t *testing.T) {
	if os.Getenv("DM_CONFIG_CHILD") == "1" {
		p := profile()
		port, _ := strconv.Atoi(os.Getenv("DM_CONFIG_PORT"))
		p.Connection.Host = "127.0.0.1"
		p.Connection.Port = port
		d := driver(t)
		_, err := d.Test(t.Context(), database.NewAccess(p, "explicit-synthetic"))
		requireCode(t, err, contracts.ResourceLimit)
		return
	}
	diagnosticProtocolAcceptance(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	peer := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			peer <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		backend := pgproto3.NewBackend(conn, conn)
		msg, err := backend.ReceiveStartupMessage()
		if err != nil {
			peer <- err
			return
		}
		start, ok := msg.(*pgproto3.StartupMessage)
		if !ok {
			peer <- errors.New("unexpected startup")
			return
		}
		for k, want := range map[string]string{"user": "reader", "database": "fixture", "application_name": "data-mate", "search_path": "pg_catalog", "TimeZone": "UTC", "DateStyle": "ISO, YMD", "bytea_output": "hex", "extra_float_digits": "3", "client_encoding": "UTF8", "default_transaction_read_only": "on"} {
			if start.Parameters[k] != want {
				peer <- errors.New("unsafe startup parameter: " + k)
				return
			}
		}
		if _, ok := start.Parameters["options"]; ok {
			peer <- errors.New("ambient options")
			return
		}
		backend.Send(&pgproto3.AuthenticationCleartextPassword{})
		if err = backend.Flush(); err != nil {
			peer <- err
			return
		}
		msg, err = backend.Receive()
		if err != nil {
			peer <- err
			return
		}
		password, ok := msg.(*pgproto3.PasswordMessage)
		if !ok || password.Password != "explicit-synthetic" {
			peer <- errors.New("ambient credential")
			return
		}
		backend.Send(&pgproto3.AuthenticationOk{})
		backend.Send(&pgproto3.ParameterStatus{Name: "server_version", Value: "16.0"})
		backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
		if err = backend.Flush(); err != nil {
			peer <- err
			return
		}
		// Advertise a huge body without sending it: the cap must reject the header,
		// before allocating/reading a body or waiting for the server deadline.
		if _, err = backend.Receive(); err != nil {
			peer <- err
			return
		}
		header := []byte{'T', 0, 0, 0, 0}
		binary.BigEndian.PutUint32(header[1:], (2<<20)+5)
		_, err = conn.Write(header)
		peer <- err
	}()
	dir := t.TempDir()
	pass := filepath.Join(dir, "passfile")
	service := filepath.Join(dir, "servicefile")
	if err = os.WriteFile(pass, []byte("*:*:*:*:ambient-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(service, []byte("[hostile]\nhost=invalid.example\nuser=attacker\npassword=ambient-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestConnectionBoundary$")
	cmd.Env = append(os.Environ(), "DM_CONFIG_CHILD=1", "DM_CONFIG_PORT="+strconv.Itoa(listener.Addr().(*net.TCPAddr).Port), "PGHOST=invalid.example", "PGPORT=1", "PGDATABASE=ambient", "PGUSER=attacker", "PGPASSWORD=ambient-secret", "PGSERVICE=hostile", "PGSERVICEFILE="+service, "PGPASSFILE="+pass, "PGSSLMODE=require", "PGSSLCERT=/unreadable", "PGOPTIONS=-c role=attacker", "PGTARGETSESSIONATTRS=read-write")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("configuration isolation: %v %s; peer: %v", err, out, <-peer)
	}
	if err = <-peer; err != nil {
		t.Fatal(err)
	}
	d := driver(t)
	p := profile()
	for _, host := range []string{"/tmp", "postgres://user:secret@host", "host,other", "bad host"} {
		bad := p
		bad.Connection.Host = host
		requireCode(t, d.Validate(database.NewAccess(bad, "")), contracts.ConfigInvalid)
	}
	p.Transport.SSH = &config.SSH{Host: "jump", Port: 22, User: "user", Auth: "password"}
	requireCode(t, d.Validate(database.NewAccess(p, "")), contracts.ConnectFailed)
	_, err = d.Query(t.Context(), database.NewAccess(profile(), ""), database.QueryRequest{SQL: "DELETE FROM app.items"})
	requireCode(t, err, contracts.QueryUnsupported)
	t.Setenv("PGPASSWORD", "unexpected-after-startup")
	requireCode(t, d.Validate(database.NewAccess(profile(), "")), contracts.ConfigInvalid)
}

// This scenario owns admission/cursor algorithms that cannot be reliably saturated
// through network timing. Synchronization is explicit rather than latency assertions.
func TestAdmissionAndCursorLifecycle(t *testing.T) {
	d := driver(t)
	var leave []func()
	for range 8 {
		f, err := d.admit(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		leave = append(leave, f)
	}
	ctx, cancel := context.WithCancel(t.Context())
	results := make(chan error, 32)
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			f, err := d.admit(ctx)
			if err == nil {
				f()
			}
			results <- err
		})
	}
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for len(d.waiting) < 32 {
		select {
		case <-deadline.C:
			t.Fatal("waiters did not enter queue")
		default:
			runtime.Gosched()
		}
	}
	_, err := d.admit(t.Context())
	requireCode(t, err, contracts.ResourceLimit)
	cancel()
	wg.Wait()
	close(results)
	for err := range results {
		requireCode(t, err, contracts.Cancelled)
	}
	for _, f := range leave {
		f()
	}
	if len(d.active) != 0 || len(d.waiting) != 0 {
		t.Fatal("admission leaked")
	}
	c := cursor{Version: 1, ID: "id", Revision: "revision", Schema: "a", Name: "b"}
	token := d.encodeCursor(c)
	if _, err = d.decodeCursor(token, c); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{token + "x", strings.Repeat("x", 2049), "!!!"} {
		_, err = d.decodeCursor(bad, c)
		requireCode(t, err, contracts.StaleCursor)
	}
	changed := c
	changed.Revision = "other"
	_, err = d.decodeCursor(token, changed)
	requireCode(t, err, contracts.StaleCursor)
	d2 := driver(t)
	_, err = d2.decodeCursor(token, c)
	requireCode(t, err, contracts.StaleCursor)
}

// Extend the synthetic protocol scenario: malformed fingerprints must fail closed,
// unsupported majors must not fetch a catalog, and size limits survive hashing.
func diagnosticProtocolAcceptance(t *testing.T) {
	t.Helper()
	expected, err := sqlpolicy.CatalogFingerprint(16)
	if err != nil {
		t.Fatal(err)
	}
	valid := []byte(`\x` + hex.EncodeToString(expected[:]))
	for _, test := range []struct {
		name, stage  string
		version      int
		size, digest []byte
		noRow        bool
		code         contracts.Code
	}{
		{name: "stalled authentication", stage: "authentication", version: 160000, code: contracts.QueryTimeout},
		{name: "old server", stage: "version", version: 150000, code: contracts.QueryUnsupported},
		{name: "unaudited major", stage: "policy", version: 170000, code: contracts.QueryUnsupported},
		{name: "valid", stage: "policy", version: 160000, size: []byte("373699"), digest: valid},
		{name: "null digest", stage: "policy", version: 160000, size: []byte("373699"), code: contracts.QueryUnsupported},
		{name: "short digest", stage: "policy", version: 160000, size: []byte("373699"), digest: []byte(`\x00`), code: contracts.QueryUnsupported},
		{name: "mismatch", stage: "policy", version: 160000, size: []byte("373699"), digest: []byte(`\x` + strings.Repeat("00", 32)), code: contracts.QueryUnsupported},
		{name: "malformed bytea", stage: "policy", version: 160000, size: []byte("373699"), digest: []byte(`\xzz`), code: contracts.QueryUnsupported},
		{name: "null size", stage: "policy", version: 160000, digest: valid, code: contracts.QueryUnsupported},
		{name: "malformed size", stage: "policy", version: 160000, size: []byte("invalid"), digest: valid, code: contracts.QueryUnsupported},
		{name: "empty size", stage: "policy", version: 160000, size: []byte("0"), digest: valid, code: contracts.QueryUnsupported},
		{name: "missing row", stage: "policy", version: 160000, noRow: true, code: contracts.QueryUnsupported},
		{name: "size boundary", stage: "policy", version: 160000, size: []byte("2097152"), digest: valid},
		{name: "oversized catalog", stage: "policy", version: 160000, size: []byte("2097153"), code: contracts.ResourceLimit},
	} {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { listener.Close() })
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		done := make(chan error, 1)
		catalogCalls := 0
		go func() {
			conn, err := listener.Accept()
			if err != nil {
				done <- err
				return
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
			backend := pgproto3.NewBackend(conn, conn)
			if _, err = backend.ReceiveStartupMessage(); err != nil {
				done <- err
				return
			}
			if test.stage == "authentication" {
				backend.Send(&pgproto3.AuthenticationCleartextPassword{})
				if err = backend.Flush(); err != nil {
					done <- err
					return
				}
				if _, err = backend.Receive(); err != nil {
					done <- err
					return
				}
				if _, err = backend.Receive(); err == nil {
					done <- errors.New("stalled auth received unexpected message")
					return
				}
				done <- nil
				return
			}
			backend.Send(&pgproto3.AuthenticationOk{})
			backend.Send(&pgproto3.ParameterStatus{Name: "server_version", Value: strconv.Itoa(test.version/10000) + ".0"})
			backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
			if err = backend.Flush(); err != nil {
				done <- err
				return
			}
			describe := func(q string) ([]pgproto3.FieldDescription, [][]byte) {
				var names []string
				var oids []uint32
				var values [][]byte
				switch {
				case strings.Contains(q, "server_version_num"):
					names = []string{"version", "current_user", "session_user", "connect"}
					oids = []uint32{23, 25, 25, 16}
					values = [][]byte{[]byte(strconv.Itoa(test.version)), []byte("reader"), []byte("reader"), []byte("t")}
				case q == sqlpolicy.CatalogFingerprintSQL:
					names = []string{"catalog_bytes", "fingerprint"}
					oids = []uint32{23, 17}
					values = [][]byte{test.size, test.digest}
					if test.noRow {
						values = nil
					}
				case q == roleSQL || strings.Contains(q, "MAINTAIN"):
					names = []string{"unsafe"}
					oids = []uint32{16}
					values = [][]byte{[]byte("f")}
				}
				fields := make([]pgproto3.FieldDescription, len(names))
				for i, name := range names {
					fields[i] = pgproto3.FieldDescription{Name: []byte(name), DataTypeOID: oids[i], DataTypeSize: -1}
				}
				return fields, values
			}
			query := ""
			for {
				msg, err := backend.Receive()
				if err != nil {
					done <- nil
					return
				}
				switch m := msg.(type) {
				case *pgproto3.Query:
					fields, values := describe(m.String)
					if len(fields) > 0 {
						backend.Send(&pgproto3.RowDescription{Fields: fields})
					}
					if values != nil {
						backend.Send(&pgproto3.DataRow{Values: values})
					}
					tag := "SELECT 1"
					status := byte('T')
					if strings.Contains(strings.ToUpper(m.String), "ROLLBACK") {
						tag = "ROLLBACK"
						status = 'I'
					}
					backend.Send(&pgproto3.CommandComplete{CommandTag: []byte(tag)})
					backend.Send(&pgproto3.ReadyForQuery{TxStatus: status})
				case *pgproto3.Parse:
					query = m.Query
					if query == sqlpolicy.CatalogFingerprintSQL || query == sqlpolicy.CatalogSQL {
						catalogCalls++
					}
					backend.Send(&pgproto3.ParseComplete{})
				case *pgproto3.Bind:
					backend.Send(&pgproto3.BindComplete{})
				case *pgproto3.Describe:
					fields, _ := describe(query)
					if len(fields) > 0 {
						backend.Send(&pgproto3.RowDescription{Fields: fields})
					} else {
						backend.Send(&pgproto3.NoData{})
					}
				case *pgproto3.Execute:
					_, values := describe(query)
					if values != nil {
						backend.Send(&pgproto3.DataRow{Values: values})
					}
					backend.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")})
				case *pgproto3.Sync:
					backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'T'})
				case *pgproto3.Terminate:
					done <- nil
					return
				}
				if err = backend.Flush(); err != nil {
					done <- err
					return
				}
			}
		}()
		p := profile()
		p.Connection.Host = "127.0.0.1"
		p.Connection.Port = listener.Addr().(*net.TCPAddr).Port
		limits := config.DefaultLimits()
		limits.QueryTimeoutMS = 1000
		if test.stage == "authentication" {
			limits.QueryTimeoutMS = 150
		}
		p.Limits = &limits
		d := driver(t)
		ready, err := d.Test(ctx, database.NewAccess(p, "synthetic-diagnostic-password"))
		if test.code == "" {
			if err != nil {
				t.Fatalf("%s: %v", test.name, err)
			}
		} else {
			requireCode(t, err, test.code)
		}
		if ready.Stage != test.stage {
			t.Fatalf("%s: expected %s, got %s: %v", test.name, test.stage, ready.Stage, err)
		}
		d.Close()
		listener.Close()
		cancel()
		select {
		case err = <-done:
			if err != nil {
				t.Fatal("protocol fixture", test.name, err)
			}
			if test.version == 170000 && catalogCalls != 0 {
				t.Fatal("unaudited major fetched catalog")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("protocol fixture cleanup stalled", test.name)
		}
	}
}
