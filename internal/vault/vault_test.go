package vault

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
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
	"github.com/tink-crypto/tink-go/v2/aead"
	"github.com/tink-crypto/tink-go/v2/insecurecleartextkeyset"
	"github.com/tink-crypto/tink-go/v2/keyset"
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
		for _, key := range f.keys.keys {
			if len(key) > 0 && bytes.Contains(b, key) {
				return errors.New("keyset material on disk")
			}
		}
		if bytes.Contains(b, []byte(syntheticPassword)) {
			return errors.New("plaintext credential on disk")
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
}

// Extends the existing transaction scenario to atomic SQLite records, cached
// Tink handles, credential-only generations, and deletion without key access.
func TestVaultTransactions(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	p, rev := snapshot(t, f.repo)
	if _, err := f.repo.Apply(ctx, Mutation{Expected: rev, Profiles: p}); err != nil {
		t.Fatal(err)
	}
	if f.keys.loads+f.keys.creates != 0 {
		t.Fatal("empty operation accessed OS store")
	}
	_, private, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(private)
	pin := &transport.HostKey{Address: "jump.invalid:22", Key: signer.PublicKey()}
	p, rev = snapshot(t, f.repo)
	f.fault = func(op, path string) error {
		if op == "before-rename" && path == "known_hosts" {
			return errors.New("host publication failure")
		}
		return nil
	}
	if out, e := f.repo.Apply(ctx, Mutation{Expected: rev, Profiles: p, HostKey: pin}); e == nil || out.ProfilesSaved {
		t.Fatal("host failure committed profile")
	}
	f.fault = nil
	first := addSecret(t, f, 1)
	second := addSecret(t, f, 2)
	if f.keys.creates != 1 || len(f.keys.keys) != 1 {
		t.Fatal("multiple OS items")
	}
	calls := f.keys.loads + f.keys.creates
	f.keys.failure = ErrDenied
	for _, c := range []config.Profile{first, second} {
		s, e := credential(t, f, c.ID)
		if e != nil || s.Password != syntheticPassword {
			t.Fatal("cached decryption", e)
		}
	}
	p, rev = snapshot(t, f.repo)
	p.Connections[0].Alias = "renamed"
	if _, e := f.repo.Apply(ctx, Mutation{Expected: rev, Profiles: p}); e != nil {
		t.Fatal(e)
	}
	p, rev = snapshot(t, f.repo)
	if _, e := f.repo.Apply(ctx, Mutation{Expected: rev, Profiles: p, Patches: map[string]Patch{first.ID: {"password": "replacement"}}}); e != nil {
		t.Fatal(e)
	}
	if f.keys.loads+f.keys.creates != calls {
		t.Fatal("unlocked edits accessed native store")
	}
	if _, e := f.repo.Apply(ctx, Mutation{Expected: rev, Profiles: p}); !errors.Is(e, config.ErrRevision) {
		t.Fatal("credential edit did not invalidate preview", e)
	}
	s, e := credential(t, f, first.ID)
	if e != nil || s.Password != "replacement" {
		t.Fatal("patch", e)
	}
	restarted := New(f.store, f.keys)
	if e = restarted.Unlock(ctx, false, ""); !errors.Is(e, ErrDenied) {
		t.Fatal("restart bypassed denial", e)
	}
	f.keys.failure = nil
	if e = restarted.Unlock(ctx, false, ""); e != nil {
		t.Fatal(e)
	}
	restarted.Close()
	f.keys.failure = ErrDenied
	p, rev = snapshot(t, f.repo)
	p.Connections = []config.Profile{}
	if _, e = f.repo.Apply(ctx, Mutation{Expected: rev, Profiles: p}); e != nil {
		t.Fatal("deletion required unlock", e)
	}
	if len(f.keys.keys) != 1 {
		t.Fatal("last deletion reset keyset")
	}
	assertNoPlaintext(t, f)
	// Aggregate ciphertext limits are enforced by the publishing transaction.
	// Three worst-case escaped bundles exceed 8 MiB without exceeding field limits.
	f.keys.failure = nil
	huge := strings.Repeat("\x01", MaxSecretBytes)
	oversized, oldRevision := snapshot(t, f.repo)
	replacements := map[string]Secrets{}
	for i := 0; i < 3; i++ {
		c := testProfile(t, 100+i)
		oversized.Connections = append(oversized.Connections, c)
		replacements[c.ID] = Secrets{Password: huge, SSHPassword: huge, SSHPrivateKey: huge, SSHKeyPassphrase: huge, ProxyPassword: huge}
	}
	if _, e := f.repo.Apply(ctx, Mutation{Expected: oldRevision, Profiles: oversized, Replacements: replacements}); e == nil {
		t.Fatal("aggregate ciphertext limit bypassed")
	}
	if current, revision := snapshot(t, f.repo); len(current.Connections) != 0 || revision != oldRevision {
		t.Fatal("oversized transaction partially committed")
	}
	f.keys.failure = ErrDenied
	sentinel := filepath.Join(f.root.Path, "unrelated")
	if e = os.WriteFile(sentinel, []byte("keep"), 0600); e != nil {
		t.Fatal(e)
	}
	if e = f.repo.PurgeCredentials(ctx); !errors.Is(e, ErrDenied) {
		t.Fatal("purge denial", e)
	}
	if _, e = os.Stat(filepath.Join(f.root.Path, "data-mate.db")); e != nil {
		t.Fatal("denial lost database")
	}
	if _, _, e = f.repo.Snapshot(ctx); !errors.Is(e, config.ErrPurging) {
		t.Fatal("purge admission", e)
	}
	f.keys.failure = nil
	if e = f.repo.PurgeCredentials(ctx); e != nil {
		t.Fatal(e)
	}
	if e = f.repo.PurgeCredentials(ctx); e != nil {
		t.Fatal("idempotent purge", e)
	}
	if len(f.keys.keys) != 0 {
		t.Fatal("key remains")
	}
	if _, e = os.Stat(sentinel); e != nil {
		t.Fatal("unrelated removed")
	}
}
func sqlFixture(t *testing.T, f *fixture, fn func(*sql.DB)) {
	t.Helper()
	l, e := f.store.WriteLease(t.Context())
	if e != nil {
		t.Fatal(e)
	}
	defer l.Release()
	db, e := sql.Open("sqlite3", filepath.Join(f.root.Path, "data-mate.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	fn(db)
}
func bundleBytes(t *testing.T, f *fixture, ref string) []byte {
	t.Helper()
	l, e := f.store.ReadLease(t.Context())
	if e != nil {
		t.Fatal(e)
	}
	defer l.Release()
	b, e := l.Bundle(ref)
	if e != nil {
		t.Fatal(e)
	}
	return b.Data
}
func setBundle(t *testing.T, f *fixture, ref string, b []byte) {
	t.Helper()
	sqlFixture(t, f, func(db *sql.DB) {
		if _, e := db.Exec("UPDATE bundles SET ciphertext=? WHERE reference=?", b, ref); e != nil {
			t.Fatal(e)
		}
	})
}
func keyMetadata(t *testing.T, f *fixture) config.KeysetMetadata {
	t.Helper()
	l, e := f.store.ReadLease(t.Context())
	if e != nil {
		t.Fatal(e)
	}
	defer l.Release()
	k, e := l.Keyset()
	if e != nil {
		t.Fatal(e)
	}
	return k
}

func TestVaultRejectsUnsafeState(t *testing.T) {
	f := newFixture(t)
	c := addSecret(t, f, 1)
	original := bundleBytes(t, f, c.CredentialRef)
	k := keyMetadata(t, f)
	for _, bad := range []error{ErrDenied, ErrLocked, ErrUnavailable, errors.New(syntheticPassword)} {
		f.keys.failure = bad
		r := New(f.store, f.keys)
		e := r.Unlock(t.Context(), false, "")
		r.Close()
		if e == nil || strings.Contains(e.Error(), syntheticPassword) {
			t.Fatal("provider failure leaked or passed")
		}
	}
	f.keys.failure = nil
	for _, bad := range [][]byte{bytes.Repeat([]byte{9}, 32), nil} {
		good := f.keys.keys[k.Account]
		f.keys.keys[k.Account] = bad
		r := New(f.store, f.keys)
		if e := r.Unlock(t.Context(), false, ""); e == nil {
			t.Fatal("invalid keyset accepted")
		}
		r.Close()
		f.keys.keys[k.Account] = good
	}
	for i := range []int{0, 5, len(original) - 1} {
		bad := bytes.Clone(original)
		bad[i] ^= 1
		setBundle(t, f, c.CredentialRef, bad)
		if _, e := credential(t, f, c.ID); e == nil {
			t.Fatal("tampered ciphertext accepted", i)
		}
		if !bytes.Equal(bad, bundleBytes(t, f, c.CredentialRef)) {
			t.Fatal("tamper deleted")
		}
	}
	setBundle(t, f, c.CredentialRef, original)
	// Authenticated malformed plaintext remains rejected after Tink authentication.
	for i, plain := range []string{`{}`, `{"version":2,"secrets":{}}`, `{"version":1,"secrets":null}`, `{"version":1,"secrets":{},"extra":true}`, `{"version":1,"version":1,"secrets":{}}`, `{"version":1,"secrets":{"unknown":"x"}}`, `{"version":1,"secrets":{"password":null}}`, `{"version":1,"secrets":{"password":"` + strings.Repeat("x", MaxSecretBytes+1) + `"}}`} {
		l, e := f.store.ReadLease(t.Context())
		if e != nil {
			t.Fatal(e)
		}
		id := l.Identity()
		primitive, e := f.repo.cipher(l)
		l.Release()
		if e != nil {
			t.Fatal(e)
		}
		encrypted, e := primitive.Encrypt([]byte(plain), aad(id, c, 1))
		if e != nil {
			t.Fatal(e)
		}
		setBundle(t, f, c.CredentialRef, encrypted)
		if _, e = credential(t, f, c.ID); e == nil {
			t.Fatal("invalid authenticated bundle", i)
		}
	}
	setBundle(t, f, c.CredentialRef, original)
	// Each authenticated context component rejects substitution independently.
	l, e := f.store.ReadLease(t.Context())
	if e != nil {
		t.Fatal(e)
	}
	id := l.Identity()
	primitive, e := f.repo.cipher(l)
	l.Release()
	if e != nil {
		t.Fatal(e)
	}
	var contextFields []string
	_ = json.Unmarshal(aad(id, c, 1), &contextFields)
	validPlain := []byte(`{"version":1,"secrets":{"password":"synthetic-context"}}`)
	for i := range contextFields {
		fields := append([]string(nil), contextFields...)
		fields[i] += "-changed"
		wrong, _ := json.Marshal(fields)
		encrypted, e := primitive.Encrypt(validPlain, wrong)
		if e != nil {
			t.Fatal(e)
		}
		setBundle(t, f, c.CredentialRef, encrypted)
		if _, e = credential(t, f, c.ID); e == nil {
			t.Fatal("context substitution accepted", i)
		}
	}
	setBundle(t, f, c.CredentialRef, original)
	// A valid Tink AES128 keyset is still outside the installation's allowed format.
	handle, e := keyset.NewHandle(aead.AES128GCMKeyTemplate())
	if e != nil {
		t.Fatal(e)
	}
	var serialized bytes.Buffer
	if e = insecurecleartextkeyset.Write(handle, keyset.NewBinaryWriter(&serialized)); e != nil {
		t.Fatal(e)
	}
	if _, e = parseKeyset(serialized.Bytes()); !errors.Is(e, ErrRepair) {
		t.Fatal("unsupported Tink keyset accepted", e)
	}
	clear(serialized.Bytes())
	second := addSecret(t, f, 2)
	setBundle(t, f, second.CredentialRef, original)
	if _, e := credential(t, f, second.ID); e == nil {
		t.Fatal("cross-record replay")
	}
	sqlFixture(t, f, func(db *sql.DB) {
		if _, e := db.Exec("UPDATE keyset SET reserved=1000000"); e != nil {
			t.Fatal(e)
		}
	})
	p, rev := snapshot(t, f.repo)
	if _, e := f.repo.Apply(t.Context(), Mutation{Expected: rev, Profiles: p, Replacements: map[string]Secrets{c.ID: {Password: "replacement"}}}); !errors.Is(e, ErrLimit) {
		t.Fatal("exhaustion", e)
	}
	if _, e := credential(t, f, c.ID); e != nil {
		t.Fatal("exhaustion blocked reads", e)
	}
	sqlFixture(t, f, func(db *sql.DB) {
		if _, e := db.Exec("DELETE FROM keyset"); e != nil {
			t.Fatal(e)
		}
	})
	if _, e := credential(t, f, c.ID); e == nil {
		t.Fatal("missing accounting accepted")
	}
	if !bytes.Equal(original, bundleBytes(t, f, c.CredentialRef)) {
		t.Fatal("failure changed ciphertext")
	}
	assertNoPlaintext(t, f)
}

func TestVaultCrashRecovery(t *testing.T) {
	if os.Getenv("DATA_MATE_VAULT_CRASH") == "1" {
		crashChild(t)
		return
	}
	for _, point := range []string{"before-key-create", "after-key-create", "after-reservation", "after-encryption", "before-commit", "after-commit", "after-profile-publication"} {
		f := newFixture(t)
		p, rev := snapshot(t, f.repo)
		c := testProfile(t, 1)
		p.Connections = append(p.Connections, c)
		hit := false
		fault := func(op string) error {
			if op == point && !hit {
				hit = true
				return errors.New("interrupted")
			}
			return nil
		}
		f.repo.fault = fault
		f.fault = func(op, path string) error {
			if path == "data-mate.db" {
				return fault(op)
			}
			return nil
		}
		if _, e := f.repo.Apply(t.Context(), Mutation{Expected: rev, Profiles: p, Replacements: map[string]Secrets{c.ID: {Password: syntheticPassword}}}); e == nil || !hit {
			t.Fatal("missing boundary", point, e)
		}
		f.fault = nil
		f.repo.Close()
		f.repo = New(f.store, f.keys)
		before := keyMetadata(t, f)
		p, rev = snapshot(t, f.repo)
		if len(p.Connections) == 0 {
			p.Connections = append(p.Connections, c)
		}
		if _, e := f.repo.Apply(t.Context(), Mutation{Expected: rev, Profiles: p, Replacements: map[string]Secrets{c.ID: {Password: syntheticPassword}}}); e != nil {
			t.Fatal("recovery", point, e)
		}
		after := keyMetadata(t, f)
		if before.Fingerprint == after.Fingerprint && after.Reserved <= before.Reserved {
			t.Fatal("reservation reused", point)
		}
		if s, e := credential(t, f, c.ID); e != nil || s.Password != syntheticPassword {
			t.Fatal("recovered credential", point, e)
		}
		assertNoPlaintext(t, f)
	}
	// Abrupt exit during a SQLite transaction exercises actual hot-journal recovery.
	for _, point := range []string{"before-commit", "after-commit"} {
		f := newFixture(t)
		c := addSecret(t, f, 1)
		k := keyMetadata(t, f)
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestVaultCrashRecovery$")
		cmd.Env = append(os.Environ(), "DATA_MATE_VAULT_CRASH=1", "DATA_MATE_CRASH_ROOT="+f.root.Path, "DATA_MATE_CRASH_POINT="+point)
		cmd.Stdin = bytes.NewReader(f.keys.keys[k.Account])
		out, e := cmd.CombinedOutput()
		cancel()
		var exit *exec.ExitError
		if !errors.As(e, &exit) || exit.ExitCode() != 77 {
			t.Fatalf("crash %s: %v %s", point, e, out)
		}
		reopened, e := config.Open(t.Context(), f.root, nil)
		if e != nil {
			t.Fatal(e)
		}
		reopened.Close()
		if s, e := credential(t, f, c.ID); e != nil || s.Password != syntheticPassword {
			t.Fatal("crash lost credential", point, e)
		}
	}
}

func TestVaultCleanupAndConcurrentWriters(t *testing.T) {
	testPurgeRecovery(t)
	f := newFixture(t)
	addSecret(t, f, 99)
	p, rev := snapshot(t, f.repo)
	p.Connections = []config.Profile{}
	f.fault = func(op, path string) error {
		if op == "before-commit" && path == "data-mate.db" {
			return errors.New("failed deletion")
		}
		return nil
	}
	if _, e := f.repo.Apply(t.Context(), Mutation{Expected: rev, Profiles: p}); e == nil {
		t.Fatal("missing failure")
	}
	f.fault = nil
	if current, _ := snapshot(t, f.repo); len(current.Connections) != 1 {
		t.Fatal("failed delete changed profile")
	}
	if _, e := f.repo.Apply(t.Context(), Mutation{Expected: rev, Profiles: p}); e != nil {
		t.Fatal(e)
	}
	const count = 16
	var wg sync.WaitGroup
	errs := make(chan error, count)
	for i := 0; i < count; i++ {
		c := testProfile(t, i)
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
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
		t.Fatal("lost concurrent mutations")
	}
	for _, c := range p.Connections {
		if s, e := credential(t, f, c.ID); e != nil || s.Password != syntheticPassword {
			t.Fatal(e)
		}
	}
	p, rev = snapshot(t, f.repo)
	p.Connections = []config.Profile{}
	f.keys.failure = ErrDenied
	if _, e := f.repo.Apply(t.Context(), Mutation{Expected: rev, Profiles: p}); e != nil {
		t.Fatal("deletion accessed denied native store", e)
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
		if _, err := os.Stat(filepath.Join(f.root.Path, "data-mate.db")); err != nil {
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
	for _, path := range []string{"data-mate.db"} {
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
	key, err := io.ReadAll(io.LimitReader(os.Stdin, MaxKeysetBytes+1))
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	root, err := config.ResolveRoot(os.Getenv("DATA_MATE_CRASH_ROOT"), "")
	if err != nil {
		t.Fatal(err)
	}
	store, err := config.Open(context.Background(), root, func(op, path string) error {
		if path == "data-mate.db" && op == os.Getenv("DATA_MATE_CRASH_POINT") {
			os.Exit(77)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	lease, e := store.ReadLease(t.Context())
	if e != nil {
		t.Fatal(e)
	}
	account := lease.Identity().KeyAccount
	lease.Release()
	keys := &fakeKeys{keys: map[string][]byte{account: key}}
	repo := New(store, keys)
	p, rev := snapshot(t, repo)
	_, err = repo.Apply(context.Background(), Mutation{Expected: rev, Profiles: p, Replacements: map[string]Secrets{p.Connections[0].ID: {Password: syntheticPassword}}})
	t.Fatalf("crash boundary not reached: %v", err)
}

func FuzzVaultDocument(f *testing.F) {
	f.Add([]byte(`{"version":1,"secrets":{}}`))
	f.Add([]byte(`{"version":1,"version":1,"secrets":{}}`))
	f.Add([]byte(`{"version":1,"secrets":{"password":"synthetic"}}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > 4096 {
			return
		}
		var decoded bundle
		if config.DecodeStrict(b, MaxPlaintextBytes, &decoded) != nil {
			return
		}
		raw, e := json.Marshal(decoded)
		if e != nil {
			t.Fatal(e)
		}
		var roundtrip bundle
		if config.DecodeStrict(raw, MaxPlaintextBytes, &roundtrip) != nil {
			t.Fatal("accepted bundle did not roundtrip")
		}
	})
}
