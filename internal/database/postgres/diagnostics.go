package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
)

// Trace belongs to one synchronous operation. It records successful stages only;
// the caller attaches the final safe failure after transaction cleanup completes.
type diagnosticTrace struct {
	stage  string
	stages []database.Stage
}

func (t *diagnosticTrace) start(stage string) {
	if t != nil {
		t.stage = stage
	}
}
func (t *diagnosticTrace) pass(stage string) {
	if t == nil {
		return
	}
	for _, s := range t.stages {
		if s.Stage == stage {
			return
		}
	}
	t.stages = append(t.stages, database.Stage{Stage: stage, OK: true})
}

// Test verifies connection and read-only transaction readiness, not account grants.
func (d *Driver) Test(ctx context.Context, a database.Access) (database.Readiness, error) {
	trace := &diagnosticTrace{}
	out := database.Readiness{TLS: a.Profile.Transport.TLS.Mode == "verify-full"}
	err := d.runObserved(ctx, a, trace, func(ctx context.Context, tx pgx.Tx, version int) error {
		out.ServerVersion = version
		return nil
	})
	if err == nil {
		trace.pass("read_only")
	} else {
		var safe *database.Error
		if !errors.As(err, &safe) {
			safe = &database.Error{Failure: contracts.Failure{Code: contracts.ConnectFailed, Message: "database readiness check failed", Retryable: true}}
		}
		trace.stages = append(trace.stages, database.Stage{Stage: trace.stage, Error: &safe.Failure})
	}
	out.Stage, out.Stages = trace.stage, trace.stages
	return out, err
}
