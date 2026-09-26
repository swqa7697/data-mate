package postgres

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
)

// DescribeDatabase lists the role-accessible catalog for the user, independently
// of saved scope. One statement preserves empty schemas and a consistent catalog.
func (d *Driver) DescribeDatabase(ctx context.Context, a database.Access) (database.DatabaseDescription, error) {
	a, rev, err := normalized(a)
	if err != nil {
		return database.DatabaseDescription{}, err
	}
	out := database.DatabaseDescription{Version: 1, Alias: a.Profile.Alias, Database: a.Profile.Connection.Database, Scope: a.Profile.Scope, Schemas: []database.SchemaDescription{}}
	err = d.runNormalized(ctx, a, rev, nil, func(ctx context.Context, tx pgx.Tx, _ int) error {
		// At least one row per schema; the extra row detects overflow without
		// materializing an unbounded catalog. Relations are filtered in the join
		// so schemas containing only unreadable relations still appear empty.
		rows, err := tx.Query(ctx, `SELECT n.nspname::text, c.relname::text, c.relkind::text
 FROM pg_catalog.pg_namespace n LEFT JOIN pg_catalog.pg_class c
 ON c.relnamespace=n.oid AND `+readableRelationSQL+`
 WHERE `+accessibleSchemaSQL+`
 ORDER BY n.nspname::text COLLATE "C", c.relname::text COLLATE "C" LIMIT 4097`)
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
				out.Schemas = append(out.Schemas, database.SchemaDescription{Name: schema, Allowed: out.Scope.ContainsSchema(schema), Tables: []database.RelationName{}})
			}
			if name != nil {
				count++
				i := len(out.Schemas) - 1
				out.Schemas[i].Tables = append(out.Schemas[i].Tables, database.RelationName{Name: *name, Kind: kind(*relationKind)})
			}
			if count > maxCatalogObjects {
				return database.Fail(contracts.ResourceLimit, "database description exceeds 4096 schemas and relations", false)
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
