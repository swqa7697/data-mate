package pool

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"

	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
)

// Cursor is an authenticated keyset position. It binds the tool, profile
// revision, filters and invalidation epoch; tokens expire on restart.
type Cursor struct {
	Version    int
	Tool       string
	KindFilter string
	Kind       string
	// Key is a driver-defined tiebreak, such as a PostgreSQL object OID.
	Key      uint64
	ID       string
	Revision config.Revision
	Filter   string
	Schema   string
	Name     string
	Epoch    uint64
}

// Encode signs a cursor with this registry's process-local key.
func (r *Registry) Encode(c Cursor) string {
	b, _ := json.Marshal(c)
	mac := hmac.New(sha256.New, r.key[:])
	mac.Write(b)
	return base64.RawURLEncoding.EncodeToString(append(b, mac.Sum(nil)...))
}

// Decode authenticates a token and requires it to match want's bindings.
// validName is the driver's catalog identifier rule for the position.
func (r *Registry) Decode(s string, want Cursor, validName func(string) bool) (Cursor, error) {
	bad := func() (Cursor, error) {
		return Cursor{}, database.Fail(contracts.StaleCursor, "metadata cursor is invalid or stale", false)
	}
	if len(s) > 2048 {
		return bad()
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(b) < 33 {
		return bad()
	}
	body, sig := b[:len(b)-32], b[len(b)-32:]
	mac := hmac.New(sha256.New, r.key[:])
	mac.Write(body)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return bad()
	}
	var c Cursor
	if json.Unmarshal(body, &c) != nil || c.Version != 1 || c.Tool != want.Tool || c.KindFilter != want.KindFilter || c.ID != want.ID || c.Revision != want.Revision || c.Filter != want.Filter || c.Epoch != want.Epoch || !validName(c.Schema) || !validName(c.Name) {
		return bad()
	}
	return c, nil
}
