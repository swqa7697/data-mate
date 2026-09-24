package sqlguard

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/swqa7697/data-mate/internal/config"
)

// Replaces the compiler boundary corpus: scope and writes remain guarded while
// expression, type and execution semantics are delegated to PostgreSQL.
func TestQueryGuard(t *testing.T) {
	scope := config.Scope{Mode: "selected", Schemas: []string{"app", "Dot.Schema"}}
	positive := []string{
		"SELECT 1", "VALUES (1),(2)", "TABLE app.items", "SELECT * FROM ONLY app.items",
		"SELECT count(*) FROM app.items", "SELECT ARRAY[1,NULL], E'escaped\\ntext', $$dollar; quoted$$",
		"SELECT count(*) OVER(),sum(id) FILTER(WHERE id>1) FROM app.items GROUP BY ROLLUP(id)",
		"SELECT * FROM app.items a JOIN app.items b USING(id)", "SELECT * FROM app.items NATURAL JOIN app.items b",
		"SELECT * FROM app.items a,LATERAL(SELECT a.id) b", "SELECT id FROM app.items a WHERE EXISTS(SELECT 1 FROM app.items b WHERE b.id=a.id)",
		"SELECT 1 UNION SELECT 2", "SELECT app.custom(1),id::app.custom FROM app.items",
		"WITH x AS (SELECT * FROM app.items),y AS (SELECT * FROM x) SELECT * FROM y",
		"WITH RECURSIVE x(n) AS (VALUES(1) UNION ALL SELECT n+1 FROM x WHERE n<3) SELECT * FROM x",
		"WITH RECURSIVE x AS (SELECT * FROM y), y AS (SELECT * FROM app.items) SELECT * FROM x",
		"WITH x AS (SELECT * FROM app.items) SELECT * FROM (WITH x AS (SELECT * FROM x) SELECT * FROM x) q",
		`SELECT * FROM "Dot.Schema"."a.b"`, "SELECT * FROM app.items TABLESAMPLE SYSTEM(10)",
	}
	for _, q := range positive {
		if err := Check(q, scope); err != nil {
			t.Errorf("accepted query %s: %v", q, err)
		}
	}
	negative := []string{
		"DELETE FROM app.items", "SELECT 1; SELECT 2", "SELECT * INTO app.copy FROM app.items", "SELECT * FROM app.items FOR UPDATE",
		"WITH x AS (DELETE FROM app.items RETURNING *) SELECT * FROM x", "WITH x AS (UPDATE app.items SET id=1 RETURNING *) SELECT * FROM x",
		"WITH x AS (INSERT INTO app.items VALUES(1) RETURNING *) SELECT * FROM x", "SET transaction_read_only=off", "COMMIT", "COPY app.items TO STDOUT", "EXPLAIN SELECT 1",
		"SELECT * FROM hidden.items", "SELECT * FROM items", "SELECT * FROM fixture.app.items", "SELECT * FROM information_schema.tables", "SELECT * FROM pg_catalog.pg_class",
		"SELECT * FROM app.items a JOIN hidden.items b ON true", "SELECT * FROM app.items WHERE EXISTS(SELECT 1 FROM hidden.items)",
		"WITH x AS (SELECT * FROM y),y AS (SELECT * FROM app.items) SELECT * FROM x",
		"SELECT * FROM (WITH x AS (SELECT 1) SELECT * FROM x) q,x",
		"WITH x AS (SELECT * FROM app.items) SELECT * FROM (WITH x AS (SELECT * FROM hidden.items) SELECT * FROM x) q",
		"WITH RECURSIVE x AS (SELECT * FROM hidden.items UNION ALL SELECT * FROM x) SELECT * FROM x",
		"SELECT * FROM (SELECT * FROM app.items FOR SHARE) q",
		strings.Repeat("(", 65) + "SELECT 1" + strings.Repeat(")", 65), strings.Repeat(" ", 65537), "SELECT " + strings.Repeat("1,", 8192) + "1",
	}
	for _, q := range negative {
		if err := Check(q, scope); err == nil {
			t.Errorf("rejected query accepted: %.200s", q)
		}
	}
	if Check("SELECT * FROM pg_catalog.pg_class", config.Scope{Mode: "all"}) == nil {
		t.Fatal("all exposes catalog")
	}
}

func FuzzQueryGuard(f *testing.F) {
	for _, q := range []string{"SELECT 1", "WITH RECURSIVE x AS (SELECT 1) SELECT * FROM x", "SELECT E'\\n'", "SELECT * FROM app.items"} {
		f.Add(q)
	}
	f.Fuzz(func(t *testing.T, q string) {
		if len(q) > 4096 {
			return
		}
		_ = Check(q, config.Scope{Mode: "all"})
	})
}

// The native C parser cannot be interrupted by a Go context. Run worst shapes
// in a killable child and bound the entire corpus, including quotes/comments.
func TestNativeParserBudget(t *testing.T) {
	if os.Getenv("DM_PARSER_CHILD") == "1" {
		corpus := []string{"SELECT " + strings.Repeat("- ", 8000) + "1", "SELECT " + strings.Repeat("NOT ", 8000) + "true", "SELECT " + strings.Repeat("1 + ", 2000) + "1", "SELECT " + strings.Repeat("(", 64) + "1" + strings.Repeat(")", 64), "SELECT $$" + strings.Repeat("(", 60000) + "$$::text", "/*" + strings.Repeat("/*", 64) + strings.Repeat("*/", 65) + "SELECT 1"}
		for _, s := range corpus {
			_ = Check(s, config.Scope{Mode: "all"})
		}
		return
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestNativeParserBudget$")
	cmd.Env = append(os.Environ(), "DM_PARSER_CHILD=1")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("native parser did not finish: %v %s", err, b)
	}
}
