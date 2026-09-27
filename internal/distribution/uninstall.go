package distribution

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/swqa7697/data-mate/internal/agent"
	"github.com/swqa7697/data-mate/internal/config"
)

type ArtifactError struct {
	Path  string
	Cause error
}

func (e *ArtifactError) Error() string {
	return fmt.Sprintf("cannot update installation file %q: %v", e.Path, fileCause(e.Cause))
}
func (e *ArtifactError) Unwrap() error { return e.Cause }

// Preview is passive and tolerates installations without a distribution receipt.
func (e *Engine) Preview(ctx context.Context) ([]string, error) {
	inv, err := e.load()
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	out := e.runtimePaths()
	for _, b := range inv.Blocks {
		if validShellPath(b.Path) {
			out = append(out, b.Path+" (Data Mate shell block)")
		}
	}
	paths, err := agent.New(e.Root).CleanupLocations(ctx)
	if err != nil && !os.IsNotExist(err) && !errors.Is(err, config.ErrPending) && !errors.Is(err, config.ErrPurging) {
		return nil, err
	}
	for _, path := range paths {
		out = append(out, path+" (owned agent registration)")
	}
	return append(out, e.Root.Path+" (saved connections and credentials retained unless --purge)"), nil
}

// Uninstall cleans known paths in order, in the current process. External
// service, agent and credential failures keep the executable available to retry.
func (e *Engine) Uninstall(ctx context.Context, purge bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if absent(e.Root.Path) {
		inv, err := e.load()
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if !purge {
			return config.ErrPurging
		}
		return e.cleanLegacy(inv)
	}
	lock, err := acquire(ctx, e.Root)
	if err != nil {
		return err
	}
	defer lock.close()
	inv, err := e.load()
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if (inv.Purge || inv.Phase == "purging") && !purge {
		return config.ErrPurging
	}
	cleanupFiles := func() error {
		if err := e.recoverLegacyShell(inv); err != nil {
			return err
		}
		e.removeShell(append(inv.Blocks, inv.PreviousBlocks...))
		// The public entry belongs to us only while it points at this installation.
		target, err := os.Readlink(e.commandPath())
		if err == nil && resolveLink(e.commandPath(), target) == config.ExecutablePath(e.Root) {
			if err = removeFile(e.commandPath()); err != nil {
				return err
			}
		} else if err != nil && !os.IsNotExist(err) {
			e.warning(fmt.Errorf("command entry %q is no longer a Data Mate symlink; preserved", e.commandPath()))
		}
		for _, path := range e.runtimePaths()[2:] {
			if err = removeFile(path); err != nil {
				return err
			}
		}
		if err = removeEmpty(filepath.Join(e.Root.Path, "shell")); err != nil {
			return err
		}
		if err = e.cleanLegacy(inv); err != nil {
			return err
		}
		return nil
	}
	if e.Cleanup == nil {
		return fmt.Errorf("installation cleanup is unavailable")
	}
	if err = e.Cleanup(ctx, purge, cleanupFiles); err != nil {
		return err
	}
	if !purge {
		inv.Changes = nil
		inv.Artifacts = nil
		inv.Directories = nil
		inv.Helper = nil
		inv.HelperSignature = nil
		inv.PreviousBlocks = nil
		inv.PreviousRelease = ""
		inv.Phase = ""
		inv.Terminal = false
		if inv.Installation != "" {
			return e.save(inv)
		}
		return nil
	}
	if err = removeFile(e.inventoryPath()); err != nil {
		return err
	}
	// Unlinking the held lock is last: waiters verify the inode after acquisition.
	if err = removeFile(filepath.Join(e.Root.Path, "distribution.lock")); err != nil {
		return err
	}
	return removeEmpty(e.Root.Path)
}
func resolveLink(path, target string) string {
	if filepath.IsAbs(target) {
		return filepath.Clean(target)
	}
	return filepath.Clean(filepath.Join(filepath.Dir(path), target))
}
