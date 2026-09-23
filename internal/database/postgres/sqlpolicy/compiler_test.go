package sqlpolicy

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

type fixtureCatalog struct{ calls int }

func (f *fixtureCatalog) Resolve(_ context.Context, s, n string, only bool) (Relation, error) {
	f.calls++
	if s != "app" || n != "items" {
		return Relation{}, unsupported()
	}
	return Relation{Schema: s, Name: n, OID: 42, Columns: []Column{{"id", 23}, {"name", 25}, {"amount", 1700}}, Only: only}, nil
}
func compileFixture(sql string) (Compiled, error) {
	p, err := Parse(sql)
	if err != nil {
		return Compiled{}, err
	}
	return p.Compile(context.Background(), 16, &fixtureCatalog{}, nil)
}

// Step 3 of the regression ladder: previous tests never compile SQL. This owns
// lexical/typed rejection and emission; live semantics extend the existing PG test.
func TestCompilerBoundary(t *testing.T) {
	positive := []string{
		"SELECT 1", "SELECT id, name FROM app.items WHERE id >= 2 ORDER BY id DESC LIMIT 3", "SELECT a.*, a.id FROM app.items a",
		"SELECT count(*), sum(id), avg(amount), min(name), max(id) FROM app.items",
		"SELECT id, count(DISTINCT name) FROM app.items GROUP BY id HAVING count(*) > 0 ORDER BY 1",
		"WITH x AS (SELECT id FROM app.items) SELECT id FROM x", "SELECT x.id FROM (SELECT id FROM app.items) x",
		"SELECT a.id,b.name FROM app.items a LEFT JOIN app.items b ON a.id=b.id",
		"SELECT id FROM app.items WHERE id BETWEEN 1 AND 3 AND name IN ('a','b')",
		"SELECT id FROM app.items WHERE EXISTS (SELECT id FROM app.items) AND id IN (SELECT id FROM app.items)",
		"SELECT -id, id::int8 + 1::int8 FROM ONLY app.items WHERE name IS NOT NULL",
		"SELECT 'synthetic-secret'::text AS name;",
	}
	for _, sql := range positive {
		q, err := compileFixture(sql)
		if err != nil {
			t.Fatalf("positive %s: %v", sql, err)
		}
		if strings.Contains(q.SQL, "synthetic-secret") {
			t.Fatal("literal leaked into emitted SQL")
		}
	}
	negative := []string{
		"SELECT count(*) FROM app.items GROUP BY 1", "SELECT id,-id AS id FROM app.items ORDER BY id", "SELECT id FROM app.items ORDER BY 1.0", "SELECT pg_sleep(1)", "SELECT 1; SELECT 2", "DELETE FROM app.items", "WITH x AS (DELETE FROM app.items RETURNING *) SELECT * FROM x",
		"SELECT * INTO x FROM app.items", "SELECT * FROM app.items FOR UPDATE", "SELECT * FROM pg_catalog.pg_class", "SELECT * FROM items",
		"SELECT * FROM app.items a JOIN app.items b USING(id)", "SELECT * FROM app.items NATURAL JOIN app.items",
		"SELECT * FROM LATERAL (SELECT 1) a", "SELECT count(*) OVER() FROM app.items", "SELECT id COLLATE \"C\" FROM app.items",
		"SELECT id::regclass FROM app.items", "SELECT ARRAY[1]", "SELECT 1 UNION SELECT 2", "SELECT DISTINCT ON(id) id FROM app.items",
		"SELECT * FROM app.items WHERE EXISTS(SELECT id FROM hidden.items)", "SELECT * FROM app.items a WHERE EXISTS(SELECT id FROM app.items b WHERE b.id=a.id)",
		"SELECT sum(count(*)) FROM app.items", "SELECT id,count(*) FROM app.items", "SELECT count(*) FROM app.items WHERE sum(id)>0", "SELECT count(*) FROM app.items GROUP BY sum(id)",
		"SELECT name OPERATOR(app.=) name FROM app.items", "SELECT app.count(*) FROM app.items", "SELECT 'ambiguous'", "SELECT * FROM app.items ORDER BY id USING >",
		"SELECT * FROM app.items FETCH FIRST 1 ROW WITH TIES", "WITH RECURSIVE x AS (SELECT 1) SELECT * FROM x", "SELECT id FROM app.items GROUP BY ROLLUP(id)",
		"SELECT count(id ORDER BY id) FROM app.items", "SELECT count(*) FILTER(WHERE id>0) FROM app.items", "SELECT id FROM app.items TABLESAMPLE SYSTEM(10)",
	}
	for _, sql := range negative {
		if _, err := compileFixture(sql); err == nil {
			t.Fatalf("negative accepted: %s", sql)
		}
	}
	for _, sql := range []string{strings.Repeat("(", 65) + "SELECT 1" + strings.Repeat(")", 65), "SELECT " + strings.Repeat("1,", 8192) + "1", strings.Repeat(" ", 65537)} {
		if _, err := Parse(sql); err == nil {
			t.Fatal("budget accepted")
		}
	}
	catalog := &fixtureCatalog{}
	unresolved, err := Parse("WITH x AS (SELECT id FROM app.items) SELECT id FROM x WHERE EXISTS(SELECT id FROM unresolved)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = unresolved.Compile(t.Context(), 16, catalog, nil); err == nil || catalog.calls != 0 {
		t.Fatal("lexical references must resolve before catalog types")
	}
	repeated, err := Parse("SELECT a.id,b.id FROM app.items a JOIN app.items b ON a.id=b.id")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repeated.Compile(t.Context(), 16, catalog, nil); err != nil || catalog.calls != 1 {
		t.Fatal("physical references were not deduplicated", err)
	}
	p, err := Parse("SELECT $1::int4 + id FROM app.items")
	if err != nil {
		t.Fatal(err)
	}
	v := "3"
	if _, err = p.Compile(t.Context(), 18, &fixtureCatalog{}, []Parameter{{23, &v}}); err != nil {
		t.Fatal(err)
	}
	if _, err = p.Compile(t.Context(), 17, &fixtureCatalog{}, []Parameter{{23, &v}}); err == nil {
		t.Fatal("unaudited major accepted")
	}
	var altered map[string]any
	if err = json.Unmarshal(catalog16, &altered); err != nil {
		t.Fatal(err)
	}
	altered["functions"].([]any)[0].(map[string]any)["prosrc"] = "unapproved_implementation"
	tampered, _ := json.Marshal(altered)
	if VerifyCatalog(16, tampered) == nil {
		t.Fatal("changed implementation accepted")
	}
	if VerifyCatalog(16, catalog16) != nil || VerifyCatalog(18, catalog18) != nil || VerifyCatalog(16, []byte(`{}`)) == nil {
		t.Fatal("signature verification")
	}
}

// The native C parser cannot be interrupted by a Go context. Run worst shapes
// in a killable child and bound the entire corpus, including quotes/comments.
func TestNativeParserBudget(t *testing.T) {
	if os.Getenv("DM_PARSER_CHILD") == "1" {
		corpus := []string{"SELECT " + strings.Repeat("- ", 8000) + "1", "SELECT " + strings.Repeat("NOT ", 8000) + "true", "SELECT " + strings.Repeat("1 + ", 2000) + "1", "SELECT " + strings.Repeat("(", 64) + "1" + strings.Repeat(")", 64), "SELECT $$" + strings.Repeat("(", 60000) + "$$::text", "/*" + strings.Repeat("/*", 64) + strings.Repeat("*/", 65) + "SELECT 1"}
		for _, s := range corpus {
			_, _ = Parse(s)
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

// No existing fuzz target exercises the native grammar or compiler traversal.
func FuzzCompiler(f *testing.F) {
	for _, s := range []string{"SELECT id FROM app.items", "SELECT 'x'::text", "WITH x AS (SELECT id FROM app.items) SELECT * FROM x", "SELECT 1 /* nested /* */ */"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if len(s) > 65536 {
			return
		}
		_, _ = compileFixture(s)
	})
}
