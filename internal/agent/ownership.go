package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"

	"github.com/swqa7697/data-mate/internal/config"
)

const recordPath = "state/registrations.json"

type ownedEntry struct {
	Agent       string   `json:"agent"`
	Config      string   `json:"config"`
	Name        string   `json:"name"`
	Command     string   `json:"command"`
	Args        []string `json:"args"`
	Fingerprint string   `json:"fingerprint"`
	Phase       string   `json:"phase"`
}
type ownership struct {
	Version      int          `json:"version"`
	Installation string       `json:"installation"`
	Root         string       `json:"root"`
	Digest       string       `json:"digest"`
	Entries      []ownedEntry `json:"entries"`
}

func (m *Manager) load(l *config.LifecycleLease) (ownership, error) {
	return m.readOwnership(l.Read, l.Identity())
}
func (m *Manager) readOwnership(read func(string, int) ([]byte, error), id config.Identity) (ownership, error) {
	want := ownership{1, id.ID, m.root.Path, m.root.Digest, []ownedEntry{}}
	if id.RootDigest != m.root.Digest {
		return want, ErrInspection
	}
	b, err := read(recordPath, 16384)
	if errors.Is(err, os.ErrNotExist) {
		return want, nil
	}
	if err != nil {
		return want, ErrInspection
	}
	var o ownership
	if config.DecodeStrict(b, 16384, &o) != nil || o.Version != 1 || o.Installation != want.Installation || o.Root != want.Root || o.Digest != want.Digest || len(o.Entries) > 2 {
		return want, ErrInspection
	}
	seen := map[string]bool{}
	for _, e := range o.Entries {
		if e.Fingerprint != fingerprint(m.desired(adapter{name: e.Agent})) {
			return want, ErrInspection
		}
		if (e.Agent != "codex" && e.Agent != "claude") || seen[e.Agent] || e.Name != Name(m.root) || !filepath.IsAbs(e.Config) || e.Command != filepath.Join(m.root.Path, "bin/data-mate") || !slices.Equal(e.Args, []string{"mcp", "bridge", "--root", m.root.Path}) || len(e.Fingerprint) != 64 || (e.Phase != "intent" && e.Phase != "owned") {
			return want, ErrInspection
		}
		seen[e.Agent] = true
	}
	return o, nil
}
func save(l *config.LifecycleLease, o ownership) error {
	b, err := json.Marshal(o)
	if err != nil {
		return ErrInspection
	}
	return l.Replace(recordPath, b)
}
func (o *ownership) find(agent string) int {
	for i, e := range o.Entries {
		if e.Agent == agent {
			return i
		}
	}
	return -1
}

// Ensure runs only after service readiness with the lifecycle lease held. Each
// adapter succeeds independently; failures never undo a healthy service or peer.
func (m *Manager) Ensure(ctx context.Context, l *config.LifecycleLease) ([]Status, error) {
	o, err := m.load(l)
	if err != nil || l.Identity().Purging {
		return []Status{{"codex", "failed"}, {"claude", "failed"}}, ErrPartial
	}
	var out []Status
	var result error
	for _, a := range m.adapters {
		state, e := m.ensure(ctx, l, &o, a)
		out = append(out, Status{a.name, state})
		if e != nil {
			result = ErrPartial
		}
	}
	if ctx.Err() != nil {
		return out, ctx.Err()
	}
	return out, result
}
func (m *Manager) ensure(ctx context.Context, l *config.LifecycleLease, o *ownership, a adapter) (string, error) {
	if a.executable == "" {
		return "unavailable", nil
	}
	if ctx.Err() != nil {
		return "failed", ctx.Err()
	}
	before, err := a.inspect(Name(m.root))
	if err != nil {
		return "failed", err
	}
	state := m.state(a, before)
	i := o.find(a.name)
	if i >= 0 && o.Entries[i].Config != a.path {
		return "conflict", ErrConflict
	}
	if state == "conflict" {
		return state, ErrConflict
	}
	if state == "disabled" {
		return state, ErrPartial
	}
	if state == "ready" {
		// Matching unowned registrations are usable but never adopted for removal.
		if i >= 0 {
			if o.Entries[i].Fingerprint != fingerprint(before.entry) {
				return "conflict", ErrConflict
			}
			if o.Entries[i].Phase == "owned" {
				return "ready", nil
			}
			o.Entries[i].Phase = "owned"
			if err = save(l, *o); err != nil {
				return "failed", err
			}
		}
		return "ready", nil
	}
	desired := m.desired(a)
	intent := ownedEntry{Agent: a.name, Config: a.path, Name: Name(m.root), Command: desired["command"].(string), Args: desired["args"].([]string), Fingerprint: fingerprint(desired), Phase: "intent"}
	if i < 0 {
		o.Entries = append(o.Entries, intent)
		i = len(o.Entries) - 1
	} else {
		o.Entries[i] = intent
	}
	if err = save(l, *o); err != nil {
		return "failed", err
	}
	// Reinspect immediately before writing to avoid overwriting an intervening entry.
	latest, err := a.inspect(Name(m.root))
	if err != nil {
		return "failed", err
	}
	if latest.entry != nil || fingerprint(latest.other) != fingerprint(before.other) {
		return "conflict", ErrConflict
	}
	runErr := a.run(ctx, a.executable, a.addArgs(Name(m.root), desired), a.env)
	after, err := a.inspect(Name(m.root))
	if err != nil {
		return "failed", err
	}
	if !preserved(before.other, after.other) {
		return "conflict", ErrConflict
	}
	if fingerprint(after.entry) != intent.Fingerprint {
		return "failed", ErrInspection
	}
	if runErr != nil {
		return "failed", runErr
	} // Durable intent reconciles a successful-but-interrupted add.
	o.Entries[i].Phase = "owned"
	if err = save(l, *o); err != nil {
		return "failed", err
	}
	return "ready", nil
}

// RemoveOwned is the P11 cleanup boundary. It never removes an unrecorded,
// relocated or edited entry, and retains retry authority after any failure.
func (m *Manager) RemoveOwned(ctx context.Context, l *config.LifecycleLease) error {
	o, err := m.load(l)
	if err != nil {
		return err
	}
	for _, a := range m.adapters {
		i := o.find(a.name)
		if i < 0 {
			continue
		}
		e := o.Entries[i]
		if a.path != e.Config {
			return ErrConflict
		}
		before, err := a.inspect(e.Name)
		if err != nil {
			return err
		}
		if before.entry != nil {
			if fingerprint(before.entry) != e.Fingerprint {
				return ErrConflict
			}
			if a.executable == "" {
				return ErrInspection
			}
			if err = a.run(ctx, a.executable, a.removeArgs(e.Name), a.env); err != nil {
				return err
			}
			after, err := a.inspect(e.Name)
			if err != nil || after.entry != nil || !preserved(before.other, after.other) {
				return ErrConflict
			}
		}
		o.Entries = append(o.Entries[:i], o.Entries[i+1:]...)
		if err = save(l, o); err != nil {
			return err
		}
	}
	return nil
}
