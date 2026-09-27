package distribution

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/swqa7697/data-mate/internal/config"
	"golang.org/x/mod/semver"
)

// Legacy fields remain decodable for installations made by published releases.
type change struct {
	Path     string `json:"path"`
	Stage    string `json:"stage"`
	Backup   string `json:"backup"`
	Before   *File  `json:"before,omitempty"`
	After    File   `json:"after"`
	External bool   `json:"external"`
}
type inventory struct {
	Version         int                `json:"version"`
	Installation    string             `json:"installation"`
	Root            string             `json:"root"`
	Environment     config.Environment `json:"environment"`
	Release         string             `json:"release"`
	Phase           string             `json:"phase,omitempty"`
	Artifacts       []Artifact         `json:"artifacts,omitempty"`
	Blocks          []ShellBlock       `json:"blocks"`
	Changes         []change           `json:"changes,omitempty"`
	PreviousRelease string             `json:"previous_release,omitempty"`
	PreviousBlocks  []ShellBlock       `json:"previous_blocks,omitempty"`
	Directories     []Directory        `json:"directories,omitempty"`
	Helper          *Artifact          `json:"helper,omitempty"`
	HelperSignature *Artifact          `json:"helper_signature,omitempty"`
	Purge           bool               `json:"purge,omitempty"`
	Terminal        bool               `json:"terminal,omitempty"`
}

type desired struct {
	path string
	raw  []byte
	mode os.FileMode
}
type Directory struct {
	Path   string `json:"path"`
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}

// Engine owns ordinary, repeatable file installation. Credentials and service
// lifecycle work remain behind their existing package boundaries.
type Engine struct {
	Changed    bool
	Home       string
	Root       config.Root
	Metadata   Metadata
	Candidate  string
	Shell      string
	ZDotDir    string
	NoShell    bool
	Completion func(string) ([]byte, error)
	Publish    func(context.Context, string) error
	Cleanup    func(context.Context, bool, func() error) error
	Warn       func(string)
	Fault      func(string) error
}

func (e *Engine) warning(err error) {
	if err != nil && e.Warn != nil {
		e.Warn(err.Error())
	}
}
func (e *Engine) point(name string) error {
	if e.Fault != nil {
		return e.Fault(name)
	}
	return nil
}
func (e *Engine) inventoryPath() string { return filepath.Join(e.Root.Path, "distribution.json") }
func (e *Engine) terminalPath() string  { return filepath.Join(e.Home, ".data-mate-cleanup.json") }
func (e *Engine) commandPath() string   { return filepath.Join(e.Home, ".local", "bin", "data-mate") }
func (e *Engine) load() (inventory, error) {
	var inv inventory
	read := func(path string) ([]byte, error) {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return io.ReadAll(io.LimitReader(f, (1<<20)+1))
	}
	raw, err := read(e.inventoryPath())
	if os.IsNotExist(err) {
		raw, err = read(e.terminalPath())
	}
	if err != nil {
		return inv, err
	}
	if config.DecodeStrict(raw, 1<<20, &inv) != nil || inv.Version != 1 || inv.Root != e.Root.Path || inv.Environment != config.Production || !config.ValidUUID(inv.Installation) {
		return inv, ErrConflict
	}
	if len(inv.Changes) > 32 || len(inv.Blocks) > 8 || len(inv.PreviousBlocks) > 8 || len(inv.Artifacts) > 32 || len(inv.Directories) > 16 {
		return inv, ErrConflict
	}
	return inv, nil
}
func (e *Engine) save(inv inventory) error {
	raw, err := json.Marshal(inv)
	if err != nil {
		return err
	}
	return writeAtomic(e.inventoryPath(), raw, 0600)
}
func (e *Engine) artifactPath(path string) bool {
	for _, p := range e.runtimePaths() {
		if p == path {
			return true
		}
	}
	return false
}
func (e *Engine) runtimePaths() []string {
	paths := []string{e.commandPath(), config.ExecutablePath(e.Root), config.ExecutablePath(e.Root) + ".sig"}
	for _, shell := range []string{"bash", "zsh"} {
		for _, name := range []string{"loader.", "completion."} {
			paths = append(paths, filepath.Join(e.Root.Path, "shell", name+shell))
		}
	}
	return paths
}

