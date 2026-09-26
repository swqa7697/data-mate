package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
	"github.com/swqa7697/data-mate/internal/database/postgres"
)

func liveDiagnostics(t *testing.T, path string) {
	t.Helper()
	if os.Getenv("CI") != "" {
		t.Fatal("owned integration excluded from CI")
	}
	var fixture struct {
		Port                   int
		Root, Container, Owner string
	}
	raw, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(raw, &fixture) != nil {
		t.Fatal("invalid owned fixture")
	}
	raw, err = exec.Command("docker", "inspect", "--format", `{{index .Config.Labels "com.data-mate.fixture"}}`, fixture.Container).Output()
	if err != nil || strings.TrimSpace(string(raw)) != fixture.Owner || !strings.HasPrefix(fixture.Owner, "dm-pg-") {
		t.Fatal("unowned diagnostic target")
	}
	raw, err = exec.Command("docker", "port", fixture.Container, "5432/tcp").Output()
	if err != nil || strings.TrimSpace(string(raw)) != "127.0.0.1:"+strconv.Itoa(fixture.Port) {
		t.Fatal("diagnostic endpoint mismatch")
	}
	postgres.SanitizeEnvironment()
	root := privateRoot(t)
	keys := &testKeys{}
	raw, err = os.ReadFile(filepath.Join(fixture.Root, "reader-password"))
	if err != nil {
		t.Fatal(err)
	}
	password := strings.TrimSpace(string(raw))
	for _, alias := range []string{"bad-auth", "extra-grants", "bad-tls", "good"} {
		secret := password
		user := "reader"
		extra := []string{}
		if alias == "bad-auth" {
			secret = "synthetic-wrong-password"
		}
		if alias == "extra-grants" {
			user = "postgres"
			raw, err = os.ReadFile(filepath.Join(fixture.Root, "admin-password"))
			if err != nil {
				t.Fatal(err)
			}
			secret = strings.TrimSpace(string(raw))
		}
		if alias == "bad-tls" {
			extra = []string{"--tls", "--tls-ca", filepath.Join(fixture.Root, "bad.crt")}
		}
		args := []string{"add", "--alias", alias, "--host", "localhost", "--port", strconv.Itoa(fixture.Port), "--database", "fixture", "--username", user, "--password-stdin", "--yes"}
		command(t, root, keys, secret+"\n", 0, append(args, extra...)...)
	}
	out, _ := command(t, root, keys, "", 1, "test", "--json")
	if contracts.Validate("db-test.output", []byte(out)) != nil {
		t.Fatal("live diagnostic schema")
	}
	var report struct {
		Results []diagnosticResult `json:"results"`
	}
	if json.Unmarshal([]byte(out), &report) != nil || len(report.Results) != 4 {
		t.Fatal("incomplete live diagnostics")
	}
	for i, want := range []string{"authentication", "dial", "read_only", "read_only"} {
		r := report.Results[i]
		if r.Stage != want || r.OK != (i == 2 || i == 3) {
			t.Fatalf("stage %d: %+v", i, r)
		}
	}
	out, _ = command(t, root, keys, "", 0, "test", "good", "--json")
	if strings.Contains(out, password) {
		t.Fatal("live diagnostic leaked password")
	}
	// Reuse the live CLI/service fixture for the complete describe path, including
	// scope exclusion labels and all-or-nothing output on connection failures.
	command(t, root, keys, "", 0, "scope", "good", "--exclude-schema", "hidden", "--yes")
	out, _ = command(t, root, keys, "", 0, "describe", "good", "--json")
	var description database.DatabaseDescription
	if contracts.Validate("db-describe.output", []byte(out)) != nil || json.Unmarshal([]byte(out), &description) != nil || strings.Contains(out, password) {
		t.Fatal("live description contract or redaction")
	}
	found := false
	for _, s := range description.Schemas {
		if s.Name == "hidden" {
			found = true
			if s.Allowed || len(s.Tables) == 0 {
				t.Fatal("excluded readable catalog missing")
			}
		}
	}
	if !found {
		t.Fatal("description filtered saved scope")
	}
	for _, alias := range []string{"bad-auth", "bad-tls"} {
		out, stderr := command(t, root, keys, "", 1, "describe", alias, "--json")
		if out != "" || strings.Contains(stderr, password) || strings.Contains(stderr, "synthetic-wrong-password") {
			t.Fatal("failed description returned partial data or secret")
		}
	}
}
