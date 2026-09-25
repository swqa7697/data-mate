package distribution

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/swqa7697/data-mate/internal/agent"
	"github.com/swqa7697/data-mate/internal/config"
	"golang.org/x/sys/unix"
)

// ArtifactError reports a recorded cleanup path without exposing its contents.
type ArtifactError struct {
	Path  string
	Cause error
}

func (e *ArtifactError) Error() string {
	return fmt.Sprintf("cannot remove recorded artifact %q; check access and path ownership", e.Path)
}
func (e *ArtifactError) Unwrap() error { return e.Cause }

// helperMatches verifies recorded executable bytes without treating a previous
// mount's device/inode as execution authority. Native verification still applies.
func helperMatches(path string, expected File) bool {
	current, _, err := inspect(path)
	return err == nil && current.Target == "" && contentEqual(current, expected)
}

// PrepareHelper verifies and records a private temporary copy before execution.
// Existing retry helpers remain authoritative until a successful cleanup.
func (e *Engine) PrepareHelper(ctx context.Context) (string, error) {
	if err := e.verify(ctx); err != nil {
		return "", err
	}
	lock, err := e.acquire(ctx)
	if err != nil {
		return "", err
	}
	defer lock.close()
	inv, err := e.load()
	if err != nil {
		return "", err
	}
	if inv.Phase == "publishing" || inv.Phase == "committed" {
		return "", config.ErrPending
	}
	if inv.Helper != nil {
		if helperMatches(inv.Helper.Path, inv.Helper.File) {
			return inv.Helper.Path, nil
		}
		// A crash after unlinking the helper can be resumed by a verified bootstrap.
		if !absent(inv.Helper.Path) {
			return "", ErrConflict
		}
		dir := filepath.Dir(inv.Helper.Path)
		if !absent(dir) {
			if config.CheckPath(dir) != nil || os.Remove(dir) != nil {
				return "", ErrConflict
			}
		}
		inv.Helper = nil
		if err = e.save(inv); err != nil {
			return "", err
		}
	}
	_, raw, err := inspect(e.Candidate)
	if err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp("/private/tmp", "data-mate-cleanup-")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "data-mate")
	if err = writeNew(path, raw, 0700); err != nil {
		return "", err
	}
	file, _, err := inspect(path)
	if err != nil {
		return "", err
	}
	verify := e.Verify
	if verify == nil {
		verify = VerifyNative
	}
	if err = verify(ctx, path, e.Metadata); err != nil {
		_ = removeExact(path, file)
		_ = os.Remove(dir)
		return "", err
	}
	inv.Helper = &Artifact{path, file}
	if err = e.save(inv); err != nil {
		_ = removeExact(path, file)
		_ = os.Remove(dir)
		return "", err
	}
	return path, nil
}

// Preview lists only recorded production targets without starting a service.
func (e *Engine) Preview(ctx context.Context) ([]string, error) {
	inv, err := e.load()
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, a := range inv.Artifacts {
		out = append(out, a.Path)
	}
	for _, b := range inv.Blocks {
		out = append(out, b.Path+" (owned shell block)")
	}
	if !inv.Terminal && (inv.Phase == "installed" || inv.Phase == "retained") {
		paths, err := agent.New(e.Root).CleanupLocations(ctx)
		if err != nil {
			return nil, err
		}
		for _, path := range paths {
			out = append(out, path+" (owned agent registration)")
		}
	}
	for _, change := range inv.Changes {
		out = append(out, change.Path, change.Stage, change.Backup)
	}
	if inv.Helper != nil {
		out = append(out, inv.Helper.Path)
	}
	out = append(out, e.Root.Path+" (owned runtime; saved store retained unless --purge)")
	return out, nil
}

