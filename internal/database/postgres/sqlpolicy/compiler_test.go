package sqlpolicy

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"sync"
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
	verifyConcurrentCatalogs(t)
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
	verifyCatalogMutations(t)
}

// Regression ladder step 2: extend the existing compiler boundary. These cases
// distinguish semantic corruption from ordering and malformed transport data.
func verifyConcurrentCatalogs(t *testing.T) {
	t.Helper()
	var workers sync.WaitGroup
	for range 4 {
		workers.Go(func() {
			for _, c := range []struct {
				major int
				data  []byte
			}{{16, catalog16}, {18, catalog18}} {
				if err := VerifyCatalog(c.major, c.data); err != nil {
					t.Errorf("concurrent major %d: %v", c.major, err)
				}
				digest, err := CatalogFingerprint(c.major)
				if err != nil || VerifyCatalogFingerprint(c.major, int64(len(c.data)), digest[:]) != nil {
					t.Errorf("concurrent fingerprint major %d: %v", c.major, err)
				}
				if _, err := loadSignatures(c.major); err != nil {
					t.Errorf("signature initialization: %v", err)
				}
			}
		})
	}
	workers.Wait()
}

func verifyCatalogMutations(t *testing.T) {
	t.Helper()
	// Extend the same corpus through both the previous full-document verifier and
	// the compact verifier; every accepted/rejected semantic outcome must agree.
	verify := func(major int, b []byte) error {
		reference := VerifyCatalog(major, b)
		_, err := decodeCatalog(b)
		if err == nil {
			var digest [32]byte
			digest, err = fingerprintCatalog(major, b)
			if err == nil {
				err = VerifyCatalogFingerprint(major, int64(len(b)), digest[:])
			}
		}
		if (reference == nil) != (err == nil) {
			t.Fatal("fingerprint changed semantic acceptance", reference, err)
		}
		return err
	}
	decode := func() map[string]any {
		t.Helper()
		var doc map[string]any
		if err := json.Unmarshal(catalog16, &doc); err != nil {
			t.Fatal(err)
		}
		return doc
	}
	row := func(doc map[string]any, section string) map[string]any {
		return doc[section].([]any)[0].(map[string]any)
	}
	mutations := []struct {
		name   string
		change func(map[string]any)
	}{
		{"null versus empty array", func(d map[string]any) { row(d, "functions")["proconfig"] = []any{} }},
		{"function argument order", func(d map[string]any) {
			for _, v := range d["functions"].([]any) {
				args := v.(map[string]any)["proargtypes"].([]any)
				if len(args) > 1 && args[0] != args[1] {
					args[0], args[1] = args[1], args[0]
					return
				}
			}
			t.Fatal("no heterogeneous signature in fixture")
		}},
		{"function implementation", func(d map[string]any) { row(d, "functions")["prosrc"] = "unapproved_implementation" }},
		{"function security", func(d map[string]any) { row(d, "functions")["prosecdef"] = true }},
		{"operator implementation", func(d map[string]any) { row(d, "operators")["oprcode"] = 31 }},
		{"aggregate helper", func(d map[string]any) { row(d, "aggregates")["aggtransfn"] = 31 }},
		{"cast implementation", func(d map[string]any) { row(d, "casts")["castfunc"] = 31 }},
		{"btree implementation", func(d map[string]any) { row(d, "supports")["amproc"] = 31 }},
		{"operator family member", func(d map[string]any) { row(d, "members")["amopopr"] = 4294967295 }},
		{"operator class family", func(d map[string]any) { row(d, "classes")["opcfamily"] = 0 }},
		{"type implementation", func(d map[string]any) { row(d, "types")["typinput"] = 31 }},
		{"planner callback", func(d map[string]any) { row(d, "functions")["prosupport"] = 31 }},
		{"dangling callback", func(d map[string]any) { row(d, "functions")["prosupport"] = 4294967295 }},
		{"missing false", func(d map[string]any) { delete(row(d, "functions"), "prosecdef") }},
		{"missing zero", func(d map[string]any) { delete(row(d, "functions"), "pronargdefaults") }},
		{"missing null", func(d map[string]any) { delete(row(d, "functions"), "proconfig") }},
		{"null scalar", func(d map[string]any) { row(d, "functions")["prosecdef"] = nil }},
		{"unknown property", func(d map[string]any) { row(d, "functions")["unexpected"] = true }},
		{"case variant field", func(d map[string]any) { r := row(d, "functions"); r["Prosrc"] = r["prosrc"]; delete(r, "prosrc") }},
		{"case variant section", func(d map[string]any) { d["Functions"] = d["functions"]; delete(d, "functions") }},
		{"case alias duplicate", func(d map[string]any) { row(d, "functions")["Prosrc"] = row(d, "functions")["prosrc"] }},
		{"string OID", func(d map[string]any) { row(d, "functions")["oid"] = "31" }},
		{"negative OID", func(d map[string]any) { row(d, "functions")["oid"] = -1 }},
		{"fractional OID", func(d map[string]any) { row(d, "functions")["oid"] = 31.5 }},
		{"overflow OID", func(d map[string]any) { row(d, "functions")["oid"] = 4294967296 }},
		{"extra definition", func(d map[string]any) {
			r := map[string]any{}
			for k, v := range row(d, "functions") {
				r[k] = v
			}
			r["oid"] = 999999
			d["functions"] = append(d["functions"].([]any), r)
		}},
	}
	for _, mutation := range mutations {
		doc := decode()
		mutation.change(doc)
		b, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		if verify(16, b) == nil {
			t.Fatalf("catalog accepted %s", mutation.name)
		}
	}
	for _, section := range []string{"types", "operators", "aggregates", "casts", "classes", "members", "supports", "functions"} {
		for _, action := range []string{"missing section", "missing record", "duplicate identity"} {
			doc := decode()
			rows := doc[section].([]any)
			switch action {
			case "missing section":
				delete(doc, section)
			case "missing record":
				doc[section] = rows[1:]
			case "duplicate identity":
				doc[section] = append(rows, rows[0])
			}
			b, _ := json.Marshal(doc)
			if verify(16, b) == nil {
				t.Fatalf("catalog accepted %s: %s", section, action)
			}
		}
	}
	for _, b := range [][]byte{[]byte(`{}`), append(append([]byte{}, catalog16...), []byte(` {}`)...), []byte(`{"types":[],"types":[]}`), []byte(`{"types":[{"oid":16,"oid":17}]}`), []byte(strings.Repeat(" ", (2<<20)+1))} {
		if verify(16, b) == nil {
			t.Fatal("malformed catalog accepted")
		}
	}
	if verify(17, catalog16) == nil || verify(18, catalog16) == nil {
		t.Fatal("wrong major accepted")
	}
	doc := decode()
	for _, value := range doc {
		rows := value.([]any)
		for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
			rows[i], rows[j] = rows[j], rows[i]
		}
	}
	b, _ := json.Marshal(doc)
	if err := verify(16, b); err != nil {
		t.Fatal("row order affected semantic verification", err)
	}
	verifyCatalogEncoding(t)
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

// Ladder 2: canonical encoding extends the compiler/catalog boundary. The same
// vectors are executed by PostgreSQL in the existing owned integration scenario.
func verifyCatalogEncoding(t *testing.T) {
	t.Helper()
	b, err := os.ReadFile("testdata/catalog-encoding.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name      string
		Input     json.RawMessage
		Canonical string
	}
	if err = json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		d := json.NewDecoder(bytes.NewReader(c.Input))
		d.UseNumber()
		var value any
		if err = d.Decode(&value); err != nil {
			t.Fatal(err)
		}
		actual, e := appendCatalogJSON(nil, value)
		if e != nil || string(actual) != c.Canonical {
			t.Fatalf("canonical encoding %s: %q %v", c.Name, actual, e)
		}
	}
}
