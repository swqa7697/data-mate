package mysql

import (
	"context"
	"encoding/json"
	"net"
	"slices"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
	protocol "github.com/swqa7697/data-mate/internal/mcp"
)

func validate(t *testing.T, contract string, v any) {
	t.Helper()
	encoded, _ := json.Marshal(v)
	if err := contracts.Validate(contract, encoded); err != nil {
		t.Fatalf("%s contract: %v\n%s", contract, err, encoded)
	}
}

// Catalog visibility follows server privileges across every database the
// account can reach; descriptions use MySQL-native shapes.
func catalogAcceptance(t *testing.T, f *fixture, d *Driver, a database.Access) {
	t.Helper()
	seen := map[string]string{}
	req := database.PageRequest{PageSize: 50}
	firstCursor := ""
	for {
		page, err := d.ListTables(t.Context(), a, req)
		if err != nil {
			t.Fatal(err)
		}
		validate(t, "list_tables.output", page)
		for _, table := range page.Tables {
			key := table.Schema + "/" + table.Name
			if _, ok := seen[key]; ok {
				t.Fatal("duplicate page row")
			}
			seen[key] = table.Kind
		}
		if page.NextCursor == nil {
			break
		}
		req.Cursor = *page.NextCursor
		if firstCursor == "" {
			firstCursor = req.Cursor
		}
	}
	if seen["app/items"] != "table" || seen["app/v"] != "view" || seen["hidden/target"] != "table" || seen["Dot.Schema/a.b"] != "table" || seen["information_schema/TABLES"] != "system_view" {
		t.Fatalf("catalog: %v", seen)
	}
	if _, ok := seen["secret/hidden_rows"]; ok {
		t.Fatal("database without grants is visible")
	}
	if _, ok := seen["app/counter"]; ok {
		t.Fatal("sequence listed as a table")
	}
	_, err := d.ListTables(t.Context(), a, database.PageRequest{Cursor: firstCursor, Schema: "app"})
	requireCode(t, err, contracts.StaleCursor)

	desc, err := d.DescribeTable(t.Context(), a, config.Table{Schema: "app", Name: "items"})
	if err != nil {
		t.Fatal(err)
	}
	validate(t, "describe_table.output", desc)
	columns := map[string]Column{}
	for _, c := range desc.Columns {
		columns[c.Name] = c
	}
	if len(desc.Columns) != 6 || !columns["id"].AutoIncrement || columns["doubled"].Generated == nil || columns["updated"].OnUpdate == nil || columns["name"].Default == nil || *columns["name"].Default != "'it''s'" || desc.Engine == nil || *desc.Engine != "InnoDB" {
		t.Fatalf("columns: %+v", desc.Columns)
	}
	if len(desc.Keys) != 2 || len(desc.Relationships) != 1 || desc.Relationships[0].Target != (config.Table{Schema: "hidden", Name: "target"}) {
		t.Fatalf("keys: %+v %+v", desc.Keys, desc.Relationships)
	}
	kinds := map[string]string{}
	for _, c := range desc.Constraints {
		kinds[c.Name] = c.Kind
	}
	indexes := map[string]string{}
	for _, i := range desc.Indexes {
		indexes[i.Name] = i.Definition
	}
	if kinds["items_target"] != "foreign" || kinds["items_amount"] != "check" || kinds["items_name"] != "unique" || indexes["PRIMARY"] == "" || indexes["items_prefix"] != "KEY `items_prefix` (`name`(4)) USING BTREE" {
		t.Fatalf("definitions: %+v %+v", desc.Constraints, desc.Indexes)
	}
	view, err := d.DescribeTable(t.Context(), a, config.Table{Schema: "app", Name: "v"})
	if err != nil || view.Kind != "view" || view.ViewDefinition == nil {
		t.Fatalf("view: %+v %v", view, err)
	}
	for _, missing := range []config.Table{{Schema: "app", Name: "missing"}, {Schema: "secret", Name: "hidden_rows"}} {
		_, err = d.DescribeTable(t.Context(), a, missing)
		requireCode(t, err, contracts.PermissionDenied)
	}

	objects, err := d.ListObjects(t.Context(), a, database.ObjectPageRequest{PageRequest: database.PageRequest{Schema: "app"}})
	if err != nil {
		t.Fatal(err)
	}
	validate(t, "list_objects.output", objects)
	want := []Object{{"function", "app", "bump"}, {"function", "app", "double_it"}, {"procedure", "app", "report"}}
	if f.flavor == MariaDB {
		want = []Object{{"function", "app", "bump"}, {"sequence", "app", "counter"}, {"function", "app", "double_it"}, {"procedure", "app", "report"}}
	}
	if !slices.Equal(objects.Objects, want) {
		t.Fatalf("objects: %+v", objects.Objects)
	}
	page, err := d.ListObjects(t.Context(), a, database.ObjectPageRequest{PageRequest: database.PageRequest{Schema: "app", PageSize: 1}, Kind: "function"})
	if err != nil || len(page.Objects) != 1 || page.NextCursor == nil {
		t.Fatalf("object page: %+v %v", page, err)
	}
	page, err = d.ListObjects(t.Context(), a, database.ObjectPageRequest{PageRequest: database.PageRequest{Schema: "app", PageSize: 1, Cursor: *page.NextCursor}, Kind: "function"})
	if err != nil || len(page.Objects) != 1 || page.Objects[0].Name != "double_it" {
		t.Fatalf("object cursor: %+v %v", page, err)
	}
	fn, err := d.DescribeObject(t.Context(), a, database.ObjectRequest{Kind: "function", Schema: "app", Name: "double_it"})
	if err != nil {
		t.Fatal(err)
	}
	validate(t, "describe_object.output", fn)
	r := fn.Routine
	if len(r.Parameters) != 1 || r.Parameters[0].Name != "x" || r.Parameters[0].Mode != nil || r.ReturnType == nil || !r.Deterministic || r.SQLDataAccess != "no_sql" || r.Security != "definer" || r.Language != "sql" {
		t.Fatalf("function: %+v", r)
	}
	if f.flavor == MySQL && r.Definition == nil {
		t.Fatal("SHOW_ROUTINE did not expose routine source")
	}
	proc, err := d.DescribeObject(t.Context(), a, database.ObjectRequest{Kind: "procedure", Schema: "app", Name: "report"})
	if err != nil || len(proc.Routine.Parameters) != 2 || *proc.Routine.Parameters[1].Mode != "out" || proc.Routine.ReturnType != nil {
		t.Fatalf("procedure: %+v %v", proc.Routine, err)
	}
	if f.flavor == MariaDB {
		seq, err := d.DescribeObject(t.Context(), a, database.ObjectRequest{Kind: "sequence", Schema: "app", Name: "counter"})
		if err != nil {
			t.Fatal(err)
		}
		validate(t, "describe_object.output", seq)
		if seq.Sequence.Start != "5" || seq.Sequence.Increment != "2" || seq.Sequence.Cycle {
			t.Fatalf("sequence: %+v", seq.Sequence)
		}
	}
	for _, bad := range []database.ObjectRequest{{Kind: "routine", Schema: "app", Name: "bump"}, {Kind: "type", Schema: "app", Name: "x"}} {
		_, err = d.DescribeObject(t.Context(), a, bad)
		requireCode(t, err, contracts.InvalidArgument)
	}
	_, err = d.DescribeObject(t.Context(), a, database.ObjectRequest{Kind: "function", Schema: "app", Name: "missing"})
	requireCode(t, err, contracts.PermissionDenied)

	described, err := d.DescribeDatabase(t.Context(), a)
	if err != nil {
		t.Fatal(err)
	}
	validate(t, "db-describe.output", described)
	schemas := map[string]database.SchemaDescription{}
	for _, s := range described.Schemas {
		schemas[s.Name] = s
	}
	app := schemas["app"]
	if described.Database != "" || described.Driver != f.flavor.Name() || len(schemas) != 3 || schemas["hidden"].Name == "" || schemas["Dot.Schema"].Name == "" {
		t.Fatalf("databases: %+v", described)
	}
	if !slices.Contains(app.Indexes, database.IndexName{Name: "PRIMARY", Table: "items"}) || !slices.Contains(app.Functions, database.CatalogName{Name: "double_it"}) || slices.Contains(app.Functions, database.CatalogName{Name: "report"}) {
		t.Fatalf("app database: %+v", app)
	}
	if f.flavor == MariaDB && !slices.Contains(app.Sequences, database.CatalogName{Name: "counter"}) {
		t.Fatal("sequence missing from description")
	}
}

