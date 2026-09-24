package service

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/database"
	"github.com/swqa7697/data-mate/internal/vault"
)

// New owning scenario (regression ladder 3): P7 had no service controller or
// native socket preamble. This exercises real sockets with only launchd faked.
func TestLifecycleIdentityAndReadiness(t *testing.T) {
	c, f, s := controllerFixture(t)
	for _, action := range []func(context.Context) (Status, error){c.Inspect, c.Stop} {
		r, e := action(t.Context())
		if e != nil || r.State != "stopped" {
			t.Fatal(r, e)
		}
	}
	if err := c.ProbeSession(t.Context()); !errors.Is(err, ErrUnavailable) {
		t.Fatal("stopped bridge", err)
	}
	// Concurrent starts serialize and reuse one service instance.
	var wg sync.WaitGroup
	errs := make(chan error, 6)
	for range 6 {
		wg.Go(func() {
			r, e := c.Start(t.Context())
			if e == nil && r.State != "running" {
				e = ErrStartup
			}
			errs <- e
		})
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	if f.starts != 1 {
		t.Fatal("duplicate bootstrap", f.starts)
	}
	r, e := c.Inspect(t.Context())
	if e != nil || r.State != "running" {
		t.Fatal(r, e)
	}
	if e = c.ProbeSession(t.Context()); e != nil {
		t.Fatal("session authentication failed", e)
	}
	l, e := s.ReadLease(t.Context())
	if e != nil {
		t.Fatal(e)
	}
	rec, e := readRecord(l.Read, c.Root, l.Identity())
	l.Release()
	if e != nil {
		t.Fatal(e)
	}
	other := rec
	other.Identity.Digest = "wrong"
	if conn, _, err := connect(t.Context(), c.Root, other, c.Build, "probe"); err == nil {
		conn.Close()
		t.Fatal("wrong installation accepted")
	}
	other = rec
	other.Nonce = "wrong"
	if conn, _, err := connect(t.Context(), c.Root, other, c.Build, "probe"); err == nil {
		conn.Close()
		t.Fatal("wrong nonce accepted")
	}
	changed := c.Build
	changed.Revision = "new"
	if _, _, err := connect(t.Context(), c.Root, rec, changed, "probe"); !errors.Is(err, ErrRestart) {
		t.Fatal("version mismatch", err)
	}
	// Oversized header and forged peer PID never create a session or allocate body.
	for _, kind := range []string{"size", "pid", "protocol"} {
		conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: filepath.Join(socketDir(c.Root), "s"), Net: "unix"})
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.SetDeadline(time.Now().Add(time.Second))
		if kind == "size" {
			var header [4]byte
			binary.BigEndian.PutUint32(header[:], 4097)
			_, _ = conn.Write(header[:])
		} else {
			h := hello{1, "probe", rec.Identity, c.Build, os.Getpid(), rec.Nonce, "", ""}
			if kind == "pid" {
				h.PID++
			} else {
				h.Protocol++
			}
			if err = writeHello(conn, h); err != nil {
				t.Fatal(err)
			}
		}
		h, err := readHello(conn)
		conn.Close()
		if kind == "protocol" {
			if err != nil || h.Error != "restart" {
				t.Fatal("protocol mismatch", h, err)
			}
		} else if err == nil {
			t.Fatal("forged handshake accepted", kind)
		}
	}
	// Exercise the native peer-credential check against a mismatching allowed
	// UID, without requiring root privileges or weakening socket permissions.
	peerConn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: filepath.Join(socketDir(c.Root), "s"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = peer(peerConn, uint32(os.Geteuid()+1)); !errors.Is(err, ErrConflict) {
		t.Fatal("foreign peer UID accepted", err)
	}
	peerConn.Close()
	// Hold incomplete preambles: excess sessions must be rejected promptly.
	var peers []*net.UnixConn
	for range 17 {
		conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: filepath.Join(socketDir(c.Root), "s"), Net: "unix"})
		if err != nil {
			t.Fatal(err)
		}
		peers = append(peers, conn)
	}
	rejected := make(chan bool, 17)
	for _, conn := range peers {
		go func() {
			defer conn.Close()
			_ = conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
			var b [1]byte
			_, err := conn.Read(b[:])
			var timeout net.Error
			rejected <- err != nil && !(errors.As(err, &timeout) && timeout.Timeout())
		}()
	}
	count := 0
	for range peers {
		if <-rejected {
			count++
		}
	}
	if count == 0 {
		t.Fatal("session cap admitted every incomplete handshake")
	}
	// Invalid manual reload becomes degraded without a last-known-good fallback.
	path := filepath.Join(c.Root.Path, "config/connections.json")
	valid, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, []byte(`{"version":99}`), 0600); err != nil {
		t.Fatal(err)
	}
	r, e = c.Inspect(t.Context())
	if !errors.Is(e, ErrState) || r.State != "degraded" {
		t.Fatal(r, e)
	}
	if err = os.WriteFile(path, valid, 0600); err != nil {
		t.Fatal(err)
	}
	// A stale recorded PID must never authorize signaling that PID.
	life, e := s.Lifecycle(t.Context())
	if e != nil {
		t.Fatal(e)
	}
	rec.PID++
	if e = saveRecord(life, rec); e != nil {
		t.Fatal(e)
	}
	life.Release()
	r, e = c.Inspect(t.Context())
	if r.State != "stale" || e == nil {
		t.Fatal("stale PID", r, e)
	}
	// Missing launchd metadata alone cannot establish that a socket is stale.
	f.mu.Lock()
	ownedJob := f.job
	f.job = job{}
	f.mu.Unlock()
	if _, e = c.Stop(t.Context()); !errors.Is(e, ErrConflict) {
		t.Fatal("live orphan socket removed", e)
	}
	f.mu.Lock()
	f.job = ownedJob
	f.mu.Unlock()
	// A foreign launchd job must remain untouched, even with an owned record.
	f.mu.Lock()
	f.job.Args = []string{"/bin/sleep", "100"}
	f.mu.Unlock()
	if _, e = c.Stop(t.Context()); !errors.Is(e, ErrConflict) {
		t.Fatal("foreign job stopped", e)
	}
	if f.stops != 0 {
		t.Fatal("foreign bootout")
	}
	f.mu.Lock()
	f.job.Args = rec.args()
	f.mu.Unlock()
	if _, e = c.Stop(t.Context()); e != nil {
		t.Fatal(e)
	}
	if _, e = c.Stop(t.Context()); e != nil {
		t.Fatal("idempotent stop", e)
	}
	// A verified job that never becomes ready is cleaned on timeout.
	f.noReady = true
	if _, e = c.Start(t.Context()); !errors.Is(e, ErrStartup) {
		t.Fatal("readiness timeout", e)
	}
	if f.job.Present {
		t.Fatal("partial job leaked")
	}
	// Symlink-substituted runtime directory is never followed or removed.
	sentinel := t.TempDir()
	if e = os.Symlink(sentinel, socketDir(c.Root)); e != nil {
		t.Fatal(e)
	}
	if _, e = c.Start(t.Context()); !errors.Is(e, ErrState) {
		t.Fatal("runtime link accepted", e)
	}
	if e = os.Remove(socketDir(c.Root)); e != nil {
		t.Fatal(e)
	}
	// Extend the lifecycle scenario: cleanup must stop the live job before waiting
	// for old state readers, preserve credentials, and fail closed on key denial.
	f.noReady = false
	if _, e = c.Start(t.Context()); e != nil {
		t.Fatal(e)
	}
	reader, e := s.ReadLease(t.Context())
	if e != nil {
		t.Fatal(e)
	}
	bounded, cancel := context.WithTimeout(t.Context(), 60*time.Millisecond)
	if e = c.Uninstall(bounded, false, noKeys{}); !errors.Is(e, context.DeadlineExceeded) {
		t.Fatal("cleanup bypassed reader", e)
	}
	cancel()
	reader.Release()
	if f.job.Present {
		t.Fatal("cleanup did not stop service before waiting")
	}
	files := []string{"config/connections.json", "state/vault.json", "state/vault-usage.json", "config/known_hosts", "unrelated", "state/unrelated"}
	for _, path := range files {
		if e = os.WriteFile(filepath.Join(c.Root.Path, path), []byte("sentinel"), 0600); e != nil {
			t.Fatal(e)
		}
	}
	if e = c.Uninstall(t.Context(), false, noKeys{}); e != nil {
		t.Fatal("default uninstall", e)
	}
	for _, path := range files {
		b, err := os.ReadFile(filepath.Join(c.Root.Path, path))
		if err != nil || string(b) != "sentinel" {
			t.Fatal("preserved data changed", path, err)
		}
	}
	if _, e = os.Lstat(filepath.Join(c.Root.Path, "bin/data-mate")); !os.IsNotExist(e) {
		t.Fatal("binary not removed", e)
	}
	if e = c.Uninstall(t.Context(), false, noKeys{}); e != nil {
		t.Fatal("repeat uninstall", e)
	}
	// Interrupted startup can leave an owned runtime marker without a record.
	life, e = s.Lifecycle(t.Context())
	if e != nil {
		t.Fatal(e)
	}
	runtime, e := openRuntime(c.Root, installation(c.Root, life.Identity()), true)
	life.Release()
	if e != nil {
		t.Fatal(e)
	}
	runtime.file.Close()
	runtimeSentinel := filepath.Join(socketDir(c.Root), "unrelated")
	if e = os.WriteFile(runtimeSentinel, []byte("keep"), 0600); e != nil {
		t.Fatal(e)
	}
	if e = c.Uninstall(t.Context(), false, noKeys{}); !errors.Is(e, ErrConflict) {
		t.Fatal("unowned runtime entries lost retry authority", e)
	}
	if b, err := os.ReadFile(runtimeSentinel); err != nil || string(b) != "keep" {
		t.Fatal("runtime sentinel changed", err)
	}
	if e = os.Remove(runtimeSentinel); e != nil {
		t.Fatal(e)
	}
	// Put back a private executable to prove external failures retain it.
	if e = os.Mkdir(filepath.Join(c.Root.Path, "bin"), 0700); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(c.Root.Path, "bin/data-mate"), []byte("retry executable"), 0700); e != nil {
		t.Fatal(e)
	}
	registration := filepath.Join(c.Root.Path, "state/registrations.json")
	if e = os.WriteFile(registration, []byte("broken ownership"), 0600); e != nil {
		t.Fatal(e)
	}
	if e = c.Uninstall(t.Context(), true, noKeys{}); e == nil {
		t.Fatal("invalid registration ownership accepted")
	}
	if _, e = os.Stat(filepath.Join(c.Root.Path, "bin/data-mate")); e != nil {
		t.Fatal("registration failure removed executable", e)
	}
	if e = os.Remove(registration); e != nil {
		t.Fatal(e)
	}
	keys := &cleanupKeys{err: vault.ErrDenied}
	if e = c.Uninstall(t.Context(), true, keys); !errors.Is(e, vault.ErrDenied) {
		t.Fatal("key denial", e)
	}
	if l, err := s.ReadLease(t.Context()); !errors.Is(err, config.ErrPurging) {
		if l != nil {
			l.Release()
		}
		t.Fatal("purge failed to revoke admission", err)
	}
	for _, path := range files {
		b, err := os.ReadFile(filepath.Join(c.Root.Path, path))
		if err != nil || string(b) != "sentinel" {
			t.Fatal("key denial lost retry data", path, err)
		}
	}
	if _, e = os.Stat(filepath.Join(c.Root.Path, "bin/data-mate")); e != nil {
		t.Fatal("key denial removed executable", e)
	}
	if _, e = os.Lstat(socketDir(c.Root)); !os.IsNotExist(e) {
		t.Fatal("orphan runtime remains", e)
	}
	if e = c.Uninstall(t.Context(), false, noKeys{}); !errors.Is(e, config.ErrPurging) {
		t.Fatal("default uninstall bypassed pending purge", e)
	}
	// An unsafe owned path blocks local deletion after key removal and retains
	// the tombstone. A repaired path lets the same exact-key operation retry.
	hostPath := filepath.Join(c.Root.Path, "config/known_hosts")
	if e = os.Remove(hostPath); e != nil {
		t.Fatal(e)
	}
	if e = os.Symlink(filepath.Join(c.Root.Path, "unrelated"), hostPath); e != nil {
		t.Fatal(e)
	}
	keys.err = nil
	if e = c.Uninstall(t.Context(), true, keys); !errors.Is(e, config.ErrOwnership) {
		t.Fatal("purge followed symlink", e)
	}
	if e = os.Remove(hostPath); e != nil {
		t.Fatal(e)
	}
	if e = c.Uninstall(t.Context(), true, keys); e != nil {
		t.Fatal("purge retry", e)
	}
	for _, digest := range keys.digests {
		if digest != c.Root.Digest {
			t.Fatal("foreign key deleted")
		}
	}
	for _, path := range []string{"unrelated", "state/unrelated"} {
		b, err := os.ReadFile(filepath.Join(c.Root.Path, path))
		if err != nil || string(b) != "sentinel" {
			t.Fatal("purge lost unrelated file", path, err)
		}
	}
	if _, e = os.Lstat(filepath.Join(c.Root.Path, "state/installation.json")); !os.IsNotExist(e) {
		t.Fatal("purge identity retained", e)
	}

	// Missing identity cannot disguise an orphaned vault as a completed purge.
	orphan := filepath.Join(c.Root.Path, "state/vault.json")
	if e = os.WriteFile(orphan, []byte("orphan sentinel"), 0600); e != nil {
		t.Fatal(e)
	}
	if e = c.Uninstall(t.Context(), true, noKeys{}); !errors.Is(e, ErrState) {
		t.Fatal("orphaned installation reported success", e)
	}
	if b, err := os.ReadFile(orphan); err != nil || string(b) != "orphan sentinel" {
		t.Fatal("unowned vault changed", err)
	}

}

