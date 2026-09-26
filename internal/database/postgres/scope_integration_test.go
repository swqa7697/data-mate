package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
)

// Extends the existing owned fixture with schema paging and future-schema policy.
func scopeAcceptance(t *testing.T, d *Driver, a database.Access, password string, sql func(string, ...any)) {
	t.Helper()
	sql(`CREATE SCHEMA picker; CREATE SCHEMA picker_empty; GRANT USAGE ON SCHEMA picker,picker_empty TO reader`)
	defer sql("DROP SCHEMA picker_empty")
	defer sql("DROP SCHEMA picker CASCADE")
	empty, err := d.BrowseScope(t.Context(), a, database.ScopeRequest{Search: "picker_empty"})
	if err != nil || len(empty.Schemas) != 1 || empty.Schemas[0] != "picker_empty" {
		t.Fatal("empty accessible schema missing", err)
	}
	// Extend the catalog fixture: descriptions preserve empty schemas, list only
	// readable relations, and annotate rather than hide scope exclusions.
	sql(`CREATE SCHEMA describe_denied; CREATE TABLE describe_denied.items(id int);
 GRANT SELECT ON describe_denied.items TO reader;
 CREATE TABLE picker_empty.unreadable(id int);
 CREATE TABLE picker.unreadable(id int);
 CREATE TABLE picker.column_only(id int, secret text); GRANT SELECT(id) ON picker.column_only TO reader;
 CREATE VIEW picker.a_view AS SELECT 1 AS id;
 CREATE MATERIALIZED VIEW picker.materialized AS SELECT 1 AS id;
 CREATE TABLE picker.partitioned(id int) PARTITION BY RANGE(id);
 CREATE FOREIGN DATA WRAPPER describe_fdw;
 CREATE SERVER describe_server FOREIGN DATA WRAPPER describe_fdw;
 CREATE FOREIGN TABLE picker.foreign_table(id int) SERVER describe_server;
 GRANT SELECT ON picker.a_view,picker.materialized,picker.partitioned,picker.foreign_table TO reader`)
	defer sql("DROP SCHEMA describe_denied CASCADE")
	defer sql("DROP FOREIGN DATA WRAPPER describe_fdw CASCADE")
	for _, scope := range []config.Scope{{Mode: "whitelist", Schemas: []string{"missing"}}, {Mode: "blacklist", Schemas: []string{"picker"}}} {
		p := a.Profile
		p.Scope = scope
		description, err := d.DescribeDatabase(t.Context(), database.NewAccess(p, password))
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(description)
		if contracts.Validate("db-describe.output", raw) != nil || !reflect.DeepEqual(description.Scope, scope) {
			t.Fatal("database description contract", string(raw))
		}
		foundEmpty, foundPicker := false, false
		previous := ""
		for _, s := range description.Schemas {
			if s.Name <= previous || strings.HasPrefix(s.Name, "pg_") || s.Name == "information_schema" || s.Name == "describe_denied" || s.Allowed != scope.ContainsSchema(s.Name) {
				t.Fatalf("schema visibility/order: %+v", s)
			}
			previous = s.Name
			if s.Name == "picker_empty" {
				foundEmpty = len(s.Tables) == 0
			}
			if s.Name == "picker" {
				foundPicker = true
				want := []database.RelationName{{Name: "a_view", Kind: "view"}, {Name: "column_only", Kind: "table"}, {Name: "foreign_table", Kind: "foreign_table"}, {Name: "materialized", Kind: "materialized_view"}, {Name: "partitioned", Kind: "partitioned_table"}}
				if !reflect.DeepEqual(s.Tables, want) {
					t.Fatalf("readable relations: %+v", s.Tables)
				}
			}
		}
		if !foundEmpty || !foundPicker {
			t.Fatal("description omitted excluded or empty schema")
		}
		page, err := d.ListTables(t.Context(), database.NewAccess(p, password), database.PageRequest{Schema: "picker"})
		if err != nil || len(page.Tables) != 0 {
			t.Fatal("user catalog relaxed agent scope", err)
		}
	}
	// A compact database-generated corpus verifies the combined schema/relation
	// boundary in this owned fixture, without thousands of test executions.
	bounded := a.Profile
	limits := config.DefaultLimits()
	bounded.Limits = &limits
	bounded.Limits.MaxResultBytes = 1 << 20
	description, err := d.DescribeDatabase(t.Context(), database.NewAccess(bounded, password))
	if err != nil {
		t.Fatal(err)
	}
	count := len(description.Schemas)
	for _, s := range description.Schemas {
		count += len(s.Tables)
	}
	sql(`DO $$ BEGIN FOR i IN 1..` + fmt.Sprint(maxCatalogObjects-count) + ` LOOP
 EXECUTE format('CREATE SCHEMA describe_bound%s',i);
 EXECUTE format('GRANT USAGE ON SCHEMA describe_bound%s TO reader',i);
 END LOOP; END $$`)
	cleanupBounds := func() {
		sql(`DO $$ DECLARE n text; BEGIN FOR n IN SELECT nspname FROM pg_namespace WHERE nspname LIKE 'describe_bound%' LOOP EXECUTE format('DROP SCHEMA %I CASCADE',n); END LOOP; END $$`)
	}
	defer cleanupBounds()
	if _, err = d.DescribeDatabase(t.Context(), database.NewAccess(bounded, password)); err != nil {
		t.Fatal("exact catalog bound rejected", err)
	}
	sql("CREATE TABLE describe_bound1.extra(id int); GRANT SELECT ON describe_bound1.extra TO reader")
	result, err := d.DescribeDatabase(t.Context(), database.NewAccess(bounded, password))
	requireCode(t, err, contracts.ResourceLimit)
	if result.Schemas != nil {
		t.Fatal("oversized catalog returned partial result")
	}
	sql("DROP TABLE describe_bound1.extra")
	bounded.Limits.MaxResultBytes = 1024
	_, err = d.DescribeDatabase(t.Context(), database.NewAccess(bounded, password))
	requireCode(t, err, contracts.ResourceLimit)
	cleanupBounds()
	sql("DROP TABLE picker.column_only,picker.unreadable,picker.partitioned,picker_empty.unreadable; DROP VIEW picker.a_view; DROP MATERIALIZED VIEW picker.materialized; DROP FOREIGN TABLE picker.foreign_table")
	// Empty schemas exercise both pagination and selection before tables exist.
	for i := range 53 {
		name := fmt.Sprintf("picker_page%03d", i)
		sql("CREATE SCHEMA " + name + "; GRANT USAGE ON SCHEMA " + name + " TO reader")
		defer sql("DROP SCHEMA " + name)
	}
	p := a.Profile
	p.Scope = config.Scope{Mode: "whitelist"}
	req := database.ScopeRequest{Search: "picker_page"}
	first, err := d.BrowseScope(t.Context(), database.NewAccess(p, password), req)
	if err != nil || len(first.Schemas) != 50 || first.Next != "picker_page049" {
		t.Fatalf("first schema page: %+v %v", first, err)
	}
	req.After = first.Next
	next, err := d.BrowseScope(t.Context(), database.NewAccess(p, password), req)
	if err != nil || len(next.Schemas) != 3 || next.Schemas[0] != "picker_page050" || next.Next != "" {
		t.Fatal("second schema page", err)
	}
	whitelist := config.Scope{Mode: "whitelist", Schemas: []string{"picker"}}
	blacklist := config.Scope{Mode: "blacklist", Schemas: []string{"picker_empty"}}
	sql("CREATE TABLE picker.original(id int); GRANT SELECT ON picker.original TO reader")
	sql("ALTER TABLE picker.original RENAME TO renamed")
	sql("CREATE TABLE picker.future(id int); GRANT SELECT ON picker.future TO reader")
	sql("CREATE TABLE picker_empty.blocked(id int); GRANT SELECT ON picker_empty.blocked TO reader")
	defer sql("DROP TABLE picker_empty.blocked")
	sql("CREATE SCHEMA picker_future; GRANT USAGE ON SCHEMA picker_future TO reader; CREATE TABLE picker_future.items(id int); GRANT SELECT ON picker_future.items TO reader")
	defer sql("DROP SCHEMA picker_future CASCADE")
	for _, scope := range []config.Scope{whitelist, blacklist} {
		p.Scope = scope
		access := database.NewAccess(p, password)
		for _, name := range []string{"renamed", "future"} {
			if _, err := d.Query(t.Context(), access, database.QueryRequest{SQL: "SELECT id FROM picker." + name}); err != nil {
				t.Fatalf("%s excluded allowed table: %v", scope.Mode, err)
			}
			if _, err := d.DescribeTable(t.Context(), access, config.Table{Schema: "picker", Name: name}); err != nil {
				t.Fatal(err)
			}
		}
		page, err := d.ListTables(t.Context(), access, database.PageRequest{Schema: "picker"})
		if err != nil || len(page.Tables) != 2 {
			t.Fatalf("%s catalog: %+v %v", scope.Mode, page, err)
		}
		_, err = d.Query(t.Context(), access, database.QueryRequest{SQL: "SELECT id FROM picker_empty.blocked"})
		requireCode(t, err, contracts.ScopeDenied)
		_, err = d.DescribeTable(t.Context(), access, config.Table{Schema: "picker_empty", Name: "blocked"})
		requireCode(t, err, contracts.ScopeDenied)
		page, err = d.ListTables(t.Context(), access, database.PageRequest{Schema: "picker_empty"})
		if err != nil || len(page.Tables) != 0 {
			t.Fatal("excluded schema leaked metadata", err)
		}
		_, err = d.Query(t.Context(), access, database.QueryRequest{SQL: "SELECT id FROM picker_future.items"})
		if scope.Mode == "whitelist" {
			requireCode(t, err, contracts.ScopeDenied)
		} else if err != nil {
			t.Fatal("blacklist excluded future schema", err)
		}
	}
	t.Log("scope: schema pages, empty schemas, both policies, renamed/future tables and future schemas passed")
}

// The CLI child shares only the owned fixture manifest and synthetic password
// file. Its fake key provider prevents native credential access; driver calls are
// real. Keep it inside this verified Docker lifecycle, before teardown.
func cliDiagnosticsAcceptance(t *testing.T, root string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "test", "-mod=readonly", "-count=1", "-timeout=25s", "-run", "^TestConnectionDiagnostics$", "./internal/cli")
	cmd.Dir = filepath.Join("..", "..", "..")
	cmd.Env = append(os.Environ(), "DATA_MATE_CLI_DIAGNOSTICS="+filepath.Join(root, "manifest.json"), "GOPROXY=off", "GOSUMDB=off")
	cmd.WaitDelay = time.Second
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("CLI diagnostic fixture: %v\n%s", err, output)
	}
	t.Log("CLI diagnostic fixture: real driver, encrypted synthetic credentials, batch/single JSON and auth/TLS/read-only stages passed")
}
