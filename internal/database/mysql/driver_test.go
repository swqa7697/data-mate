package mysql

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
)

// diagnose runs one readiness check against a fake server.
func diagnose(t *testing.T, s *fakeServer, timeout time.Duration) (database.Readiness, error) {
	t.Helper()
	go s.serve()
	p := profile(MySQL)
	p.Connection.Host = "127.0.0.1"
	p.Connection.Port = s.port()
	limits := config.DefaultLimits()
	limits.QueryTimeoutMS = int(timeout / time.Millisecond)
	p.Limits = &limits
	ready, err := driverWith(t, MySQL).Test(t.Context(), database.NewAccess(p, "explicit-synthetic"))
	select {
	case e := <-s.done:
		if e != nil {
			t.Fatal("fake server:", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fake server did not finish")
	}
	return ready, err
}

// A protocol peer observes the account, password and session pins; each
// failure is attributed to the stage that reached it.
func TestConnectionBoundary(t *testing.T) {
	if os.Getenv("DM_MYSQL_CHILD") == "1" {
		s := newFakeServer(t, "8.4.6", readyResponses("8.4.6", "1"))
		s.oversized = receiveBudget + 1
		_, err := diagnose(t, s, time.Second)
		requireCode(t, err, contracts.ResourceLimit)
		return
	}
	for _, c := range []struct {
		name, version, readOnly, stage string
		grants                         []string
		stall                          bool
		code                           contracts.Code
	}{
		{name: "valid", version: "8.4.6", readOnly: "1", stage: "read_only"},
		{name: "stalled authentication", version: "8.4.6", readOnly: "1", stage: "authentication", stall: true, code: contracts.QueryTimeout},
		{name: "old server", version: "8.0.36", readOnly: "1", stage: "version", code: contracts.QueryUnsupported},
		{name: "other flavor", version: "11.8.3-MariaDB-ubu2404", readOnly: "1", stage: "version", code: contracts.QueryUnsupported},
		{name: "writable session", version: "8.4.6", readOnly: "0", stage: "read_only", code: contracts.ReadOnlyViolation},
		{name: "write grant", version: "8.4.6", readOnly: "1", stage: "read_only", grants: []string{"GRANT INSERT ON `app`.* TO `reader`@`%`"}, code: contracts.ReadOnlyViolation},
	} {
		s := newFakeServer(t, c.version, readyResponses(c.version, c.readOnly, c.grants...))
		s.stall = c.stall
		timeout := time.Second
		if c.stall {
			timeout = 150 * time.Millisecond
		}
		ready, err := diagnose(t, s, timeout)
		if c.code == "" && err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if c.code != "" {
			requireCode(t, err, c.code)
		}
		if ready.Stage != c.stage {
			t.Fatalf("%s: expected %s, got %s: %v", c.name, c.stage, ready.Stage, err)
		}
		if c.name != "valid" {
			continue
		}
		if s.user != "reader" || !bytes.Equal(s.auth, nativePassword(s.salt, "explicit-synthetic")) {
			t.Fatal("ambient or missing credential")
		}
		var pins string
		for _, q := range s.queries {
			if strings.HasPrefix(q, "SET SESSION") {
				pins = q
			}
		}
		for _, want := range []string{"sql_mode='" + MySQL.sqlMode() + "'", "character_set_results='utf8mb4'", "time_zone='+00:00'", "sql_select_limit=18446744073709551615", "max_execution_time=", "transaction_read_only=1", "transaction_isolation='READ-COMMITTED'"} {
			if !strings.Contains(pins, want) {
				t.Fatalf("session pins missing %s: %s", want, pins)
			}
		}
	}
	// The client library must not write upstream error text to stderr; the
	// receive budget reliably triggers its error logging.
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestConnectionBoundary$")
	cmd.Env = append(os.Environ(), "DM_MYSQL_CHILD=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if out, err := cmd.Output(); err != nil || stderr.Len() != 0 {
		t.Fatalf("receive budget child: %v %s; stderr: %q", err, out, stderr.String())
	}
	// Invalid settings and non-read statements fail before any dial.
	d := driverWith(t, MySQL)
	p := profile(MySQL)
	for _, host := range []string{"/tmp", "mysql://user:secret@host", "host,other", "bad host"} {
		bad := p
		bad.Connection.Host = host
		requireCode(t, d.Validate(database.NewAccess(bad, "")), contracts.ConfigInvalid)
	}
	withDatabase := p
	withDatabase.Connection.Database = "app"
	requireCode(t, d.ValidateProfile(withDatabase), contracts.ConfigInvalid)
	long := p
	long.Connection.Username = strings.Repeat("u", 33)
	requireCode(t, d.ValidateProfile(long), contracts.ConfigInvalid)
	if err := driverWith(t, MariaDB).ValidateProfile(func() config.Profile {
		q := profile(MariaDB)
		q.Connection.Username = long.Connection.Username
		return q
	}()); err != nil {
		t.Fatal("MariaDB accepts 80-character usernames", err)
	}
	_, err := d.Query(t.Context(), database.NewAccess(p, ""), database.QueryRequest{SQL: "DELETE FROM app.items"})
	requireCode(t, err, contracts.ReadOnlyViolation)
}
