package postgres

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
	"github.com/swqa7697/data-mate/internal/transport"
)

// SanitizeEnvironment must run once at process startup before application
// goroutines. It changes only this process, never the invoking shell.
func SanitizeEnvironment() {
	for _, v := range os.Environ() {
		k, _, _ := strings.Cut(v, "=")
		if strings.HasPrefix(k, "PG") {
			_ = os.Unsetenv(k)
		}
	}
}

func normalized(a database.Access) (database.Access, config.Revision, error) {
	b, err := json.Marshal(config.Profiles{Version: 1, Connections: []config.Profile{a.Profile}})
	if err != nil {
		return a, "", database.Fail(contracts.ConfigInvalid, "invalid profile", false)
	}
	p, rev, err := config.DecodeProfiles(bytes.NewReader(b))
	if err != nil {
		return a, "", database.Fail(contracts.ConfigInvalid, "invalid profile", false)
	}
	a = database.NewAccess(p.Connections[0], a.Password())
	host := a.Profile.Connection.Host
	if len(host) > 253 || (net.ParseIP(host) == nil && strings.ContainsAny(host, "/\\:@, \t\r\n")) {
		return a, "", database.Fail(contracts.ConfigInvalid, "host must be one TCP hostname or IP address", false)
	}
	if len(a.Password()) > 128<<10 || strings.ContainsRune(a.Password(), 0) {
		return a, "", database.Fail(contracts.ConfigInvalid, "invalid credential", false)
	}
	if a.Profile.Transport.SSH != nil || a.Profile.Transport.Proxy != nil {
		return a, "", database.Fail(contracts.QueryUnsupported, "SSH and proxy transport are not ready", false)
	}
	return a, rev, nil
}

func connectionConfig(a database.Access) (*pgx.ConnConfig, error) {
	// A forgotten startup call fails closed, even for PG variables not currently
	// understood by pgx. Never unset process environment during concurrent requests.
	for _, v := range os.Environ() {
		if strings.HasPrefix(v, "PG") {
			return nil, database.Fail(contracts.ConfigInvalid, "PostgreSQL environment was not sanitized at startup", false)
		}
	}
	c, err := pgx.ParseConfig("host=localhost port=5432 user=data_mate database=data_mate sslmode=disable passfile=/dev/null servicefile=/dev/null connect_timeout=10")
	if err != nil {
		return nil, database.Fail(contracts.ConfigInvalid, "cannot initialize PostgreSQL configuration", false)
	}
	p := a.Profile
	c.Host = p.Connection.Host
	c.Port = uint16(p.Connection.Port)
	c.Database = p.Connection.Database
	c.User = p.Connection.Username
	c.Password = a.Password()
	c.TLSConfig, err = transport.TLSConfig(c.Host, p.Transport.TLS)
	if err != nil {
		return nil, database.Fail(contracts.ConnectFailed, "cannot load verified TLS configuration", false)
	}
	c.Fallbacks = nil
	c.DialFunc = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	c.ConnectTimeout = 10 * time.Second
	c.RuntimeParams = map[string]string{"application_name": "data-mate", "search_path": "pg_catalog", "TimeZone": "UTC", "DateStyle": "ISO, YMD", "bytea_output": "hex", "client_encoding": "UTF8", "extra_float_digits": "3", "default_transaction_read_only": "on", "statement_timeout": "10000", "lock_timeout": "1000"}
	c.MaxProtocolMessageBodyLen = 2 << 20
	trackWire(c)
	c.DefaultQueryExecMode = pgx.QueryExecModeExec
	c.StatementCacheCapacity = 0
	c.DescriptionCacheCapacity = 0
	return c, nil
}

// Validate checks the profile and explicit transport without dialing.
func (d *Driver) Validate(a database.Access) error {
	a, _, err := normalized(a)
	if err != nil {
		return err
	}
	_, err = connectionConfig(a)
	return err
}
