package service

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/swqa7697/data-mate/internal/config"
)

// Owner is bounded nonsecret evidence of the job occupying the per-user slot.
type Owner struct {
	Environment  config.Environment `json:"environment"`
	Root         string             `json:"root"`
	Installation string             `json:"installation"`
	Executable   string             `json:"executable"`
	StopCommand  string             `json:"stop_command"`
}

// OwnerConflict identifies a verified foreign service without granting cleanup authority.
type OwnerConflict struct{ Owner Owner }

func (e *OwnerConflict) Error() string {
	return fmt.Sprintf("service is owned by %s at %q; stop it explicitly: %s", e.Owner.Environment, e.Owner.Root, e.Owner.StopCommand)
}
func (e *OwnerConflict) Unwrap() error { return ErrConflict }
func shellQuote(s string) string       { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

func (c *Controller) owner(ctx context.Context, j job) (*Owner, error) {
	if !j.Present || filepath.Base(j.Path) != "service.plist" || len(j.Path) > 4096 {
		return nil, ErrConflict
	}
	root, err := config.ResolveRoot(filepath.Dir(j.Path), "")
	if err != nil || !safePath(root) {
		return nil, ErrConflict
	}
	s, err := config.OpenLifecycle(ctx, root)
	if err != nil {
		root.Environment = config.Production
		s, err = config.OpenLifecycle(ctx, root)
	}
	if err != nil {
		return nil, ErrConflict
	}
	defer s.Close()
	// A lifecycle lease could deadlock two competing roots. Passive state access
	// verifies the published identity without taking another root's lifecycle.
	l, err := s.ReadLease(ctx)
	if err != nil {
		return nil, ErrConflict
	}
	defer l.Release()
	r, err := readRecord(l.Read, root, l.Identity())
	if err != nil || !matching(j, r) {
		return nil, ErrConflict
	}
	raw, err := l.Read("service.plist", 16384)
	if err != nil || !bytes.Equal(raw, plist(root, r)) {
		return nil, ErrConflict
	}
	hash, err := binaryHash(r.Executable)
	if err != nil || hash != r.Build.Fingerprint {
		return nil, ErrConflict
	}
	command := shellQuote(r.Executable) + " mcp stop"
	if root.Environment.Kind() == config.Development {
		command += " --root " + shellQuote(root.Path)
	}
	return &Owner{root.Environment.Kind(), root.Path, r.Identity.Installation, r.Executable, command}, nil
}

func (c *Controller) blocker(ctx context.Context) (*Owner, error) {
	j, err := c.launcher.Inspect(ctx, c.Root)
	if err != nil || !j.Present {
		return nil, err
	}
	if j.Path == filepath.Join(c.Root.Path, "service.plist") {
		return nil, nil
	}
	return c.owner(ctx, j)
}

func (c *Controller) inspectSelected(ctx context.Context) (job, error) {
	j, err := c.launcher.Inspect(ctx, c.Root)
	if err != nil || !j.Present || j.Path == filepath.Join(c.Root.Path, "service.plist") {
		return j, err
	}
	if _, err = c.owner(ctx, j); err != nil {
		return job{}, err
	}
	return job{}, nil
}
