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
	name string
}

func (n named) ValidateProfile(config.Profile) error { return errors.New(n.name) }
func (n named) Test(context.Context, Access) (Readiness, error) {
	return Readiness{Stage: n.name}, nil
}
func (n named) DescribeTable(context.Context, Access, config.Table) (any, error) { return n.name, nil }

type resources struct{ invalidated, closed int }

func (r *resources) Invalidate(string) { r.invalidated++ }
func (r *resources) Close()            { r.closed++ }

// Every profile reaches only its own driver; unknown drivers fail as invalid
// configuration, and lifecycle calls reach the shared resources once.
func TestRouter(t *testing.T) {
	shared := &resources{}
	r := NewRouter(shared, map[string]Operations{"postgres": named{name: "postgres"}, "mysql": named{name: "mysql"}})
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
	r.Invalidate("id")
	r.Close()
	if shared.invalidated != 1 || shared.closed != 1 {
		t.Fatal("lifecycle", shared)
	}
}
