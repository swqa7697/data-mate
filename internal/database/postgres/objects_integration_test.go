package postgres

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
)

// Regression ladder 2: extend the existing owned PostgreSQL acceptance fixture.
// Catalog inspection must not run stored expressions, and explicit calls must
// fail before even an immutable routine can be evaluated during planning.
func objectAcceptance(t *testing.T, d *Driver, access database.Access, admin *pgx.Conn, sql func(string, ...any)) {
	t.Helper()
	started := time.Now()
	sql(`CREATE SCHEMA catalog; GRANT USAGE ON SCHEMA catalog TO reader;
 CREATE FUNCTION catalog.a_probe() RETURNS int LANGUAGE plpgsql IMMUTABLE AS $$BEGIN RAISE EXCEPTION 'must not execute'; END$$;
 CREATE FUNCTION catalog.a_probe(v integer) RETURNS int LANGUAGE sql AS 'SELECT v';
 CREATE FUNCTION catalog.a_probe(v text) RETURNS text LANGUAGE sql AS 'SELECT v';
 REVOKE EXECUTE ON FUNCTION catalog.a_probe() FROM PUBLIC;
 CREATE PROCEDURE catalog.procedure_probe() LANGUAGE plpgsql AS $$BEGIN RAISE EXCEPTION 'must not execute'; END$$;
 CREATE AGGREGATE catalog.total(integer) (SFUNC=pg_catalog.int4pl,STYPE=integer,INITCOND='0');
 CREATE FUNCTION catalog.trigger_probe() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RAISE EXCEPTION 'must not execute'; END$$;
 CREATE TYPE catalog.mood AS ENUM ('z','a');
 CREATE DOMAIN catalog.label AS text DEFAULT 'ok' NOT NULL CHECK(length(VALUE)>0);
 CREATE TYPE catalog.pair AS (value int,label text);
 CREATE TYPE catalog.int_range AS RANGE (subtype=int4);
 CREATE SEQUENCE catalog.counter AS bigint START 9007199254740993;
 CREATE TABLE catalog.items(id bigint DEFAULT nextval('catalog.counter'), label catalog.label,
 generated text GENERATED ALWAYS AS (label::text || '!') STORED, CHECK(id>0));
 ALTER SEQUENCE catalog.counter OWNED BY catalog.items.id;
 CREATE INDEX label_index ON catalog.items ((lower(label))) WHERE id>1;
 CREATE TRIGGER inspect_trigger BEFORE INSERT ON catalog.items FOR EACH ROW EXECUTE FUNCTION catalog.trigger_probe();
 ALTER TABLE catalog.items ENABLE ROW LEVEL SECURITY;
 CREATE POLICY inspect_policy ON catalog.items FOR SELECT TO reader USING (id>0);
 CREATE VIEW catalog.indirect AS SELECT * FROM hidden.target;
 CREATE VIEW catalog.no_execute AS SELECT catalog.a_probe() AS value;
 CREATE EXTENSION pg_trgm WITH SCHEMA catalog;
 CREATE EXTENSION pg_stat_statements WITH SCHEMA catalog;
 CREATE FUNCTION pg_catalog.fixture_call() RETURNS int LANGUAGE plpgsql IMMUTABLE AS $$BEGIN RAISE EXCEPTION 'must not execute'; END$$;
 CREATE FUNCTION pg_catalog.abs(text) RETURNS int LANGUAGE plpgsql IMMUTABLE AS $$BEGIN RAISE EXCEPTION 'must not execute'; END$$;
 GRANT SELECT ON ALL TABLES IN SCHEMA catalog TO reader;
 GRANT USAGE ON ALL SEQUENCES IN SCHEMA catalog TO reader;`)
	defer sql(`DROP FUNCTION pg_catalog.fixture_call(); DROP FUNCTION pg_catalog.abs(text)`)
	p := access.Profile
	p.Scope = config.Scope{Mode: "whitelist", Schemas: []string{"app", "catalog"}}
	access.Profile = p
	describe := func(kind, name string, signature *string) database.ObjectDescription {
		t.Helper()
		out, err := d.DescribeObject(t.Context(), access, database.ObjectRequest{Kind: kind, Schema: "catalog", Name: name, IdentityArguments: signature})
		if err != nil {
			t.Fatalf("describe %s %s: %v", kind, name, err)
		}
		raw, _ := json.Marshal(out)
		if contracts.Validate("describe_object.output", raw) != nil {
			t.Fatalf("object output contract: %s", raw)
		}
		return out
	}
	empty := ""
	routine := describe("routine", "a_probe", &empty)
	if routine.Routine.Definition == nil || !strings.Contains(*routine.Routine.Definition, "must not execute") || routine.Routine.Volatility != "immutable" {
		t.Fatal("routine source missing")
	}
	if describe("routine", "procedure_probe", &empty).Routine.Kind != "procedure" {
		t.Fatal("procedure metadata")
	}
	sig := "integer"
	if describe("routine", "total", &sig).Routine.Aggregate == nil {
		t.Fatal("aggregate metadata")
	}
	mood := describe("type", "mood", nil).Type
	if strings.Join(mood.EnumLabels, ",") != "z,a" {
		t.Fatal("enum declaration order", mood.EnumLabels)
	}
	domain := describe("type", "label", nil).Type
	hasCheck := false
	for _, constraint := range domain.Constraints {
		if constraint.Kind == "check" {
			hasCheck = true
		}
	}
	if domain.BaseType == nil || domain.Default == nil || !domain.NotNull || !hasCheck {
		t.Fatal("domain metadata", domain)
	}
	if len(describe("type", "pair", nil).Type.Attributes) != 2 {
		t.Fatal("composite metadata")
	}
	if describe("type", "int_range", nil).Type.Range.Subtype != "integer" || describe("type", "int_multirange", nil).Type.Kind != "multirange" {
		t.Fatal("range metadata")
	}
	sequence := describe("sequence", "counter", nil).Sequence
	if sequence.Start != "9007199254740993" || sequence.OwnerTable == nil || sequence.OwnerTable.Name != "items" || sequence.OwnerColumn == nil || *sequence.OwnerColumn != "id" {
		t.Fatal("sequence configuration", sequence)
	}
	// One-entry pages must distinguish overloads and carry their exact signatures.
	req := database.ObjectPageRequest{PageRequest: database.PageRequest{Schema: "catalog", PageSize: 1}, Kind: "routine"}
	seen := map[string]bool{}
	var token string
	for i := 0; i < 3; i++ {
		page, err := d.ListObjects(t.Context(), access, req)
		if err != nil || len(page.Objects) != 1 || page.NextCursor == nil {
			t.Fatalf("routine page: %+v %v", page, err)
		}
		raw, _ := json.Marshal(page)
		if contracts.Validate("list_objects.output", raw) != nil {
			t.Fatalf("object page contract: %s", raw)
		}
		item := page.Objects[0]
		if item.Name != "a_probe" || item.IdentityArguments == nil || seen[*item.IdentityArguments] {
			t.Fatal("overload page identity", item)
		}
		seen[*item.IdentityArguments] = true
		describe(item.Kind, item.Name, item.IdentityArguments)
		token = *page.NextCursor
		req.Cursor = token
	}
	req.Kind = "type"
	_, err := d.ListObjects(t.Context(), access, req)
	requireCode(t, err, contracts.StaleCursor)
	_, err = d.ListTables(t.Context(), access, database.PageRequest{Cursor: token})
	requireCode(t, err, contracts.StaleCursor)
	tables, err := d.ListTables(t.Context(), access, database.PageRequest{PageSize: 1})
	if err != nil || tables.NextCursor == nil {
		t.Fatal("table cursor", err)
	}
	_, err = d.ListObjects(t.Context(), access, database.ObjectPageRequest{PageRequest: database.PageRequest{Cursor: *tables.NextCursor}})
	requireCode(t, err, contracts.StaleCursor)
	d.Invalidate(p.ID)
	req.Kind = "routine"
	_, err = d.ListObjects(t.Context(), access, req)
	requireCode(t, err, contracts.StaleCursor)
	page, err := d.ListObjects(t.Context(), access, database.ObjectPageRequest{PageRequest: database.PageRequest{Schema: "catalog"}, Kind: "type"})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range page.Objects {
		if item.Name == "items" || strings.HasPrefix(item.Name, "_") {
			t.Fatal("automatic type exposed", item)
		}
	}
	// Extension membership is catalog metadata, never permission to execute.
	args := "text, text"
	extension := describe("routine", "similarity", &args)
	if extension.Extension == nil || *extension.Extension != "pg_trgm" {
		t.Fatal("extension membership", extension)
	}
	desc, err := d.DescribeTable(t.Context(), access, config.Table{Schema: "catalog", Name: "items"})
	if err != nil {
		t.Fatal(err)
	}
	if desc.Columns[0].Default == nil || desc.Columns[2].Generated == nil || len(desc.Indexes) != 1 || len(desc.Constraints) == 0 || len(desc.Triggers) != 1 || len(desc.Policies) != 1 || !desc.RowSecurity {
		t.Fatal("table definition metadata", desc)
	}
	raw, _ := json.Marshal(desc)
	if contracts.Validate("describe_table.output", raw) != nil {
		t.Fatalf("table output contract: %s", raw)
	}
	view, err := d.DescribeTable(t.Context(), access, config.Table{Schema: "catalog", Name: "indirect"})
	if err != nil || view.ViewDefinition == nil || !strings.Contains(*view.ViewDefinition, "hidden.target") {
		t.Fatal("complete scoped source", view, err)
	}
	if _, err = d.DescribeTable(t.Context(), access, config.Table{Schema: "catalog", Name: "no_execute"}); err != nil {
		t.Fatal("metadata evaluated immutable expression", err)
	}
	// Explicit calls are rejected without an upstream SQLSTATE, even in dead branches.
	for _, q := range []string{
		"SELECT catalog.a_probe()", "SELECT catalog.similarity('a','b')", "SELECT * FROM catalog.pg_stat_statements(true)",
		"SELECT catalog.pg_stat_statements_reset()", "SELECT pg_catalog.fixture_call()", "SELECT fixture_call()",
		"SELECT abs(1)", "SELECT pg_catalog.abs(1)", "SELECT CASE WHEN false THEN catalog.a_probe() ELSE 1 END",
		"SELECT * FROM (SELECT catalog.a_probe()) x", "CALL catalog.procedure_probe()",
	} {
		_, err = d.Query(t.Context(), access, database.QueryRequest{SQL: q})
		requireCode(t, err, contracts.ReadOnlyViolation)
		if err.(*database.Error).SQLState != "" {
			t.Fatalf("call reached PostgreSQL: %s", q)
		}
	}
	for _, q := range []string{
		"SELECT enum_range(NULL::catalog.mood)", "SELECT pg_catalog.enum_range(NULL::catalog.mood)",
		"SELECT lower('A'),count(*) OVER(), int4('1') FROM catalog.items TABLESAMPLE SYSTEM(10)",
		"SELECT * FROM catalog.pg_stat_statements LIMIT 1", "SELECT * FROM catalog.indirect",
	} {
		if _, err = d.Query(t.Context(), access, database.QueryRequest{SQL: q}); err != nil {
			t.Fatalf("core or indirect query %s: %v", q, err)
		}
	}
	// Scope and current grants apply on every lookup, without caching visibility.
	denied := access
	denied.Profile.Scope = config.Scope{Mode: "whitelist"}
	hidden, err := d.ListObjects(t.Context(), denied, database.ObjectPageRequest{})
	if err != nil || len(hidden.Objects) != 0 {
		t.Fatal("empty scope objects", err)
	}
	_, err = d.DescribeObject(t.Context(), denied, database.ObjectRequest{Kind: "type", Schema: "catalog", Name: "mood"})
	requireCode(t, err, contracts.ScopeDenied)
	sql("REVOKE USAGE ON TYPE catalog.mood FROM PUBLIC,reader")
	_, err = d.DescribeObject(t.Context(), access, database.ObjectRequest{Kind: "type", Schema: "catalog", Name: "mood"})
	requireCode(t, err, contracts.ScopeDenied)
	hiddenTypes, listErr := d.ListObjects(t.Context(), access, database.ObjectPageRequest{PageRequest: database.PageRequest{Schema: "catalog"}, Kind: "type"})
	if listErr != nil {
		t.Fatal(listErr)
	}
	for _, item := range hiddenTypes.Objects {
		if item.Name == "mood" {
			t.Fatal("revoked type remains listed")
		}
	}
	sql("GRANT USAGE ON TYPE catalog.mood TO reader")
	sql("REVOKE SELECT,USAGE ON SEQUENCE catalog.counter FROM reader")
	_, err = d.DescribeObject(t.Context(), access, database.ObjectRequest{Kind: "sequence", Schema: "catalog", Name: "counter"})
	requireCode(t, err, contracts.ScopeDenied)
	sql("GRANT USAGE ON SEQUENCE catalog.counter TO reader")
	sql("REVOKE USAGE ON SCHEMA catalog FROM reader")
	_, err = d.DescribeObject(t.Context(), access, database.ObjectRequest{Kind: "routine", Schema: "catalog", Name: "a_probe", IdentityArguments: &empty})
	requireCode(t, err, contracts.ScopeDenied)
	sql("GRANT USAGE ON SCHEMA catalog TO reader")
	// Blacklist and schema filters must be applied before a one-entry page limit.
	excluded := access
	excluded.Profile.Scope = config.Scope{Mode: "blacklist", Schemas: []string{"catalog"}}
	excludedPage, e := d.ListObjects(t.Context(), excluded, database.ObjectPageRequest{PageRequest: database.PageRequest{Schema: "catalog", PageSize: 1}})
	if e != nil || len(excludedPage.Objects) != 0 || excludedPage.NextCursor != nil {
		t.Fatal("excluded object page", excludedPage, e)
	}
	// Enum collections have their own count bound independent of encoded size.
	labels := make([]string, 4097)
	for i := range labels {
		labels[i] = fmt.Sprintf("'v%d'", i)
	}
	sql("CREATE TYPE catalog.many_labels AS ENUM (" + strings.Join(labels, ",") + ")")
	_, err = d.DescribeObject(t.Context(), access, database.ObjectRequest{Kind: "type", Schema: "catalog", Name: "many_labels"})
	requireCode(t, err, contracts.ResourceLimit)
	// A large stored body must fail as a whole, without a partial definition.
	sql("CREATE FUNCTION catalog.large_body() RETURNS text LANGUAGE sql AS 'SELECT ''" + strings.Repeat("x", 20000) + "''::text'")
	bounded := normalizedFixture(t, access)
	limits := *bounded.Profile.Limits
	limits.MaxResultBytes = 4096
	bounded.Profile.Limits = &limits
	_, err = d.DescribeObject(t.Context(), bounded, database.ObjectRequest{Kind: "routine", Schema: "catalog", Name: "large_body", IdentityArguments: &empty})
	requireCode(t, err, contracts.ResourceLimit)
	var called bool
	if err = admin.QueryRow(t.Context(), "SELECT is_called FROM catalog.counter").Scan(&called); err != nil || called {
		t.Fatal("metadata advanced sequence", err)
	}
	t.Logf("expanded catalog and explicit-call acceptance: %s", time.Since(started))
}
