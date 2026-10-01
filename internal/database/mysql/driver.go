// Package mysql implements bounded MySQL and MariaDB connection and catalog
// operations. One package serves both servers; a Flavor selects each dialect.
package mysql

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
	"github.com/swqa7697/data-mate/internal/database/pool"
)

// Flavor selects the MySQL or MariaDB dialect of a driver.
type Flavor int

// Supported flavors; each is a separate profile driver.
const (
	MySQL Flavor = iota
	MariaDB
)

// Name is the profile driver value.
func (f Flavor) Name() string {
	if f == MariaDB {
		return "mariadb"
	}
	return "mysql"
}

func (f Flavor) display() string {
	if f == MariaDB {
		return "MariaDB"
	}
	return "MySQL"
}

// minimum is the oldest supported server, encoded as major*10000+minor*100+patch.
func (f Flavor) minimum() (int, string) {
	if f == MariaDB {
		return 101100, "MariaDB 10.11"
	}
	return 80400, "MySQL 8.4"
}

// sqlMode is the flavor's documented default mode. It never includes
// ANSI_QUOTES or NO_BACKSLASH_ESCAPES, which would change how the guard's
// lexer must read strings and identifiers.
func (f Flavor) sqlMode() string {
	if f == MariaDB {
		return "STRICT_TRANS_TABLES,ERROR_FOR_DIVISION_BY_ZERO,NO_AUTO_CREATE_USER,NO_ENGINE_SUBSTITUTION"
	}
	return "ONLY_FULL_GROUP_BY,STRICT_TRANS_TABLES,NO_ZERO_IN_DATE,NO_ZERO_DATE,ERROR_FOR_DIVISION_BY_ZERO,NO_ENGINE_SUBSTITUTION"
}

// Driver implements one flavor over the service's shared registry, which owns
// admission, pools and cursor keys. No request result is cached.
type Driver struct {
	flavor Flavor
	pools  *pool.Registry
}

// New binds a flavor to a shared registry without contacting any database.
func New(pools *pool.Registry, flavor Flavor) *Driver { return &Driver{flavor: flavor, pools: pools} }

// Only the executor can signal successful truncation after terminating a socket.
var errResultDiscarded = errors.New("bounded result completed; connection discarded")

func (d *Driver) checkout(ctx context.Context, r *pool.Registry, a database.Access, rev config.Revision, trace *database.Trace) (*session, context.Context, func(bool), error) {
	open := func(ctx context.Context) (*session, error) { return d.connect(ctx, a, trace) }
	approve := func(ctx context.Context, s *session) error { return d.validateAccount(ctx, s, a, trace) }
	return pool.Checkout(ctx, r, a, rev, open, approve)
}

// runNormalized keeps admission through cleanup and bounded payload preparation.
// It accepts only the snapshot and revision returned by Normalize in this
// request. Every operation re-pins its session and verifies it; account
// approval belongs to the pool.
func (d *Driver) runNormalized(ctx context.Context, a database.Access, rev config.Revision, fn func(context.Context, *session) error) (result error) {
	ctx, cancel := context.WithTimeout(ctx, database.Timeout(a))
	defer cancel()
	leave, err := d.pools.Admit(ctx)
	if err != nil {
		return err
	}
	defer leave()
	s, ctx, release, err := d.checkout(ctx, d.pools, a, rev, nil)
	if err != nil {
		return err
	}
	s.wire.reset()
	healthy, discarded := false, false
	defer func() { release(healthy) }()
	defer func() {
		if s.wire.exceeded.Load() {
			result = d.budgetError()
		}
	}()
	if err = d.begin(ctx, s, a, nil); err != nil {
		return d.operationError(err)
	}
	defer func() {
		if !discarded {
			healthy = d.cleanup(s)
			if !healthy && result == nil {
				result = database.Fail(contracts.ConnectFailed, d.flavor.display()+" session cleanup failed", true)
			}
		}
	}()
	err = fn(ctx, s)
	if errors.Is(err, errResultDiscarded) && s.wire.closed() {
		discarded = true
		return nil
	}
	if err != nil {
		// The library only closes its socket when a statement's context ends.
		if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			d.interrupt(a, s)
		}
		return d.operationError(err)
	}
	if ctx.Err() != nil {
		return database.CommonError(ctx.Err())
	}
	return nil
}

// begin pins session settings, starts a read-only transaction and verifies
// the settings the query guard and codecs depend on.
func (d *Driver) begin(ctx context.Context, s *session, a database.Access, trace *database.Trace) error {
	if s.info == nil {
		info, err := d.identify(ctx, s)
		if err != nil {
			return err
		}
		s.info = info
	}
	if err := d.supported(s.info, trace); err != nil {
		return err
	}
	trace.Pass("version")
	trace.Start("read_only")
	if err := s.exec(ctx, d.pins(ctx, s.info)); err != nil {
		return err
	}
	if err := s.exec(ctx, "START TRANSACTION READ ONLY"); err != nil {
		return err
	}
	return d.verify(ctx, s, a.Profile.Connection.Username)
}