// Uninstall runs only from the recorded helper and retains retry authority on error.
func (e *Engine) Uninstall(ctx context.Context, purge bool) error {
	if err := e.verify(ctx); err != nil {
		return err
	}
	lock, err := e.acquire(ctx)
	if err != nil {
		return err
	}
	defer lock.close()
	inv, err := e.load()
	if err != nil {
		return err
	}
	if inv.Helper == nil || inv.Helper.Path != e.Candidate || !helperMatches(e.Candidate, inv.Helper.File) {
		return ErrConflict
	}
	if inv.Phase == "publishing" || inv.Phase == "committed" || (inv.Purge && !purge) {
		return config.ErrPending
	}
	if inv.Terminal {
		return e.finishTerminal(ctx, &inv, lock, nil)
	}
	inv.Purge = purge
	inv.Phase = "uninstalling"
	if purge {
		inv.Phase = "purging"
	}
	if err = e.save(inv); err != nil {
		return err
	}
	if store, err := config.OpenLifecycle(ctx, e.Root); err == nil {
		l, err := store.Lifecycle(ctx)
		if err == nil {
			err = l.SetDistributionPending(ctx, true)
			l.Release()
		}
		store.Close()
		if err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if e.Stop != nil {
		if err = e.Stop(ctx); err != nil {
			return err
		}
	}
	// Resume a shell edit interrupted after either rename using only its recorded
	// old/new bytes and identities. Never infer ownership from a marker alone.
	if err = e.finishShellRemoval(&inv); err != nil {
		return err
	}
	for len(inv.Blocks) != 0 {
		b := inv.Blocks[0]
		file, raw, err := inspect(b.Path)
		if err != nil {
			return err
		}
		if file.Target != "" || strings.Count(string(raw), b.Text) != 1 {
			return ErrConflict
		}
		remaining := []byte(strings.Replace(string(raw), b.Text, "", 1))
		nonce, err := config.NewID()
		if err != nil {
			return err
		}
		c := change{Path: b.Path, Stage: filepath.Join(filepath.Dir(b.Path), ".data-mate."+nonce), Backup: filepath.Join(filepath.Dir(b.Path), ".data-mate."+nonce+".rollback"), Before: &file, After: File{Hash: digest(remaining), Mode: file.Mode}, External: true}
		// An empty installer-created startup file is removed; others retain mode/bytes.
		if b.Created && len(remaining) == 0 {
			c.After.Hash = ""
		}
		inv.Changes = []change{c}
		if err = e.save(inv); err != nil {
			return err
		}
		if c.After.Hash != "" {
			if err = writeNew(c.Stage, remaining, os.FileMode(file.Mode)); err != nil {
				return err
			}
			f, _, err := inspect(c.Stage)
			if err != nil {
				return err
			}
			inv.Changes[0].After = f
			if err = e.save(inv); err != nil {
				return err
			}
		}
		if err = e.finishShellRemoval(&inv); err != nil {
			return err
		}
	}
	// The recorded path grants deletion authority even if an installed artifact
	// was edited or replaced. Check path safety before credential cleanup.
	for _, a := range inv.Artifacts {
		p, err := removalParent(a.Path)
		if err != nil {
			return &ArtifactError{Path: a.Path, Cause: err}
		}
		if p != nil {
			p.close()
		}
	}
	if e.Cleanup != nil {
		if err = e.Cleanup(ctx, purge); err != nil {
			return err
		}
	}
	for _, a := range inv.Artifacts {
		if err = removeRecorded(a.Path); err != nil {
			return err
		}
	}
	inv.Artifacts = []Artifact{}
	// Remove empty runtime directories while the final receipt still exists.
	for i := len(inv.Directories) - 1; i >= 0; i-- {
		d := inv.Directories[i]
		if d.Path == e.Root.Path || d.Path == filepath.Join(e.Home, ".local") || d.Path == filepath.Join(e.Home, ".local", "share") {
			continue
		}
		if err = removeDirectory(d); err != nil {
			return err
		}
	}
	if !purge {
		inv.Phase = "retained"
		if err = e.save(inv); err != nil {
			return err
		}
		store, err := config.OpenLifecycle(ctx, e.Root)
		if err != nil {
			return err
		}
		l, err := store.Lifecycle(ctx)
		if err == nil {
			err = l.SetDistributionPending(ctx, false)
			l.Release()
		}
		store.Close()
		if err != nil {
			return err
		}
	}

	if !purge {
		if err = removeHelper(inv.Helper); err != nil {
			return err
		}
		inv.Helper = nil
		return e.save(inv)
	}
	// A fixed account-home receipt survives removal of the data root and its
	// parents. The temporary helper and bootstrap can resume every terminal prefix.
	terminalRoot := e.Root
	terminalRoot.Path = e.Home
	terminalLock, err := acquireNamed(ctx, terminalRoot, ".data-mate-cleanup.lock", nil)
	if err != nil {
		return err
	}
	defer terminalLock.close()
	inv.Terminal = true
	if err = e.save(inv); err != nil {
		return err
	}
	if err = e.point("terminal-receipt"); err != nil {
		return err
	}
	return e.finishTerminal(ctx, &inv, terminalLock, lock)
}

func removeHelper(helper *Artifact) error {
	if helper == nil {
		return nil
	}
	if err := removeRecorded(helper.Path); err != nil {
		return err
	}
	dir := filepath.Dir(helper.Path)
	if !absent(dir) {
		if config.CheckPath(dir) != nil {
			return ErrConflict
		}
		if err := os.Remove(dir); err != nil {
			return err
		}
	}
	if !absent(helper.Path) || !absent(dir) {
		return ErrConflict
	}
	return nil
}

func (e *Engine) finishTerminal(ctx context.Context, inv *inventory, terminalLock, rootLock *distributionLease) error {
	if !absent(e.Root.Path) {
		if rootLock == nil {
			var err error
			rootLock, err = acquire(ctx, e.Root)
			if err != nil {
				return err
			}
			defer rootLock.close()
		}
		if err := rootLock.check(); err != nil {
			return err
		}
		if !absent(e.inventoryPath()) {
			file, raw, err := inspect(e.inventoryPath())
			var prior inventory
			if err != nil || config.DecodeStrict(raw, 1<<20, &prior) != nil || prior.Installation != inv.Installation || prior.Phase != "purging" {
				return ErrConflict
			}
			if err = removeExact(e.inventoryPath(), file); err != nil {
				return err
			}
		}
		if err := os.Remove(filepath.Join(e.Root.Path, "distribution.lock")); err != nil {
			return err
		}
	}
	for i := len(inv.Directories) - 1; i >= 0; i-- {
		if err := removeDirectory(inv.Directories[i]); err != nil {
			return err
		}
	}
	if err := e.point("terminal-root-removed"); err != nil {
		return err
	}
	if err := removeHelper(inv.Helper); err != nil {
		return err
	}
	if err := terminalLock.check(); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(e.Home, ".data-mate-cleanup.lock")); err != nil {
		return err
	}
	file, _, err := inspect(e.terminalPath())
	if err != nil {
		return err
	}
	return removeExact(e.terminalPath(), file)
}

