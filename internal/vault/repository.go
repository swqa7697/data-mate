package vault

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/transport"
)

// Repository owns profile/vault publication ordering. No method prints secrets.
// All form collection and confirmation belong outside this boundary (P2).
type Repository struct {
	store *config.Store
	keys  KeyProvider
	fault func(string) error
}

// New binds persistence to a native or explicitly supplied test key provider.
func New(store *config.Store, keys KeyProvider) *Repository {
	return &Repository{store: store, keys: keys}
}
func (r *Repository) point(op string) error {
	if r.fault != nil {
		return r.fault(op)
	}
	return nil
}

// Snapshot lists validated nonsecret profiles without invoking the key provider.
func (r *Repository) Snapshot(ctx context.Context) (config.Profiles, config.Revision, error) {
	l, err := r.store.ReadLease(ctx)
	if err != nil {
		return config.Profiles{}, "", err
	}
	defer l.Release()
	return l.ProfileSnapshot()
}

// Credential resolves a reference against the fresh snapshot under the caller's
// active state lease. Keep this lease through all work using the returned secret.
func (r *Repository) Credential(ctx context.Context, l *config.Lease, connectionID string) (Secrets, error) {
	p, _, err := l.ProfileSnapshot()
	if err != nil {
		return Secrets{}, err
	}
	var profile *config.Profile
	for i := range p.Connections {
		if p.Connections[i].ID == connectionID {
			profile = &p.Connections[i]
			break
		}
	}
	if profile == nil {
		return Secrets{}, ErrCredentialMissing
	}
	if profile.CredentialRef == "" {
		return Secrets{}, nil
	}
	v, err := r.load(ctx, l, false)
	if err != nil {
		return Secrets{}, err
	}
	defer clear(v.key)
	b, ok := v.document.Bundles[profile.CredentialRef]
	if !ok {
		return Secrets{}, ErrCredentialMissing
	}
	if b.ConnectionID != profile.ID {
		return Secrets{}, ErrBinding
	}
	return b.Secrets, nil
}

// Mutation is a confirmed complete profile snapshot. Replacements is keyed by
// connection ID and always receives fresh bundle references. Omitted entries keep
// their references; clearing a reference explicitly clears those credentials.
type Mutation struct {
	Expected     config.Revision
	Profiles     config.Profiles
	Replacements map[string]Secrets
	HostKey      *transport.HostKey
}

// Outcome distinguishes a committed profile change from a cleanup failure.
// PublicationUncertain means rename may have occurred but durability failed;
// reload before presenting another preview. It is never reported as success.
type Outcome struct {
	Revision                                            config.Revision
	ProfilesSaved, PublicationUncertain, CleanupPending bool
}

