package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestProfiles(t *testing.T) {
	original := profileFixture(t)
	p, rev, err := DecodeProfiles(bytes.NewReader(original))
	if err != nil {
		t.Fatal(err)
	}
	if len(rev) != 64 || *p.Connections[0].Limits != DefaultLimits() {
		t.Fatal("revision/default limits")
	}
	c := p.Connections[0]
	if !c.Scope.ContainsSchema("Any") {
		t.Fatal("all")
	}
	c.Scope = Scope{Mode: "whitelist"}
	if c.Scope.ContainsSchema("public") {
		t.Fatal("empty scope broadened")
	}
	c.Scope = Scope{Mode: "whitelist", Schemas: []string{"Mixed.Case", "public"}}
	if !c.Scope.ContainsSchema("Mixed.Case") || c.Scope.ContainsSchema("mixed.case") || !c.Scope.ContainsSchema("public") {
		t.Fatal("exact scope")
	}
	c.Scope = Scope{Mode: "blacklist", Schemas: []string{"Mixed.Case", "public"}}
	if c.Scope.ContainsSchema("Mixed.Case") || c.Scope.ContainsSchema("public") || !c.Scope.ContainsSchema("mixed.case") || !c.Scope.ContainsSchema("future") {
		t.Fatal("blacklist did not exclude exact schemas")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, original); err != nil {
		t.Fatal(err)
	}
	_, rev2, err := DecodeProfiles(&compact)
	if err != nil || rev != rev2 {
		t.Fatal("format changes revision")
	}
	tests := map[string]func(map[string]any){
		"unknown root":    func(v map[string]any) { v["extra"] = true },
		"unknown version": func(v map[string]any) { v["version"] = 2 },
		"null list":       func(v map[string]any) { v["connections"] = nil },
		"missing list":    func(v map[string]any) { delete(v, "connections") },
		"duplicate alias": func(v map[string]any) {
			// A distinct ID/reference ensures this fails only for the alias collision.
			first := v["connections"].([]any)[0].(map[string]any)
			other := make(map[string]any, len(first))
			for key, value := range first {
				other[key] = value
			}
			other["id"] = "AAAAAAAA-0000-0000-0000-000000000000"
			delete(other, "credential_ref")
			v["connections"] = append(v["connections"].([]any), other)
		},
		"too many": func(v map[string]any) {
			// Valid unique profiles isolate the count limit from item/identity failures.
			first := v["connections"].([]any)[0].(map[string]any)
			connections := make([]any, 129)
			for i := range connections {
				c := make(map[string]any, len(first))
				for key, value := range first {
					c[key] = value
				}
				c["id"] = fmt.Sprintf("%08x-0000-0000-0000-000000000000", i)
				c["alias"] = fmt.Sprintf("profile-%d", i)
				delete(c, "credential_ref")
				connections[i] = c
			}
			v["connections"] = connections
		},
	}
	changes := map[string]func(map[string]any){
		"alias uppercase":     func(c map[string]any) { c["alias"] = "Analytics" },
		"alias leading digit": func(c map[string]any) { c["alias"] = "1bad" },
		"invalid uuid":        func(c map[string]any) { c["id"] = "no" },
		"unknown driver":      func(c map[string]any) { c["driver"] = "mysql" },
		"port zero":           func(c map[string]any) { c["connection"].(map[string]any)["port"] = 0 },
		"port high":           func(c map[string]any) { c["connection"].(map[string]any)["port"] = 65536 },
		"embedded secret":     func(c map[string]any) { c["connection"].(map[string]any)["password"] = "secret-sentinel" },
		"null scope":          func(c map[string]any) { c["scope"] = nil },
		"absent scope":        func(c map[string]any) { delete(c, "scope") },
		"legacy mode":         func(c map[string]any) { c["scope"] = map[string]any{"mode": "all", "schemas": []string{"public"}} },
		"duplicate schema": func(c map[string]any) {
			c["scope"] = map[string]any{"mode": "whitelist", "schemas": []string{"x", "x"}}
		},
		"obsolete table scope": func(c map[string]any) {
			c["scope"] = map[string]any{"mode": "whitelist", "tables": []any{map[string]any{"schema": "x", "name": "y"}, map[string]any{"schema": "x", "name": "y"}}}
		},
		"tls ca disabled": func(c map[string]any) {
			c["transport"].(map[string]any)["tls"] = map[string]any{"mode": "disabled", "ca_file": "/tmp/test.pem"}
		},
		"relative ca": func(c map[string]any) {
			c["transport"].(map[string]any)["tls"] = map[string]any{"mode": "verify-full", "ca_file": "test.pem"}
		},
		"two transports": func(c map[string]any) {
			tr := c["transport"].(map[string]any)
			tr["ssh"] = map[string]any{"host": "host", "port": 22, "user": "u", "auth": "key"}
			tr["proxy"] = map[string]any{"kind": "socks5", "host": "h", "port": 1080}
		},
		"timeout high": func(c map[string]any) {
			c["limits"] = map[string]any{"query_timeout_ms": 300001, "max_rows": 500, "max_result_bytes": 1048576}
		},
	}
	for name, change := range changes {
		tests[name] = func(v map[string]any) { change(v["connections"].([]any)[0].(map[string]any)) }
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			var v map[string]any
			if err := json.Unmarshal(original, &v); err != nil {
				t.Fatal(err)
			}
			change(v)
			b, _ := json.Marshal(v)
			_, _, err := DecodeProfiles(bytes.NewReader(b))
			if err == nil {
				t.Fatal("accepted invalid profile")
			}
			if strings.Contains(err.Error(), "secret-sentinel") {
				t.Fatal("secret in error")
			}
		})
	}
	for _, b := range []string{string(original) + "{}", strings.Replace(string(original), `"version": 1`, `"version": 1, "version": 1`, 1), strings.Repeat(" ", MaxProfileBytes+1)} {
		if _, _, err := DecodeProfiles(strings.NewReader(b)); err == nil {
			t.Fatal("accepted malformed/oversized JSON")
		}
	}
}

