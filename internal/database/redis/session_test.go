package redis

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
	"github.com/swqa7697/data-mate/internal/database/pool"
)

// A synthetic peer can send unsupported handshakes, incomplete frames, and
// oversized lengths that the supported Docker images cannot produce.
func TestRedisConnectionBoundary(t *testing.T) {
	const secret = "synthetic-private"
	simple := func(s string) string { return "+" + s + "\r\n" }
	array := func(items ...string) string { return "*" + strconv.Itoa(len(items)) + "\r\n" + strings.Join(items, "") }
	hello := func(version, mode string) string {
		return array(simple("version"), simple(version), simple("mode"), simple(mode), simple("server"), simple("redis"), simple("proto"), ":2\r\n")
	}
	selector := func(rules string) []string {
		return []string{simple("commands"), simple(rules), simple("keys"), simple("%R~visible:*"), simple("channels"), simple("")}
	}
	acl := func(rules string, selectors ...string) string {
		items := selector(rules)
		return array(append(items, simple("flags"), array(simple("on"), simple("nopass")), simple("passwords"), array(), simple("selectors"), array(selectors...))...)
	}
	type exchange struct {
		request []any
		reply   string // empty means cancel while the peer holds the response
	}
	auth := func(reply string) exchange { return exchange{[]any{"HELLO", "2", "AUTH", "reader", secret}, reply} }
	identity := exchange{[]any{"ACL", "WHOAMI"}, simple("reader")}
	selectDB := exchange{[]any{"SELECT", "3"}, simple("OK")}
	grants := func(reply string) exchange { return exchange{[]any{"ACL", "GETUSER", "reader"}, reply} }
	fresh := func(rest ...exchange) []exchange {
		return append([]exchange{auth(hello("7.0.0", "standalone")), identity, selectDB}, rest...)
	}
	for _, tt := range []struct {
		name, stage string
		code        contracts.Code
		script      []exchange
	}{
		{"read-only passwordless account with a compact category", "read_only", "", fresh(grants(acl("-@all +@string -set")), exchange{[]any{"ACL", "CAT", "string"}, array(simple("get"), simple("set"))})},
		{"write in a separate selector", "read_only", contracts.ReadOnlyViolation, fresh(grants(acl("-@all +get", array(selector("-@all +set")...))))},
		{"unbounded future grants", "read_only", contracts.ReadOnlyViolation, fresh(grants(acl("+@all -@write")))},
		{"expanded category includes an admin subcommand", "read_only", contracts.ReadOnlyViolation, fresh(grants(acl("-@all +@admin -acl|whoami")), exchange{[]any{"ACL", "CAT", "admin"}, array(simple("acl|setuser"), simple("acl|whoami"))})},
		{"bad credentials redact upstream text", "authentication", contracts.ConnectFailed, []exchange{auth("-WRONGPASS " + secret + "\r\n")}},
		{"identity mismatch", "authentication", contracts.ConnectFailed, []exchange{auth(hello("7.0.0", "standalone")), {[]any{"ACL", "WHOAMI"}, simple("other")}}},
		{"old server", "version", contracts.QueryUnsupported, []exchange{auth(hello("6.2.1", "standalone")), identity}},
		{"cluster mode", "version", contracts.QueryUnsupported, []exchange{auth(hello("8.10.1", "cluster")), identity}},
		{"sentinel mode", "version", contracts.QueryUnsupported, []exchange{auth(hello("7.0.4", "sentinel")), identity}},
		{"oversized bulk length", "authentication", contracts.ResourceLimit, []exchange{auth("$9000000\r\n")}},
		{"oversized aggregate length", "authentication", contracts.ResourceLimit, []exchange{auth("*9000000\r\n")}},
		{"cancellation during authentication", "authentication", contracts.Cancelled, []exchange{auth("")}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				done <- func() error {
					c, err := ln.Accept()
					if err != nil {
						return err
					}
					defer c.Close()
					deadline, _ := ctx.Deadline()
					if err := c.SetDeadline(deadline); err != nil {
						return err
					}
					reader := bufio.NewReader(c)
					for i, step := range tt.script {
						r := responseReader{r: reader, remaining: 1 << 16}
						got, err := r.value(0)
						if err != nil || !reflect.DeepEqual(got, step.request) {
							return fmt.Errorf("unexpected request at exchange %d", i)
						}
						if step.reply == "" {
							cancel()
							break
						}
						if _, err := io.WriteString(c, step.reply); err != nil {
							return err
						}
					}
					// Keep the connection live until the driver's isolated pool closes it.
					_, err = reader.ReadByte()
					if !errors.Is(err, io.EOF) {
						return fmt.Errorf("unexpected trailing traffic or cleanup error: %w", err)
					}
					return nil
				}()
			}()
			r, err := pool.New(pool.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			port := ln.Addr().(*net.TCPAddr).Port
			p := config.Profile{Driver: "redis", Alias: "fixture", ID: "12345678-1234-1234-1234-123456789abc", Connection: config.Connection{Host: "127.0.0.1", Port: port, Username: "reader", Database: "3"}, Transport: config.Transport{TLS: config.TLS{Mode: "disabled"}}}
			out, err := New(r).Test(ctx, database.NewAccess(p, secret))
			if out.Stage != tt.stage {
				t.Fatalf("stage %q, error %v", out.Stage, err)
			}
			var safe *database.Error
			if tt.code == "" {
				if err != nil || out.ServerVersion != 70000 {
					t.Fatalf("unexpected readiness %+v, error %v", out, err)
				}
			} else if !errors.As(err, &safe) || safe.Code != tt.code || strings.Contains(err.Error(), secret) {
				t.Fatalf("incorrect safe error %v", err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Approval expires as soon as the final physical connection disconnects,
// even when no operation has checked the connection out since its last use.
func TestIdleRedisSessionObservesDisconnect(t *testing.T) {
	conn, peer := net.Pipe()
	s := newSession(conn, conn)
	defer s.Close()
	peer.Close()
	select {
	case <-s.Closed():
		if s.Reusable() {
			t.Fatal("disconnected Redis session is still reusable")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("idle Redis session did not report peer disconnect")
	}
}
