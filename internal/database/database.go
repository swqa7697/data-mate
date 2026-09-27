// Package database defines the shared driver boundary used by CLI and MCP.
package database

import (
	"context"
	"encoding/json"
	"time"

	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/transport"
)

// Shared admission bounds apply independently at the service and driver boundaries.
const (
	MaxActiveOperations  = 32
	MaxWaitingOperations = 128
	AdmissionTimeout     = 60 * time.Second
)

// Access carries a validated profile snapshot and private, in-memory credentials.
// Callers must hold their state lease until an operation and its cleanup finish.
// Never serialize or log Access; credentials do not belong in agent requests.
type Access struct {
	Profile          config.Profile
	password         string
	transportSecrets transport.Credentials
	knownHosts       []byte
}

// NewAccess binds credentials supplied by the vault to a profile snapshot.
func NewAccess(p config.Profile, password string) Access {
	return Access{Profile: p, password: password}
}

// WithTransport binds private vault credentials and an owned known-host snapshot.
// Callers read known hosts under the same state lease as the profile/credentials.
func (a Access) WithTransport(secrets transport.Credentials, knownHosts []byte) Access {
	a.transportSecrets = secrets
	a.knownHosts = append([]byte(nil), knownHosts...)
	return a
}

// TransportCredentials is for driver configuration only, never public output.
func (a Access) TransportCredentials() (transport.Credentials, []byte) {
	return a.transportSecrets, append([]byte(nil), a.knownHosts...)
}

// Password is for the driver only, never a public output field.
func (a Access) Password() string { return a.password }

// Error is safe to return at a public boundary; upstream errors are never wrapped.
type Error struct{ contracts.Failure }

func (e *Error) Error() string { return string(e.Code) + ": " + e.Message }

// Fail constructs a redacted, typed boundary error.
func Fail(code contracts.Code, message string, retry bool) error {
	return &Error{contracts.Failure{Code: code, Message: message, Retryable: retry}}
}

// Readiness reports successful connection and read-only transaction checks without reading application rows.
type Readiness struct {
	ServerVersion int
	TLS           bool
	Stage         string
	Stages        []Stage
}

// Stage records one reached diagnostic stage; skipped stages are omitted.
type Stage struct {
	Stage string             `json:"stage"`
	OK    bool               `json:"ok"`
	Error *contracts.Failure `json:"error,omitempty"`
}

// DatabaseDescription is the complete, bounded user-facing catalog. It is never
// an MCP result; it includes empty application schemas but excludes system schemas.
type DatabaseDescription struct {
	Version  int                 `json:"version"`
	Alias    string              `json:"alias"`
	Database string              `json:"database"`
	Schemas  []SchemaDescription `json:"schemas"`
}

// SchemaDescription groups catalog-visible application objects in a schema.
type SchemaDescription struct {
	Name      string         `json:"name"`
	Tables    []RelationName `json:"tables"`
	Enums     []CatalogName  `json:"enums"`
	Sequences []CatalogName  `json:"sequences"`
}

// CatalogName identifies an object without fetching its definition or values.
type CatalogName struct {
	Name string `json:"name"`
}

// RelationName describes a relation without reading its columns or rows.
type RelationName struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// Table identifies a catalog-visible relation.
type Table struct {
	Schema string `json:"schema"`
	Name   string `json:"name"`
	Kind   string `json:"kind"`
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

// Column exposes type metadata and stored expressions without evaluating them.
type Column struct {
	Name      string  `json:"name"`
	Type      string  `json:"type"`
	Nullable  bool    `json:"nullable"`
	Default   *string `json:"default"`
	Generated *string `json:"generated"`
	Identity  string  `json:"identity"`
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
	Connection       string         `json:"connection"`
	Schema           string         `json:"schema"`
	Table            string         `json:"table"`
	Kind             string         `json:"kind"`
	Columns          []Column       `json:"columns"`
	Keys             []Key          `json:"keys"`
	Relationships    []Relationship `json:"relationships"`
	Constraints      []Definition   `json:"constraints"`
	Indexes          []Definition   `json:"indexes"`
	Triggers         []Trigger      `json:"triggers"`
	Policies         []Policy       `json:"policies"`
	ViewDefinition   *string        `json:"view_definition"`
	RowSecurity      bool           `json:"row_security"`
	ForceRowSecurity bool           `json:"force_row_security"`
}

// ResultColumn preserves result labels and optional value encoding.
type ResultColumn struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Encoding string `json:"encoding,omitempty"`
}

// QueryRequest can lower the profile's row cap, never its authorization or limits.
// Zero RowLimit uses the profile cap. SQL and parameters must never be logged.
type QueryRequest struct {
	SQL        string
	Parameters []json.RawMessage
	RowLimit   int
}

// QueryResult is prepared within the state lease and includes only complete rows.
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
	ListObjects(context.Context, Access, ObjectPageRequest) (ObjectPage, error)
	DescribeObject(context.Context, Access, ObjectRequest) (ObjectDescription, error)
	DescribeTable(context.Context, Access, config.Table) (Description, error)
	Query(context.Context, Access, QueryRequest) (QueryResult, error)
	Invalidate(string)
	Close()
}
