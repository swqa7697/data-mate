package database

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/swqa7697/data-mate/internal/contracts"
)

// JSONValue retains a JSON value as RawMessage: exact numbers and
// duplicate object members survive, without allocating a potentially huge Go
// object graph.
func JSONValue(b []byte) (json.RawMessage, error) {
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

// Parameter is one query parameter. Text is the decoded string for
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

// Parameters decodes query values within the prepared-statement protocol count.
func Parameters(input []json.RawMessage) ([]Parameter, error) {
	if len(input) > 1<<16-1 {
		return nil, InvalidParameters()
	}
	out := make([]Parameter, len(input))
	for i, p := range input {
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
