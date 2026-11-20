package database

import (
	"context"
	"encoding/json"
)

// DocumentOperations is the document-oriented capability of the MongoDB driver.
type DocumentOperations interface {
	ListCollections(context.Context, Access, CollectionPageRequest) (CollectionPage, error)
	DescribeCollection(context.Context, Access, CollectionRequest) (CollectionDescription, error)
	FindDocuments(context.Context, Access, FindRequest) (DocumentResult, error)
	AggregateDocuments(context.Context, Access, AggregateRequest) (DocumentResult, error)
}

// CollectionPageRequest selects a page of listable collections, optionally in one exact database.
type CollectionPageRequest struct {
	Database string
	Cursor   string
	PageSize int
}

// CollectionInfo identifies a collection without reading its options or documents.
// Type is collection, view, timeseries or other.
type CollectionInfo struct {
	Database string `json:"database"`
	Name     string `json:"name"`
	Type     string `json:"type"`
}

// CollectionPage is the bounded list_collections result.
type CollectionPage struct {
	Connection  string           `json:"connection"`
	Driver      string           `json:"driver"`
	Collections []CollectionInfo `json:"collections"`
	NextCursor  *string          `json:"next_cursor"`
}

// CollectionRequest identifies one collection, view or time-series collection.
type CollectionRequest struct {
	Database   string
	Collection string
}

// CollectionDescription reports creation options and index specifications.
// Options and each index are verbatim canonical Extended JSON objects: untrusted
// database text that is never evaluated. Views have no indexes.
type CollectionDescription struct {
	Connection string            `json:"connection"`
	Driver     string            `json:"driver"`
	Database   string            `json:"database"`
	Collection string            `json:"collection"`
	Type       string            `json:"type"`
	Options    json.RawMessage   `json:"options"`
	Indexes    []json.RawMessage `json:"indexes"`
}

// FindRequest carries one find. Filter, Projection and Sort are agent Extended
// JSON objects kept raw to preserve key order and exact number text; nil means
// omitted. They may contain data values and must never be logged.
type FindRequest struct {
	Database   string
	Collection string
	Filter     json.RawMessage
	Projection json.RawMessage
	Sort       json.RawMessage
	Skip       int
	RowLimit   int
}

// AggregateRequest carries one pipeline of raw Extended JSON stage documents.
type AggregateRequest struct {
	Database   string
	Collection string
	Pipeline   []json.RawMessage
	RowLimit   int
}

// DocumentResult holds complete documents as canonical Extended JSON, in server
// order. Truncated reports that documents beyond the row or byte limit were omitted.
type DocumentResult struct {
	Connection    string            `json:"connection"`
	Driver        string            `json:"driver"`
	Documents     []json.RawMessage `json:"documents"`
	DocumentCount int               `json:"document_count"`
	Truncated     bool              `json:"truncated"`
	ElapsedMS     int64             `json:"elapsed_ms"`
}
