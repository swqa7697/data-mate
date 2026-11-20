package mongodb_test

import (
	"net"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/database"
	"github.com/swqa7697/data-mate/internal/testsupport/transportfixture"
	"github.com/swqa7697/data-mate/internal/transport"
)

// Each explicit route carries Data Mate's own TLS session to the server, pools
// it like a direct connection and closes it with its pool.
func transportAcceptance(t *testing.T, f *fixture, p config.Profile) {
	t.Helper()
	target := net.JoinHostPort("127.0.0.1", strconv.Itoa(f.port))
	for _, route := range []string{"ssh", "socks5"} {
		options := transportfixture.Options{User: "fixture", Password: "synthetic-route-secret", Resolve: func(string) string { return target }}
		peer := transportfixture.New(t, route, options)
		q := p
		q.Transport.TLS = config.TLS{Mode: "verify-full", CAFile: filepath.Join(f.root, "ca.crt")}
		var secrets transport.Credentials
		if route == "ssh" {
			q.Transport.SSH = peer.SSHConfig("password")
			secrets.SSHPassword = options.Password
		} else {
			q.Transport.Proxy = peer.ProxyConfig()
			secrets.ProxyPassword = options.Password
		}
		a := f.access(q).WithTransport(secrets, peer.KnownHosts())
		d, r := newDriver(t)
		ready, err := d.Test(t.Context(), a)
		if err != nil || !ready.TLS || ready.Stage != "read_only" {
			t.Fatalf("%s TLS readiness: %+v %v", route, ready, err)
		}
		res, err := d.FindDocuments(t.Context(), a, database.FindRequest{Database: "app", Collection: "items", RowLimit: 1})
		if err != nil || res.DocumentCount != 1 {
			t.Fatalf("%s pooled find: %v", route, err)
		}
		// Idle transports and credentials are retired with their pool.
		r.Invalidate(q.ID)
		deadline := time.NewTimer(3 * time.Second)
		for peer.Active() != 0 {
			select {
			case <-peer.Closed:
			case <-deadline.C:
				t.Fatalf("%s idle transport survived invalidation", route)
			}
		}
		deadline.Stop()
		r.Close()
		peer.Close()
		t.Logf("%s: verified TLS readiness, pooled find and idle retirement passed", route)
	}
}
