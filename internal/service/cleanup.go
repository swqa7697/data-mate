package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/swqa7697/data-mate/internal/agent"
	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/vault"
)

// CleanupError reports safe stage and relative owned paths for a retry.
// The underlying cause is never printed by the public CLI.
type CleanupError struct {
	Stage     string
	Remaining []string
	Cause     error
}

func (e *CleanupError) Error() string { return "installation cleanup incomplete at " + e.Stage }
func (e *CleanupError) Unwrap() error { return e.Cause }

// Uninstall stops owned admission and removes registrations before taking the
// exclusive state lease. All stages retain lifecycle; external failures leave
// the binary and retry identity intact. The caller runs outside this root.
func (c *Controller) Uninstall(ctx context.Context, purge bool, keys vault.KeyProvider) (result error) {
	s, err := config.OpenLifecycle(ctx, c.Root)
	if errors.Is(err, os.ErrNotExist) {
		// No identity means no authority to delete anything in this directory.
		// Existing owned names are unresolved cleanup, never a successful no-op.
		remaining, inspectErr := config.RemainingOwned(c.Root)
		if inspectErr != nil || len(remaining) != 0 {
			return &CleanupError{Stage: "missing installation identity", Remaining: remaining, Cause: ErrState}
		}
		_, err = c.Stop(ctx)
		return err
	}
	if err != nil {
		return err
	}
	defer s.Close()
	l, err := s.Lifecycle(ctx)
	if err != nil {
		return err
	}
	defer l.Release()
	stage := "service shutdown"
	defer func() {
		if result == nil {
			return
		}
		remaining, _ := config.RemainingOwned(c.Root)
		result = &CleanupError{Stage: stage, Remaining: remaining, Cause: result}
	}()
	if l.Identity().Purging && !purge {
		return config.ErrPurging
	}
	r, err := readRecord(l.Read, c.Root, l.Identity())
	if errors.Is(err, os.ErrNotExist) {
		j, e := c.inspectSelected(ctx)
		if e != nil {
			return e
		}
		if j.Present {
			return ErrConflict
		}
	} else if err != nil {
		return err
	} else if err = c.stopLocked(ctx, l, r); err != nil {
		return err
	}
	// Startup may publish runtime identity before its service record. A live
	// orphan listener or unrecognized runtime contents must remain retryable.
	stage = "runtime cleanup"
	if err = c.clearRuntime(record{Identity: installation(c.Root, l.Identity())}); err != nil {
		return err
	}
	if _, err = os.Lstat(filepath.Join(socketDir(c.Root), "identity.json")); !errors.Is(err, os.ErrNotExist) {
		return ErrConflict
	}
	stage = "agent registrations"
	m := c.Agents
	if m == nil {
		m = agent.New(c.Root)
	}
	if err = m.RemoveOwned(ctx, l); err != nil {
		return err
	}
	for _, path := range []string{"registrations.json.tmp", "registrations.json", "service.json.tmp", "service.plist.tmp", "service.plist"} {
		if err = l.Remove(path); err != nil {
			return err
		}
	}
	stage = "state drain"
	state, err := l.CleanupLease(ctx)
	if err != nil {
		return err
	}
	defer state.Release()
	if err = ctx.Err(); err != nil {
		return err
	}
	if purge {
		stage = "credential purge"
		if keys == nil {
			keys = vault.Keychain{}
		}
		if err = vault.New(s, keys).PurgeLocked(ctx, state); err != nil {
			return err
		}
		stage = "owned files and final identity"
		return state.FinishPurge()
	}
	stage = "installed executable"
	if err = state.RemoveBinary(); err != nil {
		return err
	}
	return state.TrimBinaryDirectory()
}
