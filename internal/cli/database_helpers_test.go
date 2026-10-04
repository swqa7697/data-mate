package cli

import (
	"context"
	"github.com/spf13/cobra"
	"github.com/swqa7697/data-mate/internal/service"
	"github.com/swqa7697/data-mate/internal/vault"
	"os"
	"slices"
	"sync"

	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
	"github.com/swqa7697/data-mate/internal/database/postgres"
)

// Shared by command diagnostics and the PTY subprocess; no network or native keys.
// The service runs batch checks concurrently, so observed calls are guarded.
type fixtureDatabase struct {
	acceptAll     bool
	describeError error
	emptyCatalog  bool
	catalog       []database.SchemaDescription
	// before runs first in each Test call; an error fails the dial stage.
	before func(context.Context, string) error

	mu        sync.Mutex
	tested    []string
	described []string
	closed    bool
}

// testedAliases returns tested aliases in alias order, independent of scheduling.
func (d *fixtureDatabase) testedAliases() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Sorted(slices.Values(d.tested))
}
func (d *fixtureDatabase) describedAliases() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.described)
}
func (d *fixtureDatabase) isClosed() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.closed
}

func (d *fixtureDatabase) DescribeDatabase(_ context.Context, a database.Access) (database.DatabaseDescription, error) {
	d.mu.Lock()
	d.described = append(d.described, a.Profile.Alias)
	d.mu.Unlock()
	if d.describeError != nil {
		return database.DatabaseDescription{}, d.describeError
	}
	out := database.DatabaseDescription{Version: 1, Alias: a.Profile.Alias, Database: a.Profile.Connection.Database, Schemas: []database.SchemaDescription{}}
	if !d.emptyCatalog {
		for _, name := range []string{"Dot.Schema", "empty", "private", "public"} {
			s := database.SchemaDescription{Name: name, Tables: []database.RelationName{}, Enums: []database.CatalogName{}, Sequences: []database.CatalogName{}, Indexes: []database.CatalogName{}, Functions: []database.CatalogName{}}
			if name != "empty" {
				s.Tables = append(s.Tables, database.RelationName{Name: "line\n\x1b[31m", Kind: "table"})
			}
			if name == "public" {
				s.Indexes = append(s.Indexes, database.CatalogName{Name: "z_idx"}, database.CatalogName{Name: "a_idx"}, database.CatalogName{Name: "idx\n\x1b[31m"})
				s.Functions = append(s.Functions, database.CatalogName{Name: "z_fn"}, database.CatalogName{Name: "a_fn"}, database.CatalogName{Name: "fn\n\x1b[31m"})
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
func (d *fixtureDatabase) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = true
}
func (d *fixtureDatabase) Test(ctx context.Context, a database.Access) (database.Readiness, error) {
	d.mu.Lock()
	d.tested = append(d.tested, a.Profile.Alias)
	d.mu.Unlock()
	if d.before != nil {
		if err := d.before(ctx, a.Profile.Alias); err != nil {
			e := database.Fail(contracts.ConnectFailed, "fixture hook: "+err.Error(), false).(*database.Error)
			return database.Readiness{Stage: "dial", Stages: []database.Stage{{Stage: "dial", Error: &e.Failure}}}, e
		}
	}
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
