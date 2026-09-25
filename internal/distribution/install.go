package distribution

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/swqa7697/data-mate/internal/config"
	"golang.org/x/mod/semver"
	"golang.org/x/sys/unix"
)

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
	Phase           string             `json:"phase"`
	Artifacts       []Artifact         `json:"artifacts"`
	Blocks          []ShellBlock       `json:"blocks"`
	Changes         []change           `json:"changes"`
	PreviousRelease string             `json:"previous_release"`
	PreviousBlocks  []ShellBlock       `json:"previous_blocks"`
	Directories     []Directory        `json:"directories"`
	Helper          *Artifact          `json:"helper,omitempty"`
	Purge           bool               `json:"purge"`
	Terminal        bool               `json:"terminal"`
}
type desired struct {
	path     string
	raw      []byte
	mode     os.FileMode
	target   string
	external bool
}

// Engine receives the fixed account paths and narrow lifecycle/native boundaries.
// No command-line or environment option can supply Home in production.
type Engine struct {
	// Changed reports successful publication; a healthy same-version install leaves it false.
	Changed        bool
	Home           string
	Root           config.Root
	Metadata       Metadata
	Candidate      string
	Shell          string
	ZDotDir        string
	NoShell        bool
	Completion     func(string) ([]byte, error)
	Stop           func(context.Context) error
	Cleanup        func(context.Context, bool) error
	Verify         func(context.Context, string, Metadata) error
	Fault          func(string) error
	checkInstalled func(context.Context, string, Metadata) error
}

