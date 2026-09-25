package distribution

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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
	e := &Engine{Home: home, Root: root, Candidate: candidate, Metadata: Contract("1.0.0"), Shell: "bash", checkInstalled: func(context.Context, string, Metadata) error { return nil }, Verify: func(context.Context, string, Metadata) error { return nil }, Completion: func(string) ([]byte, error) { return []byte("# synthetic completion\n"), nil }}
	e.Cleanup = func(ctx context.Context, purge bool) error {
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

func prepareHelper(t *testing.T, e *Engine) (string, error) {
	t.Helper()
	path, err := e.PrepareHelper(t.Context())
	if err == nil {
		t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(path)) })
	}
	return path, err
}

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
		if _, err := privateDirs([]string{filepath.Join(e.Home, ".local"), filepath.Join(e.Home, ".local", "share"), e.Root.Path}); err != nil {
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
		if err := e.Install(t.Context()); err != nil {
			t.Fatal("install", err)
		}
		initial := storeIdentity(t, e)
		// A retained installation must preserve populated state, not just its UUID.
		profile := config.Profile{
			ID: "936e3468-5b48-4ef2-9a89-964449f06d98", Alias: "retained", Driver: "postgres",
			CredentialRef: "606f9022-9128-4ab6-bb3f-410d701ef85b",
			Connection:    config.Connection{Host: "127.0.0.1", Port: 5432, Database: "fixture", Username: "reader"},
			Transport:     config.Transport{TLS: config.TLS{Mode: "disabled"}}, Scope: config.Scope{Mode: "all"},
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
		source := e.Candidate
		helper, err := prepareHelper(t, e)
		if err != nil {
			t.Fatal(err)
		}
		e.Candidate = helper
		if err = e.Uninstall(t.Context(), false); err != nil {
			t.Fatal("uninstall", err)
		}
		if !absent(helper) || !absent(config.ExecutablePath(e.Root)) || !absent(filepath.Join(e.Home, ".bashrc")) {
			t.Fatal("default uninstall left runtime artifacts")
		}
		if got := storeIdentity(t, e); got.ID != initial.ID || got.KeyAccount != initial.KeyAccount {
			t.Fatal("uninstall replaced credential namespace")
		}
		checkState(false)
		e.Candidate = source
		if err = e.Install(t.Context()); err != nil {
			t.Fatal("retained reinstall", err)
		}
		checkState(false)
		helper, err = prepareHelper(t, e)
		if err != nil {
			t.Fatal(err)
		}
		e.Candidate = helper
		if err = e.Uninstall(t.Context(), true); err != nil {
			t.Fatal("purge", err)
		}
		if !absent(e.Root.Path) || !absent(helper) {
			t.Fatal("purge left managed residue")
		}
	})
	t.Run("interrupted replacement restores old artifacts", func(t *testing.T) {
		e := fixture(t)
		if err := e.Install(t.Context()); err != nil {
			t.Fatal(err)
		}
		before, _ := e.load()
		e.Metadata.Version = "1.1.0"
		if err := os.WriteFile(e.Candidate, []byte("replacement"), 0700); err != nil {
			t.Fatal(err)
		}
		e.Fault = func(phase string) error {
			if phase == "published:completion.bash" {
				return errors.New("synthetic power loss")
			}
			return nil
		}
		if err := e.Install(t.Context()); err == nil {
			t.Fatal("fault did not propagate")
		}
		after, err := e.load()
		if err != nil || after.Release != before.Release || !reflect.DeepEqual(after.Artifacts, before.Artifacts) {
			t.Fatal("rollback", after.Phase, err)
		}
		for _, a := range before.Artifacts {
			if exact(a.Path, a.File) != nil {
				t.Fatal("old artifact not restored", a.Path)
			}
		}
		e.Fault = nil
		if err := e.Install(t.Context()); err != nil {
			t.Fatal("retry", err)
		}
	})
	t.Run("purge after default uninstall resumes without recreating runtime", func(t *testing.T) {
		e := fixture(t)
		if err := e.Install(t.Context()); err != nil {
			t.Fatal(err)
		}
		source := e.Candidate
		helper, err := prepareHelper(t, e)
		if err != nil {
			t.Fatal(err)
		}
		e.Candidate = helper
		if err = e.Uninstall(t.Context(), false); err != nil {
			t.Fatal(err)
		}
		e.Candidate = source
		helper, err = prepareHelper(t, e)
		if err != nil {
			t.Fatal(err)
		}
		e.Candidate = helper
		e.Fault = func(phase string) error {
			if phase == "terminal-root-removed" {
				return errors.New("interrupted terminal cleanup")
			}
			return nil
		}
		if err = e.Uninstall(t.Context(), true); err == nil {
			t.Fatal("terminal fault missing")
		}
		if !absent(e.Root.Path) || absent(e.terminalPath()) || absent(helper) {
			t.Fatal("terminal receipt lost recovery authority")
		}
		e.Fault = nil
		e.Candidate = source
		recovered, err := prepareHelper(t, e)
		if err != nil || recovered != helper {
			t.Fatal("bootstrap recovery helper", err)
		}
		e.Candidate = recovered
		if err = e.Uninstall(t.Context(), true); err != nil {
			t.Fatal("terminal retry", err)
		}
		if !absent(e.Root.Path) || !absent(e.terminalPath()) || !absent(helper) || !absent(filepath.Join(e.Home, ".data-mate-cleanup.lock")) {
			t.Fatal("terminal cleanup residue")
		}
	})
	t.Run("edited shell block retains helper and cleanup authority", func(t *testing.T) {
		e := fixture(t)
		if err := e.Install(t.Context()); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(e.Home, ".bashrc")
		if err := os.WriteFile(path, []byte("user replacement\n"), 0600); err != nil {
			t.Fatal(err)
		}
		helper, err := prepareHelper(t, e)
		if err != nil {
			t.Fatal(err)
		}
		e.Candidate = helper
		if err = e.Uninstall(t.Context(), true); err == nil {
			t.Fatal("edited startup file removed")
		}
		raw, _ := os.ReadFile(path)
		if string(raw) != "user replacement\n" || absent(helper) {
			t.Fatal("conflict lost user data or retry helper")
		}
		inv, err := e.load()
		if err != nil || inv.Phase != "purging" {
			t.Fatal("missing durable cleanup intent", err)
		}
	})
	t.Run("collision and signature refusal preserve unrelated state", func(t *testing.T) {
		e := fixture(t)
		e.Verify = func(context.Context, string, Metadata) error { return ErrRelease }
		if err := e.Install(t.Context()); err == nil || !absent(e.Root.Path) {
			t.Fatal("trust failure mutated root")
		}
		e.Verify = func(context.Context, string, Metadata) error { return nil }
		if err := os.MkdirAll(filepath.Join(e.Home, ".local", "bin"), 0700); err != nil {
			t.Fatal(err)
		}
		collision := filepath.Join(e.Home, ".local", "bin", "data-mate")
		if err := os.WriteFile(collision, []byte("unrelated"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := e.Install(t.Context()); err == nil {
			t.Fatal("unrelated command adopted")
		}
		raw, _ := os.ReadFile(collision)
		if string(raw) != "unrelated" {
			t.Fatal("unrelated command changed")
		}
	})
}

func TestReleaseContracts(t *testing.T) {
	valid := Contract("1.2.3").Text()
	for _, input := range []string{valid + "format=1\n", strings.Replace(valid, "format=1", "format=2", 1), strings.TrimSuffix(valid, "\n"), strings.Replace(valid, "version=1.2.3", "version=1.2.3-rc.1", 1), valid + "unknown=value\n", strings.Replace(valid, "store_schema=1", "store_schema=$(touch /tmp/never)", 1)} {
		if _, err := ParseMetadata([]byte(input)); err == nil {
			t.Fatal("invalid metadata accepted")
		}
	}
	if meta, err := ParseMetadata([]byte(valid)); err != nil || meta != Contract("1.2.3") {
		t.Fatal(meta, err)
	}
	for _, address := range []string{"http://github.com/" + Repository + "/releases/latest", "https://github.com.evil.test/" + Repository + "/releases/latest", "https://user@github.com/" + Repository + "/releases/latest", "https://github.com/another/project/releases/latest", "https://github.com:444/" + Repository + "/releases/latest"} {
		if allowedURL(address) {
			t.Fatal("unsafe URL accepted", address)
		}
	}
	// Local HTTP seam exercises pinning, integrity, truncation and trust failure.
	for _, scenario := range []string{"pinned", "checksum", "truncated", "unsigned", "prerelease", "downgrade"} {
		t.Run(scenario, func(t *testing.T) {
			metadata := Contract("1.2.3")
			encoded, _ := json.Marshal(metadata)
			executable := "#!/bin/sh\nprintf '%s\\n' '" + string(encoded) + "'\n"
			sums := fmt.Sprintf("%s  install.sh\n%s  release.txt\n%s  data-mate_darwin_arm64\n", digest(nil), digest([]byte(metadata.Text())), digest([]byte(executable)))
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
					for _, name := range []string{"install.sh", "release.txt", "SHA256SUMS", "data-mate_darwin_arm64"} {
						assets = append(assets, map[string]string{"name": name, "browser_download_url": "https://github.com/" + Repository + "/releases/download/v1.2.3/" + name})
					}
					raw, _ := json.Marshal(map[string]any{"tag_name": "v1.2.3", "draft": false, "prerelease": scenario == "prerelease", "assets": assets})
					body = string(raw)
				} else {
					if !strings.HasPrefix(req.URL.Path, "/"+Repository+"/releases/download/v1.2.3/") {
						t.Fatal("mixed release versions", req.URL)
					}
					switch filepath.Base(req.URL.Path) {
					case "release.txt":
						body = metadata.Text()
					case "SHA256SUMS":
						body = sums
					case "data-mate_darwin_arm64":
						body = executable
						if scenario == "checksum" {
							body += "#tampered"
						}
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
