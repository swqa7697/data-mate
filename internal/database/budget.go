package database

import (
	"encoding/json"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/swqa7697/data-mate/internal/contracts"
)

// UnsupportedValue reports a result value that has no safe JSON representation.
func UnsupportedValue() error {
	return Fail(contracts.QueryUnsupported, "result value has an unsupported representation", false)
}

// ValidResultName accepts any result label, including duplicates and empty
// labels, that can be represented as JSON text.
func ValidResultName(s string) bool { return utf8.ValidString(s) && !strings.ContainsRune(s, 0) }

// PayloadBound rejects metadata results that cannot fit the profile byte cap,
// including JSON escaping and the duplicated MCP compatibility representation.
func PayloadBound(v any, a Access) error {
	if a.Profile.Limits == nil || a.Profile.Limits.MaxResultBytes == 0 {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if ResultBytes(b) > a.Profile.Limits.MaxResultBytes {
		return Fail(contracts.ResourceLimit, "metadata result exceeds payload budget; request a smaller page", false)
	}
	return nil
}

// MetadataBudget bounds cumulative collections as they are read, before an
// envelope is constructed. Limit is the profile's result-byte cap.
type MetadataBudget struct {
	Limit int
	bytes int
}

// Add charges one collection entry.
func (b *MetadataBudget) Add(v any) error {
	if b.Limit == 0 {
		return nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b.bytes += len(raw)
	if b.bytes > b.Limit {
		return Fail(contracts.ResourceLimit, "metadata exceeds payload limit", false)
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

// ArrayBudget accounts the MCP envelope of a result whose elements form one
// JSON array, so that appending elements never exceeds the profile byte cap.
// The zero value has no cap.
type ArrayBudget struct {
	used, limit int
}

// NewArrayBudget reserves the complete tool result for header, the result
// with an empty array and every other field at its maximum width, so each
// element is then counted exactly once: false is one byte longer than true and
// a count can never exceed its row limit. A header over limit fails with
// message. A limit that is not positive is no cap, and header is not encoded.
func NewArrayBudget(limit int, header any, message string) (ArrayBudget, error) {
	if limit <= 0 {
		return ArrayBudget{}, nil
	}
	b, err := json.Marshal(header)
	if err != nil {
		return ArrayBudget{}, err
	}
	used := ResultBytes(b)
	if used > limit {
		return ArrayBudget{}, Fail(contracts.ResourceLimit, message, false)
	}
	return ArrayBudget{used: used, limit: limit}, nil
}

// Limited reports whether a byte cap applies. Without one, elements need not
// be encoded to be charged.
func (b *ArrayBudget) Limited() bool { return b.limit > 0 }

// Charge counts one element whose compact JSON is encoded, appended to an
// array that already holds index elements, and reports whether it fits; an
// element that does not fit is not counted. Relative to an empty [], the
// element contributes its JSON and its JSON-string escaped form (without the
// latter's two quotes), plus two commas after the first.
func (b *ArrayBudget) Charge(encoded []byte, index int) bool {
	if b.limit == 0 {
		return true
	}
	quoted, _ := json.Marshal(string(encoded))
	cost := len(encoded) + len(quoted) - 2
	if index > 0 {
		cost += 2
	}
	if b.used+cost > b.limit {
		return false
	}
	b.used += cost
	return true
}

// ResultBuilder accounts a query envelope as rows arrive so a result never
// exceeds the profile byte cap or row limit. When Next or Add reports false the
// result is truncated: callers must close the connection instead of draining
// unread rows, then return Result.
type ResultBuilder struct {
	out      QueryResult
	budget   ArrayBudget
	rowLimit int
	started  time.Time
}

// NewResult reserves the envelope header at maximum field widths, so each
// appended row is counted exactly once.
func NewResult(a Access, columns []ResultColumn, rowLimit int, started time.Time) (*ResultBuilder, error) {
	out := QueryResult{Connection: a.Profile.Alias, Columns: columns, Rows: [][]any{}}
	header := out
	header.RowCount, header.ElapsedMS = rowLimit, math.MaxInt64
	budget, err := NewArrayBudget(a.Profile.Limits.MaxResultBytes, header, "column metadata exceeds result budget")
	if err != nil {
		return nil, err
	}
	return &ResultBuilder{out: out, budget: budget, rowLimit: rowLimit, started: started}, nil
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
	if b.budget.Limited() {
		encoded, err := json.Marshal(row)
		if err != nil {
			return false, UnsupportedValue()
		}
		if !b.budget.Charge(encoded, len(b.out.Rows)) {
			b.out.Truncated = true
			return false, nil
		}
	}
	b.out.Rows = append(b.out.Rows, row)
	return true, nil
}

// Result finalizes counts and elapsed time.
func (b *ResultBuilder) Result() QueryResult {
	b.out.RowCount = len(b.out.Rows)
	b.out.ElapsedMS = time.Since(b.started).Milliseconds()
	return b.out
}
