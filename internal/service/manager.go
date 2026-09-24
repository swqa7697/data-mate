package service

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"time"

	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
	"github.com/swqa7697/data-mate/internal/mcp"
	"github.com/swqa7697/data-mate/internal/transport"
	"github.com/swqa7697/data-mate/internal/vault"
)

// Manager owns request admission and one shared driver. It never caches live
// catalog verification or query authorization. Work returns a prepared result
// before dropping state access; callers must write responses only afterwards.
type Manager struct {
	store           *config.Store
	repo            *vault.Repository
	driver          database.Driver
	ctx             context.Context
	cancel          context.CancelFunc
	active, waiting chan struct{}
	mu              sync.Mutex
	enabled         bool
	enable          func(context.Context) error
	management      chan struct{}
	profiles        config.Profiles
	revision        config.Revision
}

func newManager(ctx context.Context, s *config.Store, keys vault.KeyProvider, d database.Driver) *Manager {
	ctx, cancel := context.WithCancel(ctx)
	return &Manager{store: s, repo: vault.New(s, keys), driver: d, ctx: ctx, cancel: cancel, active: make(chan struct{}, database.MaxActiveOperations), waiting: make(chan struct{}, database.MaxWaitingOperations), management: make(chan struct{}, 4)}
}
func (m *Manager) initialize(ctx context.Context) error {
	l, err := m.store.ReadLease(ctx)
	if err != nil {
		return err
	}
	defer l.Release()
	if _, err = m.refresh(l); err != nil {
		return err
	}
	_, err = l.Keyset()
	return err
}
func (m *Manager) refresh(l *config.Lease) (config.Profiles, error) {
	p, rev, err := l.ProfileSnapshot()
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		for _, old := range m.profiles.Connections {
			m.driver.Invalidate(old.ID)
		}
		m.profiles = config.Profiles{}
		m.revision = ""
		return p, database.Fail(contracts.ConfigInvalid, "invalid or missing profile configuration", false)
	}
	if rev != m.revision {
		for _, old := range m.profiles.Connections {
			unchanged := false
			for _, next := range p.Connections {
				if reflect.DeepEqual(old, next) {
					unchanged = true
					break
				}
			}
			if !unchanged {
				m.driver.Invalidate(old.ID)
			}
		}
		m.profiles = p
		m.revision = rev
	}
	return p, nil
}
func (m *Manager) state(ctx context.Context) string {
	if m.ctx.Err() != nil {
		return "stopped"
	}
	l, err := m.store.ReadLease(ctx)
	if err != nil {
		return "degraded"
	}
	defer l.Release()
	if _, err = m.refresh(l); err != nil {
		return "degraded"
	}
	return "running"
}
func (m *Manager) admit(ctx context.Context) (func(), error) {
	if m.ctx.Err() != nil {
		return nil, ErrUnavailable
	}
	select {
	case m.active <- struct{}{}:
		return func() { <-m.active }, nil
	default:
	}
	select {
	case m.waiting <- struct{}{}:
	default:
		return nil, database.Fail(contracts.ResourceLimit, "service queue is full", true)
	}
	defer func() { <-m.waiting }()
	timer := time.NewTimer(database.AdmissionTimeout)
	defer timer.Stop()
	select {
	case m.active <- struct{}{}:
		return func() { <-m.active }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-m.ctx.Done():
		return nil, ErrUnavailable
	case <-timer.C:
		return nil, database.Fail(contracts.ResourceLimit, "service queue deadline exceeded", true)
	}
}

// Work executes an internal driver operation under a fresh shared state lease.
// The callback must prepare its bounded response and finish cleanup before return.
// Alias is resolved only after admission; queued callers retain no old snapshots.
func (m *Manager) Work(parent context.Context, alias string, fn func(context.Context, database.Driver, database.Access) error) error {
	ctx, cancel := context.WithTimeout(parent, config.MaxQueryTimeout+database.AdmissionTimeout+5*time.Second)
	defer cancel()
	stop := context.AfterFunc(m.ctx, cancel)
	defer stop()
	leave, err := m.admit(ctx)
	if err != nil {
		return err
	}
	defer leave()
	l, err := m.store.ReadLease(ctx)
	if err != nil {
		return err
	}
	defer l.Release()
	p, err := m.refresh(l)
	if err != nil {
		return err
	}
	for _, profile := range p.Connections {
		if profile.Alias != alias {
			continue
		}
		ctx, cancel := context.WithTimeout(ctx, time.Duration(profile.Limits.QueryTimeoutMS)*time.Millisecond)
		defer cancel()
		s, err := m.repo.Credential(ctx, l, profile.ID)
		if err != nil {
			code := contracts.VaultUnavailable
			if errors.Is(err, vault.ErrCredentialMissing) {
				code = contracts.CredentialMissing
			}
			return database.Fail(code, "cannot load credentials; repair with db edit", false)
		}
		var hosts []byte
		if profile.Transport.SSH != nil {
			hosts, err = transport.ReadKnownHosts(l)
			if err != nil {
				return database.Fail(contracts.ConnectFailed, "cannot load owned SSH host pins", false)
			}
		}
		access := database.NewAccess(profile, s.Password).WithTransport(transport.Credentials{SSHPassword: s.SSHPassword, SSHPrivateKey: s.SSHPrivateKey, SSHKeyPassphrase: s.SSHKeyPassphrase, ProxyPassword: s.ProxyPassword}, hosts)
		return fn(ctx, m.driver, access)
	}
	return database.Fail(contracts.ConnectionNotFound, "connection not found", false)
}

// Close rejects admission, cancels active work and retires pools and tunnels.
func (m *Manager) Close() { m.cancel(); m.driver.Close(); m.repo.Close() }

// Connections prepares a bounded public snapshot without loading credentials.
func (m *Manager) Connections(parent context.Context, prepare func([]mcp.Connection) error) error {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	stop := context.AfterFunc(m.ctx, cancel)
	defer stop()
	leave, err := m.admit(ctx)
	if err != nil {
		return err
	}
	defer leave()
	l, err := m.store.ReadLease(ctx)
	if err != nil {
		return err
	}
	defer l.Release()
	p, err := m.refresh(l)
	if err != nil {
		return err
	}
	items := make([]mcp.Connection, 0, len(p.Connections))
	for _, p := range p.Connections {
		items = append(items, mcp.Connection{Alias: p.Alias, Driver: p.Driver, Database: p.Connection.Database, Scope: p.Scope})
	}
	return prepare(items)
}

// NewManager binds the service orchestration to external driver and key providers.
func NewManager(ctx context.Context, s *config.Store, keys vault.KeyProvider, d database.Driver) *Manager {
	return newManager(ctx, s, keys, d)
}
