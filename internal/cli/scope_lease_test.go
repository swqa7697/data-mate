package cli

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/swqa7697/data-mate/internal/config"
	"golang.org/x/sys/unix"
)

// Extends CRUD with an independent database-work owner holding the production
// read lease. A successful scope publication must wait for that owner to finish.
func scopeLeaseAcceptance(t *testing.T) {
	t.Helper()
	root := privateRoot(t)
	keys := &testKeys{}
	command(t, root, keys, "", 0, append(basicAdd, "--passwordless")...)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestConnectionCRUD$")
	child.Env = append(os.Environ(), "DATA_MATE_SCOPE_LEASE="+root)
	child.WaitDelay = time.Second
	stdin, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	child.Stderr = &stderr
	if err = child.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	t.Cleanup(func() {
		stdin.Close()
		if !waited {
			_ = child.Process.Kill()
			_ = child.Wait()
		}
	})
	scan := bufio.NewScanner(stdout)
	if !scan.Scan() || scan.Text() != "ready" {
		t.Fatal("lease child not ready")
	}
	cmd := newCommand(Build{}, keys)
	cmd.SetIn(bytes.NewReader(nil))
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--root", root, "db", "scope", "analytics", "--none", "--yes"})
	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(ctx) }()
	// The writer owns admission only once it is waiting for the child's read
	// lease. Observe the OS lock instead of guessing from elapsed sleeps.
	gate, err := os.OpenFile(filepath.Join(root, "state/state-gate.lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Close()
	for {
		err = unix.Flock(int(gate.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == unix.EWOULDBLOCK {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		_ = unix.Flock(int(gate.Fd()), unix.LOCK_UN)
		select {
		case err := <-done:
			t.Fatalf("scope returned before database cleanup: %v", err)
		case <-ctx.Done():
			t.Fatal("scope never waited for reader")
		default:
			runtime.Gosched()
		}
	}
	select {
	case err := <-done:
		t.Fatalf("scope completed under old lease: %v", err)
	default:
	}
	if snapshot(t, root).Connections[0].Scope.Mode != "all" {
		t.Fatal("scope published before cleanup")
	}
	if _, err = stdin.Write([]byte("release\n")); err != nil {
		t.Fatal(err)
	}
	stdin.Close()
	if err = child.Wait(); err != nil {
		t.Fatalf("lease owner failed: %v %s", err, &stderr)
	}
	waited = true
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("scope failed to finish")
	}
	if snapshot(t, root).Connections[0].Scope.ContainsName("app", "table") {
		t.Fatal("scope publication missing")
	}
}
func scopeLeaseChild(t *testing.T, rootPath string) {
	root, err := config.ResolveRoot(rootPath, "")
	if err != nil {
		t.Fatal(err)
	}
	store, err := config.Open(t.Context(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	l, err := store.ReadLease(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	if _, _, err = l.ProfileSnapshot(); err != nil {
		t.Fatal(err)
	}
	fmt.Println("ready")
	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() || scanner.Text() != "release" {
		t.Fatal("lease release absent")
	}
}
