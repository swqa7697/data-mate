package redis

import (
	"bufio"
	"bytes"
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
// oversized lengths, and scripted SCAN batches that a live fixture cannot guarantee.
func TestRedisConnectionBoundary(t *testing.T) {
	const secret = "synthetic-private"
	simple := func(s string) string { return "+" + s + "\r\n" }
	array := func(items ...string) string { return "*" + strconv.Itoa(len(items)) + "\r\n" + strings.Join(items, "") }
	hello := func(version, mode string) string {
		return array(simple("version"), simple(version), simple("mode"), simple(mode), simple("server"), simple("redis"), simple("proto"), ":2\r\n")
	}
	type exchange struct {
		request []any
		reply   string // empty means cancel while the peer holds the response
	}
	auth := func(reply string) exchange { return exchange{[]any{"HELLO", "2", "AUTH", "reader", secret}, reply} }
	handshake := auth(hello("7.0.0", "standalone"))
	longText := strings.Repeat("a", 95) + "茶"
	longBinary := strings.Repeat("b", 96) + "\xff"
	hashProbe := array(simple("0"), array())
	for _, tt := range []struct {
		name, stage string
		code        contracts.Code
		script      []exchange
	}{
		{"managed account without ACL inspection", "read_only", "", []exchange{handshake, {[]any{"SELECT", "3"}, simple("OK")}}},
		{"database selection denied", "read_only", contracts.PermissionDenied, []exchange{handshake, {[]any{"SELECT", "3"}, "-NOPERM " + secret + "\r\n"}}},
		{"bad credentials redact upstream text", "authentication", contracts.ConnectFailed, []exchange{auth("-WRONGPASS " + secret + "\r\n")}},
		{"bounded sample despite duplicates, denied and surplus keys", "describe", "", []exchange{
			handshake, {[]any{"SELECT", "3"}, simple("OK")},
			{[]any{"SCAN", "0", "COUNT", "7"}, array(simple("9"), array(simple("s:1"), simple("s:1"), simple("other:x"), simple("s:2"), simple("other:y"), simple("s:\x01"), simple("s:\xff"), simple("over:limit")))},
			{[]any{"TYPE", "s:1"}, simple("string")}, {[]any{"GETRANGE", "s:1", "1", "0"}, "$0\r\n\r\n"},
			{[]any{"TYPE", "other:x"}, simple("string")}, {[]any{"GETRANGE", "other:x", "1", "0"}, "-NOPERM\r\n"},
			{[]any{"TYPE", "s:2"}, simple("hash")}, {[]any{"HSCAN", "s:2", "0", "MATCH", "[]", "COUNT", "1"}, array(simple("0"), array())},
			{[]any{"TYPE", "other:y"}, simple("zset")}, {[]any{"ZRANGE", "other:y", "1", "0", "WITHSCORES"}, array()},
			{[]any{"TYPE", "s:\x01"}, simple("string")}, {[]any{"GETRANGE", "s:\x01", "1", "0"}, "$0\r\n\r\n"},
			{[]any{"TYPE", "s:\xff"}, simple("string")}, {[]any{"GETRANGE", "s:\xff", "1", "0"}, "$0\r\n\r\n"},
			// A fresh describe can find only ungrouped hashes. Names must remain
			// useful without another Redis command or an unbounded result.
			{[]any{"SCAN", "0", "COUNT", "7"}, array(simple("0"), array(simple(longText), simple(longBinary), simple("active"), simple("pending")))},
			{[]any{"TYPE", longText}, simple("hash")}, {[]any{"HSCAN", longText, "0", "MATCH", "[]", "COUNT", "1"}, hashProbe},
			{[]any{"TYPE", longBinary}, simple("hash")}, {[]any{"HSCAN", longBinary, "0", "MATCH", "[]", "COUNT", "1"}, hashProbe},
			{[]any{"TYPE", "active"}, simple("hash")}, {[]any{"HSCAN", "active", "0", "MATCH", "[]", "COUNT", "1"}, hashProbe},
			{[]any{"TYPE", "pending"}, simple("hash")}, {[]any{"HSCAN", "pending", "0", "MATCH", "[]", "COUNT", "1"}, hashProbe},
		}},
		{"old server", "version", contracts.QueryUnsupported, []exchange{auth(hello("6.2.1", "standalone"))}},
		{"cluster mode", "version", contracts.QueryUnsupported, []exchange{auth(hello("8.10.1", "cluster"))}},
		{"sentinel mode", "version", contracts.QueryUnsupported, []exchange{auth(hello("7.0.4", "sentinel"))}},
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
			if tt.stage == "describe" {
				limits := config.DefaultLimits()
				limits.MaxRows = 7
				p.Limits = &limits
				driver, access := New(r), database.NewAccess(p, secret)
				out, err := driver.DescribeDatabase(ctx, access)
				want := &database.RedisKeyspace{ObservedKeys: 5, Groups: []database.RedisKeyGroup{
					{Pattern: "s:*", Type: "hash", Count: 1}, {Pattern: "s:*", Type: "string", Count: 1},
					{Type: "string", Count: 2, Examples: []database.RedisKeyExample{
						{KeyBytes: database.KeyBytes{Value: "s:\x01", Encoding: "utf8"}},
						{KeyBytes: database.KeyBytes{Value: "czr/", Encoding: "base64"}},
					}},
					{Type: "zset", Count: 1, Examples: []database.RedisKeyExample{
						{KeyBytes: database.KeyBytes{Value: "other:y", Encoding: "utf8"}},
					}},
				}}
				if err != nil || !reflect.DeepEqual(out.Keyspace, want) {
					t.Fatalf("unexpected readable sample: %+v, error %v", out.Keyspace, err)
				}
				out, err = driver.DescribeDatabase(ctx, access)
				want = &database.RedisKeyspace{ObservedKeys: 4, Groups: []database.RedisKeyGroup{
					{Type: "hash", Count: 4, Examples: []database.RedisKeyExample{
						{KeyBytes: database.KeyBytes{Value: strings.Repeat("a", 95), Encoding: "utf8"}, Truncated: true},
						{KeyBytes: database.KeyBytes{Value: strings.Repeat("YmJi", 32), Encoding: "base64"}, Truncated: true},
						{KeyBytes: database.KeyBytes{Value: "active", Encoding: "utf8"}},
					}},
				}}
				if err != nil || !reflect.DeepEqual(out.Keyspace, want) {
					t.Fatalf("unexpected ungrouped hash sample: %+v, error %v", out.Keyspace, err)
				}
				r.Close()
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				return
			}
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

	t.Run("wire guard rejects commands before buffering and frames binary keys", func(t *testing.T) {
		var wire bytes.Buffer
		w := bufio.NewWriter(&wire)
		for _, args := range [][]string{
			nil, {"SET", "k", "v"}, {"DEL", "k"}, {"EVAL", "return 1", "0"},
			{"ACL", "GETUSER", "reader"}, {"FUTURE.COMMAND"}, {"HELLO", "2", "SETNAME", "name"},
		} {
			var safe *database.Error
			if err := writeCommand(w, args); !errors.As(err, &safe) || safe.Code != contracts.ReadOnlyViolation || w.Buffered() != 0 || wire.Len() != 0 {
				t.Fatalf("denied command %v buffered %d bytes, wrote %d: %v", args, w.Buffered(), wire.Len(), err)
			}
		}
		const key = "k\r\n*2\r\n$3\r\nDEL\r\n$1\r\nx\r\n\x00"
		if err := writeCommand(w, []string{"GETRANGE", key, "0", "1"}); err != nil {
			t.Fatal(err)
		}
		r := responseReader{r: bufio.NewReader(&wire), remaining: maxReplyBytes}
		got, err := r.value(0)
		if err != nil || !reflect.DeepEqual(got, []any{"GETRANGE", key, "0", "1"}) || r.r.Buffered() != 0 || wire.Len() != 0 {
			t.Fatalf("binary key escaped the read command: %v, %v", got, err)
		}
	})
}

// Detect disconnects even when no operation has checked the session out since its last use.
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
