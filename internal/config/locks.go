package config

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// Each lease opens independent flock descriptors. The process-local queue also
// prevents newly arriving goroutines from starving a waiting writer.
type rwQueue struct {
	mu      sync.Mutex
	readers int
	writer  bool
	waiting int
	changed chan struct{}
}

func (q *rwQueue) signal() {
	if q.changed != nil {
		close(q.changed)
	}
	q.changed = make(chan struct{})
}
func (q *rwQueue) acquire(ctx context.Context, write bool) (func(), error) {
	q.mu.Lock()
	if q.changed == nil {
		q.changed = make(chan struct{})
	}
	if write {
		q.waiting++
	}
	for {
		if err := ctx.Err(); err != nil {
			if write {
				q.waiting--
				q.signal()
			}
			q.mu.Unlock()
			return nil, err
		}
		if !q.writer && ((write && q.readers == 0) || (!write && q.waiting == 0)) {
			break
		}
		changed := q.changed
		q.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-changed:
		}
		q.mu.Lock()
	}
	if write {
		q.waiting--
		q.writer = true
	} else {
		q.readers++
	}
	q.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			q.mu.Lock()
			if write {
				q.writer = false
			} else {
				q.readers--
			}
			q.signal()
			q.mu.Unlock()
		})
	}, nil
}

type localLocks struct {
	lifecycle, state rwQueue
	refs             int
	databaseSlots    chan struct{}
}

var lockRegistry = struct {
	sync.Mutex
	roots map[string]*localLocks
}{roots: map[string]*localLocks{}}

func retainLocks(root string) *localLocks {
	lockRegistry.Lock()
	defer lockRegistry.Unlock()
	l := lockRegistry.roots[root]
	if l == nil {
		// Allow all 32 admitted database operations plus management/status readers.
		l = &localLocks{databaseSlots: make(chan struct{}, 64)}
		lockRegistry.roots[root] = l
	}
	l.refs++
	return l
}
func releaseLocks(root string, l *localLocks) {
	lockRegistry.Lock()
	defer lockRegistry.Unlock()
	l.refs--
	if l.refs == 0 {
		delete(lockRegistry.roots, root)
	}
}

