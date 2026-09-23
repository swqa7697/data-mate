package postgres

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
	"github.com/swqa7697/data-mate/internal/database/postgres/sqlpolicy"
)

var decimalText = regexp.MustCompile(`^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$`)

// Match the public ISO representations, excluding relative dates, implicit
// timezones and server-specific input shortcuts. PostgreSQL validates calendar
// and type-range constraints using the explicitly audited input function.
const isoDate = `[0-9]{4,6}-[0-9]{2}-[0-9]{2}`
const isoTime = `[0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]{1,6})?`

var parameterPatterns = map[sqlpolicy.Type]*regexp.Regexp{
	1082: regexp.MustCompile(`^(?:` + isoDate + `(?: BC)?|-?infinity)$`),
	1083: regexp.MustCompile(`^` + isoTime + `$`),
	1114: regexp.MustCompile(`^(?:` + isoDate + `T` + isoTime + `(?: BC)?|-?infinity)$`),
	1184: regexp.MustCompile(`^(?:` + isoDate + `T` + isoTime + `(?:Z|[+-][0-9]{2}:[0-9]{2})(?: BC)?|-?infinity)$`),
	2950: regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`),
}

func codecError() error {
	return database.Fail(contracts.QueryUnsupported, "result value has an unsupported representation", false)
}

// JSON is retained as bounded RawMessage: exact numbers and duplicate object
// members survive, without allocating a potentially huge Go object graph.
func jsonValue(b []byte) (json.RawMessage, error) {
	if len(b) > 1<<20 {
		return nil, database.Fail(contracts.ResourceLimit, "JSON value exceeds size limit", false)
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
				return nil, database.Fail(contracts.ResourceLimit, "JSON value exceeds nesting limit", false)
			}
		case ']', '}':
			depth--
		}
	}
	if !utf8.Valid(b) || !json.Valid(b) {
		return nil, codecError()
	}
	return append(json.RawMessage(nil), b...), nil
}

func specialNumber(s string) bool { return s == "NaN" || s == "Infinity" || s == "-Infinity" }

func decodeValue(typ sqlpolicy.Type, b []byte) (any, error) {
	if typ.Name() == "" {
		return nil, codecError()
	}
	if b == nil {
		return nil, nil
	}
	if !utf8.Valid(b) {
		return nil, codecError()
	}
	s := string(b)
	switch typ {
	case 16:
		if s == "t" {
			return true, nil
		}
		if s == "f" {
			return false, nil
		}
	case 21, 23, 20:
		bits := 64
		if typ == 21 {
			bits = 16
		}
		if typ == 23 {
			bits = 32
		}
		n, e := strconv.ParseInt(s, 10, bits)
		if e == nil {
			if typ == 20 {
				return s, nil
			}
			return n, nil
		}
	case 1700:
		if specialNumber(s) || decimalText.MatchString(s) {
			return s, nil
		}
	case 700, 701:
		if specialNumber(s) {
			return s, nil
		}
		bits := 64
		if typ == 700 {
			bits = 32
		}
		n, e := strconv.ParseFloat(s, bits)
		if e == nil && !math.IsNaN(n) && !math.IsInf(n, 0) {
			if typ == 700 {
				return float32(n), nil
			}
			return n, nil
		}
	case 17:
		if strings.HasPrefix(s, `\x`) {
			decoded, e := hex.DecodeString(s[2:])
			if e == nil {
				return base64.StdEncoding.EncodeToString(decoded), nil
			}
		}
	case 114, 3802:
		return jsonValue(b)
	case 1114, 1184:
		if s == "infinity" || s == "-infinity" {
			return s, nil
		}
		// PostgreSQL's ISO output is fixed by explicit startup settings. Large years
		// and BC retain their server spelling instead of overflowing time.Time.
		date, clock, ok := strings.Cut(s, " ")
		if !ok {
			break
		}
		era := ""
		if strings.HasSuffix(clock, " BC") {
			clock = strings.TrimSuffix(clock, " BC")
			era = " BC"
		}
		if typ == 1184 {
			if !strings.HasSuffix(clock, "+00") {
				return nil, codecError()
			}
			clock = strings.TrimSuffix(clock, "+00") + "Z"
		}
		return date + "T" + clock + era, nil
	case 25, 1042, 1043, 2950, 1082, 1083:
		return s, nil
	}
	return nil, codecError()
}

func invalidParameters() error {
	return database.Fail(contracts.InvalidArgument, "invalid query parameter type or value", false)
}

func queryParameters(input []database.QueryParameter) ([]sqlpolicy.Parameter, error) {
	if len(input) > 256 {
		return nil, invalidParameters()
	}
	out := make([]sqlpolicy.Parameter, 0, len(input))
	total := 0
	for _, p := range input {
		total += len(p.Value)
		if len(p.Value) > 64<<10 || total > 256<<10 {
			return nil, invalidParameters()
		}
		typ := sqlpolicy.NamedType(p.Type)
		if typ == 0 || len(p.Value) == 0 {
			return nil, invalidParameters()
		}
		raw, err := jsonValue(p.Value)
		if err != nil {
			return nil, invalidParameters()
		}
		raw = bytes.TrimSpace(raw)
		if bytes.Equal(raw, []byte("null")) {
			out = append(out, sqlpolicy.Parameter{Type: typ})
			continue
		}
		var s string
		switch typ {
		case 114, 3802:
			s = string(raw)
		case 16:
			var b bool
			if json.Unmarshal(raw, &b) != nil {
				return nil, invalidParameters()
			}
			s = strconv.FormatBool(b)
		case 21, 23:
			var n int64
			if json.Unmarshal(raw, &n) != nil || typ == 21 && (n < -32768 || n > 32767) || typ == 23 && (n < -2147483648 || n > 2147483647) {
				return nil, invalidParameters()
			}
			s = strconv.FormatInt(n, 10)
		case 700, 701:
			if raw[0] == '"' {
				if json.Unmarshal(raw, &s) != nil || !specialNumber(s) {
					return nil, invalidParameters()
				}
			} else {
				s = string(raw)
				if _, err = decodeValue(typ, raw); err != nil {
					return nil, invalidParameters()
				}
			}
		default:
			if json.Unmarshal(raw, &s) != nil || strings.ContainsRune(s, 0) {
				return nil, invalidParameters()
			}
			if pattern := parameterPatterns[typ]; pattern != nil && !pattern.MatchString(s) {
				return nil, invalidParameters()
			}
			if typ == 17 {
				b, e := base64.StdEncoding.Strict().DecodeString(s)
				if e != nil {
					return nil, invalidParameters()
				}
				s = `\x` + hex.EncodeToString(b)
			} else if typ == 20 || typ == 1700 {
				if _, err = decodeValue(typ, []byte(s)); err != nil {
					return nil, invalidParameters()
				}
			}
		}
		out = append(out, sqlpolicy.Parameter{Type: typ, Value: &s})
	}
	return out, nil
}
