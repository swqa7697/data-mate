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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
	"github.com/swqa7697/data-mate/internal/database/pool"
)

func profile() config.Profile {
	return config.Profile{ID: "12345678-1234-1234-1234-123456789abc", Alias: "fixture", Driver: "postgres", Connection: config.Connection{Host: "localhost", Port: 5432, Database: "fixture", Username: "reader"}, Transport: config.Transport{TLS: config.TLS{Mode: "disabled"}}}
}
func driver(t *testing.T) *Driver {
	t.Helper()
	return driverWith(t, pool.Options{})
}

// driverWith binds a driver to its own registry, closed when the test ends.
func driverWith(t *testing.T, o pool.Options) *Driver {
	t.Helper()
	r, err := pool.New(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	return New(r)
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
	accountAcceptance(t, d, access, ready.ServerVersion, sql)
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
	// Fresh diagnostics reject write grants even if an existing pool is approved.
	sql("GRANT UPDATE ON app.items TO reader")
	_, err = d.Test(t.Context(), access)
	requireCode(t, err, contracts.ReadOnlyViolation)
	sql("REVOKE UPDATE ON app.items FROM reader")
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
	if len(seen) < 12 || seen["pg_catalog/pg_class"].Name == "" || seen["app/a_view"].Kind != "view" || seen["app/custom_type"].Name == "" {
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
	if len(desc.Relationships) != 1 || len(desc.Keys) != 1 || len(desc.Columns) != 5 {
		t.Fatalf("readable description: %#v", desc)
	}
	// Catalog visibility is independent of privileges to read referenced data.
	sql("REVOKE SELECT ON hidden.target FROM reader")
	desc, err = d.DescribeTable(t.Context(), access, config.Table{Schema: "app", Name: "items"})
	if err != nil || len(desc.Relationships) != 1 {
		t.Fatal("foreign-key endpoint hidden", err)
	}
	_, err = d.DescribeTable(t.Context(), access, config.Table{Schema: "hidden", Name: "target"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.DescribeTable(t.Context(), access, config.Table{Schema: "hidden", Name: "missing"})
	requireCode(t, err, contracts.PermissionDenied)
	sql("GRANT SELECT ON hidden.target TO reader")
	changed := p
	changed.Alias = "renamed"
	for _, r := range []database.PageRequest{{Cursor: firstCursor + "x"}, {Cursor: firstCursor, Schema: "app"}} {
		_, err = d.ListTables(t.Context(), access, r)
		requireCode(t, err, contracts.StaleCursor)
	}
	_, err = d.ListTables(t.Context(), database.NewAccess(changed, password), database.PageRequest{Cursor: firstCursor})
	requireCode(t, err, contracts.StaleCursor)
	fresh := driver(t)
	_, err = fresh.ListTables(t.Context(), access, database.PageRequest{Cursor: firstCursor})
	requireCode(t, err, contracts.StaleCursor)
	d.pools.Invalidate(p.ID)
	_, err = d.ListTables(t.Context(), access, database.PageRequest{Cursor: firstCursor})
	requireCode(t, err, contracts.StaleCursor)
	sql("CREATE TABLE app.future(id int); GRANT SELECT ON app.future TO reader")
	if _, err = d.DescribeTable(t.Context(), access, config.Table{Schema: "app", Name: "future"}); err != nil {
		t.Fatal(err)
	}
	sql("REVOKE SELECT ON app.items FROM reader")
	_, err = d.DescribeTable(t.Context(), access, config.Table{Schema: "app", Name: "items"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.Query(t.Context(), access, database.QueryRequest{SQL: "SELECT * FROM app.items"})
	requireCode(t, err, contracts.PermissionDenied)
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
	catalogAcceptance(t, d, access, password, sql)
	cliDiagnosticsAcceptance(t, fixture.Root)
	queryAcceptance(t, d, access, sql)
	objectAcceptance(t, d, access, admin, sql)
	poolAccountAcceptance(t, access, admin, sql)
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
		s := d.pools.Stats(p.ID)
		if s.Users == 9 && s.Slots == 8 {
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
	stats := d.pools.Stats(p.ID)
	connections, users := stats.Idle, stats.Users
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
	d.pools.Invalidate(p.ID)
	select {
	case err := <-finished:
		requireCode(t, err, contracts.Cancelled)
	case <-time.After(5 * time.Second):
		t.Fatal("invalidation did not finish")
	}
	if _, err = d.Test(t.Context(), access); err != nil {
		t.Fatal(err)
	}
	if _, err = d.Query(t.Context(), access, database.QueryRequest{SQL: "SELECT 1"}); err != nil {
		t.Fatal(err)
	}
	// A short idle TTL retires an unused pool without a five-minute wall-clock sleep.
	idle := driverWith(t, pool.Options{IdleTTL: 10 * time.Millisecond})
	if _, err = idle.Query(t.Context(), access, database.QueryRequest{SQL: "SELECT 1"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for idle.pools.Stats(p.ID).Present {
		if time.Now().After(deadline) {
			t.Fatal("idle pool not retired")
		}
		runtime.Gosched()
	}
	for i := range 17 {
		other := p
		other.ID = fmt.Sprintf("12345678-1234-1234-1234-%012d", i)
		if _, err = d.Query(t.Context(), database.NewAccess(other, password), database.QueryRequest{SQL: "SELECT 1"}); err != nil {
			t.Fatal(err)
		}
	}
	if d.pools.Len() != 16 {
		t.Fatal("idle pool eviction failed")
	}
	d.pools.Close()
	_, err = d.Test(t.Context(), access)
	requireCode(t, err, contracts.ServiceUnavailable)
}

// Optional measurements extend the owned fixture; ordinary assertions never use timing.
// Account cases extend the existing real-server privilege scenario (ladder step 2).
func accountAcceptance(t *testing.T, d *Driver, a database.Access, version int, sql func(string, ...any)) {
	t.Helper()
	cases := []struct{ grant, revoke string }{
		{"GRANT INSERT(id) ON app.items TO reader", "REVOKE INSERT(id) ON app.items FROM reader"},
		{"GRANT UPDATE ON app.items TO PUBLIC", "REVOKE UPDATE ON app.items FROM PUBLIC"},
		{"GRANT writer TO reader WITH INHERIT TRUE, SET FALSE", "REVOKE writer FROM reader"},
		{"GRANT writer TO bridge; GRANT bridge TO reader WITH INHERIT FALSE, SET TRUE", "REVOKE bridge FROM reader; REVOKE writer FROM bridge"},
		{"GRANT USAGE ON SEQUENCE app.counter TO reader", "REVOKE USAGE ON SEQUENCE app.counter FROM reader"},
		{"GRANT CREATE ON SCHEMA app TO reader", "REVOKE CREATE ON SCHEMA app FROM reader"},
		{"GRANT CREATE ON DATABASE fixture TO reader", "REVOKE CREATE ON DATABASE fixture FROM reader"},
		{"CREATE TABLE app.owned(id int); ALTER TABLE app.owned OWNER TO reader", "DROP TABLE app.owned"},
		{"ALTER ROLE reader CREATEDB", "ALTER ROLE reader NOCREATEDB"},
		{"ALTER ROLE reader CREATEROLE", "ALTER ROLE reader NOCREATEROLE"},
		{"ALTER ROLE reader REPLICATION", "ALTER ROLE reader NOREPLICATION"},
		{"ALTER ROLE reader SUPERUSER", "ALTER ROLE reader NOSUPERUSER"},
		{"GRANT pg_write_all_data TO reader", "REVOKE pg_write_all_data FROM reader"},
		{"GRANT pg_write_server_files TO reader", "REVOKE pg_write_server_files FROM reader"},
		{"GRANT pg_execute_server_program TO reader", "REVOKE pg_execute_server_program FROM reader"},
		// SET followed by INHERIT can expose a capability unavailable to the login.
		{"GRANT pg_write_server_files TO bridge WITH INHERIT TRUE, SET FALSE; GRANT bridge TO reader WITH INHERIT FALSE, SET TRUE", "REVOKE bridge FROM reader; REVOKE pg_write_server_files FROM bridge"},
		{"GRANT bridge TO reader WITH ADMIN TRUE, INHERIT FALSE, SET FALSE", "REVOKE bridge FROM reader"},
		{"SELECT lo_create(90001); GRANT UPDATE ON LARGE OBJECT 90001 TO reader", "SELECT lo_unlink(90001)"},
		{"GRANT ALTER SYSTEM ON PARAMETER work_mem TO reader", "REVOKE ALTER SYSTEM ON PARAMETER work_mem FROM reader"},
	}
	if version >= 170000 {
		cases = append(cases, struct{ grant, revoke string }{"GRANT MAINTAIN ON app.items TO reader", "REVOKE MAINTAIN ON app.items FROM reader"})
	}
	for i, tc := range cases {
		sql(tc.grant)
		_, err := d.Test(t.Context(), a)
		sql(tc.revoke)
		var failure *database.Error
		if !errors.As(err, &failure) || failure.Code != contracts.ReadOnlyViolation {
			t.Fatalf("account case %d: %v", i, err)
		}
	}
	// A membership that cannot be inherited or assumed conveys no write capability.
	sql("GRANT writer TO reader WITH INHERIT FALSE, SET FALSE; GRANT TEMP ON DATABASE fixture TO reader; CREATE TEMP TABLE account_temp(id int); ALTER TABLE account_temp OWNER TO reader")
	_, err := d.Test(t.Context(), a)
	sql("REVOKE writer FROM reader; DROP TABLE account_temp")
	if err != nil {
		t.Fatal("read-only account with TEMP/unusable membership", err)
	}
}

// Observe actual audit executions and physical connection lifetime in the owned
// fixture, extending pool lifecycle coverage rather than adding timing assertions.
func poolAccountAcceptance(t *testing.T, a database.Access, admin *pgx.Conn, sql func(string, ...any)) {
	t.Helper()
	d := driver(t)
	var openedMu sync.Mutex
	var opened []*pgx.Conn
	d.connected = func(c *pgx.Conn) {
		openedMu.Lock()
		opened = append(opened, c)
		openedMu.Unlock()
	}
	a, rev, err := database.Normalize(a)
	if err != nil {
		t.Fatal(err)
	}
	audits := func() int64 {
		t.Helper()
		var n int64
		if err := admin.QueryRow(t.Context(), `SELECT COALESCE(sum(calls),0)::bigint FROM catalog.pg_stat_statements WHERE userid=(SELECT oid FROM pg_catalog.pg_roles WHERE rolname='reader') AND query LIKE 'WITH roles AS MATERIALIZED%'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := audits()
	releases := make(chan func(bool), 8)
	failures := make(chan error, 8)
	for range 8 {
		go func() {
			_, _, release, e := d.checkout(t.Context(), d.pools, a, rev, nil)
			if e == nil {
				releases <- release
			}
			failures <- e
		}()
	}
	for range 8 {
		if e := <-failures; e != nil {
			t.Fatal(e)
		}
	}
	for range 8 {
		(<-releases)(true)
	}
	if audits() != before+1 {
		t.Fatal("concurrent pool opening repeated or omitted audit")
	}
	query := func() error { _, e := d.Query(t.Context(), a, database.QueryRequest{SQL: "SELECT 1"}); return e }
	sql("GRANT UPDATE ON app.items TO reader")
	if err = query(); err != nil {
		t.Fatal("warm pool repeated privilege audit", err)
	}
	sql(`CREATE FUNCTION app.write_items() RETURNS int LANGUAGE plpgsql AS $$BEGIN UPDATE app.items SET name='changed'; RETURN 1; END$$`)
	_, err = d.Query(t.Context(), a, database.QueryRequest{SQL: "SELECT app.write_items()"})
	requireCode(t, err, contracts.ReadOnlyViolation)
	sql("DROP FUNCTION app.write_items()")
	_, err = d.Test(t.Context(), a)
	requireCode(t, err, contracts.ReadOnlyViolation)
	if audits() != before+2 {
		t.Fatal("fresh diagnostic reused pool approval")
	}
	// The first eight connections opened belong to the pool; close them client-side.
	openedMu.Lock()
	connections := append([]*pgx.Conn(nil), opened...)
	openedMu.Unlock()
	closeConn(connections[0])
	if err = query(); err != nil {
		t.Fatal("partial disconnect lost pool approval", err)
	}
	for _, c := range connections {
		closeConn(c)
	}
	err = query()
	requireCode(t, err, contracts.ReadOnlyViolation)
	if audits() != before+3 {
		t.Fatal("empty pool retained approval")
	}
	sql("REVOKE UPDATE ON app.items FROM reader")
	if err = query(); err != nil {
		t.Fatal("failed validation poisoned retry", err)
	}
	if audits() != before+4 {
		t.Fatal("retry missing audit")
	}
	// Indirect and explicit attempted writes still fail at the transaction boundary.
	for _, q := range []string{"SELECT app.policy_probe()", "SELECT * FROM app.rls"} {
		_, err = d.Query(t.Context(), a, database.QueryRequest{SQL: q})
		requireCode(t, err, contracts.ReadOnlyViolation)
	}
	var n int
	if err = admin.QueryRow(t.Context(), "SELECT n FROM hidden.audit").Scan(&n); err != nil || n != 0 {
		t.Fatal("routine modified persistent data", err)
	}
	// Retire a generation while its audit is blocked by an owned catalog lock.
	d.pools.Invalidate(a.Profile.ID)
	lock, err := admin.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Rollback(context.Background()) }()
	if _, err = lock.Exec(t.Context(), "LOCK pg_catalog.pg_largeobject_metadata IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 2)
	for range 2 {
		go func() { done <- query() }()
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		s := d.pools.Stats(a.Profile.ID)
		if s.Validating && s.Users == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("concurrent validation did not start")
		}
		runtime.Gosched()
	}
	d.pools.Invalidate(a.Profile.ID)
	if err = lock.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		select {
		case err = <-done:
			requireCode(t, err, contracts.Cancelled)
		case <-time.After(5 * time.Second):
			t.Fatal("validation waiter not cancelled")
		}
	}
	if err = query(); err != nil {
		t.Fatal("retired validation poisoned replacement", err)
	}
}

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
		r, err := pool.New(pool.Options{})
		if err != nil {
			t.Fatal(err)
		}
		d := New(r)
		validationStarted := time.Now()
		if _, err := d.Test(t.Context(), access); err != nil {
			r.Close()
			t.Fatal(err)
		}
		t.Logf("MEASURE operation=validation trial=%d iteration=0 ns=%d", trial, time.Since(validationStarted).Nanoseconds())
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
					r.Close()
					t.Fatal(err)
				}
				if operation == "query" && iteration == 0 {
					t.Logf("MEASURE operation=cold-query trial=%d iteration=0 ns=%d", trial, elapsed)
				}
				if iteration >= 2 {
					t.Logf("MEASURE operation=%s trial=%d iteration=%d ns=%d", operation, trial, iteration-2, elapsed)
				}
			}
		}
		r.Close()
		if trial >= 2 && time.Since(started) > 4*time.Minute {
			break
		}
	}
}
