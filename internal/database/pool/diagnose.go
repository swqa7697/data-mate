package pool

import (
	"context"

	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
)

// Check opens and approves one connection from fresh pools, recording reached
// stages in trace, and returns its release function.
type Check func(ctx context.Context, fresh *Registry, a database.Access, rev config.Revision, trace *database.Trace) (release func(healthy bool), err error)

// Diagnose runs staged readiness diagnostics. It always audits a fresh,
// isolated pool, independently of cached approval, under shared admission.
func Diagnose(ctx context.Context, r *Registry, a database.Access, check Check) (database.Readiness, error) {
	if r.Closed() {
		return database.Readiness{}, database.Fail(contracts.ServiceUnavailable, "database driver is closed", false)
	}
	fresh, err := r.Isolated()
	if err != nil {
		return database.Readiness{}, err
	}
	defer fresh.Close()
	trace := &database.Trace{}
	tls := a.Profile.Transport.TLS.Mode == "verify-full"
	err = func() error {
		trace.Start("config")
		a, rev, err := database.Normalize(a)
		if err != nil {
			return err
		}
		trace.Pass("config")
		ctx, cancel := context.WithTimeout(ctx, database.Timeout(a))
		defer cancel()
		trace.Start("dial")
		leave, err := r.Admit(ctx)
		if err != nil {
			return err
		}
		defer leave()
		release, err := check(ctx, fresh, a, rev, trace)
		if err != nil {
			return err
		}
		defer release(true)
		if err = ctx.Err(); err != nil {
			return database.CommonError(err)
		}
		return nil
	}()
	return trace.Readiness(tls, err)
}
