package database

import "github.com/swqa7697/data-mate/internal/config"

// Object identifies a catalog object. IdentityArguments distinguishes routines,
// including the empty signature; Extension is null for non-extension objects.
type Object struct {
	Kind              string  `json:"kind"`
	Schema            string  `json:"schema"`
	Name              string  `json:"name"`
	IdentityArguments *string `json:"identity_arguments,omitempty"`
	Extension         *string `json:"extension"`
}

// ObjectPageRequest selects a bounded catalog page.
type ObjectPageRequest struct {
	PageRequest
	Kind string
}

// ObjectRequest identifies one object without accepting SQL fragments.
type ObjectRequest struct {
	Kind, Schema, Name string
	IdentityArguments  *string
}

// ObjectPage lists visible non-relation catalog objects.
type ObjectPage struct {
	Connection string   `json:"connection"`
	Objects    []Object `json:"objects"`
	NextCursor *string  `json:"next_cursor"`
}

// ObjectDescription contains exactly one kind-specific description.
type ObjectDescription struct {
	Connection string `json:"connection"`
	Object
	Routine  *RoutineDescription  `json:"routine,omitempty"`
	Type     *TypeDescription     `json:"type,omitempty"`
	Sequence *SequenceDescription `json:"sequence,omitempty"`
}

// Definition is a server-deparsed definition, never evaluated by metadata tools.
type Definition struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Definition string `json:"definition"`
}

// RoutineDescription exposes source without invoking the routine.
type RoutineDescription struct {
	Kind            string                `json:"kind"`
	Arguments       string                `json:"arguments"`
	ReturnType      *string               `json:"return_type"`
	Language        string                `json:"language"`
	Volatility      string                `json:"volatility"`
	SecurityDefiner bool                  `json:"security_definer"`
	Definition      *string               `json:"definition"`
	Aggregate       *AggregateDescription `json:"aggregate,omitempty"`
}

// AggregateDescription describes the principal aggregate implementation hooks.
type AggregateDescription struct {
	Kind               string  `json:"kind"`
	TransitionFunction string  `json:"transition_function"`
	StateType          string  `json:"state_type"`
	FinalFunction      *string `json:"final_function"`
	CombineFunction    *string `json:"combine_function"`
	InitialCondition   *string `json:"initial_condition"`
}

// TypeDescription describes explicit types, not automatic arrays or table rows.
type TypeDescription struct {
	Kind        string            `json:"kind"`
	Category    string            `json:"category"`
	BaseType    *string           `json:"base_type"`
	NotNull     bool              `json:"not_null"`
	Default     *string           `json:"default"`
	EnumLabels  []string          `json:"enum_labels"`
	Attributes  []Column          `json:"attributes"`
	Constraints []Definition      `json:"constraints"`
	Range       *RangeDescription `json:"range,omitempty"`
}

// RangeDescription identifies a range's subtype and related range types.
type RangeDescription struct {
	Subtype             string  `json:"subtype"`
	RangeType           string  `json:"range_type"`
	MultirangeType      string  `json:"multirange_type"`
	Collation           *string `json:"collation"`
	CanonicalFunction   *string `json:"canonical_function"`
	SubtypeDiffFunction *string `json:"subtype_diff_function"`
}

// SequenceDescription contains configuration only; inspecting it never advances a sequence.
type SequenceDescription struct {
	DataType    string        `json:"data_type"`
	Start       string        `json:"start"`
	Increment   string        `json:"increment"`
	Min         string        `json:"min"`
	Max         string        `json:"max"`
	Cache       string        `json:"cache"`
	Cycle       bool          `json:"cycle"`
	OwnerTable  *config.Table `json:"owner_table"`
	OwnerColumn *string       `json:"owner_column"`
}

// Trigger describes table triggers, including server-created constraint triggers.
type Trigger struct {
	Name       string `json:"name"`
	Definition string `json:"definition"`
	Enabled    string `json:"enabled"`
	Internal   bool   `json:"internal"`
}

// Policy exposes stored RLS expressions without evaluating them.
type Policy struct {
	Name       string   `json:"name"`
	Command    string   `json:"command"`
	Permissive bool     `json:"permissive"`
	Roles      []string `json:"roles"`
	Using      *string  `json:"using"`
	Check      *string  `json:"check"`
}