// wireBackend serves the owned fixture through the real MCP handlers.
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
	return fn([]protocol.Connection{{Alias: p.Alias, Driver: p.Driver, Database: p.Connection.Database}})
}

// Every tool's MySQL-shaped output must satisfy its driver-keyed contract.
func mcpAcceptance(t *testing.T, d *Driver, a database.Access) {
	t.Helper()
	// Decoded production profiles always carry limits; the MCP handler reads them.
	limits := config.DefaultLimits()
	a.Profile.Limits = &limits
	backend := &wireBackend{database.NewRouter(d.pools, map[string]database.Operations{a.Profile.Driver: d.Operations()}), a}
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
		raw, _ := json.Marshal(r.StructuredContent)
		if code == "" {
			if r.IsError || contracts.Validate(name+".output", raw) != nil {
				t.Fatalf("MCP %s response: %s", name, raw)
			}
		} else {
			var body struct {
				Error contracts.Failure `json:"error"`
			}
			_ = json.Unmarshal(raw, &body)
			if !r.IsError || body.Error.Code != code {
				t.Fatalf("MCP %s wanted %s: %s", name, code, raw)
			}
		}
		return raw
	}
	call("list_connections", map[string]any{}, "")
	call("list_tables", map[string]any{"connection": "fixture", "schema": "app"}, "")
	call("describe_table", map[string]any{"connection": "fixture", "schema": "app", "table": "items"}, "")
	call("list_objects", map[string]any{"connection": "fixture", "kind": "function"}, "")
	call("describe_object", map[string]any{"connection": "fixture", "kind": "procedure", "schema": "app", "name": "report"}, "")
	call("describe_object", map[string]any{"connection": "fixture", "kind": "routine", "schema": "app", "name": "report", "identity_arguments": ""}, contracts.InvalidArgument)
	call("query", map[string]any{"connection": "fixture", "sql": "select name from app.items where id = ?", "parameters": []any{1}}, "")
	call("query", map[string]any{"connection": "fixture", "sql": "select app.bump()"}, contracts.ReadOnlyViolation)
	t.Log("MCP SDK metadata, MySQL-shaped contracts, query and read-only rejection passed")
}
