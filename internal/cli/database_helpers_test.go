package cli

import (
	"context"
	"fmt"
	"github.com/spf13/cobra"
	"github.com/swqa7697/data-mate/internal/service"
	"github.com/swqa7697/data-mate/internal/vault"
	"strings"

	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
	"github.com/swqa7697/data-mate/internal/database/postgres"
)

// Shared by command diagnostics and the PTY subprocess; no network or native keys.
type fixtureDatabase struct {
	requests      []database.ScopeRequest
	tested        []string
	closed        bool
	browseError   bool
	schemaCount   int
	bulkError     bool
	bulkWait      bool
	described     []string
	describeError error
	emptyCatalog  bool
}

func (d *fixtureDatabase) DescribeDatabase(_ context.Context, a database.Access) (database.DatabaseDescription, error) {
	d.described = append(d.described, a.Profile.Alias)
	if d.describeError != nil {
		return database.DatabaseDescription{}, d.describeError
	}
	out := database.DatabaseDescription{Version: 1, Alias: a.Profile.Alias, Database: a.Profile.Connection.Database, Scope: a.Profile.Scope, Schemas: []database.SchemaDescription{}}
	if !d.emptyCatalog {
		for _, name := range []string{"Dot.Schema", "empty", "private", "public"} {
			s := database.SchemaDescription{Name: name, Allowed: a.Profile.Scope.ContainsSchema(name), Tables: []database.RelationName{}}
			if name != "empty" {
				s.Tables = append(s.Tables, database.RelationName{Name: "line\n\x1b[31m", Kind: "table"})
			}
			out.Schemas = append(out.Schemas, s)
		}
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
		if a.Profile.Alias == "bad" && s == "authentication" {
			out.Stage = s
			e := database.Fail(contracts.ConnectFailed, "authentication failed", false).(*database.Error)
			out.Stages = append(out.Stages, database.Stage{Stage: s, Error: &e.Failure})
			return out, e
		}
		out.Stages = append(out.Stages, database.Stage{Stage: s, OK: true})
	}
	return out, nil
}
func (d *fixtureDatabase) BrowseScope(ctx context.Context, _ database.Access, r database.ScopeRequest) (database.ScopePage, error) {
	d.requests = append(d.requests, r)
	if d.browseError {
		return database.ScopePage{}, database.Fail(contracts.ConnectFailed, "catalog fetch failed", true)
	}
	if d.bulkWait && len(d.requests) > 1 {
		<-ctx.Done()
		return database.ScopePage{}, ctx.Err()
	}
	if d.bulkError && len(d.requests) > 1 {
		return database.ScopePage{}, config.ErrRevision
	}
	count := d.schemaCount
	if count == 0 {
		count = 103
	}
	names := []string{"Dot.Schema"}
	for i := 0; i < count; i++ {
		names = append(names, fmt.Sprintf("schema%04d", i))
	}
	p := database.ScopePage{}
	for _, name := range names {
		if name > r.After && strings.Contains(strings.ToLower(name), strings.ToLower(r.Search)) {
			p.Schemas = append(p.Schemas, name)
		}
		if len(p.Schemas) == 51 {
			p.Schemas = p.Schemas[:50]
			p.Next = p.Schemas[49]
			break
		}
	}
	return p, nil
}

// The external management seam runs the real service orchestrator with isolated
// fake providers. Socket identity/framing is exercised by service regressions.
type cliDatabase interface {
	ValidateProfile(config.Profile) error
	Test(context.Context, database.Access) (database.Readiness, error)
	BrowseScope(context.Context, database.Access, database.ScopeRequest) (database.ScopePage, error)
	DescribeDatabase(context.Context, database.Access) (database.DatabaseDescription, error)
	Close()
}
type databaseFactory func() (cliDatabase, error)

func defaultDatabase() (cliDatabase, error) { return postgres.New() }

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
	return r, service.ManagementError(r.Error)
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