func (e *Engine) validate() error {
	root, err := config.ProductionRoot(e.Home)
	if err != nil || root != e.Root || !e.Metadata.valid() {
		return ErrConflict
	}
	return nil
}

// Install downloads nothing and never re-verifies code that is already running.
// The bootstrap/updater authenticates candidates before invoking this entrypoint.
func (e *Engine) Install(ctx context.Context) error {
	e.Changed = false
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := e.validate(); err != nil {
		return err
	}
	inv, err := e.load()
	fresh := os.IsNotExist(err)
	if err != nil && !fresh {
		return err
	}
	if inv.Purge || inv.Phase == "purging" {
		return config.ErrPurging
	}
	if stable.MatchString(inv.Release) && semver.Compare("v"+e.Metadata.Version, "v"+inv.Release) < 0 {
		return ErrRelease
	}
	if err = e.checkCommand(); err != nil {
		return err
	}
	for _, dir := range []string{e.Root.Path, filepath.Dir(config.ExecutablePath(e.Root)), filepath.Dir(e.commandPath())} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return &DirectoryError{Path: dir, Cause: err}
		}
	}
	lock, err := acquire(ctx, e.Root)
	if err != nil {
		return err
	}
	defer lock.close()
	// Reload after the operation lock; another installer may have completed.
	inv, err = e.load()
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if inv.Purge || inv.Phase == "purging" {
		return config.ErrPurging
	}
	if stable.MatchString(inv.Release) && semver.Compare("v"+e.Metadata.Version, "v"+inv.Release) < 0 {
		return ErrRelease
	}
	if err = e.checkCommand(); err != nil {
		return err
	}
	if inv.Installation != "" {
		store, err := config.OpenLifecycle(ctx, e.Root)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil {
			l, err := store.Lifecycle(ctx)
			if err != nil {
				store.Close()
				return err
			}
			id := l.Identity()
			l.Release()
			store.Close()
			if id.ID != inv.Installation {
				return ErrConflict
			}
			if id.Purging {
				return config.ErrPurging
			}
		}
	}
	staged, err := stageCopy(e.Candidate, filepath.Dir(config.ExecutablePath(e.Root)), 0700)
	if err != nil {
		return err
	}
	defer os.Remove(staged)
	if err = e.point("before-publish"); err != nil {
		return err
	}
	if err = e.recoverLegacyShell(inv); err != nil {
		return err
	}
	publish := e.Publish
	if publish == nil {
		publish = func(ctx context.Context, path string) error {
			store, err := config.Open(ctx, e.Root, nil)
			if err != nil {
				return err
			}
			defer store.Close()
			l, err := store.Lifecycle(ctx)
			if err != nil {
				return err
			}
			defer l.Release()
			if err = l.InstallBinary(ctx, filepath.Base(path)); err != nil {
				return err
			}
			if l.Identity().Pending {
				return l.SetDistributionPending(ctx, false)
			}
			return nil
		}
	}
	if err = publish(ctx, staged); err != nil {
		return err
	}
	e.Changed = true
	store, err := config.OpenLifecycle(ctx, e.Root)
	if err != nil {
		return err
	}
	l, err := store.Lifecycle(ctx)
	if err != nil {
		store.Close()
		return err
	}
	id := l.Identity()
	l.Release()
	store.Close()
	// Publish a small receipt immediately so reruns do not depend on shell setup.
	if inv.Installation != "" && inv.Installation != id.ID {
		return ErrConflict
	}
	if err = e.cleanLegacy(inv); err != nil {
		return err
	}
	if err = removeFile(config.ExecutablePath(e.Root) + ".sig"); err != nil {
		return err
	}
	inv = inventory{Version: 1, Installation: id.ID, Root: e.Root.Path, Environment: config.Production, Release: e.Metadata.Version, Blocks: append([]ShellBlock{}, append(inv.Blocks, inv.PreviousBlocks...)...)}
	if err = e.save(inv); err != nil {
		return err
	}
	if err = e.point("published:binary"); err != nil {
		return err
	}
	if err = e.checkCommand(); err != nil {
		return err
	}
	if err = replaceLink(config.ExecutablePath(e.Root), e.commandPath()); err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Join(e.Root.Path, "shell"), 0700); err != nil {
		e.warning(err)
	} else {
		for _, shell := range []string{"bash", "zsh"} {
			raw, err := e.Completion(shell)
			if err == nil {
				err = writeAtomic(filepath.Join(e.Root.Path, "shell", "completion."+shell), raw, 0600)
			}
			if err == nil {
				err = writeAtomic(filepath.Join(e.Root.Path, "shell", "loader."+shell), loader(e.Home, e.Root.Path, shell), 0600)
			}
			e.warning(err)
		}
	}
	if changes, blocks, err := e.shellChanges(inv.Blocks); err != nil {
		e.warning(err)
	} else {
		// Record paths before writing blocks; cleanup can safely inspect a missing block.
		inv.Blocks = blocks
		if err = e.save(inv); err != nil {
			return err
		}
		for _, change := range changes {
			e.warning(writeShell(change.path, change.raw, change.mode))
		}
	}
	return nil
}

