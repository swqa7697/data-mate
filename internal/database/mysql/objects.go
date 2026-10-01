package mysql

import (
	"context"
	"slices"
	"strings"

	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
	"github.com/swqa7697/data-mate/internal/database/mysql/sqlguard"
	"github.com/swqa7697/data-mate/internal/database/pool"
)

// kinds lists catalog object kinds; only MariaDB has sequences.
func (d *Driver) kinds() []string {
	if d.flavor == MariaDB {
		return []string{"function", "procedure", "sequence"}
	}
	return []string{"function", "procedure"}
}

func (d *Driver) unsupportedKind() error {
	return database.Fail(contracts.InvalidArgument, d.flavor.display()+" object kinds are "+strings.Join(d.kinds(), ", "), false)
}

// objectsSQL selects (schema, name, kind) rows; names are unique per schema and
// kind, so pages need no tiebreak beyond the kind.
func (d *Driver) objectsSQL() string {
	sequences := ""
	if d.flavor == MariaDB {
		sequences = ` UNION ALL SELECT TABLE_SCHEMA,TABLE_NAME,'sequence' FROM information_schema.TABLES WHERE TABLE_TYPE='SEQUENCE'`
	}
	return `SELECT o.s,o.n,o.k FROM (SELECT ROUTINE_SCHEMA AS s,ROUTINE_NAME AS n,LOWER(ROUTINE_TYPE) AS k FROM information_schema.ROUTINES` + sequences + `) o`
}

// ListObjects lists functions, procedures and MariaDB sequences.
func (d *Driver) ListObjects(ctx context.Context, a database.Access, req database.ObjectPageRequest) (ObjectPage, error) {
	a, rev, err := database.Normalize(a)
	if err != nil {
		return ObjectPage{}, err
	}
	if err = d.checkProfile(a.Profile); err != nil {
		return ObjectPage{}, err
	}
	if req.PageSize == 0 {
		req.PageSize = 100
	}
	if req.Kind != "" && !slices.Contains(d.kinds(), req.Kind) {
		return ObjectPage{}, d.unsupportedKind()
	}
	if req.PageSize < 1 || req.PageSize > 500 || (req.Schema != "" && !validName(req.Schema)) {
		return ObjectPage{}, database.Fail(contracts.InvalidArgument, "invalid object page request", false)
	}
	pos := pool.Cursor{Version: 1, Tool: "list_objects", ID: a.Profile.ID, Revision: rev, Filter: req.Schema, KindFilter: req.Kind, Epoch: d.pools.Epoch()}
	if req.Cursor != "" {
		if pos, err = d.pools.Decode(req.Cursor, pos, validName); err != nil {
			return ObjectPage{}, err
		}
		if !slices.Contains(d.kinds(), pos.Kind) {
			return ObjectPage{}, database.Fail(contracts.StaleCursor, "metadata cursor is invalid or stale", false)
		}
	}
	out := ObjectPage{Connection: a.Profile.Alias, Driver: a.Profile.Driver, Objects: []Object{}}
	err = d.runNormalized(ctx, a, rev, func(ctx context.Context, s *session) error {
		r, err := s.query(ctx, d.objectsSQL()+`
 WHERE (?='' OR CAST(o.s AS BINARY)=CAST(? AS BINARY)) AND (?='' OR CAST(o.k AS BINARY)=CAST(? AS BINARY))
 AND (CAST(o.s AS BINARY),CAST(o.n AS BINARY),CAST(o.k AS BINARY))>(CAST(? AS BINARY),CAST(? AS BINARY),CAST(? AS BINARY))
 ORDER BY CAST(o.s AS BINARY),CAST(o.n AS BINARY),CAST(o.k AS BINARY) LIMIT ?`, req.Schema, req.Schema, req.Kind, req.Kind, pos.Schema, pos.Name, pos.Kind, int64(req.PageSize+1))
		if err != nil {
			return err
		}
		values, err := r.all(req.PageSize + 1)
		if err != nil {
			return err
		}
		budget := database.MetadataBudget{Limit: a.Profile.Limits.MaxResultBytes}
		for _, v := range values {
			if len(out.Objects) == req.PageSize {
				last := out.Objects[len(out.Objects)-1]
				pos.Schema, pos.Name, pos.Kind = last.Schema, last.Name, last.Kind
				next := d.pools.Encode(pos)
				out.NextCursor = &next
				break
			}
			item := Object{Schema: str(v[0]), Name: str(v[1]), Kind: str(v[2])}
			if err = budget.Add(item); err != nil {
				return err
			}
			out.Objects = append(out.Objects, item)
		}
		return database.PayloadBound(out, a)
	})
	if err != nil {
		return ObjectPage{}, err
	}
	return out, nil
}

// DescribeObject looks up the exact visible identity before reading its definition.
func (d *Driver) DescribeObject(ctx context.Context, a database.Access, req database.ObjectRequest) (ObjectDescription, error) {
	if !slices.Contains(d.kinds(), req.Kind) {
		return ObjectDescription{}, d.unsupportedKind()
	}
	if !validName(req.Schema) || !validName(req.Name) || req.IdentityArguments != nil {
		return ObjectDescription{}, database.Fail(contracts.InvalidArgument, "invalid object identity", false)
	}
	a, rev, err := database.Normalize(a)
	if err != nil {
		return ObjectDescription{}, err
	}
	if err = d.checkProfile(a.Profile); err != nil {
		return ObjectDescription{}, err
	}
	out := ObjectDescription{Connection: a.Profile.Alias, Driver: a.Profile.Driver, Object: Object{Kind: req.Kind, Schema: req.Schema, Name: req.Name}}
	err = d.runNormalized(ctx, a, rev, func(ctx context.Context, s *session) error {
		var err error
		if req.Kind == "sequence" {
			out.Sequence, err = d.describeSequence(ctx, s, req)
		} else {
			out.Routine, err = d.describeRoutine(ctx, s, req)
		}
		if err != nil {
			return err
		}
		return database.PayloadBound(out, a)
	})
	if err != nil {
		return ObjectDescription{}, err
	}
	return out, nil
}

