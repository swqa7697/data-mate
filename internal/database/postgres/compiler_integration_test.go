package postgres

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/database"
	"github.com/swqa7697/data-mate/internal/database/postgres/sqlpolicy"
)

// A compile-only transaction cannot execute or prepare agent statements. Query
// methods record exactly what crosses the catalog seam, including rejected SQL.
type compileSpy struct {
	pgx.Tx
	statements []string
	executions int
}

func (s *compileSpy) Exec(ctx context.Context, q string, args ...any) (pgconn.CommandTag, error) {
	s.executions++
	return s.Tx.Exec(ctx, q, args...)
}
func (s *compileSpy) Prepare(ctx context.Context, n, q string) (*pgconn.StatementDescription, error) {
	s.executions++
	return s.Tx.Prepare(ctx, n, q)
}
func (s *compileSpy) Query(ctx context.Context, q string, args ...any) (pgx.Rows, error) {
	s.statements = append(s.statements, q)
	return s.Tx.Query(ctx, q, args...)
}
func (s *compileSpy) QueryRow(ctx context.Context, q string, args ...any) pgx.Row {
	s.statements = append(s.statements, q)
	return s.Tx.QueryRow(ctx, q, args...)
}

// Extends the P3 owned fixture rather than adding a second Docker lifecycle.
func compilerAcceptance(t *testing.T, d *Driver, access database.Access, sql func(string, ...any)) {
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
		params    []sqlpolicy.Parameter
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
	v := "2"
	cases = append(cases, struct {
		name, sql string
		params    []sqlpolicy.Parameter
	}{"parameter", "SELECT id+$1::int4 AS adjusted FROM app.items WHERE id>=$1 ORDER BY id", []sqlpolicy.Parameter{{Type: 23, Value: &v}}})
	for _, test := range cases {
		err := d.run(t.Context(), access, func(ctx context.Context, tx pgx.Tx, version int) (ret error) {
			defer func() {
				if ret != nil {
					t.Logf("fixture compiler detail: %v", ret)
				}
			}()
			p, err := sqlpolicy.Parse(test.sql)
			if err != nil {
				return err
			}
			spy := &compileSpy{Tx: tx}
			compiled, err := compileSQL(ctx, spy, access.Profile.Scope, version, p, test.params)
			if err != nil {
				return err
			}
			if spy.executions != 0 {
				t.Fatal("compiler executed/prepared a statement")
			}
			for _, q := range spy.statements {
				if q == test.sql {
					t.Fatal("original SQL sent during compilation")
				}
			}
			original, err := fixtureQuery(ctx, tx, test.sql, test.params)
			if err != nil {
				return fmt.Errorf("original: %w", err)
			}
			emitted, err := fixtureQuery(ctx, tx, compiled.SQL, compiled.Parameters)
			if err != nil {
				return fmt.Errorf("emitted: %w; SQL=%s", err, compiled.SQL)
			}
			if !reflect.DeepEqual(original, emitted) {
				return fmt.Errorf("semantic mismatch: original=%#v emitted=%#v SQL=%s", original, emitted, compiled.SQL)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("compiler case %s: %v", test.name, err)
		}
	}
	negative := []string{
		"SELECT * FROM hidden.target", "SELECT * FROM app.parent", "SELECT * FROM app.expr_index", "SELECT * FROM app.partial_index", "SELECT * FROM app.generated", "SELECT * FROM app.custom_index", "SELECT * FROM app.custom_collation_table", "SELECT * FROM app.a_view", "SELECT * FROM app.custom_type", "SELECT * FROM app.rls_parts",
		"WITH x AS (SELECT * FROM hidden.target) SELECT * FROM x", "SELECT id FROM app.items WHERE id IN(SELECT id FROM hidden.target)",
		"SELECT a.id FROM app.items a WHERE EXISTS(SELECT id FROM app.items b WHERE b.id=a.id)",
		"SELECT id OPERATOR(app.=) id FROM app.items", "SELECT app.sum(id) FROM app.items", "SELECT id::app.custom FROM app.items",
		"SELECT pg_catalog.pg_read_file('synthetic-secret')", "SELECT * FROM app.items FOR SHARE", "WITH x AS (DELETE FROM app.items RETURNING *) SELECT * FROM x",
	}
	for _, q := range negative {
		err := d.run(t.Context(), access, func(ctx context.Context, tx pgx.Tx, version int) error {
			spy := &compileSpy{Tx: tx}
			p, err := sqlpolicy.Parse(q)
			if err == nil {
				_, err = compileSQL(ctx, spy, access.Profile.Scope, version, p, nil)
			}
			if err == nil {
				t.Fatalf("rejected corpus accepted: %s", q)
			}
			if spy.executions != 0 {
				t.Fatal("rejected SQL executed/prepared")
			}
			for _, sent := range spy.statements {
				if sent == q || strings.Contains(sent, "synthetic-secret") {
					t.Fatal("rejected SQL reached server")
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	all := access.Profile
	all.Scope = config.Scope{Mode: "all"}
	err := d.run(t.Context(), database.NewAccess(all, access.Password()), func(ctx context.Context, tx pgx.Tx, version int) error {
		q := "SELECT id FROM app.parent ORDER BY id"
		p, _ := sqlpolicy.Parse(q)
		out, err := compileSQL(ctx, tx, all.Scope, version, p, nil)
		if err != nil {
			return err
		}
		a, err := fixtureQuery(ctx, tx, q, nil)
		if err != nil {
			return err
		}
		b, err := fixtureQuery(ctx, tx, out.SQL, out.Parameters)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(a, b) {
			t.Fatal("inheritance semantics")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("P4 compiler: %d differential and %d rejected cases; inheritance; compile-only spy", len(cases), len(negative))
}

type fixtureResult struct {
	Names []string
	Types []uint32
	Rows  [][][]byte
}

func fixtureQuery(ctx context.Context, tx pgx.Tx, sql string, params []sqlpolicy.Parameter) (fixtureResult, error) {
	values := [][]byte{}
	oids := []uint32{}
	for _, p := range params {
		var v []byte
		if p.Value != nil {
			v = []byte(*p.Value)
		}
		values = append(values, v)
		oids = append(oids, uint32(p.Type))
	}
	// Only this owned-fixture oracle executes SQL in P4. Production Query remains
	// unavailable; no result-codec or concurrency-safety claim is made here.
	result := tx.Conn().PgConn().ExecParams(ctx, sql, values, oids, nil, []int16{0}).Read()
	if result.Err != nil {
		return fixtureResult{}, result.Err
	}
	out := fixtureResult{Rows: result.Rows}
	for _, f := range result.FieldDescriptions {
		out.Names = append(out.Names, f.Name)
		out.Types = append(out.Types, f.DataTypeOID)
	}
	return out, nil
}
