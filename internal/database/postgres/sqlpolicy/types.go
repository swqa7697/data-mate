package sqlpolicy

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Type is an exact built-in PostgreSQL OID, never an agent-supplied SQL spelling.
type Type uint32

// UnmarshalJSON accepts the numeric OID representation in frozen catalogs.
func (t *Type) UnmarshalJSON(b []byte) error {
	n, err := strconv.ParseUint(strings.Trim(string(b), `"`), 10, 32)
	*t = Type(n)
	return err
}

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

// Compiled is a catalog-bound plan, not execution permission. P5 must lock and
// recheck Relations and the catalog signatures before sending SQL to the server.
type Compiled struct {
	SQL        string
	Parameters []Parameter
	Columns    []Column
	Relations  []Relation
}

// CatalogSQL is constant trusted metadata SQL, never derived from agent text.
//
//go:embed catalog.sql
var CatalogSQL string

//go:embed catalog16.json
var catalog16 []byte

//go:embed catalog18.json
var catalog18 []byte

type operator struct {
	Name   string `json:"oprname"`
	Left   Type   `json:"oprleft"`
	Right  Type   `json:"oprright"`
	Result Type   `json:"oprresult"`
}
type procedure struct {
	OID    Type   `json:"oid"`
	Name   string `json:"proname"`
	Args   []Type `json:"proargtypes"`
	Result Type   `json:"prorettype"`
	Kind   string `json:"prokind"`
}
type cast struct {
	Source Type `json:"castsource"`
	Target Type `json:"casttarget"`
}
type signatures struct {
	Operators []operator  `json:"operators"`
	Functions []procedure `json:"functions"`
	Casts     []cast      `json:"casts"`
}

func catalogBytes(major int) []byte {
	switch major {
	case 16:
		return catalog16
	case 18:
		return catalog18
	}
	return nil
}

// VerifyCatalog compares exact per-major built-in identities and implementations,
// including aggregate transition/final and btree support functions. No runtime
// discovery can extend this checked-in allowlist.
func VerifyCatalog(major int, actual []byte) error {
	expected := catalogBytes(major)
	if expected == nil {
		return unsupported()
	}
	var a, b any
	if json.Unmarshal(expected, &a) != nil || json.Unmarshal(actual, &b) != nil {
		return unsupported()
	}
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	if string(x) != string(y) {
		return unsupported()
	}
	return nil
}
func loadSignatures(major int) (signatures, error) {
	var s signatures
	b := catalogBytes(major)
	if b == nil || json.Unmarshal(b, &s) != nil {
		return s, unsupported()
	}
	return s, nil
}
func (s signatures) op(name string, a, b Type) (Type, error) {
	for _, o := range s.Operators {
		if o.Name == name && o.Left == a && o.Right == b {
			return o.Result, nil
		}
	}
	return 0, unsupported()
}
func (s signatures) aggregate(name string, t Type, star bool) (Type, error) {
	for _, p := range s.Functions {
		if p.Kind != "a" || p.Name != name {
			continue
		}
		arg := t
		if name == "count" {
			arg = 2276
		}
		match := len(p.Args) == 1 && p.Args[0] == arg
		if star {
			match = len(p.Args) == 0
		}
		if match && p.Result.Name() != "" {
			return p.Result, nil
		}
	}
	return 0, unsupported()
}
func (s signatures) canCast(a, b Type) bool {
	if a == b {
		return true
	}
	if !numeric(a) || !numeric(b) {
		return false
	}
	for _, c := range s.Casts {
		if c.Source == a && c.Target == b {
			return true
		}
	}
	return false
}
func (s signatures) ordered(t Type) bool {
	_, a := s.op("=", t, t)
	_, b := s.op("<", t, t)
	return a == nil && b == nil
}
func paramSQL(n int, t Type) string { return fmt.Sprintf("$%d::%s", n, typeSQL(t)) }
