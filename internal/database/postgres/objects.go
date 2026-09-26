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
)

// All names and signatures are values. No agent input becomes catalog SQL.
const objectsSQL = `WITH objects AS (
 SELECT p.oid,p.pronamespace AS namespace,p.proname AS name,'routine'::text AS kind,
 'pg_catalog.pg_proc'::regclass::oid AS classid FROM pg_catalog.pg_proc p
 UNION ALL
 SELECT t.oid,t.typnamespace,t.typname,'type','pg_catalog.pg_type'::regclass::oid
 FROM pg_catalog.pg_type t LEFT JOIN pg_catalog.pg_class c ON c.oid=t.typrelid
 WHERE t.typisdefined AND t.typtype<>'p' AND (t.typrelid=0 OR c.relkind='c')
 AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_type element WHERE element.typarray=t.oid)
 AND pg_catalog.has_type_privilege(t.oid,'USAGE')
 UNION ALL
 SELECT c.oid,c.relnamespace,c.relname,'sequence','pg_catalog.pg_class'::regclass::oid
 FROM pg_catalog.pg_class c WHERE c.relkind='S' AND c.relpersistence<>'t'
 AND (pg_catalog.has_sequence_privilege(c.oid,'SELECT') OR pg_catalog.has_sequence_privilege(c.oid,'USAGE'))
 ) SELECT o.oid,n.nspname::text,o.name::text,o.kind,
 CASE WHEN o.kind='routine' THEN pg_catalog.pg_get_function_identity_arguments(o.oid) END,
 (SELECT e.extname::text FROM pg_catalog.pg_depend d JOIN pg_catalog.pg_extension e ON e.oid=d.refobjid
 WHERE d.classid=o.classid AND d.objid=o.oid AND d.objsubid=0 AND d.refclassid='pg_catalog.pg_extension'::regclass AND d.deptype='e')
 FROM objects o JOIN pg_catalog.pg_namespace n ON n.oid=o.namespace WHERE ` + scopedSchemaSQL

func objectKind(k string) bool { return k == "routine" || k == "type" || k == "sequence" }

// ListObjects lists routines, explicit types and sequences under the saved scope.
func (d *Driver) ListObjects(ctx context.Context, a database.Access, req database.ObjectPageRequest) (database.ObjectPage, error) {
	a, rev, err := normalized(a)
	if err != nil {
		return database.ObjectPage{}, err
	}
	if req.PageSize == 0 {
		req.PageSize = 100
	}
	if req.PageSize < 1 || req.PageSize > 500 || (req.Schema != "" && !validName(req.Schema)) || (req.Kind != "" && !objectKind(req.Kind)) {
		return database.ObjectPage{}, database.Fail(contracts.InvalidArgument, "invalid object page request", false)
	}
	d.mu.Lock()
	epoch := d.epoch
	d.mu.Unlock()
	pos := cursor{Version: 1, Tool: "list_objects", ID: a.Profile.ID, Revision: rev, Filter: req.Schema, KindFilter: req.Kind, Epoch: epoch}
	if req.Cursor != "" {
		pos, err = d.decodeCursor(req.Cursor, pos)
		if err != nil {
			return database.ObjectPage{}, err
		}
		if !objectKind(pos.Kind) || pos.OID == 0 {
			return database.ObjectPage{}, database.Fail(contracts.StaleCursor, "metadata cursor is invalid or stale", false)
		}
	}
	out := database.ObjectPage{Connection: a.Profile.Alias, Objects: []database.Object{}}
	err = d.runNormalized(ctx, a, rev, nil, func(ctx context.Context, tx pgx.Tx, _ int) error {
		args := append(scopeArgs(a.Profile.Scope), req.Schema, req.Kind, pos.Schema, pos.Name, pos.Kind, pos.OID, req.PageSize+1)
		rows, e := tx.Query(ctx, objectsSQL+`
 AND ($3::text='' OR n.nspname=$3) AND ($4::text='' OR o.kind=$4)
 AND (n.nspname::text COLLATE "C",o.name::text COLLATE "C",o.kind COLLATE "C",o.oid) > ($5::text COLLATE "C",$6::text COLLATE "C",$7::text COLLATE "C",$8::oid)
 ORDER BY n.nspname::text COLLATE "C",o.name::text COLLATE "C",o.kind COLLATE "C",o.oid LIMIT $9`, args...)
		if e != nil {
			return e
		}
		defer rows.Close()
		budget := metadataBudget{limit: a.Profile.Limits.MaxResultBytes}
		more := false
		for rows.Next() {
			var item database.Object
			var oid uint32
			if e = rows.Scan(&oid, &item.Schema, &item.Name, &item.Kind, &item.IdentityArguments, &item.Extension); e != nil {
				return e
			}
			if len(out.Objects) == req.PageSize {
				more = true
				break
			}
			if e = budget.add(item); e != nil {
				return e
			}
			out.Objects = append(out.Objects, item)
			pos.Schema = item.Schema
			pos.Name = item.Name
			pos.Kind = item.Kind
			pos.OID = oid
		}
		rows.Close()
		if e = rows.Err(); e != nil {
			return e
		}
		if more {
			s := d.encodeCursor(pos)
			out.NextCursor = &s
		}
		return payloadBound(out, a)
	})
	if err != nil {
		return database.ObjectPage{}, err
	}
	return out, nil
}

