package sqlpolicy

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"reflect"
	"sync"

	"github.com/swqa7697/data-mate/internal/contracts"
)

// CatalogSQL is constant trusted metadata SQL, never derived from agent text.
//
//go:embed catalog.sql
var CatalogSQL string

//go:embed catalog16.json
var catalog16 []byte

//go:embed catalog18.json
var catalog18 []byte

// decodeRecord requires exact property names and presence, including nullable
// properties. encoding/json's default struct matching is case-insensitive and
// silently supplies zero values for missing fields, which is unsuitable here.
// contracts.JSON has already checked duplicate keys and the full input budget.
func decodeRecord(b []byte, out any) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		return unsupported()
	}
	v := reflect.ValueOf(out).Elem()
	if len(fields) != v.NumField() {
		return unsupported()
	}
	for i := 0; i < v.NumField(); i++ {
		raw, ok := fields[v.Type().Field(i).Tag.Get("json")]
		if !ok {
			return unsupported()
		}
		dest := v.Field(i)
		if bytes.Equal(raw, []byte("null")) && dest.Kind() != reflect.Pointer && dest.Kind() != reflect.Slice {
			return unsupported()
		}
		if err := json.Unmarshal(raw, dest.Addr().Interface()); err != nil {
			return unsupported()
		}
	}
	return nil
}

type opKey struct {
	name        string
	left, right uint32
}
type aggregateKey struct {
	name string
	arg  uint32
	star bool
}
type castKey struct{ source, target uint32 }
type classKey struct {
	namespace, method uint32
	name              string
}
type memberKey struct {
	family, left, right uint32
	strategy            int32
	purpose             string
}
type supportKey struct {
	family, left, right uint32
	number              int32
}

type catalogIndex struct {
	types      map[uint32]scalarType
	operators  map[opKey]operator
	aggregates map[uint32]aggregate
	casts      map[castKey]cast
	classes    map[classKey]operatorClass
	members    map[memberKey]familyMember
	supports   map[supportKey]familySupport
	functions  map[uint32]procedure
}
type signatures struct {
	operators  map[opKey]Type
	aggregates map[aggregateKey]Type
	casts      map[castKey]bool
}
type catalogPolicy struct {
	catalog    catalogIndex
	signatures signatures
}

var policy16 = sync.OnceValues(func() (*catalogPolicy, error) { return loadPolicy(catalog16) })
var policy18 = sync.OnceValues(func() (*catalogPolicy, error) { return loadPolicy(catalog18) })

func policyFor(major int) (*catalogPolicy, error) {
	switch major {
	case 16:
		return policy16()
	case 18:
		return policy18()
	default:
		return nil, unsupported()
	}
}

// indexRecords rejects empty categories and duplicate semantic identities.
func indexRecords[K comparable, R any](rows []R, key func(R) K) (map[K]R, error) {
	if len(rows) == 0 {
		return nil, unsupported()
	}
	out := make(map[K]R, len(rows))
	for _, row := range rows {
		k := key(row)
		if _, exists := out[k]; exists {
			return nil, unsupported()
		}
		out[k] = row
	}
	return out, nil
}

func decodeCatalog(b []byte) (catalogIndex, error) {
	var out catalogIndex
	// Match the driver's hard wire budget, including synthetic/offline callers.
	raw, err := contracts.JSON(bytes.NewReader(b), 2<<20)
	if err != nil {
		return out, unsupported()
	}
	var doc catalogDocument
	if err = json.Unmarshal(raw, &doc); err != nil {
		return out, unsupported()
	}
	out.types, err = indexRecords(doc.Types, func(t scalarType) uint32 { return t.OID })
	if err != nil {
		return out, err
	}
	out.operators, err = indexRecords(doc.Operators, func(o operator) opKey { return opKey{o.Oprname, o.Oprleft, o.Oprright} })
	if err != nil {
		return out, err
	}
	out.aggregates, err = indexRecords(doc.Aggregates, func(a aggregate) uint32 { return a.Aggfnoid })
	if err != nil {
		return out, err
	}
	out.casts, err = indexRecords(doc.Casts, func(c cast) castKey { return castKey{c.Castsource, c.Casttarget} })
	if err != nil {
		return out, err
	}
	out.classes, err = indexRecords(doc.Classes, func(c operatorClass) classKey {
		return classKey{c.Opcnamespace, c.Opcmethod, c.Opcname}
	})
	if err != nil {
		return out, err
	}
	out.members, err = indexRecords(doc.Members, func(m familyMember) memberKey {
		return memberKey{m.Amopfamily, m.Amoplefttype, m.Amoprighttype, m.Amopstrategy, m.Amoppurpose}
	})
	if err != nil {
		return out, err
	}
	out.supports, err = indexRecords(doc.Supports, func(s familySupport) supportKey {
		return supportKey{s.Amprocfamily, s.Amproclefttype, s.Amprocrighttype, s.Amprocnum}
	})
	if err != nil {
		return out, err
	}
	out.functions, err = indexRecords(doc.Functions, func(p procedure) uint32 { return p.OID })
	if err != nil {
		return out, err
	}
	if !out.referencesValid() {
		return out, unsupported()
	}
	return out, nil
}

