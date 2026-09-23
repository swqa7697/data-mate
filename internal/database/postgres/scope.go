package postgres

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
)

// BrowseScope is a user configuration operation, never an agent metadata tool.
// The shared role/visibility/support checks still apply; only saved scope is
// replaced with all. Search is literal, bounded, and evaluated by the database.
func (d *Driver) BrowseScope(ctx context.Context, a database.Access, req database.ScopeRequest) (database.ScopePage, error) {
	if (req.Schema != "" && !validName(req.Schema)) || (req.After != "" && !validName(req.After)) || len(req.Search) > 256 || !utf8.ValidString(req.Search) || strings.ContainsRune(req.Search, 0) {
		return database.ScopePage{}, database.Fail(contracts.InvalidArgument, "invalid catalog selection request", false)
	}
	a.Profile.Scope = config.Scope{Mode: "all"}
	out := database.ScopePage{Schemas: []string{}, Tables: []database.Table{}}
	err := d.run(ctx, a, func(ctx context.Context, tx pgx.Tx, _ int) error {
		args := append(scopeArgs(a.Profile.Scope), req.Schema, req.After, req.Search)
		if req.Schema == "" {
			rows, err := tx.Query(ctx, `SELECT n.nspname::text FROM pg_catalog.pg_namespace n
 WHERE n.nspname NOT LIKE 'pg\_%' AND n.nspname<>'information_schema' AND pg_catalog.has_schema_privilege(n.oid,'USAGE')
 AND n.nspname::text COLLATE "C">$1::text COLLATE "C"
 AND pg_catalog.strpos(pg_catalog.lower(n.nspname::text),pg_catalog.lower($2::text))>0
 ORDER BY n.nspname::text COLLATE "C" LIMIT 51`, req.After, req.Search)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var name string
				if err = rows.Scan(&name); err != nil {
					return err
				}
				out.Schemas = append(out.Schemas, name)
			}
			if err = rows.Err(); err != nil {
				return err
			}
			if len(out.Schemas) > 50 {
				out.Schemas = out.Schemas[:50]
				out.Next = out.Schemas[49]
			}
		} else {
			rows, err := tx.Query(ctx, `SELECT c.oid,n.nspname,c.relname,c.relkind::text FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE `+visibleSQL+`
    AND n.nspname=$4 AND c.relname::text COLLATE "C">$5::text COLLATE "C" AND pg_catalog.strpos(pg_catalog.lower(c.relname::text),pg_catalog.lower($6::text))>0 ORDER BY c.relname::text COLLATE "C" LIMIT 51`, args...)
			if err != nil {
				return err
			}
			var ids []uint32
			for rows.Next() {
				var oid uint32
				var t database.Table
				var k string
				if err = rows.Scan(&oid, &t.Schema, &t.Name, &k); err != nil {
					rows.Close()
					return err
				}
				t.Kind = kind(k)
				out.Tables = append(out.Tables, t)
				ids = append(ids, oid)
			}
			rows.Close()
			if err = rows.Err(); err != nil {
				return err
			}
			if len(out.Tables) > 50 {
				out.Tables = out.Tables[:50]
				ids = ids[:50]
				out.Next = out.Tables[49].Name
			}
			for i, id := range ids {
				info, err := inspect(ctx, tx, id, a.Profile.Scope)
				if err != nil {
					return err
				}
				out.Tables[i].Supported = info.reason == ""
				out.Tables[i].Reason = info.reason
			}
		}
		return payloadBound(out, a)
	})
	if err != nil {
		return database.ScopePage{}, err
	}
	return out, nil
}
