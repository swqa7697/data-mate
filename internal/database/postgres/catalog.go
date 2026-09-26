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
const accessibleSchemaSQL = `n.nspname NOT LIKE 'pg\_%' AND n.nspname<>'information_schema'
 AND pg_catalog.has_schema_privilege(n.oid,'USAGE')`

const readableRelationSQL = `c.relkind IN ('r','p','v','m','f') AND c.relpersistence<>'t'
 AND (pg_catalog.has_table_privilege(c.oid,'SELECT') OR pg_catalog.has_any_column_privilege(c.oid,'SELECT'))`

const visibleSQL = accessibleSchemaSQL + ` AND ` + readableRelationSQL + `
 AND (($1::text='blacklist' AND NOT (n.nspname::text=ANY($2::text[]))) OR ($1::text='whitelist' AND n.nspname::text=ANY($2::text[])))`

func scopeArgs(s config.Scope) []any {
	schemas := s.Schemas
	if schemas == nil {
		schemas = []string{}
	}
	return []any{s.Mode, schemas}
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
		rows, err := tx.Query(ctx, `SELECT n.nspname,c.relname,c.relkind::text FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE `+visibleSQL+`
 AND ($3::text='' OR n.nspname=$3) AND (n.nspname::text COLLATE "C",c.relname::text COLLATE "C") > ($4::text COLLATE "C",$5::text COLLATE "C")
 ORDER BY n.nspname::text COLLATE "C",c.relname::text COLLATE "C" LIMIT $6`, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var table database.Table
			var k string
			if err := rows.Scan(&table.Schema, &table.Name, &k); err != nil {
				rows.Close()
				return err
			}
			table.Kind = kind(k)
			out.Tables = append(out.Tables, table)
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return err
		}
		more := len(out.Tables) > req.PageSize
		if more {
			out.Tables = out.Tables[:req.PageSize]
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

func columns(ctx context.Context, tx pgx.Tx, oid uint32) ([]database.Column, error) {
	rows, err := tx.Query(ctx, `SELECT a.attname,pg_catalog.format_type(a.atttypid,a.atttypmod),NOT a.attnotnull
 FROM pg_catalog.pg_attribute a WHERE a.attrelid=$1 AND a.attnum>0 AND NOT a.attisdropped ORDER BY a.attnum LIMIT 1601`, oid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []database.Column{}
	for rows.Next() {
		var c database.Column
		if err := rows.Scan(&c.Name, &c.Type, &c.Nullable); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if len(out) > 1600 {
		return nil, database.Fail(contracts.ResourceLimit, "column limit exceeded", false)
	}
	return out, rows.Err()
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
		err := tx.QueryRow(ctx, `SELECT c.oid,c.relkind::text FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE `+visibleSQL+` AND n.nspname=$3 AND c.relname=$4`, args...).Scan(&oid, &k)
		if err == pgx.ErrNoRows {
			return database.Fail(contracts.ScopeDenied, "relation is outside the accessible scope", false)
		}
		if err != nil {
			return err
		}
		cols, err := columns(ctx, tx, oid)
		if err != nil {
			return err
		}
		out.Kind = kind(k)
		out.Columns = cols
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
 WHERE k.conrelid=$3 AND k.contype IN ('p','u','f') AND (k.contype<>'f' OR (`+visibleSQL+`)) ORDER BY k.oid LIMIT 4097`, args...)
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
