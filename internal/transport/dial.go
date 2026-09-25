package transport

import (
	"context"
	"errors"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/swqa7697/data-mate/internal/config"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
	"golang.org/x/net/proxy"
)

// Credentials contains only private in-memory vault values. Never log it.
type Credentials struct {
	SSHPassword, SSHPrivateKey, SSHKeyPassphrase, ProxyPassword string
}

var ErrRoute = errors.New("connection route failed; check endpoint, authentication and transport")
var ErrConfiguration = errors.New("invalid transport configuration or credentials")

func validHost(host string) bool {
	return host != "" && len(host) <= 253 && (net.ParseIP(host) != nil || !strings.ContainsAny(host, "/\\:@,?=#[] \t\r\n\x00"))
}

// Dialer constructs a route without network access or ambient configuration.
// Every returned connection owns its complete route; Close also closes SSH.
func Dialer(settings config.Transport, secrets Credentials, rawHosts []byte) (func(context.Context, string, string) (net.Conn, error), error) {
	if settings.SSH != nil && settings.Proxy != nil {
		return nil, ErrConfiguration
	}
	for _, v := range []string{secrets.SSHPassword, secrets.SSHPrivateKey, secrets.SSHKeyPassphrase, secrets.ProxyPassword} {
		if len(v) > 128<<10 || strings.ContainsRune(v, 0) {
			return nil, ErrConfiguration
		}
	}
	direct := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	if s := settings.SSH; s != nil {
		if !validHost(s.Host) || s.Port < 1 || s.Port > 65535 || s.User == "" {
			return nil, ErrConfiguration
		}
		hosts, err := parseHosts(rawHosts)
		if err != nil {
			return nil, err
		}
		address := sshAddress(*s)
		if hosts[knownhosts.Normalize(address)] == nil {
			return nil, ErrUnknownHost
		}
		var auth ssh.AuthMethod
		switch s.Auth {
		case "password":
			auth = ssh.Password(secrets.SSHPassword)
		case "key":
			var signer ssh.Signer
			if secrets.SSHKeyPassphrase != "" {
				signer, err = ssh.ParsePrivateKeyWithPassphrase([]byte(secrets.SSHPrivateKey), []byte(secrets.SSHKeyPassphrase))
			} else {
				signer, err = ssh.ParsePrivateKey([]byte(secrets.SSHPrivateKey))
			}
			if err != nil {
				return nil, ErrConfiguration
			}
			auth = ssh.PublicKeys(signer)
		default:
			return nil, ErrConfiguration
		}
		cfg := &ssh.ClientConfig{User: s.User, Auth: []ssh.AuthMethod{auth}, HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error { return checkHost(hosts, address, key) }}
		return func(ctx context.Context, network, target string) (net.Conn, error) {
			if network != "tcp" {
				return nil, ErrConfiguration
			}
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			raw, err := direct.DialContext(ctx, "tcp", address)
			if err != nil {
				return nil, routeError(ctx, err)
			}
			// Stop cancellation synchronously before handing off socket ownership.
			done := make(chan struct{})
			stop := context.AfterFunc(ctx, func() { _ = raw.Close(); close(done) })
			finish := func() {
				if !stop() {
					<-done
				}
			}
			conn, chans, reqs, err := ssh.NewClientConn(raw, address, cfg)
			if err != nil {
				finish()
				_ = raw.Close()
				return nil, routeError(ctx, err)
			}
			client := ssh.NewClient(conn, chans, reqs)
			forward, err := client.DialContext(ctx, "tcp", target)
			finish()
			if err != nil || ctx.Err() != nil {
				_ = raw.Close()
				_ = client.Close()
				return nil, routeError(ctx, err)
			}
			return &sshConn{Conn: forward, raw: raw, client: client, target: target}, nil
		}, nil
	}
	if s := settings.Proxy; s != nil {
		if s.Kind != "socks5" || !validHost(s.Host) || s.Port < 1 || s.Port > 65535 || len(s.Username) > 255 || len(secrets.ProxyPassword) > 255 || (s.Username == "" && secrets.ProxyPassword != "") {
			return nil, ErrConfiguration
		}
		var auth *proxy.Auth
		if s.Username != "" {
			auth = &proxy.Auth{User: s.Username, Password: secrets.ProxyPassword}
		}
		d, err := proxy.SOCKS5("tcp", net.JoinHostPort(s.Host, strconv.Itoa(s.Port)), auth, direct)
		if err != nil {
			return nil, ErrConfiguration
		}
		return func(ctx context.Context, network, target string) (net.Conn, error) {
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			c, err := d.(proxy.ContextDialer).DialContext(ctx, network, target)
			if err != nil {
				return nil, routeError(ctx, err)
			}
			return &targetConn{Conn: c, target: target}, nil
		}, nil
	}
	return direct.DialContext, nil
}

func routeError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	for _, e := range []error{ErrUnknownHost, ErrChangedHost} {
		if errors.Is(err, e) {
			return e
		}
	}
	return ErrRoute
}

// pgx sends cancellation requests to RemoteAddr. A route must preserve the
// database target here, not the proxy's address or SSH's unresolved TCPAddr.
type targetAddr string

func (a targetAddr) Network() string { return "tcp" }
func (a targetAddr) String() string  { return string(a) }

type targetConn struct {
	net.Conn
	target string
}

func (c *targetConn) RemoteAddr() net.Addr { return targetAddr(c.target) }

// One forwarding channel owns one SSH socket. Channel deadlines are unsupported
// by x/crypto; a deadline therefore disposes the complete route. A timer also
// unblocks writes waiting for SSH window credit without a pending TCP write.
type sshConn struct {
	net.Conn
	raw                         net.Conn
	client                      *ssh.Client
	target                      string
	mu                          sync.Mutex
	readDeadline, writeDeadline time.Time
	timer                       *time.Timer
	generation                  uint64
	closed, timedOut            bool
}

func (c *sshConn) Close() error {
	c.mu.Lock()
	c.closed = true
	if c.timer != nil {
		c.timer.Stop()
	}
	c.mu.Unlock()
	err := c.raw.Close()
	_ = c.client.Close()
	_ = c.Conn.Close()
	return err
}
func (c *sshConn) RemoteAddr() net.Addr { return targetAddr(c.target) }
func (c *sshConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	return n, c.deadlineError(err)
}
func (c *sshConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	return n, c.deadlineError(err)
}
func (c *sshConn) deadlineError(err error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil && c.timedOut {
		return os.ErrDeadlineExceeded
	}
	return err
}
func (c *sshConn) SetDeadline(t time.Time) error      { return c.setDeadline(t, true, true) }
func (c *sshConn) SetReadDeadline(t time.Time) error  { return c.setDeadline(t, true, false) }
func (c *sshConn) SetWriteDeadline(t time.Time) error { return c.setDeadline(t, false, true) }
func (c *sshConn) setDeadline(t time.Time, read, write bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return net.ErrClosed
	}
	if read {
		c.readDeadline = t
	}
	if write {
		c.writeDeadline = t
	}
	c.generation++
	generation := c.generation
	if c.timer != nil {
		c.timer.Stop()
	}
	next := c.readDeadline
	if next.IsZero() || (!c.writeDeadline.IsZero() && c.writeDeadline.Before(next)) {
		next = c.writeDeadline
	}
	if !next.IsZero() {
		c.timer = time.AfterFunc(time.Until(next), func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			if c.closed || c.generation != generation {
				return
			}
			c.timedOut = true
			_ = c.raw.Close()
		})
	}
	return nil
}
