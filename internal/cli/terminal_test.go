package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"reflect"
	"testing"
	"time"
)

// Real PTYs are necessary for hidden input, raw-mode restoration and cancellation;
// the existing CLI tests use buffers and cannot exercise terminal behavior.
func TestConnectionTerminal(t *testing.T) {
	if mode := os.Getenv("DATA_MATE_P2_PTY_HELPER"); mode != "" {
		root := os.Getenv("DATA_MATE_P2_PTY_ROOT")
		keys := &testKeys{}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		run := func(args ...string) int {
			cmd := newCommand(Build{}, keys)
			cmd.SetArgs(append([]string{"--root", root, "db"}, args...))
			cmd.SetIn(os.Stdin)
			cmd.SetOut(os.Stdout)
			cmd.SetErr(os.Stderr)
			err := cmd.ExecuteContext(ctx)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
			return ExitCode(err)
		}
		code := run("add")
		if mode != "happy" {
			os.Exit(code)
		}
		if code != 0 {
			os.Exit(code)
		}
		p := snapshot(t, root)
		id := p.Connections[0].ID
		if credential(t, root, keys, id).Password != "pty-hidden-secret" {
			t.Fatal("terminal secret changed")
		}
		before, calls := files(t, root), keys.calls
		if code = run("edit", "analytics", "--alias", "cancelled"); code != ExitCancelled {
			t.Fatal("negative edit confirmation not cancelled")
		}
		if !reflect.DeepEqual(before, files(t, root)) || keys.calls != calls {
			t.Fatal("cancelled edit changed files or accessed keys")
		}
		if code = run("edit"); code != 0 {
			os.Exit(code)
		}
		p = snapshot(t, root)
		if p.Connections[0].ID != id || p.Connections[0].Alias != "renamed" || credential(t, root, keys, id).Password != "pty-hidden-secret" {
			t.Fatal("terminal edit did not preserve ID/password")
		}
		if code = run("rm"); code != 0 {
			os.Exit(code)
		}
		if len(snapshot(t, root).Connections) != 0 {
			t.Fatal("terminal removal failed")
		}
		os.Exit(run("ls", "--json"))
	}
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/python3", "testdata/terminal.py", os.Args[0], t.TempDir())
	cmd.WaitDelay = time.Second
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("PTY regression: %v\n%s", err, b)
	}
}
