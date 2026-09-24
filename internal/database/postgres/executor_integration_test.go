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
	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
)

func executorAcceptance(t *testing.T, d *Driver, a database.Access, admin *pgx.Conn, sql func(string, ...any)) {
	t.Helper()
	a = normalizedFixture(t, a)
	fixtures := codecFixtures(t)
	req := database.QueryRequest{}
	projections := []string{}
	expected := []json.RawMessage{}
	for i, f := range fixtures {
		projections = append(projections, fmt.Sprintf("$%d::%s AS c%d", i+1, pgx.Identifier{f.Type}.Sanitize(), i))
		req.Parameters = append(req.Parameters, fixtureParameter(f))
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
	rr := admin.PgConn().ExecParams(t.Context(), "CREATE TABLE app.codec_values AS "+req.SQL, params, nil, nil, nil)
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
		{SQL: "SELECT 1", RowLimit: 5001}, {SQL: "SELECT $1::int4"}, {SQL: "SELECT 1", Parameters: []json.RawMessage{json.RawMessage(`1`)}},
		{SQL: "SELECT $1::date", Parameters: []json.RawMessage{json.RawMessage(`"synthetic-private-invalid-date"`)}},
		{SQL: "SELECT 1/0"},
		{SQL: "SELECT 'synthetic-private-invalid-date'::date"},
	} {
		if _, err = d.Query(t.Context(), a, request); err == nil || strings.Contains(err.Error(), "synthetic-private") {
			t.Fatal("invalid input or redaction", err)
		}
	}
	executorSessionCleanup(t, d, a, sql)
	executorLimits(t, d, a, admin, sql)
	executorCancellation(t, d, a, admin, sql)
	t.Logf("Query execution: %d codecs as parameters and stored columns; row limits, RLS, session reset, DDL, resource and cancellation acceptance", len(fixtures))
}

// Extends executor cleanup coverage after removing compiler/lock rechecks.
func executorSessionCleanup(t *testing.T, d *Driver, a database.Access, sql func(string, ...any)) {
	t.Helper()
	sql("CREATE TABLE app.changing(id int); INSERT INTO app.changing VALUES(1); GRANT SELECT ON app.changing TO reader")
	_, err := d.Query(t.Context(), a, database.QueryRequest{SQL: "SELECT pg_catalog.pg_advisory_lock(174937),pg_catalog.set_config('TimeZone','Asia/Tokyo',false)"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := d.Query(t.Context(), a, database.QueryRequest{SQL: "SELECT pg_catalog.pg_advisory_unlock(174937),pg_catalog.current_setting('TimeZone')"})
	if err != nil || r.Rows[0][0] != false || r.Rows[0][1] != "UTC" {
		t.Fatal("session leaked", r.Rows, err)
	}
	sql("REVOKE SELECT ON app.changing FROM reader")
	_, err = d.Query(t.Context(), a, database.QueryRequest{SQL: "SELECT * FROM app.changing"})
	requireCode(t, err, contracts.PermissionDenied)
	sql("GRANT SELECT ON app.changing TO reader")
	sql("ALTER TABLE app.changing ADD COLUMN state app.custom DEFAULT 'one'")
	r, err = d.Query(t.Context(), a, database.QueryRequest{SQL: "SELECT * FROM app.changing"})
	if err != nil || len(r.Columns) != 2 {
		t.Fatal("fresh shape", err)
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
	// query, without depending on query cost.
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
