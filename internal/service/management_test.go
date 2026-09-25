package service

import (
	"bytes"
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

type managedKeys struct {
	mu             sync.Mutex
	key            []byte
	loads, creates int
	denied         bool
	entered        chan struct{}
	release        chan struct{}
}

func (k *managedKeys) Load(context.Context, string) ([]byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.loads++
	if k.denied {
		return nil, vault.ErrDenied
	}
	if k.key == nil {
		return nil, vault.ErrMissing
	}
	return bytes.Clone(k.key), nil
}
func (k *managedKeys) CreateIfAbsent(_ context.Context, _ string, key []byte) ([]byte, error) {
	k.mu.Lock()
	k.creates++
	entered, release := k.entered, k.release
	k.entered = nil
	k.mu.Unlock()
	if entered != nil {
		close(entered)
		<-release
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.key == nil {
		k.key = bytes.Clone(key)
	}
	return bytes.Clone(k.key), nil
}
func (k *managedKeys) Delete(context.Context, string) error { return nil }
func (k *managedKeys) calls() int                           { k.mu.Lock(); defer k.mu.Unlock(); return k.loads + k.creates }

type blockedDiagnostics struct {
	database.Driver
	entered chan struct{}
	release chan struct{}
}

func (d *blockedDiagnostics) Invalidate(string) {}
func (d *blockedDiagnostics) Close()            {}
func (d *blockedDiagnostics) Test(ctx context.Context, _ database.Access) (database.Readiness, error) {
	d.entered <- struct{}{}
	select {
	case <-ctx.Done():
		return database.Readiness{}, ctx.Err()
	case <-d.release:
		return database.Readiness{Stage: "read_only", Stages: []database.Stage{{Stage: "read_only", OK: true}}}, nil
	}
}

// No previous regression exercised the new private socket. This scenario owns
// management-only admission, patches, independent clients, framing and cancellation.
func TestPrivateManagement(t *testing.T) {
	c, f, _ := controllerFixture(t)
	keys := &managedKeys{}
	f.keys = keys
	driver := &blockedDiagnostics{entered: make(chan struct{}, 4), release: make(chan struct{})}
	f.driver = driver
	state, e := c.EnsureManagement(t.Context())
	if e != nil || state.MCPEnabled || state.KeysetState != "absent" {
		t.Fatal("management bootstrap", state, e)
	}
	if e = c.ProbeSession(t.Context()); e == nil {
		t.Fatal("management exposed MCP")
	}
	p, rev, e := config.Preview(t.Context(), c.Root)
	if e != nil {
		t.Fatal(e)
	}
	profile := fixtureProfile()
	p.Connections = append(p.Connections, profile)
	apply := func(p config.Profiles, rev config.Revision, patch vault.Patch) (ManagementReply, error) {
		q := ManagementRequest{Operation: "mutate", Mutation: &vault.Mutation{Expected: rev, Profiles: p}}
		if patch != nil {
			q.Mutation.Patches = map[string]vault.Patch{profile.ID: patch}
		}
		return c.Request(t.Context(), q)
	}
	if r, e := apply(p, rev, vault.Patch{"password": "synthetic-management-secret"}); e != nil || !r.Outcome.ProfilesSaved {
		t.Fatal("initial save", r, e)
	}
	calls := keys.calls()
	keys.mu.Lock()
	keys.denied = true
	keys.mu.Unlock()
	for i := 0; i < 3; i++ {
		p, rev, e = config.Preview(t.Context(), c.Root)
		if e != nil {
			t.Fatal(e)
		}
		r, e := apply(p, rev, vault.Patch{"password": "synthetic-edit"})
		if e != nil || !r.Outcome.ProfilesSaved {
			t.Fatal("cached save", r, e)
		}
		if _, e = apply(p, rev, nil); !errors.Is(e, config.ErrRevision) {
			t.Fatal("credential revision", e)
		}
	}
	if keys.calls() != calls {
		t.Fatal("independent clients reloaded OS keyset")
	}
	// Four blocked database management requests consume the entire admission budget;
	// a fifth must fail promptly instead of waiting behind them.
	pending := make(chan error, 4)
	for i := 0; i < 4; i++ {
		go func() {
			_, e := c.Request(t.Context(), ManagementRequest{Operation: "test", ProfileID: profile.ID, Alias: profile.Alias})
			pending <- e
		}()
	}
	for i := 0; i < 4; i++ {
		select {
		case <-driver.entered:
		case <-time.After(3 * time.Second):
			t.Fatal("management request not admitted")
		}
	}
	bounded, finishAdmission := context.WithTimeout(t.Context(), time.Second)
	_, overload := c.Request(bounded, ManagementRequest{Operation: "test", ProfileID: profile.ID, Alias: profile.Alias})
	finishAdmission()
	if !errors.Is(overload, ErrUnavailable) {
		t.Fatal("management overload queued", overload)
	}
	close(driver.release)
	for i := 0; i < 4; i++ {
		if e := <-pending; e != nil {
			t.Fatal(e)
		}
	}
	if state, e = c.Start(t.Context()); e != nil || !state.MCPEnabled {
		t.Fatal("enable", state, e)
	}
	// Management operations sent to the MCP socket are rejected during handshake.
	s, e := config.OpenExisting(t.Context(), c.Root)
	if e != nil {
		t.Fatal(e)
	}
	l, e := s.ReadLease(t.Context())
	if e != nil {
		t.Fatal(e)
	}
	r, e := readRecord(l.Read, c.Root, l.Identity())
	l.Release()
	s.Close()
	if e != nil {
		t.Fatal(e)
	}
	conn, e := net.DialUnix("unix", nil, &net.UnixAddr{Name: filepath.Join(socketDir(c.Root), "s"), Net: "unix"})
	if e != nil {
		t.Fatal(e)
	}
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	_ = writeHello(conn, hello{Protocol: 2, Purpose: "management", Identity: r.Identity, Build: c.Build, PID: os.Getpid(), Nonce: r.Nonce})
	if _, e = readHello(conn); e == nil {
		t.Fatal("MCP dispatched management")
	}
	conn.Close()
	// Header length is rejected before allocating a request body.
	conn, _, e = connect(t.Context(), c.Root, r, c.Build, "management")
	if e != nil {
		t.Fatal(e)
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], managementLimit+1)
	_, _ = conn.Write(header[:])
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	var reply ManagementReply
	if e = readFrame(conn, &reply); e == nil {
		t.Fatal("oversized frame accepted")
	}
	conn.Close()
	for _, raw := range []string{`{"operation":"enable","operation":"mutate","interactive":false}`, `{"operation":"enable","interactive":false,"export":true}`, `{"operation":"get_password","interactive":false}`} {
		malformed, _, e := connect(t.Context(), c.Root, r, c.Build, "management")
		if e != nil {
			t.Fatal(e)
		}
		_ = malformed.SetDeadline(time.Now().Add(time.Second))
		var size [4]byte
		binary.BigEndian.PutUint32(size[:], uint32(len(raw)))
		_, _ = malformed.Write(size[:])
		_, _ = malformed.Write([]byte(raw))
		var reply ManagementReply
		e = readFrame(malformed, &reply)
		malformed.Close()
		if e == nil && reply.Error == "" {
			t.Fatal("invalid/private operation accepted")
		}
	}
	if _, e = c.Stop(t.Context()); e != nil {
		t.Fatal(e)
	}
	if _, e = c.EnsureManagement(t.Context()); e != nil {
		t.Fatal("locked management restart", e)
	}
	if state, e = c.Start(t.Context()); !errors.Is(e, vault.ErrDenied) || state.MCPEnabled {
		t.Fatal("enable denial", state, e)
	}
	if state, e = c.Inspect(t.Context()); e != nil || state.State != "running" || state.MCPEnabled {
		t.Fatal("denial killed management", state, e)
	}
	// Scope-only edits and deletion still work with a locked keyset.
	p, rev, e = config.Preview(t.Context(), c.Root)
	if e != nil {
		t.Fatal(e)
	}
	p.Connections[0].Scope = config.Scope{Mode: "selected"}
	if _, e = apply(p, rev, nil); e != nil {
		t.Fatal("nonsecret locked edit", e)
	}
	p, rev, e = config.Preview(t.Context(), c.Root)
	if e != nil {
		t.Fatal(e)
	}
	p.Connections = []config.Profile{}
	if _, e = apply(p, rev, nil); e != nil {
		t.Fatal("locked delete", e)
	}

	// An uninterruptible provider may complete late, but cannot publish a canceled save.
	c2, f2, _ := controllerFixture(t)
	blocked := &managedKeys{entered: make(chan struct{}), release: make(chan struct{})}
	entered := blocked.entered
	f2.keys = blocked
	if _, e = c2.EnsureManagement(t.Context()); e != nil {
		t.Fatal(e)
	}
	p, rev, e = config.Preview(t.Context(), c2.Root)
	if e != nil {
		t.Fatal(e)
	}
	p.Connections = append(p.Connections, profile)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	q := ManagementRequest{Operation: "mutate", Mutation: &vault.Mutation{Expected: rev, Profiles: p, Patches: map[string]vault.Patch{profile.ID: {"password": "synthetic-canceled"}}}}
	go func() { _, e := c2.Request(ctx, q); done <- e }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("provider not reached")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("provider blocked cancellation")
	}
	// State was not locked across the OS prompt, so passive reads remain available.
	inspect, finish := context.WithTimeout(t.Context(), time.Second)
	defer finish()
	current, _, e := config.Preview(inspect, c2.Root)
	if e != nil || len(current.Connections) != 0 {
		t.Fatal("prompt held state or published", e)
	}
	close(blocked.release)
	if _, e = c2.Stop(t.Context()); e != nil {
		t.Fatal("shutdown waited for late provider", e)
	}
	current, _, e = config.Preview(t.Context(), c2.Root)
	if e != nil || len(current.Connections) != 0 {
		t.Fatal("late publication", e)
	}
}
