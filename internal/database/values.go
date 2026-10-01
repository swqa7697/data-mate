package database

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/swqa7697/data-mate/internal/contracts"
)

// JSONValue retains a JSON value as bounded RawMessage: exact numbers and
// duplicate object members survive, without allocating a potentially huge Go
// object graph.
func JSONValue(b []byte) (json.RawMessage, error) {
	if len(b) > 1<<20 {
		return nil, Fail(contracts.ResourceLimit, "JSON value exceeds size limit", false)
	}
	depth := 0
	quoted, escaped := false, false
	for _, c := range b {
		if quoted {
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				quoted = false
			}
			continue
		}
		switch c {
		case '"':
			quoted = true
		case '[', '{':
			depth++
			if depth > 64 {
				return nil, Fail(contracts.ResourceLimit, "JSON value exceeds nesting limit", false)
			}
		case ']', '}':
			depth--
		}
	}
	if !utf8.Valid(b) || !json.Valid(b) {
		return nil, UnsupportedValue()
	}
	return append(json.RawMessage(nil), b...), nil
}

// ParameterKind classifies one JSON query parameter.
type ParameterKind int

// JSON parameter kinds; each driver maps them to its own wire values.
const (
	NullParameter ParameterKind = iota
	StringParameter
	NumberParameter
	BoolParameter
	JSONParameter
)

// Parameter is one bounded query parameter. Text is the decoded string for
// StringParameter and compact JSON text otherwise, so numbers keep their
// original decimal spelling. Parameters are never interpolated into SQL.
type Parameter struct {
	Kind ParameterKind
	Text string
}

// InvalidParameters reports a malformed or over-limit query parameter.
func InvalidParameters() error {
	return Fail(contracts.InvalidArgument, "invalid query parameter type or value", false)
}

// Parameters decodes at most 256 values of 64 KiB each and 256 KiB in total.
func Parameters(input []json.RawMessage) ([]Parameter, error) {
	if len(input) > 256 {
		return nil, InvalidParameters()
	}
	out := make([]Parameter, len(input))
	total := 0
	for i, p := range input {
		total += len(p)
		if len(p) > 64<<10 || total > 256<<10 {
			return nil, InvalidParameters()
		}
		raw, err := JSONValue(p)
		if err != nil {
			return nil, InvalidParameters()
		}
		raw = bytes.TrimSpace(raw)
		switch {
		case bytes.Equal(raw, []byte("null")):
			out[i] = Parameter{Kind: NullParameter}
		case raw[0] == '"':
			var value string
			if json.Unmarshal(raw, &value) != nil || strings.ContainsRune(value, 0) {
				return nil, InvalidParameters()
			}
			out[i] = Parameter{Kind: StringParameter, Text: value}
		default:
			var b bytes.Buffer
			if json.Compact(&b, raw) != nil {
				return nil, InvalidParameters()
			}
			kind := NumberParameter
			switch raw[0] {
			case 't', 'f':
				kind = BoolParameter
			case '{', '[':
				kind = JSONParameter
			}
			out[i] = Parameter{Kind: kind, Text: b.String()}
		}
	}
	return out, nil
}