// DescribeObject looks up the exact visible identity before reading its definition.
func (d *Driver) DescribeObject(ctx context.Context, a database.Access, req database.ObjectRequest) (database.ObjectDescription, error) {
	if !objectKind(req.Kind) || !validName(req.Schema) || !validName(req.Name) || (req.Kind == "routine") != (req.IdentityArguments != nil) {
		return database.ObjectDescription{}, database.Fail(contracts.InvalidArgument, "invalid object identity", false)
	}
	signature := ""
	if req.IdentityArguments != nil {
		signature = *req.IdentityArguments
	}
	if len(signature) > 64<<10 || !utf8.ValidString(signature) || strings.ContainsRune(signature, 0) {
		return database.ObjectDescription{}, database.Fail(contracts.InvalidArgument, "invalid routine identity", false)
	}
	a, rev, err := normalized(a)
	if err != nil {
		return database.ObjectDescription{}, err
	}
	out := database.ObjectDescription{Connection: a.Profile.Alias}
	err = d.runNormalized(ctx, a, rev, nil, func(ctx context.Context, tx pgx.Tx, _ int) error {
		args := append(scopeArgs(a.Profile.Scope), req.Schema, req.Name, req.Kind, signature)
		var oid uint32
		err := tx.QueryRow(ctx, objectsSQL+` AND n.nspname=$3 AND o.name=$4 AND o.kind=$5
 AND CASE WHEN o.kind='routine' THEN pg_catalog.pg_get_function_identity_arguments(o.oid)=$6 ELSE true END`, args...).Scan(&oid, &out.Schema, &out.Name, &out.Kind, &out.IdentityArguments, &out.Extension)
		if err == pgx.ErrNoRows {
			return database.Fail(contracts.ScopeDenied, "object is outside the accessible scope", false)
		}
		if err != nil {
			return err
		}
		budget := metadataBudget{limit: a.Profile.Limits.MaxResultBytes}
		switch req.Kind {
		case "routine":
			out.Routine, err = describeRoutine(ctx, tx, oid)
		case "type":
			out.Type, err = describeType(ctx, tx, oid, &budget)
		case "sequence":
			out.Sequence, err = describeSequence(ctx, tx, oid, a)
		}
		if err != nil {
			return err
		}
		return payloadBound(out, a)
	})
	if err != nil {
		return database.ObjectDescription{}, err
	}
	return out, nil
}

// Bound cumulative collections as they are read, before constructing an envelope.
type metadataBudget struct{ count, bytes, limit int }

func (b *metadataBudget) add(v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b.count++
	b.bytes += len(raw)
	if b.count > maxCatalogObjects || b.bytes > b.limit {
		return database.Fail(contracts.ResourceLimit, "metadata exceeds catalog or payload limit", false)
	}
	return nil
}

func describeRoutine(ctx context.Context, tx pgx.Tx, oid uint32) (*database.RoutineDescription, error) {
	out := &database.RoutineDescription{}
	err := tx.QueryRow(ctx, `SELECT CASE p.prokind WHEN 'f' THEN 'function' WHEN 'p' THEN 'procedure' WHEN 'a' THEN 'aggregate' ELSE 'window' END,
 pg_catalog.pg_get_function_arguments(p.oid),pg_catalog.pg_get_function_result(p.oid),l.lanname::text,
 CASE p.provolatile WHEN 'i' THEN 'immutable' WHEN 's' THEN 'stable' ELSE 'volatile' END,p.prosecdef,
 CASE WHEN p.prokind<>'a' THEN pg_catalog.pg_get_functiondef(p.oid) END
 FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_language l ON l.oid=p.prolang WHERE p.oid=$1`, oid).Scan(&out.Kind, &out.Arguments, &out.ReturnType, &out.Language, &out.Volatility, &out.SecurityDefiner, &out.Definition)
	if err != nil {
		return nil, err
	}
	if out.Kind == "aggregate" {
		v := &database.AggregateDescription{}
		err = tx.QueryRow(ctx, `SELECT CASE aggkind WHEN 'n' THEN 'normal' WHEN 'o' THEN 'ordered_set' ELSE 'hypothetical_set' END,
 aggtransfn::regprocedure::text,pg_catalog.format_type(aggtranstype,NULL),
 CASE WHEN aggfinalfn<>0 THEN aggfinalfn::regprocedure::text END,
 CASE WHEN aggcombinefn<>0 THEN aggcombinefn::regprocedure::text END,agginitval
 FROM pg_catalog.pg_aggregate WHERE aggfnoid=$1`, oid).Scan(&v.Kind, &v.TransitionFunction, &v.StateType, &v.FinalFunction, &v.CombineFunction, &v.InitialCondition)
		out.Aggregate = v
	}
	return out, err
}

