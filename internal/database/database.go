// Package database defines the shared driver boundary used by CLI and MCP, and
// the driver-neutral bounds, codecs and diagnostics every driver implements.
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
// MySQL-family profiles have no database; their schemas are the visible databases.
type DatabaseDescription struct {
	Version  int                 `json:"version"`
	Alias    string              `json:"alias"`
	Driver   string              `json:"driver"`
	Database string              `json:"database,omitempty"`
	Schemas  []SchemaDescription `json:"schemas"`
}

// SchemaDescription groups catalog-visible application objects in a schema.
type SchemaDescription struct {
	Name      string         `json:"name"`
	Tables    []RelationName `json:"tables"`
	Enums     []CatalogName  `json:"enums"`
	Sequences []CatalogName  `json:"sequences"`
	Indexes   []IndexName    `json:"indexes"`
	Functions []CatalogName  `json:"functions"`
}

// CatalogName identifies an object without fetching its definition or values.
type CatalogName struct {
	Name string `json:"name"`
}

// IndexName identifies an index; Table is set where index names are only
// unique per table, as in MySQL and MariaDB.
type IndexName struct {
	Name  string `json:"name"`
	Table string `json:"table,omitempty"`
}

// RelationName describes a relation without reading its columns or rows.
type RelationName struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// Table identifies a catalog-visible relation; Kind uses the driver's vocabulary.
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
	Driver     string  `json:"driver"`
	Tables     []Table `json:"tables"`
	NextCursor *string `json:"next_cursor"`
}

// Column holds the column fields every driver describes; drivers embed it to add
// their native attributes. Stored expressions are exposed, never evaluated.
type Column struct {
	Name      string  `json:"name"`
	Type      string  `json:"type"`
	Nullable  bool    `json:"nullable"`
	Default   *string `json:"default"`
	Generated *string `json:"generated"`
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

// RelationCore holds the describe_table fields every driver returns; drivers
// embed it in their driver-shaped descriptions.
type RelationCore struct {
	Connection     string         `json:"connection"`
	Driver         string         `json:"driver"`
	Schema         string         `json:"schema"`
	Table          string         `json:"table"`
	Kind           string         `json:"kind"`
	Keys           []Key          `json:"keys"`
	Relationships  []Relationship `json:"relationships"`
	Constraints    []Definition   `json:"constraints"`
	Indexes        []Definition   `json:"indexes"`
	ViewDefinition *string        `json:"view_definition"`
}

// NewRelationCore starts a description with empty, never-null collections.
func NewRelationCore(a Access, schema, table string) RelationCore {
	return RelationCore{Connection: a.Profile.Alias, Driver: a.Profile.Driver, Schema: schema, Table: table, Keys: []Key{}, Relationships: []Relationship{}, Constraints: []Definition{}, Indexes: []Definition{}}
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

// Operations are the read-only operations one driver implements. Object pages
// and descriptions are driver-shaped documents marshaled unchanged by callers;
// only the owning driver reads their fields. No operation accepts
// agent-controlled credentials or connection strings.
type Operations interface {
	ValidateProfile(config.Profile) error
	Validate(Access) error
	Test(context.Context, Access) (Readiness, error)
	ListTables(context.Context, Access, PageRequest) (TablePage, error)
	ListObjects(context.Context, Access, ObjectPageRequest) (any, error)
	DescribeObject(context.Context, Access, ObjectRequest) (any, error)
	DescribeTable(context.Context, Access, config.Table) (any, error)
	DescribeDatabase(context.Context, Access) (DatabaseDescription, error)
	Query(context.Context, Access, QueryRequest) (QueryResult, error)
}

// Driver is the service boundary: driver operations plus the lifecycle of
// shared pools, transports and cursors.
type Driver interface {
	Operations
	Invalidate(string)
	Close()
}