// cleanLegacy only removes explicitly recorded temporary names in known runtime
// directories. Old filesystem identity snapshots are not execution authority.
func (e *Engine) cleanLegacy(inv inventory) error {
	for _, c := range inv.Changes {
		allowed := e.artifactPath(c.Path)
		if c.External {
			for _, b := range append(inv.Blocks, inv.PreviousBlocks...) {
				allowed = allowed || b.Path == c.Path && validShellPath(b.Path)
			}
		}
		if !allowed {
			continue
		}
		for _, path := range []string{c.Stage, c.Backup} {
			if filepath.Dir(path) != filepath.Dir(c.Path) || !strings.HasPrefix(filepath.Base(path), ".data-mate.") {
				continue
			}
			if err := removeFile(path); err != nil {
				return err
			}
		}
	}
	if inv.Helper != nil {
		dir := filepath.Dir(inv.Helper.Path)
		if filepath.Dir(dir) == config.RuntimeTemp() && strings.HasPrefix(filepath.Base(dir), "data-mate-cleanup-") && filepath.Base(inv.Helper.Path) == "data-mate" {
			if err := removeFile(inv.Helper.Path); err != nil {
				return err
			}
			if inv.HelperSignature != nil && inv.HelperSignature.Path == inv.Helper.Path+".sig" {
				if err := removeFile(inv.HelperSignature.Path); err != nil {
					return err
				}
			}
			if err := removeEmpty(dir); err != nil {
				return err
			}
		}
	}
	if inv.Terminal {
		if err := removeFile(e.terminalPath()); err != nil {
			return err
		}
		return removeFile(filepath.Join(e.Home, ".data-mate-cleanup.lock"))
	}
	return nil
}

func (e *Engine) checkCommand() error {
	target, err := os.Readlink(e.commandPath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil || resolveLink(e.commandPath(), target) != config.ExecutablePath(e.Root) {
		cause := error(ErrConflict)
		if err != nil {
			cause = err
		}
		return &ArtifactError{Path: e.commandPath(), Cause: cause}
	}
	return nil
}

// A published older installer could have moved a startup file to its backup
// before interruption. Recover its unrelated content before removing old staging.
func (e *Engine) recoverLegacyShell(inv inventory) error {
	for _, c := range inv.Changes {
		if !c.External || !validShellPath(c.Path) || filepath.Dir(c.Backup) != filepath.Dir(c.Path) || !strings.HasPrefix(filepath.Base(c.Backup), ".data-mate.") || !strings.HasSuffix(c.Backup, ".rollback") || !absent(c.Path) || absent(c.Backup) {
			continue
		}
		recorded := false
		for _, b := range append(inv.Blocks, inv.PreviousBlocks...) {
			recorded = recorded || b.Path == c.Path
		}
		if !recorded {
			continue
		}
		if err := os.Rename(c.Backup, c.Path); err != nil {
			return err
		}
	}
	return nil
}
