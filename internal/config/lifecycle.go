package config

import (
	"context"
	"errors"
	"strings"

	"golang.org/x/sys/unix"
)

// LifecycleLease serializes process lifecycle and binary publication without
// holding state access while a child process is starting or shutting down.
type LifecycleLease struct {
	lease    *Lease
	identity Identity
}

// Lifecycle acquires only lifecycle. State leases, if needed, must follow it.
// Purging installations remain inspectable/stoppable for cleanup retries.
func (s *Store) Lifecycle(ctx context.Context) (_ *LifecycleLease, err error) {
	l := &Lease{store: s, purge: true}
	defer func() {
		if err != nil {
			l.Release()
		}
	}()
	l.unlockLifecycle, err = s.local.lifecycle.acquire(ctx, true)
	if err != nil {
		return nil, err
	}
	l.lifecycle, err = s.lock(ctx, "lifecycle.lock", true, s.terminal)
	if err != nil {
		return nil, err
	}
	if err = l.check(); err != nil {
		return nil, err
	}
	// OpenLifecycle may have read identity before another process began purge.
	// Capture current flags only after acquiring the lifecycle lock.
	raw, _, err := s.readIdentity()
	if err != nil {
		return nil, err
	}
	var current Identity
	if decodeIdentity(raw, s.root, &current) != nil || current.ID != s.identity.ID {
		return nil, ErrStale
	}
	return &LifecycleLease{lease: l, identity: current}, nil
}

// Release ends lifecycle access and is idempotent.
func (l *LifecycleLease) Release() { l.lease.Release() }

// Identity returns the installation binding held by the lease.
func (l *LifecycleLease) Identity() Identity {
	id := l.identity
	id.Owned = append([]string(nil), id.Owned...)
	return id
}

// Read reads a bounded owned document without following links.
func (l *LifecycleLease) Read(path string, limit int) ([]byte, error) {
	return l.lease.Read(path, limit)
}

// Replace publishes only lifecycle-owned records. It cannot mutate profiles.
func (l *LifecycleLease) Replace(path string, b []byte) error {
	if (path != "service.json" && path != "service.plist" && path != "registrations.json") || len(b) > 16384 {
		return ErrOwnership
	}
	if err := l.lease.check(); err != nil {
		return err
	}
	return l.lease.store.replace(path, b)
}

// Remove removes only lifecycle-owned records or their publication siblings.
func (l *LifecycleLease) Remove(path string) error {
	base := strings.TrimSuffix(path, ".tmp")
	if base != "service.json" && base != "service.plist" && base != "registrations.json" {
		return ErrOwnership
	}
	if err := l.lease.check(); err != nil {
		return err
	}
	return l.lease.store.remove(path)
}

// InstallBinary atomically publishes a freshly built private executable sibling.
// The build runs before acquiring lifecycle, so a failed build preserves output.
func (l *LifecycleLease) InstallBinary(ctx context.Context, name string) error {
	if !strings.HasPrefix(name, ".data-mate.") || strings.ContainsAny(name, "/\\") {
		return ErrOwnership
	}
	s := l.lease.store
	if err := l.lease.check(); err != nil {
		return err
	}
	if err := s.verifyIdentity(); err != nil {
		return err
	}
	d, err := s.binaryDirectory()
	if err != nil {
		return err
	}
	defer d.close()
	dir := d.dir
	fd := int(dir.Fd())
	source, err := checkBinary(fd, name, false)
	if err != nil {
		return err
	}
	defer source.Close()
	target, err := checkBinary(fd, "data-mate", true)
	if err != nil {
		return err
	}
	if target != nil {
		defer target.Close()
	}
	if err = source.Sync(); err != nil {
		return err
	}
	if err = l.lease.check(); err != nil {
		return err
	}
	if d.check() != nil || !sameNamed(fd, name, source) {
		return ErrStale
	}
	// Persist cleanup authority before publishing the executable. A failed rename
	// can be retried; it never leaves an unrecorded installed executable.
	state, err := l.CleanupLease(ctx)
	if err != nil {
		return err
	}
	defer state.Release()
	if err = ctx.Err(); err != nil {
		return err
	}
	s.identity.Executable = DevelopmentExecutable(s.root)
	if err = s.writeIdentity(); err != nil {
		return err
	}
	l.identity = s.identity
	if err = state.check(); err != nil {
		return err
	}
	if d.check() != nil || !sameNamed(fd, name, source) {
		return ErrStale
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if target != nil {
		if !sameNamed(fd, "data-mate", target) {
			return ErrStale
		}
	} else {
		var st unix.Stat_t
		if e := unix.Fstatat(fd, "data-mate", &st, unix.AT_SYMLINK_NOFOLLOW); !errors.Is(e, unix.ENOENT) {
			return ErrStale
		}
	}
	if err = unix.Renameat(fd, name, fd, "data-mate"); err != nil {
		return errors.New("cannot publish executable")
	}
	return dir.Sync()
}
