// Package contracts defines bounded JSON and shared public data contracts.
package contracts

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
)

// Schemas contains versioned profile, tool, and fixture schemas.
//
//go:embed schemas/*.json
var Schemas embed.FS

// JSON reads one bounded UTF-8 JSON value, rejecting duplicate keys at every depth.
// Returned errors deliberately exclude input values, which may contain secrets.
func JSON(r io.Reader, limit int) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, int64(limit)+1))
	if err != nil || len(b) > limit || !utf8.Valid(b) {
		return nil, errors.New("invalid or oversized JSON input")
	}
	if err := checkJSON(b, 64); err != nil {
		return nil, errors.New("invalid JSON: duplicate key, syntax, or nesting limit")
	}
	return b, nil
}

// JSONMessage checks protocol input without imposing configuration-file budgets.
// Standard-library syntax limits still apply; duplicate keys are never accepted.
func JSONMessage(b []byte) error { return checkJSON(b, 0) }

func checkJSON(b []byte, maxDepth int) error {
	if !utf8.Valid(b) || !json.Valid(b) {
		return errors.New("invalid JSON")
	}
	type frame struct {
		keys map[string]bool
		key  bool
	}
	var stack []frame
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	complete := func() {
		if len(stack) > 0 && stack[len(stack)-1].keys != nil {
			stack[len(stack)-1].key = true
		}
	}
	for {
		token, err := d.Token()
		if err == io.EOF {
			return nil // json.Valid already required exactly one complete value.
		}
		if err != nil {
			return errors.New("invalid JSON")
		}
		if token == json.Delim('}') || token == json.Delim(']') {
			stack = stack[:len(stack)-1]
			complete()
			continue
		}
		if len(stack) > 0 && stack[len(stack)-1].key {
			f := &stack[len(stack)-1]
			key, ok := token.(string)
			if !ok || f.keys[key] {
				return errors.New("duplicate JSON key")
			}
			f.keys[key], f.key = true, false
			continue
		}
		if maxDepth > 0 && len(stack) > maxDepth {
			return errors.New("JSON nesting limit")
		}
		switch token {
		case json.Delim('{'):
			stack = append(stack, frame{keys: map[string]bool{}, key: true})
		case json.Delim('['):
			stack = append(stack, frame{})
		default:
			complete()
		}
	}
}

// Validate checks a JSON value against an embedded schema without exposing its contents.
// Call JSON first when accepting external input to enforce byte/depth/key bounds.
func Validate(name string, b []byte) error {
	raw, err := Schemas.ReadFile("schemas/" + name + ".json")
	if err != nil {
		return errors.New("schema unavailable")
	}
	var s jsonschema.Schema
	if err := json.Unmarshal(raw, &s); err != nil {
		return errors.New("invalid schema")
	}
	resolved, err := s.Resolve(nil)
	if err != nil {
		return errors.New("invalid schema reference")
	}
	// This validator uses float64 for numeric schema bounds; application decoding
	// uses typed integers or RawMessage and never round-trips values through it.
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return errors.New("invalid JSON")
	}
	if err := resolved.Validate(v); err != nil {
		return errors.New("input does not match " + name + " contract")
	}
	return nil
}
