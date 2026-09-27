package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
)

// Extends the existing owned fixture with catalog metadata, bounds and future-schema access.
func catalogAcceptance(t *testing.T, d *Driver, a database.Access, password string, sql func(string, ...any)) {
	t.Helper()
	sql(`CREATE SCHEMA picker; CREATE SCHEMA picker_empty; GRANT USAGE ON SCHEMA picker,picker_empty TO reader`)
	defer sql("DROP SCHEMA picker_empty")
	defer sql("DROP SCHEMA picker CASCADE")
	// Extend the catalog fixture: descriptions preserve empty schemas, list only
	// catalog-visible relations independently of data grants.
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
	{
		description, err := d.DescribeDatabase(t.Context(), a)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(description)
		if contracts.Validate("db-describe.output", raw) != nil {
			t.Fatal("database description contract", string(raw))
		}
		foundEmpty, foundPicker := false, false
		previous := ""
		for _, s := range description.Schemas {
			if s.Name <= previous {
				t.Fatalf("schema visibility/order: %+v", s)
			}
			previous = s.Name
			if s.Name == "picker_empty" {
				foundEmpty = len(s.Tables) == 1 && s.Tables[0].Name == "unreadable"
			}
			if s.Name == "picker" {
				foundPicker = true
				want := []database.RelationName{{Name: "a_view", Kind: "view"}, {Name: "column_only", Kind: "table"}, {Name: "foreign_table", Kind: "foreign_table"}, {Name: "materialized", Kind: "materialized_view"}, {Name: "partitioned", Kind: "partitioned_table"}, {Name: "unreadable", Kind: "table"}}
				if !reflect.DeepEqual(s.Tables, want) {
					t.Fatalf("readable relations: %+v", s.Tables)
				}
			}
		}
		if !foundEmpty || !foundPicker {
			t.Fatal("description omitted accessible or empty schema")
		}
		page, err := d.ListTables(t.Context(), a, database.PageRequest{Schema: "picker"})
		if err != nil || len(page.Tables) != 6 {
			t.Fatal("agent catalog omitted readable relations", err)
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
	// Newly granted schemas and renamed tables need no connection reconfiguration.
	sql("CREATE TABLE picker.original(id int); GRANT SELECT ON picker.original TO reader")
	sql("ALTER TABLE picker.original RENAME TO renamed")
	sql("CREATE SCHEMA picker_future; GRANT USAGE ON SCHEMA picker_future TO reader; CREATE TABLE picker_future.items(id int); GRANT SELECT ON picker_future.items TO reader")
	defer sql("DROP SCHEMA picker_future CASCADE")
	for _, name := range []config.Table{{Schema: "picker", Name: "renamed"}, {Schema: "picker_future", Name: "items"}} {
		if _, err := d.Query(t.Context(), a, database.QueryRequest{SQL: "SELECT id FROM " + name.Schema + "." + name.Name}); err != nil {
			t.Fatal("new or renamed relation inaccessible", err)
		}
		if _, err := d.DescribeTable(t.Context(), a, name); err != nil {
			t.Fatal(err)
		}
		page, err := d.ListTables(t.Context(), a, database.PageRequest{Schema: name.Schema})
		if err != nil || len(page.Tables) != 1 || page.Tables[0].Name != name.Name {
			t.Fatalf("new or renamed catalog relation: %+v %v", page, err)
		}
	}
	t.Log("catalog: empty schemas, unfiltered metadata, complete-result bounds and new/renamed relations passed")

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
