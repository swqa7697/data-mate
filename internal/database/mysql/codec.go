package mysql

import (
	"database/sql/driver"
	"encoding/base64"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/swqa7697/data-mate/internal/database"
)

var (
	decimalText = regexp.MustCompile(`^[+-]?[0-9]+(?:\.[0-9]+)?$`)
	integerText = regexp.MustCompile(`^[+-]?[0-9]+$`)
)

// codec decodes one result column. Values arrive as the client library's
// binary-protocol types, or text-protocol types for unpreparable statements.
type codec struct {
	name     string
	encoding string
	decode   func(driver.Value) (any, error)
}

// typeName spells a library type name as MySQL DDL does, e.g. "bigint unsigned".
func typeName(library string) string {
	t := strings.ToLower(library)
	if rest, ok := strings.CutPrefix(t, "unsigned "); ok {
		return rest + " unsigned"
	}
	if t == "" {
		return "unknown"
	}
	return t
}

func codecFor(library string) codec {
	name := typeName(library)
	base, _, _ := strings.Cut(name, " ")
	switch base {
	case "tinyint", "smallint", "mediumint", "int", "year":
		return codec{name: name, decode: integerValue}
	case "bigint", "decimal":
		return codec{name: name, decode: exactValue}
	case "bit":
		return codec{name: name, decode: bitValue}
	case "float", "double":
		return codec{name: name, decode: floatValue}
	case "date", "time":
		return codec{name: name, decode: textValue}
	case "datetime":
		return codec{name: name, decode: func(v driver.Value) (any, error) { return temporalValue(v, "") }}
	case "timestamp":
		// The session time zone is pinned to UTC.
		return codec{name: name, decode: func(v driver.Value) (any, error) { return temporalValue(v, "Z") }}
	case "char", "varchar", "tinytext", "text", "mediumtext", "longtext", "enum", "set":
		return codec{name: name, decode: textValue}
	case "json":
		return codec{name: name, decode: jsonValue}
	case "null":
		return codec{name: name, decode: func(driver.Value) (any, error) { return nil, nil }}
	}
	// Binary strings, spatial and vector values, and unknown types keep their
	// exact bytes.
	return codec{name: name, encoding: "base64", decode: binaryValue}
}

func raw(v driver.Value) ([]byte, bool) {
	switch x := v.(type) {
	case []byte:
		return x, true
	case string:
		return []byte(x), true
	}
	return nil, false
}

func integerValue(v driver.Value) (any, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil
	case int64:
		return x, nil
	case uint64:
		if x <= math.MaxInt64 {
			return int64(x), nil
		}
	}
	if b, ok := raw(v); ok {
		if n, err := strconv.ParseInt(string(b), 10, 64); err == nil {
			return n, nil
		}
	}
	return nil, database.UnsupportedValue()
}

// exactValue keeps 64-bit integers and decimals as exact strings.
func exactValue(v driver.Value) (any, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil
	case int64:
		return strconv.FormatInt(x, 10), nil
	case uint64:
		return strconv.FormatUint(x, 10), nil
	}
	if b, ok := raw(v); ok && decimalText.Match(b) {
		return string(b), nil
	}
	return nil, database.UnsupportedValue()
}

// bitValue renders a big-endian BIT value as an exact unsigned decimal string.
func bitValue(v driver.Value) (any, error) {
	if v == nil {
		return nil, nil
	}
	if x, ok := v.(int64); ok && x >= 0 {
		return strconv.FormatInt(x, 10), nil
	}
	b, ok := raw(v)
	if !ok || len(b) > 8 {
		return nil, database.UnsupportedValue()
	}
	var n uint64
	for _, c := range b {
		n = n<<8 | uint64(c)
	}
	return strconv.FormatUint(n, 10), nil
}

func floatValue(v driver.Value) (any, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil
	case float32:
		if !math.IsNaN(float64(x)) && !math.IsInf(float64(x), 0) {
			return x, nil
		}
	case float64:
		if !math.IsNaN(x) && !math.IsInf(x, 0) {
			return x, nil
		}
	}
	return nil, database.UnsupportedValue()
}

func textValue(v driver.Value) (any, error) {
	if v == nil {
		return nil, nil
	}
	b, ok := raw(v)
	if !ok || !utf8.Valid(b) {
		return nil, database.UnsupportedValue()
	}
	return string(b), nil
}

// temporalValue writes DATETIME as YYYY-MM-DDTHH:MM:SS[.f] without inventing
// a zone and TIMESTAMP in UTC with Z. Zero dates keep their server spelling.
func temporalValue(v driver.Value, zone string) (any, error) {
	s, err := textValue(v)
	if s == nil || err != nil {
		return s, err
	}
	text := s.(string)
	date, clock, ok := strings.Cut(text, " ")
	if !ok || len(date) != 10 {
		return nil, database.UnsupportedValue()
	}
	if date[5:7] == "00" || date[8:10] == "00" {
		return text, nil
	}
	return date + "T" + clock + zone, nil
}

func jsonValue(v driver.Value) (any, error) {
	if v == nil {
		return nil, nil
	}
	b, ok := raw(v)
	if !ok {
		return nil, database.UnsupportedValue()
	}
	return database.JSONValue(b)
}

func binaryValue(v driver.Value) (any, error) {
	if v == nil {
		return nil, nil
	}
	b, ok := raw(v)
	if !ok {
		return nil, database.UnsupportedValue()
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// bindParameters maps shared parameters to binary-protocol values. Integers
// that fit 64 bits and booleans bind as integers, which MySQL compares
// numerically; other numbers bind as exact decimal text. JSON values bind as
// text, so CAST(? AS JSON) selects JSON semantics.
func bindParameters(params []database.Parameter) []any {
	out := make([]any, len(params))
	for i, p := range params {
		switch p.Kind {
		case database.NullParameter:
			out[i] = nil
		case database.BoolParameter:
			out[i] = int64(0)
			if p.Text == "true" {
				out[i] = int64(1)
			}
		case database.NumberParameter:
			out[i] = p.Text
			if integerText.MatchString(p.Text) {
				if n, err := strconv.ParseInt(p.Text, 10, 64); err == nil {
					out[i] = n
				}
			}
		default:
			out[i] = p.Text
		}
	}
	return out
}
