// Package agent owns passive inspection and supported CLI registration for agents.
package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"golang.org/x/sys/unix"
)

var (
	ErrInspection = errors.New("agent configuration is unreadable or unsupported")
	ErrPartial    = errors.New("service is available; agent registration needs attention; retry mcp start after resolving agent configuration")
	ErrConflict   = errors.New("agent registration changed; existing configuration was preserved")
)

const maxConfigBytes = 4 << 20

// Status describes user-scope registration, not an agent's effective approval policy.
type Status struct {
	Name  string `json:"name"`
	State string `json:"state"`
}

// Name is stable across rebuilds and independent between checkouts.
func Name(root config.Root) string { return "data-mate-dev-" + root.Digest[:16] }

type adapter struct {
	name, executable, path string
	env                    []string
	err                    error
	run                    func(context.Context, string, []string, []string) error
}

// Manager captures agent locations at CLI invocation. The daemon never constructs one.
type Manager struct {
	root     config.Root
	adapters []adapter
}

// New detects executables without launching them and resolves explicit config locations.
func New(root config.Root) *Manager {
	m := &Manager{root: root}
	var pathDirs []string
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if filepath.IsAbs(dir) {
			pathDirs = append(pathDirs, dir)
		}
	}
	env := os.Environ()
	for i, v := range env {
		if strings.HasPrefix(v, "PATH=") {
			env[i] = "PATH=" + strings.Join(pathDirs, string(os.PathListSeparator))
		}
	}
	for _, name := range []string{"codex", "claude"} {
		a := adapter{name: name, env: env, run: runCLI}
		// Exclude empty/relative PATH components, including the current directory.
		for _, dir := range pathDirs {
			p := filepath.Join(dir, name)
			if st, e := os.Stat(p); e == nil && st.Mode().IsRegular() && st.Mode().Perm()&0111 != 0 {
				a.executable = p
				break
			}
		}
		a.path, a.err = configPath(name)
		m.adapters = append(m.adapters, a)
	}
	return m
}

func configPath(name string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(home) {
		return "", ErrInspection
	}
	var dir, file string
	if name == "codex" {
		dir, file = os.Getenv("CODEX_HOME"), "config.toml"
		if dir == "" {
			dir = filepath.Join(home, ".codex")
		}
	} else {
		dir, file = os.Getenv("CLAUDE_CONFIG_DIR"), ".claude.json"
		if dir == "" {
			dir = home
		}
	}
	if !filepath.IsAbs(dir) || strings.ContainsAny(dir, "\x00\n\r") {
		return "", ErrInspection
	}
	return filepath.Join(filepath.Clean(dir), file), nil
}

// Read with no-follow/nonblocking flags before checking type and size. Never
// return configuration text or upstream CLI output in public diagnostics.
func readConfig(path string) ([]byte, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, nil
	}
	if err != nil {
		return nil, ErrInspection
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Uid != uint32(os.Geteuid()) || st.Mode&0022 != 0 || st.Nlink != 1 || st.Size > maxConfigBytes {
		return nil, ErrInspection
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxConfigBytes+1))
	if err != nil || len(raw) > maxConfigBytes || !utf8.Valid(raw) {
		return nil, ErrInspection
	}
	return raw, nil
}

type snapshot struct {
	entry map[string]any
	other map[string]any
}

func (a adapter) inspect(name string) (snapshot, error) {
	if a.err != nil {
		return snapshot{}, a.err
	}
	raw, err := readConfig(a.path)
	if err != nil {
		return snapshot{}, err
	}
	doc := map[string]any{}
	if raw != nil {
		if a.name == "codex" {
			if _, err = toml.Decode(string(raw), &doc); err != nil {
				return snapshot{}, ErrInspection
			}
		} else {
			raw, err = contracts.JSON(bytes.NewReader(raw), maxConfigBytes)
			if err != nil || json.Unmarshal(raw, &doc) != nil || doc == nil {
				return snapshot{}, ErrInspection
			}
		}
	}
	// Fingerprints require a lossless canonical representation. Reject TOML
	// values such as NaN before a CLI write instead of comparing failed encodings.
	if _, err = json.Marshal(doc); err != nil {
		return snapshot{}, ErrInspection
	}
	key := "mcp_servers"
	if a.name == "claude" {
		key = "mcpServers"
	}
	servers := map[string]any{}
	if v, ok := doc[key]; ok {
		servers, ok = v.(map[string]any)
		if !ok || servers == nil {
			return snapshot{}, ErrInspection
		}
	}
	var entry map[string]any
	if v, ok := servers[name]; ok {
		entry, ok = v.(map[string]any)
		if !ok || entry == nil {
			return snapshot{}, ErrInspection
		}
	}
	delete(servers, name)
	// Empty and missing server maps are equivalent for preservation checks.
	if len(servers) == 0 {
		delete(doc, key)
	} else {
		doc[key] = servers
	}
	return snapshot{entry, doc}, nil
}

func fingerprint(entry map[string]any) string {
	b, _ := json.Marshal(entry)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func (m *Manager) desired(a adapter) map[string]any {
	entry := map[string]any{"command": filepath.Join(m.root.Path, "bin/data-mate"), "args": []string{"mcp", "bridge", "--root", m.root.Path}}
	if a.name == "claude" {
		entry["type"] = "stdio"
		entry["env"] = map[string]any{}
	}
	return entry
}
func (m *Manager) state(a adapter, s snapshot) string {
	if a.executable == "" {
		return "unavailable"
	}
	if s.entry == nil {
		return "pending"
	}
	// Canonical JSON compares TOML/JSON array representations identically.
	expected := m.desired(a)
	disabled := false
	for k, v := range s.entry {
		if a.name == "codex" && k == "enabled" {
			enabled, ok := v.(bool)
			if !ok {
				return "conflict"
			}
			disabled = !enabled
			expected[k] = v
		}
	}
	if fingerprint(expected) != fingerprint(s.entry) {
		return "conflict"
	}
	if disabled {
		return "disabled"
	}
	return "ready"
}

// Inspect performs no subprocess invocation or mutation, even for Claude servers
// whose get/list commands would otherwise health-check a bridge.
func (m *Manager) Inspect(ctx context.Context) ([]Status, error) {
	var owned ownership
	store, stateErr := config.OpenExisting(ctx, m.root)
	if stateErr == nil {
		lease, err := store.ReadLease(ctx)
		if err == nil {
			owned, stateErr = m.readOwnership(lease.Read, lease.Identity())
			lease.Release()
		} else {
			stateErr = err
		}
		store.Close()
	} else if errors.Is(stateErr, os.ErrNotExist) {
		stateErr = nil
	}
	var out []Status
	var result error
	if stateErr != nil {
		result = ErrInspection
	}
	for _, a := range m.adapters {
		state := "unavailable"
		if a.executable != "" {
			s, err := a.inspect(Name(m.root))
			if ctx.Err() != nil {
				err = ctx.Err()
			}
			if stateErr != nil {
				err = stateErr
			}
			if err != nil {
				state = "failed"
				result = ErrInspection
			} else {
				state = m.state(a, s)
				if i := owned.find(a.name); i >= 0 && (owned.Entries[i].Config != a.path || (s.entry != nil && fingerprint(s.entry) != owned.Entries[i].Fingerprint && state != "disabled")) {
					state = "conflict"
				}
			}
		}
		out = append(out, Status{a.name, state})
	}
	if ctx.Err() != nil {
		return out, ctx.Err()
	}
	return out, result
}

type limitedOutput struct {
	n        int
	exceeded bool
}

func (b *limitedOutput) Write(p []byte) (int, error) {
	if len(p) > 64<<10-b.n {
		b.exceeded = true
		return 0, ErrInspection
	}
	b.n += len(p)
	return len(p), nil
}
func runCLI(parent context.Context, exe string, args, env []string) error {
	ctx, cancel := context.WithTimeout(parent, 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Dir = "/"
	cmd.Env = env
	cmd.WaitDelay = time.Second
	// Kill the owned process group on timeout, including children holding pipes.
	cmd.SysProcAttr = &unix.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return unix.Kill(-cmd.Process.Pid, unix.SIGKILL) }
	var output limitedOutput
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil || output.exceeded {
		return ErrInspection
	}
	return nil
}
func (a adapter) addArgs(name string, desired map[string]any) []string {
	args := []string{"mcp", "add"}
	if a.name == "claude" {
		args = append(args, "--transport", "stdio", "--scope", "user")
	}
	args = append(args, name, "--", desired["command"].(string))
	return append(args, desired["args"].([]string)...)
}
func (a adapter) removeArgs(name string) []string {
	args := []string{"mcp", "remove"}
	if a.name == "claude" {
		args = append(args, "--scope", "user")
	}
	return append(args, name)
}

// Existing unrelated fields must survive a CLI write. Claude may initialize
// application metadata; additions are tolerated, changes/removals are not.
func preserved(before, after map[string]any) bool {
	for k, v := range before {
		if !reflect.DeepEqual(v, after[k]) {
			return false
		}
	}
	return true
}
