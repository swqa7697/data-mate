package database

import (
	"context"

	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
)

// Resources retire the pools, transports and cursors shared by all drivers.
type Resources interface {
	Invalidate(id string)
	Close()
}

// Router dispatches each operation to the driver named by its profile. Drivers
// share one set of resources, so global admission and pool bounds span drivers.
type Router struct {
	resources Resources
	drivers   map[string]Operations
}

var _ Driver = (*Router)(nil)

// NewRouter binds driver names, as stored in profiles, to their operations.
func NewRouter(resources Resources, drivers map[string]Operations) *Router {
	return &Router{resources: resources, drivers: drivers}
}

func (r *Router) pick(name string) (Operations, error) {
	if d, ok := r.drivers[name]; ok {
		return d, nil
	}
	return nil, Fail(contracts.ConfigInvalid, "unsupported database driver", false)
}

// ValidateProfile checks driver-specific nonsecret settings before vault access.
func (r *Router) ValidateProfile(p config.Profile) error {
	d, err := r.pick(p.Driver)
	if err != nil {
		return err
	}
	return d.ValidateProfile(p)
}

// Validate checks the profile and explicit transport without dialing.
func (r *Router) Validate(a Access) error {
	d, err := r.pick(a.Profile.Driver)
	if err != nil {
		return err
	}
	return d.Validate(a)
}

// Test runs fresh staged diagnostics for one profile.
func (r *Router) Test(ctx context.Context, a Access) (Readiness, error) {
	d, err := r.pick(a.Profile.Driver)
	if err != nil {
		return Readiness{}, err
	}
	return d.Test(ctx, a)
}

// ListTables returns one bounded page of catalog-visible relations.
func (r *Router) ListTables(ctx context.Context, a Access, req PageRequest) (TablePage, error) {
	d, err := r.pick(a.Profile.Driver)
	if err != nil {
		return TablePage{}, err
	}
	return d.ListTables(ctx, a, req)
}

// ListObjects returns one bounded, driver-shaped page of catalog objects.
func (r *Router) ListObjects(ctx context.Context, a Access, req ObjectPageRequest) (any, error) {
	d, err := r.pick(a.Profile.Driver)
	if err != nil {
		return nil, err
	}
	return d.ListObjects(ctx, a, req)
}

// DescribeObject returns a driver-shaped description of one catalog object.
func (r *Router) DescribeObject(ctx context.Context, a Access, req ObjectRequest) (any, error) {
	d, err := r.pick(a.Profile.Driver)
	if err != nil {
		return nil, err
	}
	return d.DescribeObject(ctx, a, req)
}

// DescribeTable returns a driver-shaped description of one relation.
func (r *Router) DescribeTable(ctx context.Context, a Access, t config.Table) (any, error) {
	d, err := r.pick(a.Profile.Driver)
	if err != nil {
		return nil, err
	}
	return d.DescribeTable(ctx, a, t)
}

// DescribeDatabase returns the bounded CLI catalog for one profile.
func (r *Router) DescribeDatabase(ctx context.Context, a Access) (DatabaseDescription, error) {
	d, err := r.pick(a.Profile.Driver)
	if err != nil {
		return DatabaseDescription{}, err
	}
	return d.DescribeDatabase(ctx, a)
}

// Query executes one guarded read statement.
func (r *Router) Query(ctx context.Context, a Access, req QueryRequest) (QueryResult, error) {
	d, err := r.pick(a.Profile.Driver)
	if err != nil {
		return QueryResult{}, err
	}
	return d.Query(ctx, a, req)
}

// Invalidate retires credentials, transports and cursors after a profile change.
func (r *Router) Invalidate(id string) { r.resources.Invalidate(id) }

// Close stops admission and retires all pools and active operations.
func (r *Router) Close() { r.resources.Close() }
