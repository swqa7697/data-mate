// Package postgres implements bounded PostgreSQL connection and catalog operations.
package postgres

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
	"github.com/swqa7697/data-mate/internal/database/pool"
)

// Driver implements PostgreSQL operations over the service's shared registry,
// which owns admission, pools and cursor keys. No request result is cached.
type Driver struct {
	pools *pool.Registry
	// connected observes each new physical connection. Tests use it to close
	// connections client-side; production leaves it nil.
	connected func(*pgx.Conn)
}

// New binds PostgreSQL operations to a shared registry without contacting any database.
func New(pools *pool.Registry) *Driver { return &Driver{pools: pools} }

// session adapts one pgx connection to the shared pool.
type session struct{ conn *pgx.Conn }

func (s *session) Reusable() bool          { return !s.conn.IsClosed() && s.conn.PgConn().TxStatus() == 'I' }
func (s *session) Closed() <-chan struct{} { return s.conn.PgConn().CleanupDone() }
func (s *session) Close()                  { closeConn(s.conn) }

func closeConn(c *pgx.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = c.Close(ctx)
}

func messageLimit() error {
	return database.Fail(contracts.ResourceLimit, "PostgreSQL message exceeds limit", false)
}

// checkout acquires an approved connection from r, opening and auditing a new
// physical connection when the pool has no idle one.
func (d *Driver) checkout(ctx context.Context, r *pool.Registry, a database.Access, rev config.Revision, trace *database.Trace) (*pgx.Conn, context.Context, func(bool), error) {
	open := func(ctx context.Context) (*session, error) {
		cfg, err := connectionConfig(a)
		if err != nil {
			return nil, err
		}
		c, err := pgx.ConnectConfig(ctx, cfg)
		if err != nil {
			var pg *pgconn.PgError
			if cfg.Tracer.(*wireState).authStarted.Load() || (errors.As(err, &pg) && (strings.HasPrefix(pg.Code, "28") || pg.Code == "3D000")) {
				trace.Pass("dial")
				trace.Start("authentication")
			}
			if cfg.Tracer.(*wireState).exceeded.Load() {
				return nil, messageLimit()
			}
			return nil, safeError(err)
		}
		if d.connected != nil {
			d.connected(c)
		}
		return &session{c}, nil
	}
	approve := func(ctx context.Context, s *session) error { return validateAccountConnection(ctx, s.conn, a, trace) }
	s, op, release, err := pool.Checkout(ctx, r, a, rev, open, approve)
	if err != nil {
		return nil, nil, nil, err
	}
	return s.conn, op, release, nil
}

// run keeps admission through rollback and bounded payload preparation. Every
// operation rechecks transaction state; account approval belongs to the live pool.
func (d *Driver) run(ctx context.Context, a database.Access, fn func(context.Context, pgx.Tx, int) error) error {
	a, rev, err := database.Normalize(a)
	if err != nil {
		return err
	}
	return d.runNormalized(ctx, a, rev, fn)
}

// runNormalized accepts only the snapshot and revision returned by Normalize in
// this request. Query can reuse its pre-parse validation without caching access.
func (d *Driver) runNormalized(ctx context.Context, a database.Access, rev config.Revision, fn func(context.Context, pgx.Tx, int) error) (result error) {
	ctx, cancel := context.WithTimeout(ctx, database.Timeout(a))
	defer cancel()
	leave, err := d.pools.Admit(ctx)
	if err != nil {
		return err
	}
	defer leave()
	c, ctx, release, err := d.checkout(ctx, d.pools, a, rev, nil)
	if err != nil {
		return err
	}
	healthy := false
	discarded := false
	defer func() {
		if exceeded(c) {
			result = messageLimit()
		}
	}()
	defer func() { release(healthy) }()
	tx, err := c.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly})
	if err != nil {
		return safeError(err)
	}
	defer func() {
		if discarded && c.IsClosed() {
			// Truncation terminated this connection instead of draining rows.
			return
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := tx.Rollback(cleanup); err == nil {
			_, resetErr := c.Exec(cleanup, "DISCARD ALL")
			healthy = resetErr == nil
			if resetErr != nil && result == nil {
				result = safeError(resetErr)
			}
		} else if result == nil {
			result = safeError(err)
		}
	}()
	_, err = tx.Exec(ctx, "SELECT pg_catalog.set_config('statement_timeout', $1, true), pg_catalog.set_config('lock_timeout', '1000', true)", strconv.Itoa(a.Profile.Limits.QueryTimeoutMS))
	if err != nil {
		return safeError(err)
	}
	version, err := checkReadOnlyObserved(ctx, tx, a.Profile.Connection.Username, nil)
	if err != nil {
		return err
	}
	if err = fn(ctx, tx, version); errors.Is(err, errResultDiscarded) && c.IsClosed() {
		discarded = true
	} else if err != nil {
		return safeError(err)
	}
	if ctx.Err() != nil {
		return safeError(ctx.Err())
	}
	return nil
}

// Test always audits a fresh short-lived pool, independently of cached approval.
func (d *Driver) Test(ctx context.Context, a database.Access) (database.Readiness, error) {
	return pool.Diagnose(ctx, d.pools, a, func(ctx context.Context, fresh *pool.Registry, a database.Access, rev config.Revision, trace *database.Trace) (func(bool), error) {
		_, _, release, err := d.checkout(ctx, fresh, a, rev, trace)
		return release, err
	})
}

func safeError(err error) error {
	if safe := database.CommonError(err); safe != nil {
		return safe
	}
	var size *pgproto3.ExceededMaxBodyLenErr
	if errors.As(err, &size) {
		return messageLimit()
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		code, message, retry := contracts.QueryFailed, "PostgreSQL could not execute the query", false
		switch {
		case pg.Code == "57014" || pg.Code == "55P03":
			code, message, retry = contracts.QueryTimeout, "database operation timed out", true
		case pg.Code == "42501":
			code, message = contracts.PermissionDenied, "database account lacks permission for this operation"
		case pg.Code == "25006":
			code, message = contracts.ReadOnlyViolation, "PostgreSQL rejected a write in a read-only transaction"
		case strings.HasPrefix(pg.Code, "22") || strings.HasPrefix(pg.Code, "42") || pg.Code == "08P01":
			code, message = contracts.InvalidArgument, "invalid SQL or query parameter"
		}
		if strings.HasPrefix(pg.Code, "28") || strings.HasPrefix(pg.Code, "08") && pg.Code != "08P01" || pg.Code == "3D000" || pg.Code == "57P01" {
			code, message, retry = contracts.ConnectFailed, "PostgreSQL connection failed", true
		}
		return &database.Error{Failure: contracts.Failure{Code: code, Message: message, Retryable: retry, SQLState: database.SafeSQLState(pg.Code)}}
	}
	return database.Fail(contracts.ConnectFailed, "PostgreSQL operation failed; check endpoint, TLS and credentials", true)
}
