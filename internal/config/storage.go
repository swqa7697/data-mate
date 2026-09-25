package config

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
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
	ErrOwnership     = errors.New("installation ownership or file safety check failed")
	ErrStale         = errors.New("installation changed; reopen before retrying")
	ErrPending       = errors.New("distribution operation is pending; rerun installer or cleanup")
	ErrPurging       = errors.New("installation purge is pending")
	ErrCommitUnknown = errors.New("SQLite commit outcome uncertain; read current state before retrying")
	ErrObsolete      = errors.New("obsolete development state; preserve it and recreate this installation with re-entered credentials")
	ErrRecovery      = errors.New("SQLite recovery required; run an explicit management operation")
	ErrState         = errors.New("invalid SQLite installation state")
	ErrRevision      = errors.New("profiles changed; preview again")
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
	Version             int         `json:"version"`
	Environment         Environment `json:"environment,omitempty"`
	ID                  string      `json:"installation_id"`
	RootDigest          string      `json:"root_digest"`
	ProfilesEstablished bool        `json:"profiles_established"`
	Purging             bool        `json:"purging"`
	Pending             bool        `json:"pending,omitempty"`
	Owned               []string    `json:"owned"`
	KeyAccount          string      `json:"key_account"`
	Executable          string      `json:"executable,omitempty"`
}

var ownedPaths = []string{
	"data-mate.db", "data-mate.db-journal",
	"installation.json", "installation.json.tmp",
	"lifecycle.lock", "state-gate.lock", "state.lock",
	"known_hosts", "known_hosts.tmp",
	"service.json", "service.json.tmp",
	"service.plist", "service.plist.tmp",
	"registrations.json", "registrations.json.tmp",
	"purge.json", "purge.json.tmp",
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
	terminal bool // Final purge receipt supplies authority after identity removal.
}

// OpenExisting pins initialized state without creating files or migrating an
// inventory. Interactive catalog browsing uses it so cancellation cannot leave
// initialization artifacts. A state lease must still validate each snapshot.
func OpenExisting(ctx context.Context, root Root) (*Store, error) {
	return openExisting(ctx, root, false)
}

// OpenLifecycle permits an existing purge tombstone for stop/cleanup only.
// Ordinary state leases still reject the tombstone. It creates no files.
func OpenLifecycle(ctx context.Context, root Root) (*Store, error) {
	return openExisting(ctx, root, true)
}

// readIdentity recognizes only an authenticated-by-ownership terminal receipt.
// It permits cleanup retry after identity/lock removal, never ordinary startup.
func (s *Store) readIdentity() ([]byte, bool, error) {
	b, err := s.read("installation.json", 8192)
	if !errors.Is(err, os.ErrNotExist) {
		return b, false, err
	}
	b, err = s.read("purge.json", 8192)
	if err != nil {
		return nil, false, err
	}
	var id Identity
	if decodeIdentity(b, s.root, &id) != nil || !id.Purging {
		return nil, false, ErrOwnership
	}
	return b, true, nil
}