// Apply waits for old snapshots, verifies the preview, publishes vault then
// profiles, and reconciles orphans. Invalid input and stale previews change nothing.
func (r *Repository) Apply(ctx context.Context, m Mutation) (Outcome, error) {
	var outcome Outcome
	b, err := json.Marshal(m.Profiles)
	if err != nil {
		return outcome, errors.New("invalid profiles")
	}
	p, _, err := config.DecodeProfiles(bytes.NewReader(b))
	if err != nil {
		return outcome, err
	}
	for id, s := range m.Replacements {
		found := false
		for _, c := range p.Connections {
			if c.ID == id {
				found = true
				break
			}
		}
		if !found || !s.valid() {
			return outcome, errors.New("invalid credential replacement")
		}
	}
	l, err := r.store.WriteLease(ctx)
	if err != nil {
		return outcome, err
	}
	defer l.Release()
	previous, current, err := l.ProfileSnapshot()
	if err != nil {
		return outcome, err
	}
	if current != m.Expected {
		return outcome, config.ErrRevision
	}
	if m.HostKey != nil {
		if err = ctx.Err(); err != nil {
			return outcome, err
		}
		if err = transport.SaveHostKey(l, *m.HostKey); err != nil {
			return outcome, err
		}
	}

	// Deletion and unchanged credential references can publish profiles before
	// retrieving secrets. New references must be checked against bundle ownership.
	needBefore := len(m.Replacements) > 0
	oldRefs := map[string]string{}
	for _, c := range previous.Connections {
		oldRefs[c.ID] = c.CredentialRef
	}
	for _, c := range p.Connections {
		if c.CredentialRef != "" && oldRefs[c.ID] != c.CredentialRef {
			needBefore = true
		}
	}
	var v *openedVault
	if needBefore {
		v, err = r.load(ctx, l, len(m.Replacements) > 0)
		if err != nil {
			return outcome, err
		}
		defer clear(v.key)
		for _, c := range p.Connections {
			if _, replacing := m.Replacements[c.ID]; replacing {
				continue
			}
			if b, ok := v.document.Bundles[c.CredentialRef]; ok && b.ConnectionID != c.ID {
				return outcome, ErrBinding
			}
		}
	}

	if len(m.Replacements) > 0 {
		// Reconcile only pre-existing orphans here. Old referenced bundles survive until
		// the new profile document is durable, including after an interrupted edit.
		retained := config.Profiles{Connections: append(append([]config.Profile(nil), previous.Connections...), p.Connections...)}
		reconcile(v.document.Bundles, retained)
		for i := range p.Connections {
			c := &p.Connections[i]
			s, ok := m.Replacements[c.ID]
			if !ok {
				continue
			}
			ref, e := config.NewID()
			if e != nil {
				return outcome, e
			}
			v.document.Bundles[ref] = bundle{ConnectionID: c.ID, Secrets: s}
			c.CredentialRef = ref
		}
		if err = r.publish(ctx, l, v); err != nil {
			return outcome, err
		}
	}
	if err = ctx.Err(); err != nil {
		return outcome, err
	}
	outcome.Revision, err = l.SaveProfiles(p, current)
	if err != nil {
		outcome.PublicationUncertain = true
		return outcome, err
	}
	outcome.ProfilesSaved = true
	if err = r.point("after-profile-publication"); err != nil {
		outcome.CleanupPending = true
		return outcome, err
	}

	if v == nil {
		v, err = r.load(ctx, l, false)
		if err != nil {
			outcome.CleanupPending = true
			return outcome, errors.New("profiles saved; credential cleanup failed")
		}
		defer clear(v.key)
	}
	if reconcile(v.document.Bundles, p) {
		if err = r.publish(ctx, l, v); err != nil {
			outcome.CleanupPending = true
			return outcome, errors.New("profiles saved; credential cleanup failed")
		}
	}
	return outcome, nil
}
func reconcile(bundles map[string]bundle, p config.Profiles) bool {
	refs := map[string]bool{}
	for _, c := range p.Connections {
		refs[c.CredentialRef] = true
	}
	changed := false
	for ref := range bundles {
		if !refs[ref] {
			delete(bundles, ref)
			changed = true
		}
	}
	return changed
}

// PurgeCredentials is the resumable inner coordinator, not an uninstall command.
// P11 must stop the owned service/remove registrations before invoking it, then
// remove runtime/identity/locks last under the outer lifecycle protocol. On any
// failure this leaves identity and a runnable retry path; it never deletes bin.
func (r *Repository) PurgeCredentials(ctx context.Context) error {
	l, err := r.store.PurgeLease(ctx)
	if err != nil {
		return err
	}
	defer l.Release()
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = l.BeginPurge(); err != nil {
		return err
	}
	if err = r.point("before-key-delete"); err != nil {
		return err
	}
	if err = r.keys.Delete(ctx, l.Identity().RootDigest); err != nil {
		return providerError(err)
	}
	if err = r.point("after-key-delete"); err != nil {
		return err
	}
	// Usage and identity survive every unsuccessful exact-key deletion. No vault
	// decoding is required to purge a corrupt vault by explicit user intent.
	for _, path := range []string{"config/connections.json.tmp", "config/connections.json", "state/vault.json.tmp", "state/vault.json", "state/vault-usage.json.tmp", "state/vault-usage.json"} {
		if err = ctx.Err(); err != nil {
			return err
		}
		if err = l.Remove(path); err != nil {
			return err
		}
	}
	return nil
}

// ValidateExisting authenticates the existing vault and all referenced bundles
// before service readiness. Empty installations neither load nor create a key.
func (r *Repository) ValidateExisting(ctx context.Context, l *config.Lease) error {
	p, _, err := l.ProfileSnapshot()
	if err != nil {
		return err
	}
	v, err := r.load(ctx, l, false)
	if err != nil {
		return err
	}
	defer clear(v.key)
	for _, c := range p.Connections {
		if c.CredentialRef == "" {
			continue
		}
		b, ok := v.document.Bundles[c.CredentialRef]
		if !ok {
			return ErrCredentialMissing
		}
		if b.ConnectionID != c.ID {
			return ErrBinding
		}
	}
	return nil
}
