package mysql

import (
	"bytes"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strconv"
	"testing"

	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
)

type codecFixture struct {
	Name     string          `json:"name"`
	Flavors  []string        `json:"flavors"`
	Column   string          `json:"column"`
	Literal  string          `json:"literal"`
	Library  string          `json:"library"`
	Wire     map[string]any  `json:"wire"`
	Type     string          `json:"type"`
	JSON     json.RawMessage `json:"json"`
	Encoding string          `json:"encoding"`
}

func codecFixtures(t *testing.T) []codecFixture {
	t.Helper()
	b, err := os.ReadFile("testdata/codecs.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Version int            `json:"version"`
		Cases   []codecFixture `json:"cases"`
	}
	if err = json.Unmarshal(b, &doc); err != nil || doc.Version != 1 || len(doc.Cases) == 0 {
		t.Fatal("invalid codec fixtures", err)
	}
	return doc.Cases
}

func (f codecFixture) appliesTo(flavor Flavor) bool {
	return len(f.Flavors) == 0 || slices.Contains(f.Flavors, flavor.Name())
}

// wireValue rebuilds the client library's binary-protocol value.
func (f codecFixture) wireValue(t *testing.T) driver.Value {
	t.Helper()
	for kind, raw := range f.Wire {
		s, _ := raw.(string)
		switch kind {
		case "null":
			return nil
		case "int64":
			n, err := strconv.ParseInt(s, 10, 64)
			if err != nil {
				t.Fatal(f.Name, err)
			}
			return n
		case "float32":
			n, err := strconv.ParseFloat(s, 32)
			if err != nil {
				t.Fatal(f.Name, err)
			}
			return float32(n)
		case "float64":
			n, err := strconv.ParseFloat(s, 64)
			if err != nil {
				t.Fatal(f.Name, err)
			}
			return n
		case "bytes":
			return []byte(s)
		case "hex":
			b, err := hex.DecodeString(s)
			if err != nil {
				t.Fatal(f.Name, err)
			}
			return b
		}
	}
	t.Fatalf("%s: unknown wire value", f.Name)
	return nil
}

func compactJSON(t *testing.T, b []byte) string {
	t.Helper()
	var out bytes.Buffer
	if err := json.Compact(&out, b); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// The fixture corpus defines exact representations; integration replays it on
// real servers. Rejections cover values the codec must never guess at.
func TestQueryCodecs(t *testing.T) {
	for _, f := range codecFixtures(t) {
		c := codecFor(f.Library)
		value, err := c.decode(f.wireValue(t))
		if err != nil {
			t.Fatalf("%s: %v", f.Name, err)
		}
		encoded, _ := json.Marshal(value)
		if compactJSON(t, encoded) != compactJSON(t, f.JSON) || c.name != f.Type || c.encoding != f.Encoding {
			t.Fatalf("%s: %s %s/%s", f.Name, encoded, c.name, c.encoding)
		}
	}
	for _, bad := range []struct {
		library string
		value   driver.Value
	}{
		{"VARCHAR", []byte{0xff}}, {"DECIMAL", []byte("1e5")}, {"BIGINT", []byte("12x")}, {"BIT", make([]byte, 9)},
		{"INT", []byte("x")}, {"DATETIME", []byte("2024-01-01")}, {"JSON", []byte(`{"x":`)}, {"DOUBLE", []byte("1")},
	} {
		if _, err := codecFor(bad.library).decode(bad.value); err == nil {
			t.Fatalf("%s accepted %q", bad.library, bad.value)
		}
	}
	deep := bytes.Repeat([]byte("["), 65)
	if _, err := codecFor("JSON").decode(deep); !isCode(err, contracts.ResourceLimit) {
		t.Fatal("deep JSON", err)
	}
	// Parameters keep exact numbers: integers bind as integers, other numbers as text.
	params, err := database.Parameters([]json.RawMessage{json.RawMessage(`9007199254740993`), json.RawMessage(`1.10`), json.RawMessage(`1e400`), json.RawMessage(`true`), json.RawMessage(`null`), json.RawMessage(`"text"`), json.RawMessage(`{"n": 1}`)})
	if err != nil {
		t.Fatal(err)
	}
	want := []any{int64(9007199254740993), "1.10", "1e400", int64(1), nil, "text", `{"n":1}`}
	if got := bindParameters(params); !slices.Equal(got, want) {
		t.Fatalf("parameter binding: %#v", got)
	}
}

func isCode(err error, code contracts.Code) bool {
	var e *database.Error
	return errors.As(err, &e) && e.Code == code
}
