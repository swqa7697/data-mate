// Package database defines the shared driver boundary used by CLI and MCP.
package database

import (
	"context"

	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
)

// Access carries a validated profile snapshot and private, in-memory credentials.
// Callers must hold their state lease until an operation and its cleanup finish.
// Never serialize or log Access; credentials do not belong in agent requests.
type Access struct {
	Profile  config.Profile
	password string
}

// NewAccess binds credentials supplied by the vault to a profile snapshot.
func NewAccess(p config.Profile, password string) Access { return Access{p, password} }

// Password is for the driver only, never a public output field.
func (a Access) Password() string { return a.password }

// Error is safe to return at a public boundary; upstream errors are never wrapped.
type Error struct{ contracts.Failure }

func (e *Error) Error() string { return string(e.Code) + ": " + e.Message }

// Fail constructs a redacted, typed boundary error.
func Fail(code contracts.Code, message string, retry bool) error {
	return &Error{contracts.Failure{Code: code, Message: message, Retryable: retry}}
}

// Readiness reports successful connection and policy checks without reading application rows.
type Readiness struct {
	ServerVersion int
	TLS           bool
}

// Table identifies a visible relation and its query support status.
type Table struct {
	Schema    string `json:"schema"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Supported bool   `json:"supported"`
	Reason    string `json:"reason,omitempty"`
}

// PageRequest carries an optional exact schema filter and authenticated keyset cursor.
type PageRequest struct {
	Schema   string
	Cursor   string
	PageSize int
}

// TablePage is the bounded list_tables result.
type TablePage struct {
	Connection string  `json:"connection"`
	Tables     []Table `json:"tables"`
	NextCursor *string `json:"next_cursor"`
}

// Column exposes safe type metadata without expressions or defaults.
type Column struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	Nullable  bool   `json:"nullable"`
	Supported bool   `json:"supported"`
}

// Key contains a primary or unique key without its server definition.
type Key struct {
	Kind    string   `json:"kind"`
	Columns []string `json:"columns"`
}

// Relationship references only another visible endpoint.
type Relationship struct {
	Columns       []string     `json:"columns"`
	Target        config.Table `json:"target"`
	TargetColumns []string     `json:"target_columns"`
}

// Description is the bounded describe_table result.
type Description struct {
	Connection    string         `json:"connection"`
	Schema        string         `json:"schema"`
	Table         string         `json:"table"`
	Kind          string         `json:"kind"`
	Supported     bool           `json:"supported"`
	Columns       []Column       `json:"columns"`
	Keys          []Key          `json:"keys"`
	Relationships []Relationship `json:"relationships"`
}

// ResultColumn preserves result labels and optional value encoding.
type ResultColumn struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Encoding string `json:"encoding,omitempty"`
}

// QueryResult defines the eventual bounded P5 result; P3 cannot produce rows.
type QueryResult struct {
	Connection string         `json:"connection"`
	Columns    []ResultColumn `json:"columns"`
	Rows       [][]any        `json:"rows"`
	RowCount   int            `json:"row_count"`
	Truncated  bool           `json:"truncated"`
	ElapsedMS  int64          `json:"elapsed_ms"`
}

// Driver never accepts agent-controlled credentials or connection strings.
type Driver interface {
	Validate(Access) error
	Test(context.Context, Access) (Readiness, error)
	ListTables(context.Context, Access, PageRequest) (TablePage, error)
	DescribeTable(context.Context, Access, config.Table) (Description, error)
	Query(context.Context, Access, string) (QueryResult, error)
	Invalidate(string)
	Close()
}
