package cli

import (
	"context"
	"github.com/spf13/cobra"
	"github.com/swqa7697/data-mate/internal/service"
	"github.com/swqa7697/data-mate/internal/vault"
	"os"

	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
	"github.com/swqa7697/data-mate/internal/database/postgres"
)

// Shared by command diagnostics and the PTY subprocess; no network or native keys.
type fixtureDatabase struct {
	acceptAll     bool
	tested        []string
	closed        bool
	described     []string
	describeError error
	emptyCatalog  bool
	catalog       []database.SchemaDescription
}

func (d *fixtureDatabase) DescribeDatabase(_ context.Context, a database.Access) (database.DatabaseDescription, error) {
	d.described = append(d.described, a.Profile.Alias)
	if d.describeError != nil {
		return database.DatabaseDescription{}, d.describeError
	}
	out := database.DatabaseDescription{Version: 1, Alias: a.Profile.Alias, Database: a.Profile.Connection.Database, Schemas: []database.SchemaDescription{}}
	if !d.emptyCatalog {
		for _, name := range []string{"Dot.Schema", "empty", "private", "public"} {
			s := database.SchemaDescription{Name: name, Tables: []database.RelationName{}, Enums: []database.CatalogName{}, Sequences: []database.CatalogName{}}
			if name != "empty" {
				s.Tables = append(s.Tables, database.RelationName{Name: "line\n\x1b[31m", Kind: "table"})
			}
			if name == "public" {
				s.Enums = append(s.Enums, database.CatalogName{Name: "status"})
				s.Sequences = append(s.Sequences, database.CatalogName{Name: "items_id_seq"})
				s.Tables = append(s.Tables, database.RelationName{Name: "items", Kind: "table"}, database.RelationName{Name: "report", Kind: "view"}, database.RelationName{Name: "cached_report", Kind: "materialized_view"})
			}
			out.Schemas = append(out.Schemas, s)
		}
	}
	if d.catalog != nil {
		out.Schemas = d.catalog
	}
	return out, nil
}

func (d *fixtureDatabase) ValidateProfile(p config.Profile) error {
	var driver postgres.Driver
	return driver.ValidateProfile(p)
}
func (d *fixtureDatabase) Close() { d.closed = true }
func (d *fixtureDatabase) Test(_ context.Context, a database.Access) (database.Readiness, error) {
	d.tested = append(d.tested, a.Profile.Alias)
	out := database.Readiness{ServerVersion: 160000, Stage: "read_only"}
	for _, s := range []string{"config", "dial", "authentication", "version", "read_only"} {
		if !d.acceptAll && a.Profile.Alias == "bad" && s == "authentication" {
			out.Stage = s
			e := database.Fail(contracts.ConnectFailed, "authentication failed", false).(*database.Error)
			out.Stages = append(out.Stages, database.Stage{Stage: s, Error: &e.Failure})
			return out, e
		}
		out.Stages = append(out.Stages, database.Stage{Stage: s, OK: true})
	}
	return out, nil
}

// The external management seam runs the real service orchestrator with isolated
// fake providers. Socket identity/framing is exercised by service regressions.
type cliDatabase interface {
	ValidateProfile(config.Profile) error
	Test(context.Context, database.Access) (database.Readiness, error)
	DescribeDatabase(context.Context, database.Access) (database.DatabaseDescription, error)
	Close()
}
type databaseFactory func() (cliDatabase, error)

func defaultDatabase() (cliDatabase, error) {
	if os.Getenv("DATA_MATE_CLI_REAL_DRIVER") == "1" {
		return postgres.New()
	}
	return &fixtureDatabase{acceptAll: true}, nil
}

type testDriver struct {
	database.Driver
	cliDatabase
}

func (d testDriver) ValidateProfile(p config.Profile) error { return d.cliDatabase.ValidateProfile(p) }
func (d testDriver) Test(c context.Context, a database.Access) (database.Readiness, error) {
	return d.cliDatabase.Test(c, a)
}
func (d testDriver) Close()            { d.cliDatabase.Close() }
func (d testDriver) Invalidate(string) {}

type localManagement struct {
	manager *service.Manager
	store   *config.Store
}

func (c *localManagement) Request(ctx context.Context, q service.ManagementRequest) (service.ManagementReply, error) {
	r := c.manager.HandleManagement(ctx, q)
	return r, r.ResultError()
}
func (c *localManagement) Close() { c.manager.Close(); c.store.Close() }
func newCommand(build Build, keys vault.KeyProvider) *cobra.Command {
	return commandWithDatabase(build, keys, defaultDatabase)
}
func commandWithDatabase(build Build, keys vault.KeyProvider, factory databaseFactory) *cobra.Command {
	return commandWithManagement(build, keys, func(ctx context.Context, root config.Root) (managementClient, error) {
		s, e := config.Open(ctx, root, nil)
		if e != nil {
			return nil, e
		}
		d, e := factory()
		if e != nil {
			s.Close()
			return nil, e
		}
		return &localManagement{service.NewManager(ctx, s, keys, testDriver{cliDatabase: d}), s}, nil
	})
}
