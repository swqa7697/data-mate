package postgres

import (
	"context"

	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/database"
)

// operations adapts the typed driver to the shared boundary, which marshals
// PostgreSQL-shaped documents unchanged.
type operations struct{ *Driver }

var _ database.Operations = operations{}

// Operations returns the shared-boundary view of the driver.
func (d *Driver) Operations() database.Operations { return operations{d} }

func (o operations) ListObjects(ctx context.Context, a database.Access, req database.ObjectPageRequest) (any, error) {
	return o.Driver.ListObjects(ctx, a, req)
}

func (o operations) DescribeObject(ctx context.Context, a database.Access, req database.ObjectRequest) (any, error) {
	return o.Driver.DescribeObject(ctx, a, req)
}

func (o operations) DescribeTable(ctx context.Context, a database.Access, t config.Table) (any, error) {
	return o.Driver.DescribeTable(ctx, a, t)
}
