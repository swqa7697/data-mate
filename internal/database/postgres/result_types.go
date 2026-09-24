package postgres

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
)

// These are output representations, not an authorization allowlist.
var scalarNames = map[uint32]string{16: "bool", 17: "bytea", 20: "int8", 21: "int2", 23: "int4", 25: "text", 114: "json", 700: "float4", 701: "float8", 1042: "bpchar", 1043: "varchar", 1082: "date", 1083: "time", 1114: "timestamp", 1184: "timestamptz", 1700: "numeric", 2950: "uuid", 3802: "jsonb"}

type resultType struct {
	oid, base, element uint32
	name               string
	delimiter          byte
	arrayPlan          pgtype.ScanPlan
}
type resultTypes map[uint32]resultType

// Look up just the described result types and their domain/array dependencies.
func describeTypes(ctx context.Context, tx pgx.Tx, fields []pgconn.FieldDescription) (resultTypes, error) {
	ids := make([]uint32, 0, len(fields))
	seen := map[uint32]bool{}
	for _, f := range fields {
		if !seen[f.DataTypeOID] {
			ids = append(ids, f.DataTypeOID)
			seen[f.DataTypeOID] = true
		}
	}
	out := resultTypes{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := tx.Query(ctx, `WITH RECURSIVE types(oid) AS (
 SELECT unnest($1::oid[]) UNION
 SELECT child FROM types x JOIN pg_catalog.pg_type t ON t.oid=x.oid
 CROSS JOIN LATERAL unnest(ARRAY[t.typbasetype,CASE WHEN t.typcategory='A' AND t.typinput='pg_catalog.array_in'::regproc THEN t.typelem ELSE 0::oid END]) child WHERE child<>0)
 SELECT t.oid,t.typbasetype,CASE WHEN t.typcategory='A' AND t.typinput='pg_catalog.array_in'::regproc THEN t.typelem ELSE 0::oid END,
 CASE WHEN n.nspname='pg_catalog' THEN t.typname::text ELSE pg_catalog.quote_ident(n.nspname)||'.'||pg_catalog.quote_ident(t.typname) END,t.typdelim::text
 FROM types JOIN pg_catalog.pg_type t USING(oid) JOIN pg_catalog.pg_namespace n ON n.oid=t.typnamespace LIMIT 8193`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var t resultType
		var delim string
		if err = rows.Scan(&t.oid, &t.base, &t.element, &t.name, &delim); err != nil {
			return nil, err
		}
		if len(delim) != 1 {
			return nil, codecError()
		}
		t.delimiter = delim[0]
		out[t.oid] = t
	}
	if len(out) > 8192 {
		return nil, database.Fail(contracts.ResourceLimit, "result type metadata exceeds limit", false)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	for _, id := range ids {
		if _, ok := out[id]; !ok {
			return nil, codecError()
		}
	}
	for id, t := range out {
		if t.element == 0 {
			continue
		}
		element, ok := out[t.element]
		if !ok {
			return nil, codecError()
		}
		// A query-local scan plan reuses pgx's text-array grammar across rows.
		m := pgtype.NewMap()
		et := &pgtype.Type{Name: "element", OID: t.element, Codec: pgtype.TextCodec{}}
		m.RegisterType(et)
		codec := &pgtype.ArrayCodec{ElementType: et, Delimiter: element.delimiter}
		t.arrayPlan = codec.PlanScan(m, id, pgtype.TextFormatCode, &boundedTextArray{})
		if t.arrayPlan == nil {
			return nil, codecError()
		}
		out[id] = t
	}
	return out, nil
}
func (types resultTypes) representation(oid uint32) (string, error) {
	for range 64 {
		t, ok := types[oid]
		if !ok {
			return "", codecError()
		}
		if t.base != 0 {
			oid = t.base
			continue
		}
		if t.element != 0 {
			oid = t.element
			continue
		}
		if oid == 17 {
			return "base64", nil
		}
		if _, ok := scalarNames[oid]; ok {
			return "", nil
		}
		return "postgres_text", nil
	}
	return "", codecError()
}
func (types resultTypes) decode(oid uint32, raw []byte, depth int) (any, error) {
	if raw == nil {
		return nil, nil
	}
	if depth > 64 {
		return nil, database.Fail(contracts.ResourceLimit, "result nesting exceeds limit", false)
	}
	t, ok := types[oid]
	if !ok || !utf8.Valid(raw) {
		return nil, codecError()
	}
	if t.base != 0 {
		return types.decode(t.base, raw, depth+1)
	}
	if t.element == 0 {
		if _, ok := scalarNames[oid]; ok {
			return decodeValue(oid, raw)
		}
		return string(raw), nil
	}
	// Capture raw elements, then use the same exact scalar representations.
	array := boundedTextArray{limit: len(raw) + 1}
	if t.arrayPlan == nil {
		return nil, codecError()
	}
	if err := t.arrayPlan.Scan(raw, &array); err != nil {
		return nil, codecError()
	}
	if len(array.Dims) == 0 {
		return []any{}, nil
	}
	index := 0
	var nested func(int) ([]any, error)
	nested = func(dim int) ([]any, error) {
		values := make([]any, int(array.Dims[dim].Length))
		for i := range values {
			var err error
			if dim+1 < len(array.Dims) {
				values[i], err = nested(dim + 1)
			} else {
				v := array.Elements[index]
				index++
				if v.Valid {
					values[i], err = types.decode(t.element, []byte(v.String), depth+1)
				}
			}
			if err != nil {
				return nil, err
			}
		}
		return values, nil
	}
	return nested(0)
}

// Bound dimension-driven allocations before pgx materializes array elements.
type boundedTextArray struct {
	pgtype.Array[pgtype.Text]
	limit int
}

func (a *boundedTextArray) SetDimensions(d []pgtype.ArrayDimension) error {
	if len(d) > 6 {
		return codecError()
	}
	n := 1
	for _, v := range d {
		if v.Length < 0 || int64(v.Length) > int64(a.limit) || v.Length != 0 && n > a.limit/int(v.Length) {
			return codecError()
		}
		n *= int(v.Length)
	}
	return a.Array.SetDimensions(d)
}

func validResultName(s string) bool { return utf8.ValidString(s) && !strings.ContainsRune(s, 0) }
