package service

import (
	"context"
	"net"
	"os"
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
		keys = vault.Keychain{Interactive: true}
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
	var wg sync.WaitGroup
	// Bound pending handshakes and established sessions together.
	slots := make(chan struct{}, 16)
	stop := context.AfterFunc(ctx, func() { listener.Close(); m.cancel() })
	defer stop()
	defer func() { cancel(); m.Close(); wg.Wait() }()
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
			_ = c.SetDeadline(time.Now().Add(2 * time.Second))
			pid, err := peer(c, uint32(os.Geteuid()))
			if err != nil {
				return
			}
			h, err := readHello(c)
			if err != nil {
				return
			}
			if h.Identity != r.Identity || h.Nonce != r.Nonce || h.PID != pid || (h.Purpose != "probe" && h.Purpose != "session") || h.State != "" || h.Error != "" {
				return
			}
			reply := hello{1, h.Purpose, r.Identity, r.Build, os.Getpid(), r.Nonce, "starting", ""}
			if h.Protocol != 1 || h.Build != r.Build {
				reply.Error = "restart"
			} else {
				check, cancel := context.WithTimeout(ctx, time.Second)
				reply.State = m.state(check)
				cancel()
			}
			if writeHello(c, reply) != nil {
				return
			}
			if h.Purpose == "session" && reply.Error == "" {
				_ = c.SetDeadline(time.Time{})
				_ = mcp.Serve(ctx, c, m, r.Build.Version)
			}
		}()
	}
}
