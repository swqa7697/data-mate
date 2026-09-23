package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/vault"
)

type testKeys struct {
	key    []byte
	denied bool
	calls  int
}

func (k *testKeys) Load(context.Context, string) ([]byte, error) {
	k.calls++
	if k.denied {
		return nil, vault.ErrDenied
	}
	if k.key == nil {
		return nil, vault.ErrMissing
	}
	return bytes.Clone(k.key), nil
}
func (k *testKeys) CreateIfAbsent(_ context.Context, _ string, key []byte) ([]byte, error) {
	k.calls++
	if k.denied {
		return nil, vault.ErrDenied
	}
	if k.key == nil {
		k.key = bytes.Clone(key)
	}
	return bytes.Clone(k.key), nil
}
func (k *testKeys) Delete(context.Context, string) error { k.key = nil; return nil }
func privateRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	return root
}
func command(t *testing.T, root string, keys vault.KeyProvider, input string, want int, args ...string) (string, string) {
	t.Helper()
	cmd := newCommand(Build{}, keys)
	cmd.SetArgs(append([]string{"--root", root, "db"}, args...))
	cmd.SetIn(strings.NewReader(input))
	var out, stderr bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&stderr)
	err := cmd.ExecuteContext(t.Context())
	code := ExitCode(err)
	// Cobra parse errors are translated to usage failures by Run.
	var public *Error
	if err != nil && !errors.As(err, &public) && !errors.Is(err, context.Canceled) {
		code = ExitInvalid
	}
	if code != want {
		t.Fatalf("command %v: code %d want %d; err=%v stdout=%s stderr=%s", args, code, want, err, &out, &stderr)
	}
	if err != nil {
		stderr.WriteString(err.Error())
	}
	return out.String(), stderr.String()
}
func snapshot(t *testing.T, root string) config.Profiles {
	t.Helper()
	r, e := config.ResolveRoot(root, "")
	if e != nil {
		t.Fatal(e)
	}
	p, _, e := config.Preview(t.Context(), r)
	if e != nil {
		t.Fatal(e)
	}
	return p
}
func credential(t *testing.T, root string, k *testKeys, id string) vault.Secrets {
	t.Helper()
	r, e := config.ResolveRoot(root, "")
	if e != nil {
		t.Fatal(e)
	}
	s, e := config.Open(t.Context(), r, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	l, e := s.ReadLease(t.Context())
	if e != nil {
		t.Fatal(e)
	}
	defer l.Release()
	v, e := vault.New(s, k).Credential(t.Context(), l, id)
	if e != nil {
		t.Fatal(e)
	}
	return v
}
func files(t *testing.T, root string) map[string]string {
	t.Helper()
	result := map[string]string{}
	err := filepath.WalkDir(root, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			result[path] = "directory"
			return nil
		}
		b, err := os.ReadFile(path)
		result[path] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func saveProfiles(t *testing.T, root string, p config.Profiles) {
	t.Helper()
	b, e := json.Marshal(p)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.MkdirAll(filepath.Join(root, "config"), 0700); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(root, "config/connections.json"), b, 0600); e != nil {
		t.Fatal(e)
	}
}

var basicAdd = []string{"add", "--alias", "analytics", "--host", "localhost", "--database", "app", "--username", "reader", "--yes"}

// No pre-P2 scenario exercised command-to-vault transactions. This scenario owns
// scripted CRUD, preservation, repair and durable partial outcomes end to end.
func TestConnectionCRUD(t *testing.T) {
	if root := os.Getenv("DATA_MATE_SCOPE_LEASE"); root != "" {
		scopeLeaseChild(t, root)
		return
	}
	scopeLeaseAcceptance(t)
	root := privateRoot(t)
	keys := &testKeys{}
	password := " synthetic-db-secret "
	out, diag := command(t, root, keys, password+"\r\n", 0, append(basicAdd, "--password-stdin")...)
	if strings.Contains(out+diag, password) {
		t.Fatal("password leaked")
	}
	p := snapshot(t, root)
	id := p.Connections[0].ID
	ref := p.Connections[0].CredentialRef
	if p.Connections[0].Scope.Mode != "all" || p.Connections[0].Transport.TLS.Mode != "disabled" || credential(t, root, keys, id).Password != password {
		t.Fatal("add defaults or exact secret lost")
	}
	before := files(t, root)
	command(t, root, keys, "", 2, append(basicAdd, "--passwordless")...)
	if !reflect.DeepEqual(before, files(t, root)) {
		t.Fatal("duplicate changed persisted files")
	}
	command(t, root, keys, "", 0, "edit", "analytics", "--alias", "renamed", "--none", "--yes")
	p = snapshot(t, root)
	if p.Connections[0].ID != id || p.Connections[0].CredentialRef != ref || p.Connections[0].Scope.ContainsName("public", "t") {
		t.Fatal("rename/empty scope/credential preservation")
	}
	calls := keys.calls
	out, diag = command(t, root, keys, "", 0, "ls", "--json")
	if contracts.Validate("db-list.output", []byte(out)) != nil || keys.calls != calls || diag != "" || strings.Contains(out, "credential_ref") {
		t.Fatal("list contract or key access")
	}
	for path, b := range files(t, root) {
		if strings.Contains(b, password) {
			t.Fatalf("plaintext persisted in %s", path)
		}
	}
	command(t, root, keys, `{"ssh_password":"synthetic-ssh-secret","password":"replacement-secret"}`, 0, "edit", "renamed", "--ssh-host", "jump.local", "--ssh-user", "jump", "--tls", "--tls-ca", "/tmp/synthetic-ca.pem", "--query-timeout", "750ms", "--max-rows", "12", "--max-result-bytes", "4096", "--schema", "public", "--table", "other.Exact", "--credentials-stdin", "--yes")
	p = snapshot(t, root)
	s := credential(t, root, keys, id)
	if s.SSHPassword != "synthetic-ssh-secret" || s.Password != "replacement-secret" || p.Connections[0].Limits.QueryTimeoutMS != 750 || !p.Connections[0].Scope.ContainsName("other", "Exact") {
		t.Fatal("advanced settings or secrets lost")
	}
	command(t, root, keys, "", 0, "edit", "renamed", "--clear-password", "--tls", "--scope-json", `{"mode":"selected","tables":[{"schema":"a.b","name":"c.d"}]}`, "--yes")
	p = snapshot(t, root)
	if p.Connections[0].Transport.TLS.CAFile != "/tmp/synthetic-ca.pem" || !p.Connections[0].Scope.ContainsName("a.b", "c.d") {
		t.Fatal("TLS edit lost omitted CA or structured scope lost exact identifiers")
	}
	s = credential(t, root, keys, id)
	if s.Password != "" || s.SSHPassword != "synthetic-ssh-secret" {
		t.Fatal("clear removed unrelated secret")
	}
	command(t, root, keys, "", 0, "edit", "renamed", "--clear-ssh", "--tls=false", "--yes")
	p = snapshot(t, root)
	if p.Connections[0].CredentialRef != "" || p.Connections[0].Transport.SSH != nil {
		t.Fatal("clearing final secrets retained reference")
	}
	// P7 scope replacement is nonsecret and requires neither network nor vault.
	beforeScope := snapshot(t, root).Connections[0]
	calls = keys.calls
	keys.denied = true
	for _, args := range [][]string{
		{"--all"}, {"--none"}, {"--schema", "public", "--table", "other.Exact"},
		{"--scope-json", `{"mode":"selected","tables":[{"schema":"a.b","name":"c.d"}]}`},
	} {
		command(t, root, keys, "", 0, append([]string{"scope", "renamed", "--yes"}, args...)...)
	}
	afterScope := snapshot(t, root).Connections[0]
	if !afterScope.Scope.ContainsName("a.b", "c.d") || afterScope.Scope.ContainsName("public", "future") || keys.calls != calls {
		t.Fatal("scope replacement used credentials or lost exact names")
	}
	afterScope.Scope = beforeScope.Scope
	if !reflect.DeepEqual(afterScope, beforeScope) {
		t.Fatal("scope modified unrelated profile fields")
	}
	before = files(t, root)
	for _, args := range [][]string{
		{"scope", "renamed", "--yes"}, {"scope", "renamed", "--none"},
		{"scope", "renamed", "--all", "--none", "--yes"},
		{"scope", "renamed", "--table", "a.b.c", "--yes"},
		{"scope", "--all", "--yes"},
	} {
		command(t, root, keys, "", 2, args...)
	}
	if !reflect.DeepEqual(before, files(t, root)) {
		t.Fatal("invalid scope changed state")
	}
	keys.denied = false
	// Missing manual bundles are reported and repaired without activation records.
	manual := privateRoot(t)
	keys = &testKeys{}
	p.Connections[0].CredentialRef = "00000000-0000-4000-8000-000000000009"
	saveProfiles(t, manual, p)
	before = files(t, manual)
	command(t, manual, keys, "", 0, "list", "--json")
	if !reflect.DeepEqual(before, files(t, manual)) {
		t.Fatal("manual list initialized state")
	}
	_, diag = command(t, manual, keys, "", 1, "edit", "renamed", "--alias", "manual", "--yes")
	if !strings.Contains(diag, "CREDENTIAL_MISSING") || !strings.Contains(diag, "db edit") || snapshot(t, manual).Connections[0].Alias != "renamed" {
		t.Fatal("missing credential was not reported/preserved")
	}
	command(t, manual, keys, "repaired-secret\n", 0, "edit", "renamed", "--alias", "manual", "--password-stdin", "--yes")
	if credential(t, manual, keys, id).Password != "repaired-secret" {
		t.Fatal("manual credential repair failed")
	}
	// A durable removal must remain visible even when the OS denies cleanup.
	keys.denied = true
	_, diag = command(t, manual, keys, "", 1, "rm", "manual", "--yes")
	if len(snapshot(t, manual).Connections) != 0 || !strings.Contains(diag, "change saved") {
		t.Fatal("partial removal not reported")
	}
	keys.denied = false
	command(t, manual, keys, "", 0, append(basicAdd, "--passwordless")...)
	command(t, manual, keys, "", 0, "remove", "analytics", "--yes")
	if len(snapshot(t, manual).Connections) != 0 {
		t.Fatal("remove failed")
	}
	// Reject malformed established bytes, leaving them available for manual repair.
	if err := os.WriteFile(filepath.Join(manual, "config/connections.json"), []byte(`{"version":1,"password":"synthetic-bad-secret"}`), 0600); err != nil {
		t.Fatal(err)
	}
	before = files(t, manual)
	out, diag = command(t, manual, keys, "", 2, "list", "--json")
	if strings.Contains(out+diag, "synthetic-bad-secret") || !reflect.DeepEqual(before, files(t, manual)) {
		t.Fatal("malformed profile changed or leaked")
	}
}

// Curated input failures exercise the CLI ownership boundary, including bounded
// stdin and preview-time races which pure profile/vault tests cannot cover.
func TestConnectionInputs(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		extra       []string
	}{
		{"extra line", "secret-sentinel\nextra\n", []string{"--password-stdin"}},
		{"bare carriage return", "secret-sentinel\r", []string{"--password-stdin"}},
		{"oversized", strings.Repeat("s", vault.MaxSecretBytes+1), []string{"--password-stdin"}},
		{"invalid utf8", string([]byte{255}), []string{"--password-stdin"}},
		{"duplicate JSON", `{"password":"secret-sentinel","password":"x"}`, []string{"--credentials-stdin"}},
		{"unknown JSON", `{"password":"secret-sentinel","other":"x"}`, []string{"--credentials-stdin"}},
		{"null JSON", `{"password":null}`, []string{"--credentials-stdin"}},
		{"missing add password", `{}`, []string{"--credentials-stdin"}},
		{"URL", "", []string{"--passwordless", "--host", "postgres://reader:secret-sentinel@host/db"}},
		{"proxy URL", "", []string{"--passwordless", "--proxy", "socks5://reader:secret-sentinel@host:1080"}},
		{"noninteractive host enrollment", "", []string{"--passwordless", "--ssh-enroll"}},
		{"nonTTY host enrollment", "", []string{"--passwordless", "--ssh-enroll", "--yes=false"}},
		{"password argv", "", []string{"--password", "secret-sentinel"}},
		{"scope conflict", "", []string{"--passwordless", "--none", "--all"}},
		{"transport conflict", "", []string{"--passwordless", "--ssh-host", "jump", "--ssh-user", "u", "--proxy", "socks5://host:1080"}},
		{"CA without TLS", "", []string{"--passwordless", "--tls-ca", "/tmp/ca.pem"}},
		{"stdin confirmation", "secret-sentinel", []string{"--password-stdin", "--yes=false"}},
		{"missing confirmation", "", []string{"--passwordless", "--yes=false"}},
		{"invalid limit", "", []string{"--passwordless", "--max-rows", "0"}},
		{"no alias nonTTY", "", []string{"--passwordless", "--alias", ""}},
	} {
		root := privateRoot(t)
		keys := &testKeys{}
		before := files(t, root)
		out, diag := command(t, root, keys, tc.input, 2, append(append([]string{}, basicAdd...), tc.extra...)...)
		if keys.calls != 0 || !reflect.DeepEqual(before, files(t, root)) || strings.Contains(out+diag, "secret-sentinel") {
			t.Fatalf("%s changed state or leaked input", tc.name)
		}
	}
	root := privateRoot(t)
	keys := &testKeys{}
	keyFile := filepath.Join(t.TempDir(), "source-key")
	keyText := "synthetic-private-key\n"
	if err := os.WriteFile(keyFile, []byte(keyText), 0600); err != nil {
		t.Fatal(err)
	}
	command(t, root, keys, `{"ssh_key_passphrase":"synthetic-passphrase"}`, 0, append(basicAdd, "--passwordless", "--credentials-stdin", "--ssh-host", "jump", "--ssh-user", "u", "--ssh-key-file", keyFile)...)
	p := snapshot(t, root)
	id := p.Connections[0].ID
	if credential(t, root, keys, id).SSHPrivateKey != keyText {
		t.Fatal("key not imported")
	}
	if err := os.Remove(keyFile); err != nil {
		t.Fatal(err)
	}
	command(t, root, keys, "", 0, "edit", "analytics", "--database", "changed", "--yes")
	if credential(t, root, keys, id).SSHKeyPassphrase != "synthetic-passphrase" {
		t.Fatal("edit reread key source or lost passphrase")
	}
	command(t, root, keys, `{"proxy_password":"proxy-secret"}`, 0, "edit", "analytics", "--clear-ssh", "--proxy", "socks5://[::1]:1080", "--proxy-user", "proxyuser", "--credentials-stdin", "--yes")
	s := credential(t, root, keys, id)
	if s.SSHPrivateKey != "" || s.ProxyPassword != "proxy-secret" {
		t.Fatal("transport switch failed")
	}
	// Change config as the preview is written: the confirmed stale mutation fails.
	cmd := newCommand(Build{}, keys)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetArgs([]string{"--root", root, "db", "scope", "analytics", "--none", "--yes"})
	changedOnce := false
	cmd.SetErr(writerFunc(func(b []byte) (int, error) {
		if !changedOnce {
			changedOnce = true
			p := snapshot(t, root)
			p.Connections[0].Connection.Database = "newer"
			saveProfiles(t, root, p)
		}
		return len(b), nil
	}))
	if err := cmd.ExecuteContext(t.Context()); ExitCode(err) != 1 {
		t.Fatal("stale preview accepted", err)
	}
	p = snapshot(t, root)
	if p.Connections[0].Alias != "analytics" || p.Connections[0].Connection.Database != "newer" {
		t.Fatal("newer manual state overwritten")
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }
