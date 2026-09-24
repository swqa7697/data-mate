package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/swqa7697/data-mate/internal/agent"
	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/vault"
)

// No existing test owns the installed CLI + launchd lifecycle. This opt-in gate
// uses two isolated installations and synthetic credentials only; it never reads
// real agent configuration or stops any job not created by this fixture.
func TestNativeServiceLifecycle(t *testing.T) {
	if os.Getenv("DATA_MATE_NATIVE_TEST") != "1" {
		t.Skip("requires DATA_MATE_NATIVE_TEST=1, macOS GUI launchd and an unlocked user Keychain")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()
	dir, err := os.MkdirTemp("/tmp", "data-mate-p8-native-")
	if err != nil {
		t.Fatal(err)
	}
	// Installed CLI registration uses isolated native-client configuration.
	for _, variable := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR"} {
		configDir := filepath.Join(dir, variable)
		if err = os.Mkdir(configDir, 0700); err != nil {
			t.Fatal(err)
		}
		t.Setenv(variable, configDir)
	}
	// Keep retry material if external cleanup fails.
	clean := true
	t.Cleanup(func() {
		if clean {
			if err := os.RemoveAll(dir); err != nil {
				t.Error(err)
			}
		} else {
			t.Log("native cleanup retry material retained:", dir)
		}
	})
	run := func(bin string, args ...string) []byte {
		t.Helper()
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Dir = "/"
		out, e := cmd.CombinedOutput()
		if e != nil {
			t.Fatalf("native %s: %v %s", filepath.Base(bin), e, out)
		}
		return out
	}
	build := func(revision, path string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, "go", "build", "-mod=readonly", "-trimpath", "-ldflags=-X main.version=native -X main.revision="+revision, "-o", path, "../vault/testdata/native")
		if out, e := cmd.CombinedOutput(); e != nil {
			t.Fatalf("native build: %v %s", e, out)
		}
		if e := os.Chmod(path, 0700); e != nil {
			t.Fatal(e)
		}
	}
	original := filepath.Join(dir, "original")
	build("p8-native", original)
	data, err := os.ReadFile(original)
	if err != nil {
		t.Fatal(err)
	}
	var controllers []*Controller
	for _, name := range []string{"first checkout " + strings.Repeat("long", 35), "second checkout"} {
		path := filepath.Join(dir, name, ".dev")
		if err = os.MkdirAll(filepath.Join(path, "bin"), 0700); err != nil {
			t.Fatal(err)
		}
		root, e := config.ResolveRoot(path, "")
		if e != nil {
			t.Fatal(e)
		}
		tmp := filepath.Join(root.Path, "bin/.data-mate.native")
		if e = os.WriteFile(tmp, data, 0700); e != nil {
			t.Fatal(e)
		}
		run(tmp, "__install", "--root", root.Path)
		binary := filepath.Join(root.Path, "bin/data-mate")
		hash, e := binaryHash(binary)
		if e != nil {
			t.Fatal(e)
		}
		store, e := config.OpenExisting(ctx, root)
		if e != nil {
			t.Fatal(e)
		}
		lease, e := store.ReadLease(ctx)
		if e != nil {
			t.Fatal(e)
		}
		account := lease.Identity().KeyAccount
		lease.Release()
		store.Close()
		c := New(root, Build{"native", "p8-native", hash})
		c.Agents = agent.New(root)
		controllers = append(controllers, c)
		t.Cleanup(func() {
			cleanup, done := context.WithTimeout(context.Background(), 15*time.Second)
			defer done()
			if _, e := c.Stop(cleanup); e != nil {
				clean = false
				t.Error("native service cleanup", e)
			}
			store, e := config.OpenLifecycle(cleanup, root)
			if e == nil {
				lease, le := store.Lifecycle(cleanup)
				if le == nil {
					e = c.Agents.RemoveOwned(cleanup, lease)
					lease.Release()
				} else {
					e = le
				}
				store.Close()
			}
			if e != nil && !errors.Is(e, os.ErrNotExist) {
				clean = false
				t.Error("native registration cleanup", e)
			}
			if e := (vault.Keychain{}).Delete(cleanup, account); e != nil {
				clean = false
				t.Error("native exact key cleanup", e)
			}
		})
		out := run(binary, "mcp", "status", "--json")
		var result Status
		if e = json.Unmarshal(out, &result); e != nil || result.State != "stopped" {
			t.Fatal("initial passive status", result, e)
		}
		run(binary, "mcp", "start", "--json")
		command := exec.CommandContext(ctx, binary, "mcp", "bridge")
		command.Dir = "/"
		var diagnostics bytes.Buffer
		command.Stderr = &diagnostics
		client := sdk.NewClient(&sdk.Implementation{Name: "native-fixture", Version: "1"}, nil)
		session, err := client.Connect(ctx, &sdk.CommandTransport{Command: command}, nil)
		if err != nil {
			t.Fatal("installed bridge initialization", err)
		}
		tools, err := session.ListTools(ctx, nil)
		if err != nil || len(tools.Tools) != 4 {
			t.Fatal("installed bridge tools", err)
		}
		toolResult, err := session.CallTool(ctx, &sdk.CallToolParams{Name: "list_connections", Arguments: map[string]any{}})
		if err != nil || toolResult.IsError {
			t.Fatal("installed bridge call", err)
		}
		if err = session.Close(); err != nil {
			t.Fatal("installed bridge EOF", err)
		}
		if diagnostics.Len() != 0 {
			t.Fatal("unexpected installed bridge diagnostics")
		}

		if _, e = os.Lstat(filepath.Join(root.Path, "state/vault.json")); !os.IsNotExist(e) {
			t.Fatal("empty startup created vault", e)
		}
		if _, e = nativeStart(t, ctx, c); e != nil {
			t.Fatal("reuse", e)
		}
	}
	c := controllers[0]
	binary := filepath.Join(c.Root.Path, "bin/data-mate")
	var wg sync.WaitGroup
	starts := make(chan error, 4)
	for range 4 {
		wg.Go(func() { _, e := nativeStart(t, ctx, c); starts <- e })
	}
	wg.Wait()
	close(starts)
	for e := range starts {
		if e != nil {
			t.Fatal("native concurrent start", e)
		}
	}
	// A same-name conflict returns structured partial readiness without stopping
	// the service, altering the other adapter, or overwriting either checkout.
	codexConfig := filepath.Join(os.Getenv("CODEX_HOME"), "config.toml")
	originalConfig, e := os.ReadFile(codexConfig)
	if e != nil {
		t.Fatal(e)
	}
	changedConfig := bytes.Replace(originalConfig, []byte(binary), []byte("/foreign/data-mate"), 1)
	if bytes.Equal(originalConfig, changedConfig) {
		t.Fatal("missing native registration")
	}
	if e = os.WriteFile(codexConfig, changedConfig, 0600); e != nil {
		t.Fatal(e)
	}
	partialCmd := exec.CommandContext(ctx, binary, "mcp", "start", "--json")
	partialCmd.Dir = "/"
	partialOutput, partialErr := partialCmd.Output()
	var partial Status
	if partialErr == nil || json.Unmarshal(partialOutput, &partial) != nil || partial.State != "running" || len(partial.Agents) != 2 || partial.Agents[0].State != "conflict" || partial.Agents[1].State != "ready" {
		t.Fatal("native partial readiness", partial, partialErr)
	}
	preservedConfig, e := os.ReadFile(codexConfig)
	if e != nil || !bytes.Equal(preservedConfig, changedConfig) {
		t.Fatal("native registration conflict overwrote config")
	}
	if e = os.WriteFile(codexConfig, originalConfig, 0600); e != nil {
		t.Fatal(e)
	}
	for _, controller := range controllers {
		result, e := controller.Inspect(ctx)
		if e != nil || result.State != "running" {
			t.Fatal("native independent registration status", e)
		}
		for _, a := range result.Agents {
			if a.State != "ready" {
				t.Fatal("native adapter unavailable", a)
			}
		}
	}
	t.Log("native two-checkout registrations, partial readiness, conflict preservation and passive status passed")
	// A listening endpoint observes zero accepts during startup/status/reload.
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()
	contacted := make(chan struct{}, 1)
	go func() {
		conn, e := listener.Accept()
		if e == nil {
			conn.Close()
			contacted <- struct{}{}
		}
	}()
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	run(binary, "db", "add", "--alias", "fixture", "--host", "127.0.0.1", "--port", port, "--database", "synthetic", "--username", "reader", "--passwordless", "--yes")
	run(binary, "mcp", "status", "--json")
	if _, e = c.Stop(ctx); e != nil {
		t.Fatal(e)
	}
	// Save with the same signed executable that launchd runs. No secret argument.
	save := exec.CommandContext(ctx, binary, "db", "edit", "fixture", "--password-stdin", "--yes")
	save.Stdin = strings.NewReader("synthetic-p8-password\n")
	save.Dir = "/"
	if out, e := save.CombinedOutput(); e != nil {
		t.Fatalf("native synthetic credential save: %v %s", e, out)
	}
	// Separate CLI processes reuse one loaded keyset in management-only mode.
	accesses, e := os.ReadFile(c.Root.Path + ".key-access")
	if e != nil {
		t.Fatal(e)
	}
	for _, alias := range []string{"another", "third"} {
		add := exec.CommandContext(ctx, binary, "db", "add", "--alias", alias, "--host", "127.0.0.1", "--port", port, "--database", "synthetic", "--username", "reader", "--password-stdin", "--yes")
		add.Stdin = strings.NewReader("synthetic-extra-password\n")
		if out, e := add.CombinedOutput(); e != nil {
			t.Fatalf("independent native save: %v %s", e, out)
		}
	}
	again, e := os.ReadFile(c.Root.Path + ".key-access")
	if e != nil || !bytes.Equal(accesses, again) {
		t.Fatal("independent saves accessed OS store", e)
	}
	if state, e := c.Inspect(ctx); e != nil || state.MCPEnabled || state.KeysetState != "ready" {
		t.Fatal("management-only saves", state, e)
	}
	if _, e = nativeStart(t, ctx, c); e != nil {
		t.Fatal("native existing vault readiness", e)
	}

	path := filepath.Join(c.Root.Path, "state/data-mate.db")
	profiles, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(path, []byte(`{"version":99}`), 0600); e != nil {
		t.Fatal(e)
	}
	if result, e := c.Inspect(ctx); !errors.Is(e, ErrState) || result.State != "degraded" {
		t.Fatal("native invalid reload", result, e)
	}
	if e = os.WriteFile(path, profiles, 0600); e != nil {
		t.Fatal(e)
	}
	if result, e := c.Inspect(ctx); e != nil || result.State != "running" {
		t.Fatal("native reload recovery", result, e)
	}
	select {
	case <-contacted:
		t.Fatal("lifecycle contacted database")
	default:
	}
	// Crash does not restart automatically. Only the owned launchd job is signaled.
	j, e := c.launcher.Inspect(ctx, c.Root)
	if e != nil || j.PID <= 0 {
		t.Fatal(j, e)
	}
	if _, e = launch(ctx, "kill", "SIGKILL", target(c.Root)); e != nil {
		t.Fatal(e)
	}
	waitFor(t, func() bool { j, e := c.launcher.Inspect(ctx, c.Root); return e == nil && j.PID == 0 })
	if result, e := c.Inspect(ctx); e != nil || result.State != "stale" {
		t.Fatal("crashed service", result, e)
	}
	if _, e = nativeStart(t, ctx, c); e != nil {
		t.Fatal("explicit crash restart", e)
	}
	// A byte-changing rebuild is published through the actual hidden install entry.
	changed := filepath.Join(c.Root.Path, "bin/.data-mate.changed")
	build("p8-changed", changed)
	out := run(changed, "__install", "--root", c.Root.Path)
	if len(out) == 0 {
		t.Fatal("build omitted restart diagnostic")
	}
	hash, e := binaryHash(binary)
	if e != nil {
		t.Fatal(e)
	}
	newer := New(c.Root, Build{"native", "p8-changed", hash})
	newer.readiness = 2 * time.Second
	if result, e := newer.Inspect(ctx); !errors.Is(e, ErrRestart) || result.State != "stale" {
		t.Fatal("rebuild skew", result, e)
	}
	if e = newer.ProbeSession(ctx); !errors.Is(e, ErrRestart) {
		t.Fatal("bridge rebuild skew", e)
	}
	if _, e = newer.Stop(ctx); e != nil {
		t.Fatal("stop older executable", e)
	}
	// Keychain may grant an already approved identity or deny the changed ad-hoc
	// executable. Either way startup must be explicit and leave no partial job.
	result, changedErr := nativeStart(t, ctx, newer)
	if changedErr != nil && !errors.Is(changedErr, ErrStartup) && !errors.Is(changedErr, vault.ErrDenied) && !errors.Is(changedErr, vault.ErrLocked) {
		t.Fatal("changed native key identity", changedErr)
	}
	t.Logf("changed executable readiness: state=%s error=%v", result.State, changedErr)
	if _, e = newer.Stop(ctx); e != nil {
		t.Fatal(e)
	}
	restore := filepath.Join(c.Root.Path, "bin/.data-mate.restore")
	if e = os.WriteFile(restore, data, 0700); e != nil {
		t.Fatal(e)
	}
	run(restore, "__install", "--root", c.Root.Path)
	if _, e = nativeStart(t, ctx, c); e != nil {
		t.Fatal("original identity restart", e)
	}
	run(binary, "mcp", "stop", "--json")
	stopped := exec.CommandContext(ctx, binary, "mcp", "bridge")
	var stdout, stderr bytes.Buffer
	stopped.Stdout = &stdout
	stopped.Stderr = &stderr
	if e = stopped.Run(); e == nil || stdout.Len() != 0 || stderr.Len() == 0 {
		t.Fatal("stopped bridge exit/streams", e)
	}
	if result, e := controllers[1].Inspect(ctx); e != nil || result.State != "running" {
		t.Fatal("other checkout affected", result, e)
	}
	// Native foreign job under our suffix must survive start/stop conflicts.
	// This job is itself owned by the fixture, so final cleanup is exact.
	foreign := filepath.Join(dir, "foreign.plist")
	body := `<?xml version="1.0"?><plist version="1.0"><dict><key>Label</key><string>` + label(c.Root) + `</string><key>ProgramArguments</key><array><string>/bin/sleep</string><string>30</string></array><key>RunAtLoad</key><true/><key>KeepAlive</key><false/></dict></plist>`
	if e = os.WriteFile(foreign, []byte(body), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = launch(ctx, "bootstrap", domain(), foreign); e != nil {
		t.Fatal(e)
	}
	foreignPresent := true
	t.Cleanup(func() {
		if !foreignPresent {
			return
		}
		cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		if _, e := launch(cleanup, "bootout", target(c.Root)); e != nil {
			clean = false
			t.Error("foreign fixture cleanup", e)
		}
	})
	for _, action := range []func(context.Context) (Status, error){c.Start, c.Stop} {
		if _, e = action(ctx); !errors.Is(e, ErrConflict) {
			t.Fatal("native foreign job altered", e)
		}
	}
	if j, e := c.launcher.Inspect(ctx, c.Root); e != nil || !j.Present || j.Args[0] != "/bin/sleep" {
		t.Fatal("foreign job lost", e)
	}
	// Integrated cleanup uses the same external helper entry point as Make.
	if _, e = launch(ctx, "bootout", target(c.Root)); e != nil {
		t.Fatal(e)
	}
	foreignPresent = false
	run(binary, "mcp", "start", "--json")
	retained := map[string][]byte{}
	for _, path := range []string{"state/data-mate.db", "state/installation.json"} {
		b, err := os.ReadFile(filepath.Join(c.Root.Path, path))
		if err != nil {
			t.Fatal(err)
		}
		retained[path] = b
	}
	sentinel := filepath.Join(c.Root.Path, "unrelated")
	if e = os.WriteFile(sentinel, []byte("keep"), 0600); e != nil {
		t.Fatal(e)
	}
	run(original, "__uninstall", "--root", c.Root.Path)
	if _, e = os.Lstat(binary); !os.IsNotExist(e) {
		t.Fatal("native uninstall binary", e)
	}
	for path, before := range retained {
		after, err := os.ReadFile(filepath.Join(c.Root.Path, path))
		if err != nil || !bytes.Equal(before, after) {
			t.Fatal("native uninstall changed credentials", path, err)
		}
	}
	if states, err := c.Agents.Inspect(ctx); err != nil {
		t.Fatal(err)
	} else {
		for _, state := range states {
			if state.State == "ready" {
				t.Fatal("native uninstall registration remains", state)
			}
		}
	}
	if e = os.Mkdir(filepath.Join(c.Root.Path, "bin"), 0700); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(restore, data, 0700); e != nil {
		t.Fatal(e)
	}
	run(restore, "__install", "--root", c.Root.Path)
	run(binary, "mcp", "start", "--json") // authenticates the retained vault/key in a new process
	run(original, "__uninstall", "--root", c.Root.Path, "--purge")
	if _, e = (vault.Keychain{}).Load(ctx, c.Root.Digest); !errors.Is(e, vault.ErrMissing) {
		t.Fatal("native purge key remains", e)
	}
	if b, err := os.ReadFile(sentinel); err != nil || string(b) != "keep" {
		t.Fatal("native purge unrelated data", err)
	}
	for _, path := range []string{"config", "state", "bin"} {
		if _, err := os.Lstat(filepath.Join(c.Root.Path, path)); !os.IsNotExist(err) {
			t.Fatal("native purge artifact remains", path, err)
		}
	}
	if result, e := controllers[1].Inspect(ctx); e != nil || result.State != "running" {
		t.Fatal("purge affected other checkout", result, e)
	}
	run(original, "__uninstall", "--root", c.Root.Path, "--purge")
	t.Log("native default uninstall/reinstall authenticated retained credentials; integrated purge removed exact key, registrations and files while preserving unrelated and second-root resources")
	t.Log("native two-root, long-path, concurrent/repeated start, zero-dial readiness, existing vault, reload, crash, rebuild, stop and stopped bridge passed")
}

func nativeStart(t *testing.T, ctx context.Context, c *Controller) (Status, error) {
	t.Helper()
	cmd := exec.CommandContext(ctx, filepath.Join(c.Root.Path, "bin/data-mate"), "mcp", "start", "--json")
	var out, diag bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &diag
	err := cmd.Run()
	var s Status
	if out.Len() > 0 && json.Unmarshal(out.Bytes(), &s) != nil {
		t.Fatalf("native start invalid response: %s", &out)
	}
	if err != nil {
		if strings.Contains(diag.String(), "interaction") || strings.Contains(diag.String(), "unlock") {
			return s, vault.ErrLocked
		}
		if strings.Contains(diag.String(), "denied") {
			return s, vault.ErrDenied
		}
		return s, ErrStartup
	}
	return s, nil
}
