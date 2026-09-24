package transport

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/testsupport/transportfixture"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func event(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("fixture did not finish owned work")
	}
}

func echoPeer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Go(func() { defer c.Close(); c.SetDeadline(time.Now().Add(5 * time.Second)); _, _ = io.Copy(c, c) })
		}
	}()
	t.Cleanup(func() { ln.Close(); <-done; wg.Wait() })
	return ln.Addr().String()
}

// No prior scenario owns SSH/SOCKS5 protocol negotiation or channel lifetime.
// These peers verify the public dial boundary; PG policy/TLS remain integration.
func TestOptionalRoutes(t *testing.T) {
	echo := echoPeer(t)
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKeyWithPassphrase(key, "fixture", []byte("synthetic-passphrase"))
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := string(pem.EncodeToMemory(block))
	// Ambient routes must have no effect, even when set to broken endpoints.
	t.Setenv("SSH_AUTH_SOCK", "/unavailable/agent")
	t.Setenv("ALL_PROXY", "socks5://invalid.invalid:1")
	t.Setenv("HTTP_PROXY", "http://invalid.invalid:1")
	for _, kind := range []string{"ssh-password", "ssh-key", "socks-auth", "socks-none"} {
		t.Run(kind, func(t *testing.T) {
			options := transportfixture.Options{User: "fixture", Password: "synthetic-route-secret", PublicKey: signer.PublicKey(), Resolve: func(string) string { return echo }}
			protocol := "socks"
			if strings.HasPrefix(kind, "ssh") {
				protocol = "ssh"
			}
			if kind == "socks-none" {
				options.User = ""
				options.Password = ""
			}
			s := transportfixture.New(t, protocol, options)
			settings := config.Transport{}
			secrets := Credentials{}
			if protocol == "ssh" {
				auth := "password"
				secrets.SSHPassword = options.Password
				if kind == "ssh-key" {
					auth = "key"
					secrets = Credentials{SSHPrivateKey: keyPEM, SSHKeyPassphrase: "synthetic-passphrase"}
				}
				settings.SSH = s.SSHConfig(auth)
			} else {
				settings.Proxy = s.ProxyConfig()
				secrets.ProxyPassword = options.Password
			}
			dial, err := Dialer(settings, secrets, s.KnownHosts())
			if err != nil {
				t.Fatal(err)
			}
			// Domain is deliberately unresolvable locally; literals retain v4/v6 form.
			for _, target := range []string{"database.invalid:5432", "127.0.0.1:5432", "[::1]:5432", "database.invalid:5432"} {
				c, err := dial(t.Context(), "tcp", target)
				if err != nil {
					t.Fatal(err)
				}
				if err = c.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
					t.Fatal(err)
				}
				if _, err = c.Write([]byte("ping")); err != nil {
					t.Fatal(err)
				}
				b := make([]byte, 4)
				if _, err = io.ReadFull(c, b); err != nil || string(b) != "ping" {
					t.Fatalf("forward: %v", err)
				}
				c.Close()
				event(t, s.Closed)
				if got := <-s.Targets; got != target {
					t.Fatalf("DNS routing changed: %q", got)
				}
				if s.Active() != 0 {
					t.Fatal("route leaked after Close")
				}
			}
			if kind != "socks-none" {
				bad := secrets
				if kind == "ssh-password" {
					bad.SSHPassword = "wrong-secret"
				} else if kind == "ssh-key" {
					bad.SSHKeyPassphrase = "wrong-secret"
				} else {
					bad.ProxyPassword = "wrong-secret"
				}
				badDial, err := Dialer(settings, bad, s.KnownHosts())
				if err == nil {
					_, err = badDial(t.Context(), "tcp", "database.invalid:5432")
					event(t, s.Closed)
				}
				if err == nil || strings.Contains(err.Error(), "wrong-secret") {
					t.Fatal("wrong credentials succeeded or leaked")
				}
			}
			// A peer disconnect ends a live route and cannot leave a blocked reader.
			c, err := dial(t.Context(), "tcp", "database.invalid:5432")
			if err != nil {
				t.Fatal(err)
			}
			s.Close()
			_ = c.SetReadDeadline(time.Now().Add(time.Second))
			if _, err = c.Read(make([]byte, 1)); err == nil {
				t.Fatal("disconnect was ignored")
			}
			c.Close()
		})
	}
	// Cancellation covers pre-SSH handshake and SSH channel-open, as well as
	// both SOCKS negotiation phases. Peers signal progress instead of sleeping.
	for _, kind := range []string{"ssh", "socks"} {
		for _, stall := range []string{"handshake", "forward"} {
			s := transportfixture.New(t, kind, transportfixture.Options{User: "fixture", Password: "synthetic", Stall: stall})
			settings := config.Transport{}
			secrets := Credentials{}
			if kind == "ssh" {
				settings.SSH = s.SSHConfig("password")
				secrets.SSHPassword = "synthetic"
			} else {
				settings.Proxy = s.ProxyConfig()
				secrets.ProxyPassword = "synthetic"
			}
			dial, err := Dialer(settings, secrets, s.KnownHosts())
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			result := make(chan error, 1)
			go func() {
				c, e := dial(ctx, "tcp", "database.invalid:5432")
				if c != nil {
					c.Close()
				}
				result <- e
			}()
			if stall == "handshake" {
				event(t, s.Accepted)
			} else {
				event(t, s.Forwarded)
			}
			cancel()
			select {
			case e := <-result:
				if !errors.Is(e, context.Canceled) {
					t.Fatalf("%s/%s: %v", kind, stall, e)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("cancel blocked")
			}
			event(t, s.Closed)
			if s.Active() != 0 {
				t.Fatal("cancel leaked peer")
			}
			s.Close()
		}
	}
	// Actual deadline expiry must also release a silent peer.
	s := transportfixture.New(t, "ssh", transportfixture.Options{User: "fixture", Stall: "handshake"})
	dial, err := Dialer(config.Transport{SSH: s.SSHConfig("password")}, Credentials{}, s.KnownHosts())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_, err = dial(ctx, "tcp", "database.invalid:5432")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	event(t, s.Closed)
	_, err = Dialer(config.Transport{SSH: s.SSHConfig("password"), Proxy: s.ProxyConfig()}, Credentials{}, s.KnownHosts())
	if !errors.Is(err, ErrConfiguration) {
		t.Fatal("combined route accepted")
	}
	// Channel.Write can wait for window credit without a blocked TCP Write.
	// Socket write deadlines alone cannot release that wait.
	blocked := transportfixture.New(t, "ssh", transportfixture.Options{User: "fixture", Stall: "stream"})
	dial, err = Dialer(config.Transport{SSH: blocked.SSHConfig("password")}, Credentials{}, blocked.KnownHosts())
	if err != nil {
		t.Fatal(err)
	}
	c, err := dial(t.Context(), "tcp", "database.invalid:5432")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	written := make(chan error, 1)
	go func() { _, e := c.Write(make([]byte, 3<<20)); written <- e }()
	if err = c.SetWriteDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-written:
		if !errors.Is(e, os.ErrDeadlineExceeded) {
			t.Fatalf("window deadline: %v", e)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("channel write survived deadline")
	}
	event(t, blocked.Closed)
}