func TestRevisionNormalizationAndUniqueReferences(t *testing.T) {
	var p Profiles
	if err := json.Unmarshal(profileFixture(t), &p); err != nil {
		t.Fatal(err)
	}
	p.Connections[0].Scope = Scope{Mode: "whitelist", Schemas: []string{"z", "a"}}
	encode := func() (Revision, error) {
		b, _ := json.Marshal(p)
		_, r, e := DecodeProfiles(bytes.NewReader(b))
		return r, e
	}
	first, err := encode()
	if err != nil {
		t.Fatal(err)
	}
	p.Connections[0].Scope.Schemas = []string{"a", "z"}
	defaults := DefaultLimits()
	p.Connections[0].Limits = &defaults
	second, err := encode()
	if err != nil || first != second {
		t.Fatal("normalization changed revision")
	}
	p.Connections[0].Scope = Scope{Mode: "whitelist"}
	third, err := encode()
	if err != nil {
		t.Fatal(err)
	}
	if third == first {
		t.Fatal("scope change did not change revision")
	}
	c := p.Connections[0]
	c.ID = "AAAAAAAA-0000-0000-0000-000000000000"
	c.Alias = "other"
	p.Connections = append(p.Connections, c)
	p.Connections[0].CredentialRef = "BBBBBBBB-0000-0000-0000-000000000000"
	p.Connections[1].CredentialRef = strings.ToLower(p.Connections[0].CredentialRef)
	if _, err := encode(); err == nil {
		t.Fatal("duplicate reference")
	}
	p.Connections[1].CredentialRef = ""
	p.Connections[0].ID = strings.ToLower(c.ID)
	if _, err := encode(); err == nil {
		t.Fatal("case variant duplicate UUID")
	}
}

func TestPartialLimits(t *testing.T) {
	var v map[string]any
	if err := json.Unmarshal(profileFixture(t), &v); err != nil {
		t.Fatal(err)
	}
	c := v["connections"].([]any)[0].(map[string]any)
	c["limits"] = map[string]any{"max_rows": 12}
	b, _ := json.Marshal(v)
	p, _, err := DecodeProfiles(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if *p.Connections[0].Limits != (Limits{60000, 12, 1048576}) {
		t.Fatal("partial defaults")
	}
	// Expanded timeout bounds must survive decoding, including explicit short limits.
	for _, tc := range []struct {
		timeout int
		valid   bool
	}{{1, true}, {60000, true}, {300000, true}, {0, false}} {
		c["limits"] = map[string]any{"query_timeout_ms": tc.timeout}
		b, _ = json.Marshal(v)
		got, _, err := DecodeProfiles(bytes.NewReader(b))
		if (err == nil) != tc.valid {
			t.Fatalf("timeout %d: %v", tc.timeout, err)
		}
		if tc.valid && got.Connections[0].Limits.QueryTimeoutMS != tc.timeout {
			t.Fatalf("explicit timeout %d changed", tc.timeout)
		}
	}
	c["limits"] = map[string]any{"max_rows": 0}
	b, _ = json.Marshal(v)
	if _, _, err := DecodeProfiles(bytes.NewReader(b)); err == nil {
		t.Fatal("zero limit accepted")
	}
}
