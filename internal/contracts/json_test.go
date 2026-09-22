package contracts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStrictJSON(t *testing.T) {
	for _, input := range []string{`{"a":1,"a":2}`, `{"a":{"b":1,"b":2}}`, `[{"a":1,"\u0061":2}]`, `{} {}`, ``, strings.Repeat("[", 66) + strings.Repeat("]", 66), "\xff"} {
		if _, err := JSON(strings.NewReader(input), 1000); err == nil {
			t.Errorf("accepted invalid JSON")
		}
	}
	for _, input := range []string{`{"a":[],"b":{}}`, `{"a":1}`, `[{"a":1},{"a":2}]`} {
		if _, err := JSON(strings.NewReader(input), 1000); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := JSON(strings.NewReader(`{"a":1}`), 6); err == nil {
		t.Fatal("byte bound")
	}
}

func TestSchemasAndFixtures(t *testing.T) {
	entries, err := Schemas.ReadDir("schemas")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, _ := Schemas.ReadFile("schemas/" + e.Name())
		if !json.Valid(b) {
			t.Fatal(e.Name())
		}
	}
	paths, err := filepath.Glob("testdata/*.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			name := strings.TrimSuffix(filepath.Base(path), ".json")
			if err := Validate(name, b); err != nil {
				t.Fatal(err)
			}
		})
	}
	for file, schema := range map[string]string{"codecs": "codec-fixtures", "signatures": "signature-fixtures"} {
		b, err := os.ReadFile("../database/postgres/testdata/" + file + ".json")
		if err != nil {
			t.Fatal(err)
		}
		if err := Validate(schema, b); err != nil {
			t.Fatal(err)
		}
	}
}

func TestToolInputsFailClosed(t *testing.T) {
	cases := map[string][]string{
		"list_connections.input": {`{"host":"secret"}`, `null`},
		"list_tables.input":      {`{}`, `{"connection":"analytics","page_size":501}`, `{"connection":"analytics","scope":{"mode":"all"}}`},
		"describe_table.input":   {`{"connection":"analytics","schema":"public"}`},
		"query.input":            {`{"connection":"analytics","sql":"select 1","timeout":99}`, `{"connection":"analytics","sql":"select 1","row_limit":0}`, `{"connection":"analytics","sql":"select $1","parameters":[{"type":"int8","value":9223372036854775807}]}`, `{"connection":"analytics","sql":"select $1","parameters":[{"type":"unknown","value":null}]}`, `{"connection":"analytics","sql":"select $1","parameters":[{"type":"text","value":"x","password":"y"}]}`},
	}
	for name, inputs := range cases {
		for _, input := range inputs {
			if err := Validate(name, []byte(input)); err == nil {
				t.Errorf("accepted bad %s", name)
			}
		}
	}
}

func FuzzProfilesJSONBoundary(f *testing.F) {
	for _, s := range []string{`{}`, `{"version":1,"connections":[]}`, `{"a":1,"a":2}`, `[]`} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if len(s) > 4096 {
			return
		}
		b, err := JSON(strings.NewReader(s), 4096)
		if err == nil && !json.Valid(b) {
			t.Fatal("accepted invalid JSON")
		}
	})
}
