package postgres

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
)

const maxCatalogObjects = 4096

// Selection is always parameterized. It is applied before keyset LIMIT, so sparse
// scopes do not require materializing the database catalog in the application.
const visibleSQL = `n.nspname NOT LIKE 'pg\_%' AND n.nspname<>'information_schema'
 AND c.relkind IN ('r','p','v','m','f') AND c.relpersistence<>'t'
 AND pg_catalog.has_schema_privilege(n.oid,'USAGE') AND pg_catalog.has_table_privilege(c.oid,'SELECT')
 AND ($1::boolean OR n.nspname::text=ANY($2::text[]) OR EXISTS (SELECT FROM pg_catalog.jsonb_to_recordset($3::jsonb) AS s(schema text,name text) WHERE s.schema=n.nspname AND s.name=c.relname))`

func scopeArgs(s config.Scope) []any {
	tables := s.Tables
	if tables == nil {
		tables = []config.Table{}
	}
	b, _ := json.Marshal(tables)
	schemas := s.Schemas
	if schemas == nil {
		schemas = []string{}
	}
	return []any{s.Mode == "all", schemas, string(b)}
}
func validName(s string) bool {
	return s != "" && len(s) <= 63 && utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}
func kind(s string) string {
	switch s {
	case "r":
		return "table"
	case "p":
		return "partitioned_table"
	case "v":
		return "view"
	case "m":
		return "materialized_view"
	case "f":
		return "foreign_table"
	}
	return "other"
}

// OIDs are checked with pg_catalog namespace/type kind as well as canonical IDs.
func supportedType(oid uint32, ns, typtype string) bool {
	if ns != "pg_catalog" || typtype != "b" {
		return false
	}
	switch oid {
	case 16, 17, 20, 21, 23, 25, 114, 700, 701, 1042, 1043, 1082, 1083, 1114, 1184, 1700, 2950, 3802:
		return true
	}
	return false
}

type cursor struct {
	Version  int
	ID       string
	Revision config.Revision
	Filter   string
	Schema   string
	Name     string
	Epoch    uint64
}

func (d *Driver) encodeCursor(c cursor) string {
	b, _ := json.Marshal(c)
	mac := hmac.New(sha256.New, d.key[:])
	mac.Write(b)
	return base64.RawURLEncoding.EncodeToString(append(b, mac.Sum(nil)...))
}
func (d *Driver) decodeCursor(s string, want cursor) (cursor, error) {
	bad := func() (cursor, error) {
		return cursor{}, database.Fail(contracts.StaleCursor, "metadata cursor is invalid or stale", false)
	}
	if len(s) > 2048 {
		return bad()
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(b) < 33 {
		return bad()
	}
	body, sig := b[:len(b)-32], b[len(b)-32:]
	mac := hmac.New(sha256.New, d.key[:])
	mac.Write(body)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return bad()
	}
	var c cursor
	if json.Unmarshal(body, &c) != nil || c.Version != 1 || c.ID != want.ID || c.Revision != want.Revision || c.Filter != want.Filter || c.Epoch != want.Epoch || !validName(c.Schema) || !validName(c.Name) {
		return bad()
	}
	return c, nil
}

// ListTables intersects scope and privileges before returning a keyset page.
func (d *Driver) ListTables(ctx context.Context, a database.Access, req database.PageRequest) (database.TablePage, error) {
	a, rev, err := normalized(a)
	if err != nil {
		return database.TablePage{}, err
	}
	if req.PageSize == 0 {
		req.PageSize = 100
	}
	if req.PageSize < 1 || req.PageSize > 500 || (req.Schema != "" && !validName(req.Schema)) {
		return database.TablePage{}, database.Fail(contracts.InvalidArgument, "invalid metadata page request", false)
	}
	d.mu.Lock()
	epoch := d.epoch
	d.mu.Unlock()
	pos := cursor{Version: 1, ID: a.Profile.ID, Revision: rev, Filter: req.Schema, Epoch: epoch}
	if req.Cursor != "" {
		pos, err = d.decodeCursor(req.Cursor, pos)
		if err != nil {
			return database.TablePage{}, err
		}
	}
	out := database.TablePage{Connection: a.Profile.Alias, Tables: []database.Table{}}
	err = d.run(ctx, a, func(ctx context.Context, tx pgx.Tx, _ int) error {
		args := append(scopeArgs(a.Profile.Scope), req.Schema, pos.Schema, pos.Name, req.PageSize+1)
		rows, err := tx.Query(ctx, `SELECT c.oid,n.nspname,c.relname,c.relkind::text FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE `+visibleSQL+`
 AND ($4::text='' OR n.nspname=$4) AND (n.nspname::text COLLATE "C",c.relname::text COLLATE "C") > ($5::text COLLATE "C",$6::text COLLATE "C")
 ORDER BY n.nspname::text COLLATE "C",c.relname::text COLLATE "C" LIMIT $7`, args...)
		if err != nil {
			return err
		}
		type item struct {
			oid   uint32
			table database.Table
		}
		items := []item{}
		for rows.Next() {
			var i item
			var k string
			if err := rows.Scan(&i.oid, &i.table.Schema, &i.table.Name, &k); err != nil {
				rows.Close()
				return err
			}
			i.table.Kind = kind(k)
			items = append(items, i)
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return err
		}
		more := len(items) > req.PageSize
		if more {
			items = items[:req.PageSize]
		}
		for _, i := range items {
			info, err := inspect(ctx, tx, i.oid, a.Profile.Scope)
			if err != nil {
				return err
			}
			i.table.Supported = info.reason == ""
			i.table.Reason = info.reason
			out.Tables = append(out.Tables, i.table)
		}
		if more {
			last := out.Tables[len(out.Tables)-1]
			pos.Schema = last.Schema
			pos.Name = last.Name
			s := d.encodeCursor(pos)
			out.NextCursor = &s
		}
		return payloadBound(out, a)
	})
	if err != nil {
		return database.TablePage{}, err
	}
	return out, nil
}

