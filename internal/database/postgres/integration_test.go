package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
)

func profile() config.Profile {
	return config.Profile{ID: "12345678-1234-1234-1234-123456789abc", Alias: "fixture", Driver: "postgres", Connection: config.Connection{Host: "localhost", Port: 5432, Database: "fixture", Username: "reader"}, Transport: config.Transport{TLS: config.TLS{Mode: "disabled"}}, Scope: config.Scope{Mode: "all"}}
}
func driver(t *testing.T) *Driver {
	t.Helper()
	d, err := New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	return d
}
func requireCode(t *testing.T, err error, code contracts.Code) {
	t.Helper()
	var e *database.Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("wanted %s, got %v", code, err)
	}
}
func TestMain(m *testing.M) { SanitizeEnvironment(); os.Exit(m.Run()) }

// No earlier regression exercised a database. This scenario owns P3 acceptance;
// fixture changes below exercise direct, reachable and PUBLIC ACLs on both majors.
func TestPostgresIntegration(t *testing.T) {
	path := os.Getenv("DATA_MATE_PG_FIXTURE")
	if path == "" {
		t.Skip("requires owned Docker fixture: make test-integration DB_DRIVER=postgres")
	}
	if os.Getenv("CI") != "" {
		t.Fatal("integration is excluded from CI")
	}
	var fixture struct {
		Port                          int
		Root, Container, Owner, Image string
	}
	b, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(b, &fixture) != nil {
		t.Fatal("invalid fixture manifest")
	}
	// A manually supplied URL cannot turn this test into an external DB runner.
	b, err = exec.Command("docker", "inspect", "--format", `{{index .Config.Labels "com.data-mate.fixture"}}`, fixture.Container).Output()
	if err != nil || strings.TrimSpace(string(b)) != fixture.Owner || !strings.HasPrefix(fixture.Owner, "dm-pg-") {
		t.Fatal("fixture ownership mismatch")
	}
	b, err = exec.Command("docker", "port", fixture.Container, "5432/tcp").Output()
	if err != nil || strings.TrimSpace(string(b)) != "127.0.0.1:"+strconv.Itoa(fixture.Port) {
		t.Fatal("fixture endpoint mismatch")
	}
	secret := func(name string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(fixture.Root, name))
		if err != nil {
			t.Fatal("fixture credential unavailable")
		}
		return strings.TrimSpace(string(b))
	}
	p := profile()
	p.Connection.Port = fixture.Port
	adminProfile := p
	adminProfile.Connection.Username = "postgres"
	cfg, err := connectionConfig(database.NewAccess(adminProfile, secret("admin-password")))
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["default_transaction_read_only"] = "off"
	admin, err := pgx.ConnectConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal("owned admin connection failed")
	}
	defer closeConn(admin)
	sql := func(q string, args ...any) {
		t.Helper()
		if _, err := admin.Exec(t.Context(), q, args...); err != nil {
			var pg interface{ SQLState() string }
			if errors.As(err, &pg) {
				t.Fatalf("fixture SQL failed (%s): %s", pg.SQLState(), q)
			}
			t.Fatal("fixture SQL failed")
		}
	}
	password := secret("reader-password")
	// Passwords are random hex, used only in memory, never command arguments/logs.
	if len(password) != 64 || strings.Trim(password, "0123456789abcdef") != "" {
		t.Fatal("invalid synthetic password")
	}
	_, err = admin.Exec(t.Context(), "CREATE ROLE reader LOGIN PASSWORD '"+password+"'")
	if err != nil {
		t.Fatal("create synthetic role failed")
	}
	sql(`CREATE ROLE writer; CREATE ROLE elevated BYPASSRLS; CREATE ROLE bridge;
 CREATE SCHEMA app; CREATE SCHEMA hidden; CREATE SCHEMA "Dot.Schema";
 CREATE TABLE hidden.audit(n int); INSERT INTO hidden.audit VALUES(0);
 CREATE TABLE hidden.target(id int PRIMARY KEY);
 CREATE SEQUENCE app.counter;
 CREATE TABLE app.items(id int PRIMARY KEY, target int REFERENCES hidden.target(id), name text, amount numeric, at timestamptz);
 CREATE TABLE app.rls(id int); INSERT INTO app.rls VALUES(1); ALTER TABLE app.rls ENABLE ROW LEVEL SECURITY;
 CREATE FUNCTION app.policy_probe() RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$BEGIN UPDATE hidden.audit SET n=n+1; RETURN true; END$$;
 CREATE POLICY reader_policy ON app.rls USING(app.policy_probe());
 CREATE VIEW app.a_view AS SELECT id FROM app.items;
 CREATE TYPE app.custom AS ENUM ('one'); CREATE TABLE app.custom_type(value app.custom);
 CREATE TABLE app.parts(id int) PARTITION BY RANGE(id); CREATE TABLE hidden.part PARTITION OF app.parts FOR VALUES FROM(0) TO(10);
 CREATE TABLE app.parent(id int); CREATE TABLE hidden.child() INHERITS(app.parent);
 CREATE TABLE app.rls_parts(id int) PARTITION BY RANGE(id); ALTER TABLE app.rls_parts ENABLE ROW LEVEL SECURITY;
 CREATE TABLE "Dot.Schema"."a.b"(id int);
 GRANT USAGE ON SCHEMA app,hidden,"Dot.Schema" TO reader;
 GRANT SELECT ON ALL TABLES IN SCHEMA app,hidden,"Dot.Schema" TO reader;
 GRANT CONNECT ON DATABASE fixture TO reader;
 GRANT UPDATE ON app.items TO writer;`)
	d := driver(t)
	access := database.NewAccess(p, password)
	ready, err := d.Test(t.Context(), access)
	if err != nil {
		t.Fatal(err)
	}
	if ready.Stage != "read_only" || len(ready.Stages) != 5 {
		t.Fatalf("readiness stages: %+v", ready)
	}
	for _, stage := range ready.Stages {
		if !stage.OK {
			t.Fatal("successful readiness has failed stage")
		}
	}
	t.Logf("server_version_num=%d image=%s", ready.ServerVersion, fixture.Image)
	readPathMeasurements(t, access, sql)
	wantMajor := 16
	if fixture.Image == "postgres:18" {
		wantMajor = 18
	}
	if ready.ServerVersion/10000 != wantMajor {
		t.Fatal("server/image major mismatch")
	}
	tlsProfile := p
	tlsProfile.Transport.TLS = config.TLS{Mode: "verify-full", CAFile: filepath.Join(fixture.Root, "ca.crt")}
	ready, err = d.Test(t.Context(), database.NewAccess(tlsProfile, password))
	if err != nil || !ready.TLS {
		t.Fatalf("verified TLS: %v", err)
	}
	for _, mode := range []string{"hostname", "ca"} {
		bad := tlsProfile
		if mode == "hostname" {
			bad.Connection.Host = "127.0.0.1"
		} else {
			bad.Transport.TLS.CAFile = filepath.Join(fixture.Root, "bad.crt")
		}
		ready, err = d.Test(t.Context(), database.NewAccess(bad, password))
		if ready.Stage != "dial" {
			t.Fatalf("TLS failed at %s", ready.Stage)
		}
		requireCode(t, err, contracts.ConnectFailed)
	}
	ready, err = d.Test(t.Context(), database.NewAccess(p, "synthetic-wrong-password"))
	if ready.Stage != "authentication" {
		t.Fatalf("bad password failed at %s", ready.Stage)
	}
	requireCode(t, err, contracts.ConnectFailed)
	if strings.Contains(err.Error(), password) || strings.Contains(err.Error(), "synthetic-wrong-password") {
		t.Fatal("credential in error")
	}
	// Readiness certifies the transaction, not an exhaustive audit of grants.
	sql("GRANT UPDATE ON app.items TO reader")
	if _, err = d.Test(t.Context(), access); err != nil {
		t.Fatal("readiness audited grants", err)
	}
	sql("REVOKE UPDATE ON app.items FROM reader")
	p.Scope = config.Scope{Mode: "selected", Schemas: []string{"app"}, Tables: []config.Table{{Schema: "Dot.Schema", Name: "a.b"}}}
	access = database.NewAccess(p, password)
	seen := map[string]database.Table{}
	req := database.PageRequest{PageSize: 2}
	firstCursor := ""
	for {
		page, err := d.ListTables(t.Context(), access, req)
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(page)
		if err := contracts.Validate("list_tables.output", encoded); err != nil {
			t.Fatal("metadata contract", err)
		}
		for _, table := range page.Tables {
			if table.Schema == "hidden" {
				t.Fatal("hidden metadata")
			}
			key := table.Schema + "/" + table.Name
			if _, ok := seen[key]; ok {
				t.Fatal("duplicate page row")
			}
			seen[key] = table
		}
		if page.NextCursor == nil {
			break
		}
		req.Cursor = *page.NextCursor
		if firstCursor == "" {
			firstCursor = req.Cursor
		}
	}
	if len(seen) != 8 || seen["app/a_view"].Kind != "view" || seen["app/custom_type"].Name == "" {
		t.Fatalf("catalog: %#v", seen)
	}

	desc, err := d.DescribeTable(t.Context(), access, config.Table{Schema: "app", Name: "items"})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(desc)
	if err := contracts.Validate("describe_table.output", encoded); err != nil {
		t.Fatal("description contract", err)
	}
	if len(desc.Relationships) != 0 || len(desc.Keys) != 1 || len(desc.Columns) != 5 {
		t.Fatalf("scoped description: %#v", desc)
	}
	all := p
	all.Scope = config.Scope{Mode: "all"}
	desc, err = d.DescribeTable(t.Context(), database.NewAccess(all, password), config.Table{Schema: "app", Name: "items"})
	if err != nil || len(desc.Relationships) != 1 {
		t.Fatalf("visible relationship: %v", err)
	}
	_, err = d.DescribeTable(t.Context(), access, config.Table{Schema: "hidden", Name: "target"})
	requireCode(t, err, contracts.ScopeDenied)
	none := p
	none.Scope = config.Scope{Mode: "selected"}
	page, err := d.ListTables(t.Context(), database.NewAccess(none, password), database.PageRequest{})
	if err != nil || len(page.Tables) != 0 {
		t.Fatal("empty scope", err)
	}
	// User browsing ignores saved scope but retains role visibility and relation
	// support. Agents still see none through ListTables above.
	browse, err := d.BrowseScope(t.Context(), database.NewAccess(none, password), database.ScopeRequest{})
	if err != nil || !slices.Contains(browse.Schemas, "Dot.Schema") || !slices.Contains(browse.Schemas, "hidden") {
		t.Fatalf("full user catalog: %+v %v", browse, err)
	}
	browse, err = d.BrowseScope(t.Context(), database.NewAccess(none, password), database.ScopeRequest{Schema: "Dot.Schema", Search: "a.b"})
	if err != nil || len(browse.Tables) != 1 || browse.Tables[0].Name != "a.b" {
		t.Fatalf("exact catalog identifiers: %+v %v", browse, err)
	}
	browse, err = d.BrowseScope(t.Context(), database.NewAccess(none, password), database.ScopeRequest{Schema: "app", Search: "%"})
	if err != nil || len(browse.Tables) != 0 {
		t.Fatal("search interpreted wildcard", err)
	}
	for _, r := range []database.PageRequest{{Cursor: firstCursor + "x"}, {Cursor: firstCursor, Schema: "app"}} {
		_, err = d.ListTables(t.Context(), access, r)
		requireCode(t, err, contracts.StaleCursor)
	}
	_, err = d.ListTables(t.Context(), database.NewAccess(none, password), database.PageRequest{Cursor: firstCursor})
	requireCode(t, err, contracts.StaleCursor)
	fresh := driver(t)
	_, err = fresh.ListTables(t.Context(), access, database.PageRequest{Cursor: firstCursor})
	requireCode(t, err, contracts.StaleCursor)
	d.Invalidate(p.ID)
	_, err = d.ListTables(t.Context(), access, database.PageRequest{Cursor: firstCursor})
	requireCode(t, err, contracts.StaleCursor)
	sql("CREATE TABLE app.future(id int); GRANT SELECT ON app.future TO reader")
	if _, err = d.DescribeTable(t.Context(), access, config.Table{Schema: "app", Name: "future"}); err != nil {
		t.Fatal(err)
	}
	sql("REVOKE SELECT ON app.items FROM reader")
	_, err = d.DescribeTable(t.Context(), access, config.Table{Schema: "app", Name: "items"})
	requireCode(t, err, contracts.ScopeDenied)
	sql("GRANT SELECT ON app.items TO reader")
	if _, err = d.Query(t.Context(), access, database.QueryRequest{SQL: "DELETE FROM app.rls"}); err == nil {
		t.Fatal("write query accepted")
	}
	var count int
	if err = admin.QueryRow(t.Context(), "SELECT n FROM hidden.audit").Scan(&count); err != nil || count != 0 {
		t.Fatal("metadata/test executed application RLS or rows")
	}
	mcpAcceptance(t, d, access)
	nativeAgentAcceptance(t, p, password, sql)
	scopeAcceptance(t, d, access, password, sql)
	cliDiagnosticsAcceptance(t, fixture.Root)
	queryAcceptance(t, d, access, sql)
	executorAcceptance(t, d, access, admin, sql)
	transportAcceptance(t, p, password, fixture.Root, admin)
	// Observe eight executing queries before admitting a ninth: the old two-slot
	// pool cannot reach this barrier. The ninth must wait and recover after release.
	sleepers, stopSleepers := context.WithCancel(t.Context())
	defer stopSleepers()
	sleepersFinished := make(chan error, 8)
	for range 8 {
		go func() {
			_, err := d.Query(sleepers, access, database.QueryRequest{SQL: "SELECT 1 FROM pg_catalog.pg_sleep(40)"})
			sleepersFinished <- err
		}()
	}
	observation, stopObservation := context.WithTimeout(t.Context(), 10*time.Second)
	defer stopObservation()
	for {
		var active int
		err := admin.QueryRow(observation, "SELECT count(*) FROM pg_catalog.pg_stat_activity WHERE usename='reader' AND wait_event='PgSleep'").Scan(&active)
		if err != nil {
			t.Fatal("eight queries did not execute concurrently", err)
		}
		if active == 8 {
			break
		}
		runtime.Gosched()
	}
	ninth := make(chan error, 1)
	go func() {
		_, err := d.Query(t.Context(), access, database.QueryRequest{SQL: "SELECT id FROM app.items"})
		ninth <- err
	}()
	for {
		d.mu.Lock()
		pool := d.pools[p.ID]
		queued := pool != nil && pool.users == 9 && len(pool.slots) == 8
		d.mu.Unlock()
		if queued {
			break
		}
		select {
		case err := <-ninth:
			t.Fatalf("ninth query did not wait: %v", err)
		case <-observation.Done():
			t.Fatal("ninth query did not reach pool")
		default:
			runtime.Gosched()
		}
	}
	stopSleepers()
	for range 8 {
		requireCode(t, <-sleepersFinished, contracts.Cancelled)
	}
	if err := <-ninth; err != nil {
		t.Fatal("queued query did not recover", err)
	}
	// Successful work leaves a reusable connection and no outstanding pool users.
	if _, err := d.Query(t.Context(), access, database.QueryRequest{SQL: "SELECT 1"}); err != nil {
		t.Fatal("pool reuse after saturation", err)
	}
	d.mu.Lock()
	pool := d.pools[p.ID]
	connections, users := len(pool.idle), pool.users
	d.mu.Unlock()
	if connections == 0 || connections > 8 || users != 0 {
		t.Fatal("pool leak")
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = d.Test(ctx, access)
	requireCode(t, err, contracts.Cancelled)
	// Cancellation during a real catalog request must dispose uncertain connections.
	err = d.run(t.Context(), access, func(ctx context.Context, tx pgx.Tx, _ int) error {
		short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
		defer cancel()
		_, err := tx.Exec(short, "SELECT pg_catalog.pg_sleep(1)")
		return err
	})
	requireCode(t, err, contracts.QueryTimeout)
	if _, err = d.Test(t.Context(), access); err != nil {
		t.Fatal("failed to recover after cancellation", err)
	}

	// Retirement cancels an operation and waits for rollback before its caller returns.
	started := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		finished <- d.run(t.Context(), access, func(ctx context.Context, _ pgx.Tx, _ int) error { close(started); <-ctx.Done(); return ctx.Err() })
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("operation did not start")
	}
	d.Invalidate(p.ID)
	select {
	case err := <-finished:
		requireCode(t, err, contracts.Cancelled)
	case <-time.After(5 * time.Second):
		t.Fatal("invalidation did not finish")
	}
	if _, err = d.Test(t.Context(), access); err != nil {
		t.Fatal(err)
	}
	// Advance the existing idle timer directly; no five-minute wall-clock sleep.
	d.mu.Lock()
	d.pools[p.ID].timer.Reset(0)
	d.mu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for {
		d.mu.Lock()
		_, present := d.pools[p.ID]
		d.mu.Unlock()
		if !present {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("idle pool not retired")
		}
		runtime.Gosched()
	}
	for i := range 17 {
		other := p
		other.ID = fmt.Sprintf("12345678-1234-1234-1234-%012d", i)
		if _, err = d.Test(t.Context(), database.NewAccess(other, password)); err != nil {
			t.Fatal(err)
		}
	}
	d.mu.Lock()
	countPools := len(d.pools)
	d.mu.Unlock()
	if countPools != 16 {
		t.Fatal("idle pool eviction failed")
	}
	d.Close()
	_, err = d.Test(t.Context(), access)
	requireCode(t, err, contracts.ServiceUnavailable)
}

