package postgres

import (
	"context"
	"encoding/json"
	"net"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
	protocol "github.com/swqa7697/data-mate/internal/mcp"
)

// The owned fixture has immutable access. Production leases remain service-owned.
type wireBackend struct {
	driver database.Driver
	access database.Access
}

func (b *wireBackend) Work(ctx context.Context, alias string, fn func(context.Context, database.Driver, database.Access) error) error {
	if alias != b.access.Profile.Alias {
		return database.Fail(contracts.ConnectionNotFound, "connection not found", false)
	}
	return fn(ctx, b.driver, b.access)
}
func (b *wireBackend) Connections(_ context.Context, fn func([]protocol.Connection) error) error {
	p := b.access.Profile
	return fn([]protocol.Connection{{Alias: p.Alias, Driver: p.Driver, Database: p.Connection.Database, Scope: p.Scope}})
}

// Regression ladder 2: extend the existing PostgreSQL 16/18 acceptance scenario
// through MCP. Query/codec cases stay in the shared driver corpus.
func mcpAcceptance(t *testing.T, d *Driver, a database.Access) {
	t.Helper()
	limits := config.DefaultLimits()
	a.Profile.Limits = &limits
	backend := &wireBackend{d, a}
	server, client := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- protocol.Serve(t.Context(), server, backend, "fixture") }()
	session, err := sdk.NewClient(&sdk.Implementation{Name: "integration", Version: "1"}, nil).Connect(t.Context(), &sdk.IOTransport{Reader: client, Writer: client}, nil)
	if err != nil {
		client.Close()
		t.Fatal(err)
	}
	defer func() { session.Close(); <-done }()
	call := func(name string, args map[string]any, code contracts.Code) []byte {
		t.Helper()
		r, err := session.CallTool(t.Context(), &sdk.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(r.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		if code == "" {
			if r.IsError || contracts.Validate(name+".output", raw) != nil {
				t.Fatalf("MCP %s response: %s", name, raw)
			}
		} else {
			var body struct {
				Error contracts.Failure `json:"error"`
			}
			_ = json.Unmarshal(raw, &body)
			if !r.IsError || body.Error.Code != code || contracts.Validate("error", raw) != nil {
				t.Fatalf("MCP %s wanted %s: %s", name, code, raw)
			}
		}
		return raw
	}
	call("list_connections", map[string]any{}, "")
	raw := call("list_tables", map[string]any{"connection": "fixture", "page_size": 1}, "")
	var page database.TablePage
	_ = json.Unmarshal(raw, &page)
	if page.NextCursor == nil {
		t.Fatal("missing metadata cursor")
	}
	call("list_tables", map[string]any{"connection": "fixture", "cursor": *page.NextCursor}, "")
	raw = call("describe_table", map[string]any{"connection": "fixture", "schema": "app", "table": "items"}, "")
	var desc database.Description
	_ = json.Unmarshal(raw, &desc)
	if len(desc.Relationships) != 0 {
		t.Fatal("hidden relationship exposed")
	}
	call("describe_table", map[string]any{"connection": "fixture", "schema": "hidden", "table": "target"}, contracts.ScopeDenied)
	call("list_objects", map[string]any{"connection": "fixture", "kind": "type", "schema": "app"}, "")
	call("describe_object", map[string]any{"connection": "fixture", "kind": "type", "schema": "app", "name": "custom"}, "")
	call("describe_object", map[string]any{"connection": "fixture", "kind": "routine", "schema": "app", "name": "policy_probe", "identity_arguments": ""}, "")
	call("query", map[string]any{"connection": "fixture", "sql": "select count(*) from app.items"}, "")
	call("query", map[string]any{"connection": "fixture", "sql": "select app.policy_probe()"}, contracts.ReadOnlyViolation)

	call("query", map[string]any{"connection": "fixture", "sql": "select $1::int8,$2::jsonb", "parameters": []any{"9007199254740993", map[string]any{"ok": true}}}, "")
	// Grant/scope checks are repeated on later calls in the same initialized session.
	d.Invalidate(a.Profile.ID)
	call("list_tables", map[string]any{"connection": "fixture", "cursor": *page.NextCursor}, contracts.StaleCursor)
	t.Log("MCP SDK metadata, hidden endpoints, cursor invalidation, query and shared read-only rejection passed")
}
