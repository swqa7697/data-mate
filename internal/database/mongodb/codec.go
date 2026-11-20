package mongodb

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/swqa7697/data-mate/internal/database"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// encodeDocument returns a complete document as canonical Extended JSON:
// lossless typed numbers, dates and binary values, with key order and
// duplicate keys preserved. Values without a safe text form, such as invalid
// UTF-8, are unsupported rather than replaced. The result is already the
// compact, HTML-escaped form the MCP envelope carries, so it is budgeted and
// stored without being encoded again.
func encodeDocument(doc bson.Raw) (out json.RawMessage, err error) {
	defer func() {
		if recover() != nil {
			out, err = nil, database.UnsupportedValue()
		}
	}()
	if validateDocument(doc, 0, resultText) != nil {
		return nil, database.UnsupportedValue()
	}
	b, err := bson.MarshalExtJSON(doc, true, false)
	if err != nil || !utf8.Valid(b) {
		return nil, database.UnsupportedValue()
	}
	// MarshalExtJSON never returns nil on success, so this cannot become null;
	// encoding/json rejects text that is not one valid JSON value.
	if out, err = json.Marshal(json.RawMessage(b)); err != nil {
		return nil, database.UnsupportedValue()
	}
	return out, nil
}

// validName accepts a database or collection name in a cursor position.
func validName(s string) bool {
	return s != "" && len(s) <= 255 && database.ValidResultName(s)
}

// validCollectionName accepts a collection MongoDB could store in db: 1 to 255
// bytes of UTF-8 without NUL or $, within the 255-byte namespace limit.
func validCollectionName(db, collection string) bool {
	return collection != "" && len(db)+1+len(collection) <= 255 && utf8.ValidString(collection) && !strings.ContainsAny(collection, "$\x00")
}

// checkResultName accepts a server-provided name that can be represented as
// JSON text. An invalid name fails the whole result; nothing is replaced.
func checkResultName(s string) error {
	if !database.ValidResultName(s) {
		return database.UnsupportedValue()
	}
	return nil
}
