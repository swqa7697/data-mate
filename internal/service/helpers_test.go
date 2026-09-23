package service

import (
	"context"
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

type noKeys struct{}

// cleanupKeys observes exact deletion without ever loading real credentials.
type cleanupKeys struct {
	noKeys
	err     error
	digests []string
}

func (k *cleanupKeys) Delete(_ context.Context, digest string) error {
	k.digests = append(k.digests, digest)
	return k.err
}

func (noKeys) Load(context.Context, string) ([]byte, error) {
	return nil, errors.New("unexpected key load")
}
func (noKeys) CreateIfAbsent(context.Context, string, []byte) ([]byte, error) {
	return nil, errors.New("unexpected key creation")
}
func (noKeys) Delete(context.Context, string) error { return errors.New("unexpected key deletion") }

type observedDriver struct {
	database.Driver
	mu          sync.Mutex
	invalidated []string
	closed      bool
}

func (d *observedDriver) Invalidate(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.invalidated = append(d.invalidated, id)
}
func (d *observedDriver) Close()       { d.mu.Lock(); defer d.mu.Unlock(); d.closed = true }
func (d *observedDriver) retired() int { d.mu.Lock(); defer d.mu.Unlock(); return len(d.invalidated) }
func serviceFixture(t *testing.T) (*config.Store, config.Root) {
	t.Helper()
	path := t.TempDir()
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := config.ResolveRoot(path, "")
	if err != nil {
		t.Fatal(err)
	}
	s, err := config.Open(t.Context(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, root
}
func putProfiles(t *testing.T, s *config.Store, p config.Profiles) {
	t.Helper()
	l, err := s.WriteLease(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	_, rev, err := l.ProfileSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = l.SaveProfiles(p, rev); err != nil {
		t.Fatal(err)
	}
}
func fixtureProfile() config.Profile {
	limits := config.DefaultLimits()
	return config.Profile{ID: "936e3468-5b48-4ef2-9a89-964449f06d98", Alias: "fixture", Driver: "postgres", Connection: config.Connection{Host: "127.0.0.1", Port: 1, Database: "fixture", Username: "reader"}, Transport: config.Transport{TLS: config.TLS{Mode: "disabled"}}, Scope: config.Scope{Mode: "all"}, Limits: &limits}
}

type fakeLaunch struct {
	mu            sync.Mutex
	job           job
	starts, stops int
	cancel        context.CancelFunc
	done          chan error
	noReady       bool
	keys          vault.KeyProvider
	driver        database.Driver
}

func (f *fakeLaunch) Inspect(_ context.Context, _ config.Root) (job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.job, nil
}
func (f *fakeLaunch) Bootstrap(ctx context.Context, root config.Root) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, err := config.OpenExisting(ctx, root)
	if err != nil {
		return err
	}
	l, err := s.ReadLease(ctx)
	if err != nil {
		s.Close()
		return err
	}
	r, err := readRecord(l.Read, root, l.Identity())
	l.Release()
	if err != nil {
		s.Close()
		return err
	}
	f.job = job{true, os.Getpid(), filepath.Join(root.Path, "state/service.plist"), r.args()}
	f.starts++
	if f.noReady {
		s.Close()
		return nil
	}
	runtime, err := openRuntime(root, r.Identity, false)
	if err != nil {
		s.Close()
		return err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: runtime.socket(), Net: "unix"})
	if err != nil {
		runtime.file.Close()
		s.Close()
		return err
	}
	listener.SetUnlinkOnClose(false)
	if err = runtime.protectSocket(); err != nil {
		return err
	}
	bg, cancel := context.WithCancel(context.Background())
	f.cancel = cancel
	f.done = make(chan error, 1)
	keys := f.keys
	if keys == nil {
		keys = noKeys{}
	}
	d := f.driver
	if d == nil {
		d = &observedDriver{}
	}
	m := newManager(bg, s, keys, d)
	go func() {
		defer s.Close()
		defer runtime.file.Close()
		defer runtime.removeSocket()
		defer listener.Close()
		defer m.Close()
		if err := m.initialize(bg); err != nil {
			f.done <- err
			return
		}
		f.done <- serveListener(bg, listener, r, m)
	}()
	return nil
}
func (f *fakeLaunch) Bootout(ctx context.Context, _ config.Root) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops++
	if f.cancel != nil {
		f.cancel()
		select {
		case <-f.done:
		case <-ctx.Done():
			return ctx.Err()
		}
		f.cancel = nil
	}
	f.job = job{}
	return nil
}
func controllerFixture(t *testing.T) (*Controller, *fakeLaunch, *config.Store) {
	t.Helper()
	s, root := serviceFixture(t)
	if err := os.Mkdir(filepath.Join(root.Path, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root.Path, "bin/data-mate"), []byte("fixture executable"), 0700); err != nil {
		t.Fatal(err)
	}
	hash, err := binaryHash(filepath.Join(root.Path, "bin/data-mate"))
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeLaunch{}
	c := New(root, Build{"test", "fixture", hash})
	c.launcher = f
	c.readiness = 150 * time.Millisecond
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, _ = c.Stop(ctx)
	})
	return c, f, s
}
