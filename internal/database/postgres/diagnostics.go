package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
)

// Trace belongs to one synchronous operation. It records successful stages only;
// the caller attaches the final safe failure after transaction cleanup completes.
type diagnosticTrace struct {
	stage   string
	stages  []database.Stage
	version int
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

// Test always audits a fresh short-lived pool, independently of cached approval.
func (d *Driver) Test(ctx context.Context, a database.Access) (database.Readiness, error) {
	d.mu.Lock()
	closed := d.closed
	d.mu.Unlock()
	if closed {
		return database.Readiness{}, database.Fail(contracts.ServiceUnavailable, "database driver is closed", false)
	}
	fresh, err := New()
	if err != nil {
		return database.Readiness{}, err
	}
	defer fresh.Close()
	trace := &diagnosticTrace{}
	out := database.Readiness{TLS: a.Profile.Transport.TLS.Mode == "verify-full"}
	err = func() error {
		trace.start("config")
		a, rev, err := normalized(a)
		if err != nil {
			return err
		}
		trace.pass("config")
		ctx, cancel := context.WithTimeout(ctx, time.Duration(a.Profile.Limits.QueryTimeoutMS)*time.Millisecond)
		defer cancel()
		trace.start("dial")
		leave, err := d.admit(ctx)
		if err != nil {
			return err
		}
		defer leave()
		_, _, release, err := fresh.checkout(ctx, a, rev, trace)
		if err != nil {
			return err
		}
		defer release(true)
		if err = ctx.Err(); err != nil {
			return safeError(err)
		}
		out.ServerVersion = trace.version
		return nil
	}()
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
