package postgres

import (
	"context"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
	"github.com/swqa7697/data-mate/internal/database/pool"
)

// Catalog reads use PostgreSQL catalog permissions, independent of row grants.
const relationKindSQL = `c.relkind IN ('r','p','v','m','f')`

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

// ListTables returns a keyset page.
func (d *Driver) ListTables(ctx context.Context, a database.Access, req database.PageRequest) (database.TablePage, error) {
	a, rev, err := database.Normalize(a)
	if err != nil {
		return database.TablePage{}, err
	}
	if req.PageSize == 0 {
		req.PageSize = 100
	}
	if req.PageSize < 1 || req.PageSize > 500 || (req.Schema != "" && !validName(req.Schema)) {
		return database.TablePage{}, database.Fail(contracts.InvalidArgument, "invalid metadata page request", false)
	}
	pos := pool.Cursor{Version: 1, Tool: "list_tables", ID: a.Profile.ID, Revision: rev, Filter: req.Schema, Epoch: d.pools.Epoch()}
	if req.Cursor != "" {
		pos, err = d.pools.Decode(req.Cursor, pos, validName)
		if err != nil {
			return database.TablePage{}, err
		}
	}
	out := database.TablePage{Connection: a.Profile.Alias, Driver: a.Profile.Driver, Tables: []database.Table{}}
	err = d.run(ctx, a, func(ctx context.Context, tx pgx.Tx, _ int) error {
		args := []any{req.Schema, pos.Schema, pos.Name, req.PageSize + 1}
		rows, err := tx.Query(ctx, `SELECT n.nspname,c.relname,c.relkind::text FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE `+relationKindSQL+`
 AND ($1::text='' OR n.nspname=$1) AND (n.nspname::text COLLATE "C",c.relname::text COLLATE "C") > ($2::text COLLATE "C",$3::text COLLATE "C")
 ORDER BY n.nspname::text COLLATE "C",c.relname::text COLLATE "C" LIMIT $4`, args...)
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
			s := d.pools.Encode(pos)
			out.NextCursor = &s
		}
		return database.PayloadBound(out, a)
	})
	if err != nil {
		return database.TablePage{}, err
	}
	return out, nil
}

func columns(ctx context.Context, tx pgx.Tx, oid uint32, budget *database.MetadataBudget) ([]Column, error) {
	rows, err := tx.Query(ctx, `SELECT a.attname,pg_catalog.format_type(a.atttypid,a.atttypmod),NOT a.attnotnull,
 CASE WHEN a.attgenerated='' THEN pg_catalog.pg_get_expr(d.adbin,d.adrelid) END,
 CASE WHEN a.attgenerated<>'' THEN pg_catalog.pg_get_expr(d.adbin,d.adrelid) END,a.attidentity::text
 FROM pg_catalog.pg_attribute a LEFT JOIN pg_catalog.pg_attrdef d ON d.adrelid=a.attrelid AND d.adnum=a.attnum WHERE a.attrelid=$1 AND a.attnum>0 AND NOT a.attisdropped ORDER BY a.attnum LIMIT 1601`, oid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Column{}
	for rows.Next() {
		var c Column
		if err := rows.Scan(&c.Name, &c.Type, &c.Nullable, &c.Default, &c.Generated, &c.Identity); err != nil {
			return nil, err
		}
		if err := budget.Add(c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if len(out) > 1600 {
		return nil, database.Fail(contracts.ResourceLimit, "column limit exceeded", false)
	}
	return out, rows.Err()
}

// DescribeTable reads stored definitions including foreign-key endpoints.
func (d *Driver) DescribeTable(ctx context.Context, a database.Access, name config.Table) (Description, error) {
	if !validName(name.Schema) || !validName(name.Name) {
		return Description{}, database.Fail(contracts.InvalidArgument, "invalid relation name", false)
	}
	a, rev, err := database.Normalize(a)
	if err != nil {
		return Description{}, err
	}
	out := Description{RelationCore: database.NewRelationCore(a, name.Schema, name.Name), Columns: []Column{}, Triggers: []Trigger{}, Policies: []Policy{}}
	err = d.runNormalized(ctx, a, rev, func(ctx context.Context, tx pgx.Tx, _ int) error {
		args := []any{name.Schema, name.Name}
		var oid uint32
		var k string
		err := tx.QueryRow(ctx, `SELECT c.oid,c.relkind::text,c.relrowsecurity,c.relforcerowsecurity,CASE WHEN c.relkind IN ('v','m') THEN pg_catalog.pg_get_viewdef(c.oid) END FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE `+relationKindSQL+` AND n.nspname=$1 AND c.relname=$2`, args...).Scan(&oid, &k, &out.RowSecurity, &out.ForceRowSecurity, &out.ViewDefinition)
		if err == pgx.ErrNoRows {
			return database.Fail(contracts.PermissionDenied, "relation is unavailable or inaccessible", false)
		}
		if err != nil {
			return err
		}
		budget := database.MetadataBudget{Limit: a.Profile.Limits.MaxResultBytes}
		cols, err := columns(ctx, tx, oid, &budget)
		if err != nil {
			return err
		}
		out.Kind = kind(k)
		out.Columns = cols
		if err = constraints(ctx, tx, oid, &out); err != nil {
			return err
		}
		if err = tableDefinitions(ctx, tx, oid, &out, &budget); err != nil {
			return err
		}
		return database.PayloadBound(out, a)
	})
	if err != nil {
		return Description{}, err
	}
	return out, nil
}
func constraints(ctx context.Context, tx pgx.Tx, oid uint32, out *Description) error {
	args := []any{oid}
	rows, err := tx.Query(ctx, `SELECT k.contype::text,
 ARRAY(SELECT a.attname::text FROM pg_catalog.unnest(k.conkey) WITH ORDINALITY x(num,ord) JOIN pg_catalog.pg_attribute a ON a.attrelid=k.conrelid AND a.attnum=x.num ORDER BY x.ord),
 COALESCE(n.nspname::text,''),COALESCE(c.relname::text,''),
 ARRAY(SELECT a.attname::text FROM pg_catalog.unnest(k.confkey) WITH ORDINALITY x(num,ord) JOIN pg_catalog.pg_attribute a ON a.attrelid=k.confrelid AND a.attnum=x.num ORDER BY x.ord)
 FROM pg_catalog.pg_constraint k LEFT JOIN pg_catalog.pg_class c ON c.oid=k.confrelid LEFT JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
 WHERE k.conrelid=$1 AND k.contype IN ('p','u','f') ORDER BY k.oid LIMIT 4097`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	count := 0
	budget := 0
	for rows.Next() {
		count++
		if count > database.MaxCatalogObjects {
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
