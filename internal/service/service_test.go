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
	if e = c.ProbeSession(t.Context()); !errors.Is(e, ErrUnavailable) {
		t.Fatal("unfinished session succeeded", e)
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
