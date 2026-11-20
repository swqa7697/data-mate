package postgres

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
	"github.com/swqa7697/data-mate/internal/testsupport/mcpfixture"
)

// Regression ladder 2: extend the existing PostgreSQL 16/18 acceptance scenario
// through MCP. Query/codec cases stay in the shared driver corpus.
func mcpAcceptance(t *testing.T, d *Driver, a database.Access) {
	t.Helper()
	router := database.NewRouter(d.pools, map[string]database.Operations{a.Profile.Driver: d.Operations()})
	session := mcpfixture.Connect(t, router, a)
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
				t.Fatalf("MCP %s response: %.200s", name, raw)
			}
		} else {
			var body struct {
				Error contracts.Failure `json:"error"`
			}
			_ = json.Unmarshal(raw, &body)
			if !r.IsError || body.Error.Code != code || contracts.Validate("error", raw) != nil {
				t.Fatalf("MCP %s wanted %s: %.200s", name, code, raw)
			}
			probe, e := session.CallTool(t.Context(), &sdk.CallToolParams{Name: "query", Arguments: map[string]any{"connection": "fixture", "sql": "SELECT 1"}})
			if e != nil || probe.IsError {
				t.Fatal("database failure ended MCP session", code, e)
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
	var desc Description
	_ = json.Unmarshal(raw, &desc)
	if len(desc.Relationships) != 1 {
		t.Fatal("readable cross-schema relationship missing")
	}
	call("describe_table", map[string]any{"connection": "fixture", "schema": "hidden", "table": "target"}, "")
	call("list_objects", map[string]any{"connection": "fixture", "kind": "type", "schema": "app"}, "")
	call("describe_object", map[string]any{"connection": "fixture", "kind": "type", "schema": "app", "name": "custom"}, "")
	call("describe_object", map[string]any{"connection": "fixture", "kind": "routine", "schema": "app", "name": "policy_probe", "identity_arguments": ""}, "")
	call("query", map[string]any{"connection": "fixture", "sql": "select count(*) from app.items"}, "")
	call("query", map[string]any{"connection": "fixture", "sql": "select app.policy_probe()"}, contracts.ReadOnlyViolation)
	call("query", map[string]any{"connection": "fixture", "sql": "SELECT 1/0"}, contracts.InvalidArgument)
	call("query", map[string]any{"connection": "fixture", "sql": "SELECT * FROM items"}, contracts.QueryUnsupported)
	call("query", map[string]any{"connection": "fixture", "sql": "SELECT ("}, contracts.InvalidArgument)

	call("query", map[string]any{"connection": "fixture", "sql": "select $1::int8,$2::jsonb", "parameters": []any{"9007199254740993", map[string]any{"ok": true}}}, "")
	// Extend the same real-database MCP scenario across the former inbound,
	// parameter, result-envelope, wire-body and row limits on both supported majors.
	call("query", map[string]any{"connection": "fixture", "sql": "SELECT 1 /*" + strings.Repeat("x", 300<<10) + "*/"}, "")
	call("query", map[string]any{"connection": "fixture", "sql": "SELECT $1::text", "parameters": []any{strings.Repeat("x", 300<<10)}}, "")
	call("query", map[string]any{"connection": "fixture", "sql": "SELECT $1::json", "parameters": []json.RawMessage{json.RawMessage(strings.Repeat("[", 65) + "0" + strings.Repeat("]", 65))}}, "")
	parameters, placeholders := make([]any, 257), make([]string, 257)
	for i := range parameters {
		parameters[i] = i
		placeholders[i] = fmt.Sprintf("$%d::int4", i+1)
	}
	call("query", map[string]any{"connection": "fixture", "sql": "SELECT cardinality(ARRAY[" + strings.Join(placeholders, ",") + "])", "parameters": parameters}, "")
	call("query", map[string]any{"connection": "fixture", "sql": "SELECT repeat('x',2200000)"}, "")
	raw = call("query", map[string]any{"connection": "fixture", "sql": "SELECT * FROM pg_catalog.generate_series(1,50001)"}, "")
	var limited database.QueryResult
	if json.Unmarshal(raw, &limited) != nil || limited.RowCount != 50000 || !limited.Truncated {
		t.Fatal("default 50000 row cap not enforced")
	}
	call("query", map[string]any{"connection": "fixture", "sql": "SELECT 1"}, "")
	// Cursor validity is checked on later calls in the same initialized session.
	d.pools.Invalidate(a.Profile.ID)
	call("list_tables", map[string]any{"connection": "fixture", "cursor": *page.NextCursor}, contracts.StaleCursor)
	t.Log("MCP SDK metadata, cross-schema endpoints, cursor invalidation, query and shared read-only rejection passed")
}
