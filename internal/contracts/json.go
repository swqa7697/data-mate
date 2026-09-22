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
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err := value(d, 0); err != nil {
		return nil, errors.New("invalid JSON: duplicate key, syntax, or nesting limit")
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, errors.New("expected one JSON value")
	}
	return b, nil
}

func value(d *json.Decoder, depth int) error {
	if depth > 64 {
		return errors.New("depth")
	}
	t, err := d.Token()
	if err != nil {
		return err
	}
	switch t {
	case json.Delim('{'):
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			s, ok := key.(string)
			if !ok || seen[s] {
				return errors.New("key")
			}
			seen[s] = true
			if err := value(d, depth+1); err != nil {
				return err
			}
		}
		t, err = d.Token()
		if err != nil || t != json.Delim('}') {
			return errors.New("object")
		}
	case json.Delim('['):
		for d.More() {
			if err := value(d, depth+1); err != nil {
				return err
			}
		}
		t, err = d.Token()
		if err != nil || t != json.Delim(']') {
			return errors.New("array")
		}
	default:
		if _, ok := t.(json.Delim); ok {
			return errors.New("delimiter")
		}
	}
	return nil
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