func openExisting(ctx context.Context, root Root, cleanup bool) (_ *Store, err error) {
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if root.Environment.Kind() == Production {
		if _, e := os.Lstat(root.Path); errors.Is(e, os.ErrNotExist) {
			return nil, os.ErrNotExist
		}
	}
	if validateRoot(root) != nil {
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
	raw, terminal, err := s.readIdentity()
	s.terminal = terminal
	if err != nil {
		return nil, err
	}
	if err = decodeIdentity(raw, root, &s.identity); err != nil {
		return nil, err
	}
	if s.identity.Pending && !cleanup {
		return nil, ErrPending
	}
	if s.identity.Purging && !cleanup {
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
	if validateRoot(root) != nil {
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
	if err = s.checkObsolete(); err != nil {
		return nil, err
	}
	if err = s.dir.Sync(); err != nil {
		return nil, errors.New("cannot sync installation root")
	}
	unlock, err := s.local.lifecycle.acquire(ctx, true)
	if err != nil {
		return nil, err
	}
	defer unlock()
	_, identityErr := s.read("installation.json", 8192)
	if identityErr != nil && !errors.Is(identityErr, os.ErrNotExist) {
		return nil, identityErr
	}
	lifecycle, err := s.lock(ctx, "lifecycle.lock", true, errors.Is(identityErr, os.ErrNotExist))
	if err != nil {
		return nil, err
	}
	defer releaseFile(lifecycle)
	// A final purge receipt blocks initialization even after identity removal.
	if _, e := s.read("purge.json", 8192); e == nil {
		return nil, ErrPurging
	} else if !errors.Is(e, os.ErrNotExist) {
		return nil, e
	}
	raw, err := s.read("installation.json", 8192)
	if errors.Is(err, os.ErrNotExist) {
		var existing unix.Stat_t
		if e := unix.Fstatat(int(s.dir.Fd()), databasePath, &existing, unix.AT_SYMLINK_NOFOLLOW); !errors.Is(e, unix.ENOENT) {
			return nil, ErrOwnership
		}

		id, e := NewID()
		if e != nil {
			return nil, e
		}
		sum := sha256.Sum256([]byte(root.Digest + ":" + id))
		s.identity = Identity{Version: 3, Environment: root.Environment.Kind(), ID: id, RootDigest: root.Digest, KeyAccount: hex.EncodeToString(sum[:]), Owned: slices.Clone(ownedPaths)}
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
		// Upgrade only an exact historical inventory, under the lifecycle lease.
		// Identity, credential namespace and existing owned data remain intact.
		if s.identity.Version == 2 || !slices.Equal(s.identity.Owned, ownedPaths) {
			s.identity.Version = 3
			s.identity.Environment = root.Environment.Kind()
			s.identity.Owned = slices.Clone(ownedPaths)
			if err = s.writeIdentity(); err != nil {
				return nil, err
			}
		}
	}
	for _, p := range []string{"state-gate.lock", "state.lock"} {
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
	d, err := s.directory(".")
	if err != nil {
		return nil, err
	}
	err = d.Sync()
	d.Close()
	if err != nil {
		return nil, errors.New("cannot sync lock directory")
	}
	if !s.identity.Purging {
		var journal unix.Stat_t
		journalErr := unix.Fstatat(int(s.dir.Fd()), databasePath+"-journal", &journal, unix.AT_SYMLINK_NOFOLLOW)
		if !s.identity.ProfilesEstablished || !errors.Is(journalErr, unix.ENOENT) {
			if err = s.initializeDatabase(ctx); err != nil {
				return nil, err
			}
		}
		if !s.identity.ProfilesEstablished {
			s.identity.ProfilesEstablished = true
			if err = s.writeIdentity(); err != nil {
				return nil, err
			}
		}
	}

	return s, nil
}

func decodeIdentity(raw []byte, root Root, id *Identity) error {
	if err := DecodeStrict(raw, 8192, id); err != nil {
		return ErrOwnership
	}
	if id.Executable != "" && (id.Executable != ExecutablePath(root) || strings.ContainsAny(id.Executable, "\x00\n\r\t")) {
		return ErrOwnership
	}
	if id.Version != 2 && id.Version != 3 {
		return ErrObsolete
	}
	if id.Environment.Kind() != root.Environment.Kind() || (id.Version == 2 && id.Environment != "") || (id.Version == 3 && id.Environment == "") {
		return ErrOwnership
	}
	sum := sha256.Sum256([]byte(root.Digest + ":" + id.ID))
	if !ValidUUID(id.ID) || id.RootDigest != root.Digest || id.KeyAccount != hex.EncodeToString(sum[:]) || !slices.Equal(id.Owned, ownedPaths) {
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
	return s.replace("installation.json", b)
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
	if name != "." {
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
	return ".", path, nil
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
	b, _, err := s.readIdentity()
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
	if current.Pending {
		return ErrPending
	}
	if !current.ProfilesEstablished {
		return ErrStale
	}
	return nil
}

// Path returns the canonical root for nonsecret diagnostics and owned fixtures.
func (s *Store) Path() string { return filepath.Clean(s.root.Path) }

func encodeIdentity(id Identity) ([]byte, error) { return json.Marshal(id) }
