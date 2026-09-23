// Package postgres implements bounded PostgreSQL connection and catalog operations.
package postgres

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
	"github.com/swqa7697/data-mate/internal/transport"
)

const idleTTL = 5 * time.Minute

type pool struct {
	fingerprint string
	slots       chan struct{}
	idle        []*pgx.Conn
	users       int
	ctx         context.Context
	cancel      context.CancelFunc
	timer       *time.Timer
}

// Driver owns process-local admission, lazy two-connection pools and cursor keys.
// A service should share one Driver across profiles. No request result is cached.
type Driver struct {
	mu      sync.Mutex
	pools   map[string]*pool
	active  chan struct{}
	waiting chan struct{}
	key     [32]byte
	closed  bool
	epoch   uint64
}

var _ database.Driver = (*Driver)(nil)

// New creates an idle driver without contacting any database.
func New() (*Driver, error) {
	d := &Driver{pools: make(map[string]*pool), active: make(chan struct{}, 8), waiting: make(chan struct{}, 32)}
	if _, err := rand.Read(d.key[:]); err != nil {
		return nil, err
	}
	return d, nil
}
func (d *Driver) admit(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, safeError(err)
	}
	select {
	case d.active <- struct{}{}:
		return func() { <-d.active }, nil
	default:
	}
	select {
	case d.waiting <- struct{}{}:
	default:
		return nil, database.Fail(contracts.ResourceLimit, "database queue is full", true)
	}
	defer func() { <-d.waiting }()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case d.active <- struct{}{}:
		return func() { <-d.active }, nil
	case <-ctx.Done():
		return nil, safeError(ctx.Err())
	case <-timer.C:
		return nil, database.Fail(contracts.ResourceLimit, "database queue deadline exceeded", true)
	}
}
func closeConn(c *pgx.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = c.Close(ctx)
}
func closePool(p *pool) {
	for _, c := range p.idle {
		closeConn(c)
	}
}
func (d *Driver) retireLocked(id string) *pool {
	p := d.pools[id]
	if p != nil {
		delete(d.pools, id)
		p.cancel()
		if p.timer != nil {
			p.timer.Stop()
		}
	}
	if p != nil {
		retired := &pool{idle: p.idle}
		p.idle = nil
		return retired
	}
	return nil
}

// Invalidate retires credentials, transports and cursors after a profile change.
// Active operations are canceled; their owners finish cleanup before releasing leases.
func (d *Driver) Invalidate(id string) {
	d.mu.Lock()
	d.epoch++
	p := d.retireLocked(id)
	d.mu.Unlock()
	if p != nil {
		closePool(p)
	}
}

// Close stops admission at checkout and retires all pools and active operations.
func (d *Driver) Close() {
	d.mu.Lock()
	d.closed = true
	var all []*pool
	for id := range d.pools {
		all = append(all, d.retireLocked(id))
	}
	d.mu.Unlock()
	for _, p := range all {
		closePool(p)
	}
}
func (d *Driver) checkout(ctx context.Context, a database.Access, rev config.Revision, trace *diagnosticTrace) (*pgx.Conn, context.Context, func(bool), error) {
	mac := hmac.New(sha256.New, d.key[:])
	secrets, hosts := a.TransportCredentials()
	// Hash exact length-framed bytes, including private transport credentials.
	// JSON would replace invalid UTF-8 and could alias distinct credential inputs.
	for _, value := range []string{string(rev), a.Password(), secrets.SSHPassword, secrets.SSHPrivateKey, secrets.SSHKeyPassphrase, secrets.ProxyPassword, string(hosts)} {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(value)))
		mac.Write(size[:])
		mac.Write([]byte(value))
	}
	fp := hex.EncodeToString(mac.Sum(nil))
	id := a.Profile.ID
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil, nil, nil, database.Fail(contracts.ServiceUnavailable, "database driver is closed", false)
	}
	var retired *pool
	p := d.pools[id]
	if p != nil && p.fingerprint != fp {
		retired = d.retireLocked(id)
		p = nil
	}
	if p == nil {
		if len(d.pools) >= 16 {
			for old, v := range d.pools {
				if v.users == 0 {
					retired = d.retireLocked(old)
					break
				}
			}
		}
		if len(d.pools) >= 16 {
			d.mu.Unlock()
			return nil, nil, nil, database.Fail(contracts.ResourceLimit, "database pool limit reached", true)
		}
		pc, cancel := context.WithCancel(context.Background())
		p = &pool{fingerprint: fp, slots: make(chan struct{}, 2), ctx: pc, cancel: cancel}
		d.pools[id] = p
	}
	p.users++
	if p.timer != nil {
		p.timer.Stop()
	}
	d.mu.Unlock()
	if retired != nil {
		closePool(retired)
	}
	op, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(p.ctx, cancel)
	var c *pgx.Conn
	acquired := false
	release := func(healthy bool) {
		stop()
		cancel()
		if c != nil && (!healthy || c.IsClosed() || c.PgConn().TxStatus() != 'I') {
			closeConn(c)
			c = nil
		}
		d.mu.Lock()
		if c != nil && d.pools[id] == p && !d.closed {
			p.idle = append(p.idle, c)
			c = nil
		}
		p.users--
		if p.users == 0 && d.pools[id] == p {
			p.timer = time.AfterFunc(idleTTL, func() {
				d.mu.Lock()
				var old *pool
				if d.pools[id] == p && p.users == 0 {
					old = d.retireLocked(id)
				}
				d.mu.Unlock()
				if old != nil {
					closePool(old)
				}
			})
		}
		d.mu.Unlock()
		if c != nil {
			closeConn(c)
		}
		if acquired {
			<-p.slots
		}
	}
	select {
	case p.slots <- struct{}{}:
		acquired = true
	case <-op.Done():
		release(false)
		return nil, nil, nil, safeError(op.Err())
	}
	d.mu.Lock()
	if len(p.idle) > 0 {
		n := len(p.idle) - 1
		c = p.idle[n]
		p.idle = p.idle[:n]
	}
	d.mu.Unlock()
	if c == nil {
		cfg, err := connectionConfig(a)
		if err != nil {
			release(false)
			return nil, nil, nil, err
		}
		var errDial error
		c, errDial = pgx.ConnectConfig(op, cfg)
		if errDial != nil {
			var pg *pgconn.PgError
			if cfg.Tracer.(*wireState).authStarted.Load() || (errors.As(errDial, &pg) && (strings.HasPrefix(pg.Code, "28") || pg.Code == "3D000")) {
				trace.pass("dial")
				trace.start("authentication")
			}
			release(false)
			if cfg.Tracer.(*wireState).exceeded.Load() {
				return nil, nil, nil, database.Fail(contracts.ResourceLimit, "PostgreSQL message exceeds limit", false)
			}
			return nil, nil, nil, safeError(errDial)
		}
	}
	return c, op, release, nil
}

// run keeps admission through rollback and bounded payload preparation. Every
// operation rechecks readiness in its own read-only transaction; no cached grants.
func (d *Driver) run(ctx context.Context, a database.Access, fn func(context.Context, pgx.Tx, int) error) error {
	return d.runObserved(ctx, a, nil, fn)
}
func (d *Driver) runObserved(ctx context.Context, a database.Access, trace *diagnosticTrace, fn func(context.Context, pgx.Tx, int) error) (result error) {
	trace.start("config")
	a, rev, err := normalized(a)
	if err != nil {
		return err
	}
	trace.pass("config")
	trace.start("dial")
	ctx, cancel := context.WithTimeout(ctx, time.Duration(a.Profile.Limits.QueryTimeoutMS)*time.Millisecond)
	defer cancel()
	leave, err := d.admit(ctx)
	if err != nil {
		return err
	}
	defer leave()
	c, ctx, release, err := d.checkout(ctx, a, rev, trace)
	if err != nil {
		return err
	}
	trace.pass("dial")
	trace.pass("authentication")
	trace.start("version")
	healthy := false
	discarded := false
	defer func() {
		if exceeded(c) {
			result = database.Fail(contracts.ResourceLimit, "PostgreSQL message exceeds limit", false)
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
			healthy = true
		} else if result == nil {
			result = safeError(err)
		}
	}()
	_, err = tx.Exec(ctx, "SELECT pg_catalog.set_config('statement_timeout', $1, true), pg_catalog.set_config('lock_timeout', '1000', true)", strconv.Itoa(a.Profile.Limits.QueryTimeoutMS))
	if err != nil {
		return safeError(err)
	}
	version, err := checkRoleObserved(ctx, tx, a.Profile.Connection.Username, trace)
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

func safeError(err error) error {
	var known *database.Error
	if errors.As(err, &known) {
		return known
	}
	for _, safe := range []error{transport.ErrUnknownHost, transport.ErrChangedHost, transport.ErrKnownHosts} {
		if errors.Is(err, safe) {
			return database.Fail(contracts.ConnectFailed, safe.Error(), false)
		}
	}
	if errors.Is(err, transport.ErrConfiguration) {
		return database.Fail(contracts.ConfigInvalid, "invalid transport configuration or credentials", false)
	}
	if errors.Is(err, context.Canceled) {
		return database.Fail(contracts.Cancelled, "database operation canceled", true)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return database.Fail(contracts.QueryTimeout, "database operation timed out", true)
	}
	var size *pgproto3.ExceededMaxBodyLenErr
	if errors.As(err, &size) {
		return database.Fail(contracts.ResourceLimit, "PostgreSQL message exceeds limit", false)
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "57014", "55P03":
			return database.Fail(contracts.QueryTimeout, "database operation timed out", true)
		case "42501":
			return database.Fail(contracts.PolicyUnsafe, "database privileges changed or are insufficient", false)
		case "25006":
			return database.Fail(contracts.PolicyUnsafe, "server policy attempted a write in a read-only query", false)
		case "22003", "22007", "22008", "22012", "22023", "22P02":
			return database.Fail(contracts.InvalidArgument, "query value or arithmetic operation is invalid", false)
		}
	}
	return database.Fail(contracts.ConnectFailed, "PostgreSQL operation failed; check endpoint, TLS and credentials", true)
}
func payloadBound(v any, a database.Access) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	limit := config.DefaultLimits().MaxResultBytes
	if a.Profile.Limits != nil {
		limit = a.Profile.Limits.MaxResultBytes
	} // reserve JSON escaping plus duplicated compatibility representation
	if len(b)*3+1024 > limit {
		return database.Fail(contracts.ResourceLimit, "metadata result exceeds payload budget; request a smaller page", false)
	}
	return nil
}
