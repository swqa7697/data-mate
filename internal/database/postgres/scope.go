package postgres

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
)

// BrowseScope is a user configuration operation, never an agent metadata tool.
// It lists accessible application schemas independently of saved scope.
// Search is literal, bounded, and evaluated by the database.
func (d *Driver) BrowseScope(ctx context.Context, a database.Access, req database.ScopeRequest) (database.ScopePage, error) {
	if (req.After != "" && !validName(req.After)) || len(req.Search) > 256 || !utf8.ValidString(req.Search) || strings.ContainsRune(req.Search, 0) {
		return database.ScopePage{}, database.Fail(contracts.InvalidArgument, "invalid catalog selection request", false)
	}
	out := database.ScopePage{Schemas: []string{}}
	err := d.run(ctx, a, func(ctx context.Context, tx pgx.Tx, _ int) error {
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
		return payloadBound(out, a)
	})
	if err != nil {
		return database.ScopePage{}, err
	}
	return out, nil
}
