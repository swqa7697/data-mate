package service

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/database/postgres"
	"github.com/swqa7697/data-mate/internal/mcp"
	"github.com/swqa7697/data-mate/internal/vault"
)

// Serve is the private launchd entry point. A matching durable launch intent is
// required; calling this function never bootstraps a job or creates credentials.
func Serve(ctx context.Context, root config.Root, build Build, nonce string, keys vault.KeyProvider) error {
	s, err := config.OpenExisting(ctx, root)
	if err != nil {
		return ErrState
	}
	defer s.Close()
	l, err := s.ReadLease(ctx)
	if err != nil {
		return ErrState
	}
	r, err := readRecord(l.Read, root, l.Identity())
	l.Release()
	if err != nil || r.Nonce != nonce || r.Build != build {
		return ErrConflict
	}
	d, err := postgres.New()
	if err != nil {
		return ErrStartup
	}
	if keys == nil {
		keys = vault.Keychain{}
	}
	m := newManager(ctx, s, keys, d)
	defer m.Close()
	if err = m.initialize(ctx); err != nil {
		return ErrStartup
	}
	runtime, err := openRuntime(root, r.Identity, false)
	if err != nil {
		return ErrState
	}
	defer runtime.file.Close()
	runtime.name = "m"
	// Only the lifecycle controller removes stale sockets before publishing intent.
	if err = runtime.checkSocket(); !os.IsNotExist(err) {
		return ErrConflict
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: runtime.socket(), Net: "unix"})
	if err != nil {
		return ErrStartup
	}
	listener.SetUnlinkOnClose(false)
	defer listener.Close()
	if err = runtime.protectSocket(); err != nil {
		return ErrState
	}
	if err = runtime.checkSocket(); err != nil {
		return ErrState
	}
	defer runtime.removeSocket()
	return serveListener(ctx, listener, r, m)
}
func serveListener(parent context.Context, listener *net.UnixListener, r record, m *Manager) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	management := filepath.Base(listener.Addr().String()) == "m"
	var wg sync.WaitGroup
	var enableMu sync.Mutex
	if management {
		m.enable = func(op context.Context) error {
			enableMu.Lock()
			defer enableMu.Unlock()
			if err := op.Err(); err != nil {
				return err
			}
			m.mu.Lock()
			enabled := m.enabled
			m.mu.Unlock()
			if enabled {
				return nil
			}
			root, err := config.ResolveRoot(r.Identity.Root, "")
			if err != nil {
				return ErrState
			}
			runtime, err := openRuntime(root, r.Identity, false)
			if err != nil {
				return err
			}
			fail := func(e error) error { runtime.file.Close(); return e }
			if err = runtime.checkSocket(); !os.IsNotExist(err) {
				return fail(ErrConflict)
			}
			session, err := net.ListenUnix("unix", &net.UnixAddr{Name: runtime.socket(), Net: "unix"})
			if err != nil {
				return fail(ErrStartup)
			}
			session.SetUnlinkOnClose(false)
			if err = runtime.protectSocket(); err != nil {
				session.Close()
				return fail(err)
			}
			if err = op.Err(); err != nil {
				session.Close()
				runtime.removeSocket()
				return fail(err)
			}
			m.mu.Lock()
			m.enabled = true
			m.mu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer runtime.file.Close()
				defer runtime.removeSocket()
				defer session.Close()
				_ = serveListener(ctx, session, r, m)
			}()
			return nil
		}
	}
	slots := make(chan struct{}, 16)
	stop := context.AfterFunc(ctx, func() { listener.Close() })
	defer stop()
	defer func() { cancel(); wg.Wait() }()
	for {
		c, err := listener.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return ErrUnavailable
		}
		select {
		case slots <- struct{}{}:
		default:
			c.Close()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			defer c.Close()
			stop := context.AfterFunc(ctx, func() { c.Close() })
			defer stop()
			_ = c.SetDeadline(time.Now().Add(10 * time.Second))
			pid, err := peer(c, uint32(os.Geteuid()))
			if err != nil {
				return
			}
			h, err := readHello(c)
			if err != nil {
				return
			}
			validPurpose := (management && (h.Purpose == "probe" || h.Purpose == "management")) || (!management && h.Purpose == "session")
			if h.Identity != r.Identity || h.Nonce != r.Nonce || h.PID != pid || !validPurpose || h.State != "" || h.Error != "" || h.MCPEnabled || h.KeysetState != "" {
				return
			}
			reply := hello{Protocol: 2, Purpose: h.Purpose, Identity: r.Identity, Build: r.Build, PID: os.Getpid(), Nonce: r.Nonce, State: "starting"}
			if h.Protocol != 2 || h.Build != r.Build {
				reply.Error = "restart"
			} else {
				check, finish := context.WithTimeout(ctx, time.Second)
				reply.State = m.state(check)
				reply.KeysetState = m.keysetState(check)
				finish()
				m.mu.Lock()
				reply.MCPEnabled = m.enabled
				m.mu.Unlock()
			}
			if h.Purpose == "management" && reply.Error == "" && !executablePeer(pid, r.Build.Fingerprint) {
				return
			}
			if writeHello(c, reply) != nil {
				return
			}
			if reply.Error != "" {
				return
			}
			_ = c.SetDeadline(time.Time{})
			if h.Purpose == "management" {
				serveManagement(ctx, c, m)
			} else if h.Purpose == "session" {
				_ = mcp.Serve(ctx, c, m, r.Build.Version)
			}
		}()
	}
}