// Optional measurements extend the owned fixture; ordinary assertions never use timing.
func readPathMeasurements(t *testing.T, access database.Access, sql func(string, ...any)) {
	t.Helper()
	if os.Getenv("DATA_MATE_MEASURE") != "1" {
		return
	}
	sql("CREATE SCHEMA measurement; GRANT USAGE ON SCHEMA measurement TO reader")
	for i := range 30 {
		sql(fmt.Sprintf("CREATE TABLE measurement.t%02d(id int); GRANT SELECT ON measurement.t%02d TO reader", i, i))
	}
	defer sql("DROP SCHEMA measurement CASCADE")
	started := time.Now()
	for trial := range 5 {
		d, err := New()
		if err != nil {
			t.Fatal(err)
		}
		for _, operation := range []string{"query", "list"} {
			for iteration := range 7 {
				begin := time.Now()
				if operation == "query" {
					_, err = d.Query(t.Context(), access, database.QueryRequest{SQL: "SELECT count(*) FROM app.items"})
				} else {
					_, err = d.ListTables(t.Context(), access, database.PageRequest{Schema: "measurement", PageSize: 100})
				}
				elapsed := time.Since(begin).Nanoseconds()
				if err != nil {
					d.Close()
					t.Fatal(err)
				}
				if iteration >= 2 {
					t.Logf("MEASURE operation=%s trial=%d iteration=%d ns=%d", operation, trial, iteration-2, elapsed)
				}
			}
		}
		d.Close()
		if trial >= 2 && time.Since(started) > 4*time.Minute {
			break
		}
	}
}
