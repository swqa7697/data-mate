// Package devtools_test validates developer entry points against disposable copies.
package devtools_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallIsolationAndClean(t *testing.T) {
	temp := t.TempDir()
	roots := []string{filepath.Join(temp, "first checkout"), filepath.Join(temp, "second checkout")}
	version, err := os.ReadFile("../../VERSION")
	if err != nil {
		t.Fatal(err)
	}
	for i, root := range roots {
		copyCheckout(t, root)
		installArgs := []string{"make", "install"}
		if i == 0 {
			// Dependency setup must succeed even before application source compiles,
			// and must not create an installation or runtime state.
			broken := filepath.Join(root, "cmd", "data-mate", "broken.go")
			if err := os.WriteFile(broken, []byte("not go"), 0600); err != nil {
				t.Fatal(err)
			}
			run(t, root, true, "make", "setup")
			if _, err := os.Lstat(filepath.Join(root, ".dev")); !os.IsNotExist(err) {
				t.Fatal("setup created installation state", err)
			}
			if err := os.Remove(broken); err != nil {
				t.Fatal(err)
			}
			installArgs = append(installArgs, "VERBOSE=1")
		}
		run(t, root, true, installArgs...)
		bin := filepath.Join(root, ".dev", "bin", "data-mate")
		if output := run(t, temp, true, bin, "version"); !strings.Contains(output, "data-mate "+strings.TrimSpace(string(version))+" ") {
			t.Fatal(output)
		}
		run(t, root, true, "make", "dev", "ARGS=version")
		// No root is passed: this proves root inference from a different cwd.
		run(t, temp, true, bin, "db", "list", "--json")
		entries, err := os.ReadDir(filepath.Join(root, ".dev"))
		if err != nil || len(entries) != 1 || entries[0].Name() != "bin" {
			t.Fatal("install created runtime state")
		}
		info, err := os.Stat(bin)
		if err != nil || info.Mode().Perm() != 0700 {
			t.Fatal("binary permissions")
		}
	}
	root := roots[0]
	bin := filepath.Join(root, ".dev", "bin", "data-mate")
	before, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(root, "cmd", "data-mate", "broken.go")
	if err := os.WriteFile(bad, []byte("not go"), 0600); err != nil {
		t.Fatal(err)
	}
	run(t, root, false, "make", "build", "VERBOSE=1")
	after, err := os.ReadFile(bin)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("failed build replaced installed binary")
	}
	if err := os.Remove(bad); err != nil {
		t.Fatal(err)
	}
	leftovers, _ := filepath.Glob(filepath.Join(root, ".dev", "bin", ".data-mate.*"))
	if len(leftovers) != 0 {
		t.Fatal("temporary build output leaked")
	}
	for _, name := range []string{".dev/config/connections.json", ".dev/state/vault.json", ".dev/unrelated", ".misc/evidence"} {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("sentinel"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	run(t, root, false, "make", "clean")
	for _, name := range []string{".dev/config/connections.json", ".dev/state/vault.json", ".dev/unrelated", ".misc/evidence"} {
		b, err := os.ReadFile(filepath.Join(root, name))
		if err != nil || string(b) != "sentinel" {
			t.Fatal("clean changed retained data", name)
		}
	}
	after, err = os.ReadFile(bin)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("clean changed installed binary")
	}
	for _, target := range []string{"test-integration", "uninstall"} {
		run(t, root, false, "make", target)
	}
	run(t, root, false, "make", "uninstall", "PURGE=1")
	after, err = os.ReadFile(bin)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("not-ready uninstall mutated binary")
	}
	// Clean must delegate failure and suppress an explicit purge request. The
	// current uninstall stub cannot otherwise expose which mode it received.
	uninstall := filepath.Join(root, "scripts", "uninstall.sh")
	if err := os.WriteFile(uninstall, []byte("#!/bin/bash\nset -euo pipefail\nprintf '%s' \"${PURGE:-unset}\" > clean-purge\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	run(t, root, false, "make", "clean", "PURGE=1")
	purge, err := os.ReadFile(filepath.Join(root, "clean-purge"))
	if err != nil || string(purge) != "0" {
		t.Fatal("clean did not disable purge", string(purge), err)
	}
}

func TestBuildRefusesSymlinkOutput(t *testing.T) {
	root := filepath.Join(t.TempDir(), "checkout")
	copyCheckout(t, root)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, ".dev")); err != nil {
		t.Fatal(err)
	}
	run(t, root, false, "make", "build")
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatal("build wrote through symlink")
	}
}
