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

// Extends the existing owned fixture; each exact-name selection must still pass
// current relation resolution after DDL. No query path is authorized by browsing.
func scopeAcceptance(t *testing.T, d *Driver, a database.Access, password string, sql func(string, ...any)) {
	t.Helper()
	sql(`CREATE SCHEMA picker; CREATE SCHEMA picker_empty; GRANT USAGE ON SCHEMA picker,picker_empty TO reader`)
	defer sql("DROP SCHEMA picker_empty")
	empty, err := d.BrowseScope(t.Context(), a, database.ScopeRequest{Search: "picker_empty"})
	if err != nil || len(empty.Schemas) != 1 || empty.Schemas[0] != "picker_empty" {
		t.Fatal("empty accessible schema cannot select future tables", err)
	}
	// Two real pages exercise keyset boundaries and server-side literal search.
	for i := range 53 {
		sql(fmt.Sprintf("CREATE TABLE picker.t%03d(id int)", i))
	}
	sql("GRANT SELECT ON ALL TABLES IN SCHEMA picker TO reader")
	defer sql("DROP SCHEMA picker CASCADE")
	p := a.Profile
	p.Scope = config.Scope{Mode: "selected"}
	req := database.ScopeRequest{Schema: "picker"}
	first, err := d.BrowseScope(t.Context(), database.NewAccess(p, password), req)
	if err != nil || len(first.Tables) != 50 || first.Next != "t049" {
		t.Fatalf("first picker page: %d %s %v", len(first.Tables), first.Next, err)
	}
	req.After = first.Next
	next, err := d.BrowseScope(t.Context(), database.NewAccess(p, password), req)
	if err != nil || len(next.Tables) != 3 || next.Tables[0].Name != "t050" || next.Next != "" {
		t.Fatal("second picker page", err)
	}
	p.Scope = config.Scope{Mode: "selected", Tables: []config.Table{{Schema: "picker", Name: "t000"}}}
	query := func(p config.Profile, name string) error {
		_, err := d.Query(t.Context(), database.NewAccess(p, password), database.QueryRequest{SQL: "SELECT id FROM picker." + name})
		return err
	}
	if err = query(p, "t000"); err != nil {
		t.Fatal(err)
	}
	sql("ALTER TABLE picker.t000 RENAME TO renamed")
	requireCode(t, query(p, "t000"), contracts.ScopeDenied)
	requireCode(t, query(p, "renamed"), contracts.ScopeDenied)
	sql("CREATE TABLE picker.t000(id int); GRANT SELECT ON picker.t000 TO reader")
	if err = query(p, "t000"); err != nil {
		t.Fatal("name recreation not resolved afresh", err)
	}
	sql("CREATE TABLE picker.future(id int); GRANT SELECT ON picker.future TO reader")
	requireCode(t, query(p, "future"), contracts.ScopeDenied)
	p.Scope = config.Scope{Mode: "selected", Schemas: []string{"picker"}}
	if err = query(p, "future"); err != nil {
		t.Fatal("whole schema excluded future table", err)
	}
	p.Scope = config.Scope{Mode: "all"}
	if err = query(p, "future"); err != nil {
		t.Fatal(err)
	}
	p.Scope = config.Scope{Mode: "selected"}
	requireCode(t, query(p, "future"), contracts.ScopeDenied)
	t.Log("scope: bounded catalog pages, full role visibility, exact names, rename/recreation and future-table semantics passed")
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
	t.Log("CLI diagnostic fixture: real driver, encrypted synthetic credentials, batch/single JSON and auth/TLS/policy stages passed")
}
