package database

import (
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
)

// MaxCatalogObjects bounds metadata collections and catalog descriptions.
const MaxCatalogObjects = 4096

// UnsupportedValue reports a result value that has no safe JSON representation.
func UnsupportedValue() error {
	return Fail(contracts.QueryUnsupported, "result value has an unsupported representation", false)
}

// ValidResultName accepts any result label, including duplicates and empty
// labels, that can be represented as JSON text.
func ValidResultName(s string) bool { return utf8.ValidString(s) && !strings.ContainsRune(s, 0) }

// PayloadBound rejects metadata results that cannot fit the profile byte cap,
// reserving JSON escaping plus the duplicated MCP compatibility representation.
func PayloadBound(v any, a Access) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	limit := config.DefaultLimits().MaxResultBytes
	if a.Profile.Limits != nil {
		limit = a.Profile.Limits.MaxResultBytes
	}
	if len(b)*3+1024 > limit {
		return Fail(contracts.ResourceLimit, "metadata result exceeds payload budget; request a smaller page", false)
	}
	return nil
}

// MetadataBudget bounds cumulative collections as they are read, before an
// envelope is constructed. Limit is the profile's result-byte cap.
type MetadataBudget struct {
	Limit        int
	count, bytes int
}

// Add charges one collection entry.
func (b *MetadataBudget) Add(v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b.count++
	b.bytes += len(raw)
	if b.count > MaxCatalogObjects || b.bytes > b.Limit {
		return Fail(contracts.ResourceLimit, "metadata exceeds catalog or payload limit", false)
	}
	return nil
}

// ResultBytes counts a complete tool result for an encoded value, including the
// duplicated compact JSON text, exactly as the MCP handler sends it.
func ResultBytes(b []byte) int {
	quoted, _ := json.Marshal(string(b))
	return len(`{"content":[{"type":"text","text":`) + len(quoted) + len(`}],"structuredContent":`) + len(b) + len(`}`)
}

// QueryPayloadSize counts the complete query tool result. Values are never
// converted through floating point here.
func QueryPayloadSize(result QueryResult) (int, error) {
	b, err := json.Marshal(result)
	if err != nil {
		return 0, err
	}
	return ResultBytes(b), nil
}

// ResultBuilder accounts a query envelope as rows arrive so a result never
// exceeds the profile byte cap or row limit. When Next or Add reports false the
// result is truncated: callers must close the connection instead of draining
// unread rows, then return Result.
type ResultBuilder struct {
	out      QueryResult
	used     int
	limit    int
	rowLimit int
	started  time.Time
}

// NewResult reserves the envelope header at maximum field widths, so each
// appended row is counted exactly once. false is one byte longer than true and
// RowCount can never exceed the row limit.
func NewResult(a Access, columns []ResultColumn, rowLimit int, started time.Time) (*ResultBuilder, error) {
	out := QueryResult{Connection: a.Profile.Alias, Columns: columns, Rows: [][]any{}, RowCount: rowLimit, ElapsedMS: 1<<63 - 1}
	header, _ := json.Marshal(out)
	used := ResultBytes(header)
	if used > a.Profile.Limits.MaxResultBytes {
		return nil, Fail(contracts.ResourceLimit, "column metadata exceeds result budget", false)
	}
	out.RowCount = 0
	out.ElapsedMS = 0
	return &ResultBuilder{out: out, used: used, limit: a.Profile.Limits.MaxResultBytes, rowLimit: rowLimit, started: started}, nil
}

// Next reports whether another row may be read. At the row limit the extra row
// is not decoded and the result is marked truncated.
func (b *ResultBuilder) Next() bool {
	if len(b.out.Rows) == b.rowLimit {
		b.out.Truncated = true
		return false
	}
	return true
}

// Add appends one decoded row unless it would exceed the byte cap, in which
// case the result is marked truncated and the row is dropped.
func (b *ResultBuilder) Add(row []any) (bool, error) {
	encoded, err := json.Marshal(row)
	if err != nil {
		return false, UnsupportedValue()
	}
	// Relative to empty [], a row contributes its JSON and its JSON-string
	// escaped form (without the latter's two quotes), plus two commas after row 1.
	quoted, _ := json.Marshal(string(encoded))
	cost := len(encoded) + len(quoted) - 2
	if len(b.out.Rows) > 0 {
		cost += 2
	}
	if b.used+cost > b.limit {
		b.out.Truncated = true
		return false, nil
	}
	b.used += cost
	b.out.Rows = append(b.out.Rows, row)
	return true, nil
}

// Result finalizes counts and elapsed time.
func (b *ResultBuilder) Result() QueryResult {
	b.out.RowCount = len(b.out.Rows)
	b.out.ElapsedMS = time.Since(b.started).Milliseconds()
	return b.out
}
