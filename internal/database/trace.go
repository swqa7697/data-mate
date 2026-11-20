package database

import (
	"errors"

	"github.com/swqa7697/data-mate/internal/contracts"
)

// Trace belongs to one synchronous diagnostic operation. It records successful
// stages only; Readiness attaches the final safe failure after cleanup completes.
// A nil Trace ignores every call, so ordinary operations can share stage hooks.
type Trace struct {
	stage  string
	stages []Stage
	// Version is the server version observed by the version stage.
	Version int
}

// Start marks the stage that a subsequent failure belongs to.
func (t *Trace) Start(stage string) {
	if t != nil {
		t.stage = stage
	}
}

// Pass records a reached stage once.
func (t *Trace) Pass(stage string) {
	if t == nil {
		return
	}
	for _, s := range t.stages {
		if s.Stage == stage {
			return
		}
	}
	t.stages = append(t.stages, Stage{Stage: stage, OK: true})
}

// Readiness completes the trace: success passes the final read_only stage, and
// failure attaches a safe error to the current stage. Later stages are omitted.
func (t *Trace) Readiness(tls bool, err error) (Readiness, error) {
	out := Readiness{TLS: tls}
	if err == nil {
		out.ServerVersion = t.Version
		t.Pass("read_only")
	} else {
		var safe *Error
		if !errors.As(err, &safe) {
			safe = &Error{Failure: contracts.Failure{Code: contracts.ConnectFailed, Message: "database readiness check failed", Retryable: true}}
		}
		t.stages = append(t.stages, Stage{Stage: t.stage, Error: &safe.Failure})
	}
	out.Stage, out.Stages = t.stage, t.stages
	return out, err
}
