package distribution

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/vault"
	"golang.org/x/sys/unix"
)

func fixture(t *testing.T) *Engine {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root, err := config.ProductionRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	candidateDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(candidateDir, "candidate")
	if err := os.WriteFile(candidate, []byte("synthetic executable"), 0700); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "linux" {
		if err := os.WriteFile(candidate+".sig", make([]byte, 384), 0600); err != nil {
			t.Fatal(err)
		}
	}
	e := &Engine{Home: home, Root: root, Candidate: candidate, Metadata: Contract("1.0.0"), Shell: "bash", Completion: func(string) ([]byte, error) { return []byte("# synthetic completion\n"), nil }}
	e.Cleanup = func(ctx context.Context, purge bool, files func() error) error {
		s, err := config.OpenLifecycle(ctx, root)
		if err != nil {
			return err
		}
		defer s.Close()
		l, err := s.Lifecycle(ctx)
		if err != nil {
			return err
		}
		defer l.Release()
		if err = files(); err != nil {
			return err
		}
		state, err := l.CleanupLease(ctx)
		if err != nil {
			return err
		}
		defer state.Release()
		if purge {
			if err = vault.New(s, fixtureKeys{}).PurgeLocked(ctx, state); err != nil {
				return err
			}
			return state.FinishPurge()
		}
		if err = state.RemoveBinary(); err != nil {
			return err
		}
		return state.TrimBinaryDirectory()
	}
	return e
}

type fixtureKeys struct{}

func (fixtureKeys) Load(context.Context, string) ([]byte, error) { return nil, vault.ErrMissing }
func (fixtureKeys) CreateIfAbsent(context.Context, string, []byte) ([]byte, error) {
	return nil, errors.New("unexpected key creation")
}
func (fixtureKeys) Delete(context.Context, string) error { return nil }

