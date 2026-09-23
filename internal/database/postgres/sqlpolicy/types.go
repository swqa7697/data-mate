package sqlpolicy

import (
	"context"
	"fmt"
	"strings"
)

// Type is an exact built-in PostgreSQL OID, never an agent-supplied SQL spelling.
type Type uint32

var typeNames = map[Type]string{16: "bool", 17: "bytea", 20: "int8", 21: "int2", 23: "int4", 25: "text", 114: "json", 700: "float4", 701: "float8", 1042: "bpchar", 1043: "varchar", 1082: "date", 1083: "time", 1114: "timestamp", 1184: "timestamptz", 1700: "numeric", 2950: "uuid", 3802: "jsonb"}

// Name returns the canonical built-in name, or empty for an unsupported OID.
func (t Type) Name() string { return typeNames[t] }

// NamedType accepts canonical names only.
func NamedType(n string) Type {
	for t, s := range typeNames {
		if s == n {
			return t
		}
	}
	return 0
}
func numeric(t Type) bool   { return t == 20 || t == 21 || t == 23 || t == 700 || t == 701 || t == 1700 }
func quote(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
func typeSQL(t Type) string { return "pg_catalog." + quote(t.Name()) }

// Column and Relation are catalog facts supplied by the driver's trusted adapter.
type Column struct {
	Name string
	Type Type
}

// Identity captures a relation name and OID for the later lock/recheck phase.
type Identity struct {
	OID          uint32
	Schema, Name string
}

// Relation records a logical table, its captured closure and physical scans.
type Relation struct {
	Scans, Dependencies []Identity
	Schema, Name        string
	OID                 uint32
	Columns             []Column
	Only                bool
	// State fingerprints columns and hierarchy edges, including physical children.
	// The driver compares fresh state under locks before executing this plan.
	State string
}

// Catalog must enforce scope, privileges, storage/index/type support and provide
// a snapshot of every physical relation. It never receives original agent SQL.
type Catalog interface {
	Resolve(context.Context, string, string, bool) (Relation, error)
}

// Parameter is a text-format value with an explicit supported built-in type.
// Values are private execution data and must not be logged.
type Parameter struct {
	Type  Type
	Value *string
}

// Compiled is a catalog-bound plan, not execution permission. The executor must
// lock and recheck Relations and live catalog signatures before sending SQL.
type Compiled struct {
	SQL        string
	Parameters []Parameter
	Columns    []Column
	Relations  []Relation
}

func paramSQL(n int, t Type) string { return fmt.Sprintf("$%d::%s", n, typeSQL(t)) }
