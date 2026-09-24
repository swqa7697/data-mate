package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
	"github.com/swqa7697/data-mate/internal/database/postgres/sqlpolicy"
)

// All P5 helpers extend TestPostgresIntegration's owned fixture and lifecycle.
// This spy refuses the only connection seam used to execute compiled SQL.
type executionSpy struct {
	pgx.Tx
	connections, prepares int
	onCatalog             func()
}

func (s *executionSpy) QueryRow(ctx context.Context, q string, args ...any) pgx.Row {
	if q == sqlpolicy.CatalogFingerprintSQL && s.onCatalog != nil {
		s.onCatalog()
		s.onCatalog = nil
	}
	return s.Tx.QueryRow(ctx, q, args...)
}
func (s *executionSpy) Conn() *pgx.Conn { s.connections++; return s.Tx.Conn() }
func (s *executionSpy) Prepare(ctx context.Context, n, q string) (*pgconn.StatementDescription, error) {
	s.prepares++
	return s.Tx.Prepare(ctx, n, q)
}

func executorAcceptance(t *testing.T, d *Driver, a database.Access, admin *pgx.Conn, sql func(string, ...any)) {
	t.Helper()
	a = normalizedFixture(t, a)
	fixtures := codecFixtures(t)
	req := database.QueryRequest{}
	projections := []string{}
	expected := []json.RawMessage{}
	for i, f := range fixtures {
		projections = append(projections, fmt.Sprintf("$%d::%s AS c%d", i+1, pgx.Identifier{f.Type}.Sanitize(), i))
		req.Parameters = append(req.Parameters, database.QueryParameter{Type: f.Type, Value: f.JSON})
		expected = append(expected, f.JSON)
	}
	req.SQL = "SELECT " + strings.Join(projections, ",")
	result, err := d.Query(t.Context(), a, req)
	if err != nil {
		t.Fatal("codec parameters", err)
	}
	want, _ := json.Marshal(expected)
	got, _ := json.Marshal(result.Rows[0])
	if string(got) != string(want) {
		t.Fatalf("codec parameters: %s != %s", got, want)
	}
	// Materialize all supported types under the fixture owner, then scan through
	// the production relation/column boundary as the read-only role.
	params, e := queryParameters(req.Parameters)
	if e != nil {
		t.Fatal(e)
	}
	values := make([][]byte, len(params))
	oids := make([]uint32, len(params))
	for i, p := range params {
		oids[i] = uint32(p.Type)
		if p.Value != nil {
			values[i] = []byte(*p.Value)
		}
	}
	rr := admin.PgConn().ExecParams(t.Context(), "CREATE TABLE app.codec_values AS "+req.SQL, values, oids, nil, nil)
	if _, e = rr.Close(); e != nil {
		t.Fatal("codec fixture", e)
	}
	sql("GRANT SELECT ON app.codec_values TO reader")
	result, err = d.Query(t.Context(), a, database.QueryRequest{SQL: "SELECT * FROM app.codec_values"})
	if err != nil {
		t.Fatal("codec table", err)
	}
	got, _ = json.Marshal(result.Rows[0])
	if string(got) != string(want) {
		t.Fatalf("codec table: %s != %s", got, want)
	}
	for i, f := range fixtures {
		if result.Columns[i].Encoding != f.Encoding {
			t.Fatalf("encoding %s", f.Name)
		}
	}
	// Duplicate members/numbers remain raw, and UTC comes from an explicit session.
	result, err = d.Query(t.Context(), a, database.QueryRequest{SQL: `SELECT '{"n":9007199254740993,"n":9007199254740994}'::json, '2026-09-22 15:34:56+03'::timestamptz`})
	if err != nil {
		t.Fatal(err)
	}
	got, _ = json.Marshal(result.Rows[0])
	if !strings.Contains(string(got), `9007199254740993,"n":9007199254740994`) || result.Rows[0][1] != "2026-09-22T12:34:56Z" {
		t.Fatal("JSON/UTC fidelity", string(got))
	}
	// RLS executes with the non-owner reader identity, and write-bearing policy
	// code fails inside the read-only transaction with no persisted audit mutation.
	result, err = d.Query(t.Context(), a, database.QueryRequest{SQL: "SELECT id FROM app.safe_rls"})
	if err != nil || result.RowCount != 1 || result.Rows[0][0] != int64(1) {
		t.Fatal("RLS filter", err)
	}
	if _, err = d.Query(t.Context(), a, database.QueryRequest{SQL: "SELECT * FROM app.rls"}); err == nil {
		t.Fatal("RLS write succeeded")
	}
	var count int
	if err = admin.QueryRow(t.Context(), "SELECT n FROM hidden.audit").Scan(&count); err != nil || count != 0 {
		t.Fatal("RLS mutated audit", err)
	}
	for _, test := range []struct {
		q           string
		limit, rows int
		truncated   bool
	}{
		{"SELECT id FROM app.items ORDER BY id", 2, 2, true},
		{"SELECT id FROM app.items ORDER BY id LIMIT 1", 2, 1, false},
		{"SELECT id FROM app.items ORDER BY id LIMIT 0", 2, 0, false},
		{"SELECT id FROM app.items ORDER BY id LIMIT 2", 2, 2, false},
		{"SELECT id FROM app.items ORDER BY id LIMIT 3 OFFSET 1", 2, 2, true},
		{"SELECT id FROM (SELECT id FROM app.items ORDER BY id LIMIT 3) x ORDER BY id", 2, 2, true},
	} {
		result, err = d.Query(t.Context(), a, database.QueryRequest{SQL: test.q, RowLimit: test.limit})
		if err != nil || result.RowCount != test.rows || result.Truncated != test.truncated {
			t.Fatalf("row budget %s: %+v %v", test.q, result, err)
		}
	}
	for _, request := range []database.QueryRequest{
		{SQL: "SELECT 1", RowLimit: 5001}, {SQL: "SELECT $1::int4"}, {SQL: "SELECT 1", Parameters: []database.QueryParameter{{Type: "int4", Value: json.RawMessage(`1`)}}},
		{SQL: "SELECT $1::date", Parameters: []database.QueryParameter{{Type: "date", Value: json.RawMessage(`"synthetic-private-invalid-date"`)}}},
		{SQL: "SELECT 1/0"},
		{SQL: "SELECT 'synthetic-private-invalid-date'::date"},
	} {
		if _, err = d.Query(t.Context(), a, request); err == nil || strings.Contains(err.Error(), "synthetic-private") {
			t.Fatal("invalid input or redaction", err)
		}
	}
	executorRechecks(t, d, a, admin, sql)
	executorLimits(t, d, a, admin, sql)
	executorCancellation(t, d, a, admin, sql)
	t.Logf("P5 execution: %d codecs as parameters and stored columns; row limits, RLS, rechecks, DDL, resource and cancellation acceptance", len(fixtures))
}

