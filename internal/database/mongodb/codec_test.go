package mongodb

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
	"go.mongodb.org/mongo-driver/v2/bson"
)

type codecFixture struct {
	Name        string          `json:"name"`
	ExtJSON     json.RawMessage `json:"extjson"`
	BSONHex     string          `json:"bson_hex"`
	JSON        json.RawMessage `json:"json"`
	Unsupported bool            `json:"unsupported"`
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

// normalize compacts JSON text without reordering keys or merging duplicates.
func normalize(t *testing.T, name string, b []byte) string {
	t.Helper()
	out, err := json.Marshal(json.RawMessage(b))
	if err != nil {
		t.Fatalf("%s: invalid JSON: %v", name, err)
	}
	return string(out)
}

// Results are canonical Extended JSON, and BSON the writer cannot represent
// faithfully, including a binary value that would forge a sibling field, is
// refused instead of being replaced or emitted.
func TestDocumentCodecs(t *testing.T) {
	for _, c := range codecFixtures(t) {
		var doc bson.Raw
		want := c.JSON
		switch {
		case c.BSONHex != "":
			b, err := hex.DecodeString(c.BSONHex)
			if err != nil {
				t.Fatalf("%s: %v", c.Name, err)
			}
			doc = b
		case len(c.ExtJSON) > 0:
			value := append(append([]byte(`{"v":`), c.ExtJSON...), '}')
			if err := bson.UnmarshalExtJSON(value, false, &doc); err != nil {
				t.Fatalf("%s: %v", c.Name, err)
			}
			want = append(append([]byte(`{"v":`), c.JSON...), '}')
		default:
			t.Fatalf("%s: no input", c.Name)
		}
		got, err := encodeDocument(doc)
		if c.Unsupported {
			var safe *database.Error
			if !errors.As(err, &safe) || safe.Code != contracts.QueryUnsupported {
				t.Errorf("%s: encoded unsupported value as %s, error %v", c.Name, got, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.Name, err)
			continue
		}
		if g, w := normalize(t, c.Name, got), normalize(t, c.Name, want); g != w {
			t.Errorf("%s: got %s, want %s", c.Name, g, w)
		}
	}
}
