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
)

var decimalText = regexp.MustCompile(`^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$`)

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

func decodeValue(typ uint32, b []byte) (any, error) {
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

// queryParameters preserves JSON number text and leaves type inference to PostgreSQL.
func queryParameters(input []json.RawMessage) ([][]byte, error) {
	if len(input) > 256 {
		return nil, invalidParameters()
	}
	out := make([][]byte, len(input))
	total := 0
	for i, p := range input {
		total += len(p)
		if len(p) > 64<<10 || total > 256<<10 {
			return nil, invalidParameters()
		}
		raw, err := jsonValue(p)
		if err != nil {
			return nil, invalidParameters()
		}
		raw = bytes.TrimSpace(raw)
		if bytes.Equal(raw, []byte("null")) {
			continue
		}
		if raw[0] == '"' {
			var value string
			if json.Unmarshal(raw, &value) != nil || strings.ContainsRune(value, 0) {
				return nil, invalidParameters()
			}
			out[i] = []byte(value)
		} else {
			var b bytes.Buffer
			if json.Compact(&b, raw) != nil {
				return nil, invalidParameters()
			}
			out[i] = b.Bytes()
		}
	}
	return out, nil
}
