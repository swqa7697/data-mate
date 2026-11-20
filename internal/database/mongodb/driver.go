package mongodb

import (
	"context"
	"time"

	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
	"github.com/swqa7697/data-mate/internal/database/pool"
	"go.mongodb.org/mongo-driver/v2/bson"
)

const auditTimeout = 10 * time.Second

// Driver serves MongoDB operations over the service's shared physical pools.
// MongoDB has no read-only transaction: the read-only boundary is the account
// audit and authorization probe, commands built by the driver, the input guard
// and cursors that never outlive an operation.
type Driver struct {
	pools *pool.Registry
}

var (
	_ database.Operations         = (*Driver)(nil)
	_ database.DocumentOperations = (*Driver)(nil)
)

// New binds MongoDB to the shared registry without opening a connection.
func New(pools *pool.Registry) *Driver { return &Driver{pools: pools} }

func checkProfile(p config.Profile) error {
	if p.Driver != "mongodb" {
		return database.Fail(contracts.ConfigInvalid, "profile driver does not match MongoDB", false)
	}
	return nil
}

// normalizeAccess revalidates an entry point's access snapshot with
// database.Normalize and requires a MongoDB profile.
func normalizeAccess(a database.Access) (database.Access, config.Revision, error) {
	a, rev, err := database.Normalize(a)
	if err != nil {
		return a, "", err
	}
	if err = checkProfile(a.Profile); err != nil {
		return a, "", err
	}
	return a, rev, nil
}

// ValidateProfile validates the public profile without accessing the vault.
func (d *Driver) ValidateProfile(p config.Profile) error {
	_, _, err := normalizeAccess(database.NewAccess(p, ""))
	return err
}

// Validate checks the explicit route and credentials without dialing.
func (d *Driver) Validate(a database.Access) error {
	a, _, err := normalizeAccess(a)
	if err != nil {
		return err
	}
	if _, err = makeRoute(a); err != nil {
		return err
	}
	_, err = scramPassword(a)
	return err
}

func checkout(ctx context.Context, r *pool.Registry, a database.Access, rev config.Revision, trace *database.Trace) (*session, context.Context, func(bool), error) {
	open := func(ctx context.Context) (*session, error) { return connect(ctx, a, trace) }
	approve := func(ctx context.Context, s *session) error {
		ctx, cancel := context.WithTimeout(ctx, auditTimeout)
		defer cancel()
		return auditAccount(ctx, s, a.Profile.Connection.Username, a.Profile.Connection.Database)
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

// runNormalized holds admission and a pooled session for fn, which closes its
// own cursors. It accepts only the snapshot and revision returned by Normalize
// in this request. A session is reused only if it is still open and the
// operation was not interrupted.
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
		return safeError(err)
	}
	healthy := false
	defer func() { release(healthy) }()
	err = fn(ctx, s)
	if ctx.Err() != nil {
		return database.CommonError(ctx.Err())
	}
	healthy = s.Reusable()
	return safeError(err)
}

// withMaxTime adds the server-side time limit to a command that runs on the
// server: the remaining profile budget, at least 1 ms, or none when the
// profile sets no timeout. It is never sent on getMore or driver control
// commands.
func withMaxTime(ctx context.Context, a database.Access, cmd bson.D) bson.D {
	timeoutMS := a.Profile.Limits.QueryTimeoutMS
	if timeoutMS == 0 {
		return cmd
	}
	ms := int64(timeoutMS)
	if deadline, ok := ctx.Deadline(); ok {
		ms = max(1, min(ms, time.Until(deadline).Milliseconds()))
	}
	return append(cmd[:len(cmd):len(cmd)], kv("maxTimeMS", ms))
}
