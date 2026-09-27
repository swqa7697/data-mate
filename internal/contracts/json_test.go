package contracts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStrictJSON(t *testing.T) {
	for i, input := range []string{`{"a":1,"a":2}`, `{"a":{"b":1,"b":2}}`, `[{"a":1,"\u0061":2}]`, `{} {}`, ``, strings.Repeat("[", 66) + strings.Repeat("]", 66), "\xff"} {
		if _, err := JSON(strings.NewReader(input), 1000); err == nil {
			t.Errorf("invalid JSON case %d accepted", i)
		}
	}
	for i, input := range []string{`{"a":[],"b":{}}`, `{"a":1}`, `[{"a":1},{"a":2}]`} {
		if _, err := JSON(strings.NewReader(input), 1000); err != nil {
			t.Fatalf("valid JSON case %d rejected: %v", i, err)
		}
	}
	if _, err := JSON(strings.NewReader(`{"a":1}`), 6); err == nil {
		t.Fatal("byte bound")
	}
}

// TestSchemasAndFixtures exercises the public validator, not fixture text parity.
func TestSchemasAndFixtures(t *testing.T) {
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
}

func TestToolInputsFailClosed(t *testing.T) {
	cases := map[string][]string{
		"list_connections.input": {`{"host":"secret"}`, `null`},
		"list_tables.input":      {`{}`, `{"connection":"analytics","page_size":501}`, `{"connection":"analytics","scope":{"mode":"blacklist"}}`},
		"list_objects.input":     {`{"connection":"analytics","kind":"trigger"}`, `{"connection":"analytics","page_size":501}`},
		"describe_object.input":  {`{"connection":"analytics","kind":"routine","schema":"app","name":"f"}`, `{"connection":"analytics","kind":"type","schema":"app","name":"t","identity_arguments":""}`},
		"describe_table.input":   {`{"connection":"analytics","schema":"public"}`},
		"query.input":            {`{"connection":"analytics","sql":"select 1","timeout":99}`, `{"connection":"analytics","sql":"select 1","row_limit":0}`, `{"connection":"analytics","sql":"select $1","parameters":{}}`},
	}
	for name, inputs := range cases {
		for i, input := range inputs {
			if err := Validate(name, []byte(input)); err == nil {
				t.Errorf("accepted bad %s case %d", name, i)
			}
		}
	}
}

func FuzzJSONBoundary(f *testing.F) {
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