type relationInfo struct {
	columns []database.Column
	reason  string
	oid     uint32
}

func columns(ctx context.Context, tx pgx.Tx, oid uint32) ([]database.Column, bool, error) {
	rows, err := tx.Query(ctx, `SELECT a.attname,t.typname,n.nspname,t.typtype::text,t.oid,NOT a.attnotnull,a.attgenerated::text
 FROM pg_catalog.pg_attribute a JOIN pg_catalog.pg_type t ON t.oid=a.atttypid JOIN pg_catalog.pg_namespace n ON n.oid=t.typnamespace
 WHERE a.attrelid=$1 AND a.attnum>0 AND NOT a.attisdropped ORDER BY a.attnum LIMIT 1601`, oid)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	out := []database.Column{}
	ok := true
	for rows.Next() {
		var c database.Column
		var ns, tp, generated string
		var tid uint32
		if err := rows.Scan(&c.Name, &c.Type, &ns, &tp, &tid, &c.Nullable, &generated); err != nil {
			return nil, false, err
		}
		c.Supported = supportedType(tid, ns, tp) && generated == ""
		if !c.Supported {
			ok = false
		}
		if ns != "pg_catalog" {
			c.Type = "unsupported"
		}
		out = append(out, c)
	}
	if len(out) > 1600 {
		return nil, false, database.Fail(contracts.ResourceLimit, "column limit exceeded", false)
	}
	return out, ok, rows.Err()
}
func inspect(ctx context.Context, tx pgx.Tx, oid uint32, scope config.Scope) (relationInfo, error) {
	out := relationInfo{oid: oid}
	cols, ok, err := columns(ctx, tx, oid)
	if err != nil {
		return out, err
	}
	out.columns = cols
	if !ok {
		out.reason = "unsupported column type or generated column"
	}
	// UNION deduplicates DAG paths and bounds the client materialization. Pending
	// detach edges are surfaced; Query separately locks and rechecks frozen scans.
	rows, err := tx.Query(ctx, `WITH RECURSIVE tree(oid) AS (SELECT $1::oid UNION SELECT i.inhrelid FROM pg_catalog.pg_inherits i JOIN tree t ON i.inhparent=t.oid)
 SELECT c.oid,n.nspname,c.relname,c.relkind::text,c.relrowsecurity,c.relpersistence::text,am.amname,
 pg_catalog.has_schema_privilege(n.oid,'USAGE') AND pg_catalog.has_table_privilege(c.oid,'SELECT'),
 EXISTS(SELECT FROM pg_catalog.pg_inherits i WHERE i.inhrelid=c.oid AND i.inhdetachpending),
 EXISTS(SELECT FROM pg_catalog.pg_rewrite r WHERE r.ev_class=c.oid),
 EXISTS(SELECT FROM pg_catalog.pg_inherits i WHERE i.inhrelid=c.oid)
 FROM tree JOIN pg_catalog.pg_class c ON c.oid=tree.oid JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace LEFT JOIN pg_catalog.pg_am am ON am.oid=c.relam
 ORDER BY c.oid LIMIT 4097`, oid)
	if err != nil {
		return out, err
	}
	type node struct {
		id                          uint32
		schema, name, k             string
		rls                         bool
		persistence                 string
		am                          *string
		read, detach, rules, parent bool
	}
	nodes := []node{}
	for rows.Next() {
		var n node
		if err := rows.Scan(&n.id, &n.schema, &n.name, &n.k, &n.rls, &n.persistence, &n.am, &n.read, &n.detach, &n.rules, &n.parent); err != nil {
			rows.Close()
			return out, err
		}
		nodes = append(nodes, n)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return out, err
	}
	if len(nodes) == 0 {
		return out, database.Fail(contracts.ScopeDenied, "relation is no longer available", true)
	}
	if len(nodes) > maxCatalogObjects {
		return out, database.Fail(contracts.ResourceLimit, "relation hierarchy exceeds limit", false)
	}
	rootPartition := false
	for _, n := range nodes {
		if n.id == oid {
			rootPartition = n.k == "p"
		}
	}
	for _, n := range nodes {
		if n.k != "r" && n.k != "p" {
			out.reason = "relation kind is not supported"
		}
		if n.persistence == "t" || (n.k == "r" && (n.am == nil || *n.am != "heap")) || n.rules {
			out.reason = "relation execution features are not supported"
		}
		if n.detach {
			out.reason = "partition hierarchy is changing"
		}
		if n.rls && (len(nodes) > 1 || n.k == "p" || n.parent) {
			out.reason = "RLS with partitioning or inheritance is not supported"
		}
		if !n.read || strings.HasPrefix(n.schema, "pg_") || n.schema == "information_schema" || (!rootPartition && !scope.ContainsName(n.schema, n.name)) {
			out.reason = "hierarchy contains unavailable relations"
		}
		if n.id != oid {
			_, supported, err := columns(ctx, tx, n.id)
			if err != nil {
				return out, err
			}
			if !supported {
				out.reason = "hierarchy has unsupported columns"
			}
		}
	}
	return out, nil
}