// referencesValid requires implementation records for every captured callback.
// Non-callable references (including internal argument types and operator-family
// member operator OIDs) retain their exact reviewed identities in the manifest.
func (c catalogIndex) referencesValid() bool {
	functionsExist := func(ids ...uint32) bool {
		for _, id := range ids {
			if id != 0 {
				if _, ok := c.functions[id]; !ok {
					return false
				}
			}
		}
		return true
	}
	for id, t := range c.types {
		if id == 0 || !functionsExist(t.Typinput, t.Typoutput, t.Typreceive, t.Typsend, t.Typmodin, t.Typmodout, t.Typanalyze, t.Typsubscript) {
			return false
		}
	}
	for _, o := range c.operators {
		if o.OID == 0 || !functionsExist(o.Oprcode, o.Oprrest, o.Oprjoin) {
			return false
		}
	}
	for _, a := range c.aggregates {
		if a.Aggfnoid == 0 || !functionsExist(a.Aggfnoid, a.Aggtransfn, a.Aggfinalfn, a.Aggcombinefn, a.Aggserialfn, a.Aggdeserialfn, a.Aggmtransfn, a.Aggminvtransfn, a.Aggmfinalfn) {
			return false
		}
		if c.functions[a.Aggfnoid].Prokind != "a" {
			return false
		}
	}
	for _, v := range c.casts {
		if !functionsExist(v.Castfunc) {
			return false
		}
	}
	for _, s := range c.supports {
		if s.Amproc == 0 || !functionsExist(s.Amproc) {
			return false
		}
	}
	for id, p := range c.functions {
		if id == 0 || !functionsExist(p.Prosupport) {
			return false
		}
	}
	return true
}

func loadPolicy(b []byte) (*catalogPolicy, error) {
	c, err := decodeCatalog(b)
	if err != nil {
		return nil, err
	}
	s := signatures{operators: make(map[opKey]Type), aggregates: make(map[aggregateKey]Type), casts: make(map[castKey]bool)}
	for k, o := range c.operators {
		s.operators[k] = Type(o.Oprresult)
	}
	for id := range c.aggregates {
		p := c.functions[id]
		if Type(p.Prorettype).Name() == "" {
			continue
		}
		args := p.Proargtypes
		if len(args) > 1 {
			continue
		}
		k := aggregateKey{name: p.Proname, star: len(args) == 0}
		if len(args) == 1 {
			k.arg = args[0]
		}
		if _, exists := s.aggregates[k]; exists {
			return nil, unsupported()
		}
		s.aggregates[k] = Type(p.Prorettype)
	}
	for k := range c.casts {
		s.casts[k] = true
	}
	return &catalogPolicy{c, s}, nil
}

// VerifyCatalog checks the exact reviewed semantic definitions for this major.
// Only embedded policy is cached; live catalog facts are verified on every call.
func VerifyCatalog(major int, actual []byte) error {
	expected, err := policyFor(major)
	if err != nil {
		return err
	}
	observed, err := decodeCatalog(actual)
	if err != nil || !reflect.DeepEqual(expected.catalog, observed) {
		return unsupported()
	}
	return nil
}
func loadSignatures(major int) (signatures, error) {
	p, err := policyFor(major)
	if err != nil {
		return signatures{}, err
	}
	return p.signatures, nil
}
func (s signatures) op(name string, a, b Type) (Type, error) {
	if t, ok := s.operators[opKey{name, uint32(a), uint32(b)}]; ok {
		return t, nil
	}
	return 0, unsupported()
}
func (s signatures) aggregate(name string, t Type, star bool) (Type, error) {
	if name == "count" {
		t = 2276
	}
	if star {
		t = 0
	}
	if result, ok := s.aggregates[aggregateKey{name, uint32(t), star}]; ok {
		return result, nil
	}
	return 0, unsupported()
}
func (s signatures) canCast(a, b Type) bool {
	return a == b || numeric(a) && numeric(b) && s.casts[castKey{uint32(a), uint32(b)}]
}
func (s signatures) ordered(t Type) bool {
	_, a := s.op("=", t, t)
	_, b := s.op("<", t, t)
	return a == nil && b == nil
}