// Service work, unlike driver-only scenarios, must acquire state after queue
// admission and retain it through response preparation and driver cleanup.
func TestRequestReloadAndAdmission(t *testing.T) {
	s, root := serviceFixture(t)
	p := fixtureProfile()
	profiles := config.Profiles{Version: 1, Connections: []config.Profile{p}}
	putProfiles(t, s, profiles)
	d := &observedDriver{}
	m := newManager(t.Context(), s, noKeys{}, d)
	defer m.Close()
	if err := m.initialize(t.Context()); err != nil {
		t.Fatal(err)
	} // unreachable DB does not prevent readiness
	started := make(chan struct{})
	finish := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- m.Work(t.Context(), "fixture", func(_ context.Context, _ database.Driver, a database.Access) error {
			close(started)
			<-finish
			if a.Profile.Scope.Mode != "all" {
				return ErrState
			}
			return nil
		})
	}()
	<-started
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	if l, e := s.WriteLease(ctx); !errors.Is(e, context.DeadlineExceeded) {
		if l != nil {
			l.Release()
		}
		t.Fatal("writer passed active old snapshot", e)
	}
	cancel()
	close(finish)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	profiles.Connections[0].Scope = config.Scope{Mode: "selected", Schemas: []string{}, Tables: []config.Table{}}
	putProfiles(t, s, profiles)
	if err := m.Work(t.Context(), "fixture", func(_ context.Context, _ database.Driver, a database.Access) error {
		if a.Profile.Scope.Mode != "selected" {
			return ErrState
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if d.retired() != 1 {
		t.Fatal("changed pool not retired")
	}
	path := filepath.Join(root.Path, "config/connections.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	called := false
	if err = m.Work(t.Context(), "fixture", func(context.Context, database.Driver, database.Access) error { called = true; return nil }); err == nil || called {
		t.Fatal("invalid reload ran driver", err)
	}
	if d.retired() != 2 {
		t.Fatal("invalid reload retained resources")
	}
	if err = os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	// Saturated requests wait before acquiring a state lease; a writer can proceed.
	for range cap(m.active) {
		m.active <- struct{}{}
	}
	queued := make(chan error, 1)
	go func() {
		queued <- m.Work(t.Context(), "fixture", func(_ context.Context, _ database.Driver, a database.Access) error {
			if a.Profile.Scope.Mode != "all" {
				return ErrState
			}
			return nil
		})
	}()
	waitFor(t, func() bool { return len(m.waiting) == 1 })
	profiles.Connections[0].Scope = config.Scope{Mode: "all"}
	putProfiles(t, s, profiles)
	for range cap(m.waiting) - 1 {
		m.waiting <- struct{}{}
	}
	if _, err = m.admit(t.Context()); err == nil {
		t.Fatal("queue exceeded cap")
	}
	for range cap(m.waiting) - 1 {
		<-m.waiting
	}
	<-m.active
	if err = <-queued; err != nil {
		t.Fatal("queued request kept old snapshot", err)
	}
	for len(m.active) > 0 {
		<-m.active
	}
	// The old 35-second outer deadline must not shorten either profile budget.
	// Observe actual propagated contexts instead of waiting for minute-long timers.
	for _, tc := range []struct {
		name          string
		limits        *config.Limits
		parentTimeout time.Duration
		want          time.Duration
	}{
		{"default", nil, 0, 60 * time.Second},
		{"maximum", &config.Limits{QueryTimeoutMS: 300000, MaxRows: 500, MaxResultBytes: 1048576}, 0, 5 * time.Minute},
		{"earlier caller", nil, 10 * time.Second, 10 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profiles.Connections[0].Limits = tc.limits
			putProfiles(t, s, profiles)
			parent := t.Context()
			if tc.parentTimeout != 0 {
				var cancel context.CancelFunc
				parent, cancel = context.WithTimeout(parent, tc.parentTimeout)
				defer cancel()
			}
			before := time.Now()
			err := m.Work(parent, "fixture", func(ctx context.Context, _ database.Driver, _ database.Access) error {
				deadline, ok := ctx.Deadline()
				if !ok {
					t.Error("missing operation deadline")
				} else if tc.parentTimeout != 0 {
					pd, _ := parent.Deadline()
					if !deadline.Equal(pd) {
						t.Error("earlier caller deadline was changed")
					}
				} else if deadline.Before(before.Add(tc.want)) || deadline.After(time.Now().Add(tc.want)) {
					t.Errorf("wrong propagated deadline for %s: %v", tc.name, deadline)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
	cancelled := make(chan struct{})
	active := make(chan error, 1)
	go func() {
		active <- m.Work(t.Context(), "fixture", func(ctx context.Context, _ database.Driver, _ database.Access) error {
			close(cancelled)
			<-ctx.Done()
			return ctx.Err()
		})
	}()
	<-cancelled
	m.Close()
	if err = <-active; !errors.Is(err, context.Canceled) {
		t.Fatal("shutdown did not cancel active work", err)
	}
	if err = m.Work(t.Context(), "fixture", func(context.Context, database.Driver, database.Access) error { return nil }); err == nil {
		t.Fatal("shutdown admitted work")
	}
}
func waitFor(t *testing.T, fn func() bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for !fn() {
		select {
		case <-ctx.Done():
			t.Fatal("condition deadline")
		case <-tick.C:
		}
	}
}
