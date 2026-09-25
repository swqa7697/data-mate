package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/swqa7697/data-mate/internal/config"
)

func writeDocument(t *testing.T, a adapter, doc map[string]any) {
	t.Helper()
	var raw []byte
	if a.name == "codex" {
		var b bytes.Buffer
		if err := toml.NewEncoder(&b).Encode(doc); err != nil {
			t.Fatal(err)
		}
		raw = b.Bytes()
	} else {
		raw, _ = json.Marshal(doc)
	}
	if err := os.WriteFile(a.path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}
func agentFixture(t *testing.T) (*Manager, *config.LifecycleLease, *int) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "data-mate")
	if err := os.Chmod(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := config.ResolveRoot(path, "")
	if err != nil {
		t.Fatal(err)
	}
	store, err := config.Open(t.Context(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	l, err := store.Lifecycle(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Release)
	if err = os.Mkdir(filepath.Dir(config.DevelopmentExecutable(root)), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(filepath.Dir(config.DevelopmentExecutable(root)), ".data-mate.fixture"), []byte("fixture"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = l.InstallBinary(t.Context(), ".data-mate.fixture"); err != nil {
		t.Fatal(err)
	}
	m := &Manager{root: root}
	calls := new(int)
	for _, name := range []string{"codex", "claude"} {
		dir, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		a := adapter{name: name, executable: "/fixture/" + name, path: filepath.Join(dir, "config")}
		a.run = func(_ context.Context, exe string, args, env []string) error {
			*calls++
			if exe != a.executable {
				t.Fatal("wrong executable")
			}
			before, err := a.inspect(Name(root))
			if err != nil {
				return err
			}
			key := "mcp_servers"
			if name == "claude" {
				key = "mcpServers"
			}
			servers, _ := before.other[key].(map[string]any)
			if servers == nil {
				servers = map[string]any{}
			}
			if args[1] == "add" {
				prefix := []string{"mcp", "add", Name(root), "--"}
				if name == "claude" {
					prefix = []string{"mcp", "add", "--transport", "stdio", "--scope", "user", Name(root), "--"}
				}
				if len(args) <= len(prefix) || !reflect.DeepEqual(args[:len(prefix)], prefix) {
					t.Fatal("wrong add scope/arguments", args)
				}
				command := args[len(prefix):]
				entry := map[string]any{"command": command[0], "args": command[1:]}
				if name == "claude" {
					entry["type"] = "stdio"
					entry["env"] = map[string]any{}
				}
				servers[Name(root)] = entry
			} else {
				want := []string{"mcp", "remove", Name(root)}
				if name == "claude" {
					want = []string{"mcp", "remove", "--scope", "user", Name(root)}
				}
				if !reflect.DeepEqual(args, want) {
					t.Fatal("wrong remove scope", args)
				}
			}
			before.other[key] = servers
			writeDocument(t, a, before.other)
			return nil
		}
		fixture := "codex-0.156.1.toml"
		if name == "claude" {
			fixture = "claude-2.1.281.json"
		}
		raw, err := os.ReadFile(filepath.Join("testdata", fixture))
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(a.path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		snapshot, err := a.inspect(Name(root))
		if err != nil {
			t.Fatal(err)
		}
		snapshot.other["fixture_foreign"] = map[string]any{"trust": "deny"}
		writeDocument(t, a, snapshot.other)
		m.adapters = append(m.adapters, a)
	}
	return m, l, calls
}

// Ladder 3: no existing scenario owns external agent configuration and durable
// registration intent. One lifecycle corpus covers distinct mutation failures.
func TestRegistrationOwnership(t *testing.T) {
	m, l, calls := agentFixture(t)
	inspect := func(want string) {
		t.Helper()
		states, err := m.Inspect(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range states {
			if s.State != want {
				t.Fatal(states)
			}
		}
	}
	inspect("pending")
	if *calls != 0 {
		t.Fatal("passive inspection invoked agents")
	}
	// An add succeeds externally but the caller loses completion. Its peer proceeds.
	real := m.adapters[0].run
	m.adapters[0].run = func(c context.Context, e string, a, v []string) error {
		if err := real(c, e, a, v); err != nil {
			return err
		}
		return ErrInspection
	}
	states, err := m.Ensure(t.Context(), l)
	if !errors.Is(err, ErrPartial) || states[0].State != "failed" || states[1].State != "ready" {
		t.Fatal(states, err)
	}
	m.adapters[0].run = real
	if _, err = m.Ensure(t.Context(), l); err != nil {
		t.Fatal(err)
	}
	if *calls != 2 {
		t.Fatal("retry duplicated external add", *calls)
	}
	inspect("ready")
	o, err := m.load(l)
	if err != nil || len(o.Entries) != 2 || o.Entries[0].Phase != "owned" {
		t.Fatal(o, err)
	}
	// Disabled policy survives repeated starts and removal refuses the edited hash.
	a := m.adapters[0]
	s, _ := a.inspect(Name(m.root))
	s.entry["enabled"] = false
	s.other["mcp_servers"] = map[string]any{Name(m.root): s.entry}
	writeDocument(t, a, s.other)
	states, err = m.Ensure(t.Context(), l)
	if err == nil || states[0].State != "disabled" || *calls != 2 {
		t.Fatal(states, err, *calls)
	}
	if err = m.RemoveOwned(t.Context(), l); !errors.Is(err, ErrConflict) {
		t.Fatal("edited removal", err)
	}
	delete(s.entry, "enabled")
	writeDocument(t, a, s.other)
	// A foreign command with our name is not overwritten; peer remains available.
	s.entry["command"] = "/foreign"
	writeDocument(t, a, s.other)
	states, err = m.Ensure(t.Context(), l)
	if err == nil || states[0].State != "conflict" || states[1].State != "ready" || *calls != 2 {
		t.Fatal(states, err)
	}
	s.other["mcp_servers"] = map[string]any{Name(m.root): m.desired(a, l.Identity().Executable)}
	writeDocument(t, a, s.other)
	// Config relocation must not strand authority or delete a different user's entry.
	m.adapters[0].path = filepath.Join(t.TempDir(), "different")
	if _, err = m.Ensure(t.Context(), l); err == nil {
		t.Fatal("relocated config accepted")
	}
	if err = m.RemoveOwned(t.Context(), l); err != nil {
		t.Fatal("recorded-location cleanup after relocation", err)
	}
	m.adapters[0] = a
	inspect("pending")
	if *calls != 4 {
		t.Fatal("remove count", *calls)
	}
	// Matching manual registration is usable without acquiring deletion ownership.
	writeDocument(t, a, map[string]any{"mcp_servers": map[string]any{Name(m.root): m.desired(a, l.Identity().Executable)}})
	if _, err = m.Ensure(t.Context(), l); err != nil {
		t.Fatal(err)
	}
	if err = m.RemoveOwned(t.Context(), l); err != nil {
		t.Fatal(err)
	}
	current, _ := a.inspect(Name(m.root))
	if current.entry == nil {
		t.Fatal("deleted manual entry")
	}
	// TOML duplicate keys and values that cannot be fingerprinted fail before writes.
	original, err := os.ReadFile(a.path)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"x = 1\nx = 2\n", "x = nan\n"} {
		if err = os.WriteFile(a.path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err = a.inspect(Name(m.root)); err == nil {
			t.Fatal("unsafe TOML accepted")
		}
	}
	if err = os.WriteFile(a.path, original, 0600); err != nil {
		t.Fatal(err)
	}
	// Duplicate JSON, oversized data, symlink and unknown schema fail passively.
	a = m.adapters[1]
	for _, raw := range []string{"", `{"mcpServers":{},"mcpServers":{}}`, `{"mcpServers":[]}`, `null`, string(bytes.Repeat([]byte("x"), maxConfigBytes+1))} {
		if err = os.WriteFile(a.path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err = a.inspect(Name(m.root)); err == nil {
			t.Fatal("unsafe config accepted")
		}
	}
	if err = os.Remove(a.path); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(m.adapters[0].path, a.path); err != nil {
		t.Fatal(err)
	}
	if _, err = a.inspect(Name(m.root)); err == nil {
		t.Fatal("symlink accepted")
	}
	// Absent clients are skipped even if their unused config location is invalid.
	m.adapters[1].executable = ""
	states, err = m.Ensure(t.Context(), l)
	if err != nil || states[1].State != "unavailable" {
		t.Fatal(states, err)
	}
	// Damaged local ownership cannot authorize repair or produce ready status.
	if err = l.Replace(recordPath, []byte(`{"version":99}`)); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Inspect(t.Context()); !errors.Is(err, ErrInspection) {
		t.Fatal("corrupt ownership inspected as ready", err)
	}
	if _, err = m.Ensure(t.Context(), l); !errors.Is(err, ErrPartial) {
		t.Fatal("corrupt ownership repaired", err)
	}
	// Detection ignores current-directory PATH entries and honors explicit locations.
	t.Setenv("PATH", ".:relative")
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	detected := New(m.root)
	for _, a := range detected.adapters {
		if a.executable != "" || a.err != nil {
			t.Fatal("unsafe detection", a.name)
		}
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "relative")
	if _, err = configPath("claude"); err == nil {
		t.Fatal("ambiguous config override")
	}
	// Cancellation and excessive output cannot leave a subprocess blocking a start.
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if err = runCLI(ctx, "/bin/sh", []string{"-c", "sleep 30"}, []string{"PATH=/bin:/usr/bin"}); err == nil {
		t.Fatal("timeout accepted")
	}
	if err = runCLI(t.Context(), "/bin/sh", []string{"-c", "yes fixture"}, []string{"PATH=/bin:/usr/bin"}); err == nil {
		t.Fatal("unbounded output accepted")
	}
}