// Pin persistence has no existing owner scenario: exercise enrollment, changed
// keys, bounded parsing and descriptor-safe publication together at this boundary.
func TestOwnedHostEnrollment(t *testing.T) {
	s := transportfixture.New(t, "ssh", transportfixture.Options{User: "fixture", Password: "synthetic"})
	settings := config.Transport{SSH: s.SSHConfig("password")}
	if _, err := Dialer(settings, Credentials{}, nil); !errors.Is(err, ErrUnknownHost) {
		t.Fatal("unknown background host accepted")
	}
	pin, err := ProbeHostKey(t.Context(), *settings.SSH, nil)
	if err != nil {
		t.Fatal(err)
	}
	event(t, s.Closed)
	path := t.TempDir()
	if err = os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := config.ResolveRoot(path, "")
	if err != nil {
		t.Fatal(err)
	}
	store, err := config.Open(t.Context(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	l, err := store.WriteLease(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	if err = SaveHostKey(l, pin); err != nil {
		t.Fatal(err)
	}
	before, err := ReadKnownHosts(l)
	if err != nil {
		t.Fatal(err)
	}
	if err = SaveHostKey(l, pin); err != nil {
		t.Fatal(err)
	}
	after, err := ReadKnownHosts(l)
	if err != nil || string(before) != string(after) {
		t.Fatal("repeat enrollment changed pins")
	}
	other := transportfixture.New(t, "ssh", transportfixture.Options{User: "fixture"})
	if err = SaveHostKey(l, HostKey{Address: pin.Address, Key: other.Signer.PublicKey()}); !errors.Is(err, ErrChangedHost) {
		t.Fatal("changed pin overwritten")
	}
	wrong := []byte(knownhosts.Line([]string{pin.Address}, other.Signer.PublicKey()) + "\n")
	if _, err = ProbeHostKey(t.Context(), *settings.SSH, wrong); !errors.Is(err, ErrChangedHost) {
		t.Fatal("changed probe accepted")
	}
	event(t, s.Closed)
	dial, err := Dialer(settings, Credentials{SSHPassword: "synthetic"}, wrong)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = dial(t.Context(), "tcp", "database.invalid:5432"); !errors.Is(err, ErrChangedHost) {
		t.Fatal("changed runtime host accepted")
	}
	event(t, s.Closed)
	for _, bad := range [][]byte{[]byte("garbage"), append(append([]byte{}, before...), before...), []byte(strings.Repeat("x", MaxKnownHostsBytes+1)), []byte(knownhosts.Line([]string{"*"}, pin.Key) + "\n")} {
		if _, err = Dialer(settings, Credentials{}, bad); !errors.Is(err, ErrKnownHosts) {
			t.Fatal("malformed pins accepted")
		}
	}
	knownPath := filepath.Join(path, "known_hosts")
	if err = os.Remove(knownPath); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "sentinel")
	if err = os.WriteFile(outside, []byte("sentinel"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(outside, knownPath); err != nil {
		t.Fatal(err)
	}
	if err = SaveHostKey(l, pin); err == nil {
		t.Fatal("followed host file symlink")
	}
	b, err := os.ReadFile(outside)
	if err != nil || string(b) != "sentinel" {
		t.Fatal("changed external file")
	}
}