func releaseFile(f *os.File) {
	if f != nil {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		_ = f.Close()
	}
}
func (s *Store) lock(ctx context.Context, path string, exclusive, create bool) (*os.File, error) {
	flags := unix.O_RDWR
	if create {
		flags |= unix.O_CREAT
	}
	f, err := s.openFile(path, flags, 0600)
	if err != nil {
		return nil, err
	}
	if err := s.point("lock-open", path); err != nil {
		f.Close()
		return nil, err
	}
	op := unix.LOCK_SH
	if exclusive {
		op = unix.LOCK_EX
	}
	for {
		if err := ctx.Err(); err != nil {
			f.Close()
			return nil, err
		}
		err = unix.Flock(int(f.Fd()), op|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			f.Close()
			return nil, errors.New("cannot acquire installation lock")
		}
		// flock has no context API. This bounded polling delay never grants a lease
		// after cancellation; all contention tests synchronize via readiness pipes.
		timer := time.NewTimer(5 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
	if err := s.verifyLock(path, f); err != nil {
		releaseFile(f)
		return nil, err
	}
	return f, nil
}
func (s *Store) verifyLock(path string, f *os.File) error {
	dirname, name, err := splitOwned(path)
	if err != nil {
		return err
	}
	d, err := s.directory(dirname)
	if err != nil {
		return err
	}
	defer d.Close()
	if checkFD(int(f.Fd()), false) != nil || !sameNamed(int(d.Fd()), name, f) {
		return ErrStale
	}
	return nil
}

// Lease holds state access through database cleanup/result preparation. Do not
// share a lease across goroutines; Release is idempotent. Lifecycle is acquired
// only for purge, before admission and state. Ordinary mutations need only Write.
type Lease struct {
	ctx                          context.Context
	db                           *sql.DB
	databasePermit               bool
	store                        *Store
	parent                       *Lease
	lifecycle, gate, state       *os.File
	unlockState, unlockLifecycle func()
	write, purge, released       bool
}

// ReadLease admits one shared snapshot; callers must hold it until work ends.
func (s *Store) ReadLease(ctx context.Context) (*Lease, error) { return s.acquire(ctx, false, false) }

// WriteLease blocks admission and waits for every old shared snapshot to finish.
func (s *Store) WriteLease(ctx context.Context) (*Lease, error) { return s.acquire(ctx, true, false) }

// PurgeLease takes lifecycle, admission and state in that order. Only the internal
// purge coordinator should request this capability. It accepts a purge tombstone.
func (s *Store) PurgeLease(ctx context.Context) (*Lease, error) { return s.acquire(ctx, true, true) }
func (s *Store) acquire(ctx context.Context, write, purge bool) (_ *Lease, err error) {
	l := &Lease{store: s, write: write, purge: purge, ctx: ctx}
	defer func() {
		if err != nil {
			l.Release()
		}
	}()
	if purge {
		l.unlockLifecycle, err = s.local.lifecycle.acquire(ctx, true)
		if err != nil {
			return nil, err
		}
		l.lifecycle, err = s.lock(ctx, "state/lifecycle.lock", true, false)
		if err != nil {
			return nil, err
		}
	}
	l.unlockState, err = s.local.state.acquire(ctx, write)
	if err != nil {
		return nil, err
	}
	l.gate, err = s.lock(ctx, "state/state-gate.lock", true, false)
	if err != nil {
		return nil, err
	}
	l.state, err = s.lock(ctx, "state/state.lock", write, false)
	if err != nil {
		return nil, err
	}
	if err = l.check(); err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if !write {
		releaseFile(l.gate)
		l.gate = nil
	}
	return l, nil
}

// Release drops state before admission/lifecycle, including partially acquired leases.
func (l *Lease) Release() {
	if l.released {
		return
	}
	l.released = true
	if l.db != nil {
		l.db.Close()
		l.db = nil
	}
	if l.databasePermit {
		<-l.store.local.databaseSlots
		l.databasePermit = false
	}
	releaseFile(l.state)
	releaseFile(l.gate)
	if l.unlockState != nil {
		l.unlockState()
	}
	releaseFile(l.lifecycle)
	if l.unlockLifecycle != nil {
		l.unlockLifecycle()
	}
}
func (l *Lease) check() error {
	if l.parent != nil {
		if err := l.parent.check(); err != nil {
			return err
		}
	}
	if l.released {
		return ErrStale
	}
	for _, item := range []struct {
		path string
		f    *os.File
	}{{"state/lifecycle.lock", l.lifecycle}, {"state/state-gate.lock", l.gate}, {"state/state.lock", l.state}} {
		if item.f != nil {
			if err := l.store.verifyLock(item.path, item.f); err != nil {
				return err
			}
		}
	}
	err := l.store.verifyIdentity()
	if l.purge && errors.Is(err, ErrPurging) {
		return nil
	}
	return err
}

// Identity returns a copy of the verified installation identity.
func (l *Lease) Identity() Identity {
	id := l.store.identity
	id.Owned = append([]string(nil), id.Owned...)
	return id
}

// Read reads a bounded owned document without following links.
func (l *Lease) Read(path string, limit int) ([]byte, error) {
	if err := l.check(); err != nil {
		return nil, err
	}
	if limit < 1 || limit > 8<<20 {
		return nil, ErrOwnership
	}
	return l.store.read(path, limit)
}

// Replace durably publishes an owned data document under an exclusive lease.
func (l *Lease) Replace(path string, b []byte) error {
	if !l.write || path != "config/known_hosts" || len(b) > 8<<20 {
		return ErrOwnership
	}
	if err := l.check(); err != nil {
		return err
	}
	return l.store.replace(path, b)
}

// Remove is restricted to owned data files; FinishPurge owns terminal cleanup.
func (l *Lease) Remove(path string) error {
	base := strings.TrimSuffix(path, ".tmp")
	if !l.write || (base != "state/data-mate.db" && base != "state/data-mate.db-journal" && base != "config/known_hosts") {
		return ErrOwnership
	}
	if err := l.check(); err != nil {
		return err
	}
	if l.purge {
		return l.store.removeOptionalDirectoryFile(path)
	}
	return l.store.remove(path)
}

// BeginPurge durably revokes ordinary admission before exact-key deletion. The
// identity and stable locks remain available for retry and P11's outer cleanup.
func (l *Lease) BeginPurge() error {
	if !l.purge {
		return ErrOwnership
	}
	if err := l.check(); err != nil {
		return err
	}
	id := l.store.identity
	id.Purging = true
	b, err := encodeIdentity(id)
	if err != nil {
		return err
	}
	if err = l.store.replace("state/installation.json", b); err != nil {
		return err
	}
	l.store.identity = id
	return nil
}
