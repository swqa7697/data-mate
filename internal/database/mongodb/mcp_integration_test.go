package mongodb_test

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
	"github.com/swqa7697/data-mate/internal/testsupport/mcpfixture"
)

// documentToolAcceptance drives the four MongoDB tools through a real MCP
// session against the seeded server. Every result must match its tool's output
// schema or the error schema, and an error must leave the session usable.
func documentToolAcceptance(t *testing.T, router *database.Router, a database.Access) {
	t.Helper()
	session := mcpfixture.Connect(t, router, a)
	alias := a.Profile.Alias
	call := func(name string, args map[string]any, code contracts.Code) []byte {
		t.Helper()
		if name != "list_connections" {
			args["connection"] = alias
		}
		r, err := session.CallTool(t.Context(), &sdk.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatal(name, err)
		}
		raw, err := json.Marshal(r.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		if code == "" {
			if r.IsError || contracts.Validate(name+".output", raw) != nil {
				t.Fatalf("MCP %s response: %.500s", name, raw)
			}
			return raw
		}
		var body struct {
			Error contracts.Failure `json:"error"`
		}
		_ = json.Unmarshal(raw, &body)
		if !r.IsError || body.Error.Code != code || contracts.Validate("error", raw) != nil {
			t.Fatalf("MCP %s wanted %s: %.300s", name, code, raw)
		}
		probe, err := session.CallTool(t.Context(), &sdk.CallToolParams{Name: "find_documents",
			Arguments: map[string]any{"connection": alias, "database": "app", "collection": "items", "row_limit": 1}})
		if err != nil || probe.IsError {
			t.Fatal("database failure ended MCP session", code, err)
		}
		return raw
	}
	decode := func(raw []byte, out any) {
		t.Helper()
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatal(err)
		}
	}

	var connections struct {
		Connections []map[string]json.RawMessage `json:"connections"`
	}
	decode(call("list_connections", map[string]any{}, ""), &connections)
	if len(connections.Connections) != 1 || connections.Connections[0]["database"] != nil {
		t.Fatal("list_connections exposed the authentication database", connections)
	}

	// Paging covers every seeded collection the reader may list and nothing
	// from databases it cannot read.
	seen := map[string]string{}
	var cursor *string
	for i := 0; ; i++ {
		args := map[string]any{"page_size": 1}
		if cursor != nil {
			args["cursor"] = *cursor
		}
		var page database.CollectionPage
		decode(call("list_collections", args, ""), &page)
		if len(page.Collections) > 1 {
			t.Fatal("collection page overflow")
		}
		for _, c := range page.Collections {
			seen[c.Database+"."+c.Name] = c.Type
		}
		if cursor = page.NextCursor; cursor == nil {
			break
		}
		if i == 100 {
			t.Fatal("collection paging did not terminate")
		}
	}
	want := map[string]string{"app.items": "collection", "app.v_items": "view", "app.capped_log": "collection", "app.metrics": "timeseries",
		"app.validated": "collection", "app.a.b": "collection", "app.codecs": "collection", "app.big": "collection",
		"app.large": "collection", "hidden.target": "collection"}
	for name, kind := range seen {
		db, coll, _ := strings.Cut(name, ".")
		if kind != want[name] && ((db != "app" && db != "hidden") || !strings.HasPrefix(coll, "system.")) {
			t.Fatalf("listing leaked or mistyped %s (%s): %v", name, kind, seen)
		}
	}
	for name, kind := range want {
		if seen[name] != kind {
			t.Fatalf("listing lost %s: %v", name, seen)
		}
	}
	list := func(db string) []database.CollectionInfo {
		t.Helper()
		var page database.CollectionPage
		decode(call("list_collections", map[string]any{"database": db}, ""), &page)
		if page.NextCursor != nil {
			t.Fatal("single page returned a cursor")
		}
		return page.Collections
	}
	if got := list("hidden"); !slices.Equal(got, []database.CollectionInfo{{Database: "hidden", Name: "target", Type: "collection"}}) {
		t.Fatal("exact database filter", got)
	}
	if got := list("secret"); len(got) != 0 {
		t.Fatal("unreadable database listed", got)
	}
	// A collection-level grant lists exactly that collection.
	narrow := a
	narrow.Profile.ID = "12345678-1234-1234-1234-123456789a03"
	narrow.Profile.Connection.Username = "narrow"
	page, err := router.ListCollections(t.Context(), narrow, database.CollectionPageRequest{})
	if err != nil || page.NextCursor != nil || !slices.Equal(page.Collections, []database.CollectionInfo{{Database: "app", Name: "items", Type: "collection"}}) {
		t.Fatalf("collection-level grant listing: %+v %v", page, err)
	}

	describe := func(db, coll string) (database.CollectionDescription, map[string]json.RawMessage) {
		t.Helper()
		var desc database.CollectionDescription
		decode(call("describe_collection", map[string]any{"database": db, "collection": coll}, ""), &desc)
		options := map[string]json.RawMessage{}
		decode(desc.Options, &options)
		return desc, options
	}
	desc, _ := describe("app", "items")
	specs := map[string]map[string]json.RawMessage{}
	for _, raw := range desc.Indexes {
		var spec map[string]json.RawMessage
		decode(raw, &spec)
		var name string
		decode(spec["name"], &name)
		specs[name] = spec
	}
	names := slices.Sorted(maps.Keys(specs))
	if !slices.Equal(names, []string{"_id_", "items_geo", "items_group_qty", "items_hashed", "items_hidden", "items_name", "items_name_ci",
		"items_partial", "items_text", "items_ttl", "items_wildcard"}) || desc.Type != "collection" ||
		string(specs["items_name"]["unique"]) != "true" || string(specs["items_hidden"]["hidden"]) != "true" ||
		string(specs["items_ttl"]["expireAfterSeconds"]) != `{"$numberInt":"3600"}` || specs["items_partial"]["partialFilterExpression"] == nil {
		t.Fatalf("items index specifications: %s", desc.Indexes)
	}
	desc, options := describe("app", "v_items")
	var pipeline []json.RawMessage
	decode(options["pipeline"], &pipeline)
	if desc.Type != "view" || string(options["viewOn"]) != `"items"` || len(pipeline) != 2 || len(desc.Indexes) != 0 {
		t.Fatalf("view description: %+v", desc)
	}
	if _, options = describe("app", "capped_log"); string(options["capped"]) != "true" {
		t.Fatal("capped option missing", options)
	}
	desc, options = describe("app", "metrics")
	var series struct {
		TimeField string `json:"timeField"`
	}
	decode(options["timeseries"], &series)
	if desc.Type != "timeseries" || series.TimeField != "ts" {
		t.Fatalf("time-series description: %+v", desc)
	}
	if _, options = describe("app", "validated"); options["validator"] == nil || string(options["validationLevel"]) != `"moderate"` || options["collation"] == nil {
		t.Fatal("validator options missing", options)
	}
	describe("app", "a.b")
	call("describe_collection", map[string]any{"database": "app", "collection": "missing"}, contracts.PermissionDenied)
	call("describe_collection", map[string]any{"database": "secret", "collection": "rows"}, contracts.PermissionDenied)

	ids := func(raw []byte) (database.DocumentResult, []string) {
		t.Helper()
		var res database.DocumentResult
		decode(raw, &res)
		var out []string
		for _, doc := range res.Documents {
			var d struct {
				ID struct {
					N string `json:"$numberInt"`
				} `json:"_id"`
			}
			decode(doc, &d)
			out = append(out, d.ID.N)
		}
		return res, out
	}
	res, got := ids(call("find_documents", map[string]any{"database": "app", "collection": "items",
		"filter":     json.RawMessage(`{"group":"a","qty":{"$gte":{"$numberInt":"10"}}}`),
		"projection": json.RawMessage(`{"name":1}`), "sort": json.RawMessage(`{"_id":-1}`), "skip": 1, "row_limit": 2}, ""))
	if !slices.Equal(got, []string{"9", "7"}) || !res.Truncated || !strings.Contains(string(res.Documents[0]), `"name":"item-09"`) ||
		strings.Contains(string(res.Documents[0]), "qty") {
		t.Fatalf("find with filter, projection, sort and skip: %s", res.Documents)
	}
	if res, got = ids(call("find_documents", map[string]any{"database": "app", "collection": "v_items"}, "")); len(got) != 6 || res.Truncated {
		t.Fatal("view find", got)
	}
	if res, _ = ids(call("find_documents", map[string]any{"database": "app", "collection": "big"}, "")); res.DocumentCount != 50000 || !res.Truncated {
		t.Fatal("default 50000 document cap not enforced", res.DocumentCount)
	}
	call("find_documents", map[string]any{"database": "app", "collection": "items", "row_limit": 50001}, contracts.InvalidArgument)
	call("find_documents", map[string]any{"database": "app", "collection": "items", "filter": json.RawMessage(`{"_id":9223372036854775808}`)}, contracts.InvalidArgument)
	call("find_documents", map[string]any{"database": "app", "collection": "items", "filter": json.RawMessage(`{"$where":"true"}`)}, contracts.QueryUnsupported)
	call("find_documents", map[string]any{"database": "secret", "collection": "rows"}, contracts.PermissionDenied)

	aggregate := func(coll string, code contracts.Code, stages ...string) []json.RawMessage {
		t.Helper()
		pipeline := make([]json.RawMessage, len(stages))
		for i, s := range stages {
			pipeline[i] = json.RawMessage(s)
		}
		raw := call("aggregate_documents", map[string]any{"database": "app", "collection": coll, "pipeline": pipeline}, code)
		if code != "" {
			return nil
		}
		var res database.DocumentResult
		decode(raw, &res)
		return res.Documents
	}
	// Six group a items plus the validated document, each joined to itself.
	docs := aggregate("items", "",
		`{"$match":{"group":"a"}}`,
		`{"$unionWith":{"coll":"validated","pipeline":[{"$project":{"name":1}}]}}`,
		`{"$lookup":{"from":"items","let":{"id":"$_id"},"pipeline":[{"$match":{"$expr":{"$eq":["$_id","$$id"]}}}],"as":"self"}}`,
		`{"$facet":{"c":[{"$count":"n"}]}}`, `{"$unwind":"$c"}`, `{"$replaceWith":"$c"}`)
	if len(docs) != 1 || string(docs[0]) != `{"n":{"$numberInt":"7"}}` {
		t.Fatalf("nested pipeline: %s", docs)
	}
	if docs = aggregate("big", "", `{"$count":"n"}`); len(docs) != 1 || string(docs[0]) != `{"n":{"$numberInt":"50001"}}` {
		t.Fatalf("count: %s", docs)
	}
	aggregate("items", contracts.ReadOnlyViolation, `{"$out":"copy"}`)
	aggregate("items", contracts.ReadOnlyViolation, `{"$unionWith":{"coll":"items","pipeline":[{"$merge":{"into":"copy"}}]}}`)
	aggregate("items", contracts.QueryUnsupported, `{"$bogus":{}}`)
	aggregate("items", contracts.QueryUnsupported, `{"$addFields":{"x":{"$function":{"body":"function () { return 1; }","args":[],"lang":"js"}}}}`)
	t.Log("MCP collection listing, descriptions, finds, aggregates and guarded rejections passed")
}
