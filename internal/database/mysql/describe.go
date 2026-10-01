package mysql

import (
	"context"
	"encoding/json"

	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
)

// describeSQL lists every visible application database once, preserving empty
// ones with a left join, and orders names byte-wise. The extra row detects
// overflow without materializing an unbounded catalog.
const describeSQL = `SELECT s.SCHEMA_NAME,o.name,o.kind,o.tbl FROM information_schema.SCHEMATA s LEFT JOIN (
 SELECT TABLE_SCHEMA AS sch,TABLE_NAME AS name,TABLE_TYPE AS kind,NULL AS tbl FROM information_schema.TABLES
 WHERE TABLE_TYPE IN ('BASE TABLE','VIEW','SYSTEM VERSIONED','SEQUENCE')
 UNION ALL SELECT DISTINCT TABLE_SCHEMA,INDEX_NAME,'INDEX',TABLE_NAME FROM information_schema.STATISTICS
 UNION ALL SELECT ROUTINE_SCHEMA,ROUTINE_NAME,'FUNCTION',NULL FROM information_schema.ROUTINES WHERE ROUTINE_TYPE='FUNCTION'
 ) o ON CAST(o.sch AS BINARY)=CAST(s.SCHEMA_NAME AS BINARY)
 WHERE s.SCHEMA_NAME NOT IN ('information_schema','mysql','performance_schema','sys')
 ORDER BY CAST(s.SCHEMA_NAME AS BINARY),CAST(o.name AS BINARY),CAST(o.kind AS BINARY),CAST(o.tbl AS BINARY) LIMIT 4097`

// DescribeDatabase lists the application databases visible to the account.
// MySQL-family profiles name no database, so each schema is a database.
func (d *Driver) DescribeDatabase(ctx context.Context, a database.Access) (database.DatabaseDescription, error) {
	a, rev, err := database.Normalize(a)
	if err != nil {
		return database.DatabaseDescription{}, err
	}
	if err = d.checkProfile(a.Profile); err != nil {
		return database.DatabaseDescription{}, err
	}
	out := database.DatabaseDescription{Version: 1, Alias: a.Profile.Alias, Driver: a.Profile.Driver, Schemas: []database.SchemaDescription{}}
	err = d.runNormalized(ctx, a, rev, func(ctx context.Context, s *session) error {
		r, err := s.query(ctx, describeSQL)
		if err != nil {
			return err
		}
		values, err := r.all(database.MaxCatalogObjects + 1)
		if err != nil {
			return err
		}
		count := 0
		for _, v := range values {
			schema := str(v[0])
			if len(out.Schemas) == 0 || out.Schemas[len(out.Schemas)-1].Name != schema {
				count++
				out.Schemas = append(out.Schemas, database.SchemaDescription{Name: schema, Tables: []database.RelationName{}, Enums: []database.CatalogName{}, Sequences: []database.CatalogName{}, Indexes: []database.IndexName{}, Functions: []database.CatalogName{}})
			}
			if name, ok := text(v[1]); ok {
				count++
				i := len(out.Schemas) - 1
				switch str(v[2]) {
				case "SEQUENCE":
					out.Schemas[i].Sequences = append(out.Schemas[i].Sequences, database.CatalogName{Name: name})
				case "INDEX":
					out.Schemas[i].Indexes = append(out.Schemas[i].Indexes, database.IndexName{Name: name, Table: str(v[3])})
				case "FUNCTION":
					out.Schemas[i].Functions = append(out.Schemas[i].Functions, database.CatalogName{Name: name})
				case "VIEW":
					out.Schemas[i].Tables = append(out.Schemas[i].Tables, database.RelationName{Name: name, Kind: "view"})
				case "SYSTEM VERSIONED":
					out.Schemas[i].Tables = append(out.Schemas[i].Tables, database.RelationName{Name: name, Kind: "system_versioned_table"})
				default:
					out.Schemas[i].Tables = append(out.Schemas[i].Tables, database.RelationName{Name: name, Kind: "table"})
				}
			}
			if count > database.MaxCatalogObjects {
				return database.Fail(contracts.ResourceLimit, "database description exceeds 4096 schemas and objects", false)
			}
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