func describeType(ctx context.Context, tx pgx.Tx, oid uint32, budget *metadataBudget) (*database.TypeDescription, error) {
	out := &database.TypeDescription{EnumLabels: []string{}, Attributes: []database.Column{}, Constraints: []database.Definition{}}
	var relation uint32
	err := tx.QueryRow(ctx, `SELECT CASE typtype WHEN 'e' THEN 'enum' WHEN 'd' THEN 'domain' WHEN 'c' THEN 'composite' WHEN 'r' THEN 'range' WHEN 'm' THEN 'multirange' ELSE 'base' END,
 typcategory::text,CASE WHEN typbasetype<>0 THEN pg_catalog.format_type(typbasetype,typtypmod) END,
 typnotnull,pg_catalog.pg_get_expr(typdefaultbin,0),typrelid FROM pg_catalog.pg_type WHERE oid=$1`, oid).Scan(&out.Kind, &out.Category, &out.BaseType, &out.NotNull, &out.Default, &relation)
	if err != nil {
		return nil, err
	}
	if out.Kind == "enum" {
		rows, e := tx.Query(ctx, `SELECT enumlabel::text FROM pg_catalog.pg_enum WHERE enumtypid=$1 ORDER BY enumsortorder LIMIT 4097`, oid)
		if e != nil {
			return nil, e
		}
		defer rows.Close()
		for rows.Next() {
			var label string
			if e = rows.Scan(&label); e != nil {
				return nil, e
			}
			if e = budget.add(label); e != nil {
				return nil, e
			}
			out.EnumLabels = append(out.EnumLabels, label)
		}
		rows.Close()
		if e = rows.Err(); e != nil {
			return nil, e
		}
	}
	if out.Kind == "composite" {
		out.Attributes, err = columns(ctx, tx, relation, budget)
		if err != nil {
			return nil, err
		}
	}
	if out.Kind == "domain" {
		out.Constraints, err = readDefinitions(ctx, tx, budget, `SELECT conname::text,CASE contype WHEN 'c' THEN 'check' WHEN 'n' THEN 'not_null' ELSE contype::text END,pg_catalog.pg_get_constraintdef(oid) FROM pg_catalog.pg_constraint WHERE contypid=$1 ORDER BY conname::text COLLATE "C" LIMIT 4097`, oid)
		if err != nil {
			return nil, err
		}
	}
	if out.Kind == "range" || out.Kind == "multirange" {
		v := &database.RangeDescription{}
		err = tx.QueryRow(ctx, `SELECT pg_catalog.format_type(rngsubtype,NULL),pg_catalog.format_type(rngtypid,NULL),pg_catalog.format_type(rngmultitypid,NULL),
 CASE WHEN rngcollation<>0 THEN rngcollation::regcollation::text END,
 CASE WHEN rngcanonical<>0 THEN rngcanonical::regprocedure::text END,
 CASE WHEN rngsubdiff<>0 THEN rngsubdiff::regprocedure::text END FROM pg_catalog.pg_range WHERE rngtypid=$1 OR rngmultitypid=$1`, oid).Scan(&v.Subtype, &v.RangeType, &v.MultirangeType, &v.Collation, &v.CanonicalFunction, &v.SubtypeDiffFunction)
		out.Range = v
	}
	return out, err
}

func describeSequence(ctx context.Context, tx pgx.Tx, oid uint32, a database.Access) (*database.SequenceDescription, error) {
	out := &database.SequenceDescription{}
	err := tx.QueryRow(ctx, `SELECT pg_catalog.format_type(seqtypid,NULL),seqstart::text,seqincrement::text,seqmin::text,seqmax::text,seqcache::text,seqcycle FROM pg_catalog.pg_sequence WHERE seqrelid=$1`, oid).Scan(&out.DataType, &out.Start, &out.Increment, &out.Min, &out.Max, &out.Cache, &out.Cycle)
	if err != nil {
		return nil, err
	}
	args := append(scopeArgs(a.Profile.Scope), oid)
	var schema, name, column string
	err = tx.QueryRow(ctx, `SELECT n.nspname::text,c.relname::text,at.attname::text FROM pg_catalog.pg_depend d
 JOIN pg_catalog.pg_class c ON c.oid=d.refobjid JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
 JOIN pg_catalog.pg_attribute at ON at.attrelid=c.oid AND at.attnum=d.refobjsubid
 WHERE d.classid='pg_catalog.pg_class'::regclass AND d.objid=$3 AND d.refclassid='pg_catalog.pg_class'::regclass AND d.deptype IN ('a','i') AND `+visibleSQL, args...).Scan(&schema, &name, &column)
	if err == pgx.ErrNoRows {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	out.OwnerTable = &config.Table{Schema: schema, Name: name}
	out.OwnerColumn = &column
	return out, nil
}
