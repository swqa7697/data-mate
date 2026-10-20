// Package config validates nonsecret profiles and resolves installation paths.
package config

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/swqa7697/data-mate/internal/contracts"
)

const MaxProfileBytes = 1 << 20

const (
	// DefaultQueryTimeout leaves execution deadlines to the caller and database.
	DefaultQueryTimeout time.Duration = 0
	// MaxQueryTimeout is PostgreSQL's maximum statement_timeout in milliseconds.
	MaxQueryTimeout = time.Duration(1<<31-1) * time.Millisecond
)

// Profiles is the versioned connections file.
type Profiles struct {
	Version     int       `json:"version"`
	Connections []Profile `json:"connections"`
}

// Profile contains only nonsecret connection settings.
type Profile struct {
	ID            string     `json:"id"`
	Alias         string     `json:"alias"`
	Driver        string     `json:"driver"`
	Connection    Connection `json:"connection"`
	CredentialRef string     `json:"credential_ref,omitempty"`
	Transport     Transport  `json:"transport"`
	Limits        *Limits    `json:"limits,omitempty"`
}

// Connection identifies a single PostgreSQL endpoint.
type Connection struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Database string `json:"database"`
	Username string `json:"username"`
}

// Transport selects explicit, optional transport protection.
type Transport struct {
	TLS   TLS    `json:"tls"`
	SSH   *SSH   `json:"ssh,omitempty"`
	Proxy *Proxy `json:"proxy,omitempty"`
}

// TLS is either disabled or hostname-verified TLS.
type TLS struct {
	Mode   string `json:"mode"`
	CAFile string `json:"ca_file,omitempty"`
}

// SSH describes one jump host; authentication material lives in the vault.
type SSH struct {
	Host string `json:"host"`
	Port int    `json:"port"`
	User string `json:"user"`
	Auth string `json:"auth"`
}

// Proxy describes a SOCKS5 endpoint without credentials.
type Proxy struct {
	Kind     string `json:"kind"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username,omitempty"`
}

// Limits holds optional execution budgets. Zero timeout/bytes means unlimited.
type Limits struct {
	QueryTimeoutMS int `json:"query_timeout_ms"`
	MaxRows        int `json:"max_rows"`
	MaxResultBytes int `json:"max_result_bytes"`
}

// MarshalJSON marks current settings so historical limits are reset only once.
func (l Limits) MarshalJSON() ([]byte, error) {
	type plain Limits
	return json.Marshal(struct {
		Version int `json:"version"`
		plain
	}{2, plain(l)})
}

// UnmarshalJSON resets historical, unversioned limits, including custom values.
// Passive snapshots apply this migration without writing files or using the vault.
func (l *Limits) UnmarshalJSON(b []byte) error {
	type plain Limits
	v := struct {
		Version json.RawMessage `json:"version"`
		plain
	}{plain: plain(DefaultLimits())}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&v); err != nil {
		return err
	}
	if v.QueryTimeoutMS < 0 || int64(v.QueryTimeoutMS) > int64(MaxQueryTimeout/time.Millisecond) || v.MaxRows < 1 || v.MaxResultBytes < 0 {
		return errors.New("invalid execution limits")
	}
	if len(v.Version) == 0 {
		*l = DefaultLimits()
		return nil
	}
	var version int
	if json.Unmarshal(v.Version, &version) != nil || version != 2 {
		return errors.New("unsupported execution limits version")
	}
	*l = Limits(v.plain)
	return nil
}

// DefaultLimits returns independent default settings.
func DefaultLimits() Limits { return Limits{0, 50000, 0} }

// QueryContext applies a positive optional deadline and always returns a cancel.
func QueryContext(parent context.Context, milliseconds int) (context.Context, context.CancelFunc) {
	if milliseconds == 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, time.Duration(milliseconds)*time.Millisecond)
}

// Table is an exact, case-sensitive name, not a glob.
type Table struct {
	Schema string `json:"schema"`
	Name   string `json:"name"`
}

// Revision is the SHA-256 of validated normalized profile data.
type Revision string

// DecodeProfiles enforces the strict version-1 schema and cross-profile uniqueness.
func DecodeProfiles(r io.Reader) (Profiles, Revision, error) {
	var p Profiles
	b, err := contracts.JSON(r, MaxProfileBytes)
	if err != nil {
		return p, "", err
	}
	if err := contracts.Validate("profiles", b); err != nil {
		return p, "", err
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return Profiles{}, "", errors.New("invalid profile data")
	}
	ids, aliases, refs := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for i := range p.Connections {
		c := &p.Connections[i]
		c.ID = strings.ToLower(c.ID)
		c.CredentialRef = strings.ToLower(c.CredentialRef)
		if ids[c.ID] || aliases[c.Alias] || (c.CredentialRef != "" && refs[c.CredentialRef]) {
			return Profiles{}, "", errors.New("duplicate profile identity, alias, or credential reference")
		}
		ids[c.ID], aliases[c.Alias] = true, true
		if c.CredentialRef != "" {
			refs[c.CredentialRef] = true
		}
		if c.Transport.TLS.CAFile != "" && !filepath.IsAbs(c.Transport.TLS.CAFile) {
			return Profiles{}, "", errors.New("TLS CA path must be absolute")
		}
		if c.Limits == nil {
			v := DefaultLimits()
			c.Limits = &v
		}
	}
	slices.SortFunc(p.Connections, func(a, b Profile) int { return strings.Compare(a.ID, b.ID) })
	canonical, err := json.Marshal(p)
	if err != nil {
		return Profiles{}, "", errors.New("cannot encode profiles")
	}
	hash := sha256.Sum256(canonical)
	return p, Revision(hex.EncodeToString(hash[:])), nil
}
