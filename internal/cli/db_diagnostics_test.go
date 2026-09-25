package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/swqa7697/data-mate/internal/contracts"
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
	run := func(want int, args ...string) ([]diagnosticResult, *fixtureDatabase) {
		t.Helper()
		d := &fixtureDatabase{}
		cmd := commandWithDatabase(Build{}, keys, func() (cliDatabase, error) { return d, nil })
		cmd.SetArgs(append([]string{"--root", root, "db", "test"}, args...))
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
		if contracts.Validate("db-test.output", out.Bytes()) != nil {
			t.Fatalf("invalid diagnostics output: %s", &out)
		}
		var report struct {
			Results []diagnosticResult `json:"results"`
		}
		if json.Unmarshal(out.Bytes(), &report) != nil {
			t.Fatal("decode diagnostics")
		}
		return report.Results, d
	}
	results, d := run(1, "--json")
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
	results, d = run(0, "good", "--json")
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
	results, d = run(2, "good", "--json")
	if len(results) != 1 || results[0].Stage != "config" || len(results[0].Stages) != 1 || keys.calls != calls || len(d.tested) != 0 {
		t.Fatal("invalid config reached vault or driver")
	}
	saveProfiles(t, root, original)
	keys.denied = true
	results, d = run(1, "--json")
	if len(d.tested) != 0 || len(results) != 3 {
		t.Fatal("vault denial reached driver or stopped batch")
	}
	for _, r := range results {
		if r.Stage != "vault" || r.Error.Code != contracts.VaultUnavailable {
			t.Fatal("vault denial misclassified")
		}
	}
	empty := privateRoot(t)
	out, _ := command(t, empty, keys, "", 0, "test", "--json")
	if contracts.Validate("db-test.output", []byte(out)) != nil || len(files(t, empty)) != 1 {
		t.Fatal("empty diagnostic changed installation")
	}
	command(t, root, keys, "", 2, "test", "absent", "--json")
}
