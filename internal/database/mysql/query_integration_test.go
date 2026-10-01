package mysql

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
)

// The corpus proves on each server that the guard's accepted statements read,
// rejected and server-refused statements fail safely, and sessions stay clean.
func queryAcceptance(t *testing.T, f *fixture, d *Driver, a database.Access) {
	t.Helper()
	query := func(q string, params ...any) (database.QueryResult, error) {
		raw := make([]json.RawMessage, len(params))
		for i, p := range params {
			raw[i], _ = json.Marshal(p)
		}
		return d.Query(t.Context(), a, database.QueryRequest{SQL: q, Parameters: raw})
	}
	accepted := []string{
		"SELECT COUNT(*) AS n FROM app.items",
		"SELECT id FROM hidden.target",
		"WITH x AS (SELECT 1 AS n) SELECT n FROM x",
		"SELECT 1;",
		"SHOW TABLES FROM app",
		"SHOW CREATE TABLE app.items",
		"EXPLAIN SELECT * FROM app.items",
		"DESCRIBE app.items",
		"SELECT * FROM `Dot.Schema`.`a.b`",
		"SELECT app.double_it(21)",
	}
	if f.flavor == MySQL {
		accepted = append(accepted, "EXPLAIN ANALYZE SELECT * FROM app.items", "TABLE hidden.target", "VALUES ROW(1, 'a')")
	} else {
		accepted = append(accepted, "ANALYZE SELECT * FROM app.items", "VALUES (1, 'a')")
	}
	for _, q := range accepted {
		if _, err := query(q); err != nil {
			t.Fatalf("%q: %v", q, err)
		}
	}
	result, err := query("SELECT name, amount, ? AS flag FROM app.items WHERE id = ? AND amount > ?", true, 1, "0.5")
	// A bound integer is BIGINT, which is an exact string like every 64-bit value.
	if err != nil || result.RowCount != 1 || result.Rows[0][0] != "one" || result.Rows[0][1] != "1.50" || fmt.Sprint(result.Rows[0][2]) != "1" {
		t.Fatalf("parameters: %#v %v", result.Rows, err)
	}
	for _, c := range []struct {
		q      string
		params []any
		code   contracts.Code
	}{
		// MySQL-family connections have no default database.
		{"SELECT * FROM items", nil, contracts.InvalidArgument},
		{"SELECT * FROM secret.hidden_rows", nil, contracts.PermissionDenied},
		// A routine's write fails at the read-only transaction boundary.
		{"SELECT app.bump()", nil, contracts.ReadOnlyViolation},
		{"DELETE FROM app.items", nil, contracts.ReadOnlyViolation},
		{"SELECT @x := 1", nil, contracts.ReadOnlyViolation},
		{"SELECT 1 INTO @x", nil, contracts.ReadOnlyViolation},
		{"SELECT * FROM app.items FOR UPDATE", nil, contracts.ReadOnlyViolation},
		{"SELECT * FROM app.items INTO OUTFILE '/tmp/data-mate'", nil, contracts.ReadOnlyViolation},
		{"SELECT ?", nil, contracts.InvalidArgument},
		{"SELECT 1", []any{1}, contracts.InvalidArgument},
		{"SELECT 1 /*!50000 , 2 */", nil, contracts.QueryUnsupported},
	} {
		_, err := query(c.q, c.params...)
		requireCode(t, err, c.code)
	}
	if _, err = query("SELECT * FROM items"); !strings.Contains(err.Error(), "database.table") {
		t.Fatal("unqualified table hint missing", err)
	}
	if f.scalar(t, "SELECT n FROM app.counter_log") != "0" {
		t.Fatal("routine modified persistent data")
	}
	// Cleanup releases named locks before the connection returns to the pool.
	result, err = query("SELECT GET_LOCK('data-mate-fixture', 0)")
	if err != nil || fmt.Sprint(result.Rows[0][0]) != "1" || f.scalar(t, "SELECT IS_FREE_LOCK('data-mate-fixture')") != "1" {
		t.Fatal("named lock survived cleanup", err)
	}
	// Row and byte limits return complete rows and close instead of draining.
	result, err = d.Query(t.Context(), a, database.QueryRequest{SQL: "SELECT id FROM app.items ORDER BY id", RowLimit: 3})
	if err != nil || !result.Truncated || result.RowCount != 3 {
		t.Fatalf("row limit: %+v %v", result, err)
	}
	small := a
	limits := config.DefaultLimits()
	limits.MaxResultBytes = 2048
	small.Profile.Limits = &limits
	result, err = d.Query(t.Context(), small, database.QueryRequest{SQL: "SELECT REPEAT('x', 500) FROM app.items"})
	if size, _ := database.QueryPayloadSize(result); err != nil || !result.Truncated || result.RowCount == 0 || result.RowCount == 10 || size > 2048 {
		t.Fatalf("byte limit: %d rows %v", result.RowCount, err)
	}
	// One value beyond the receive budget fails without buffering it whole.
	_, err = query("SELECT REPEAT('x', 9 * 1024 * 1024)")
	requireCode(t, err, contracts.ResourceLimit)
	if _, err = query("SELECT 1"); err != nil {
		t.Fatal("pool did not recover after receive limit", err)
	}
	// Cancellation interrupts server-side work through KILL QUERY. A deadline is
	// also pinned as max_execution_time, which stops SLEEP without an error, so
	// cancel explicitly: only the side connection can stop this statement.
	ctx, cancel := context.WithCancel(t.Context())
	stop := time.AfterFunc(300*time.Millisecond, cancel)
	defer stop.Stop()
	_, err = d.Query(ctx, a, database.QueryRequest{SQL: "SELECT SLEEP(30)"})
	requireCode(t, err, contracts.Cancelled)
	deadline := time.Now().Add(5 * time.Second)
	for f.scalar(t, "SELECT COUNT(*) FROM information_schema.PROCESSLIST WHERE USER='reader' AND COMMAND='Query' AND INFO LIKE 'SELECT SLEEP%'") != "0" {
		if time.Now().After(deadline) {
			t.Fatal("server query survived cancellation")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err = query("SELECT 1"); err != nil {
		t.Fatal("pool did not recover after cancellation", err)
	}
}

// The codec corpus is stored in typed columns and read back through query on
// every server, proving exact representations and reported type names.
func codecAcceptance(t *testing.T, f *fixture, d *Driver, a database.Access) {
	t.Helper()
	var columns, literals []string
	var cases []codecFixture
	for _, c := range codecFixtures(t) {
		if c.appliesTo(f.flavor) {
			columns = append(columns, "c"+strconv.Itoa(len(cases))+" "+c.Column)
			literals = append(literals, c.Literal)
			cases = append(cases, c)
		}
	}
	f.sql(t, "CREATE TABLE app.codec_values("+strings.Join(columns, ",")+")", "INSERT INTO app.codec_values VALUES ("+strings.Join(literals, ",")+")")
	result, err := d.Query(t.Context(), a, database.QueryRequest{SQL: "SELECT * FROM app.codec_values"})
	if err != nil || result.RowCount != 1 || len(result.Columns) != len(cases) {
		t.Fatalf("codec table: %+v %v", result.Columns, err)
	}
	for i, c := range cases {
		got, _ := json.Marshal(result.Rows[0][i])
		column := result.Columns[i]
		if compactJSON(t, got) != compactJSON(t, c.JSON) || column.Type != c.Type || column.Encoding != c.Encoding {
			t.Errorf("%s: %s as %s/%s", c.Name, got, column.Type, column.Encoding)
		}
	}
	encoded, _ := json.Marshal(result)
	if err = contracts.Validate("query.output", encoded); err != nil {
		t.Fatal("query contract", err)
	}
}
