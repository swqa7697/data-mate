package service

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/swqa7697/data-mate/internal/config"
)

// The real systemd counterpart of the macOS native lifecycle gate uses empty
// isolated installations: no Keychain/keyring access or agent registration.
func TestNativeServiceLifecycle(t *testing.T) {
	if os.Getenv("DATA_MATE_NATIVE_TEST") != "1" {
		t.Skip("requires DATA_MATE_NATIVE_TEST=1 and a Linux systemd user session")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	launcher := nativeLauncher()
	if j, err := launcher.Inspect(ctx, config.Root{}); err != nil || j.Present {
		t.Fatalf("native fixture requires a vacant Data Mate service slot: %v", err)
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "candidate")
	cmd := exec.CommandContext(ctx, "go", "build", "-mod=readonly", "-ldflags=-X main.version=native -X main.revision=fixture", "-o", source, "../../cmd/data-mate")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	controllers := []*Controller{}
	for _, name := range []string{"one", "two"} {
		base := filepath.Join(dir, name)
		if err := os.Mkdir(base, 0700); err != nil {
			t.Fatal(err)
		}
		for _, child := range []string{"data-mate", "bin"} {
			if err := os.Mkdir(filepath.Join(base, child), 0700); err != nil {
				t.Fatal(err)
			}
		}
		root, err := config.ResolveRoot(filepath.Join(base, "data-mate"), "")
		if err != nil {
			t.Fatal(err)
		}
		staged := filepath.Join(base, "bin", ".data-mate.fixture")
		raw, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(staged, raw, 0700); err != nil {
			t.Fatal(err)
		}
		cmd = exec.CommandContext(ctx, staged, "__install", "--root", root.Path)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("install: %v %s", err, out)
		}
		hash, err := binaryHash(config.ExecutablePath(root))
		if err != nil {
			t.Fatal(err)
		}
		c := New(root, Build{"native", "fixture", hash})
		controllers = append(controllers, c)
		t.Cleanup(func() {
			cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
			defer done()
			if _, err := c.Stop(cleanup); err != nil {
				t.Error("owned fixture stop", err)
			}
		})
	}
	first, second := controllers[0], controllers[1]
	state, err := first.EnsureManagement(ctx)
	if err != nil || state.State != "running" || state.MCPEnabled {
		t.Fatal("management startup", state, err)
	}
	var owner *OwnerConflict
	if _, err = second.EnsureManagement(ctx); !errors.As(err, &owner) || owner.Owner.Root != first.Root.Path {
		t.Fatal("first-wins", err)
	}
	if _, err = first.Stop(ctx); err != nil {
		t.Fatal("stop", err)
	}
	if _, err = second.EnsureManagement(ctx); err != nil {
		t.Fatal("handoff", err)
	}
	if _, err = second.Stop(ctx); err != nil {
		t.Fatal("handoff stop", err)
	}
}
