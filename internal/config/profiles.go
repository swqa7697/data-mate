// Package config validates nonsecret profiles and resolves installation paths.
package config

import (
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
	// DefaultQueryTimeout budgets setup, pool waiting, execution and result reading.
	DefaultQueryTimeout = 60 * time.Second
	// MaxQueryTimeout is the largest timeout accepted for a profile.
	MaxQueryTimeout = 5 * time.Minute
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

// Connection identifies one database endpoint and account. PostgreSQL profiles
// name the one database they reach; MySQL and MariaDB profiles name none, since
// their accounts reach every database their grants allow.
type Connection struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Database string `json:"database,omitempty"`
	Username string `json:"username"`
}

// Drivers lists the supported database drivers in presentation order.
var Drivers = []string{"postgres", "mysql", "mariadb"}

// DefaultPort returns a driver's standard TCP port, or 0 for an unknown driver.
func DefaultPort(driver string) int {
	switch driver {
	case "postgres":
		return 5432
	case "mysql", "mariadb":
		return 3306
	}
	return 0
}

// NamesDatabase reports whether a driver's profiles name a database.
func NamesDatabase(driver string) bool { return driver == "postgres" }

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

// Limits caps work for one connection.
type Limits struct {
	QueryTimeoutMS int `json:"query_timeout_ms"`
	MaxRows        int `json:"max_rows"`
	MaxResultBytes int `json:"max_result_bytes"`
}

// UnmarshalJSON applies defaults only to omitted limits, preserving explicit zeros
// for validation to reject. The enclosing schema rejects unknown members first.
func (l *Limits) UnmarshalJSON(b []byte) error {
	type plain Limits
	defaults := plain(DefaultLimits())
	if err := json.Unmarshal(b, &defaults); err != nil {
		return err
	}
	*l = Limits(defaults)
	return nil
}

// DefaultLimits returns independent default settings.
func DefaultLimits() Limits { return Limits{int(DefaultQueryTimeout / time.Millisecond), 500, 1048576} }

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
