package vault

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/transport"
	"golang.org/x/crypto/ssh"
)

const syntheticPassword = "SYNTHETIC-P1-password-never-persist-plaintext"

type fakeKeys struct {
	mu                      sync.Mutex
	keys                    map[string][]byte
	failure                 error
	creates, loads, deletes int
}

func (f *fakeKeys) Load(ctx context.Context, a string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loads++
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	if f.failure != nil {
		return nil, f.failure
	}
	v, ok := f.keys[a]
	if !ok {
		return nil, ErrMissing
	}
	return bytes.Clone(v), nil
}
func (f *fakeKeys) CreateIfAbsent(ctx context.Context, a string, k []byte) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates++
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	if f.failure != nil {
		return nil, f.failure
	}
	if v, ok := f.keys[a]; ok {
		return bytes.Clone(v), nil
	}
	f.keys[a] = bytes.Clone(k)
	return bytes.Clone(k), nil
}
func (f *fakeKeys) Delete(ctx context.Context, a string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes++
	if e := ctx.Err(); e != nil {
		return e
	}
	if f.failure != nil {
		return f.failure
	}
	delete(f.keys, a)
	return nil
}

type fixture struct {
	root  config.Root
	store *config.Store
	repo  *Repository
	keys  *fakeKeys
	fault config.Fault
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	path := t.TempDir()
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := config.ResolveRoot(path, "")
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{root: root, keys: &fakeKeys{keys: map[string][]byte{}}}
	f.store, err = config.Open(context.Background(), root, func(op, path string) error {
		if f.fault != nil {
			return f.fault(op, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	f.repo = New(f.store, f.keys)
	t.Cleanup(func() { f.store.Close() })
	return f
}
func testProfile(t *testing.T, n int) config.Profile {
	t.Helper()
	id, err := config.NewID()
	if err != nil {
		t.Fatal(err)
	}
	return config.Profile{ID: id, Alias: fmt.Sprintf("connection-%d", n), Driver: "postgres", Connection: config.Connection{Host: "127.0.0.1", Port: 5432, Database: "fixture", Username: "reader"}, Transport: config.Transport{TLS: config.TLS{Mode: "disabled"}}, Scope: config.Scope{Mode: "all"}}
}
func snapshot(t *testing.T, r *Repository) (config.Profiles, config.Revision) {
	t.Helper()
	p, rev, e := r.Snapshot(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	return p, rev
}
func addSecret(t *testing.T, f *fixture, n int) config.Profile {
	t.Helper()
	p, rev := snapshot(t, f.repo)
	c := testProfile(t, n)
	p.Connections = append(p.Connections, c)
	o, e := f.repo.Apply(context.Background(), Mutation{Expected: rev, Profiles: p, Replacements: map[string]Secrets{c.ID: {Password: syntheticPassword}}})
	if e != nil || !o.ProfilesSaved {
		t.Fatal("save", e)
	}
	p, _ = snapshot(t, f.repo)
	for _, c2 := range p.Connections {
		if c2.ID == c.ID {
			return c2
		}
	}
	t.Fatal("missing profile")
	return c
}
func credential(t *testing.T, f *fixture, id string) (Secrets, error) {
	t.Helper()
	l, e := f.store.ReadLease(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	defer l.Release()
	return f.repo.Credential(context.Background(), l, id)
}
func readOwned(t *testing.T, f *fixture, path string) []byte {
	t.Helper()
	b, e := os.ReadFile(filepath.Join(f.root.Path, path))
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func overwrite(t *testing.T, f *fixture, path string, b []byte) {
	t.Helper()
	if e := os.WriteFile(filepath.Join(f.root.Path, path), b, 0600); e != nil {
		t.Fatal(e)
	}
}
func assertNoPlaintext(t *testing.T, f *fixture) {
	t.Helper()
	if e := filepath.WalkDir(f.root.Path, func(path string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		b, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		if bytes.Contains(b, []byte(syntheticPassword)) {
			return errors.New("plaintext credential on disk")
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
}

// No pre-P1 regression owns encryption or a provider. This scenario covers
// single-key multi-profile transactions, revision conflicts and exact-key purge.
func TestVaultTransactions(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	p, rev := snapshot(t, f.repo)
	if f.keys.loads != 0 {
		t.Fatal("listing touched key store")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, e := f.repo.Apply(canceled, Mutation{Expected: rev, Profiles: p}); !errors.Is(e, context.Canceled) {
		t.Fatal("canceled write", e)
	}
	if f.keys.creates != 0 {
		t.Fatal("cancel created key")
	}
	// Enrollment is part of the confirmed writer transaction. A host publication
	// failure must precede profile publication, and stale previews cannot add pins.
	_, private, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	signer, e := ssh.NewSignerFromKey(private)
	if e != nil {
		t.Fatal(e)
	}
	pin := &transport.HostKey{Address: "jump.invalid:22", Key: signer.PublicKey()}
	f.fault = func(op, path string) error {
		if op == "before-rename" && path == "config/known_hosts" {
			return errors.New("injected host publication failure")
		}
		return nil
	}
	if out, e := f.repo.Apply(ctx, Mutation{Expected: rev, Profiles: config.Profiles{Version: 1, Connections: []config.Profile{testProfile(t, 99)}}, HostKey: pin}); e == nil || out.ProfilesSaved {
		t.Fatal("host failure published profile")
	}
	f.fault = nil
	if current, _ := snapshot(t, f.repo); len(current.Connections) != 0 {
		t.Fatal("profile changed before pin publication")
	}
	if _, e = f.repo.Apply(ctx, Mutation{Expected: rev, Profiles: p, HostKey: pin}); e != nil {
		t.Fatal(e)
	}
	first := addSecret(t, f, 1)
	second := addSecret(t, f, 2)
	if f.keys.creates != 1 || len(f.keys.keys) != 1 {
		t.Fatal("per-profile keys")
	}
	for _, c := range []config.Profile{first, second} {
		s, e := credential(t, f, c.ID)
		if e != nil || s.Password != syntheticPassword {
			t.Fatal("credential", e)
		}
	}
	p, rev = snapshot(t, f.repo)
	p.Connections[0].Alias = "renamed"
	if _, e := f.repo.Apply(ctx, Mutation{Expected: rev, Profiles: p}); e != nil {
		t.Fatal(e)
	}
	before := readOwned(t, f, "state/vault.json")
	if _, e := f.repo.Apply(ctx, Mutation{Expected: rev, Profiles: p, Replacements: map[string]Secrets{first.ID: {Password: "stale"}}}); !errors.Is(e, config.ErrRevision) {
		t.Fatal("stale preview", e)
	}
	if !bytes.Equal(before, readOwned(t, f, "state/vault.json")) {
		t.Fatal("stale preview changed vault")
	}
	beforeHosts := readOwned(t, f, "config/known_hosts")
	if _, e := f.repo.Apply(ctx, Mutation{Expected: rev, Profiles: p, HostKey: &transport.HostKey{Address: "other.invalid:22", Key: signer.PublicKey()}}); !errors.Is(e, config.ErrRevision) {
		t.Fatal("stale enrollment accepted", e)
	}
	if !bytes.Equal(beforeHosts, readOwned(t, f, "config/known_hosts")) {
		t.Fatal("stale enrollment changed host pins")
	}
	// A fresh Store represents restart; the same key and identities must decrypt.
	restarted, e := config.Open(ctx, f.root, nil)
	if e != nil {
		t.Fatal(e)
	}
	lease, e := restarted.ReadLease(ctx)
	if e != nil {
		t.Fatal(e)
	}
	s, e := New(restarted, f.keys).Credential(ctx, lease, first.ID)
	lease.Release()
	restarted.Close()
	if e != nil || s.Password != syntheticPassword {
		t.Fatal("restart", e)
	}
	p, rev = snapshot(t, f.repo)
	p.Connections = []config.Profile{}
	if _, e := f.repo.Apply(ctx, Mutation{Expected: rev, Profiles: p}); e != nil {
		t.Fatal("remove all", e)
	}
	if len(f.keys.keys) != 1 {
		t.Fatal("last removal reset key")
	}
	var ledger usage
	if json.Unmarshal(readOwned(t, f, "state/vault-usage.json"), &ledger) != nil || ledger.Reserved < 3 {
		t.Fatal("accounting lost")
	}
	assertNoPlaintext(t, f)
	sentinel := filepath.Join(f.root.Path, "unrelated")
	if e := os.WriteFile(sentinel, []byte("keep"), 0600); e != nil {
		t.Fatal(e)
	}
	f.keys.failure = ErrDenied
	if e := f.repo.PurgeCredentials(ctx); !errors.Is(e, ErrDenied) {
		t.Fatal("purge denial", e)
	}
	if _, e := os.Stat(filepath.Join(f.root.Path, "state/vault-usage.json")); e != nil {
		t.Fatal("denial removed retry accounting")
	}
	if _, _, e := f.repo.Snapshot(ctx); !errors.Is(e, config.ErrPurging) {
		t.Fatal("purge did not revoke admission", e)
	}
	f.keys.failure = nil
	if e := f.repo.PurgeCredentials(ctx); e != nil {
		t.Fatal(e)
	}
	if e := f.repo.PurgeCredentials(ctx); e != nil {
		t.Fatal("purge retry", e)
	}
	for _, p := range []string{"state/vault.json", "state/vault-usage.json", "config/connections.json"} {
		if _, e := os.Stat(filepath.Join(f.root.Path, p)); !errors.Is(e, os.ErrNotExist) {
			t.Fatal("purge artifact", p, e)
		}
	}
	if len(f.keys.keys) != 0 {
		t.Fatal("key remains")
	}
	if _, e := os.Stat(sentinel); e != nil {
		t.Fatal("unrelated file removed")
	}
}

// Tamper, accounting loss, provider failure, and binding errors preserve bytes.
func TestVaultRejectsUnsafeState(t *testing.T) {
	f := newFixture(t)
	c := addSecret(t, f, 1)
	original := readOwned(t, f, "state/vault.json")
	accounting := readOwned(t, f, "state/vault-usage.json")
	for _, bad := range []error{ErrDenied, ErrLocked, ErrUnavailable, errors.New(syntheticPassword)} {
		f.keys.failure = bad
		if _, e := credential(t, f, c.ID); e == nil || strings.Contains(e.Error(), syntheticPassword) {
			t.Fatal("provider failure leaked or passed")
		}
		if !bytes.Equal(original, readOwned(t, f, "state/vault.json")) {
			t.Fatal("provider error mutated vault")
		}
	}
	f.keys.failure = nil
	key := bytes.Clone(f.keys.keys[f.root.Digest])
	f.keys.keys[f.root.Digest] = bytes.Repeat([]byte{9}, 32)
	if _, e := credential(t, f, c.ID); !errors.Is(e, ErrRepair) {
		t.Fatal("wrong key accepted", e)
	}
	delete(f.keys.keys, f.root.Digest)
	if _, e := credential(t, f, c.ID); !errors.Is(e, ErrRepair) {
		t.Fatal("missing key replaced", e)
	}
	f.keys.keys[f.root.Digest] = key
	mutations := []func(*envelope){func(e *envelope) { e.Ciphertext[0] ^= 1 }, func(e *envelope) { e.Nonce[0] ^= 1 }, func(e *envelope) { e.WriteSequence++ }, func(e *envelope) { e.VaultID = "aaaaaaaa-0000-0000-0000-000000000000" }, func(e *envelope) { e.Version = 2 }, func(e *envelope) { e.Nonce = e.Nonce[:11] }}
	for i, mutate := range mutations {
		var e envelope
		if json.Unmarshal(original, &e) != nil {
			t.Fatal("fixture")
		}
		mutate(&e)
		bad, _ := json.Marshal(e)
		overwrite(t, f, "state/vault.json", bad)
		if _, e := credential(t, f, c.ID); !errors.Is(e, ErrRepair) {
			t.Fatal("tamper accepted", i, e)
		}
		if !bytes.Equal(bad, readOwned(t, f, "state/vault.json")) {
			t.Fatal("tamper overwritten", i)
		}
	}
	overwrite(t, f, "state/vault.json", original)
	for _, bad := range [][]byte{[]byte(`{}`), []byte(`{"version":1,"version":1}`), bytes.Replace(accounting, []byte(`"reserved":1`), []byte(`"reserved":null`), 1)} {
		overwrite(t, f, "state/vault-usage.json", bad)
		if _, e := credential(t, f, c.ID); !errors.Is(e, ErrRepair) {
			t.Fatal("invalid accounting accepted", e)
		}
	}
	overwrite(t, f, "state/vault-usage.json", accounting)
	if e := os.Remove(filepath.Join(f.root.Path, "state/vault-usage.json")); e != nil {
		t.Fatal(e)
	}
	if _, e := credential(t, f, c.ID); !errors.Is(e, ErrRepair) {
		t.Fatal("missing ledger accepted", e)
	}
	overwrite(t, f, "state/vault-usage.json", accounting)

	for i, plain := range []string{
		`{"version":2,"bundles":{}}`,
		`{"version":1,"bundles":null}`,
		`{"version":1,"bundles":{},"unexpected":true}`,
		`{"version":1,"bundles":{},"version":1}`,
		`{"version":1,"bundles":{"` + c.CredentialRef + `":{"connection_id":"` + c.ID + `"}}}`,
		`{"version":1,"bundles":{"` + c.CredentialRef + `":{"connection_id":"` + c.ID + `","secrets":{"unknown":"synthetic"}}}}`,
		`{"version":1,"bundles":{"` + c.CredentialRef + `":{"connection_id":"` + c.ID + `","secrets":{"password":"` + strings.Repeat("x", MaxSecretBytes+1) + `"}}}}`,
	} {
		bad := malformedCiphertext(t, f, original, []byte(plain))
		overwrite(t, f, "state/vault.json", bad)
		if _, err := credential(t, f, c.ID); !errors.Is(err, ErrRepair) {
			t.Fatal("invalid decrypted schema", i, err)
		}
	}
	overwrite(t, f, "state/vault.json", original)
	// Manual profiles remain equivalent, but a reference cannot cross identities.
	p, rev := snapshot(t, f.repo)
	p.Connections[0].ID = "aaaaaaaa-0000-0000-0000-000000000000"
	if _, e := f.repo.Apply(context.Background(), Mutation{Expected: rev, Profiles: p}); !errors.Is(e, ErrBinding) {
		t.Fatal("bundle rebound", e)
	}
	raw, _ := json.Marshal(p)
	overwrite(t, f, "config/connections.json", raw)
	if _, e := credential(t, f, p.Connections[0].ID); !errors.Is(e, ErrBinding) {
		t.Fatal("manual bundle rebound", e)
	}
	p.Connections[0].CredentialRef = "bbbbbbbb-0000-0000-0000-000000000000"
	raw, _ = json.Marshal(p)
	overwrite(t, f, "config/connections.json", raw)
	if _, e := credential(t, f, p.Connections[0].ID); !errors.Is(e, ErrCredentialMissing) {
		t.Fatal("missing bundle", e)
	}
	// Exhaustion preserves decryption and listing while refusing a new Seal.
	var u usage
	_ = json.Unmarshal(accounting, &u)
	u.Reserved = MaxReservations
	raw, _ = json.Marshal(u)
	overwrite(t, f, "state/vault-usage.json", raw)
	p, rev = snapshot(t, f.repo)
	if _, e := f.repo.Apply(context.Background(), Mutation{Expected: rev, Profiles: p, Replacements: map[string]Secrets{p.Connections[0].ID: {Password: "replacement"}}}); !errors.Is(e, ErrLimit) {
		t.Fatal("exhausted accounting accepted", e)
	}
	if !bytes.Equal(original, readOwned(t, f, "state/vault.json")) {
		t.Fatal("failure changed vault")
	}
	assertNoPlaintext(t, f)
}

// Every filesystem durability boundary is injected during initial save and edit.
// This loop corpus is counted separately in retained evidence, never hidden as a
// claimed reduction in work. No existing scenario covers crash publication order.
func TestVaultCrashRecovery(t *testing.T) {
	if os.Getenv("DATA_MATE_VAULT_CRASH") == "1" {
		crashChild(t)
		return
	}

	points := []string{"before-file-sync", "after-file-sync", "before-rename", "after-rename", "before-directory-sync", "after-directory-sync"}
	for _, initial := range []bool{true, false} {
		for _, path := range []string{"state/vault-usage.json", "state/vault.json", "config/connections.json"} {
			for _, point := range points {
				f := newFixture(t)
				var c config.Profile
				if !initial {
					c = addSecret(t, f, 1)
				} else {
					c = testProfile(t, 1)
				}
				p, rev := snapshot(t, f.repo)
				if initial {
					p.Connections = append(p.Connections, c)
				}
				hit := false
				f.fault = func(op, p string) error {
					if !hit && op == point && p == path {
						hit = true
						return errors.New("synthetic crash")
					}
					return nil
				}
				_, e := f.repo.Apply(context.Background(), Mutation{Expected: rev, Profiles: p, Replacements: map[string]Secrets{c.ID: {Password: syntheticPassword}}})
				if e == nil || !hit {
					t.Fatal(initial, path, point, "missed injection")
				}
				f.fault = nil
				// Reopen/reload rather than reuse a pre-failure counter or ciphertext.
				reopened, e := config.Open(context.Background(), f.root, nil)
				if e != nil {
					t.Fatal(point, e)
				}
				repo := New(reopened, f.keys)
				current, currentRev := snapshot(t, repo)
				if len(current.Connections) > 0 {
					l, e := reopened.ReadLease(context.Background())
					if e != nil {
						t.Fatal(e)
					}
					s, e := repo.Credential(context.Background(), l, c.ID)
					l.Release()
					if e != nil || s.Password != syntheticPassword {
						t.Fatal(initial, path, point, "published dangling reference", e)
					}
				}
				if len(current.Connections) == 0 {
					current.Connections = append(current.Connections, c)
				}
				if _, e := repo.Apply(context.Background(), Mutation{Expected: currentRev, Profiles: current, Replacements: map[string]Secrets{c.ID: {Password: syntheticPassword}}}); e != nil {
					t.Fatal(initial, path, point, "recovery", e)
				}
				reopened.Close()
				assertNoPlaintext(t, f)
			}
		}
	}
	// Abrupt process exit bypasses Go defers. Pass the fake test key through a
	// private pipe, never environment/arguments/files, and keep secrets synthetic.
	for _, point := range []string{"after-file-sync", "after-rename", "after-directory-sync"} {
		f := newFixture(t)
		c := addSecret(t, f, 1)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestVaultCrashRecovery$")
		cmd.Env = append(os.Environ(), "DATA_MATE_VAULT_CRASH=1", "DATA_MATE_CRASH_ROOT="+f.root.Path, "DATA_MATE_CRASH_POINT="+point)
		cmd.Stdin = bytes.NewReader(f.keys.keys[f.root.Digest])
		out, err := cmd.CombinedOutput()
		cancel()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 77 {
			t.Fatalf("abrupt crash %s: %v %s", point, err, out)
		}
		s, err := credential(t, f, c.ID)
		if err != nil || s.Password != syntheticPassword {
			t.Fatal("abrupt crash lost original credential", point, err)
		}
		p, rev := snapshot(t, f.repo)
		if _, err := f.repo.Apply(context.Background(), Mutation{Expected: rev, Profiles: p, Replacements: map[string]Secrets{c.ID: {Password: syntheticPassword}}}); err != nil {
			t.Fatal("abrupt retry", point, err)
		}
		assertNoPlaintext(t, f)
	}

	for _, point := range []string{"before-key-create", "after-key-create", "after-reservation", "after-encryption", "after-profile-publication"} {
		f := newFixture(t)
		p, rev := snapshot(t, f.repo)
		c := testProfile(t, 1)
		p.Connections = append(p.Connections, c)
		hit := false
		f.repo.fault = func(op string) error {
			if op == point && !hit {
				hit = true
				return errors.New("synthetic crash")
			}
			return nil
		}
		if _, e := f.repo.Apply(context.Background(), Mutation{Expected: rev, Profiles: p, Replacements: map[string]Secrets{c.ID: {Password: syntheticPassword}}}); e == nil || !hit {
			t.Fatal(point, "missing failure")
		}
		var reservedBefore usage
		if raw, readErr := os.ReadFile(filepath.Join(f.root.Path, "state/vault-usage.json")); readErr == nil {
			if err := json.Unmarshal(raw, &reservedBefore); err != nil {
				t.Fatal(err)
			}
		}
		f.repo.fault = nil
		p, rev = snapshot(t, f.repo)
		if len(p.Connections) == 0 {
			p.Connections = append(p.Connections, c)
		}
		if _, e := f.repo.Apply(context.Background(), Mutation{Expected: rev, Profiles: p, Replacements: map[string]Secrets{c.ID: {Password: syntheticPassword}}}); e != nil {
			t.Fatal(point, e)
		}

		var reservedAfter usage
		if err := json.Unmarshal(readOwned(t, f, "state/vault-usage.json"), &reservedAfter); err != nil {
			t.Fatal(err)
		}
		if reservedBefore.KeyFingerprint == reservedAfter.KeyFingerprint && reservedAfter.Reserved <= reservedBefore.Reserved {
			t.Fatal("retry reused a consumed reservation", point)
		}
		assertNoPlaintext(t, f)
	}
}

func TestVaultCleanupAndConcurrentWriters(t *testing.T) {
	testPurgeRecovery(t)
	f := newFixture(t)
	first := addSecret(t, f, 1)
	p, rev := snapshot(t, f.repo)
	p.Connections = []config.Profile{}
	hit := false
	f.fault = func(op, path string) error {
		if path == "state/vault.json" && op == "before-rename" && !hit {
			hit = true
			return errors.New("cleanup failed")
		}
		return nil
	}
	o, e := f.repo.Apply(context.Background(), Mutation{Expected: rev, Profiles: p})
	if e == nil || !o.ProfilesSaved || !o.CleanupPending {
		t.Fatal("removal status", o, e)
	}
	f.fault = nil
	p, rev = snapshot(t, f.repo)
	if len(p.Connections) != 0 {
		t.Fatal("failed cleanup restored profile")
	}
	if _, e := f.repo.Apply(context.Background(), Mutation{Expected: rev, Profiles: p}); e != nil {
		t.Fatal("orphan reconciliation", e)
	}
	if _, e := credential(t, f, first.ID); !errors.Is(e, ErrCredentialMissing) {
		t.Fatal("removed credential reachable", e)
	}
	const count = 16
	var wg sync.WaitGroup
	errs := make(chan error, count)
	for i := 0; i < count; i++ {
		c := testProfile(t, i)
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			for {
				p, rev, e := f.repo.Snapshot(ctx)
				if e != nil {
					errs <- e
					return
				}
				p.Connections = append(p.Connections, c)
				_, e = f.repo.Apply(ctx, Mutation{Expected: rev, Profiles: p, Replacements: map[string]Secrets{c.ID: {Password: syntheticPassword}}})
				if errors.Is(e, config.ErrRevision) {
					continue
				}
				errs <- e
				return
			}
		})
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	p, _ = snapshot(t, f.repo)
	if len(p.Connections) != count || f.keys.creates != 1 {
		t.Fatal("lost writers or extra keys")
	}
	for _, c := range p.Connections {
		s, e := credential(t, f, c.ID)
		if e != nil || s.Password != syntheticPassword {
			t.Fatal("concurrent credential", e)
		}
	}

	// Native denial during deletion cleanup must not resurrect the removed profile.
	p, rev = snapshot(t, f.repo)
	p.Connections = []config.Profile{}
	f.keys.failure = ErrDenied
	deniedOutcome, deniedErr := f.repo.Apply(context.Background(), Mutation{Expected: rev, Profiles: p})
	if deniedErr == nil || !deniedOutcome.ProfilesSaved || !deniedOutcome.CleanupPending {
		t.Fatal("denied cleanup lost partial outcome", deniedOutcome, deniedErr)
	}
	p, rev = snapshot(t, f.repo)
	if len(p.Connections) != 0 {
		t.Fatal("denial undid deletion")
	}
	f.keys.failure = nil
	if _, err := f.repo.Apply(context.Background(), Mutation{Expected: rev, Profiles: p}); err != nil {
		t.Fatal("denied cleanup retry", err)
	}
	assertNoPlaintext(t, f)
}

// Purge interruption corpus extends transaction recovery with exact-key deletion
// and unlink durability. Identity/ledger are retained until deletion succeeds.
func testPurgeRecovery(t *testing.T) {
	t.Helper()
	for _, point := range []string{"before-key-delete", "after-key-delete"} {
		f := newFixture(t)
		addSecret(t, f, 1)
		hit := false
		f.repo.fault = func(op string) error {
			if op == point && !hit {
				hit = true
				return errors.New("synthetic key boundary")
			}
			return nil
		}
		if err := f.repo.PurgeCredentials(context.Background()); err == nil || !hit {
			t.Fatal("missing purge injection", point)
		}
		if _, err := os.Stat(filepath.Join(f.root.Path, "state/vault-usage.json")); err != nil {
			t.Fatal("key boundary lost ledger", point, err)
		}
		f.repo.fault = nil
		reopened, err := config.Open(context.Background(), f.root, nil)
		if err != nil {
			t.Fatal(err)
		}
		err = New(reopened, f.keys).PurgeCredentials(context.Background())
		reopened.Close()
		if err != nil {
			t.Fatal("purge restart", point, err)
		}
	}
	for _, path := range []string{"config/connections.json", "state/vault.json", "state/vault-usage.json"} {
		for _, point := range []string{"before-unlink", "after-unlink", "before-directory-sync", "after-directory-sync"} {
			f := newFixture(t)
			addSecret(t, f, 1)
			hit := false
			f.fault = func(op, p string) error {
				if op == point && p == path && !hit {
					hit = true
					return errors.New("synthetic unlink failure")
				}
				return nil
			}
			if err := f.repo.PurgeCredentials(context.Background()); err == nil || !hit {
				t.Fatal("purge boundary not hit", path, point)
			}
			f.fault = nil
			if err := f.repo.PurgeCredentials(context.Background()); err != nil {
				t.Fatal("purge boundary retry", path, point, err)
			}
		}
	}
}

func crashChild(t *testing.T) {
	t.Helper()
	key := make([]byte, 32)
	if _, err := io.ReadFull(os.Stdin, key); err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	root, err := config.ResolveRoot(os.Getenv("DATA_MATE_CRASH_ROOT"), "")
	if err != nil {
		t.Fatal(err)
	}
	store, err := config.Open(context.Background(), root, func(op, path string) error {
		if path == "state/vault.json" && op == os.Getenv("DATA_MATE_CRASH_POINT") {
			os.Exit(77)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	keys := &fakeKeys{keys: map[string][]byte{root.Digest: key}}
	repo := New(store, keys)
	p, rev := snapshot(t, repo)
	_, err = repo.Apply(context.Background(), Mutation{Expected: rev, Profiles: p, Replacements: map[string]Secrets{p.Connections[0].ID: {Password: syntheticPassword}}})
	t.Fatalf("crash boundary not reached: %v", err)
}

// Only this helper deliberately produces authenticated malformed plaintext to
// exercise post-decryption validation; its key and all payloads are synthetic.
func malformedCiphertext(t *testing.T, f *fixture, original, plain []byte) []byte {
	t.Helper()
	var e envelope
	if err := json.Unmarshal(original, &e); err != nil {
		t.Fatal(err)
	}
	block, err := aes.NewCipher(f.keys.keys[f.root.Digest])
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	// Unique test nonce for each crafted document, never a production encryption.
	id, err := config.NewID()
	if err != nil {
		t.Fatal(err)
	}
	e.Nonce = []byte(id[:12])
	lease, err := f.store.ReadLease(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	identity := lease.Identity()
	lease.Release()
	e.Ciphertext = gcm.Seal(nil, e.Nonce, plain, aad(e, identity))
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func FuzzVaultDocument(f *testing.F) {
	f.Add([]byte(`{"version":1,"bundles":{}}`))
	f.Add([]byte(`{"version":1,"bundles":{},"version":1}`))
	f.Add([]byte(`{"version":1,"bundles":{"00000000-0000-0000-0000-000000000000":{"connection_id":"00000000-0000-0000-0000-000000000001","secrets":{"password":"synthetic"}}}}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > 4096 {
			return
		}
		var d document
		if config.DecodeStrict(b, MaxPlaintextBytes, &d) != nil || !validDocument(d) {
			return
		}
		canonical, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		var roundtrip document
		if config.DecodeStrict(canonical, MaxPlaintextBytes, &roundtrip) != nil || !validDocument(roundtrip) {
			t.Fatal("accepted vault cannot round trip")
		}
	})
}
