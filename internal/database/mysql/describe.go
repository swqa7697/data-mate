package mysql

import (
	"context"
	"database/sql/driver"
	"strings"

	"github.com/swqa7697/data-mate/internal/database"
)

// describeSQL lists every visible application database once, preserving empty
// ones with a left join, and orders names byte-wise.
const describeSQL = `SELECT s.SCHEMA_NAME,o.name,o.kind,o.tbl FROM information_schema.SCHEMATA s LEFT JOIN (
 SELECT TABLE_SCHEMA AS sch,TABLE_NAME AS name,TABLE_TYPE AS kind,NULL AS tbl FROM information_schema.TABLES
 WHERE TABLE_TYPE IN ('BASE TABLE','VIEW','SYSTEM VERSIONED','SEQUENCE')
 UNION ALL SELECT DISTINCT CAST(TABLE_SCHEMA AS BINARY),CAST(INDEX_NAME AS BINARY),'INDEX',CAST(TABLE_NAME AS BINARY) FROM information_schema.STATISTICS
 UNION ALL SELECT ROUTINE_SCHEMA,ROUTINE_NAME,'FUNCTION',NULL FROM information_schema.ROUTINES WHERE ROUTINE_TYPE='FUNCTION'
 ) o ON CAST(o.sch AS BINARY)=CAST(s.SCHEMA_NAME AS BINARY)
 WHERE s.SCHEMA_NAME NOT IN ('information_schema','mysql','performance_schema','sys')
 ORDER BY CAST(s.SCHEMA_NAME AS BINARY),CAST(o.name AS BINARY),CAST(o.kind AS BINARY),CAST(o.tbl AS BINARY)`

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
		budget := database.MetadataBudget{Limit: a.Profile.Limits.MaxResultBytes}
		err = r.each(func(v []driver.Value) error {
			kind := str(v[2])
			switch kind {
			case "SEQUENCE", "INDEX", "FUNCTION", "VIEW":
				kind = strings.ToLower(kind)
			case "SYSTEM VERSIONED":
				kind = "system_versioned_table"
			case "BASE TABLE":
				kind = "table"
			}
			return out.AddCatalogRow(&budget, str(v[0]), kind, str(v[1]), str(v[3]))
		})
		if err != nil {
			return err
		}
		return out.CheckSize(budget.Limit)
	})
	if err != nil {
		return database.DatabaseDescription{}, err
	}
	return out, nil
}
