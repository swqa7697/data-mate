// Package devtools_test validates developer entry points against disposable copies.
package devtools_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
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
			// The split data/bin layout used to leave an empty .dev behind;
			// purge retries must trim it without removing unrelated contents.
			dev := filepath.Join(root, ".dev")
			if err := os.Mkdir(dev, 0700); err != nil {
				t.Fatal(err)
			}
			sentinel := filepath.Join(dev, "unrelated")
			if err := os.WriteFile(sentinel, []byte("sentinel"), 0600); err != nil {
				t.Fatal(err)
			}
			run(t, root, true, "make", "uninstall", "PURGE=1")
			if b, err := os.ReadFile(sentinel); err != nil || string(b) != "sentinel" {
				t.Fatal("purge changed unrelated contents", err)
			}
			if err := os.Remove(sentinel); err != nil {
				t.Fatal(err)
			}
			run(t, root, true, "make", "uninstall")
			if _, err := os.Stat(dev); err != nil {
				t.Fatal("default uninstall removed development container", err)
			}
			run(t, root, true, "make", "uninstall", "PURGE=1")
			if _, err := os.Lstat(dev); !os.IsNotExist(err) {
				t.Fatal("purge retained empty development container", err)
			}
			run(t, root, true, "make", "uninstall", "PURGE=1")
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
		if output := run(t, temp, true, bin, "version"); !strings.Contains(output, "data-mate "+strings.TrimSpace(string(version))) {
			t.Fatal(output)
		}
		if i == 0 {
			shellHome := t.TempDir()
			for _, shell := range []string{"bash", "zsh"} {
				if shell == "zsh" && runtime.GOOS == "linux" {
					if _, err := os.Stat("/bin/zsh"); os.IsNotExist(err) {
						t.Log("optional zsh execution unverified: not installed")
						continue
					}
				}
				generated := run(t, temp, true, bin, "completion", shell)
				path := filepath.Join(shellHome, "completion."+shell)
				if err := os.WriteFile(path, []byte(generated), 0600); err != nil {
					t.Fatal(err)
				}
				script := `source "$1"; COMP_WORDS=(data-mate d); COMP_CWORD=1; COMP_LINE="data-mate d"; COMP_POINT=11; __start_data-mate; printf '%s\n' "${COMPREPLY[@]}"`
				args := []string{"/usr/bin/env", "HOME=" + shellHome, "ZDOTDIR=" + shellHome, "PATH=" + filepath.Dir(bin) + ":/usr/bin:/bin", "/bin/bash", "--noprofile", "--norc", "-c", script, "completion", path}
				if shell == "zsh" {
					script = `autoload -Uz compinit; compinit -D; source "$1"; (( $+functions[_data-mate] )); data-mate __complete d`
					args = []string{"/usr/bin/env", "HOME=" + shellHome, "ZDOTDIR=" + shellHome, "PATH=" + filepath.Dir(bin) + ":/usr/bin:/bin", "/bin/zsh", "-f", "-c", script, "completion", path}
				}
				if shell == "bash" {
					sentinel := filepath.Join(shellHome, "completion-must-not-execute")
					literalScript := `source "$1"
sentinel="$2"
expected="space ; \$(touch $sentinel)"
data-mate() { printf '%s\n' "$expected" ':4'; }
COMP_WORDS=(data-mate db edit "\$(touch $sentinel)"); COMP_CWORD=3
__start_data-mate
[[ ! -e "$sentinel" ]] || exit 1
COMP_WORDS=(data-mate db edit space); COMP_CWORD=3
__start_data-mate
# Simulate inserting the generated completion into a shell assignment.
eval "actual=${COMPREPLY[0]}"
[[ "$actual" == "$expected" && ! -e "$sentinel" ]]`
					run(t, temp, true, "/bin/bash", "--noprofile", "--norc", "-c", literalScript, "completion", path, sentinel)
				}
				output := run(t, temp, true, args...)
				if !strings.Contains(output, "db") {
					t.Fatal("installed shell completion", shell, output)
				}
				if _, err := os.Stat(filepath.Join(shellHome, ".zcompdump")); !os.IsNotExist(err) {
					t.Fatal("completion created shared cache")
				}
			}
		}
		run(t, root, true, "make", "dev", "ARGS=version")
		// No root is passed: this proves root inference from a different cwd.
		run(t, temp, true, bin, "db", "list", "--json")
		entries, err := os.ReadDir(filepath.Join(root, ".dev"))
		if err != nil || len(entries) != 2 {
			t.Fatal("install must create bin plus owned data-mate directory", err)
		}
		// Inspect the installed process contract: initialization creates neither
		// a keyset nor a service, and passive status must keep it that way.
		var status struct {
			State  string `json:"state"`
			Keyset string `json:"keyset_state"`
		}
		output := run(t, temp, true, bin, "mcp", "status", "--json")
		if err = json.Unmarshal([]byte(output), &status); err != nil || status.State != "stopped" || status.Keyset != "absent" {
			t.Fatal("installation initialized credentials or service", output, err)
		}
		run(t, temp, false, bin, "mcp", "bridge")
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
	for _, name := range []string{".dev/unrelated", ".dev/data-mate/unrelated", ".dev/bin/unrelated", ".misc/evidence"} {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("sentinel"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	databaseBefore, err := os.ReadFile(filepath.Join(root, ".dev/data-mate/data-mate.db"))
	if err != nil {
		t.Fatal(err)
	}
	run(t, root, true, "make", "clean", "PURGE=1")
	databaseAfter, err := os.ReadFile(filepath.Join(root, ".dev/data-mate/data-mate.db"))
	if err != nil || !bytes.Equal(databaseBefore, databaseAfter) {
		t.Fatal("clean changed database", err)
	}
	for _, name := range []string{".dev/unrelated", ".dev/data-mate/unrelated", ".dev/bin/unrelated", ".misc/evidence"} {
		b, err := os.ReadFile(filepath.Join(root, name))
		if err != nil || string(b) != "sentinel" {
			t.Fatal("clean changed retained data", name)
		}
	}
	if _, err = os.Lstat(bin); !os.IsNotExist(err) {
		t.Fatal("clean retained binary", err)
	}
	run(t, root, true, "make", "uninstall")
	run(t, root, true, "make", "build")
	after, err = os.ReadFile(bin)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("reinstall failed", err)
	}
	// Integration preflight must fail before creating Docker resources.
	for _, args := range [][]string{
		{"make", "test-integration"},
		{"make", "test-integration", "DB_DRIVER=oracle"},
		{"make", "test-integration", "DB_DRIVER=postgres", "DB_IMAGE=postgres:15"},
		{"make", "test-integration", "DB_DRIVER=mysql", "DB_IMAGE=mysql:8.0"},
		{"make", "test-integration", "DB_DRIVER=mariadb", "DB_IMAGE=mysql:8.4"},
		{"env", "CI=1", "make", "test-integration", "DB_DRIVER=postgres"},
		{"env", "DATABASE_URL=postgres://unrelated", "make", "test-integration", "DB_DRIVER=postgres"},
	} {
		run(t, root, false, args...)
	}
	// Default uninstall works with malformed saved profiles/vault and never loads keys.
	run(t, root, true, "make", "uninstall")
	if _, err = os.Lstat(bin); !os.IsNotExist(err) {
		t.Fatal("uninstall retained binary", err)
	}
	// Clean delegates failure and suppresses an explicit purge request.
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

// Regression ladder 2: replace the old symlink refusal corpus with ordinary
// publication and cleanup through user-managed output/data directory links.
func TestBuildAndCleanupPreserveDirectoryLinks(t *testing.T) {
	root := filepath.Join(t.TempDir(), "checkout")
	copyCheckout(t, root)
	for _, name := range []string{".dev", ".dev/bin", ".dev/data-mate"} {
		outside := t.TempDir()
		if err := os.Chmod(outside, 0775); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0775); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, path); err != nil {
			t.Fatal(err)
		}
		run(t, root, true, "make", "build")
		run(t, root, true, "make", "uninstall", "PURGE=1")
		if target, err := os.Readlink(path); err != nil || target != outside {
			t.Fatal("cleanup replaced user directory link", name, err)
		}
		if info, err := os.Stat(outside); err != nil || info.Mode().Perm() != 0775 {
			t.Fatal("build changed directory mode", name, err)
		}
		entries, err := os.ReadDir(outside)
		if err != nil || len(entries) != 0 {
			t.Fatal("cleanup left installation files", name, err)
		}
		if err = os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
}
