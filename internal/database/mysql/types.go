package mysql

import "github.com/swqa7697/data-mate/internal/database"

// Column adds MySQL attributes to the shared column fields. OnUpdate holds the
// expression of an ON UPDATE clause, such as CURRENT_TIMESTAMP.
type Column struct {
	database.Column
	AutoIncrement bool    `json:"auto_increment"`
	OnUpdate      *string `json:"on_update"`
}

// Description is the MySQL-shaped describe_table result. Triggers are absent:
// MySQL shows them only to accounts with the TRIGGER privilege, which the
// read-only audit rejects.
type Description struct {
	database.RelationCore
	Engine  *string  `json:"engine"`
	Columns []Column `json:"columns"`
}

// Object identifies a function, procedure or MariaDB sequence; names are
// unique per schema and kind, so no signature is needed.
type Object struct {
	Kind   string `json:"kind"`
	Schema string `json:"schema"`
	Name   string `json:"name"`
}

// ObjectPage lists visible catalog objects.
type ObjectPage struct {
	Connection string   `json:"connection"`
	Driver     string   `json:"driver"`
	Objects    []Object `json:"objects"`
	NextCursor *string  `json:"next_cursor"`
}

// ObjectDescription contains exactly one kind-specific description.
type ObjectDescription struct {
	Connection string `json:"connection"`
	Driver     string `json:"driver"`
	Object
	Routine  *Routine  `json:"routine,omitempty"`
	Sequence *Sequence `json:"sequence,omitempty"`
}

// Parameter is one routine parameter; Mode is set for procedures only.
type Parameter struct {
	Name string  `json:"name"`
	Mode *string `json:"mode"`
	Type string  `json:"type"`
}

// Routine exposes declared characteristics and source without invoking it.
type Routine struct {
	Parameters    []Parameter `json:"parameters"`
	ReturnType    *string     `json:"return_type"`
	Language      string      `json:"language"`
	Deterministic bool        `json:"deterministic"`
	SQLDataAccess string      `json:"sql_data_access"`
	Security      string      `json:"security"`
	Definition    *string     `json:"definition"`
}

// Sequence contains MariaDB sequence configuration; reading it never advances it.
type Sequence struct {
	DataType  string `json:"data_type"`
	Start     string `json:"start"`
	Increment string `json:"increment"`
	Min       string `json:"min"`
	Max       string `json:"max"`
	Cache     string `json:"cache"`
	Cycle     bool   `json:"cycle"`
}