// cleanup ends the transaction and releases named locks. The pinned client
// cannot send COM_RESET_CONNECTION; the guard rejects session assignments and
// every operation re-pins its settings, so these are the remaining resets.
func (d *Driver) cleanup(s *session) bool {
	if s.wire.closed() {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return s.exec(ctx, "ROLLBACK") == nil && s.exec(ctx, "DO RELEASE_ALL_LOCKS()") == nil
}

// interrupt stops server-side work after cancellation. The client library only
// closes its socket, so a short-lived connection to the same endpoint, route
// and account sends KILL QUERY. It is best effort and bounded.
func (d *Driver) interrupt(a database.Access, s *session) {
	if s.info == nil || s.info.id == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	side, err := d.connect(ctx, a, nil)
	if err != nil {
		return
	}
	defer side.Close()
	_ = side.exec(ctx, "KILL QUERY "+strconv.FormatUint(s.info.id, 10))
}

// Test always audits a fresh short-lived pool, independently of cached approval.
func (d *Driver) Test(ctx context.Context, a database.Access) (database.Readiness, error) {
	return pool.Diagnose(ctx, d.pools, a, func(ctx context.Context, fresh *pool.Registry, a database.Access, rev config.Revision, trace *database.Trace) (func(bool), error) {
		if err := d.checkProfile(a.Profile); err != nil {
			return nil, err
		}
		_, _, release, err := d.checkout(ctx, fresh, a, rev, trace)
		return release, err
	})
}

func (d *Driver) budgetError() error {
	return database.Fail(contracts.ResourceLimit, d.flavor.display()+" response exceeds receive limit", false)
}

func (d *Driver) malformed() error {
	return database.Fail(contracts.ConnectFailed, d.flavor.display()+" server sent a malformed response", true)
}

// connectError classifies connection and authentication failures.
func (d *Driver) connectError(err error) error {
	if safe := database.CommonError(err); safe != nil {
		return safe
	}
	switch {
	case errors.Is(err, errReceiveBudget):
		return d.budgetError()
	case errors.Is(err, errMalformed):
		return d.malformed()
	case errors.Is(err, mysql.ErrNoTLS):
		return database.Fail(contracts.ConnectFailed, d.flavor.display()+" server does not support TLS", false)
	case errors.Is(err, mysql.ErrCleartextPassword), errors.Is(err, mysql.ErrNativePassword), errors.Is(err, mysql.ErrOldPassword), errors.Is(err, mysql.ErrUnknownPlugin):
		return database.Fail(contracts.ConnectFailed, d.flavor.display()+" authentication method is not supported", false)
	}
	var my *mysql.MySQLError
	if errors.As(err, &my) {
		message, retry := d.flavor.display()+" connection failed", true
		switch my.Number {
		case 1045, 1698, 1130, 1251, 1862, 3118:
			message = d.flavor.display() + " authentication failed"
		case 1040, 1129, 1203, 1226:
			message = d.flavor.display() + " refused the connection"
		case 3159:
			message, retry = d.flavor.display()+" requires secure transport; enable verified TLS", false
		}
		return &database.Error{Failure: contracts.Failure{Code: contracts.ConnectFailed, Message: message, Retryable: retry, SQLState: database.SafeSQLState(string(my.SQLState[:]))}}
	}
	return database.Fail(contracts.ConnectFailed, d.flavor.display()+" operation failed; check endpoint, TLS and credentials", true)
}

// operationError classifies failures after connecting. Upstream text is never copied.
func (d *Driver) operationError(err error) error {
	if safe := database.CommonError(err); safe != nil {
		return safe
	}
	switch {
	case errors.Is(err, errReceiveBudget):
		return d.budgetError()
	case errors.Is(err, errMalformed):
		return d.malformed()
	case errors.Is(err, mysql.ErrPktTooLarge):
		return database.Fail(contracts.ResourceLimit, "query parameters exceed packet limit", false)
	}
	var my *mysql.MySQLError
	if errors.As(err, &my) {
		state := string(my.SQLState[:])
		code, message, retry := contracts.QueryFailed, d.flavor.display()+" could not execute the query", false
		switch my.Number {
		case 1044, 1142, 1143, 1227, 1370:
			code, message = contracts.PermissionDenied, "database account lacks permission for this operation"
		case 1046:
			code, message = contracts.InvalidArgument, d.flavor.display()+" connections have no default database; name tables as database.table"
		case 1049:
			code, message = contracts.InvalidArgument, "unknown database"
		case 1205, 1317, 1969, 3024:
			code, message, retry = contracts.QueryTimeout, "database operation timed out", true
		case 1290, 1792, 1836:
			code, message = contracts.ReadOnlyViolation, d.flavor.display()+" rejected a write in a read-only transaction"
		case 1295:
			code, message = contracts.QueryUnsupported, d.flavor.display()+" cannot prepare this statement"
		case 1153:
			code, message = contracts.ResourceLimit, "query exceeds packet limit"
		default:
			if strings.HasPrefix(state, "42") || strings.HasPrefix(state, "22") {
				code, message = contracts.InvalidArgument, "invalid SQL or query parameter"
			}
		}
		return &database.Error{Failure: contracts.Failure{Code: code, Message: message, Retryable: retry, SQLState: database.SafeSQLState(state)}}
	}
	return database.Fail(contracts.ConnectFailed, d.flavor.display()+" operation failed; check endpoint, TLS and credentials", true)
}
