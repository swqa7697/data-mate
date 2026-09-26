package postgres

import (
	"encoding/json"
	"testing"

	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
)

// Extends the existing owned scenario. Former compiler restrictions become
// successful reads; direct scope and PostgreSQL write rejection remain tested.
func queryAcceptance(t *testing.T, d *Driver, access database.Access, sql func(string, ...any)) {
	t.Helper()
	sql(`INSERT INTO app.items(id,name,amount) VALUES(1,'alpha',1.25),(2,'beta',2.50),(3,'alpha',NULL),(4,NULL,-4.75);
 INSERT INTO app.parts VALUES(1),(2); INSERT INTO app.parent VALUES(3); INSERT INTO hidden.child VALUES(4);
 CREATE TABLE app.empty_parts(id int) PARTITION BY RANGE(id);
 CREATE TABLE app.expr_index(id int); CREATE INDEX ON app.expr_index((id+1));
 CREATE TABLE app.partial_index(id int); CREATE INDEX ON app.partial_index(id) WHERE id>0;
 CREATE COLLATION app.custom_collation (provider=libc,locale='C');
 CREATE TABLE app.custom_collation_table(value text COLLATE app.custom_collation);
 CREATE OPERATOR CLASS app.custom_int_ops FOR TYPE int4 USING btree AS OPERATOR 1 <,OPERATOR 2 <=,OPERATOR 3 =,OPERATOR 4 >=,OPERATOR 5 >,FUNCTION 1 pg_catalog.btint4cmp(int4,int4);
 CREATE TABLE app.custom_index(id int);CREATE INDEX ON app.custom_index(id app.custom_int_ops);
 CREATE TABLE app.generated(id int, computed int GENERATED ALWAYS AS (id+1) STORED);
 CREATE TABLE app.safe_rls(id int); INSERT INTO app.safe_rls VALUES(1),(2); ALTER TABLE app.safe_rls ENABLE ROW LEVEL SECURITY;
 CREATE POLICY safe_policy ON app.safe_rls USING(id=1);
 INSERT INTO "Dot.Schema"."a.b" VALUES(5);
 CREATE FUNCTION app.evil(int,int) RETURNS boolean LANGUAGE sql AS 'SELECT true';
 CREATE OPERATOR app.= (LEFTARG=int,RIGHTARG=int,FUNCTION=app.evil);
 CREATE FUNCTION app.sum(int) RETURNS bigint LANGUAGE sql AS 'SELECT 777::bigint';
 GRANT SELECT ON ALL TABLES IN SCHEMA app TO reader;`)
	cases := []struct {
		name, sql string
		params    []json.RawMessage
	}{
		{"literal", "SELECT 1 AS one, 'a''b'::text AS quoted, NULL::int4 AS nullable", nil},
		{"typed literals", "SELECT true,false,1.25,2147483648,1::numeric,'x'::text,'x'::varchar,'2026-09-23'::date,'{}'::json,'{}'::jsonb", nil},
		{"duplicate derived labels", "SELECT x.* FROM (SELECT id,id,name FROM app.items ORDER BY id) x ORDER BY 1", nil},
		{"column alias list", "SELECT x.value FROM app.items x(value) ORDER BY x.value", nil},
		{"group expression", "SELECT id+1,count(*) FROM app.items GROUP BY id+1 HAVING id+1>1 ORDER BY id+1", nil},
		{"numeric signatures", "SELECT 1::int2+2::int2, 1::int8*2::int8, 1::float4/2::float4, 1::float8-2::float8, 2::numeric::int4, 3::float8::numeric, 4::numeric::float4, 5::int4::int2", nil},
		{"IN common type", "SELECT '1.1' IN (1,1.1::numeric), 1 IN (NULL,2::int8), (id::int8)::numeric FROM app.items ORDER BY id", nil},
		{"unknown context", "SELECT id FROM app.items WHERE id='2' ORDER BY id", nil},
		{"output alias ordering", "SELECT -id AS id FROM app.items ORDER BY id", nil},
		{"filter", "SELECT id,name,amount FROM app.items WHERE id >= 2 AND (name IS NULL OR name='alpha') ORDER BY id", nil},
		{"arithmetic", "SELECT id+1, -id, amount/2::numeric, id::int8+1::int8 FROM app.items ORDER BY id", nil},
		{"stars and duplicates", "SELECT a.*, a.id AS id FROM app.items a ORDER BY a.id", nil},
		{"left join", "SELECT a.id,b.id FROM app.items a LEFT JOIN app.items b ON a.id=b.id+1 ORDER BY a.id,b.id NULLS FIRST", nil},
		{"right join", "SELECT a.id,b.id FROM app.items a RIGHT JOIN app.items b ON a.id=b.id+1 ORDER BY b.id,a.id NULLS FIRST", nil},
		{"full join", "SELECT a.id,b.id FROM app.items a FULL JOIN app.items b ON a.id=b.id+1 ORDER BY a.id NULLS LAST,b.id NULLS FIRST", nil},
		{"cross join", "SELECT a.id,b.id FROM app.items a CROSS JOIN app.items b WHERE a.id=1 ORDER BY b.id", nil},
		{"aggregate", "SELECT count(*),count(name),sum(id),avg(amount),min(name),max(id),count(DISTINCT name) FROM app.items", nil},
		{"group", "SELECT name,count(*),sum(id) FROM app.items GROUP BY name HAVING count(*)>0 ORDER BY name NULLS LAST", nil},
		{"cte", "WITH x AS (SELECT id,name FROM app.items WHERE id>1), y AS (SELECT * FROM x WHERE id<4) SELECT * FROM y ORDER BY id", nil},
		{"shadow", "WITH x AS (SELECT id FROM app.items WHERE id=1) SELECT * FROM (WITH x AS (SELECT id FROM app.items WHERE id=2) SELECT id FROM x) q", nil},
		{"subqueries", "SELECT id FROM app.items WHERE EXISTS(SELECT id FROM app.items WHERE id=2) AND id IN (SELECT id FROM app.items WHERE id<3) ORDER BY id", nil},
		{"between in", "SELECT id FROM app.items WHERE id BETWEEN 1 AND 4 AND id NOT IN (2,3) ORDER BY id", nil},
		{"null in", "SELECT id, id IN (1,NULL::int4),id NOT IN (1,NULL::int4) FROM app.items ORDER BY id", nil},
		{"pagination", "SELECT name,id FROM app.items ORDER BY 2 DESC LIMIT 2 OFFSET 1", nil},
		{"quoted", "SELECT \"a.b\".id FROM \"Dot.Schema\".\"a.b\" ORDER BY id", nil},
		{"partition", "SELECT id FROM app.parts ORDER BY id", nil},
		{"empty partition", "SELECT * FROM app.empty_parts", nil},
		{"only", "SELECT id FROM ONLY app.parent ORDER BY id", nil},
		{"rls", "SELECT id FROM app.safe_rls ORDER BY id", nil},
	}

	for _, test := range cases {
		result, err := d.Query(t.Context(), access, database.QueryRequest{SQL: test.sql, Parameters: test.params})
		if err != nil {
			t.Fatalf("query corpus %s: %v", test.name, err)
		}
		encoded, _ := json.Marshal(result)
		if err = contracts.Validate("query.output", encoded); err != nil {
			t.Fatalf("query schema %s: %v", test.name, err)
		}
	}
	sql(`CREATE TABLE app.mixed(id int,state app.custom,tags text[],values numeric[][],extra int GENERATED ALWAYS AS(id+1) STORED);
 INSERT INTO app.mixed(id,state,tags,values) VALUES(1,'one',ARRAY['alpha',NULL,'NULL','a,b','quote"'],ARRAY[[9007199254740993::numeric,2],[3,NULL]]);
 CREATE VIEW app.indirect AS SELECT id FROM hidden.target;
 CREATE MATERIALIZED VIEW app.materialized AS SELECT id FROM app.items;
 CREATE DOMAIN app.label AS text CHECK(length(VALUE)>0);
 CREATE TABLE app.column_grants(id int,secret text);INSERT INTO app.column_grants VALUES(1,'private');
 GRANT SELECT(id) ON app.column_grants TO reader;
 GRANT SELECT ON app.mixed,app.indirect,app.materialized TO reader;
 CREATE FUNCTION app.read_hidden() RETURNS SETOF integer LANGUAGE sql SECURITY DEFINER AS 'SELECT id FROM hidden.target';
 INSERT INTO hidden.target VALUES(42);
 CREATE EXTENSION file_fdw;
 CREATE SERVER fixture_file FOREIGN DATA WRAPPER file_fdw;
 CREATE FOREIGN TABLE app.foreign_empty(id int) SERVER fixture_file OPTIONS(filename '/dev/null',format 'text');
 GRANT SELECT ON app.foreign_empty TO reader;`)
	for _, q := range []string{
		"SELECT count(*) FROM app.mixed", "SELECT id FROM app.mixed", "SELECT * FROM app.mixed",
		"SELECT * FROM app.foreign_empty", "SELECT * FROM app.a_view", "SELECT * FROM app.materialized", "SELECT * FROM app.expr_index", "SELECT * FROM app.partial_index", "SELECT * FROM app.generated", "SELECT * FROM app.custom_index", "SELECT * FROM app.custom_collation_table", "SELECT * FROM app.custom_type", "SELECT * FROM app.rls_parts", "SELECT * FROM app.parent",
		"SELECT count(*) OVER(),id FROM app.items", "SELECT DISTINCT name FROM app.items", "SELECT id FROM app.items a WHERE EXISTS(SELECT 1 FROM app.items b WHERE b.id=a.id)",
		"SELECT * FROM app.items a,LATERAL(SELECT a.id) b", "SELECT 1 UNION SELECT 2", "WITH RECURSIVE x(n) AS (VALUES(1) UNION ALL SELECT n+1 FROM x WHERE n<3) SELECT * FROM x",
		"SELECT * FROM app.indirect", "SELECT id OPERATOR(app.=) 1 FROM app.items",
	} {
		if _, err := d.Query(t.Context(), access, database.QueryRequest{SQL: q}); err != nil {
			t.Fatalf("newly readable %s: %v", q, err)
		}
	}
	for _, c := range []struct {
		q    string
		want any
	}{
		{"SELECT count(*) FROM app.mixed", "1"}, {"SELECT id FROM app.mixed", int64(1)},
		{"SELECT * FROM app.indirect", int64(42)},
	} {
		r, e := d.Query(t.Context(), access, database.QueryRequest{SQL: c.q})
		if e != nil || r.RowCount != 1 || r.Rows[0][0] != c.want {
			t.Fatalf("read outcome %s: %v %v", c.q, r.Rows, e)
		}
	}
	result, err := d.Query(t.Context(), access, database.QueryRequest{SQL: "SELECT * FROM app.mixed"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(result.Rows)
	if string(raw) != `[[1,"one",["alpha",null,"NULL","a,b","quote\""],[["9007199254740993","2"],["3",null]],2]]` {
		t.Fatalf("mixed result: %s", raw)
	}
	if result.Columns[1].Type != "app.custom" || result.Columns[1].Encoding != "postgres_text" {
		t.Fatal("enum metadata", result.Columns)
	}
	for _, c := range []struct{ q, want string }{
		{"SELECT ARRAY[]::int[],NULL::text[],ARRAY[NULL,'one']::app.custom[]", `[[],null,[null,"one"]]`},
		{"SELECT '[0:1]={1,2}'::int[],ARRAY[9007199254740993::bigint],ARRAY['\\x01'::bytea]", `[[1,2],["9007199254740993"],["AQ=="]]`},
		{"SELECT '[1,4)'::int4range, '127.0.0.1'::inet, 'ok'::app.label", `["[1,4)","127.0.0.1","ok"]`},
		{"SELECT 1 AS id,2 AS id,3", `[1,2,3]`},
		{"SELECT '1 2'::int2vector,ARRAY[true,false,NULL],ARRAY['ok'::app.label]", `["1 2",[true,false,null],["ok"]]`},
		{"SELECT ARRAY[box(point(0,0),point(1,1)),box(point(2,2),point(3,3))]", `[["(1,1),(0,0)","(3,3),(2,2)"]]`},
	} {
		r, e := d.Query(t.Context(), access, database.QueryRequest{SQL: c.q})
		if e != nil {
			t.Fatalf("codec query %s: %v", c.q, e)
		}
		raw, _ := json.Marshal(r.Rows[0])
		if string(raw) != c.want {
			t.Fatalf("codec query %s: %s != %s", c.q, raw, c.want)
		}
	}
	for _, c := range []struct {
		q    string
		code contracts.Code
	}{
		{"SELECT * FROM hidden.target", contracts.ScopeDenied}, {"SELECT * FROM pg_catalog.pg_class", contracts.ScopeDenied},
		{"SELECT * FROM app.items WHERE EXISTS(SELECT 1 FROM hidden.target)", contracts.ScopeDenied},
		{"SELECT secret FROM app.column_grants", contracts.PermissionDenied},
		{"SELECT app.policy_probe()", contracts.ReadOnlyViolation},
		{"SELECT * FROM app.read_hidden()", contracts.ReadOnlyViolation},
		{"WITH x AS (SELECT app.sum(1)) SELECT * FROM x", contracts.ReadOnlyViolation},
		{"DELETE FROM app.items", contracts.ReadOnlyViolation}, {"WITH x AS (DELETE FROM app.items RETURNING *) SELECT * FROM x", contracts.ReadOnlyViolation},
	} {
		_, e := d.Query(t.Context(), access, database.QueryRequest{SQL: c.q})
		requireCode(t, e, c.code)
		if c.code == contracts.PermissionDenied {
			if safeSQLState(e.(*database.Error).SQLState) == "" {
				t.Fatal("missing SQLSTATE")
			}
		}
	}
	if _, err = d.Query(t.Context(), access, database.QueryRequest{SQL: "SELECT id FROM app.column_grants"}); err != nil {
		t.Fatal(err)
	}
	desc, err := d.DescribeTable(t.Context(), access, config.Table{Schema: "app", Name: "column_grants"})
	if err != nil || len(desc.Columns) != 2 {
		t.Fatal("column grants metadata", err)
	}
	page, err := d.ListTables(t.Context(), access, database.PageRequest{Schema: "app"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, table := range page.Tables {
		if table.Name == "column_grants" {
			found = true
		}
	}
	if !found {
		t.Fatal("column-granted relation missing")
	}
	// Unknown types do not prevent normal parameter inference or explicit casts.
	for _, q := range []database.QueryRequest{
		{SQL: "SELECT $1::app.custom,$2::text[],$3::jsonb", Parameters: []json.RawMessage{json.RawMessage(`"one"`), json.RawMessage(`"{a,b}"`), json.RawMessage(`{"n":9007199254740993}`)}},
		{SQL: "SELECT id FROM app.items WHERE id=$1", Parameters: []json.RawMessage{json.RawMessage(`1`)}},
	} {
		if _, err = d.Query(t.Context(), access, q); err != nil {
			t.Fatal("parameter inference", err)
		}
	}
	// Original SQL must have identical string-literal rules in the guard/server.
	func() {
		sql("ALTER ROLE reader SET standard_conforming_strings=off")
		defer sql("ALTER ROLE reader RESET standard_conforming_strings")
		d.Invalidate(access.Profile.ID)
		r, e := d.Query(t.Context(), access, database.QueryRequest{SQL: `SELECT 'a\b'::text`})
		if e != nil || r.Rows[0][0] != `a\b` {
			t.Fatal("startup literal rules", r.Rows, e)
		}
	}()
	t.Log("query guard, broad SQL, indirect dependencies, mixed types and column grants passed")
}

func normalizedFixture(t *testing.T, a database.Access) database.Access {
	t.Helper()
	out, _, err := normalized(a)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
