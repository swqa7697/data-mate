package postgres

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
)

type codecFixture struct {
	Name     string          `json:"name"`
	Type     string          `json:"type"`
	Wire     *string         `json:"wire_text"`
	JSON     json.RawMessage `json:"json"`
	Encoding string          `json:"encoding"`
}

func codecFixtures(t *testing.T) []codecFixture {
	t.Helper()
	b, e := os.ReadFile("testdata/codecs.json")
	if e != nil {
		t.Fatal(e)
	}
	var doc struct {
		Cases []codecFixture `json:"cases"`
	}
	if e = json.Unmarshal(b, &doc); e != nil {
		t.Fatal(e)
	}
	return doc.Cases
}

// Ladder step 3: earlier compiler and protocol regressions do not own codecs or
// encoded response accounting. This scenario executes the curated codec corpus,
// including precision/NULL/BC, and exercises malformed and resource boundaries.
func TestQueryCodecs(t *testing.T) {
	for _, f := range codecFixtures(t) {
		var b []byte
		if f.Wire != nil {
			b = []byte(*f.Wire)
		}
		got, e := decodeValue(fixtureType(f.Type), b)
		if e != nil {
			t.Fatalf("%s: %v", f.Name, e)
		}
		encoded, e := json.Marshal(got)
		if e != nil {
			t.Fatal(e)
		}
		// Compact via RawMessage preserves numbers, unlike unmarshalling to float64.
		want := json.RawMessage(f.JSON)
		expected, _ := json.Marshal(want)
		if string(encoded) != string(expected) {
			t.Fatalf("%s: %s != %s", f.Name, encoded, expected)
		}
		_, e = queryParameters([]json.RawMessage{f.JSON})
		if e != nil {
			t.Fatalf("parameter %s: %v", f.Name, e)
		}
	}
	for _, f := range []struct {
		typ   uint32
		value string
	}{
		{21, "32768"}, {23, "2147483648"}, {20, "9223372036854775808"}, {1700, "1/0"}, {16, "true"}, {17, `\xzz`}, {701, "1e999"}, {700, "1e99"}, {114, `{"x":`}, {25, string([]byte{255})}, {0, ""},
	} {
		if _, err := decodeValue(f.typ, []byte(f.value)); err == nil {
			t.Fatalf("invalid %d accepted", f.typ)
		}
	}
	for _, raw := range []string{strings.Repeat("[", 65) + "0" + strings.Repeat("]", 65), `"` + strings.Repeat("x", 1<<20) + `"`} {
		if _, err := decodeValue(114, []byte(raw)); err != nil {
			t.Fatal("large or nested JSON rejected", err)
		}
	}
	for _, p := range []json.RawMessage{json.RawMessage(`"\u0000"`), json.RawMessage(`{"invalid":`), nil} {
		_, err := queryParameters([]json.RawMessage{p})
		requireCode(t, err, contracts.InvalidArgument)
	}
	params, err := queryParameters([]json.RawMessage{json.RawMessage(`9007199254740993`), json.RawMessage(`true`), json.RawMessage(`null`), json.RawMessage(`"text"`), json.RawMessage(`{"n": 9007199254740993}`), json.RawMessage(`[1,null]`)})
	if err != nil || string(params[0]) != "9007199254740993" || string(params[1]) != "true" || params[2] != nil || string(params[3]) != "text" || string(params[4]) != `{"n":9007199254740993}` || string(params[5]) != `[1,null]` {
		t.Fatal("parameter fidelity", err)
	}
	// Extend the same codec boundary with a formerly oversized parameter and
	// parameter count, retaining the PostgreSQL extended protocol's uint16 count.
	large, _ := json.Marshal(strings.Repeat("x", 300<<10))
	params, err = queryParameters([]json.RawMessage{large})
	if err != nil || len(params[0]) != 300<<10 {
		t.Fatal("large parameter rejected", err)
	}
	for _, count := range []int{257, 1 << 16} {
		values := make([]json.RawMessage, count)
		for i := range values {
			values[i] = json.RawMessage(`1`)
		}
		_, err = queryParameters(values)
		if count == 257 && err != nil {
			t.Fatal("former parameter cap retained", err)
		}
		if count == 1<<16 {
			requireCode(t, err, contracts.InvalidArgument)
		}
	}
	// Type chains longer than the former depth bound decode exact values; cycles
	// are invalid representations rather than recursion or resource failures.
	types := resultTypes{20: {oid: 20, name: "int8"}}
	for oid := uint32(1000); oid < 1128; oid++ {
		types[oid] = resultType{oid: oid, base: oid + 1}
	}
	types[1128] = resultType{oid: 1128, base: 20}
	value, err := types.decode(1000, []byte("9007199254740993"))
	if err != nil || value != "9007199254740993" {
		t.Fatal("long type chain rejected", err)
	}
	types[1128] = resultType{oid: 1128, base: 1000}
	_, err = types.decode(1000, []byte("1"))
	requireCode(t, err, contracts.QueryUnsupported)

	// Count the actual compatibility envelope, including escaping and exact JSON.
	out := database.QueryResult{Connection: "fixture", Columns: []database.ResultColumn{{Name: "q\"", Type: "json"}}, Rows: [][]any{{json.RawMessage(`{"n":9007199254740993,"s":"<\n\""}`)}}, RowCount: 1}
	raw, _ := json.Marshal(out)
	envelope := struct {
		Content    []map[string]string `json:"content"`
		Structured json.RawMessage     `json:"structuredContent"`
	}{[]map[string]string{{"type": "text", "text": string(raw)}}, raw}
	full, _ := json.Marshal(envelope)
	n, e := QueryPayloadSize(out)
	if e != nil || n != len(full) {
		t.Fatalf("payload budget %d != %d: %v", n, len(full), e)
	}
}

func fixtureType(name string) uint32 {
	for oid, n := range scalarNames {
		if n == name {
			return oid
		}
	}
	return 0
}

// Wire strings allow PostgreSQL to parse each fixture's declared SQL cast.
func fixtureParameter(f codecFixture) json.RawMessage {
	if f.Wire == nil {
		return json.RawMessage(`null`)
	}
	b, _ := json.Marshal(*f.Wire)
	return b
}
