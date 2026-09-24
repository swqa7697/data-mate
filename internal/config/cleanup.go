package config

import (
	"context"
	"errors"
	"os"
	"slices"

	"golang.org/x/sys/unix"
)

// RemainingOwned inspects only inventoried names without requiring identity or
// following links. Missing identity cannot turn retained vault/binary files into
// a successful uninstall. Unknown files grant no removal authority.
func RemainingOwned(root Root) ([]string, error) {
	fd, err := unix.Open(root.Path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, ErrOwnership
	}
	s := &Store{root: root, dir: os.NewFile(uintptr(fd), root.Path)}
	defer s.dir.Close()
	if err = s.validRoot(); err != nil {
		return nil, err
	}
	remaining := []string{}
	for _, path := range ownedPaths {
		dirname, name, _ := splitOwned(path)
		var st unix.Stat_t
		if err = unix.Fstatat(fd, dirname, &st, unix.AT_SYMLINK_NOFOLLOW); errors.Is(err, unix.ENOENT) {
			continue
		}
		d, err := s.directory(dirname)
		if err != nil {
			return remaining, err
		}
		err = unix.Fstatat(int(d.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW)
		d.Close()
		if err == nil {
			remaining = append(remaining, path)
		} else if !errors.Is(err, unix.ENOENT) {
			return remaining, ErrOwnership
		}
	}
	return remaining, nil
}

// CleanupLease drains state access while the caller retains lifecycle. Purging
// permits recovery of locks removed by an interrupted terminal cleanup only.
// Release this lease before releasing the parent lifecycle lease.
func (l *LifecycleLease) CleanupLease(ctx context.Context) (_ *Lease, err error) {
	if err = l.lease.check(); err != nil {
		return nil, err
	}
	s := l.lease.store
	n := &Lease{store: s, write: true, purge: true, parent: l.lease, ctx: ctx}
	defer func() {
		if err != nil {
			n.Release()
		}
	}()
	n.unlockState, err = s.local.state.acquire(ctx, true)
	if err != nil {
		return nil, err
	}
	n.gate, err = s.lock(ctx, "state/state-gate.lock", true, l.identity.Purging)
	if err != nil {
		return nil, err
	}
	n.state, err = s.lock(ctx, "state/state.lock", true, l.identity.Purging)
	if err != nil {
		return nil, err
	}
	if err = n.check(); err != nil {
		return nil, err
	}
	// Refresh mutable identity only with exclusive state access, so readers of
	// this Store cannot race cleanup's inventory or purge-flag refresh.
	s.identity = l.Identity()
	// Adopt only the decoder-approved historical inventory, preserving identity.
	if !slices.Equal(s.identity.Owned, ownedPaths) {
		s.identity.Owned = slices.Clone(ownedPaths)
		if err = s.writeIdentity(); err != nil {
			return nil, err
		}
	}
	l.identity = s.identity
	return n, ctx.Err()
}

// RemoveBinary removes only the inventoried executable after external cleanup.
func (l *Lease) RemoveBinary() error {
	if l.parent == nil || !l.write {
		return ErrOwnership
	}
	if err := l.check(); err != nil {
		return err
	}
	return l.store.removeOptionalDirectoryFile("bin/data-mate")
}

func (s *Store) removeOptionalDirectoryFile(path string) error {
	dir, _, err := splitOwned(path)
	if err != nil {
		return err
	}
	var st unix.Stat_t
	if err = unix.Fstatat(int(s.dir.Fd()), dir, &st, unix.AT_SYMLINK_NOFOLLOW); errors.Is(err, unix.ENOENT) {
		return nil
	}
	return s.remove(path)
}

// FinishPurge removes owned data before identity and stable locks. The caller
// has already deleted the exact key and registrations and retains a helper
// outside the installation. Unknown files, including logs we never created,
// remain untouched. Missing locks are recoverable only with a purge tombstone.
func (l *Lease) FinishPurge() error {
	if l.parent == nil || !l.write || !l.store.identity.Purging {
		return ErrOwnership
	}
	if err := l.check(); err != nil {
		return err
	}
	s := l.store
	for _, path := range ownedPaths {
		switch path {
		case "state/installation.json", "state/lifecycle.lock", "state/state-gate.lock", "state/state.lock", "bin/data-mate", "state/purge.json", "state/purge.json.tmp":
			continue
		}
		if err := s.removeOptionalDirectoryFile(path); err != nil {
			return err
		}
	}
	if err := l.RemoveBinary(); err != nil {
		return err
	}
	// Remove directories before relinquishing identity; installers still wait
	// on lifecycle. Nonempty directories are unrelated user data, not errors.
	for _, name := range []string{"config", "bin"} {
		if err := s.removeEmptyDirectory(name); err != nil {
			return err
		}
	}
	// Keep a terminal receipt until every stable lock is gone. A process
	// interrupted after identity removal can still verify exactly this root.
	receipt, err := encodeIdentity(s.identity)
	if err != nil {
		return err
	}
	if err = s.replace("state/purge.json", receipt); err != nil {
		return err
	}
	// No credential or external authority remains beyond this point. Remove
	// identity after state locks so every interrupted prefix is reopenable.
	for _, path := range []string{"state/state.lock", "state/state-gate.lock", "state/installation.json", "state/lifecycle.lock"} {
		if err := s.remove(path); err != nil {
			return err
		}
	}
	if err := s.remove("state/purge.json.tmp"); err != nil {
		return err
	}
	if err := s.remove("state/purge.json"); err != nil {
		return err
	}
	if err := s.removeEmptyDirectory("state"); err != nil {
		return err
	}
	if err := s.validRoot(); err != nil {
		return err
	}
	err = unix.Rmdir(s.root.Path)
	if errors.Is(err, unix.ENOTEMPTY) || errors.Is(err, unix.EEXIST) {
		return nil
	}
	return err
}

func (s *Store) removeEmptyDirectory(name string) error {
	var st unix.Stat_t
	if err := unix.Fstatat(int(s.dir.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW); errors.Is(err, unix.ENOENT) {
		return nil
	}
	d, err := s.directory(name)
	if err != nil {
		return err
	}
	defer d.Close()
	if !sameNamed(int(s.dir.Fd()), name, d) {
		return ErrStale
	}
	err = unix.Unlinkat(int(s.dir.Fd()), name, unix.AT_REMOVEDIR)
	if errors.Is(err, unix.ENOTEMPTY) || errors.Is(err, unix.EEXIST) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.dir.Sync()
}

// TrimBinaryDirectory removes the now-empty binary directory on default uninstall.
func (l *Lease) TrimBinaryDirectory() error {
	if l.parent == nil {
		return ErrOwnership
	}
	if err := l.check(); err != nil {
		return err
	}
	return l.store.removeEmptyDirectory("bin")
}
