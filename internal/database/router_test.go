package database

import (
	"context"
	"errors"
	"testing"

	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
)

// named answers every operation with its own name, so dispatch is observable.
type named struct {
	Operations
	RelationalOperations
	name string
}

func (n named) ValidateProfile(config.Profile) error { return errors.New(n.name) }
func (n named) Test(context.Context, Access) (Readiness, error) {
	return Readiness{Stage: n.name}, nil
}
func (n named) DescribeTable(context.Context, Access, config.Table) (any, error) { return n.name, nil }

type keyDriver struct {
	Operations
	KeyOperations
}

func (keyDriver) ListKeys(context.Context, Access, KeyPageRequest) (KeyPage, error) {
	return KeyPage{Driver: "redis"}, nil
}

type documentDriver struct {
	Operations
	DocumentOperations
}

func (documentDriver) FindDocuments(context.Context, Access, FindRequest) (DocumentResult, error) {
	return DocumentResult{Driver: "mongodb"}, nil
}

type resources struct{ invalidated, closed int }

func (r *resources) Invalidate(string) { r.invalidated++ }
func (r *resources) Close()            { r.closed++ }

// Every profile reaches only its own driver; unknown drivers fail as invalid
// configuration, and lifecycle calls reach the shared resources once.
func TestRouter(t *testing.T) {
	shared := &resources{}
	r := NewRouter(shared, map[string]Operations{"postgres": named{name: "postgres"}, "mysql": named{name: "mysql"}, "redis": keyDriver{}, "mongodb": documentDriver{}})
	for _, driver := range []string{"postgres", "mysql"} {
		a := NewAccess(config.Profile{Driver: driver}, "")
		ready, err := r.Test(t.Context(), a)
		described, _ := r.DescribeTable(t.Context(), a, config.Table{})
		if err != nil || ready.Stage != driver || described != driver || r.ValidateProfile(a.Profile).Error() != driver {
			t.Fatalf("%s dispatch: %v %v", driver, ready, described)
		}
	}
	unknown := NewAccess(config.Profile{Driver: "oracle"}, "")
	_, err := r.Query(t.Context(), unknown, QueryRequest{})
	var e *Error
	if !errors.As(err, &e) || e.Code != contracts.ConfigInvalid || !errors.As(r.ValidateProfile(unknown.Profile), &e) {
		t.Fatal("unknown driver", err)
	}
	redis := NewAccess(config.Profile{Driver: "redis"}, "")
	page, err := r.ListKeys(t.Context(), redis, KeyPageRequest{})
	if err != nil || page.Driver != "redis" {
		t.Fatal("key dispatch", err)
	}
	mongodb := NewAccess(config.Profile{Driver: "mongodb"}, "")
	documents, err := r.FindDocuments(t.Context(), mongodb, FindRequest{})
	if err != nil || documents.Driver != "mongodb" {
		t.Fatal("document dispatch", err)
	}
	// Capabilities are exclusive in both directions: SQL and key tools refuse
	// the other families, document tools refuse Redis, and MongoDB refuses SQL.
	_, redisErr := r.Query(t.Context(), redis, QueryRequest{})
	_, sqlErr := r.ReadKey(t.Context(), NewAccess(config.Profile{Driver: "mysql"}, ""), KeyReadRequest{})
	_, mongoErr := r.Query(t.Context(), mongodb, QueryRequest{})
	_, collectionErr := r.ListCollections(t.Context(), redis, CollectionPageRequest{})
	for _, err := range []error{redisErr, sqlErr, mongoErr, collectionErr} {
		if !errors.As(err, &e) || e.Code != contracts.QueryUnsupported {
			t.Fatal("incompatible tool", err)
		}
	}
	r.Invalidate("id")
	r.Close()
	if shared.invalidated != 1 || shared.closed != 1 {
		t.Fatal("lifecycle", shared)
	}
}
