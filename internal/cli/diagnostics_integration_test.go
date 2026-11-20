package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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
		Port                           int
		Root, Container, Owner, Driver string
	}
	raw, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(raw, &fixture) != nil {
		t.Fatal("invalid owned fixture")
	}
	// Each driver's fixture has its own ownership prefix, port and admin account.
	prefix, port, admin := "dm-pg-", "5432/tcp", "postgres"
	if fixture.Driver == "mysql" || fixture.Driver == "mariadb" {
		prefix, port, admin = "dm-my-", "3306/tcp", "root"
	} else if fixture.Driver == "redis" {
		prefix, port = "dm-redis-", "6379/tcp"
	} else if fixture.Driver == "mongodb" {
		prefix, port, admin = "dm-mongo-", "27017/tcp", "root"
	} else if fixture.Driver != "" && fixture.Driver != "postgres" {
		t.Fatal("unknown fixture driver")
	}
	raw, err = exec.Command("docker", "inspect", "--format", `{{index .Config.Labels "com.data-mate.fixture"}}`, fixture.Container).Output()
	if err != nil || strings.TrimSpace(string(raw)) != fixture.Owner || !strings.HasPrefix(fixture.Owner, prefix) {
		t.Fatal("unowned diagnostic target")
	}
	raw, err = exec.Command("docker", "port", fixture.Container, port).Output()
	if err != nil || strings.TrimSpace(string(raw)) != "127.0.0.1:"+strconv.Itoa(fixture.Port) {
		t.Fatal("diagnostic endpoint mismatch")
	}
	connectPort := strconv.Itoa(fixture.Port)
	if fixture.Driver == "redis" {
		// The fixture runs verified TLS on a separate loopback port.
		raw, err = exec.Command("docker", "port", fixture.Container, "6380/tcp").Output()
		endpoint := strings.TrimSpace(string(raw))
		value, ok := strings.CutPrefix(endpoint, "127.0.0.1:")
		n, parseErr := strconv.ParseUint(value, 10, 16)
		if err != nil || !ok || parseErr != nil || n == 0 {
			t.Fatal("invalid diagnostic TLS endpoint")
		}
		connectPort = strconv.FormatUint(n, 10)
	}
	postgres.SanitizeEnvironment()
	root := privateRoot(t)
	keys := &testKeys{}
	raw, err = os.ReadFile(filepath.Join(fixture.Root, "reader-password"))
	if err != nil {
		t.Fatal(err)
	}
	password := strings.TrimSpace(string(raw))
	// Seed historical profiles through the isolated fake; use the real driver below.
	for _, alias := range []string{"bad-auth", "extra-grants", "bad-tls", "good"} {
		secret := password
		user := "reader"
		extra := []string{}
		if alias == "bad-auth" {
			secret = "synthetic-wrong-password"
		}
		if alias == "extra-grants" && fixture.Driver == "redis" {
			user = "unsafe" // Grants a write capability through an ACL selector.
		} else if alias == "extra-grants" {
			user = admin
			raw, err = os.ReadFile(filepath.Join(fixture.Root, "admin-password"))
			if err != nil {
				t.Fatal(err)
			}
			secret = strings.TrimSpace(string(raw))
		}
		if alias == "bad-tls" {
			extra = []string{"--tls", "--tls-ca", filepath.Join(fixture.Root, "bad.crt")}
		} else if fixture.Driver == "redis" {
			extra = []string{"--tls", "--tls-ca", filepath.Join(fixture.Root, "ca.crt")}
		}
		args := []string{"add", "--alias", alias, "--host", "localhost", "--port", connectPort, "--username", user, "--password-stdin", "--yes"}
		switch fixture.Driver {
		case "mysql", "mariadb":
			args = append(args, "--driver", fixture.Driver)
		case "redis":
			args = append(args, "--driver", "redis", "--database", "1")
		case "mongodb":
			// The default admin authentication database holds every fixture user.
			args = append(args, "--driver", "mongodb")
		default:
			args = append(args, "--database", "fixture")
		}
		command(t, root, keys, secret+"\n", 0, append(args, extra...)...)
	}
	t.Setenv("DATA_MATE_CLI_REAL_DRIVER", "1")
	before := files(t, root)
	command(t, root, keys, "synthetic-wrong-password\n", 1, "edit", "good", "--password-stdin", "--yes")
	command(t, root, keys, "", 1, "edit", "extra-grants", "--yes")
	if !reflect.DeepEqual(before, files(t, root)) {
		t.Fatal("failed live validation published edit")
	}
	command(t, root, keys, "", 0, "edit", "good", "--yes")
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
		if r.Stage != want || r.OK != (i == 3) {
			t.Fatalf("stage %d: %+v", i, r)
		}
	}
	out, _ = command(t, root, keys, "", 0, "test", "good", "--json")
	if strings.Contains(out, password) {
		t.Fatal("live diagnostic leaked password")
	}
	// Reuse the live CLI/service fixture for the complete describe path, including
	// cross-schema access and all-or-nothing output on connection failures.
	out, _ = command(t, root, keys, "", 0, "describe", "good", "--json")
	var description database.DatabaseDescription
	if contracts.Validate("db-describe.output", []byte(out)) != nil || json.Unmarshal([]byte(out), &description) != nil || strings.Contains(out, password) {
		t.Fatal("live description contract or redaction")
	}
	if fixture.Driver == "redis" {
		if description.RedisKeys == nil || description.Database != "1" || strings.Contains(out, "private:secret") || strings.Contains(out, "view:wrongdatabase") {
			t.Fatal("description returned keys outside the selected database or ACL")
		}
		found := false
		for _, item := range description.RedisKeys.Keys {
			if item.Key.Value == "view:hash" && item.Type == "hash" {
				found = true
			}
		}
		if !found {
			t.Fatal("description omitted readable Redis hash")
		}
	} else {
		found := false
		for _, s := range description.Schemas {
			if s.Name == "hidden" {
				found = true
				if len(s.Tables) == 0 {
					t.Fatal("readable catalog missing")
				}
			}
		}
		if !found {
			t.Fatal("description omitted accessible schema")
		}
	}
	for _, alias := range []string{"bad-auth", "bad-tls"} {
		out, stderr := command(t, root, keys, "", 1, "describe", alias, "--json")
		if out != "" || strings.Contains(stderr, password) || strings.Contains(stderr, "synthetic-wrong-password") {
			t.Fatal("failed description returned partial data or secret")
		}
	}
}
