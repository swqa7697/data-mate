package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/swqa7697/data-mate/internal/database"
)

// DescribeDatabase lists the role-accessible catalog for the user,
// with one statement that preserves empty schemas and a consistent catalog.
func (d *Driver) DescribeDatabase(ctx context.Context, a database.Access) (database.DatabaseDescription, error) {
	a, rev, err := database.Normalize(a)
	if err != nil {
		return database.DatabaseDescription{}, err
	}
	out := database.DatabaseDescription{Version: 1, Alias: a.Profile.Alias, Driver: a.Profile.Driver, Database: a.Profile.Connection.Database, Schemas: []database.SchemaDescription{}}
	err = d.runNormalized(ctx, a, rev, func(ctx context.Context, tx pgx.Tx, _ int) error {
		// A left join preserves empty schemas in the same catalog snapshot.
		rows, err := tx.Query(ctx, `WITH schemas AS MATERIALIZED (
 SELECT oid,nspname FROM pg_catalog.pg_namespace
 WHERE nspname <> 'information_schema' AND left(nspname::text,3) <> 'pg_'
 ), objects AS (
 SELECT c.relnamespace AS namespace,c.relname::text AS name,c.relkind::text AS kind
 FROM pg_catalog.pg_class c JOIN schemas n ON n.oid=c.relnamespace
 WHERE (`+relationKindSQL+`) OR c.relkind IN ('S','i','I')
 UNION ALL
 SELECT t.typnamespace,t.typname::text,'enum'::text
 FROM pg_catalog.pg_type t JOIN schemas n ON n.oid=t.typnamespace WHERE t.typtype='e'
 UNION ALL
 SELECT DISTINCT p.pronamespace,p.proname::text COLLATE "C",'function'::text
 FROM pg_catalog.pg_proc p JOIN schemas n ON n.oid=p.pronamespace WHERE p.prokind IN ('f','w')
 ) SELECT n.nspname::text,COALESCE(o.name,''),COALESCE(o.kind,'')
 FROM schemas n LEFT JOIN objects o ON o.namespace=n.oid
 ORDER BY n.nspname::text COLLATE "C",o.name COLLATE "C",o.kind COLLATE "C"`)
		if err != nil {
			return err
		}
		defer rows.Close()
		budget := database.MetadataBudget{Limit: a.Profile.Limits.MaxResultBytes}
		for rows.Next() {
			var schema, name, relationKind string
			if err := rows.Scan(&schema, &name, &relationKind); err != nil {
				return err
			}
			if err := out.AddCatalogRow(&budget, schema, kind(relationKind), name, ""); err != nil {
				return abortCatalogRead(rows, err)
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		return out.CheckSize(budget.Limit)
	})
	if err != nil {
		return database.DatabaseDescription{}, err
	}
	return out, nil
}