func unavailableObject() error {
	return database.Fail(contracts.PermissionDenied, "object is unavailable or inaccessible", false)
}

func (d *Driver) describeRoutine(ctx context.Context, s *session, req database.ObjectRequest) (*Routine, error) {
	language := "COALESCE(EXTERNAL_LANGUAGE,ROUTINE_BODY)"
	if d.flavor == MariaDB {
		language = "ROUTINE_BODY"
	}
	kind := strings.ToUpper(req.Kind)
	values, err := d.catalogRows(ctx, s, `SELECT DTD_IDENTIFIER,`+language+`,IS_DETERMINISTIC,SQL_DATA_ACCESS,SECURITY_TYPE,ROUTINE_DEFINITION,SPECIFIC_NAME FROM information_schema.ROUTINES
 WHERE ROUTINE_SCHEMA=? AND ROUTINE_NAME=? AND CAST(ROUTINE_SCHEMA AS BINARY)=CAST(? AS BINARY) AND CAST(ROUTINE_NAME AS BINARY)=CAST(? AS BINARY) AND ROUTINE_TYPE=?`, req.Schema, req.Name, req.Schema, req.Name, kind)
	if err != nil {
		return nil, err
	}
	if len(values) != 1 {
		return nil, unavailableObject()
	}
	v := values[0]
	out := &Routine{
		Parameters:    []Parameter{},
		Language:      strings.ToLower(str(v[1])),
		Deterministic: str(v[2]) == "YES",
		SQLDataAccess: strings.ReplaceAll(strings.ToLower(str(v[3])), " ", "_"),
		Security:      strings.ToLower(str(v[4])),
		Definition:    optional(v[5]),
	}
	if kind == "FUNCTION" {
		out.ReturnType = optional(v[0])
	}
	specific := str(v[6])
	params, err := d.catalogRows(ctx, s, `SELECT PARAMETER_MODE,PARAMETER_NAME,DTD_IDENTIFIER FROM information_schema.PARAMETERS
 WHERE SPECIFIC_SCHEMA=? AND SPECIFIC_NAME=? AND CAST(SPECIFIC_SCHEMA AS BINARY)=CAST(? AS BINARY) AND CAST(SPECIFIC_NAME AS BINARY)=CAST(? AS BINARY)
 AND ROUTINE_TYPE=? AND ORDINAL_POSITION>0 ORDER BY ORDINAL_POSITION LIMIT 4097`, req.Schema, specific, req.Schema, specific, kind)
	if err != nil {
		return nil, err
	}
	for _, p := range params {
		param := Parameter{Name: str(p[1]), Type: str(p[2])}
		if kind == "PROCEDURE" {
			if mode, ok := text(p[0]); ok {
				mode = strings.ToLower(mode)
				param.Mode = &mode
			}
		}
		out.Parameters = append(out.Parameters, param)
	}
	return out, nil
}

// describeSequence reads a MariaDB sequence's configuration row, which needs
// SELECT on the sequence and never advances it.
func (d *Driver) describeSequence(ctx context.Context, s *session, req database.ObjectRequest) (*Sequence, error) {
	values, err := d.catalogRows(ctx, s, `SELECT TABLE_NAME FROM information_schema.TABLES WHERE TABLE_TYPE='SEQUENCE' AND `+relationSQL, req.Schema, req.Name, req.Schema, req.Name)
	if err != nil {
		return nil, err
	}
	if len(values) != 1 {
		return nil, unavailableObject()
	}
	// Names are validated identifiers, quoted; no other input enters the SQL.
	r, err := s.query(ctx, "SELECT start_value,minimum_value,maximum_value,increment,cache_size,cycle_option FROM "+sqlguard.Quote(req.Schema)+"."+sqlguard.Quote(req.Name))
	if err != nil {
		return nil, err
	}
	values, err = r.all(1)
	if err != nil {
		return nil, err
	}
	if len(values) != 1 {
		return nil, errMalformed
	}
	v := values[0]
	out := &Sequence{DataType: "bigint", Start: str(v[0]), Min: str(v[1]), Max: str(v[2]), Increment: str(v[3]), Cache: str(v[4]), Cycle: str(v[5]) == "1"}
	if s.info.version >= 110500 {
		// MariaDB 11.5 added typed sequences and information_schema.SEQUENCES.
		types, err := d.catalogRows(ctx, s, `SELECT DATA_TYPE FROM information_schema.SEQUENCES WHERE SEQUENCE_SCHEMA=? AND SEQUENCE_NAME=? AND CAST(SEQUENCE_SCHEMA AS BINARY)=CAST(? AS BINARY) AND CAST(SEQUENCE_NAME AS BINARY)=CAST(? AS BINARY)`, req.Schema, req.Name, req.Schema, req.Name)
		if err != nil {
			return nil, err
		}
		if len(types) == 1 {
			out.DataType = strings.ToLower(str(types[0][0]))
		}
	}
	return out, nil
}
