package postgres

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