func executorRechecks(t *testing.T, d *Driver, a database.Access, admin *pgx.Conn, sql func(string, ...any)) {
	t.Helper()
	sql(`CREATE TABLE app.changing(id int); INSERT INTO app.changing VALUES(1); GRANT SELECT ON app.changing TO reader;
 CREATE TABLE app.tree(id int) PARTITION BY RANGE(id); CREATE TABLE hidden.leaf PARTITION OF app.tree FOR VALUES FROM(0) TO(10); INSERT INTO app.tree VALUES(1);
 CREATE TABLE hidden.new_leaf(id int);INSERT INTO hidden.new_leaf VALUES(11);
 GRANT SELECT ON app.tree,hidden.leaf,hidden.new_leaf TO reader;`)
	// Mutations happen after a successful compilation and before rechecking. Each
	// rejected plan must fail before access to the executor's raw protocol seam.
	for _, test := range []struct{ name, q, before, after string }{
		{"recreate", "SELECT * FROM app.changing", "DROP TABLE app.changing;CREATE TABLE app.changing(id int);GRANT SELECT ON app.changing TO reader", ""},
		{"rename", "SELECT * FROM app.changing", "ALTER TABLE app.changing RENAME TO renamed;CREATE TABLE app.changing(id int);GRANT SELECT ON app.changing TO reader", "DROP TABLE app.changing;ALTER TABLE app.renamed RENAME TO changing"},
		{"column", "SELECT * FROM app.changing", "ALTER TABLE app.changing ALTER COLUMN id TYPE bigint", "ALTER TABLE app.changing ALTER COLUMN id TYPE int"},
		{"privilege", "SELECT * FROM app.changing", "REVOKE SELECT ON app.changing FROM reader", "GRANT SELECT ON app.changing TO reader"},
		{"role", "SELECT * FROM app.changing", "GRANT UPDATE ON app.changing TO reader", "REVOKE UPDATE ON app.changing FROM reader"},
		{"attach", "SELECT * FROM app.tree", "ALTER TABLE app.tree ATTACH PARTITION hidden.new_leaf FOR VALUES FROM(10) TO(20)", "ALTER TABLE app.tree DETACH PARTITION hidden.new_leaf"},
		{"detach", "SELECT * FROM app.tree", "ALTER TABLE app.tree DETACH PARTITION hidden.leaf", "ALTER TABLE app.tree ATTACH PARTITION hidden.leaf FOR VALUES FROM(0) TO(10)"},
		{"signature no relations", "SELECT 1", "ALTER FUNCTION pg_catalog.int4pl(int4,int4) CALLED ON NULL INPUT", "ALTER FUNCTION pg_catalog.int4pl(int4,int4) RETURNS NULL ON NULL INPUT"},
		{"signature with relations", "SELECT * FROM app.changing", "ALTER FUNCTION pg_catalog.int4pl(int4,int4) CALLED ON NULL INPUT", "ALTER FUNCTION pg_catalog.int4pl(int4,int4) RETURNS NULL ON NULL INPUT"},
	} {
		err := d.run(t.Context(), a, func(ctx context.Context, tx pgx.Tx, v int) error {
			p, e := sqlpolicy.Parse(test.q)
			if e != nil {
				return e
			}
			plan, e := compileSQL(ctx, tx, a.Profile.Scope, v, p, nil)
			if e != nil {
				return e
			}
			spy := &executionSpy{Tx: tx}
			if strings.HasPrefix(test.name, "signature") {
				spy.onCatalog = func() { sql(test.before) }
			} else {
				sql(test.before)
			}
			e = recheckPlan(ctx, spy, a, v, plan)
			if e == nil {
				_, e = executePlan(ctx, spy, a, plan, 500, time.Now())
				t.Fatalf("stale %s reached execution: %v", test.name, e)
			}
			if spy.connections != 0 || spy.prepares != 0 {
				t.Fatalf("stale %s prepared/executed", test.name)
			}
			return nil
		})
		if test.after != "" {
			sql(test.after)
		}
		if err != nil {
			t.Fatalf("recheck %s: %v", test.name, err)
		}
	}
	// A profile's scope is re-applied rather than inferred from the compiled plan.
	err := d.run(t.Context(), a, func(ctx context.Context, tx pgx.Tx, v int) error {
		p, _ := sqlpolicy.Parse("SELECT * FROM app.changing")
		plan, e := compileSQL(ctx, tx, a.Profile.Scope, v, p, nil)
		if e != nil {
			return e
		}
		none := a.Profile
		none.Scope = config.Scope{Mode: "selected"}
		e = recheckPlan(ctx, tx, database.NewAccess(none, a.Password()), v, plan)
		requireCode(t, e, contracts.ScopeDenied)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Attach after the final recheck is legal under ACCESS SHARE. Frozen ONLY
	// scans must exclude the newly attached child until the following request.
	err = d.run(t.Context(), a, func(ctx context.Context, tx pgx.Tx, v int) error {
		p, _ := sqlpolicy.Parse("SELECT id FROM app.tree ORDER BY id")
		plan, e := p.CompileBounded(ctx, v/10000, &compilerCatalog{tx: tx, scope: a.Profile.Scope}, nil, 501)
		if e != nil {
			return e
		}
		if e = recheckPlan(ctx, tx, a, v, plan); e != nil {
			return e
		}
		sql("ALTER TABLE app.tree ATTACH PARTITION hidden.new_leaf FOR VALUES FROM(10) TO(20)")
		out, e := executePlan(ctx, tx, normalizedFixture(t, a), plan, 500, time.Now())
		if e != nil {
			return e
		}
		if out.RowCount != 1 || out.Rows[0][0] != int64(1) {
			t.Fatal("post-check attach entered frozen scan")
		}
		return nil
	})
	if err != nil {
		t.Fatal("post-check attach", err)
	}
	out, err := d.Query(t.Context(), a, database.QueryRequest{SQL: "SELECT id FROM app.tree ORDER BY id"})
	if err != nil || out.RowCount != 2 {
		t.Fatal("next request missed new partition", err)
	}
	// Ordinary inheritance requires explicit descendant scope on every request.
	sql("CREATE TABLE app.plain(id int);INSERT INTO app.plain VALUES(1)")
	sql("GRANT SELECT ON app.plain TO reader")
	out, err = d.Query(t.Context(), a, database.QueryRequest{SQL: "SELECT * FROM app.plain"})
	if err != nil || out.RowCount != 1 {
		t.Fatal(err)
	}
	err = d.run(t.Context(), a, func(ctx context.Context, tx pgx.Tx, v int) error {
		p, _ := sqlpolicy.Parse("SELECT * FROM app.plain")
		plan, e := p.CompileBounded(ctx, v/10000, &compilerCatalog{tx: tx, scope: a.Profile.Scope}, nil, 501)
		if e != nil {
			return e
		}
		if e = recheckPlan(ctx, tx, a, v, plan); e != nil {
			return e
		}
		sql("CREATE TABLE hidden.plain_child() INHERITS(app.plain);INSERT INTO hidden.plain_child VALUES(2);GRANT SELECT ON hidden.plain_child TO reader")
		out, e := executePlan(ctx, tx, a, plan, 500, time.Now())
		if e != nil {
			return e
		}
		if out.RowCount != 1 || out.Rows[0][0] != int64(1) {
			t.Fatal("post-check inheritance leaked child")
		}
		return nil
	})
	if err != nil {
		t.Fatal("post-check inheritance", err)
	}
	_, err = d.Query(t.Context(), a, database.QueryRequest{SQL: "SELECT * FROM app.plain"})
	requireCode(t, err, contracts.QueryUnsupported)
	out, err = d.Query(t.Context(), a, database.QueryRequest{SQL: "SELECT * FROM ONLY app.plain"})
	if err != nil || out.RowCount != 1 {
		t.Fatal("ONLY scope", err)
	}
	executorLockRace(t, d, a, admin, sql)
}

func normalizedFixture(t *testing.T, a database.Access) database.Access {
	t.Helper()
	a, _, err := normalized(a)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func executorLockRace(t *testing.T, d *Driver, a database.Access, admin *pgx.Conn, sql func(string, ...any)) {
	t.Helper()
	// A writer holds ACCESS EXCLUSIVE while a reader waits for its lock. The
	// post-wait snapshot must see a committed column change and reject the plan.
	err := d.run(t.Context(), a, func(ctx context.Context, tx pgx.Tx, v int) error {
		p, _ := sqlpolicy.Parse("SELECT * FROM app.changing")
		plan, e := compileSQL(ctx, tx, a.Profile.Scope, v, p, nil)
		if e != nil {
			return e
		}
		sql("BEGIN;LOCK TABLE app.changing IN ACCESS EXCLUSIVE MODE;ALTER TABLE app.changing ADD COLUMN newer int")
		defer func() { _, _ = admin.Exec(context.Background(), "ROLLBACK") }()
		finished := make(chan error, 1)
		pid := tx.Conn().PgConn().PID()
		go func() { finished <- recheckPlan(ctx, tx, a, v, plan) }()
		// The observer is the writer's own session; relation locks do not block its
		// pg_locks read. No sleeps or timing guesses select the mutation window.
		deadline := time.Now().Add(750 * time.Millisecond)
		for {
			var waiting bool
			if e = admin.QueryRow(ctx, "SELECT EXISTS(SELECT FROM pg_catalog.pg_locks WHERE pid=$1 AND NOT granted)", pid).Scan(&waiting); e != nil {
				return e
			}
			if waiting {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("reader failed to wait on DDL lock")
			}
			runtime.Gosched()
		}
		sql("COMMIT")
		select {
		case e = <-finished:
			requireCode(t, safeError(e), contracts.QueryUnsupported)
		case <-ctx.Done():
			return ctx.Err()
		}
		return nil
	})
	if err != nil {
		t.Fatal("lock race", err)
	}
}

func executorLimits(t *testing.T, d *Driver, a database.Access, admin *pgx.Conn, sql func(string, ...any)) {
	t.Helper()
	sql(`CREATE TABLE app.large(id int,value text);INSERT INTO app.large SELECT i,repeat('x',10000) FROM generate_series(1,200) i;GRANT SELECT ON app.large TO reader`)
	small := a.Profile
	l := config.DefaultLimits()
	l.MaxResultBytes = 1024
	small.Limits = &l
	bounded := database.NewAccess(small, a.Password())
	out, err := d.Query(t.Context(), bounded, database.QueryRequest{SQL: "SELECT value FROM app.large"})
	if err != nil || !out.Truncated || out.RowCount != 0 {
		t.Fatal("whole row omission", err)
	}
	size, _ := QueryPayloadSize(out)
	if size > 1024 {
		t.Fatal("result exceeded byte cap")
	}
	_, err = d.Query(t.Context(), bounded, database.QueryRequest{SQL: "SELECT * FROM app.codec_values"})
	requireCode(t, err, contracts.ResourceLimit)
	// Quoting, HTML escaping, base64 and compatibility text all count. A row that
	// fits alone need not fit beside the preceding row.
	sql(`CREATE TABLE app.escaped(id int,value text,raw bytea);INSERT INTO app.escaped VALUES(1,repeat('"',60),decode(repeat('ff',30),'hex')),(2,repeat('<',60),decode(repeat('ff',30),'hex'));GRANT SELECT ON app.escaped TO reader`)
	out, err = d.Query(t.Context(), bounded, database.QueryRequest{SQL: "SELECT * FROM app.escaped ORDER BY id"})
	if err != nil || !out.Truncated || out.RowCount != 1 {
		t.Fatalf("escaped byte budget: rows=%d truncated=%v %v", out.RowCount, out.Truncated, err)
	}
	size, _ = QueryPayloadSize(out)
	if size > 1024 {
		t.Fatal("escaped payload overflow")
	}
	for _, value := range []string{strings.Repeat("x", (2<<20)+1), strings.Repeat("[", 65) + "0" + strings.Repeat("]", 65)} {
		sql("UPDATE app.large SET value=$1 WHERE id=1", value)
		q := "SELECT value FROM app.large WHERE id=1"
		if value[0] == '[' {
			q = "SELECT '" + value + "'::json"
		}
		_, err = d.Query(t.Context(), a, database.QueryRequest{SQL: q})
		requireCode(t, err, contracts.ResourceLimit)
	}
	sql("UPDATE app.large SET value=repeat('x',10000) WHERE id=1")
	// Two cells individually fit the protocol cap; their row does not.
	sql("UPDATE app.large SET value=repeat('x',1100000) WHERE id=1")
	_, err = d.Query(t.Context(), a, database.QueryRequest{SQL: "SELECT value,value FROM app.large WHERE id=1"})
	requireCode(t, err, contracts.ResourceLimit)
	sql("UPDATE app.large SET value=repeat('x',10000) WHERE id=1")
	executorMemory(t, d, a)
	var idle int
	if err = admin.QueryRow(t.Context(), "SELECT count(*) FROM pg_catalog.pg_stat_activity WHERE usename='reader' AND state LIKE 'idle in transaction%'").Scan(&idle); err != nil || idle != 0 {
		t.Fatal("idle transaction after limits", err)
	}
}

func executorMemory(t *testing.T, d *Driver, a database.Access) {
	t.Helper()
	// Hold one completed maximum-budget result as a slow consumer while preparing
	// successive results. Driver admission/leases must already have been released.
	var baseline runtime.MemStats
	var held database.QueryResult
	samples := []uint64{}
	rssSamples := []int64{}
	queryMicros := []int64{}
	var baselineRSS int64
	started := time.Now()
	for i := range 24 {
		queryStarted := time.Now()
		out, err := d.Query(t.Context(), a, database.QueryRequest{SQL: "SELECT value FROM app.large"})
		queryMicros = append(queryMicros, time.Since(queryStarted).Microseconds())
		if err != nil || !out.Truncated || out.RowCount < 40 {
			t.Fatal("maximum result fixture", err)
		}
		n, _ := QueryPayloadSize(out)
		if n > config.DefaultLimits().MaxResultBytes {
			t.Fatal("payload overflow")
		}
		if i == 0 {
			held = out
		}
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		rssRaw, e := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(os.Getpid())).Output()
		if e != nil {
			t.Fatal("RSS measurement", e)
		}
		rss, e := strconv.ParseInt(strings.TrimSpace(string(rssRaw)), 10, 64)
		if e != nil {
			t.Fatal(e)
		}
		rss *= 1024
		rssSamples = append(rssSamples, rss)
		if i <= 5 && rss > baselineRSS {
			baselineRSS = rss
		}
		// Eight admitted requests, each allowing two wire buffers and two result
		// representations, define the warmed RSS headroom (48 MiB).
		if i >= 6 && rss > baselineRSS+8*(2*(2<<20)+2*(1<<20)) {
			t.Fatal("RSS did not plateau")
		}
		if i == 5 {
			baseline = m
		}
		if i >= 6 {
			samples = append(samples, m.HeapAlloc)
		}
		// Allow four bounded wire bodies per active request plus two output copies.
		// This threshold derives from the warmed fixture heap and protocol/result caps.
		if i >= 6 && m.HeapAlloc > baseline.HeapAlloc+4*(2<<20)+2*(1<<20) {
			t.Fatal("retained heap did not plateau")
		}
	}
	runtime.KeepAlive(held)
	t.Logf("P5 query timings: microseconds=%v", queryMicros)
	t.Logf("P5 RSS: bytes=%v warm_max=%d ceiling=%d", rssSamples, baselineRSS, baselineRSS+48*(1<<20))
	t.Logf("P5 memory: warm_heap=%d later_heap=%v ceiling=%d trials=24 elapsed=%s", baseline.HeapAlloc, samples, baseline.HeapAlloc+10*(1<<20), time.Since(started))
}

func executorCancellation(t *testing.T, d *Driver, a database.Access, admin *pgx.Conn, sql func(string, ...any)) {
	t.Helper()
	// RLS is trusted server code: use a sleeping policy to observe a real executing
	// query, rather than allowing pg_sleep in user SQL or depending on query cost.
	sql(`CREATE TABLE app.slow(id int);INSERT INTO app.slow VALUES(1);ALTER TABLE app.slow ENABLE ROW LEVEL SECURITY;
 CREATE FUNCTION app.slow_policy() RETURNS boolean LANGUAGE plpgsql AS $$BEGIN PERFORM pg_catalog.pg_sleep(30);RETURN true;END$$;
 CREATE POLICY slow ON app.slow USING(app.slow_policy());GRANT SELECT ON app.slow TO reader`)
	for _, mode := range []string{"cancel", "invalidate", "timeout"} {
		p := a.Profile
		l := config.DefaultLimits()
		if mode == "timeout" {
			l.QueryTimeoutMS = 1000
		}
		p.Limits = &l
		access := database.NewAccess(p, a.Password())
		ctx, cancel := context.WithCancel(t.Context())
		finished := make(chan error, 1)
		go func() {
			out, e := d.Query(ctx, access, database.QueryRequest{SQL: "SELECT * FROM app.slow"})
			if out.Rows != nil {
				finished <- fmt.Errorf("partial result on cancellation")
			} else {
				finished <- e
			}
		}()
		deadline := time.Now().Add(5 * time.Second)
		var pid uint32
		for {
			err := admin.QueryRow(t.Context(), "SELECT COALESCE((SELECT pid FROM pg_catalog.pg_stat_activity WHERE usename='reader' AND wait_event='PgSleep' LIMIT 1),0)").Scan(&pid)
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			if pid != 0 {
				break
			}
			if time.Now().After(deadline) {
				cancel()
				t.Fatal("query did not start sleeping")
			}
			runtime.Gosched()
		}
		if mode == "invalidate" {
			d.Invalidate(p.ID)
		} else if mode == "cancel" {
			cancel()
		}
		select {
		case err := <-finished:
			code := contracts.Cancelled
			if mode == "timeout" {
				code = contracts.QueryTimeout
			}
			requireCode(t, err, code)
		case <-time.After(5 * time.Second):
			cancel()
			t.Fatal("cancellation failed")
		}
		cancel()
		waitBackendStopped(t, admin, pid)
		if _, err := d.Query(t.Context(), a, database.QueryRequest{SQL: "SELECT 1"}); err != nil {
			t.Fatal("query after cancellation", err)
		}
	}
	// Relation lock deadline is independently bounded to one second.
	sql("BEGIN;LOCK TABLE app.changing IN ACCESS EXCLUSIVE MODE")
	_, err := d.Query(t.Context(), a, database.QueryRequest{SQL: "SELECT * FROM app.changing"})
	sql("ROLLBACK")
	requireCode(t, err, contracts.QueryTimeout)
}

// PostgreSQL can process cancellation/EOF just after the caller returns. This
// shared bounded observation verifies actual server cleanup for every route.
func waitBackendStopped(t *testing.T, admin *pgx.Conn, pid uint32) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		var active bool
		if err := admin.QueryRow(t.Context(), "SELECT EXISTS(SELECT FROM pg_catalog.pg_stat_activity WHERE pid=$1 AND (state='active' OR state LIKE 'idle in transaction%'))", pid).Scan(&active); err != nil {
			t.Fatal(err)
		}
		if !active {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("backend survived cancellation")
		}
		runtime.Gosched()
	}
}