func (e *Engine) directoryPath(path string) bool {
	for _, p := range []string{e.Root.Path, filepath.Join(e.Root.Path, "bin"), filepath.Join(e.Root.Path, "shell"), filepath.Join(e.Home, ".local"), filepath.Join(e.Home, ".local", "share"), filepath.Join(e.Home, ".local", "bin")} {
		if path == p {
			return true
		}
	}
	return false
}

func (e *Engine) finishShellRemoval(inv *inventory) error {
	if len(inv.Changes) == 0 {
		return nil
	}
	if len(inv.Changes) != 1 || len(inv.Blocks) == 0 {
		return ErrConflict
	}
	c := inv.Changes[0]
	if !c.External || c.Path != inv.Blocks[0].Path || c.Before == nil {
		return ErrConflict
	}
	if !absent(c.Path) && exact(c.Path, *c.Before) == nil {
		if c.After.Hash != "" && absent(c.Stage) {
			_, raw, err := inspect(c.Path)
			if err != nil {
				return err
			}
			remaining := []byte(strings.Replace(string(raw), inv.Blocks[0].Text, "", 1))
			if digest(remaining) != c.After.Hash {
				return ErrConflict
			}
			if err = writeNew(c.Stage, remaining, os.FileMode(c.After.Mode)); err != nil {
				return err
			}
		}
		if err := renameExact(c.Path, c.Backup, *c.Before); err != nil {
			return err
		}
	}
	if c.After.Hash != "" {
		if absent(c.Path) {
			if err := renameExact(c.Stage, c.Path, c.After); err != nil {
				return err
			}
		} else if exact(c.Path, c.After) != nil {
			return ErrConflict
		}
	} else if !absent(c.Path) {
		return ErrConflict
	}
	if err := removeExact(c.Backup, *c.Before); err != nil {
		return err
	}
	inv.Blocks = inv.Blocks[1:]
	inv.Changes = []change{}
	return e.save(*inv)
}

func removeDirectory(d Directory) error {
	if absent(d.Path) {
		return nil
	}
	if _, err := directoryIdentity(d.Path); err != nil {
		return ErrConflict
	}
	p, err := pinParent(d.Path)
	if err != nil {
		return err
	}
	defer p.close()
	if p.check() != nil {
		return ErrConflict
	}
	err = unix.Unlinkat(int(p.last().Fd()), filepath.Base(d.Path), unix.AT_REMOVEDIR)
	if errors.Is(err, unix.ENOTEMPTY) || errors.Is(err, unix.EEXIST) {
		return nil
	}
	if err != nil {
		return err
	}
	return p.last().Sync()
}