func (e *Engine) point(name string) error {
	if e.Fault != nil {
		return e.Fault(name)
	}
	return nil
}
func (e *Engine) inventoryPath() string { return filepath.Join(e.Root.Path, "distribution.json") }
func (e *Engine) terminalPath() string  { return filepath.Join(e.Home, ".data-mate-cleanup.json") }
func (e *Engine) acquire(ctx context.Context) (*distributionLease, error) {
	if !absent(e.terminalPath()) {
		root := e.Root
		root.Path = e.Home
		return acquireNamed(ctx, root, ".data-mate-cleanup.lock", nil)
	}
	return acquire(ctx, e.Root)
}
func (e *Engine) load() (inventory, error) {
	var inv inventory
	path := e.inventoryPath()
	terminal := !absent(e.terminalPath())
	if terminal {
		path = e.terminalPath()
	}
	f, raw, err := inspect(path)
	if err != nil {
		return inv, err
	}
	if f.Target != "" || f.Mode != 0600 || config.DecodeStrict(raw, 1<<20, &inv) != nil || inv.Version != 1 || inv.Root != e.Root.Path || inv.Environment != config.Production || !config.ValidUUID(inv.Installation) {
		return inv, ErrConflict
	}
	if inv.Terminal != terminal || (terminal && (!inv.Purge || inv.Phase != "purging")) {
		return inv, ErrConflict
	}
	switch inv.Phase {
	case "installed", "publishing", "committed", "uninstalling", "retained", "purging":
	default:
		return inv, ErrConflict
	}
	if len(inv.Artifacts) > 32 || len(inv.Changes) > 32 || len(inv.Blocks) > 8 || len(inv.Directories) > 16 {
		return inv, ErrConflict
	}
	// Exact recorded locations supply cleanup authority; paths are never discovered
	// by scanning the account. Each operation still revalidates ownership and bytes.
	for _, d := range inv.Directories {
		if !e.directoryPath(d.Path) || d.Inode == 0 {
			return inv, ErrConflict
		}
	}
	if inv.Helper != nil {
		dir := filepath.Dir(inv.Helper.Path)
		if filepath.Dir(dir) != "/private/tmp" || !strings.HasPrefix(filepath.Base(dir), "data-mate-cleanup-") || filepath.Base(inv.Helper.Path) != "data-mate" {
			return inv, ErrConflict
		}
	}
	seen := map[string]bool{}
	for _, a := range inv.Artifacts {
		if !e.artifactPath(a.Path) || seen[a.Path] {
			return inv, ErrConflict
		}
		seen[a.Path] = true
	}
	for _, c := range inv.Changes {
		if !filepath.IsAbs(c.Path) || filepath.Clean(c.Path) != c.Path || (!c.External && !e.artifactPath(c.Path)) || filepath.Dir(c.Stage) != filepath.Dir(c.Path) || filepath.Dir(c.Backup) != filepath.Dir(c.Path) || !strings.HasPrefix(filepath.Base(c.Stage), ".data-mate.") || c.Backup != c.Stage+".rollback" {
			return inv, ErrConflict
		}
	}
	for _, b := range inv.Blocks {
		if !filepath.IsAbs(b.Path) || !strings.Contains(b.Text, "# >>> Data Mate >>>") || len(b.Text) > 8192 {
			return inv, ErrConflict
		}
	}
	return inv, nil
}
func (e *Engine) artifactPath(path string) bool {
	if path == filepath.Join(e.Home, ".local", "bin", "data-mate") || path == config.ExecutablePath(e.Root) {
		return true
	}
	for _, shell := range []string{"bash", "zsh"} {
		for _, name := range []string{"loader.", "completion."} {
			if path == filepath.Join(e.Root.Path, "shell", name+shell) {
				return true
			}
		}
	}
	return false
}
func (e *Engine) save(inv inventory) error {
	raw, err := json.Marshal(inv)
	if err != nil || len(raw) > 1<<20 {
		return ErrConflict
	}
	path := e.inventoryPath()
	if inv.Terminal {
		path = e.terminalPath()
	}
	tmp := path + ".tmp"
	// A stale fully-written publication sibling is recoverable only if it decodes
	// to the same installation; an unknown sibling is never overwritten.
	if !absent(tmp) {
		f, b, er := inspect(tmp)
		var pending inventory
		if er != nil || config.DecodeStrict(b, 1<<20, &pending) != nil || pending.Installation != inv.Installation || pending.Root != inv.Root {
			return ErrConflict
		}
		if er = removeExact(tmp, f); er != nil {
			return er
		}
	}
	var before *File
	if f, _, er := inspect(path); er == nil {
		before = &f
	} else if !os.IsNotExist(er) {
		return er
	}
	if err = writeNew(tmp, raw, 0600); err != nil {
		return err
	}
	if before != nil {
		if exact(path, *before) != nil {
			return ErrConflict
		}
	} else if !absent(path) {
		return ErrConflict
	}
	if err = replaceExact(tmp, path, before); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

type Directory struct {
	Path   string `json:"path"`
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}

func directoryIdentity(path string) (Directory, error) {
	var st unix.Stat_t
	if config.CheckPath(path) != nil || unix.Lstat(path, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Uid != uint32(os.Geteuid()) {
		return Directory{}, ErrConflict
	}
	return Directory{path, uint64(st.Dev), st.Ino}, nil
}
func privateDirs(paths []string) ([]Directory, error) {
	made := []Directory{}
	for _, path := range paths {
		if config.CheckPath(path) != nil {
			return made, ErrConflict
		}
		if absent(path) {
			if err := os.Mkdir(path, 0700); err != nil {
				if !os.IsExist(err) {
					return made, err
				}
			} else {
				id, e := directoryIdentity(path)
				if e != nil {
					return made, e
				}
				made = append(made, id)
				if err = syncDir(filepath.Dir(path)); err != nil {
					return made, err
				}
			}
		}
	}
	return made, nil
}
func (e *Engine) verify(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !e.Metadata.valid() || e.Metadata.StoreSchema != 1 || e.Metadata.InventorySchema != 1 {
		return ErrRelease
	}
	root, err := config.ProductionRoot(e.Home)
	if err != nil || root != e.Root {
		return ErrConflict
	}
	verify := e.Verify
	if verify == nil {
		verify = VerifyNative
	}
	return verify(ctx, e.Candidate, e.Metadata)
}

// Install is the sole publication path for bootstrap, reinstall, and upgrade.
func (e *Engine) Install(ctx context.Context) (result error) {
	e.Changed = false
	if err := e.verify(ctx); err != nil {
		return err
	}
	if !absent(e.terminalPath()) {
		return config.ErrPending
	}
	// Refuse unrelated command collisions and unsafe shell edits before creating
	// any installation state. Repeat these checks after acquiring ownership.
	preflight, preflightErr := e.load()
	if preflightErr != nil && !os.IsNotExist(preflightErr) {
		return preflightErr
	}
	if !absent(filepath.Join(e.Home, ".local", "bin", "data-mate")) && !slices.ContainsFunc(preflight.Artifacts, func(a Artifact) bool { return a.Path == filepath.Join(e.Home, ".local", "bin", "data-mate") }) && !((preflight.Phase == "publishing" || preflight.Phase == "committed") && slices.ContainsFunc(preflight.Changes, func(c change) bool { return c.Path == filepath.Join(e.Home, ".local", "bin", "data-mate") })) {
		return ErrConflict
	}
	if preflight.Phase != "publishing" && preflight.Phase != "committed" {
		if _, _, err := e.shellChanges(preflight.Blocks); err != nil {
			return err
		}
	}
	candidate, raw, err := inspect(e.Candidate)
	if err != nil || candidate.Target != "" {
		return ErrRelease
	}
	var space unix.Statfs_t
	if unix.Statfs(e.Home, &space) != nil || uint64(space.Bavail)*uint64(space.Bsize) < uint64(len(raw))*3+(16<<20) {
		return ErrRelease
	}
	dirs, err := privateDirs([]string{filepath.Join(e.Home, ".local"), filepath.Join(e.Home, ".local", "share"), e.Root.Path})
	if err != nil {
		return err
	}
	lock, err := acquire(ctx, e.Root)
	if err != nil {
		return err
	}
	defer lock.close()
	inv, err := e.load()
	fresh := os.IsNotExist(err)
	if !fresh && inv.Terminal {
		return config.ErrPending
	}
	if err != nil && !fresh {
		return err
	}
	if !fresh && (inv.Phase == "purging" || inv.Phase == "uninstalling") {
		return config.ErrPending
	}
	if !fresh && stable.MatchString(inv.Release) && semver.Compare("v"+e.Metadata.Version, "v"+inv.Release) < 0 {
		return ErrRelease
	}
	// Open preserves compatible store UUID/key account. Pending roots are reopened
	// for recovery only, never through ordinary state initialization.
	var store *config.Store
	if fresh {
		store, err = config.Open(ctx, e.Root, nil)
	} else {
		store, err = config.OpenLifecycle(ctx, e.Root)
	}
	if err != nil {
		return err
	}
	defer store.Close()
	l, err := store.Lifecycle(ctx)
	if err != nil {
		return err
	}
	id := l.Identity()
	if fresh {
		inv = inventory{Version: 1, Installation: id.ID, Root: e.Root.Path, Environment: config.Production, Phase: "retained", Artifacts: []Artifact{}, Blocks: []ShellBlock{}, Changes: []change{}, PreviousBlocks: []ShellBlock{}, Directories: dirs}
		if err = e.save(inv); err != nil {
			l.Release()
			return err
		}
	} else if inv.Installation != id.ID {
		l.Release()
		return ErrConflict
	}
	if inv.Phase == "publishing" || inv.Phase == "committed" {
		if err = e.recover(&inv); err != nil {
			l.Release()
			return err
		}
		if err = l.SetDistributionPending(ctx, false); err != nil {
			l.Release()
			return err
		}
	}
	l.Release()
	if _, _, err = config.Preview(ctx, e.Root); err != nil {
		return err
	}
	for _, a := range inv.Artifacts {
		if exact(a.Path, a.File) != nil {
			return ErrConflict
		}
	}
	shellChanges, blocks, err := e.shellChanges(inv.Blocks)
	if err != nil {
		return err
	}
	if inv.Release == e.Metadata.Version && inv.Phase == "installed" && len(shellChanges) == 0 {
		return nil
	}
	more, err := privateDirs([]string{filepath.Join(e.Root.Path, "bin"), filepath.Join(e.Root.Path, "shell"), filepath.Join(e.Home, ".local", "bin")})
	if err != nil {
		return err
	}
	for _, dir := range more {
		inv.Directories = slices.DeleteFunc(inv.Directories, func(old Directory) bool { return old.Path == dir.Path })
		inv.Directories = append(inv.Directories, dir)
	}
	desiredFiles := []desired{{path: config.ExecutablePath(e.Root), raw: raw, mode: 0700}, {path: filepath.Join(e.Home, ".local", "bin", "data-mate"), target: config.ExecutablePath(e.Root)}}
	for _, shell := range []string{"bash", "zsh"} {
		script, err := e.Completion(shell)
		if err != nil {
			return err
		}
		desiredFiles = append(desiredFiles, desired{path: filepath.Join(e.Root.Path, "shell", "completion."+shell), raw: script, mode: 0600}, desired{path: filepath.Join(e.Root.Path, "shell", "loader."+shell), raw: loader(e.Home, e.Root.Path, shell), mode: 0600})
	}
	desiredFiles = append(desiredFiles, shellChanges...)
	nonce, err := config.NewID()
	if err != nil {
		return err
	}
	inv.Changes = []change{}
	for i, d := range desiredFiles {
		after := File{Hash: digest(d.raw), Mode: uint32(d.mode)}
		if d.target != "" {
			after = File{Target: d.target, Mode: 0755}
		}
		c := change{Path: d.path, Stage: filepath.Join(filepath.Dir(d.path), ".data-mate."+nonce+"-"+itoa(i)), Backup: filepath.Join(filepath.Dir(d.path), ".data-mate."+nonce+"-"+itoa(i)+".rollback"), After: after, External: d.external}
		f, _, err := inspect(d.path)
		if err == nil {
			if !d.external && !slices.ContainsFunc(inv.Artifacts, func(a Artifact) bool { return a.Path == d.path && a.File == f }) {
				return ErrConflict
			}
			c.Before = &f
		} else if !os.IsNotExist(err) {
			return err
		}
		inv.Changes = append(inv.Changes, c)
	}
	inv.PreviousRelease, inv.PreviousBlocks = inv.Release, inv.Blocks
	inv.Release, inv.Blocks, inv.Phase = e.Metadata.Version, blocks, "publishing"
	if err = e.save(inv); err != nil {
		return err
	}
	l, err = store.Lifecycle(ctx)
	if err != nil {
		return err
	}
	err = l.SetDistributionPending(ctx, true)
	l.Release()
	if err != nil {
		return err
	}
	if err = e.point("intent"); err != nil {
		return err
	}
	if e.Stop != nil {
		if err = e.Stop(ctx); err != nil {
			return err
		}
	}
	l, err = store.Lifecycle(ctx)
	if err != nil {
		return err
	}
	defer l.Release()
	if err = lock.check(); err != nil {
		return err
	}
	defer func() {
		if result != nil && inv.Phase == "publishing" {
			if e.recover(&inv) == nil {
				_ = l.SetDistributionPending(context.WithoutCancel(ctx), false)
			}
		}
	}()
	for i := range inv.Changes {
		c := &inv.Changes[i]
		d := desiredFiles[i]
		if err = ctx.Err(); err != nil {
			return err
		}
		if d.target != "" {
			err = symlinkNew(d.target, c.Stage)
		} else {
			err = writeNew(c.Stage, d.raw, d.mode)
		}
		if err != nil {
			return err
		}
		actual, _, err := inspect(c.Stage)
		if err != nil {
			return err
		}
		c.After = actual
		if err = e.save(inv); err != nil {
			return err
		}
		if c.Before != nil {
			if err = renameExact(c.Path, c.Backup, *c.Before); err != nil {
				return err
			}
		}
		if c.Path == config.ExecutablePath(e.Root) {
			err = l.InstallBinary(ctx, filepath.Base(c.Stage))
		} else {
			err = renameExact(c.Stage, c.Path, c.After)
		}
		if err != nil {
			return err
		}
		if err = e.point("published:" + filepath.Base(c.Path)); err != nil {
			return err
		}
	}
	// Verify the installed bytes before discarding rollback authority.
	if exact(config.ExecutablePath(e.Root), inv.Changes[0].After) != nil {
		return ErrRelease
	}
	check := e.checkInstalled
	if check == nil {
		check = func(ctx context.Context, path string, expected Metadata) error {
			raw, err := command(ctx, path, "__release-metadata")
			var actual Metadata
			if err != nil || config.DecodeStrict(raw, 4096, &actual) != nil || actual != expected {
				return ErrRelease
			}
			return nil
		}
	}
	if err = check(ctx, config.ExecutablePath(e.Root), e.Metadata); err != nil {
		return err
	}
	inv.Phase = "committed"
	if err = e.save(inv); err != nil {
		return err
	}
	if err = e.point("committed"); err != nil {
		return err
	}
	if err = e.recover(&inv); err != nil {
		return err
	}
	if err := l.SetDistributionPending(ctx, false); err != nil {
		return err
	}
	e.Changed = true
	return nil
}

func (e *Engine) recover(inv *inventory) error {
	if inv.Phase == "publishing" {
		for i := len(inv.Changes) - 1; i >= 0; i-- {
			c := inv.Changes[i]
			if !(c.Before != nil && exact(c.Path, *c.Before) == nil) && !absent(c.Path) && exact(c.Path, c.After) == nil {
				if err := removeExact(c.Path, c.After); err != nil {
					return err
				}
			}
			if c.Before != nil {
				if absent(c.Path) {
					if err := renameExact(c.Backup, c.Path, *c.Before); err != nil {
						return err
					}
				} else if exact(c.Path, *c.Before) != nil {
					return ErrConflict
				}
			} else if !absent(c.Path) {
				return ErrConflict
			}
			if err := removeExact(c.Stage, c.After); err != nil {
				return err
			}
		}
		inv.Release, inv.Blocks = inv.PreviousRelease, inv.PreviousBlocks
		inv.Phase = "installed"
		if inv.Release == "" {
			inv.Phase = "retained"
		}
	} else if inv.Phase == "committed" {
		artifacts := []Artifact{}
		for _, c := range inv.Changes {
			if exact(c.Path, c.After) != nil {
				return ErrConflict
			}
			if c.Before != nil {
				if err := removeExact(c.Backup, *c.Before); err != nil {
					return err
				}
			}
			if !c.External {
				artifacts = append(artifacts, Artifact{c.Path, c.After})
			}
		}
		inv.Artifacts, inv.Phase = artifacts, "installed"
	}
	inv.Changes, inv.PreviousBlocks, inv.PreviousRelease = []change{}, []ShellBlock{}, ""
	return e.save(*inv)
}

func itoa(n int) string { return strconv.Itoa(n) }
