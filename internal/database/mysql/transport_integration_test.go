package mysql

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
	"github.com/swqa7697/data-mate/internal/testsupport/transportfixture"
	"github.com/swqa7697/data-mate/internal/transport"
	"golang.org/x/crypto/ssh"
)

// Each explicit route carries the client library's connection, including MySQL's
// in-protocol TLS upgrade, and closes with its pool.
func transportAcceptance(t *testing.T, f *fixture, p config.Profile) {
	t.Helper()
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
	for _, kind := range []string{"ssh-password", "ssh-key", "socks-auth", "socks-none"} {
		protocol := "ssh"
		if kind == "socks-auth" || kind == "socks-none" {
			protocol = "socks"
		}
		options := transportfixture.Options{User: "fixture", Password: "synthetic-route-secret", PublicKey: signer.PublicKey(), Resolve: func(string) string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(f.port)) }}
		if kind == "socks-none" {
			options.User, options.Password = "", ""
		}
		peer := transportfixture.New(t, protocol, options)
		secrets := transport.Credentials{}
		profile := p
		if protocol == "ssh" {
			auth := "password"
			secrets.SSHPassword = options.Password
			if kind == "ssh-key" {
				auth = "key"
				secrets = transport.Credentials{SSHPrivateKey: string(pem.EncodeToMemory(block)), SSHKeyPassphrase: "synthetic-passphrase"}
			}
			profile.Transport.SSH = peer.SSHConfig(auth)
		} else {
			profile.Transport.Proxy = peer.ProxyConfig()
			secrets.ProxyPassword = options.Password
		}
		d := driverWith(t, f.flavor)
		for _, tlsMode := range []string{"disabled", "verify-full"} {
			profile.Transport.TLS = config.TLS{Mode: tlsMode}
			if tlsMode == "verify-full" {
				profile.Transport.TLS.CAFile = filepath.Join(f.root, "ca.crt")
			}
			a := database.NewAccess(profile, f.password).WithTransport(secrets, peer.KnownHosts())
			result, err := d.Query(t.Context(), a, database.QueryRequest{SQL: "SELECT 1 + 2 AS total"})
			// MySQL types 1 + 2 as BIGINT (an exact string), MariaDB as INT (a number).
			if err != nil || result.RowCount != 1 || fmt.Sprint(result.Rows[0][0]) != "3" {
				t.Fatalf("%s/%s accepted query: %#v %v", kind, tlsMode, result.Rows, err)
			}
			_, err = d.Query(t.Context(), a, database.QueryRequest{SQL: "SELECT app.bump()"})
			requireCode(t, err, contracts.ReadOnlyViolation)
		}
		bad := profile
		bad.Connection.Host = "database.invalid"
		_, err = d.Test(t.Context(), database.NewAccess(bad, f.password).WithTransport(secrets, peer.KnownHosts()))
		requireCode(t, err, contracts.ConnectFailed)
		// Idle transports and credentials are retired with their pool.
		d.pools.Invalidate(profile.ID)
		deadline := time.NewTimer(3 * time.Second)
		for peer.Active() != 0 {
			select {
			case <-peer.Closed:
			case <-deadline.C:
				t.Fatal("idle transport survived invalidation")
			}
		}
		deadline.Stop()
		d.pools.Close()
		peer.Close()
		t.Logf("%s: plaintext/TLS query, read-only rejection, bad hostname and idle retirement passed", kind)
	}
}
