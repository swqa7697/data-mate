package postgres

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
	"github.com/swqa7697/data-mate/internal/contracts"
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
		// At least one row per schema; the extra row detects overflow without
		// materializing an unbounded catalog. A left join preserves empty schemas.
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
 ) SELECT n.nspname::text,o.name,o.kind
 FROM schemas n LEFT JOIN objects o ON o.namespace=n.oid
 ORDER BY n.nspname::text COLLATE "C",o.name COLLATE "C",o.kind COLLATE "C" LIMIT 4097`)
		if err != nil {
			return err
		}
		defer rows.Close()
		count := 0
		for rows.Next() {
			var schema string
			var name, relationKind *string
			if err := rows.Scan(&schema, &name, &relationKind); err != nil {
				return err
			}
			if len(out.Schemas) == 0 || out.Schemas[len(out.Schemas)-1].Name != schema {
				count++
				out.Schemas = append(out.Schemas, database.SchemaDescription{Name: schema, Tables: []database.RelationName{}, Enums: []database.CatalogName{}, Sequences: []database.CatalogName{}, Indexes: []database.IndexName{}, Functions: []database.CatalogName{}})
			}
			if name != nil {
				count++
				i := len(out.Schemas) - 1
				switch *relationKind {
				case "enum":
					out.Schemas[i].Enums = append(out.Schemas[i].Enums, database.CatalogName{Name: *name})
				case "S":
					out.Schemas[i].Sequences = append(out.Schemas[i].Sequences, database.CatalogName{Name: *name})
				case "i", "I":
					out.Schemas[i].Indexes = append(out.Schemas[i].Indexes, database.IndexName{Name: *name})
				case "function":
					out.Schemas[i].Functions = append(out.Schemas[i].Functions, database.CatalogName{Name: *name})
				default:
					out.Schemas[i].Tables = append(out.Schemas[i].Tables, database.RelationName{Name: *name, Kind: kind(*relationKind)})
				}
			}
			if count > database.MaxCatalogObjects {
				return database.Fail(contracts.ResourceLimit, "database description exceeds 4096 schemas and objects", false)
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		// This CLI result has no duplicated MCP compatibility representation.
		encoded, err := json.Marshal(out)
		if err != nil {
			return err
		}
		if len(encoded) > a.Profile.Limits.MaxResultBytes {
			return database.Fail(contracts.ResourceLimit, "database description exceeds profile result-byte limit", false)
		}
		return nil
	})
	if err != nil {
		return database.DatabaseDescription{}, err
	}
	return out, nil
}
