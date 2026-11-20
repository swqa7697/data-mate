package redis

import (
	"context"
	"time"

	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
	"github.com/swqa7697/data-mate/internal/database/pool"
)

// Driver serves Redis operations over the service's shared physical pools.
type Driver struct {
	pools *pool.Registry
}

var (
	_ database.Operations    = (*Driver)(nil)
	_ database.KeyOperations = (*Driver)(nil)
)

// New binds Redis to the shared registry without opening a connection.
func New(pools *pool.Registry) *Driver { return &Driver{pools: pools} }

func checkProfile(p config.Profile) error {
	if p.Driver != "redis" {
		return database.Fail(contracts.ConfigInvalid, "profile driver does not match Redis", false)
	}
	return nil
}

// ValidateProfile validates the public profile without accessing the vault.
func (d *Driver) ValidateProfile(p config.Profile) error {
	a, _, err := database.Normalize(database.NewAccess(p, ""))
	if err != nil {
		return err
	}
	return checkProfile(a.Profile)
}

// Validate checks the explicit route and credentials without dialing.
func (d *Driver) Validate(a database.Access) error {
	a, _, err := database.Normalize(a)
	if err != nil {
		return err
	}
	if err = checkProfile(a.Profile); err != nil {
		return err
	}
	_, err = makeRoute(a)
	return err
}

func checkout(ctx context.Context, r *pool.Registry, a database.Access, rev config.Revision, trace *database.Trace) (*session, context.Context, func(bool), error) {
	open := func(ctx context.Context) (*session, error) { return connect(ctx, a, trace) }
	approve := func(ctx context.Context, s *session) error {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		return auditAccount(ctx, s, a.Profile.Connection.Username)
	}
	return pool.Checkout(ctx, r, a, rev, open, approve)
}

// Test uses isolated physical sessions and performs a fresh audit.
func (d *Driver) Test(ctx context.Context, a database.Access) (database.Readiness, error) {
	return pool.Diagnose(ctx, d.pools, a, func(ctx context.Context, r *pool.Registry, a database.Access, rev config.Revision, trace *database.Trace) (func(bool), error) {
		if err := checkProfile(a.Profile); err != nil {
			return nil, err
		}
		_, _, release, err := checkout(ctx, r, a, rev, trace)
		return release, err
	})
}

func (d *Driver) runNormalized(ctx context.Context, a database.Access, rev config.Revision, fn func(context.Context, *session) error) error {
	if err := checkProfile(a.Profile); err != nil {
		return err
	}
	ctx, cancel := config.QueryContext(ctx, a.Profile.Limits.QueryTimeoutMS)
	defer cancel()
	leave, err := d.pools.Admit(ctx)
	if err != nil {
		return err
	}
	defer leave()
	s, ctx, release, err := checkout(ctx, d.pools, a, rev, nil)
	if err != nil {
		return err
	}
	healthy := false
	defer func() { release(healthy) }()
	err = fn(ctx, s)
	if ctx.Err() != nil {
		return database.CommonError(ctx.Err())
	}
	healthy = s.Reusable()
	return err
}
