package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/vault"
)

type testKeys struct {
	key     []byte
	denied  bool
	calls   int
	keyring string
}

func (k *testKeys) Load(ctx context.Context, _ string) ([]byte, error) {
	k.calls++
	if k.keyring != "" {
		if err := k.prepare(ctx); err != nil {
			return nil, err
		}
	}
	if k.denied {
		return nil, vault.ErrDenied
	}
	if k.key == nil {
		return nil, vault.ErrMissing
	}
	return bytes.Clone(k.key), nil
}
func (k *testKeys) CreateIfAbsent(ctx context.Context, _ string, key []byte) ([]byte, error) {
	k.calls++
	if k.keyring != "" {
		if err := k.prepare(ctx); err != nil {
			return nil, err
		}
	}
	if k.denied {
		return nil, vault.ErrDenied
	}
	if k.key == nil {
		k.key = bytes.Clone(key)
	}
	return bytes.Clone(k.key), nil
}
func (k *testKeys) prepare(ctx context.Context) error {
	for attempt := 1; attempt <= 3; attempt++ {
		password, err := vault.AskKeyring(ctx, vault.KeyringChallenge{Kind: k.keyring, Attempt: attempt})
		if err != nil {
			return err
		}
		match := string(password) == "pty-keyring-secret"
		clear(password)
		if match {
			return nil
		}
	}
	return vault.ErrPassword
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
	repo := vault.New(s, k)
	if e = repo.Unlock(t.Context(), false, ""); e != nil {
		t.Fatal(e)
	}
	defer repo.Close()
	l, e := s.ReadLease(t.Context())
	if e != nil {
		t.Fatal(e)
	}
	defer l.Release()
	v, e := repo.Credential(t.Context(), l, id)
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
	r, e := config.ResolveRoot(root, "")
	if e != nil {
		t.Fatal(e)
	}
	store, e := config.Open(t.Context(), r, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	l, e := store.WriteLease(t.Context())
	if e != nil {
		t.Fatal(e)
	}
	defer l.Release()
	// Deliberately bypass constraints only in this corruption fixture.
	db, e := sql.Open("sqlite3", filepath.Join(root, "data-mate.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	tx, e := db.Begin()
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback()
	if _, e = tx.Exec("DELETE FROM profiles"); e != nil {
		t.Fatal(e)
	}
	for _, profile := range p.Connections {
		raw, _ := json.Marshal(profile)
		var ref any
		if profile.CredentialRef != "" {
			ref = profile.CredentialRef
		}
		if _, e = tx.Exec("INSERT INTO profiles VALUES(?,?,?,?)", profile.ID, profile.Alias, raw, ref); e != nil {
			t.Fatal(e)
		}
	}
	if _, e = tx.Exec("UPDATE installation SET generation=generation+1"); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
}

var basicAdd = []string{"add", "--alias", "analytics", "--host", "localhost", "--database", "app", "--username", "reader", "--yes"}

// No pre-P2 scenario exercised command-to-vault transactions. This scenario owns
// scripted CRUD, preservation, repair and durable partial outcomes end to end.
func TestConnectionCRUD(t *testing.T) {
	if root := os.Getenv("DATA_MATE_EDIT_LEASE"); root != "" {
		editLeaseChild(t, root)
		return
	}
	editLeaseAcceptance(t)
	root := privateRoot(t)
	keys := &testKeys{}
	password := " synthetic-db-secret "
	out, diag := command(t, root, keys, password+"\r\n", 0, append(basicAdd, "--password-stdin")...)
	if strings.Contains(out+diag, password) {
		t.Fatal("password leaked")
	}
	p := snapshot(t, root)
	if p.Connections[0].Limits.QueryTimeoutMS != 60000 {
		t.Fatal("new connection did not save the 60-second default")
	}
	id := p.Connections[0].ID
	ref := p.Connections[0].CredentialRef
	if p.Connections[0].Transport.TLS.Mode != "disabled" || credential(t, root, keys, id).Password != password {
		t.Fatal("add defaults or exact secret lost")
	}
	before := files(t, root)
	command(t, root, keys, "", 2, append(basicAdd, "--passwordless")...)
	if !reflect.DeepEqual(before, files(t, root)) {
		t.Fatal("duplicate changed persisted files")
	}
	command(t, root, keys, "", 0, "edit", "analytics", "--alias", "renamed", "--yes")
	p = snapshot(t, root)
	if p.Connections[0].ID != id || p.Connections[0].CredentialRef != ref {
		t.Fatal("rename/credential preservation")
	}

	// Completion reuses this CRUD fixture and must not unlock credentials or write.
	completionBefore := files(t, root)
	completionCalls := keys.calls
	for _, action := range []string{"edit", "remove", "rm", "test", "describe"} {
		cmd := newCommand(Build{}, keys)
		var output bytes.Buffer
		cmd.SetOut(&output)
		cmd.SetErr(&output)
		cmd.SetArgs([]string{"--root", root, "__complete", "db", action, "ren"})
		if err := cmd.ExecuteContext(t.Context()); err != nil || !strings.Contains(output.String(), "renamed\n") {
			t.Fatal("alias completion", action, err, output.String())
		}
	}
	if keys.calls != completionCalls || !reflect.DeepEqual(completionBefore, files(t, root)) {
		t.Fatal("completion accessed secrets or mutated state")
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
	command(t, root, keys, `{"ssh_password":"synthetic-ssh-secret","password":"replacement-secret"}`, 0, "edit", "renamed", "--ssh-host", "jump.local", "--ssh-user", "jump", "--tls", "--tls-ca", "/tmp/synthetic-ca.pem", "--query-timeout", "750ms", "--max-rows", "12", "--max-result-bytes", "4096", "--credentials-stdin", "--yes")
	p = snapshot(t, root)
	s := credential(t, root, keys, id)
	if s.SSHPassword != "synthetic-ssh-secret" || s.Password != "replacement-secret" || p.Connections[0].Limits.QueryTimeoutMS != 750 {
		t.Fatal("advanced settings or secrets lost")
	}
	connectionTransfer(t, root, keys)
	command(t, root, keys, "", 0, "edit", "renamed", "--clear-password", "--tls", "--yes")
	p = snapshot(t, root)
	if p.Connections[0].Transport.TLS.CAFile != "/tmp/synthetic-ca.pem" {
		t.Fatal("TLS edit lost omitted CA")
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
	// An operator can save the full five-minute budget through the normal edit flow.
	command(t, root, keys, "", 0, "edit", "renamed", "--query-timeout", "5m", "--yes")
	if snapshot(t, root).Connections[0].Limits.QueryTimeoutMS != 300000 {
		t.Fatal("five-minute timeout was not saved")
	}
	// Removed scope commands and flags must fail before touching state or keys.
	before = files(t, root)
	calls = keys.calls
	command(t, root, keys, "", 2, "scope", "renamed")
	for _, args := range [][]string{{"--all"}, {"--none"}, {"--schema", "public"}, {"--exclude-schema", "private"}, {"--scope-json", `{"mode":"blacklist"}`}} {
		command(t, root, keys, "", 2, append([]string{"edit", "renamed", "--yes"}, args...)...)
		command(t, root, keys, "", 2, append(append([]string{}, basicAdd...), args...)...)
	}
	if keys.calls != calls || !reflect.DeepEqual(before, files(t, root)) {
		t.Fatal("removed scope options changed state or accessed keys")
	}
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
	command(t, manual, keys, "", 0, "rm", "manual", "--yes")
	if len(snapshot(t, manual).Connections) != 0 {
		t.Fatal("partial removal not reported")
	}
	keys.denied = false
	command(t, manual, keys, "", 0, append(basicAdd, "--passwordless")...)
	command(t, manual, keys, "", 0, "remove", "analytics", "--yes")
	if len(snapshot(t, manual).Connections) != 0 {
		t.Fatal("remove failed")
	}
	// Reject malformed established bytes, leaving them available for manual repair.
	if err := os.WriteFile(filepath.Join(manual, "data-mate.db"), []byte(`{"version":1,"password":"synthetic-bad-secret"}`), 0600); err != nil {
		t.Fatal(err)
	}
	before = files(t, manual)
	out, diag = command(t, manual, keys, "", 2, "list", "--json")
	if strings.Contains(out+diag, "synthetic-bad-secret") || !reflect.DeepEqual(before, files(t, manual)) {
		t.Fatal("malformed profile changed or leaked")
	}
}

// connectionTransfer extends the CRUD fixture once it carries TLS, SSH and
// custom limits. Export must mark absent secrets without exporting values, and
// import must reproduce settings and secrets under each conflict policy. The
// extra connections are removed so the remaining CRUD steps see one profile.
func connectionTransfer(t *testing.T, root string, keys *testKeys) {
	t.Helper()
	command(t, root, keys, "", 0, "add", "--alias", "nopass", "--host", "localhost", "--database", "app", "--username", "reader", "--passwordless", "--yes")
	command(t, root, keys, `{"ssh_password":"sshonly-secret"}`, 0, "add", "--alias", "sshonly", "--host", "db.internal", "--database", "app", "--username", "reader", "--passwordless", "--ssh-host", "jump.local", "--ssh-user", "jump", "--credentials-stdin", "--yes")
	dir := t.TempDir()
	exported := filepath.Join(dir, "connections.csv")
	before := files(t, root)
	if out, _ := command(t, root, keys, "", 0, "export", exported); out != "Exported 3 connections.\n" || !reflect.DeepEqual(before, files(t, root)) {
		t.Fatal("export output or state", out)
	}
	raw, err := os.ReadFile(exported)
	if err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(exported); err != nil || st.Mode().Perm() != 0600 {
		t.Fatal("export is not owner-only", err)
	}
	leaks := []string{"replacement-secret", "synthetic-ssh-secret", "sshonly-secret"}
	for _, p := range snapshot(t, root).Connections {
		leaks = append(leaks, p.ID)
		if p.CredentialRef != "" {
			leaks = append(leaks, p.CredentialRef)
		}
	}
	for _, leak := range leaks {
		if strings.Contains(string(raw), leak) {
			t.Fatal("export leaked a secret or identity")
		}
	}
	// Empty means a saved secret was withheld; <none> means the secret is absent.
	cells := csvCells(t, exported)
	for alias, want := range map[string]map[string]string{
		"renamed": {"password": "", "ssh_password": "", "ssh_auth": "password", "tls": "true", "tls_ca": "/tmp/synthetic-ca.pem", "query_timeout": "750ms", "max_rows": "12", "max_result_bytes": "4096"},
		"nopass":  {"password": noSecret, "ssh_password": "", "ssh_host": ""},
		"sshonly": {"password": noSecret, "ssh_password": "", "ssh_host": "jump.local"},
	} {
		for column, value := range want {
			if cells[alias][column] != value {
				t.Fatalf("export %s.%s = %q, want %q", alias, column, cells[alias][column], value)
			}
		}
	}
	command(t, root, keys, "", 2, "export", exported)
	if after, _ := os.ReadFile(exported); !bytes.Equal(raw, after) {
		t.Fatal("unconfirmed export replaced the file")
	}
	command(t, root, keys, "", 0, "export", exported, "--yes")

	// Filling only the withheld cells reproduces settings and secrets elsewhere.
	filled := filepath.Join(dir, "filled.csv")
	editCSV(t, exported, filled, map[string]map[string]string{"renamed": {"password": "replacement-secret", "ssh_password": "synthetic-ssh-secret"}, "sshonly": {"ssh_password": "sshonly-secret"}})
	fresh, freshKeys := privateRoot(t), &testKeys{}
	if out, _ := command(t, fresh, freshKeys, "", 0, "import", filled, "--yes"); out != "nopass  added\nrenamed  added\nsshonly  added\n" {
		t.Fatal("import output", out)
	}
	settings := func(root string) []config.Profile {
		p := snapshot(t, root).Connections
		for i := range p {
			p[i].ID, p[i].CredentialRef = "", ""
		}
		slices.SortFunc(p, func(a, b config.Profile) int { return strings.Compare(a.Alias, b.Alias) })
		return p
	}
	if !reflect.DeepEqual(settings(root), settings(fresh)) {
		t.Fatal("round trip changed settings", settings(fresh))
	}
	imported := map[string]config.Profile{}
	for _, p := range snapshot(t, fresh).Connections {
		imported[p.Alias] = p
	}
	if s := credential(t, fresh, freshKeys, imported["renamed"].ID); s.Password != "replacement-secret" || s.SSHPassword != "synthetic-ssh-secret" {
		t.Fatal("round trip lost secrets")
	}
	if s := credential(t, fresh, freshKeys, imported["sshonly"].ID); s.Password != "" || s.SSHPassword != "sshonly-secret" || imported["nopass"].CredentialRef != "" {
		t.Fatal("round trip did not preserve absent secrets")
	}

	// Existing aliases need an explicit policy; stop and a missing choice save nothing.
	before = files(t, fresh)
	command(t, fresh, freshKeys, "", 2, "import", filled, "--yes")
	command(t, fresh, freshKeys, "", 2, "import", filled, "--on-conflict", "stop", "--yes")
	if !reflect.DeepEqual(before, files(t, fresh)) {
		t.Fatal("refused conflict changed state")
	}
	if out, _ := command(t, fresh, freshKeys, "", 0, "import", filled, "--on-conflict", "skip", "--yes"); out != "nopass  skipped\nrenamed  skipped\nsshonly  skipped\n" {
		t.Fatal("skip output", out)
	}
	// Updating from the unfilled export keeps withheld secrets; <none> reapplies absence.
	if out, _ := command(t, fresh, freshKeys, "", 0, "import", exported, "--on-conflict", "update", "--yes"); out != "nopass  updated\nrenamed  unchanged\nsshonly  updated\n" {
		t.Fatal("update output", out)
	}
	if s := credential(t, fresh, freshKeys, imported["sshonly"].ID); s.Password != "" || s.SSHPassword != "sshonly-secret" {
		t.Fatal("update lost a withheld secret")
	}
	changed := filepath.Join(dir, "changed.csv")
	editCSV(t, exported, changed, map[string]map[string]string{"renamed": {"max_rows": "99"}})
	command(t, fresh, freshKeys, "", 0, "import", changed, "--on-conflict", "update", "--yes")
	for _, p := range snapshot(t, fresh).Connections {
		if p.Alias == "renamed" && (p.ID != imported["renamed"].ID || p.CredentialRef != imported["renamed"].CredentialRef || p.Limits.MaxRows != 99) {
			t.Fatal("update changed identity or skipped settings")
		}
	}
	if s := credential(t, fresh, freshKeys, imported["renamed"].ID); s.Password != "replacement-secret" || s.SSHPassword != "synthetic-ssh-secret" {
		t.Fatal("update lost secrets")
	}

	// A row rejected by live validation does not stop later rows.
	partial := filepath.Join(dir, "partial.csv")
	if err = os.WriteFile(partial, []byte("alias,host,database,username,password\nbad,localhost,app,reader,<none>\ngood,localhost,app,reader,<none>\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := commandWithDatabase(Build{}, freshKeys, func() (cliDatabase, error) { return &fixtureDatabase{}, nil })
	cmd.SetArgs([]string{"--root", fresh, "db", "import", partial, "--yes"})
	cmd.SetIn(strings.NewReader(""))
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err = cmd.ExecuteContext(t.Context()); ExitCode(err) != ExitFailure || out.String() != "bad  FAIL  CONNECT_FAILED: authentication failed\ngood  added\n" {
		t.Fatal("partial import", err, out.String())
	}
	aliases := []string{}
	for _, p := range snapshot(t, fresh).Connections {
		aliases = append(aliases, p.Alias)
	}
	if slices.Contains(aliases, "bad") || !slices.Contains(aliases, "good") {
		t.Fatal("partial import saved the wrong rows", aliases)
	}
	command(t, root, keys, "", 0, "rm", "nopass", "--yes")
	command(t, root, keys, "", 0, "rm", "sshonly", "--yes")
}

// csvCells indexes an exported CSV by alias, then column.
func csvCells(t *testing.T, path string) map[string]map[string]string {
	t.Helper()
	records := readCSV(t, path)
	cells := map[string]map[string]string{}
	for _, record := range records[1:] {
		row := map[string]string{}
		for i, column := range records[0] {
			row[column] = record[i]
		}
		cells[row["alias"]] = row
	}
	return cells
}

// editCSV copies src to dst, replacing cells selected by alias and column.
func editCSV(t *testing.T, src, dst string, edits map[string]map[string]string) {
	t.Helper()
	records := readCSV(t, src)
	for _, record := range records[1:] {
		for i, column := range records[0] {
			if value, ok := edits[record[0]][column]; ok {
				record[i] = value
			}
		}
	}
	var b bytes.Buffer
	w := csv.NewWriter(&b)
	if err := w.WriteAll(records); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
}
func readCSV(t *testing.T, path string) [][]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	records, err := csv.NewReader(f).ReadAll()
	if err != nil || len(records) == 0 || records[0][0] != "alias" {
		t.Fatal("unreadable CSV", err)
	}
	return records
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
		{"transport conflict", "", []string{"--passwordless", "--ssh-host", "jump", "--ssh-user", "u", "--proxy", "socks5://host:1080"}},
		{"CA without TLS", "", []string{"--passwordless", "--tls-ca", "/tmp/ca.pem"}},
		{"stdin confirmation", "secret-sentinel", []string{"--password-stdin", "--yes=false"}},
		{"missing confirmation", "", []string{"--passwordless", "--yes=false"}},
		{"zero timeout", "", []string{"--passwordless", "--query-timeout", "0ms"}},
		{"timeout above maximum", "", []string{"--passwordless", "--query-timeout", "300001ms"}},
		{"fractional millisecond timeout", "", []string{"--passwordless", "--query-timeout", "1.5ms"}},
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
	// CSV import shares the boundary: a malformed file or missing scripted input
	// fails before key access or state changes and never echoes cell values.
	header := "alias,host,database,username,password"
	tooMany := []string{header}
	for i := range 129 {
		tooMany = append(tooMany, fmt.Sprintf("a%03d,localhost,app,reader,secret-sentinel", i))
	}
	for _, tc := range []struct {
		name, csv string
		extra     []string
	}{
		{"unknown column", "alias,host,database,username,secret-sentinel\na,localhost,app,reader,x\n", nil},
		{"duplicate column", "alias,host,database,username,host\na,localhost,app,reader,secret-sentinel\n", nil},
		{"missing column", "alias,host,database\na,localhost,secret-sentinel\n", nil},
		{"empty alias", header + "\n,localhost,app,reader,secret-sentinel\n", nil},
		{"duplicate alias", header + "\na,localhost,app,reader,secret-sentinel\na,localhost,app,reader,secret-sentinel\n", nil},
		{"malformed quote", header + "\na,\"localhost,app,reader,secret-sentinel\n", nil},
		{"invalid utf8", header + "\na,localhost,\xff,reader,secret-sentinel\n", nil},
		{"hex port", header + ",port\na,localhost,app,reader,secret-sentinel,0x10\n", nil},
		{"proxy secret without proxy", "alias,host,database,username,password,proxy_password\na,localhost,app,reader,<none>,secret-sentinel\n", nil},
		{"credential URL host", header + "\na,postgres://reader:secret-sentinel@host/db,app,reader,<none>\n", nil},
		{"missing password nonTTY", "alias,host,database,username\na,localhost,app,reader\n", nil},
		{"missing key nonTTY", header + ",ssh_host,ssh_user,ssh_auth\na,localhost,app,reader,secret-sentinel,jump,u,key\n", nil},
		{"marker key file", header + ",ssh_host,ssh_user,ssh_key_file\na,localhost,app,reader,secret-sentinel,jump,u,<none>\n", nil},
		{"relative key file", header + ",ssh_host,ssh_user,ssh_key_file\na,localhost,app,reader,secret-sentinel,jump,u,key.pem\n", nil},
		{"too many rows", strings.Join(tooMany, "\n") + "\n", nil},
		{"noninteractive host enrollment", header + "\na,localhost,app,reader,secret-sentinel\n", []string{"--ssh-enroll"}},
		{"unknown conflict policy", header + "\na,localhost,app,reader,secret-sentinel\n", []string{"--on-conflict", "merge"}},
		{"missing confirmation", header + "\na,localhost,app,reader,secret-sentinel\n", []string{"--yes=false"}},
	} {
		root := privateRoot(t)
		keys := &testKeys{}
		path := filepath.Join(t.TempDir(), "import.csv")
		if err := os.WriteFile(path, []byte(tc.csv), 0600); err != nil {
			t.Fatal(err)
		}
		before := files(t, root)
		out, diag := command(t, root, keys, "", 2, append([]string{"import", path, "--yes"}, tc.extra...)...)
		if keys.calls != 0 || !reflect.DeepEqual(before, files(t, root)) || strings.Contains(out+diag, "secret-sentinel") {
			t.Fatalf("%s changed state or leaked input: %s", tc.name, diag)
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
	cmd.SetArgs([]string{"--root", root, "db", "edit", "analytics", "--max-rows", "12", "--yes"})
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
	// Another writer between import rows stops the import; later rows are not
	// built on profiles the confirmation never reviewed.
	rows := filepath.Join(t.TempDir(), "rows.csv")
	if err := os.WriteFile(rows, []byte("alias,host,database,username,password\nfirst,localhost,app,reader,<none>\nsecond,localhost,app,reader,<none>\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cmd = newCommand(Build{}, keys)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--root", root, "db", "import", rows, "--yes"})
	interrupted := false
	cmd.SetOut(writerFunc(func(b []byte) (int, error) {
		if !interrupted {
			interrupted = true
			p := snapshot(t, root)
			for i := range p.Connections {
				if p.Connections[i].Alias == "analytics" {
					p.Connections[i].Connection.Database = "outside"
				}
			}
			saveProfiles(t, root, p)
		}
		return len(b), nil
	}))
	if err := cmd.ExecuteContext(t.Context()); ExitCode(err) != ExitFailure {
		t.Fatal("import continued after a concurrent change", err)
	}
	saved := map[string]string{}
	for _, p := range snapshot(t, root).Connections {
		saved[p.Alias] = p.Connection.Database
	}
	if _, ok := saved["second"]; ok || saved["first"] != "app" || saved["analytics"] != "outside" {
		t.Fatal("concurrent change was overwritten or ignored", saved)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }
