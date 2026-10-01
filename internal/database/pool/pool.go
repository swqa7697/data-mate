// Package pool owns process-local admission, lazy per-profile connection pools,
// account approval and authenticated metadata cursors shared by all drivers.
// Drivers supply how a physical connection opens and how its account is audited.
package pool

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"sync"
	"time"

	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
)

const (
	defaultIdleTTL           = 5 * time.Minute
	maxConnectionsPerProfile = 8
	maxPools                 = 16
)

// Conn is one physical database session owned by a pool.
type Conn interface {
	// Reusable reports whether the session is open, idle and safe to keep.
	Reusable() bool
	// Closed is closed once the physical connection has finished cleanup.
	Closed() <-chan struct{}
	// Close terminates the physical connection within a bounded cleanup.
	Close()
}

// Options tunes registry timing; zero values select production defaults.
type Options struct {
	// IdleTTL retires a pool once it has had no users for this long.
	IdleTTL time.Duration
}

type admission struct{ active, waiting chan struct{} }

type pool struct {
	fingerprint string
	slots       chan struct{}
	idle        []Conn
	users       int
	ctx         context.Context
	cancel      context.CancelFunc
	timer       *time.Timer
	live        map[Conn]bool
	approved    bool
	generation  uint64
	validation  *validation
}

type validation struct {
	done chan struct{}
	err  error
}

// forgetClosed runs under Registry.mu. Waiting operations never extend approval.
func (p *pool) forgetClosed() {
	for c := range p.live {
		select {
		case <-c.Closed():
			delete(p.live, c)
		default:
		}
	}
	if len(p.live) == 0 && p.approved {
		p.approved = false
		p.generation++
	}
}

// Registry owns process-local admission, lazy eight-connection pools and cursor
// keys. A service shares one Registry across all drivers and profiles, so global
// bounds span drivers. No request result is cached.
type Registry struct {
	mu        sync.Mutex
	pools     map[string]*pool
	admission *admission
	key       [32]byte
	idleTTL   time.Duration
	closed    bool
	epoch     uint64
}

var _ database.Resources = (*Registry)(nil)

// New creates an idle registry without contacting any database.
func New(o Options) (*Registry, error) {
	a := &admission{active: make(chan struct{}, database.MaxActiveOperations), waiting: make(chan struct{}, database.MaxWaitingOperations)}
	return newRegistry(a, o.IdleTTL)
}

func newRegistry(a *admission, idleTTL time.Duration) (*Registry, error) {
	if idleTTL <= 0 {
		idleTTL = defaultIdleTTL
	}
	r := &Registry{pools: make(map[string]*pool), admission: a, idleTTL: idleTTL}
	if _, err := rand.Read(r.key[:]); err != nil {
		return nil, err
	}
	return r, nil
}

// Isolated returns a registry with fresh pools and cursor keys that shares this
// registry's admission. Diagnostics use it so they never reuse pool approval.
func (r *Registry) Isolated() (*Registry, error) { return newRegistry(r.admission, r.idleTTL) }

// Admit reserves one active operation, waiting in a bounded queue.
func (r *Registry) Admit(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, database.CommonError(err)
	}
	a := r.admission
	select {
	case a.active <- struct{}{}:
		return func() { <-a.active }, nil
	default:
	}
	select {
	case a.waiting <- struct{}{}:
	default:
		return nil, database.Fail(contracts.ResourceLimit, "database queue is full", true)
	}
	defer func() { <-a.waiting }()
	timer := time.NewTimer(database.AdmissionTimeout)
	defer timer.Stop()
	select {
	case a.active <- struct{}{}:
		return func() { <-a.active }, nil
	case <-ctx.Done():
		return nil, database.CommonError(ctx.Err())
	case <-timer.C:
		return nil, database.Fail(contracts.ResourceLimit, "database queue deadline exceeded", true)
	}
}

func closePool(p *pool) {
	for _, c := range p.idle {
		c.Close()
	}
}

func (r *Registry) retireLocked(id string) *pool {
	p := r.pools[id]
	if p == nil {
		return nil
	}
	delete(r.pools, id)
	p.approved = false
	p.generation++
	p.cancel()
	if p.timer != nil {
		p.timer.Stop()
	}
	retired := &pool{idle: p.idle}
	p.idle = nil
	return retired
}

// Invalidate retires credentials, transports and cursors after a profile change.
// Active operations are canceled; their owners finish cleanup before releasing leases.
func (r *Registry) Invalidate(id string) {
	r.mu.Lock()
	r.epoch++
	p := r.retireLocked(id)
	r.mu.Unlock()
	if p != nil {
		closePool(p)
	}
}

// Close stops admission at checkout and retires all pools and active operations.
func (r *Registry) Close() {
	r.mu.Lock()
	r.closed = true
	var all []*pool
	for id := range r.pools {
		all = append(all, r.retireLocked(id))
	}
	r.mu.Unlock()
	for _, p := range all {
		closePool(p)
	}
}

// Closed reports whether Close has run.
func (r *Registry) Closed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed
}

// Epoch is the process invalidation epoch bound into cursors.
func (r *Registry) Epoch() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.epoch
}

// Stats is a point-in-time view of one profile's pool for diagnostics.
type Stats struct {
	Present    bool
	Users      int
	Slots      int
	Idle       int
	Live       int
	Approved   bool
	Validating bool
}

// Stats reports one profile's pool without changing it.
func (r *Registry) Stats(id string) Stats {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.pools[id]
	if p == nil {
		return Stats{}
	}
	return Stats{Present: true, Users: p.users, Slots: len(p.slots), Idle: len(p.idle), Live: len(p.live), Approved: p.approved, Validating: p.validation != nil}
}

// Len reports the number of retained pools.
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pools)
}

// fingerprint hashes exact length-framed bytes, including private transport
// credentials. JSON would replace invalid UTF-8 and could alias distinct inputs.
func (r *Registry) fingerprint(a database.Access, rev config.Revision) string {
	mac := hmac.New(sha256.New, r.key[:])
	secrets, hosts := a.TransportCredentials()
	for _, value := range []string{string(rev), a.Password(), secrets.SSHPassword, secrets.SSHPrivateKey, secrets.SSHKeyPassphrase, secrets.ProxyPassword, string(hosts)} {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(value)))
		mac.Write(size[:])
		mac.Write([]byte(value))
	}
	return hex.EncodeToString(mac.Sum(nil))
}

// Checkout acquires an approved connection for one operation. It reuses an idle
// connection of the profile's live pool or opens one, and audits a pool's first
// physical connection; concurrent openers share that validation. The returned
// context is canceled when the pool retires. The caller must call release
// exactly once, after its cleanup, passing whether the connection may be reused.
func Checkout[C Conn](ctx context.Context, r *Registry, a database.Access, rev config.Revision, open func(context.Context) (C, error), approve func(context.Context, C) error) (C, context.Context, func(healthy bool), error) {
	var zero C
	fp := r.fingerprint(a, rev)
	id := a.Profile.ID
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return zero, nil, nil, database.Fail(contracts.ServiceUnavailable, "database driver is closed", false)
	}
	var retired *pool
	p := r.pools[id]
	if p != nil && p.fingerprint != fp {
		retired = r.retireLocked(id)
		p = nil
	}
	if p == nil {
		if len(r.pools) >= maxPools {
			for old, v := range r.pools {
				if v.users == 0 {
					retired = r.retireLocked(old)
					break
				}
			}
		}
		if len(r.pools) >= maxPools {
			r.mu.Unlock()
			return zero, nil, nil, database.Fail(contracts.ResourceLimit, "database pool limit reached", true)
		}
		pc, cancel := context.WithCancel(context.Background())
		p = &pool{fingerprint: fp, slots: make(chan struct{}, maxConnectionsPerProfile), ctx: pc, cancel: cancel}
		r.pools[id] = p
	}
	p.users++
	if p.timer != nil {
		p.timer.Stop()
	}
	r.mu.Unlock()
	if retired != nil {
		closePool(retired)
	}
	op, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(p.ctx, cancel)
	var c C
	have, acquired := false, false
	release := func(healthy bool) {
		stop()
		cancel()
		if have && (!healthy || !c.Reusable()) {
			// A discarded connection cannot retain approval while socket cleanup
			// finishes asynchronously and another caller opens a replacement.
			r.mu.Lock()
			delete(p.live, c)
			p.forgetClosed()
			r.mu.Unlock()
			c.Close()
			have = false
		}
		r.mu.Lock()
		p.forgetClosed()
		if have && r.pools[id] == p && !r.closed {
			p.idle = append(p.idle, c)
			have = false
		}
		p.users--
		if p.users == 0 && r.pools[id] == p {
			p.timer = time.AfterFunc(r.idleTTL, func() {
				r.mu.Lock()
				var old *pool
				if r.pools[id] == p && p.users == 0 {
					old = r.retireLocked(id)
				}
				r.mu.Unlock()
				if old != nil {
					closePool(old)
				}
			})
		}
		r.mu.Unlock()
		if have {
			c.Close()
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
		return zero, nil, nil, database.CommonError(op.Err())
	}
	var discard []Conn
	r.mu.Lock()
	p.forgetClosed()
	for len(p.idle) > 0 {
		n := len(p.idle) - 1
		candidate := p.idle[n]
		p.idle = p.idle[:n]
		if typed, ok := candidate.(C); ok && candidate.Reusable() {
			c, have = typed, true
			break
		}
		// A connection of another driver or a closed one is never asserted
		// into use; it is forgotten and closed outside the lock.
		delete(p.live, candidate)
		p.forgetClosed()
		discard = append(discard, candidate)
	}
	r.mu.Unlock()
	for _, old := range discard {
		old.Close()
	}
	if !have {
		opened, err := open(op)
		if err != nil {
			release(false)
			return zero, nil, nil, err
		}
		c, have = opened, true
	}
	if err := approvePool(op, r, p, id, c, approve); err != nil {
		release(false)
		return zero, nil, nil, err
	}
	return c, op, release, nil
}

func approvePool[C Conn](ctx context.Context, r *Registry, p *pool, id string, c C, approve func(context.Context, C) error) error {
	r.mu.Lock()
	p.forgetClosed()
	if p.live == nil {
		p.live = make(map[Conn]bool)
	}
	p.live[c] = true
	if p.approved {
		r.mu.Unlock()
		return nil
	}
	if pending := p.validation; pending != nil {
		r.mu.Unlock()
		select {
		case <-ctx.Done():
			return database.CommonError(ctx.Err())
		case <-pending.done:
			return pending.err
		}
	}
	pending := &validation{done: make(chan struct{})}
	p.validation = pending
	generation := p.generation
	r.mu.Unlock()
	err := approve(ctx, c)
	r.mu.Lock()
	p.forgetClosed()
	if err == nil && (r.closed || r.pools[id] != p || p.ctx.Err() != nil || ctx.Err() != nil || generation != p.generation || !c.Reusable()) {
		err = database.Fail(contracts.Cancelled, "account validation was cancelled", false)
	}
	p.approved = err == nil
	pending.err = err
	p.validation = nil
	close(pending.done)
	r.mu.Unlock()
	return err
}
