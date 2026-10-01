package postgres

import (
	"encoding/base64"
	"encoding/hex"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/swqa7697/data-mate/internal/database"
)

var decimalText = regexp.MustCompile(`^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$`)

func specialNumber(s string) bool { return s == "NaN" || s == "Infinity" || s == "-Infinity" }

func decodeValue(typ uint32, b []byte) (any, error) {
	if b == nil {
		return nil, nil
	}
	if !utf8.Valid(b) {
		return nil, database.UnsupportedValue()
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
		return database.JSONValue(b)
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
				return nil, database.UnsupportedValue()
			}
			clock = strings.TrimSuffix(clock, "+00") + "Z"
		}
		return date + "T" + clock + era, nil
	case 25, 1042, 1043, 2950, 1082, 1083:
		return s, nil
	}
	return nil, database.UnsupportedValue()
}

// textParameters encodes shared parameters in PostgreSQL text format. Number
// text keeps its original spelling and PostgreSQL infers every type.
func textParameters(params []database.Parameter) [][]byte {
	out := make([][]byte, len(params))
	for i, p := range params {
		if p.Kind != database.NullParameter {
			out[i] = []byte(p.Text)
		}
	}
	return out
}