func storeIdentity(t *testing.T, e *Engine) config.Identity {
	t.Helper()
	s, err := config.OpenLifecycle(t.Context(), e.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	l, err := s.Lifecycle(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	return l.Identity()
}

// Regression ladder 3: existing config tests own single-store publication, not
// release trust or recoverable publication across binary, shell and account paths.
func TestDistributionPublication(t *testing.T) {
	if home := os.Getenv("DATA_MATE_DISTRIBUTION_LOCK_CHILD"); home != "" {
		root, err := config.ProductionRoot(home)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		l, err := acquireObserved(ctx, root, func() { fmt.Println("waiting") })
		if l != nil {
			l.close()
		}
		if !errors.Is(err, config.ErrStale) {
			t.Fatal("stale waiter acquired recreated installation", err)
		}
		return
	}
	t.Run("cross-process stale waiter cannot enter recreated root", func(t *testing.T) {
		e := fixture(t)
		if err := os.MkdirAll(e.Root.Path, 0700); err != nil {
			t.Fatal(err)
		}
		held, err := acquire(t.Context(), e.Root)
		if err != nil {
			t.Fatal(err)
		}
		defer held.close()
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		child := exec.CommandContext(ctx, exe, "-test.run=^TestDistributionPublication$")
		child.Env = append(os.Environ(), "DATA_MATE_DISTRIBUTION_LOCK_CHILD="+e.Home)
		stdout, err := child.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err = child.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if child.ProcessState == nil {
				_ = child.Process.Kill()
				_ = child.Wait()
			}
		})
		line, err := bufio.NewReader(stdout).ReadString('\n')
		if err != nil || line != "waiting\n" {
			t.Fatal("waiter synchronization", line, err)
		}
		if err = os.Rename(e.Root.Path, e.Root.Path+".retired"); err != nil {
			t.Fatal(err)
		}
		if err = os.Mkdir(e.Root.Path, 0700); err != nil {
			t.Fatal(err)
		}
		// Unlock without closing the descriptors so the deferred close stays valid.
		if err = unix.Flock(int(held.file.Fd()), unix.LOCK_UN); err != nil {
			t.Fatal(err)
		}
		if err = child.Wait(); err != nil {
			t.Fatal("waiter did not fail closed", err)
		}
	})

	t.Run("install no-op upgrade retention and purge", func(t *testing.T) {
		e := fixture(t)
		// Regression ladder 2: exercise the existing full lifecycle with a
		// user-managed mode-0775 command directory, without permission repair.
		bin := filepath.Join(e.Home, ".local", "bin")
		if err := os.MkdirAll(bin, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(bin, 0775); err != nil {
			t.Fatal(err)
		}
		if err := e.Install(t.Context()); err != nil {
			t.Fatal("install", err)
		}
		initial := storeIdentity(t, e)
		// A retained installation must preserve populated state, not just its UUID.
		profile := config.Profile{
			ID: "936e3468-5b48-4ef2-9a89-964449f06d98", Alias: "retained", Driver: "postgres",
			CredentialRef: "606f9022-9128-4ab6-bb3f-410d701ef85b",
			Connection:    config.Connection{Host: "127.0.0.1", Port: 5432, Database: "fixture", Username: "reader"},
			Transport:     config.Transport{TLS: config.TLS{Mode: "disabled"}},
		}
		bundle := config.Ciphertext{Reference: profile.CredentialRef, ConnectionID: profile.ID, Version: 1, Data: []byte{0x01, 0x8f, 0x00, 0xfe, 0x03}}
		key := config.KeysetMetadata{Account: initial.KeyAccount, Phase: "ready", Fingerprint: strings.Repeat("a", 64), Reserved: 1}
		var saved config.Profiles
		var revision config.Revision
		checkState := func(seed bool) {
			t.Helper()
			s, err := config.OpenExisting(t.Context(), e.Root)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			lease, err := s.WriteLease(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Release()
			if seed {
				_, rev, err := lease.ProfileSnapshot()
				if err != nil {
					t.Fatal(err)
				}
				if err = lease.SaveKeyset(key); err != nil {
					t.Fatal(err)
				}
				if _, err = lease.Publish(config.Profiles{Version: 1, Connections: []config.Profile{profile}}, rev, []config.Ciphertext{bundle}); err != nil {
					t.Fatal(err)
				}
				saved, revision, err = lease.ProfileSnapshot()
				if err != nil {
					t.Fatal(err)
				}
			}
			got, rev, err := lease.ProfileSnapshot()
			if err != nil || rev != revision || !reflect.DeepEqual(got, saved) {
				t.Fatal("distribution changed profiles or revision", err)
			}
			actual, err := lease.Bundle(bundle.Reference)
			if err != nil || !reflect.DeepEqual(actual, bundle) {
				t.Fatal("distribution changed opaque credentials", err)
			}
			meta, err := lease.Keyset()
			if err != nil || meta != key {
				t.Fatal("distribution changed keyset namespace or usage", err)
			}
		}
		checkState(true)
		inv, err := e.load()
		if err != nil {
			t.Fatal(err)
		}
		if err = e.Install(t.Context()); err != nil {
			t.Fatal("reinstall", err)
		}
		repeated, _ := e.load()
		if !reflect.DeepEqual(inv, repeated) {
			t.Fatal("current version replaced artifacts")
		}
		e.Metadata.Version = "1.1.0"
		if err = os.WriteFile(e.Candidate, []byte("replacement executable"), 0700); err != nil {
			t.Fatal(err)
		}
		if err = e.Install(t.Context()); err != nil {
			t.Fatal("upgrade", err)
		}
		if got := storeIdentity(t, e); got.ID != initial.ID || got.KeyAccount != initial.KeyAccount {
			t.Fatal("upgrade replaced store identity")
		}
		checkState(false)

		// Known runtime leaves can be edited or replaced without touching link targets.
		sentinel := filepath.Join(e.Home, "unrelated")
		if err = os.WriteFile(sentinel, []byte("keep"), 0600); err != nil {
			t.Fatal(err)
		}
		completion := filepath.Join(e.Root.Path, "shell", "completion.bash")
		if err = os.Remove(completion); err != nil {
			t.Fatal(err)
		}
		if err = os.Link(sentinel, completion); err != nil {
			t.Fatal(err)
		}
		if err = e.Uninstall(t.Context(), false); err != nil {
			t.Fatal("uninstall", err)
		}
		if !absent(config.ExecutablePath(e.Root)) || !absent(e.commandPath()) {
			t.Fatal("default uninstall left runtime")
		}
		checkState(false)
		if got := storeIdentity(t, e); got.ID != initial.ID || got.KeyAccount != initial.KeyAccount {
			t.Fatal("uninstall replaced identity")
		}
		if err = e.Install(t.Context()); err != nil {
			t.Fatal("retained reinstall", err)
		}
		checkState(false)
		if err = e.Uninstall(t.Context(), true); err != nil {
			t.Fatal("purge", err)
		}
		if err = e.Uninstall(t.Context(), true); err != nil {
			t.Fatal("repeat purge", err)
		}
		if !absent(e.Root.Path) {
			t.Fatal("purge left managed residue")
		}
		if raw, err := os.ReadFile(sentinel); err != nil || string(raw) != "keep" {
			t.Fatal("unrelated target changed", err)
		}
		if st, err := os.Stat(bin); err != nil || st.Mode().Perm() != 0775 {
			t.Fatal("user directory mode changed", err)
		}
	})

	t.Run("failed publication preserves binary and interrupted completion is repeatable", func(t *testing.T) {
		e := fixture(t)
		if err := e.Install(t.Context()); err != nil {
			t.Fatal(err)
		}
		target := config.ExecutablePath(e.Root)
		before, err := os.ReadFile(target)
		if err != nil {
			t.Fatal(err)
		}
		e.Metadata.Version = "1.1.0"
		if err = os.WriteFile(e.Candidate, []byte("new binary"), 0700); err != nil {
			t.Fatal(err)
		}
		e.Fault = func(phase string) error {
			if phase == "before-publish" {
				return errors.New("interrupted")
			}
			return nil
		}
		if err = e.Install(t.Context()); err == nil {
			t.Fatal("fault missing")
		}
		if got, err := os.ReadFile(target); err != nil || string(got) != string(before) {
			t.Fatal("failed preparation damaged old binary", err)
		}
		publish := e.Publish
		e.Fault = nil
		e.Publish = func(context.Context, string) error { return os.ErrPermission }
		if err = e.Install(t.Context()); !errors.Is(err, os.ErrPermission) {
			t.Fatal("replacement failure", err)
		}
		if got, err := os.ReadFile(target); err != nil || string(got) != string(before) {
			t.Fatal("failed replacement damaged binary", err)
		}
		e.Publish = publish
		e.Fault = func(phase string) error {
			if phase == "published:binary" {
				return errors.New("interrupted")
			}
			return nil
		}
		if err = e.Install(t.Context()); err == nil {
			t.Fatal("post-publication fault missing")
		}
		if got, err := os.ReadFile(target); err != nil || string(got) != "new binary" {
			t.Fatal("published binary missing", err)
		}
		e.Fault = nil
		if err = e.Install(t.Context()); err != nil {
			t.Fatal("retry", err)
		}
	})
	t.Run("legacy partial receipt and pending install reuse identity", func(t *testing.T) {
		e := fixture(t)
		if err := e.Install(t.Context()); err != nil {
			t.Fatal(err)
		}
		id := storeIdentity(t, e)
		inv, err := e.load()
		if err != nil {
			t.Fatal(err)
		}
		inv.Phase = "publishing"
		inv.Release = ""
		inv.Directories = []Directory{{Path: e.Root.Path, Device: 999, Inode: 999}}
		stage := filepath.Join(e.Root.Path, "bin", ".data-mate.old")
		outside := filepath.Join(e.Home, ".data-mate.unrelated")
		for _, path := range []string{stage, outside} {
			if err = os.WriteFile(path, []byte("keep until scoped cleanup"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		inv.Changes = []change{{Path: config.ExecutablePath(e.Root), Stage: stage, Backup: outside}}
		startup := filepath.Join(e.Home, ".bashrc")
		backup := filepath.Join(e.Home, ".data-mate.legacy.rollback")
		if err = os.WriteFile(startup, []byte("# unrelated startup\n"), 0644); err != nil {
			t.Fatal(err)
		}
		if err = os.Rename(startup, backup); err != nil {
			t.Fatal(err)
		}
		inv.Changes = append(inv.Changes, change{Path: startup, Backup: backup, External: true})
		if err = e.save(inv); err != nil {
			t.Fatal(err)
		}
		store, err := config.OpenLifecycle(t.Context(), e.Root)
		if err != nil {
			t.Fatal(err)
		}
		l, err := store.Lifecycle(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if err = l.SetDistributionPending(t.Context(), true); err != nil {
			t.Fatal(err)
		}
		l.Release()
		store.Close()
		if err = e.Install(t.Context()); err != nil {
			t.Fatal("adopt partial", err)
		}
		if raw, err := os.ReadFile(startup); err != nil || !strings.HasPrefix(string(raw), "# unrelated startup\n") || !absent(backup) {
			t.Fatal("legacy rename lost unrelated shell content", err)
		}
		got := storeIdentity(t, e)
		if got.ID != id.ID || got.KeyAccount != id.KeyAccount || got.Pending {
			t.Fatal("migration changed namespace or retained pending")
		}
		if !absent(stage) || absent(outside) {
			t.Fatal("legacy cleanup escaped recorded scope")
		}
		if err = e.Uninstall(t.Context(), false); err != nil {
			t.Fatal(err)
		}
		if err = e.Uninstall(t.Context(), true); err != nil {
			t.Fatal("purge retained state", err)
		}
	})
	t.Run("shell ambiguity warns without blocking installation or cleanup", func(t *testing.T) {
		e := fixture(t)
		path := filepath.Join(e.Home, ".bashrc")
		text := "user content\n# >>> Data Mate >>>\nuser edit\n"
		if err := os.WriteFile(path, []byte(text), 0644); err != nil {
			t.Fatal(err)
		}
		warnings := []string{}
		e.Warn = func(s string) { warnings = append(warnings, s) }
		if err := e.Install(t.Context()); err != nil {
			t.Fatal(err)
		}
		if len(warnings) == 0 || absent(config.ExecutablePath(e.Root)) {
			t.Fatal("shell conflict blocked install or lacked warning")
		}
		if os.Geteuid() != 0 {
			// The OS denies this real write; integration warns and preserves the program.
			readOnly := filepath.Join(e.Home, ".bash_profile")
			if err := os.WriteFile(readOnly, []byte("# read only\n"), 0400); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(readOnly, 0400); err != nil {
				t.Fatal(err)
			}
			beforeWarnings := len(warnings)
			if err := e.Install(t.Context()); err != nil {
				t.Fatal("read-only shell blocked program", err)
			}
			if len(warnings) <= beforeWarnings {
				t.Fatal("shell write failure lacked warning")
			}
			if raw, err := os.ReadFile(readOnly); err != nil || string(raw) != "# read only\n" {
				t.Fatal("read-only shell changed", err)
			}
		}
		// A recorded startup block can also become ambiguous after installation.
		inv, err := e.load()
		if err != nil {
			t.Fatal(err)
		}
		inv.Blocks = append(inv.Blocks, ShellBlock{Path: path})
		if err = e.save(inv); err != nil {
			t.Fatal(err)
		}
		if err = e.Uninstall(t.Context(), true); err != nil {
			t.Fatal(err)
		}
		if raw, err := os.ReadFile(path); err != nil || string(raw) != text {
			t.Fatal("ambiguous user content changed", err)
		}
	})
	t.Run("command collision and user managed symlinks", func(t *testing.T) {
		e := fixture(t)
		local := filepath.Join(e.Home, ".local")
		external := filepath.Join(e.Home, "shared directory")
		if err := os.Mkdir(external, 0775); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(external, 0775); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(external, local); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(local, "bin"), 0775); err != nil {
			t.Fatal(err)
		}
		collision := e.commandPath()
		if err := os.WriteFile(collision, []byte("unrelated"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := e.Install(t.Context()); err == nil {
			t.Fatal("unrelated command overwritten")
		}
		if raw, _ := os.ReadFile(collision); string(raw) != "unrelated" {
			t.Fatal("command changed")
		}
		if err := os.Remove(collision); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(e.Root.Path), 0775); err != nil {
			t.Fatal(err)
		}
		dataTarget := filepath.Join(e.Home, "application data")
		if err := os.Mkdir(dataTarget, 0775); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dataTarget, 0775); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(dataTarget, e.Root.Path); err != nil {
			t.Fatal(err)
		}
		binaryTarget := filepath.Join(e.Home, "application binaries")
		if err := os.Mkdir(binaryTarget, 0775); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(binaryTarget, filepath.Join(e.Root.Path, "bin")); err != nil {
			t.Fatal(err)
		}
		startup := filepath.Join(e.Home, "my shell config")
		rc := filepath.Join(e.Home, ".bashrc")
		if err := os.WriteFile(startup, []byte("# user's configuration\n"), 0640); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(startup, 0640); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(startup, rc); err != nil {
			t.Fatal(err)
		}
		if err := e.Install(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := e.Install(t.Context()); err != nil {
			t.Fatal("repeat with links", err)
		}
		if link, err := os.Readlink(rc); err != nil || link != startup {
			t.Fatal("startup symlink replaced", err)
		}
		if st, err := os.Stat(startup); err != nil || st.Mode().Perm() != 0640 {
			t.Fatal("startup mode changed", err)
		}
		raw, err := os.ReadFile(startup)
		if err != nil || strings.Count(string(raw), blockStart) != 1 {
			t.Fatal("shell block duplicated", err)
		}
		if err = e.Uninstall(t.Context(), true); err != nil {
			t.Fatal(err)
		}
		if raw, err = os.ReadFile(startup); err != nil || string(raw) != "# user's configuration\n" {
			t.Fatal("unrelated shell content changed", err)
		}
		if st, err := os.Stat(external); err != nil || st.Mode().Perm() != 0775 {
			t.Fatal("directory mode changed", err)
		}
		if link, err := os.Readlink(local); err != nil || link != external {
			t.Fatal("directory link removed", err)
		}
	})
}

func TestReleaseContracts(t *testing.T) {
	// Extend the release trust regression: an unavailable notarization service
	// must not block a valid publisher, and invalid signatures must still fail.
	t.Run("publisher-without-online-notarization", func(t *testing.T) {
		for _, scenario := range []struct {
			name string
			want error
		}{
			{"accepted", nil},
			{"rejected", ErrRelease},
			{"canceled", context.Canceled},
		} {
			ctx, cancel := context.WithCancel(t.Context())
			if scenario.name == "canceled" {
				cancel()
			}
			calls := 0
			run := func(_ context.Context, exe string, args ...string) ([]byte, error) {
				calls++
				if exe != "/usr/bin/codesign" || len(args) != 5 || args[0] != "--verify" || args[1] != "--strict" || args[2] != "-R" || args[4] != "candidate" {
					t.Fatalf("%s: unexpected verification command: %s %v", scenario.name, exe, args)
				}
				// Model a host whose publisher check works but ticket lookup fails.
				if strings.Contains(args[3], "notarized") {
					return nil, ErrRelease
				}
				return nil, scenario.want
			}
			err := verifyCodeSignature(ctx, "candidate", run)
			cancel()
			wantCalls := 1
			if scenario.name == "canceled" {
				wantCalls = 0
			}
			if !errors.Is(err, scenario.want) || calls != wantCalls {
				t.Fatalf("%s: calls=%d err=%v", scenario.name, calls, err)
			}
			if scenario.name == "canceled" {
				continue
			}
			// Run the actual bootstrap verifier with an offline codesign peer.
			cmd := exec.CommandContext(t.Context(), "/bin/bash", "-c", `
source "$1"
scenario="$2"
/usr/bin/codesign() {
  [[ "$#" == 5 && "$1" == --verify && "$2" == --strict && "$3" == -R && "$5" == candidate ]] || return 1
  [[ "$4" != *notarized* && "$scenario" == accepted ]]
}
verify_darwin_signature candidate
`, "verification", "../../scripts/install-release.sh", scenario.name)
			output, shellErr := cmd.CombinedOutput()
			if (shellErr == nil) != (scenario.want == nil) {
				t.Fatalf("%s: shell err=%v output=%s", scenario.name, shellErr, output)
			}
		}
		// Sourcing must not install; normal and piped invocations must still
		// enter argument validation before network or installation effects.
		for _, invocation := range []string{`/bin/bash "$1" --invalid-option`, `/bin/bash -s -- --invalid-option < "$1"`} {
			cmd := exec.CommandContext(t.Context(), "/bin/bash", "-c", invocation, "bootstrap", "../../scripts/install-release.sh")
			output, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 2 {
				t.Fatalf("bootstrap entry: %v %s", err, output)
			}
		}
	})
	// Regression ladder 2: extend bootstrap coverage through main's EXIT trap.
	// A failing download or candidate must preserve its status and remove staging
	// after main's local variables have gone out of scope under errexit.
	for _, scenario := range []string{"success", "download-failure", "install-failure"} {
		scratch := t.TempDir()
		cmd := exec.CommandContext(t.Context(), "/bin/bash", "-c", `
source "$1"
scenario="$2"
scratch="$3"
fetch() {
  printf '%s' "$stage" > "$scratch/stage"
  if [[ "$scenario" == download-failure ]]; then return 37; fi
  fetched_url=https://github.com/swqa7697/data-mate/releases/tag/v1.2.3
  if [[ "$2" == "$stage/$binary_name" ]]; then
    printf '#!/bin/bash\nif [[ "$1" == __release-metadata ]]; then exit 0; fi\nexit %s\n' "$candidate_status" > "$2"
  else
    : > "$2"
  fi
}
metadata() { :; }
verify_checksums() { :; }
verify_signature() { :; }
/usr/bin/file() {
  if [[ "$platform" == darwin_arm64 ]]; then
    printf 'Mach-O 64-bit executable arm64\n'
  else
    printf 'ELF 64-bit LSB executable, x86-64\n'
  fi
}
candidate_status=0
if [[ "$scenario" == install-failure ]]; then candidate_status=23; fi
main
`, "bootstrap", "../../scripts/install-release.sh", scenario, scratch)
		output, err := cmd.CombinedOutput()
		want := 0
		if scenario == "download-failure" {
			want = 37
		} else if scenario == "install-failure" {
			want = 23
		}
		stage, readErr := os.ReadFile(filepath.Join(scratch, "stage"))
		if readErr != nil {
			t.Fatalf("%s: bootstrap did not stage: %v %s", scenario, readErr, output)
		}
		if !absent(string(stage)) {
			_ = os.RemoveAll(string(stage))
			t.Errorf("%s: bootstrap leaked staging directory", scenario)
		}
		if cmd.ProcessState.ExitCode() != want || len(output) != 0 {
			t.Errorf("%s: exit=%d want=%d err=%v output=%s", scenario, cmd.ProcessState.ExitCode(), want, err, output)
		}
	}
	valid := Contract("1.2.3").Text()
	for _, input := range []string{valid + "format=1\n", strings.Replace(valid, "format=1", "format=2", 1), strings.TrimSuffix(valid, "\n"), strings.Replace(valid, "version=1.2.3", "version=1.2.3-rc.1", 1), valid + "unknown=value\n", strings.Replace(valid, "store_schema=1", "store_schema=$(touch /tmp/never)", 1)} {
		if _, err := ParseMetadata([]byte(input)); err == nil {
			t.Fatal("invalid metadata accepted")
		}
	}
	if meta, err := ParseMetadata([]byte(valid)); err != nil || meta != Contract("1.2.3") {
		t.Fatal(meta, err)
	}
	for _, address := range []string{"http://github.com/" + Repository + "/releases/latest", "https://user@github.com/" + Repository + "/releases/latest", "https:///missing-host"} {
		if allowedURL(address) {
			t.Fatal("unsafe URL accepted", address)
		}
	}
	// Use normal HTTPS redirects (including CDN hosts) and the standard proxy
	// resolver, while still refusing a redirect downgrade to plain HTTP.
	client := NewClient().HTTP
	cdn, _ := http.NewRequest("GET", "https://cdn.example.test/asset", nil)
	if err := client.CheckRedirect(cdn, nil); err != nil {
		t.Fatal("ordinary CDN redirect refused", err)
	}
	insecure, _ := http.NewRequest("GET", "http://cdn.example.test/asset", nil)
	if err := client.CheckRedirect(insecure, nil); err == nil {
		t.Fatal("HTTPS downgrade accepted")
	}
	request, _ := http.NewRequest("GET", "https://github.com/", nil)
	standard, err := http.ProxyFromEnvironment(request)
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := client.Transport.(*http.Transport).Proxy(request)
	if err != nil {
		t.Fatal(err)
	}
	asText := func(value *url.URL) string {
		if value == nil {
			return ""
		}
		return value.String()
	}
	if asText(proxy) != asText(standard) {
		t.Fatal("release client bypassed standard proxy configuration")
	}
	// Local HTTP seam exercises pinning, integrity, truncation and trust failure.
	for _, scenario := range []string{"pinned", "checksum", "truncated", "unsigned", "prerelease", "downgrade"} {
		t.Run(scenario, func(t *testing.T) {
			metadata := Contract("1.2.3")
			names := platformAssets(metadata.Platform)
			encoded, _ := json.Marshal(metadata)
			executable := "#!/bin/sh\nprintf '%s\\n' '" + string(encoded) + "'\n"
			sums := fmt.Sprintf("%s  install.sh\n%s  %s\n%s  %s\n", digest(nil), digest([]byte(metadata.Text())), names.metadata, digest([]byte(executable)), names.binary)
			if names.signature != "" {
				sums += fmt.Sprintf("%s  %s\n", digest([]byte("signature")), names.signature)
			}
			requests, verified := 0, false
			c := NewClient()
			c.verify = func(_ context.Context, path string, got Metadata) error {
				verified = true
				if got != metadata || scenario == "unsigned" {
					return ErrRelease
				}
				return nil
			}
			c.HTTP.Transport = roundTrip(func(req *http.Request) (*http.Response, error) {
				requests++
				var body string
				if requests == 1 {
					if !strings.HasSuffix(req.URL.Path, "/releases/latest") {
						t.Fatal("not resolving latest first")
					}
					assets := []map[string]string{}
					for _, name := range names.all() {
						assets = append(assets, map[string]string{"name": name, "browser_download_url": "https://github.com/" + Repository + "/releases/download/v1.2.3/" + name})
					}
					raw, _ := json.Marshal(map[string]any{"tag_name": "v1.2.3", "draft": false, "prerelease": scenario == "prerelease", "assets": assets})
					body = string(raw)
				} else {
					if !strings.HasPrefix(req.URL.Path, "/"+Repository+"/releases/download/v1.2.3/") {
						t.Fatal("mixed release versions", req.URL)
					}
					switch filepath.Base(req.URL.Path) {
					case names.metadata:
						body = metadata.Text()
					case names.sums:
						body = sums
					case names.binary:
						body = executable
						if scenario == "checksum" {
							body += "#tampered"
						}
					case names.signature:
						body = "signature"
					default:
						t.Fatal("unexpected request", req.URL)
					}
				}
				length := int64(len(body))
				if scenario == "truncated" && requests == 4 {
					length++
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), ContentLength: length, Header: make(http.Header), Request: req}, nil
			})
			current := "1.0.0"
			if scenario == "downgrade" {
				current = "2.0.0"
			}
			candidate, err := c.Latest(t.Context(), current)
			if candidate != nil {
				defer candidate.Close()
			}
			if scenario == "pinned" {
				if err != nil || candidate.Metadata != metadata || !verified {
					t.Fatal("pinned acquisition", err)
				}
			} else {
				if err == nil {
					t.Fatal("unsafe release accepted")
				}
				if scenario != "unsigned" && verified {
					t.Fatal("invalid release reached native verifier")
				}
			}
		})
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// Regression ladder 3: CLI callbacks cannot exercise sourced shell option/PATH
// behavior. These real shells use isolated startup directories and synthetic assets.
func TestShellActivation(t *testing.T) {
	for _, shell := range []string{"bash", "zsh"} {
		t.Run(shell, func(t *testing.T) {
			if shell == "zsh" && runtime.GOOS == "linux" {
				if _, err := os.Stat("/bin/zsh"); os.IsNotExist(err) {
					t.Skip("optional zsh is not installed on Linux")
				}
			}
			e := fixture(t)
			e.Shell = shell
			if err := e.Install(t.Context()); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(e.Root.Path, "shell", "loader."+shell)
			script := "source " + Quote(path) + "\nsource " + Quote(path) + "\nprintf '%s\\n' \"$PATH\"\n"
			args := []string{"--noprofile", "--norc", "-c", script}
			if shell == "zsh" {
				args = []string{"-f", "-c", script}
			}
			cmd := exec.CommandContext(t.Context(), "/bin/"+shell, args...)
			cmd.Env = []string{"HOME=" + e.Home, "ZDOTDIR=" + e.Home, "PATH=/usr/bin:/bin"}
			raw, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatal(err, string(raw))
			}
			if strings.Count(string(raw), filepath.Join(e.Home, ".local", "bin")) != 1 {
				t.Fatal("PATH duplicated", string(raw))
			}
			if !absent(filepath.Join(e.Home, ".zcompdump")) {
				t.Fatal("loader created shared completion cache")
			}
		})
	}
}
