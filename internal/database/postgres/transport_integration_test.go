package postgres

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"net"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
	"github.com/swqa7697/data-mate/internal/testsupport/transportfixture"
	"github.com/swqa7697/data-mate/internal/transport"
	"golang.org/x/crypto/ssh"
)

// Extend the owned PG16/18 scenario with representative shared-driver results,
// fresh policy rejection, TLS and cleanup through each route. P5 owns the corpus.
func transportAcceptance(t *testing.T, p config.Profile, password, fixtureRoot string, admin *pgx.Conn) {
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
		options := transportfixture.Options{User: "fixture", Password: "synthetic-route-secret", PublicKey: signer.PublicKey(), Resolve: func(string) string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(p.Connection.Port)) }}
		if kind == "socks-none" {
			options.User = ""
			options.Password = ""
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
		d := driver(t)
		for _, tlsMode := range []string{"disabled", "verify-full"} {
			profile.Transport.TLS = config.TLS{Mode: tlsMode}
			if tlsMode == "verify-full" {
				profile.Transport.TLS.CAFile = filepath.Join(fixtureRoot, "ca.crt")
			}
			a := database.NewAccess(profile, password).WithTransport(secrets, peer.KnownHosts())
			result, err := d.Query(t.Context(), a, database.QueryRequest{SQL: "SELECT 1 + 2 AS total"})
			if err != nil || result.RowCount != 1 || result.Rows[0][0] != int64(3) {
				t.Fatalf("%s/%s accepted query: %#v %v", kind, tlsMode, result.Rows, err)
			}
			// A previous success on the same pool cannot cache semantic permission.
			func() {
				if _, err := admin.Exec(t.Context(), "ALTER FUNCTION pg_catalog.int4pl(int4,int4) CALLED ON NULL INPUT"); err != nil {
					t.Fatal(err)
				}
				defer func() {
					ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					defer cancel()
					if _, err := admin.Exec(ctx, "ALTER FUNCTION pg_catalog.int4pl(int4,int4) RETURNS NULL ON NULL INPUT"); err != nil {
						t.Fatal(err)
					}
				}()
				_, err = d.Query(t.Context(), a, database.QueryRequest{SQL: "SELECT 1 + 2 AS total"})
				requireCode(t, err, contracts.QueryUnsupported)
			}()
			if _, err = d.Query(t.Context(), a, database.QueryRequest{SQL: "SELECT 1 + 2 AS total"}); err != nil {
				t.Fatal(err)
			}
		}
		for _, badTLS := range []string{"hostname", "ca"} {
			bad := profile
			if badTLS == "hostname" {
				bad.Connection.Host = "database.invalid"
			} else {
				bad.Transport.TLS.CAFile = filepath.Join(fixtureRoot, "bad.crt")
			}
			_, err = d.Test(t.Context(), database.NewAccess(bad, password).WithTransport(secrets, peer.KnownHosts()))
			requireCode(t, err, contracts.ConnectFailed)
		}
		a := database.NewAccess(profile, password).WithTransport(secrets, peer.KnownHosts())
		// A credential-only or pin-only change must retire an authenticated idle
		// connection even when profile revision/database password are unchanged.
		if kind == "ssh-password" || kind == "socks-auth" {
			if _, err = d.Test(t.Context(), a); err != nil {
				t.Fatal(err)
			}
			wrong := secrets
			if kind == "ssh-password" {
				wrong.SSHPassword = "wrong-secret"
			} else {
				wrong.ProxyPassword = "wrong-secret"
			}
			_, err = d.Test(t.Context(), a.WithTransport(wrong, peer.KnownHosts()))
			requireCode(t, err, contracts.ConnectFailed)
			if _, err = d.Test(t.Context(), a); err != nil {
				t.Fatal(err)
			}
			if kind == "ssh-password" {
				_, err = d.Test(t.Context(), a.WithTransport(secrets, nil))
				requireCode(t, err, contracts.ConnectFailed)
			}
		}
		var pid uint32
		err = d.run(t.Context(), a, func(ctx context.Context, tx pgx.Tx, _ int) error {
			if err := tx.QueryRow(ctx, "SELECT pg_catalog.pg_backend_pid()").Scan(&pid); err != nil {
				return err
			}
			short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
			defer cancel()
			_, err := tx.Exec(short, "SELECT pg_catalog.pg_sleep(10)")
			return err
		})
		requireCode(t, err, contracts.QueryTimeout)
		waitBackendStopped(t, admin, pid)
		if _, err = d.Test(t.Context(), a); err != nil {
			t.Fatal("route did not recover", err)
		}
		// Idle transports and credentials must be retired with their pool.
		d.Invalidate(profile.ID)
		deadline := time.NewTimer(3 * time.Second)
		for peer.Active() != 0 {
			select {
			case <-peer.Closed:
			case <-deadline.C:
				t.Fatal("idle transport survived invalidation")
			}
		}
		deadline.Stop()
		d.Close()
		peer.Close()
		t.Logf("P6 %s: plaintext/TLS query, fresh catalog rejection, bad CA/hostname, database cancellation and idle retirement passed", kind)
	}
}
