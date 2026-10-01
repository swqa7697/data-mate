package mysql

import (
	"context"
	"crypto/tls"
	"database/sql/driver"
	"net"
	"strconv"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/go-sql-driver/mysql"
	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
	"github.com/swqa7697/data-mate/internal/transport"
)

// dialState records one physical connection's progress for diagnostics.
type dialState struct {
	wire     *wire
	tls      bool
	verified atomic.Bool
}

// config builds an explicit client configuration for one physical connection.
// The client library reads no environment, option files or ambient credentials;
// every setting that could change behavior is spelled out here.
func (d *Driver) config(a database.Access) (*mysql.Config, *dialState, error) {
	p := a.Profile
	tlsConfig, err := transport.TLSConfig(p.Connection.Host, p.Transport.TLS)
	if err != nil {
		return nil, nil, database.Fail(contracts.ConnectFailed, "cannot load verified TLS configuration", false)
	}
	secrets, hosts := a.TransportCredentials()
	dial, err := transport.Dialer(p.Transport, secrets, hosts)
	if err != nil {
		return nil, nil, d.connectError(err)
	}
	state := &dialState{tls: tlsConfig != nil}
	cfg := mysql.NewConfig()
	// The default logger writes upstream error text to stderr.
	cfg.Logger = &mysql.NopLogger{}
	cfg.User = p.Connection.Username
	cfg.Passwd = a.Password()
	cfg.Net = "tcp"
	cfg.Addr = net.JoinHostPort(p.Connection.Host, strconv.Itoa(p.Connection.Port))
	cfg.Collation = "utf8mb4_general_ci"
	cfg.Loc = time.UTC
	cfg.ConnectionAttributes = "program_name:data-mate"
	// A fixed value bounds parameter packets and skips a startup query.
	cfg.MaxAllowedPacket = 4 << 20
	cfg.Timeout = 10 * time.Second
	cfg.AllowAllFiles = false
	cfg.AllowCleartextPasswords = false
	cfg.AllowFallbackToPlaintext = false
	cfg.AllowOldPasswords = false
	cfg.InterpolateParams = false
	cfg.MultiStatements = false
	cfg.ParseTime = false
	cfg.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
		c, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		state.wire = newWire(c)
		return state.wire, nil
	}
	if tlsConfig != nil {
		t := tlsConfig.Clone()
		// Called only after the certificate chain and hostname verify.
		t.VerifyConnection = func(tls.ConnectionState) error { state.verified.Store(true); return nil }
		cfg.TLS = t
	}
	return cfg, state, nil
}

// connect opens one physical connection without auditing it. A failure after
// the server greeting (and any TLS verification) belongs to authentication.
func (d *Driver) connect(ctx context.Context, a database.Access, trace *database.Trace) (*session, error) {
	cfg, state, err := d.config(a)
	if err != nil {
		return nil, err
	}
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		return nil, database.Fail(contracts.ConfigInvalid, "invalid "+d.flavor.display()+" configuration", false)
	}
	conn, err := func() (c driver.Conn, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = errMalformed
			}
		}()
		return connector.Connect(ctx)
	}()
	if err != nil {
		if w := state.wire; w != nil {
			if w.greeted.Load() && (!state.tls || state.verified.Load()) {
				trace.Pass("dial")
				trace.Start("authentication")
			}
			exceeded := w.exceeded.Load()
			_ = w.Close()
			if exceeded {
				return nil, d.budgetError()
			}
		}
		return nil, d.connectError(err)
	}
	return &session{conn: conn, wire: state.wire}, nil
}

// checkProfile applies flavor rules beyond the shared profile schema.
func (d *Driver) checkProfile(p config.Profile) error {
	if p.Driver != d.flavor.Name() {
		return database.Fail(contracts.ConfigInvalid, "profile driver does not match", false)
	}
	if p.Connection.Database != "" {
		return database.Fail(contracts.ConfigInvalid, d.flavor.Name()+" profiles have no database", false)
	}
	limit := 32
	if d.flavor == MariaDB {
		limit = 80
	}
	if utf8.RuneCountInString(p.Connection.Username) > limit {
		return database.Fail(contracts.ConfigInvalid, "username exceeds "+strconv.Itoa(limit)+" characters", false)
	}
	return nil
}

// ValidateProfile checks driver-specific nonsecret settings before vault access.
func (d *Driver) ValidateProfile(p config.Profile) error {
	a, _, err := database.Normalize(database.NewAccess(p, ""))
	if err != nil {
		return err
	}
	return d.checkProfile(a.Profile)
}

// Validate checks the profile and explicit transport without dialing.
func (d *Driver) Validate(a database.Access) error {
	a, _, err := database.Normalize(a)
	if err != nil {
		return err
	}
	if err = d.checkProfile(a.Profile); err != nil {
		return err
	}
	_, _, err = d.config(a)
	return err
}
