package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
)

// Step 3: no prior CLI scenario owned staged batch diagnostics. CRUD assertions
// cannot establish continue-on-failure, one/all selection or machine result shape.
func TestConnectionDiagnostics(t *testing.T) {
	if path := os.Getenv("DATA_MATE_CLI_DIAGNOSTICS"); path != "" {
		liveDiagnostics(t, path)
		return
	}
	root := privateRoot(t)
	keys := &testKeys{}
	for _, alias := range []string{"good", "bad", "missing"} {
		command(t, root, keys, "synthetic-secret\n", 0, "add", "--alias", alias, "--host", "localhost", "--database", "app", "--username", "reader", "--password-stdin", "--yes")
	}
	p := snapshot(t, root)
	for i := range p.Connections {
		if p.Connections[i].Alias == "missing" {
			p.Connections[i].CredentialRef = "00000000-0000-4000-8000-000000000009"
		}
	}
	saveProfiles(t, root, p)
	runCommand := func(action string, d *fixtureDatabase, want int, args ...string) (string, *fixtureDatabase) {
		t.Helper()
		cmd := commandWithDatabase(Build{}, keys, func() (cliDatabase, error) { return d, nil })
		cmd.SetArgs(append([]string{"--root", root, "db", action}, args...))
		cmd.SetIn(strings.NewReader(""))
		var out, diag bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&diag)
		before := files(t, root)
		err := cmd.ExecuteContext(t.Context())
		if ExitCode(err) != want {
			t.Fatalf("diagnostics: %v %s", err, &out)
		}
		if !reflect.DeepEqual(before, files(t, root)) || strings.Contains(out.String()+diag.String(), "synthetic-secret") {
			t.Fatal("diagnostics wrote state or exposed credentials")
		}
		if strings.Contains(out.String(), "\x1b") || diag.Len() != 0 {
			t.Fatalf("diagnostics emitted color or unexpected stderr: %q %q", out.String(), diag.String())
		}
		if action == "describe" && err != nil && out.Len() != 0 {
			t.Fatal("failed description emitted partial stdout")
		}
		return out.String(), d
	}
	run := func(want int, args ...string) (string, *fixtureDatabase) {
		t.Helper()
		return runCommand("test", &fixtureDatabase{}, want, args...)
	}
	runJSON := func(want int, args ...string) ([]diagnosticResult, *fixtureDatabase) {
		t.Helper()
		out, d := run(want, append(args, "--json")...)
		if contracts.Validate("db-test.output", []byte(out)) != nil {
			t.Fatalf("invalid diagnostics output: %s", out)
		}
		var report struct {
			Results []diagnosticResult `json:"results"`
		}
		if json.Unmarshal([]byte(out), &report) != nil {
			t.Fatal("decode diagnostics")
		}
		return report.Results, d
	}
	results, d := runJSON(1)
	if len(results) != 3 || results[0].Alias != "bad" || results[0].Stage != "authentication" || results[0].OK || !results[1].OK || results[2].Error.Code != contracts.CredentialMissing || results[2].Stage != "vault" || !reflect.DeepEqual(d.tested, []string{"bad", "good"}) || !d.closed {
		t.Fatalf("batch diagnostics: %+v", results)
	}
	for _, r := range results {
		for _, s := range r.Stages {
			if !s.OK && s.Error == nil {
				t.Fatal("failed stage without safe error")
			}
		}
	}
	// Extend the staged diagnostics scenario: successful checks must not obscure
	// failed checks in human output, and pipes must never receive ANSI escapes.
	if out, _ := run(0, "good"); out != "good  PASS\n" {
		t.Fatalf("single success: %q", out)
	}
	wantBatch := fmt.Sprintf("bad  FAIL\n  authentication: %s: %s\ngood  PASS\nmissing  FAIL\n  vault: %s: %s\n", results[0].Error.Code, results[0].Error.Message, results[2].Error.Code, results[2].Error.Message)
	if out, _ := run(1); out != wantBatch {
		t.Fatalf("compact batch diagnostics: %q", out)
	}
	results, d = runJSON(0, "good")
	if len(results) != 1 || len(results[0].Stages) != 6 || len(d.tested) != 1 {
		t.Fatal("single diagnostics")
	}
	// Driver-specific config rejection precedes credential access and remains
	// a per-profile failure, so valid neighbors still produce their results.
	original := snapshot(t, root)
	invalidProfile := snapshot(t, root)
	for i := range invalidProfile.Connections {
		if invalidProfile.Connections[i].Alias == "good" {
			invalidProfile.Connections[i].Connection.Host = "/tmp/synthetic-invalid-host"
		}
	}
	saveProfiles(t, root, invalidProfile)
	calls := keys.calls
	results, d = runJSON(2, "good")
	if len(results) != 1 || results[0].Stage != "config" || len(results[0].Stages) != 1 || keys.calls != calls || len(d.tested) != 0 {
		t.Fatal("invalid config reached vault or driver")
	}
	saveProfiles(t, root, original)
	// Extend the existing CLI/service database scenario: descriptions must pair
	// the full user catalog with saved policy without altering stored settings.
	for _, scope := range []config.Scope{
		{Mode: "blacklist", Schemas: []string{"missing", "private"}},
		{Mode: "whitelist", Schemas: []string{"missing", "public"}},
		{Mode: "whitelist"},
		{Mode: "blacklist"},
	} {
		profiles := snapshot(t, root)
		for i := range profiles.Connections {
			if profiles.Connections[i].Alias == "good" {
				profiles.Connections[i].Scope = scope
			}
		}
		saveProfiles(t, root, profiles)
		out, d := runCommand("describe", &fixtureDatabase{}, 0, "good", "--json")
		var description database.DatabaseDescription
		if contracts.Validate("db-describe.output", []byte(out)) != nil || json.Unmarshal([]byte(out), &description) != nil {
			t.Fatal("description JSON contract", out)
		}
		if description.Alias != "good" || description.Database != "app" || !reflect.DeepEqual(description.Scope, scope) || len(description.Schemas) != 4 || len(d.described) != 1 || !d.closed {
			t.Fatalf("description snapshot: %+v", description)
		}
		for _, s := range description.Schemas {
			if s.Allowed != scope.ContainsSchema(s.Name) || s.Tables == nil {
				t.Fatal("description lost policy or empty collections")
			}
		}
		out, _ = runCommand("describe", &fixtureDatabase{}, 0, "good")
		for _, s := range description.Schemas {
			status := "excluded"
			if s.Allowed {
				status = "allowed"
			}
			if !strings.Contains(out, fmt.Sprintf("%q [%s]", s.Name, status)) {
				t.Fatal("human description lost schema status", out)
			}
		}
		if !strings.Contains(out, `"line\n\x1b[31m" (table)`) {
			t.Fatal("relation identifier not safely escaped", out)
		}
	}
	for _, args := range [][]string{{}, {"absent"}} {
		_, d := runCommand("describe", &fixtureDatabase{}, 2, args...)
		if len(d.described) != 0 {
			t.Fatal("invalid selection reached catalog")
		}
	}
	for _, e := range []error{database.Fail(contracts.ConnectFailed, "safe connection failure", false), database.Fail(contracts.ResourceLimit, "safe catalog limit", false), database.Fail(contracts.QueryTimeout, "safe query timeout", false), context.Canceled} {
		want := ExitFailure
		if e == context.Canceled {
			want = ExitCancelled
		}
		runCommand("describe", &fixtureDatabase{describeError: e}, want, "good", "--json")
	}
	out, _ := runCommand("describe", &fixtureDatabase{emptyCatalog: true}, 0, "good", "--json")
	if !strings.Contains(out, `"schemas":[]`) {
		t.Fatal("empty catalog was not an array")
	}
	runCommand("describe", &fixtureDatabase{emptyCatalog: true}, 0, "good")
	runCommand("describe", &fixtureDatabase{}, 1, "missing", "--json")
	saveProfiles(t, root, invalidProfile)
	runCommand("describe", &fixtureDatabase{}, 2, "good", "--json")
	saveProfiles(t, root, original)
	keys.denied = true
	runCommand("describe", &fixtureDatabase{}, 1, "good", "--json")
	results, d = runJSON(1)
	if len(d.tested) != 0 || len(results) != 3 {
		t.Fatal("vault denial reached driver or stopped batch")
	}
	for _, r := range results {
		if r.Stage != "vault" || r.Error.Code != contracts.VaultUnavailable {
			t.Fatal("vault denial misclassified")
		}
	}
	empty := privateRoot(t)
	command(t, empty, keys, "", 2, "describe", "--json")
	out, _ = command(t, empty, keys, "", 0, "test", "--json")
	if contracts.Validate("db-test.output", []byte(out)) != nil || len(files(t, empty)) != 1 {
		t.Fatal("empty diagnostic changed installation")
	}
	command(t, root, keys, "", 2, "test", "absent", "--json")
}
