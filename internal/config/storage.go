package config

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/swqa7697/data-mate/internal/contracts"
	"golang.org/x/sys/unix"
)

var (
	ErrOwnership = errors.New("installation ownership or file safety check failed")
	ErrStale     = errors.New("installation changed; reopen before retrying")
	ErrPurging   = errors.New("installation purge is pending")
	ErrRevision  = errors.New("profiles changed; preview again")
)

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// ValidUUID accepts canonical lowercase UUIDs used in persisted identities.
func ValidUUID(s string) bool { return uuidPattern.MatchString(s) }

// NewID generates a random version-4 UUID without exposing entropy errors.
func NewID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", errors.New("cannot generate identity")
	}
	b[6] = b[6]&15 | 64
	b[8] = b[8]&63 | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}

// Identity is nonsecret authority for this root. Purging is a durable tombstone:
// ordinary work cannot resume after key deletion even if cleanup was interrupted.
type Identity struct {
	Version             int      `json:"version"`
	ID                  string   `json:"installation_id"`
	RootDigest          string   `json:"root_digest"`
	ProfilesEstablished bool     `json:"profiles_established"`
	Purging             bool     `json:"purging"`
	Owned               []string `json:"owned"`
}

var ownedPaths = []string{
	"config/connections.json", "config/connections.json.tmp",
	"state/installation.json", "state/installation.json.tmp",
	"state/lifecycle.lock", "state/state-gate.lock", "state/state.lock",
	"state/vault-usage.json", "state/vault-usage.json.tmp",
	"state/vault.json", "state/vault.json.tmp",
	"config/known_hosts", "config/known_hosts.tmp",
}

// Fault is an optional test seam called before/after durability boundaries. It
// receives only an operation and owned relative path, never file contents.
type Fault func(operation, path string) error

// Store pins an owner-checked installation root. Close only after all leases end.
// Use Open for initialization; merely resolving a root never creates state.
type Store struct {
	root     Root
	dir      *os.File
	identity Identity
	local    *localLocks
	fault    Fault
}

// OpenExisting pins initialized state without creating files or migrating an
// inventory. Interactive catalog browsing uses it so cancellation cannot leave
// initialization artifacts. A state lease must still validate each snapshot.
func OpenExisting(ctx context.Context, root Root) (_ *Store, err error) {
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	actual, err := ResolveRoot(root.Path, "")
	if err != nil || actual != root {
		return nil, ErrOwnership
	}
	fd, err := unix.Open(root.Path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, ErrOwnership
	}
	s := &Store{root: root, dir: os.NewFile(uintptr(fd), root.Path), local: retainLocks(root.Path)}
	defer func() {
		if err != nil {
			s.Close()
		}
	}()
	if err = s.validRoot(); err != nil {
		return nil, err
	}
	raw, err := s.read("state/installation.json", 8192)
	if err != nil {
		return nil, err
	}
	if err = decodeIdentity(raw, root, &s.identity); err != nil {
		return nil, err
	}
	if s.identity.Purging {
		return nil, ErrPurging
	}
	return s, nil
}