// DescribeTable omits expressions and filters foreign-key endpoints by scope.
func (d *Driver) DescribeTable(ctx context.Context, a database.Access, name config.Table) (database.Description, error) {
	if !validName(name.Schema) || !validName(name.Name) {
		return database.Description{}, database.Fail(contracts.InvalidArgument, "invalid relation name", false)
	}
	out := database.Description{Connection: a.Profile.Alias, Schema: name.Schema, Table: name.Name, Columns: []database.Column{}, Keys: []database.Key{}, Relationships: []database.Relationship{}}
	err := d.run(ctx, a, func(ctx context.Context, tx pgx.Tx, _ int) error {
		args := append(scopeArgs(a.Profile.Scope), name.Schema, name.Name)
		var oid uint32
		var k string
		err := tx.QueryRow(ctx, `SELECT c.oid,c.relkind::text FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE `+visibleSQL+` AND n.nspname=$4 AND c.relname=$5`, args...).Scan(&oid, &k)
		if err == pgx.ErrNoRows {
			return database.Fail(contracts.ScopeDenied, "relation is outside the accessible scope", false)
		}
		if err != nil {
			return err
		}
		info, err := inspect(ctx, tx, oid, a.Profile.Scope)
		if err != nil {
			return err
		}
		out.Kind = kind(k)
		out.Supported = info.reason == ""
		out.Columns = info.columns
		if err = constraints(ctx, tx, oid, a.Profile.Scope, &out); err != nil {
			return err
		}
		return payloadBound(out, a)
	})
	if err != nil {
		return database.Description{}, err
	}
	return out, nil
}
func constraints(ctx context.Context, tx pgx.Tx, oid uint32, scope config.Scope, out *database.Description) error {
	args := append(scopeArgs(scope), oid)
	rows, err := tx.Query(ctx, `SELECT k.contype::text,
 ARRAY(SELECT a.attname::text FROM pg_catalog.unnest(k.conkey) WITH ORDINALITY x(num,ord) JOIN pg_catalog.pg_attribute a ON a.attrelid=k.conrelid AND a.attnum=x.num ORDER BY x.ord),
 COALESCE(n.nspname::text,''),COALESCE(c.relname::text,''),
 ARRAY(SELECT a.attname::text FROM pg_catalog.unnest(k.confkey) WITH ORDINALITY x(num,ord) JOIN pg_catalog.pg_attribute a ON a.attrelid=k.confrelid AND a.attnum=x.num ORDER BY x.ord)
 FROM pg_catalog.pg_constraint k LEFT JOIN pg_catalog.pg_class c ON c.oid=k.confrelid LEFT JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
 WHERE k.conrelid=$4 AND k.contype IN ('p','u','f') AND (k.contype<>'f' OR (`+visibleSQL+`)) ORDER BY k.oid LIMIT 4097`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	count := 0
	budget := 0
	for rows.Next() {
		count++
		if count > maxCatalogObjects {
			return database.Fail(contracts.ResourceLimit, "constraint count exceeds limit", false)
		}
		var k, s, n string
		var cols, target []string
		if err = rows.Scan(&k, &cols, &s, &n, &target); err != nil {
			return err
		}
		encoded, _ := json.Marshal([]any{cols, s, n, target})
		budget += len(encoded) + 64
		if budget > (1<<20)/3 {
			return database.Fail(contracts.ResourceLimit, "constraint metadata exceeds payload budget", false)
		}
		if k == "f" {
			out.Relationships = append(out.Relationships, database.Relationship{Columns: cols, Target: config.Table{Schema: s, Name: n}, TargetColumns: target})
		} else {
			label := "unique"
			if k == "p" {
				label = "primary"
			}
			out.Keys = append(out.Keys, database.Key{Kind: label, Columns: cols})
		}
	}
	return rows.Err()
}
