package config

import (
	"os"
	"testing"
)

func profileFixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/profiles.json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}