// Open initializes private state under the lifecycle lock, or verifies an existing
// identity. A failed initialization is retryable; it never creates a vault key.
func Open(ctx context.Context, root Root, fault Fault) (_ *Store, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	actual, err := ResolveRoot(root.Path, "")
	if err != nil || actual != root {
		return nil, ErrOwnership
	}
	fd, err := unix.Open(root.Path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, ErrOwnership
	}
	s := &Store{root: root, dir: os.NewFile(uintptr(fd), root.Path), fault: fault, local: retainLocks(root.Path)}
	defer func() {
		if err != nil {
			s.Close()
		}
	}()
	if err = checkFD(fd, true); err != nil {
		return nil, err
	}
	for _, name := range []string{"state", "config"} {
		if e := unix.Mkdirat(fd, name, 0700); e != nil && !errors.Is(e, unix.EEXIST) {
			return nil, ErrOwnership
		}
		d, e := s.directory(name)
		if e != nil {
			return nil, e
		}
		d.Close()
	}
	if err = s.dir.Sync(); err != nil {
		return nil, errors.New("cannot sync installation root")
	}
	unlock, err := s.local.lifecycle.acquire(ctx, true)
	if err != nil {
		return nil, err
	}
	defer unlock()
	_, identityErr := s.read("state/installation.json", 8192)
	if identityErr != nil && !errors.Is(identityErr, os.ErrNotExist) {
		return nil, identityErr
	}
	lifecycle, err := s.lock(ctx, "state/lifecycle.lock", true, errors.Is(identityErr, os.ErrNotExist))
	if err != nil {
		return nil, err
	}
	defer releaseFile(lifecycle)
	raw, err := s.read("state/installation.json", 8192)
	if errors.Is(err, os.ErrNotExist) {
		// Existing secret state without identity must never receive a new namespace.
		for _, p := range []string{"state/vault.json", "state/vault-usage.json"} {
			if _, e := s.read(p, 8<<20); !errors.Is(e, os.ErrNotExist) {
				return nil, ErrOwnership
			}
		}
		id, e := NewID()
		if e != nil {
			return nil, e
		}
		s.identity = Identity{Version: 1, ID: id, RootDigest: root.Digest, Owned: slices.Clone(ownedPaths)}
		if err = s.writeIdentity(); err != nil {
			return nil, err
		}
	} else {
		if err != nil {
			return nil, err
		}
		if err = decodeIdentity(raw, root, &s.identity); err != nil {
			return nil, err
		}
		// Upgrade only the exact pre-P6 inventory, under the lifecycle lease.
		// Identity, credential namespace and existing owned data remain intact.
		if !slices.Equal(s.identity.Owned, ownedPaths) {
			s.identity.Owned = slices.Clone(ownedPaths)
			if err = s.writeIdentity(); err != nil {
				return nil, err
			}
		}
	}
	for _, p := range []string{"state/state-gate.lock", "state/state.lock"} {
		flags := unix.O_RDWR
		if !s.identity.ProfilesEstablished {
			flags |= unix.O_CREAT
		}
		f, e := s.openFile(p, flags, 0600)
		if e != nil {
			return nil, e
		}
		f.Close()
	}
	d, err := s.directory("state")
	if err != nil {
		return nil, err
	}
	err = d.Sync()
	d.Close()
	if err != nil {
		return nil, errors.New("cannot sync lock directory")
	}
	if !s.identity.ProfilesEstablished && !s.identity.Purging {
		if _, e := s.read("config/connections.json", MaxProfileBytes); errors.Is(e, os.ErrNotExist) {
			if err = s.replace("config/connections.json", []byte(`{"version":1,"connections":[]}`)); err != nil {
				return nil, err
			}
		} else if e != nil {
			return nil, e
		}
		s.identity.ProfilesEstablished = true
		if err = s.writeIdentity(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func decodeIdentity(raw []byte, root Root, id *Identity) error {
	if err := DecodeStrict(raw, 8192, id); err != nil {
		return ErrOwnership
	}
	if id.Version != 1 || !ValidUUID(id.ID) || id.RootDigest != root.Digest || (!slices.Equal(id.Owned, ownedPaths) && !slices.Equal(id.Owned, ownedPaths[:len(ownedPaths)-2])) {
		return ErrOwnership
	}
	return nil
}

// DecodeStrict is the bounded decoder for internal versioned documents. Required
// fields and semantic bounds must additionally be checked by the caller.
func DecodeStrict(raw []byte, limit int, dst any) error {
	b, err := contracts.JSON(bytes.NewReader(raw), limit)
	if err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return errors.New("invalid persisted document")
	}
	// null is not a document, and null members are not omission/default values.
	var members map[string]json.RawMessage
	if err := json.Unmarshal(b, &members); err != nil || members == nil {
		return errors.New("invalid persisted object")
	}
	typ := reflect.TypeOf(dst).Elem()
	for i := 0; i < typ.NumField(); i++ {
		tag := typ.Field(i).Tag.Get("json")
		name, options, _ := strings.Cut(tag, ",")
		if name != "" && name != "-" && !strings.Contains(options, "omitempty") && members[name] == nil {
			return errors.New("missing persisted document member")
		}
	}
	var value any
	if err := json.Unmarshal(b, &value); err != nil || containsNull(value) {
		return errors.New("null persisted document member")
	}
	return nil
}
func containsNull(v any) bool {
	if v == nil {
		return true
	}
	switch x := v.(type) {
	case map[string]any:
		for _, e := range x {
			if containsNull(e) {
				return true
			}
		}
	case []any:
		for _, e := range x {
			if containsNull(e) {
				return true
			}
		}
	}
	return false
}

func (s *Store) writeIdentity() error {
	b, err := json.Marshal(s.identity)
	if err != nil {
		return err
	}
	return s.replace("state/installation.json", b)
}

// Close releases the root descriptor. Leases must be released first.
func (s *Store) Close() error { releaseLocks(s.root.Path, s.local); return s.dir.Close() }

func checkFD(fd int, directory bool) error {
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || st.Uid != uint32(os.Geteuid()) {
		return ErrOwnership
	}
	kind, mode := uint16(unix.S_IFREG), uint16(0600)
	if directory {
		kind, mode = unix.S_IFDIR, 0700
	}
	if st.Mode&unix.S_IFMT != kind || st.Mode&07777 != mode || (!directory && st.Nlink != 1) {
		return ErrOwnership
	}
	return nil
}
func sameNamed(parent int, name string, file *os.File) bool {
	var opened, named unix.Stat_t
	return unix.Fstat(int(file.Fd()), &opened) == nil && unix.Fstatat(parent, name, &named, unix.AT_SYMLINK_NOFOLLOW) == nil && opened.Dev == named.Dev && opened.Ino == named.Ino
}
func (s *Store) validRoot() error {
	if !sameNamed(unix.AT_FDCWD, s.root.Path, s.dir) || checkFD(int(s.dir.Fd()), true) != nil {
		return ErrStale
	}
	return nil
}
func (s *Store) directory(name string) (*os.File, error) {
	if name != "state" && name != "config" {
		return nil, ErrOwnership
	}
	if err := s.validRoot(); err != nil {
		return nil, err
	}
	fd, err := unix.Openat(int(s.dir.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, ErrOwnership
	}
	f := os.NewFile(uintptr(fd), name)
	if checkFD(fd, true) != nil || !sameNamed(int(s.dir.Fd()), name, f) {
		f.Close()
		return nil, ErrOwnership
	}
	return f, nil
}
func splitOwned(path string) (string, string, error) {
	if !slices.Contains(ownedPaths, path) {
		return "", "", ErrOwnership
	}
	dir, name, _ := strings.Cut(path, "/")
	return dir, name, nil
}
func (s *Store) openFile(path string, flags int, mode uint32) (*os.File, error) {
	dirname, name, err := splitOwned(path)
	if err != nil {
		return nil, err
	}
	dir, err := s.directory(dirname)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	fd, err := unix.Openat(int(dir.Fd()), name, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, mode)
	if errors.Is(err, unix.ENOENT) {
		return nil, os.ErrNotExist
	}
	if err != nil {
		return nil, ErrOwnership
	}
	f := os.NewFile(uintptr(fd), name)
	if checkFD(fd, false) != nil || !sameNamed(int(dir.Fd()), name, f) {
		f.Close()
		return nil, ErrOwnership
	}
	return f, nil
}
func (s *Store) read(path string, limit int) ([]byte, error) {
	f, err := s.openFile(path, unix.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() > int64(limit) {
		return nil, errors.New("persisted document exceeds limit")
	}
	b, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil || len(b) > limit {
		return nil, errors.New("cannot read bounded document")
	}
	return b, nil
}
func (s *Store) point(op, path string) error {
	if s.fault != nil {
		return s.fault(op, path)
	}
	return nil
}

// replace only publishes nonsecret metadata or ciphertext. No plaintext secret
// ever reaches this API from the vault. An error after rename is indeterminate;
// callers must stop and reload rather than roll back another durable document.
func (s *Store) replace(path string, b []byte) error {
	dirname, name, err := splitOwned(path)
	if err != nil {
		return err
	}
	dir, err := s.directory(dirname)
	if err != nil {
		return err
	}
	defer dir.Close()
	old, err := s.openFile(path, unix.O_RDONLY, 0)
	if err == nil {
		old.Close()
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	// A fixed, inventoried sibling is safe because all writers hold the stable
	// exclusive lease (identity publication holds lifecycle). Recover only this
	// exact owned file after a crash, never arbitrary prefix-matched files.
	tmp := name + ".tmp"
	previousTemp, tempErr := s.openFile(path+".tmp", unix.O_RDONLY, 0)
	if tempErr == nil {
		if !sameNamed(int(dir.Fd()), tmp, previousTemp) {
			previousTemp.Close()
			return ErrStale
		}
		previousTemp.Close()
		if err := unix.Unlinkat(int(dir.Fd()), tmp, 0); err != nil {
			return errors.New("cannot recover publication sibling")
		}
	} else if !errors.Is(tempErr, os.ErrNotExist) {
		return tempErr
	}

	fd, err := unix.Openat(int(dir.Fd()), tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return errors.New("cannot create publication file")
	}
	f := os.NewFile(uintptr(fd), tmp)
	defer func() { f.Close(); _ = unix.Unlinkat(int(dir.Fd()), tmp, 0) }()
	if _, err = f.Write(b); err != nil {
		return errors.New("cannot write publication file")
	}
	if err = s.point("before-file-sync", path); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return errors.New("cannot sync publication file")
	}
	if err = s.point("after-file-sync", path); err != nil {
		return err
	}
	if err = s.validRoot(); err != nil {
		return err
	}
	if !sameNamed(int(s.dir.Fd()), dirname, dir) {
		return ErrStale
	}
	// Recheck the target immediately before replacement; never follow links.
	old, err = s.openFile(path, unix.O_RDONLY, 0)
	if err == nil {
		old.Close()
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err = s.point("before-rename", path); err != nil {
		return err
	}
	if err = unix.Renameat(int(dir.Fd()), tmp, int(dir.Fd()), name); err != nil {
		return errors.New("cannot publish document")
	}
	if err = s.point("after-rename", path); err != nil {
		return err
	}
	if err = s.point("before-directory-sync", path); err != nil {
		return err
	}
	if err = dir.Sync(); err != nil {
		return errors.New("cannot sync publication directory")
	}
	return s.point("after-directory-sync", path)
}
func (s *Store) remove(path string) error {
	dirname, name, err := splitOwned(path)
	if err != nil {
		return err
	}
	dir, err := s.directory(dirname)
	if err != nil {
		return err
	}
	defer dir.Close()
	f, err := s.openFile(path, unix.O_RDONLY, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	if !sameNamed(int(dir.Fd()), name, f) || !sameNamed(int(s.dir.Fd()), dirname, dir) {
		return ErrStale
	}
	if err := s.point("before-unlink", path); err != nil {
		return err
	}
	if err := unix.Unlinkat(int(dir.Fd()), name, 0); err != nil {
		return errors.New("cannot remove owned document")
	}
	if err := s.point("after-unlink", path); err != nil {
		return err
	}
	if err := s.point("before-directory-sync", path); err != nil {
		return err
	}
	if err := dir.Sync(); err != nil {
		return errors.New("cannot sync cleanup")
	}
	return s.point("after-directory-sync", path)
}

func (s *Store) verifyIdentity() error {
	b, err := s.read("state/installation.json", 8192)
	if err != nil {
		return ErrStale
	}
	var current Identity
	if decodeIdentity(b, s.root, &current) != nil || current.ID != s.identity.ID {
		return ErrStale
	}
	if current.Purging {
		return ErrPurging
	}
	if !current.ProfilesEstablished {
		return ErrStale
	}
	return nil
}

// ProfileSnapshot freshly validates profiles. Missing established files fail.
func (l *Lease) ProfileSnapshot() (Profiles, Revision, error) {
	b, err := l.Read("config/connections.json", MaxProfileBytes)
	if err != nil {
		return Profiles{}, "", err
	}
	return DecodeProfiles(bytes.NewReader(b))
}

// SaveProfiles publishes only if the preview revision still matches. The vault
// orchestrator is responsible for publishing credentials first.
func (l *Lease) SaveProfiles(p Profiles, expected Revision) (Revision, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return "", errors.New("invalid profiles")
	}
	canonical, revision, err := DecodeProfiles(bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	_, current, err := l.ProfileSnapshot()
	if err != nil {
		return "", err
	}
	if current != expected {
		return "", ErrRevision
	}
	b, err = json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	return revision, l.Replace("config/connections.json", b)
}

// Path returns the canonical root for nonsecret diagnostics and owned fixtures.
func (s *Store) Path() string { return filepath.Clean(s.root.Path) }

func encodeIdentity(id Identity) ([]byte, error) { return json.Marshal(id) }
