package redis_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
	"github.com/swqa7697/data-mate/internal/database/pool"
	"github.com/swqa7697/data-mate/internal/database/redis"
	"github.com/swqa7697/data-mate/internal/testsupport/mcpfixture"
	"github.com/swqa7697/data-mate/internal/testsupport/transportfixture"
	"github.com/swqa7697/data-mate/internal/transport"
)

// The writer can modify private keys and inspect their types, but cannot read
// their values or inspect ACLs. Every public key tool must respect those grants.
// Run this scenario only against the container created by make test-integration.
func TestRedisIntegration(t *testing.T) {
	path := os.Getenv("DATA_MATE_REDIS_FIXTURE")
	if path == "" {
		t.Skip("requires the owned Redis Docker fixture")
	}
	var f struct {
		Root, Container, Owner, Image, Driver string
		Port                                  int
	}
	b, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(b, &f) != nil || f.Driver != "redis" ||
		!strings.HasPrefix(f.Owner, "dm-redis-") || f.Container != f.Owner+"-db" || f.Root != filepath.Dir(path) {
		t.Fatal("invalid Redis fixture manifest")
	}
	docker := func(args ...string) string {
		t.Helper()
		output, err := exec.CommandContext(t.Context(), "docker", args...).Output()
		if err != nil {
			t.Fatal("owned fixture operation failed", err)
		}
		return strings.TrimSpace(string(output))
	}
	if docker("inspect", "--format", `{{index .Config.Labels "com.data-mate.fixture"}}`, f.Container) != f.Owner ||
		docker("port", f.Container, "6379/tcp") != "127.0.0.1:"+strconv.Itoa(f.Port) {
		t.Fatal("Redis fixture ownership or endpoint mismatch")
	}
	// Only the owned container receives administrative fixture setup via stdin.
	cmd := exec.CommandContext(t.Context(), "docker", "exec", "-i", f.Container, "sh", "-ec",
		`export REDISCLI_AUTH="$(cat /fixture/admin-password)"; exec redis-cli -e --quoted-input`)
	cmd.Stdin = strings.NewReader(`SELECT 0
SET view:wrongdatabase unrelated
SELECT 1
SET private:secret hidden
SET "view:\xff" "\x00\xff"
SET view:empty "" PX 300000
HSET view:hash "" 1 b 2 c 3
RPUSH view:list a b c
SADD view:set "" b c
ZADD view:zset 1 a 2 b 3 c
XADD view:stream 1-0 f a f b
`)
	if err := cmd.Run(); err != nil {
		t.Fatal("cannot seed owned Redis fixture", err)
	}
	password, err := os.ReadFile(filepath.Join(f.Root, "reader-password"))
	if err != nil {
		t.Fatal(err)
	}
	limits := config.DefaultLimits()
	p := config.Profile{ID: "12345678-1234-1234-1234-123456789abc", Alias: "fixture", Driver: "redis",
		Connection: config.Connection{Host: "localhost", Port: f.Port, Database: "1", Username: "writer"},
		Transport:  config.Transport{TLS: config.TLS{Mode: "disabled"}}, Limits: &limits}
	access := func(p config.Profile) database.Access {
		return database.NewAccess(p, strings.TrimSpace(string(password)))
	}
	r, err := pool.New(pool.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	d := redis.New(r)
	router := database.NewRouter(r, map[string]database.Operations{"redis": d})
	a := access(p)
	ready, err := d.Test(t.Context(), a)
	if err != nil || ready.Stage != "read_only" {
		t.Fatalf("writer admission on %s: %v %v", f.Image, ready, err)
	}
	majorMinor := strconv.Itoa(ready.ServerVersion/10000) + "." + strconv.Itoa(ready.ServerVersion/100%100)
	if f.Image != "redis:"+majorMinor {
		t.Fatalf("fixture %s reports Redis %s", f.Image, majorMinor)
	}
	tlsEndpoint := docker("port", f.Container, "6380/tcp")
	if !strings.HasPrefix(tlsEndpoint, "127.0.0.1:") {
		t.Fatal("unexpected fixture TLS binding")
	}
	port, err := strconv.Atoi(strings.TrimPrefix(tlsEndpoint, "127.0.0.1:"))
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range []string{"direct", "ssh", "socks5"} {
		q := p
		q.Connection.Port = port
		q.Transport.TLS = config.TLS{Mode: "verify-full", CAFile: filepath.Join(f.Root, "ca.crt")}
		routed := access(q)
		if route != "direct" {
			opts := transportfixture.Options{User: "fixture", Password: "synthetic-route", Resolve: func(string) string { return tlsEndpoint }}
			peer := transportfixture.New(t, route, opts)
			var secrets transport.Credentials
			if route == "ssh" {
				q.Transport.SSH = peer.SSHConfig("password")
				secrets.SSHPassword = opts.Password
			} else {
				q.Transport.Proxy = peer.ProxyConfig()
				secrets.ProxyPassword = opts.Password
			}
			routed = access(q).WithTransport(secrets, peer.KnownHosts())
		}
		res, err := d.Test(t.Context(), routed)
		if err != nil || !res.TLS {
			t.Fatalf("%s TLS readiness: %v", route, err)
		}
	}
	desc, err := router.DescribeDatabase(t.Context(), a)
	raw, _ := json.Marshal(desc)
	if err != nil || contracts.Validate("db-describe.output", raw) != nil || bytes.Contains(raw, []byte("private:")) || bytes.Contains(raw, []byte("view:hash")) {
		t.Fatalf("CLI keyspace: %s %v", raw, err)
	}
	if desc.Keyspace == nil || desc.Keyspace.ObservedKeys < 1 || desc.Keyspace.ObservedKeys > 7 {
		t.Fatalf("sample includes keys outside the readable fixture: %s", raw)
	}
	count := 0
	for _, group := range desc.Keyspace.Groups {
		if group.Pattern != "" && group.Pattern != "view:*" {
			t.Fatalf("sample leaked an unreadable pattern: %s", raw)
		}
		count += group.Count
	}
	if count != desc.Keyspace.ObservedKeys {
		t.Fatalf("sample groups do not account for the readable keys: %s", raw)
	}

	verifyKeyTools(t, mcpfixture.Connect(t, router, a), p.Alias)
	page, err := router.ListKeys(t.Context(), a, database.KeyPageRequest{PageSize: 1})
	if err != nil || page.NextCursor == nil {
		t.Fatal("missing test cursor", err)
	}
	r.Invalidate(p.ID)
	_, err = router.ListKeys(t.Context(), a, database.KeyPageRequest{PageSize: 1, Cursor: *page.NextCursor})
	var safe *database.Error
	if !errors.As(err, &safe) || safe.Code != contracts.StaleCursor {
		t.Fatal("accepted stale cursor", err)
	}
}

func verifyKeyTools(t *testing.T, session *sdk.ClientSession, alias string) {
	t.Helper()
	call := func(tool string, args map[string]any, out any) {
		t.Helper()
		args["connection"] = alias
		res, err := session.CallTool(t.Context(), &sdk.CallToolParams{Name: tool, Arguments: args})
		if err != nil {
			t.Fatal(tool, err)
		}
		raw, err := json.Marshal(res.StructuredContent)
		if err != nil || res.IsError || contracts.Validate(tool+".output", raw) != nil || json.Unmarshal(raw, out) != nil {
			t.Fatalf("%s response: %.500s", tool, raw)
		}
	}
	seen := map[database.KeyBytes]string{}
	var cursor *string
	for i := 0; i < 100; i++ {
		args := map[string]any{"page_size": 1}
		if cursor != nil {
			args["cursor"] = *cursor
		}
		var page database.KeyPage
		call("list_keys", args, &page)
		if len(page.Keys) > 1 {
			t.Fatal("key page overflow")
		}
		for _, k := range page.Keys {
			seen[k.Key] = k.Type
		}
		cursor = page.NextCursor
		if cursor == nil {
			break
		}
	}
	binaryKey := database.KeyBytes{Encoding: "base64", Value: base64.StdEncoding.EncodeToString([]byte("view:\xff"))}
	expected := map[database.KeyBytes]string{binaryKey: "string", textKey("view:empty"): "string", textKey("view:hash"): "hash", textKey("view:list"): "list", textKey("view:set"): "set", textKey("view:zset"): "zset", textKey("view:stream"): "stream"}
	if cursor != nil || !maps.Equal(seen, expected) {
		t.Fatal("discovery lost an authorized key or leaked another key", seen)
	}
	for key, typ := range seen {
		var metadata database.KeyDescription
		call("describe_key", map[string]any{"key": key}, &metadata)
		validTTL := metadata.TTLMS == -1
		if key.Value == "view:empty" {
			validTTL = metadata.TTLMS > 0 && metadata.TTLMS <= 300000
		}
		if metadata.Type != typ || metadata.Key != key || metadata.Length == nil || !validTTL {
			t.Fatalf("metadata for %q: %+v", key, metadata)
		}
		var entries []json.RawMessage
		args := map[string]any{"key": key, "page_size": 1}
		for i := 0; i < 10; i++ {
			var page struct {
				Items      []json.RawMessage `json:"items"`
				NextCursor *string           `json:"next_cursor"`
			}
			call("read_key", args, &page)
			entries = append(entries, page.Items...)
			if page.NextCursor == nil {
				break
			}
			args["cursor"] = *page.NextCursor
			if i == 9 {
				t.Fatal("read did not terminate")
			}
		}
		verifyKeyValue(t, key, typ, *metadata.Length, entries)
	}
	var missing database.KeyDescription
	call("describe_key", map[string]any{"key": database.KeyBytes{Encoding: "utf8", Value: "view:missing"}}, &missing)
	if missing.Type != "none" || missing.Length != nil || missing.TTLMS != -2 {
		t.Fatal("missing key reported as present")
	}
	// TYPE is allowed for these names. A denied value read must hide even existence.
	for _, tool := range []string{"describe_key", "read_key"} {
		var denied contracts.Failure
		for i, key := range []string{"private:secret", "private:missing"} {
			args := map[string]any{"connection": alias, "key": textKey(key)}
			result, err := session.CallTool(t.Context(), &sdk.CallToolParams{Name: tool, Arguments: args})
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(result.StructuredContent)
			var body struct{ Error contracts.Failure }
			if err != nil || !result.IsError || json.Unmarshal(raw, &body) != nil || body.Error.Code != contracts.PermissionDenied || bytes.Contains(raw, []byte(key)) {
				t.Fatalf("%s disclosed %q without a permitted value read: %.300s", tool, key, raw)
			}
			if i == 0 {
				denied = body.Error
			} else if body.Error != denied {
				t.Fatalf("%s distinguished inaccessible keys: %+v and %+v", tool, denied, body.Error)
			}
		}
	}
}

func textKey(s string) database.KeyBytes { return database.KeyBytes{Encoding: "utf8", Value: s} }

func verifyKeyValue(t *testing.T, key database.KeyBytes, typ string, length int64, entries []json.RawMessage) {
	t.Helper()
	switch typ {
	case "string":
		if key.Value == "view:empty" {
			if length != 0 || len(entries) != 1 {
				t.Fatal("empty string")
			}
			break
		}
		var value []byte
		for _, entry := range entries {
			var b database.KeyBytes
			if json.Unmarshal(entry, &b) != nil {
				t.Fatal("invalid string chunk")
			}
			chunk := []byte(b.Value)
			if b.Encoding == "base64" {
				var err error
				chunk, err = base64.StdEncoding.DecodeString(b.Value)
				if err != nil {
					t.Fatal(err)
				}
			}
			value = append(value, chunk...)
		}
		if !bytes.Equal(value, []byte{0, 255}) {
			t.Fatal("binary round trip")
		}
	case "stream":
		var record struct {
			ID     string
			Fields []struct{ Field, Value database.KeyBytes }
		}
		if len(entries) != 1 || json.Unmarshal(entries[0], &record) != nil || record.ID != "1-0" || len(record.Fields) != 2 ||
			record.Fields[0].Field != textKey("f") || record.Fields[1].Field != textKey("f") ||
			record.Fields[0].Value != textKey("a") || record.Fields[1].Value != textKey("b") {
			t.Fatal("stream entry lost field order or multiplicity")
		}
	default:
		var items []string
		for _, raw := range entries {
			switch typ {
			case "list", "set":
				var item database.KeyBytes
				if err := json.Unmarshal(raw, &item); err != nil {
					t.Fatal(err)
				}
				items = append(items, item.Value)
			case "hash":
				var pair struct{ Field, Value database.KeyBytes }
				if err := json.Unmarshal(raw, &pair); err != nil {
					t.Fatal(err)
				}
				items = append(items, pair.Field.Value+"="+pair.Value.Value)
			case "zset":
				var pair struct {
					Member database.KeyBytes
					Score  string
				}
				if err := json.Unmarshal(raw, &pair); err != nil {
					t.Fatal(err)
				}
				items = append(items, pair.Member.Value+"="+pair.Score)
			}
		}
		want := []string{"a", "b", "c"}
		switch typ {
		case "hash":
			want = []string{"=1", "b=2", "c=3"}
		case "set":
			want = []string{"", "b", "c"}
		case "zset":
			want = []string{"a=1", "b=2", "c=3"}
		}
		if typ == "set" || typ == "hash" {
			slices.Sort(items)
		}
		if length != 3 || !slices.Equal(items, want) {
			t.Fatalf("%s collection pagination: %v", typ, items)
		}
	}
}
