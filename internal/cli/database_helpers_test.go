package cli

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
	"github.com/swqa7697/data-mate/internal/database/postgres"
)

// Shared by command diagnostics and the PTY subprocess; no network or native keys.
type fixtureDatabase struct {
	requests    []database.ScopeRequest
	tested      []string
	closed      bool
	browseError bool
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
func (d *fixtureDatabase) BrowseScope(_ context.Context, _ database.Access, r database.ScopeRequest) (database.ScopePage, error) {
	d.requests = append(d.requests, r)
	if d.browseError {
		return database.ScopePage{}, database.Fail(contracts.ConnectFailed, "catalog fetch failed", true)
	}
	p := database.ScopePage{}
	if r.Schema == "" {
		if r.Search == "Dot" {
			p.Schemas = []string{"Dot.Schema"}
			return p, nil
		}
		start := 0
		if r.After != "" {
			start, _ = strconv.Atoi(strings.TrimPrefix(r.After, "schema"))
			start++
		}
		for i := start; i < min(5000, start+50); i++ {
			p.Schemas = append(p.Schemas, fmt.Sprintf("schema%04d", i))
		}
		if start+50 < 5000 {
			p.Next = p.Schemas[len(p.Schemas)-1]
		}
		return p, nil
	}
	if r.Schema == "Dot.Schema" {
		p.Tables = []database.Table{{Schema: r.Schema, Name: "a.b", Kind: "table"}, {Schema: r.Schema, Name: "parts", Kind: "partitioned_table"}}
		return p, nil
	}
	start := 0
	if r.After != "" {
		start, _ = strconv.Atoi(strings.TrimPrefix(r.After, "table"))
		start++
	}
	for i := start; i < min(5000, start+50); i++ {
		p.Tables = append(p.Tables, database.Table{Schema: r.Schema, Name: fmt.Sprintf("table%04d", i), Kind: "table"})
	}
	if start+50 < 5000 {
		p.Next = p.Tables[len(p.Tables)-1].Name
	}
	return p, nil
}
