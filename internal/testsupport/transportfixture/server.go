// Package transportfixture owns synthetic SSH/SOCKS5 peers shared by transport,
// CLI enrollment and PostgreSQL integration scenarios. It never runs helpers or
// accesses user SSH configuration. All listeners and forwarded sockets are owned.
package transportfixture

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/swqa7697/data-mate/internal/config"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Options selects one synthetic peer behavior. Resolve maps only fixture targets.
type Options struct {
	User, Password string
	PublicKey      ssh.PublicKey
	Stall          string // handshake, forward or stream (SSH window-credit stall)
	Resolve        func(string) string
}

// Server owns one listener and every accepted/forwarded connection.
type Server struct {
	Listener    net.Listener
	Signer      ssh.Signer
	Targets     chan string
	Accepted    chan struct{}
	Forwarded   chan struct{}
	Closed      chan struct{}
	ctx         context.Context
	cancel      context.CancelFunc
	mu          sync.Mutex
	connections map[net.Conn]bool
	wg          sync.WaitGroup
	acceptDone  chan struct{}
	once        sync.Once
	options     Options
}

// New starts a local SSH or SOCKS5 peer and registers deterministic cleanup.
func New(t testing.TB, kind string, options Options) *Server {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{Listener: ln, Signer: signer, Targets: make(chan string, 128), Accepted: make(chan struct{}, 128), Forwarded: make(chan struct{}, 128), Closed: make(chan struct{}, 128), ctx: ctx, cancel: cancel, connections: make(map[net.Conn]bool), acceptDone: make(chan struct{}), options: options}
	go func() {
		defer close(s.acceptDone)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.connections[c] = true
			s.mu.Unlock()
			s.wg.Go(func() {
				defer func() { c.Close(); s.mu.Lock(); delete(s.connections, c); s.mu.Unlock(); s.Closed <- struct{}{} }()
				s.Accepted <- struct{}{}
				if options.Stall == "handshake" {
					_, _ = io.Copy(io.Discard, c)
					return
				}
				if kind == "ssh" {
					s.serveSSH(c)
				} else {
					s.serveSOCKS(c)
				}
			})
		}
	}()
	t.Cleanup(s.Close)
	return s
}

// Close stops peers and waits for all forwarding workers to exit.
func (s *Server) Close() {
	s.once.Do(func() {
		s.cancel()
		s.Listener.Close()
		<-s.acceptDone
		s.mu.Lock()
		for c := range s.connections {
			c.Close()
		}
		s.mu.Unlock()
		s.wg.Wait()
	})
}

// Active returns the number of still-owned client sockets.
func (s *Server) Active() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.connections) }

// SSHConfig and ProxyConfig return explicit nonsecret loopback settings.
func (s *Server) SSHConfig(auth string) *config.SSH {
	return &config.SSH{Host: "127.0.0.1", Port: s.Listener.Addr().(*net.TCPAddr).Port, User: s.options.User, Auth: auth}
}
func (s *Server) ProxyConfig() *config.Proxy {
	return &config.Proxy{Kind: "socks5", Host: "127.0.0.1", Port: s.Listener.Addr().(*net.TCPAddr).Port, Username: s.options.User}
}
func (s *Server) KnownHosts() []byte {
	return []byte(knownhosts.Line([]string{s.Listener.Addr().String()}, s.Signer.PublicKey()) + "\n")
}

func (s *Server) target(address string) (net.Conn, error) {
	s.Targets <- address
	if s.options.Resolve != nil {
		address = s.options.Resolve(address)
	}
	return (&net.Dialer{Timeout: time.Second}).DialContext(s.ctx, "tcp", address)
}

func relay(a io.ReadWriteCloser, b net.Conn) {
	done := make(chan struct{})
	go func() { _, _ = io.Copy(b, a); b.Close(); a.Close(); close(done) }()
	_, _ = io.Copy(a, b)
	a.Close()
	b.Close()
	<-done
}

