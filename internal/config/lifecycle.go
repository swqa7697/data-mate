package config

import (
	"context"
	"errors"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// LifecycleLease serializes process lifecycle and binary publication without
// holding state access while a child process is starting or shutting down.
type LifecycleLease struct{ lease *Lease }

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
	l.lifecycle, err = s.lock(ctx, "state/lifecycle.lock", true, false)
	if err != nil {
		return nil, err
	}
	if err = l.check(); err != nil {
		return nil, err
	}
	return &LifecycleLease{l}, nil
}

// Release ends lifecycle access and is idempotent.
func (l *LifecycleLease) Release() { l.lease.Release() }

// Identity returns the installation binding held by the lease.
func (l *LifecycleLease) Identity() Identity { return l.lease.Identity() }

// Read reads a bounded owned document without following links.
func (l *LifecycleLease) Read(path string, limit int) ([]byte, error) {
	return l.lease.Read(path, limit)
}

// Replace publishes only lifecycle-owned records. It cannot mutate profiles.
func (l *LifecycleLease) Replace(path string, b []byte) error {
	if (path != "state/service.json" && path != "state/service.plist" && path != "state/registrations.json") || len(b) > 16384 {
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
	if base != "state/service.json" && base != "state/service.plist" && base != "state/registrations.json" {
		return ErrOwnership
	}
	if err := l.lease.check(); err != nil {
		return err
	}
	return l.lease.store.remove(path)
}

// InstallBinary atomically publishes a freshly built private executable sibling.
// The build runs before acquiring lifecycle, so a failed build preserves output.
func (l *LifecycleLease) InstallBinary(name string) error {
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
	fd, err := unix.Openat(int(s.dir.Fd()), "bin", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return ErrOwnership
	}
	dir := os.NewFile(uintptr(fd), "bin")
	defer dir.Close()
	if checkFD(fd, true) != nil || !sameNamed(int(s.dir.Fd()), "bin", dir) {
		return ErrOwnership
	}
	check := func(n string, optional bool) (*os.File, error) {
		f, e := unix.Openat(fd, n, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
		if optional && errors.Is(e, unix.ENOENT) {
			return nil, nil
		}
		if e != nil {
			return nil, ErrOwnership
		}
		file := os.NewFile(uintptr(f), n)
		var st unix.Stat_t
		if unix.Fstat(f, &st) != nil || st.Uid != uint32(os.Geteuid()) || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&07777 != 0700 || st.Nlink != 1 || !sameNamed(fd, n, file) {
			file.Close()
			return nil, ErrOwnership
		}
		return file, nil
	}
	source, err := check(name, false)
	if err != nil {
		return err
	}
	defer source.Close()
	target, err := check("data-mate", true)
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
	if !sameNamed(int(s.dir.Fd()), "bin", dir) || !sameNamed(fd, name, source) {
		return ErrStale
	}
	if err = unix.Renameat(fd, name, fd, "data-mate"); err != nil {
		return errors.New("cannot publish executable")
	}
	return dir.Sync()
}
