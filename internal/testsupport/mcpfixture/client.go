// Package mcpfixture connects database integration tests to Data Mate's MCP handlers.
package mcpfixture

import (
	"context"
	"net"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
	protocol "github.com/swqa7697/data-mate/internal/mcp"
)

// The owned fixture has immutable access. Production leases remain service-owned.
type backend struct {
	driver database.Driver
	access database.Access
}

func (b *backend) Work(ctx context.Context, alias string, fn func(context.Context, database.Driver, database.Access) error) error {
	if alias != b.access.Profile.Alias {
		return database.Fail(contracts.ConnectionNotFound, "connection not found", false)
	}
	return fn(ctx, b.driver, b.access)
}
func (b *backend) Connections(_ context.Context, fn func([]protocol.Connection) error) error {
	p := b.access.Profile
	return fn([]protocol.Connection{{
		Alias: p.Alias, Driver: p.Driver, Database: p.Connection.Database,
		Limits: protocol.ConnectionLimits{
			MaxRows: p.Limits.MaxRows, MaxResultBytes: p.Limits.MaxResultBytes, QueryTimeoutMS: p.Limits.QueryTimeoutMS,
		},
	}})
}

// Connect starts a real MCP session over a private pipe for one immutable
// connection. The test owns the driver; cleanup closes both ends of the pipe.
func Connect(t *testing.T, driver database.Driver, access database.Access) *sdk.ClientSession {
	t.Helper()
	if access.Profile.Limits == nil {
		limits := config.DefaultLimits()
		access.Profile.Limits = &limits
	}
	b := &backend{driver: driver, access: access}
	server, client := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = protocol.Serve(t.Context(), server, b, "fixture")
	}()
	t.Cleanup(func() {
		_ = client.Close()
		<-done
	})
	session, err := sdk.NewClient(&sdk.Implementation{Name: "integration", Version: "1"}, nil).Connect(t.Context(), &sdk.IOTransport{Reader: client, Writer: client}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}