func (s *Server) serveSSH(raw net.Conn) {
	cfg := &ssh.ServerConfig{PasswordCallback: func(c ssh.ConnMetadata, p []byte) (*ssh.Permissions, error) {
		if c.User() != s.options.User || string(p) != s.options.Password {
			return nil, errors.New("denied")
		}
		return nil, nil
	}, PublicKeyCallback: func(c ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
		if c.User() != s.options.User || s.options.PublicKey == nil || !bytes.Equal(k.Marshal(), s.options.PublicKey.Marshal()) {
			return nil, errors.New("denied")
		}
		return nil, nil
	}}
	cfg.AddHostKey(s.Signer)
	c, channels, requests, err := ssh.NewServerConn(raw, cfg)
	if err != nil {
		return
	}
	defer c.Close()
	go ssh.DiscardRequests(requests)
	var wg sync.WaitGroup
	defer wg.Wait()
	for ch := range channels {
		if ch.ChannelType() != "direct-tcpip" {
			_ = ch.Reject(ssh.UnknownChannelType, "unsupported")
			continue
		}
		s.Forwarded <- struct{}{}
		if s.options.Stall == "forward" {
			continue
		}
		if s.options.Stall == "stream" {
			stream, reqs, err := ch.Accept()
			if err != nil {
				continue
			}
			go ssh.DiscardRequests(reqs)
			wg.Go(func() { _ = c.Wait(); _ = stream.Close() })
			continue
		}
		var req struct {
			Host       string
			Port       uint32
			Origin     string
			OriginPort uint32
		}
		if ssh.Unmarshal(ch.ExtraData(), &req) != nil {
			_ = ch.Reject(ssh.ConnectionFailed, "invalid")
			continue
		}
		remote, err := s.target(net.JoinHostPort(req.Host, strconv.Itoa(int(req.Port))))
		if err != nil {
			_ = ch.Reject(ssh.ConnectionFailed, "unavailable")
			continue
		}
		stream, reqs, err := ch.Accept()
		if err != nil {
			remote.Close()
			continue
		}
		go ssh.DiscardRequests(reqs)
		wg.Go(func() { relay(stream, remote) })
	}
}

func (s *Server) serveSOCKS(c net.Conn) {
	var h [4]byte
	if _, err := io.ReadFull(c, h[:2]); err != nil || h[0] != 5 {
		return
	}
	methods := make([]byte, int(h[1]))
	if _, err := io.ReadFull(c, methods); err != nil {
		return
	}
	method := byte(0)
	if s.options.User != "" {
		method = 2
	}
	if !bytes.Contains(methods, []byte{method}) {
		_, _ = c.Write([]byte{5, 255})
		return
	}
	if _, err := c.Write([]byte{5, method}); err != nil {
		return
	}
	if method == 2 {
		if _, err := io.ReadFull(c, h[:2]); err != nil || h[0] != 1 {
			return
		}
		user := make([]byte, int(h[1]))
		if _, err := io.ReadFull(c, user); err != nil {
			return
		}
		if _, err := io.ReadFull(c, h[:1]); err != nil {
			return
		}
		pass := make([]byte, int(h[0]))
		if _, err := io.ReadFull(c, pass); err != nil {
			return
		}
		if string(user) != s.options.User || string(pass) != s.options.Password {
			_, _ = c.Write([]byte{1, 1})
			return
		}
		if _, err := c.Write([]byte{1, 0}); err != nil {
			return
		}
	}
	if _, err := io.ReadFull(c, h[:]); err != nil || h[0] != 5 || h[1] != 1 || h[2] != 0 {
		return
	}
	var host string
	switch h[3] {
	case 1, 4:
		n := 4
		if h[3] == 4 {
			n = 16
		}
		ip := make([]byte, n)
		if _, err := io.ReadFull(c, ip); err != nil {
			return
		}
		host = net.IP(ip).String()
	case 3:
		if _, err := io.ReadFull(c, h[:1]); err != nil {
			return
		}
		b := make([]byte, int(h[0]))
		if _, err := io.ReadFull(c, b); err != nil {
			return
		}
		host = string(b)
	default:
		return
	}
	if _, err := io.ReadFull(c, h[:2]); err != nil {
		return
	}
	s.Forwarded <- struct{}{}
	if s.options.Stall == "forward" {
		_, _ = io.Copy(io.Discard, c)
		return
	}
	remote, err := s.target(net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(h[:2])))))
	if err != nil {
		_, _ = c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	if _, err = c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		remote.Close()
		return
	}
	relay(c, remote)
}
