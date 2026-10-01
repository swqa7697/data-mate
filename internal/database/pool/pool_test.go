package pool

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
)

func requireCode(t *testing.T, err error, code contracts.Code) {
	t.Helper()
	var e *database.Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("wanted %s, got %v", code, err)
	}
}

func registry(t *testing.T, o Options) *Registry {
	t.Helper()
	r, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	return r
}

// fakeConn is a physical connection whose lifetime the test controls.
type fakeConn struct {
	closed chan struct{}
	once   sync.Once
}

func newFake() *fakeConn                    { return &fakeConn{closed: make(chan struct{})} }
func (c *fakeConn) Closed() <-chan struct{} { return c.closed }
func (c *fakeConn) Close()                  { c.once.Do(func() { close(c.closed) }) }
func (c *fakeConn) Reusable() bool {
	select {
	case <-c.closed:
		return false
	default:
		return true
	}
}

// otherConn is a connection type of a different driver.
type otherConn struct{ *fakeConn }

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
		runtime.Gosched()
	}
}

// This scenario owns admission/cursor algorithms that cannot be reliably saturated
// through network timing. Synchronization is explicit rather than latency assertions.
func TestAdmissionAndCursorLifecycle(t *testing.T) {
	r := registry(t, Options{})
	var leave []func()
	for range 32 {
		f, err := r.Admit(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		leave = append(leave, f)
	}
	ctx, cancel := context.WithCancel(t.Context())
	results := make(chan error, 128)
	var wg sync.WaitGroup
	for range 128 {
		wg.Go(func() {
			f, err := r.Admit(ctx)
			if err == nil {
				f()
			}
			results <- err
		})
	}
	waitFor(t, "waiters did not enter queue", func() bool { return len(r.admission.waiting) == 128 })
	_, err := r.Admit(t.Context())
	requireCode(t, err, contracts.ResourceLimit)
	cancel()
	wg.Wait()
	close(results)
	for err := range results {
		requireCode(t, err, contracts.Cancelled)
	}
	for _, f := range leave {
		f()
	}
	if len(r.admission.active) != 0 || len(r.admission.waiting) != 0 {
		t.Fatal("admission leaked")
	}
	valid := func(s string) bool { return s != "" }
	c := Cursor{Version: 1, ID: "id", Revision: "revision", Schema: "a", Name: "b"}
	token := r.Encode(c)
	if _, err = r.Decode(token, c, valid); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{token + "x", strings.Repeat("x", 2049), "!!!"} {
		_, err = r.Decode(bad, c, valid)
		requireCode(t, err, contracts.StaleCursor)
	}
	changed := c
	changed.Revision = "other"
	_, err = r.Decode(token, changed, valid)
	requireCode(t, err, contracts.StaleCursor)
	// A driver's identifier rule rejects positions it cannot have produced.
	_, err = r.Decode(token, c, func(string) bool { return false })
	requireCode(t, err, contracts.StaleCursor)
	_, err = registry(t, Options{}).Decode(token, c, valid)
	requireCode(t, err, contracts.StaleCursor)
}

// The shared registry owns pool lifetime and account approval for every driver.
// Each step protects a contract formerly reachable only through a live server.
func TestPoolLifecycle(t *testing.T) {
	r := registry(t, Options{})
	access := func(id, password string) database.Access {
		return database.NewAccess(config.Profile{ID: id}, password)
	}
	a := access("12345678-1234-1234-1234-123456789abc", "synthetic")
	var audits atomic.Int64
	var auditErr atomic.Pointer[error]
	open := func(context.Context) (*fakeConn, error) { return newFake(), nil }
	approve := func(context.Context, *fakeConn) error {
		audits.Add(1)
		if p := auditErr.Load(); p != nil {
			return *p
		}
		return nil
	}
	checkout := func(ctx context.Context, a database.Access) (*fakeConn, context.Context, func(bool), error) {
		return Checkout(ctx, r, a, "revision", open, approve)
	}

	// Concurrent openers share one validation; approval survives partial disconnects.
	held := make(chan func(bool), 8)
	conns := make(chan *fakeConn, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			c, _, release, err := checkout(t.Context(), a)
			if err != nil {
				t.Error(err)
				return
			}
			conns <- c
			held <- release
		})
	}
	wg.Wait()
	if t.Failed() || audits.Load() != 1 {
		t.Fatalf("concurrent pool opening ran %d audits", audits.Load())
	}
	// A ninth operation waits for a slot instead of opening a ninth connection.
	ninth := make(chan error, 1)
	go func() {
		_, _, release, err := checkout(t.Context(), a)
		if err == nil {
			release(true)
		}
		ninth <- err
	}()
	waitFor(t, "ninth operation did not queue", func() bool { s := r.Stats(a.Profile.ID); return s.Users == 9 && s.Slots == 8 })
	for range 8 {
		(<-held)(true)
	}
	if err := <-ninth; err != nil {
		t.Fatal(err)
	}
	if s := r.Stats(a.Profile.ID); s.Idle != 8 || s.Users != 0 || !s.Approved {
		t.Fatalf("pool after release: %+v", s)
	}
	close(conns)
	all := []*fakeConn{}
	for c := range conns {
		all = append(all, c)
	}
	all[0].Close()
	_, _, release, err := checkout(t.Context(), a)
	if err != nil || audits.Load() != 1 {
		t.Fatal("partial disconnect lost approval", err)
	}
	release(true)
	for _, c := range all {
		c.Close()
	}
	// The final disconnect clears approval; a failed audit wakes waiters and
	// permits a fresh retry instead of poisoning the pool.
	denied := error(database.Fail(contracts.ReadOnlyViolation, "synthetic write privilege", false))
	auditErr.Store(&denied)
	_, _, _, err = checkout(t.Context(), a)
	requireCode(t, err, contracts.ReadOnlyViolation)
	auditErr.Store(nil)
	_, _, release, err = checkout(t.Context(), a)
	if err != nil || audits.Load() != 3 {
		t.Fatalf("retry after failed audit: %v, %d audits", err, audits.Load())
	}
	release(true)

	// Changed credentials retire the pool and close its idle connections.
	before := r.Stats(a.Profile.ID).Idle
	changed, _, release, err := checkout(t.Context(), access(a.Profile.ID, "rotated"))
	if err != nil || before == 0 || r.Stats(a.Profile.ID).Live != 1 {
		t.Fatal("credential change reused old pool", err)
	}
	// Invalidation cancels active work through the returned operation context.
	_, op, opRelease, err := checkout(t.Context(), access(a.Profile.ID, "rotated"))
	if err != nil {
		t.Fatal(err)
	}
	r.Invalidate(a.Profile.ID)
	select {
	case <-op.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("invalidation did not cancel active work")
	}
	opRelease(true)
	release(true)
	if r.Stats(a.Profile.ID).Present || changed.Reusable() {
		t.Fatal("retired pool retained a connection")
	}

	// Another driver's idle connection is closed, never asserted into use.
	_, _, release, err = Checkout(t.Context(), r, a, "revision", func(context.Context) (otherConn, error) { return otherConn{newFake()}, nil }, func(context.Context, otherConn) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	release(true)
	opened := 0
	c, _, release, err := Checkout(t.Context(), r, a, "revision", func(context.Context) (*fakeConn, error) { opened++; return newFake(), nil }, approve)
	if err != nil || opened != 1 || c == nil {
		t.Fatal("mismatched idle connection was reused", err)
	}
	release(true)

	// At most sixteen pools exist; an idle pool is evicted for a new profile.
	for i := range 17 {
		_, _, release, err := checkout(t.Context(), access(fmt.Sprintf("12345678-1234-1234-1234-%012d", i), "synthetic"))
		if err != nil {
			t.Fatal(err)
		}
		release(true)
	}
	if r.Len() != 16 {
		t.Fatalf("pool count %d", r.Len())
	}
	r.Close()
	_, _, _, err = checkout(t.Context(), a)
	requireCode(t, err, contracts.ServiceUnavailable)

	// A pool without users retires after its idle TTL.
	short := registry(t, Options{IdleTTL: 10 * time.Millisecond})
	_, _, release, err = Checkout(t.Context(), short, a, "revision", open, approve)
	if err != nil {
		t.Fatal(err)
	}
	release(true)
	waitFor(t, "idle pool not retired", func() bool { return !short.Stats(a.Profile.ID).Present })
}
