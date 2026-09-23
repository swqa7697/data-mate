package config

import (
	"bufio"
	"bytes"

	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

func storageFixture(t *testing.T) (*Store, Root) {
	t.Helper()
	path := t.TempDir()
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := ResolveRoot(path, "")
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, root
}
func profileLease(t *testing.T, s *Store, write bool) *Lease {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var l *Lease
	var err error
	if write {
		l, err = s.WriteLease(ctx)
	} else {
		l, err = s.ReadLease(ctx)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Release)
	return l
}

// Parsing/root-resolution regressions cannot exercise durable files or leases.
// This scenario owns the filesystem safety and fresh-snapshot failure contracts.
func TestOwnedStorage(t *testing.T) {
	s, root := storageFixture(t)
	l := profileLease(t, s, true)
	p, rev, err := l.ProfileSnapshot()
	if err != nil || len(p.Connections) != 0 {
		t.Fatal("initial snapshot", err)
	}
	var fixture Profiles
	if json.Unmarshal(profileFixture(t), &fixture) != nil {
		t.Fatal("fixture")
	}
	next, err := l.SaveProfiles(fixture, rev)
	if err != nil || next == rev {
		t.Fatal("publish", err)
	}
	if _, err := l.SaveProfiles(p, rev); !errors.Is(err, ErrRevision) {
		t.Fatal("stale preview changed profiles", err)
	}
	l.Release()
	s2, err := Open(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	l2 := profileLease(t, s2, false)
	if l2.Identity().ID != s.identity.ID {
		t.Fatal("restart changed identity")
	}
	l2.Release()
	path := filepath.Join(root.Path, "config/connections.json")
	for _, kind := range []string{"mode", "symlink", "hardlink", "directory", "missing", "malformed"} {
		original, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		switch kind {
		case "mode":
			err = os.Chmod(path, 0644)
		case "symlink":
			err = os.Rename(path, path+".saved")
			if err == nil {
				err = os.Symlink(path+".saved", path)
			}
		case "hardlink":
			err = os.Link(path, path+".saved")
		case "directory":
			err = os.Rename(path, path+".saved")
			if err == nil {
				err = os.Mkdir(path, 0700)
			}
		case "missing":
			err = os.Rename(path, path+".saved")
		case "malformed":
			err = os.WriteFile(path, []byte(`{"version":1,"connections":[] ,"unknown":1}`), 0600)
		}
		if err != nil {
			t.Fatal(kind, err)
		}
		lease := profileLease(t, s, false)
		if _, _, err := lease.ProfileSnapshot(); err == nil {
			t.Fatal("unsafe snapshot accepted", kind)
		}
		lease.Release()
		switch kind {
		case "mode":
			err = os.Chmod(path, 0600)
		case "symlink", "directory":
			err = os.Remove(path)
			if err == nil {
				err = os.Rename(path+".saved", path)
			}
		case "hardlink":
			err = os.Remove(path + ".saved")
		case "missing":
			err = os.Rename(path+".saved", path)
		case "malformed":
			err = os.WriteFile(path, original, 0600)
		}
		if err != nil {
			t.Fatal(kind, err)
		}
	}
	// A lock removed by purge/recreation cannot silently confer authority.
	lease := profileLease(t, s, true)
	lockPath := filepath.Join(root.Path, "state/state.lock")
	if err := os.Rename(lockPath, lockPath+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := lease.ProfileSnapshot(); !errors.Is(err, ErrStale) {
		t.Fatal("old inode retained authority", err)
	}
	lease.Release()
}

// A crash between any publication boundaries leaves a whole old/new document.
// There are no new per-boundary subtests; each failure names its boundary.
func TestPublicationRecovery(t *testing.T) {
	for _, point := range []string{"before-file-sync", "after-file-sync", "before-rename", "after-rename", "before-directory-sync", "after-directory-sync"} {
		s, root := storageFixture(t)
		l := profileLease(t, s, true)
		_, rev, err := l.ProfileSnapshot()
		if err != nil {
			t.Fatal(err)
		}
		p, _, err := DecodeProfiles(bytes.NewReader(profileFixture(t)))
		if err != nil {
			t.Fatal(err)
		}
		hit := false
		s.fault = func(op, path string) error {
			if !hit && op == point && path == "config/connections.json" {
				hit = true
				return errors.New("injected boundary failure")
			}
			return nil
		}
		if _, err := l.SaveProfiles(p, rev); err == nil || !hit {
			t.Fatal(point, "boundary not reached")
		}
		l.Release()
		s.fault = nil
		reopened, err := Open(context.Background(), root, nil)
		if err != nil {
			t.Fatal(point, err)
		}
		snap := profileLease(t, reopened, false)
		got, _, err := snap.ProfileSnapshot()
		snap.Release()
		reopened.Close()
		if err != nil || len(got.Connections) > 1 {
			t.Fatal(point, "torn publication", err)
		}
	}
}

// TestStateLeases exercises independent flock ownership, writer preference,
// cancellation, and cross-process stale-inode/identity rejection using pipes.
func TestStateLeases(t *testing.T) {
	if mode := os.Getenv("DATA_MATE_LOCK_HELPER"); mode != "" {
		lockHelper(t, mode)
		return
	}
	s, root := storageFixture(t)
	first := profileLease(t, s, false)
	second := profileLease(t, s, false)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if _, err := s.WriteLease(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("writer bypassed readers", err)
	}
	first.Release()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel2()
	if _, err := s.WriteLease(ctx2); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("one release unlocked another reader", err)
	}
	second.Release()
	// Independent process holds a shared lease until stdin closes.
	child := startLockHelper(t, root, "hold")
	ctx3, cancel3 := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel3()
	if _, err := s.WriteLease(ctx3); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("process lease ignored", err)
	}
	child.finish()
	// Many goroutine writers across independent Store handles share the same queue.
	const writers = 24
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Go(func() {
			other, err := Open(context.Background(), root, nil)
			if err != nil {
				errs <- err
				return
			}
			defer other.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			l, err := other.WriteLease(ctx)
			if err != nil {
				errs <- err
				return
			}
			defer l.Release()
			p, rev, err := l.ProfileSnapshot()
			if err == nil {
				_, err = l.SaveProfiles(p, rev)
			}
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	// Independent processes mutate the same document, proving that flock protects
	// read-modify-write across processes rather than just goroutines.
	children := make([]lockChild, 0, 6)
	for i := 0; i < 6; i++ {
		children = append(children, startLockHelper(t, root, "write"))
	}
	for _, child := range children {
		child.finish()
	}
	inspect := profileLease(t, s, false)
	profiles, _, err := inspect.ProfileSnapshot()
	inspect.Release()
	if err != nil || len(profiles.Connections) != 6 {
		t.Fatal("cross-process updates lost", err)
	}
	// A queued writer must be admitted ahead of new in-process readers.
	reader := profileLease(t, s, false)
	acquired := make(chan *Lease, 1)
	writerErr := make(chan error, 1)
	go func() {
		l, e := s.WriteLease(context.Background())
		if e != nil {
			writerErr <- e
			return
		}
		acquired <- l
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.local.state.mu.Lock()
		waiting := s.local.state.waiting
		s.local.state.mu.Unlock()
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("writer never queued")
		}
		runtime.Gosched()
	}
	lateContext, lateCancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	if l, e := s.ReadLease(lateContext); !errors.Is(e, context.DeadlineExceeded) {
		if l != nil {
			l.Release()
		}
		t.Fatal("late reader bypassed writer", e)
	}
	lateCancel()
	reader.Release()
	select {
	case writer := <-acquired:
		writer.Release()
	case e := <-writerErr:
		t.Fatal(e)
	case <-time.After(5 * time.Second):
		t.Fatal("writer failed to progress")
	}
	// Lifecycle initialization waits for purge and cannot recreate an active root.
	purge := profileLeaseForPurge(t, s)
	openContext, openCancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	if other, e := Open(openContext, root, nil); !errors.Is(e, context.DeadlineExceeded) {
		if other != nil {
			other.Close()
		}
		t.Fatal("initialization bypassed purge lifecycle", e)
	}
	openCancel()
	purge.Release()
	// Waiter captured old state inode, then a simulated purge unlinks it. It must
	// fail after acquiring the old lock instead of writing into the new installation.
	held := profileLease(t, s, false)
	stale := startLockHelper(t, root, "stale")
	// Wait for child to hold admission: it has opened state.lock and is waiting.
	statePath := filepath.Join(root.Path, "state/state.lock")
	if err := os.Rename(statePath, statePath+".retired"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	held.Release()
	stale.finish()
}

type lockChild struct{ finish func() }

func startLockHelper(t *testing.T, root Root, mode string) lockChild {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStateLeases$")
	cmd.Env = append(os.Environ(), "DATA_MATE_LOCK_HELPER="+mode, "DATA_MATE_LOCK_ROOT="+root.Path)
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(output)
	if !scanner.Scan() || scanner.Text() != "ready" {
		t.Fatal("child not ready")
	}
	return lockChild{func() {
		input.Close()
		for scanner.Scan() {
		}
		if err := cmd.Wait(); err != nil {
			t.Fatal("lock child", err)
		}
	}}
}
func lockHelper(t *testing.T, mode string) {
	root, err := ResolveRoot(os.Getenv("DATA_MATE_LOCK_ROOT"), "")
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if mode == "write" {
		fmt.Println("ready")
		l, err := s.WriteLease(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer l.Release()
		p, rev, err := l.ProfileSnapshot()
		if err != nil {
			t.Fatal(err)
		}
		fixture, _, err := DecodeProfiles(bytes.NewReader(profileFixture(t)))
		if err != nil {
			t.Fatal(err)
		}
		c := fixture.Connections[0]
		c.ID, err = NewID()
		if err != nil {
			t.Fatal(err)
		}
		c.Alias = fmt.Sprintf("process-%d", os.Getpid())
		p.Connections = append(p.Connections, c)
		if _, err := l.SaveProfiles(p, rev); err != nil {
			t.Fatal(err)
		}
		return
	}
	if mode == "hold" {
		l, err := s.ReadLease(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer l.Release()
		fmt.Println("ready")
		_, _ = bufio.NewReader(os.Stdin).ReadByte()
		return
	}
	s.fault = func(op, path string) error {
		if op == "lock-open" && path == "state/state.lock" {
			fmt.Println("ready")
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	l, err := s.WriteLease(ctx)
	if l != nil {
		l.Release()
	}
	if !errors.Is(err, ErrStale) {
		t.Fatal("stale waiter result", err)
	}
}

func profileLeaseForPurge(t *testing.T, s *Store) *Lease {
	t.Helper()
	l, e := s.PurgeLease(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(l.Release)
	return l
}
