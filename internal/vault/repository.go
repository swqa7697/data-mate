package vault

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"

	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/transport"
	"github.com/tink-crypto/tink-go/v2/tink"
)

// Repository is owned by the service; callers acquire keysets before state leases.
type Repository struct {
	store       *config.Store
	keys        KeyProvider
	fault       func(string) error
	gate        chan struct{}
	mu          sync.Mutex
	primitive   tink.AEAD
	fingerprint string
	closed      bool
	state       string
}

func New(store *config.Store, keys KeyProvider) *Repository {
	return &Repository{store: store, keys: keys, gate: make(chan struct{}, 1), state: "locked"}
}
func (r *Repository) point(op string) error {
	if r.fault != nil {
		return r.fault(op)
	}
	return nil
}
func (r *Repository) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	r.primitive = nil
	r.fingerprint = ""
}
func (r *Repository) State() string { r.mu.Lock(); defer r.mu.Unlock(); return r.state }
func (r *Repository) Snapshot(ctx context.Context) (config.Profiles, config.Revision, error) {
	l, e := r.store.ReadLease(ctx)
	if e != nil {
		return config.Profiles{}, "", e
	}
	defer l.Release()
	return l.ProfileSnapshot()
}

// Unlock performs OS access without any state lease or SQLite transaction.
// Initialization holds lifecycle, with short state leases around durable metadata.
func (r *Repository) Unlock(ctx context.Context, create bool, expected config.Revision) (result error) {
	select {
	case r.gate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-r.gate }()
	r.mu.Lock()
	ready := r.primitive != nil
	closed := r.closed
	r.mu.Unlock()
	if closed {
		return ErrUnavailable
	}
	if ready {
		return ctx.Err()
	}
	var lifecycle *config.LifecycleLease
	if create {
		var err error
		lifecycle, err = r.store.Lifecycle(ctx)
		if err != nil {
			return err
		}
		defer lifecycle.Release()
	}
	l, err := r.store.ReadLease(ctx)
	if err != nil {
		return err
	}
	k, err := l.Keyset()
	id := l.Identity()
	_, rev, pe := l.ProfileSnapshot()
	l.Release()
	if err != nil {
		return ErrRepair
	}
	if pe != nil {
		return pe
	}
	if expected != "" && expected != rev {
		return config.ErrRevision
	}
	r.mu.Lock()
	if k.Phase == "" {
		r.state = "absent"
	}
	r.mu.Unlock()
	if k.Phase == "" && !create {
		return nil
	}
	defer func() {
		if result != nil {
			r.mu.Lock()
			r.state = "unavailable"
			if errors.Is(result, ErrLocked) || errors.Is(result, ErrDenied) {
				r.state = "locked"
			}
			r.mu.Unlock()
		}
	}()
	var raw []byte
	if k.Phase != "" {
		raw, err = providerCall(ctx, func() ([]byte, error) { return r.keys.Load(ctx, id.KeyAccount) })
		if err != nil && !(errors.Is(err, ErrMissing) && k.Phase == "intent" && create && k.Reserved == 0) {
			return providerError(err)
		}
		if err == nil && fingerprint(raw) != k.Fingerprint {
			clear(raw)
			return ErrRepair
		}
	}
	if raw == nil {
		if !create {
			return ErrRepair
		}
		raw, err = generateKeyset()
		if err != nil {
			return err
		}
		defer clear(raw)
		k = config.KeysetMetadata{Account: id.KeyAccount, Phase: "intent", Fingerprint: fingerprint(raw)}
		l, err = r.store.WriteLease(ctx)
		if err != nil {
			return err
		}
		_, rev, err = l.ProfileSnapshot()
		if err == nil && expected != "" && rev != expected {
			err = config.ErrRevision
		}
		if err == nil {
			err = l.SaveKeyset(k)
		}
		l.Release()
		if err != nil {
			return err
		}
		if err = r.point("before-key-create"); err != nil {
			return err
		}
		candidate := bytes.Clone(raw)
		loaded, e := providerCall(ctx, func() ([]byte, error) {
			defer clear(candidate)
			return r.keys.CreateIfAbsent(ctx, id.KeyAccount, candidate)
		})
		if e != nil {
			return providerError(e)
		}
		defer clear(loaded)
		if fingerprint(loaded) != k.Fingerprint {
			return ErrRepair
		}
		if err = r.point("after-key-create"); err != nil {
			return err
		}
	} else {
		defer clear(raw)
	}
	primitive, err := parseKeyset(raw)
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if k.Phase == "intent" {
		if !create {
			return ErrRepair
		}
		l, err = r.store.WriteLease(ctx)
		if err != nil {
			return err
		}
		_, rev, err = l.ProfileSnapshot()
		if err == nil && expected != "" && rev != expected {
			err = config.ErrRevision
		}
		if err == nil {
			k.Phase = "ready"
			err = l.SaveKeyset(k)
		}
		l.Release()
		if err != nil {
			return err
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrUnavailable
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	r.primitive = primitive
	r.fingerprint = k.Fingerprint
	r.state = "ready"
	return nil
}

// providerCall bounds the caller even when native prompting is not interruptible.
// The unbuffered rendezvous gives late results exactly one owner, which clears them.
func providerCall(ctx context.Context, fn func() ([]byte, error)) ([]byte, error) {
	type result struct {
		b []byte
		e error
	}
	done := make(chan result)
	go func() {
		b, e := fn()
		select {
		case done <- result{b, e}:
		case <-ctx.Done():
			clear(b)
		}
	}()
	select {
	case r := <-done:
		if ctx.Err() != nil {
			clear(r.b)
			return nil, ctx.Err()
		}
		return r.b, r.e
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (r *Repository) cipher(l *config.Lease) (tink.AEAD, error) {
	k, err := l.Keyset()
	if err != nil || k.Phase != "ready" {
		return nil, ErrRepair
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.primitive == nil {
		return nil, ErrLocked
	}
	if r.closed || r.fingerprint != k.Fingerprint {
		return nil, ErrRepair
	}
	return r.primitive, nil
}
func (r *Repository) Credential(ctx context.Context, l *config.Lease, id string) (Secrets, error) {
	if err := ctx.Err(); err != nil {
		return Secrets{}, err
	}
	p, _, err := l.ProfileSnapshot()
	if err != nil {
		return Secrets{}, err
	}
	for _, profile := range p.Connections {
		if profile.ID == id {
			return r.credential(l, profile)
		}
	}
	return Secrets{}, ErrCredentialMissing
}
func (r *Repository) credential(l *config.Lease, p config.Profile) (Secrets, error) {
	if p.CredentialRef == "" {
		return Secrets{}, nil
	}
	b, err := l.Bundle(p.CredentialRef)
	if errors.Is(err, os.ErrNotExist) {
		return Secrets{}, ErrCredentialMissing
	}
	if err != nil {
		return Secrets{}, err
	}
	if b.ConnectionID != p.ID || b.Version != 1 || len(b.Data) > MaxPlaintextBytes+33 {
		return Secrets{}, ErrBinding
	}
	primitive, err := r.cipher(l)
	if err != nil {
		return Secrets{}, err
	}
	plain, err := primitive.Decrypt(b.Data, aad(l.Identity(), p, b.Version))
	if err != nil {
		return Secrets{}, ErrRepair
	}
	defer clear(plain)
	var decoded bundle
	if config.DecodeStrict(plain, MaxPlaintextBytes, &decoded) != nil {
		return Secrets{}, ErrRepair
	}
	return decoded.Secrets, nil
}

// Mutation contains a confirmed profile snapshot and private credential patches.
type Mutation struct {
	Expected     config.Revision    `json:"expected"`
	Profiles     config.Profiles    `json:"profiles"`
	Patches      map[string]Patch   `json:"patches,omitempty"`
	Replacements map[string]Secrets `json:"-"`
	HostKey      *transport.HostKey `json:"-"`
}

// Outcome distinguishes committed changes from an uncertain transaction outcome.
type Outcome struct {
	Revision             config.Revision `json:"revision"`
	ProfilesSaved        bool            `json:"profiles_saved"`
	PublicationUncertain bool            `json:"publication_uncertain"`
}

func (r *Repository) Apply(ctx context.Context, m Mutation) (Outcome, error) {
	var out Outcome
	raw, err := json.Marshal(m.Profiles)
	if err != nil {
		return out, ErrRepair
	}
	p, _, err := config.DecodeProfiles(bytes.NewReader(raw))
	if err != nil {
		return out, err
	}
	needsKey := len(m.Replacements) > 0
	for id, patch := range m.Patches {
		var check Secrets
		if !config.ValidUUID(id) || patch.Apply(&check) != nil {
			return out, ErrRepair
		}
		for _, c := range p.Connections {
			if c.ID == id && (check != (Secrets{}) || c.CredentialRef != "") {
				needsKey = true
			}
		}
	}
	if needsKey {
		if err = r.Unlock(ctx, true, m.Expected); err != nil {
			return out, err
		}
	}
	l, err := r.store.WriteLease(ctx)
	if err != nil {
		return out, err
	}
	defer l.Release()
	previous, rev, err := l.ProfileSnapshot()
	if err != nil {
		return out, err
	}
	if rev != m.Expected {
		return out, config.ErrRevision
	}
	old := map[string]config.Profile{}
	for _, c := range previous.Connections {
		old[c.ID] = c
	}
	replacements := []config.Ciphertext{}
	for i := range p.Connections {
		profile := &p.Connections[i]
		before := old[profile.ID]
		if profile.CredentialRef != before.CredentialRef {
			return out, ErrBinding
		}
		secrets, replacing := m.Replacements[profile.ID]
		if patch, ok := m.Patches[profile.ID]; ok && len(patch) > 0 {
			secrets, err = r.credential(l, before)
			if errors.Is(err, ErrCredentialMissing) && patch.Complete(*profile) {
				err = nil
			}
			if err != nil {
				return out, err
			}
			if err = patch.Apply(&secrets); err != nil {
				return out, err
			}
			replacing = true
		}
		if !replacing {
			if profile.CredentialRef != "" {
				if _, e := l.Bundle(profile.CredentialRef); errors.Is(e, os.ErrNotExist) {
					return out, ErrCredentialMissing
				} else if e != nil {
					return out, e
				}
			}
			continue
		}
		if !secrets.valid() {
			return out, ErrRepair
		}
		if secrets == (Secrets{}) {
			profile.CredentialRef = ""
			continue
		}
		primitive, e := r.cipher(l)
		if e != nil {
			return out, e
		}
		profile.CredentialRef, err = config.NewID()
		if err != nil {
			return out, err
		}
		plain, err := json.Marshal(bundle{Version: 1, Secrets: secrets})
		if err != nil || len(plain) > MaxPlaintextBytes {
			return out, ErrRepair
		}
		k, err := l.Keyset()
		if err != nil {
			clear(plain)
			return out, ErrRepair
		}
		if k.Reserved >= MaxReservations {
			clear(plain)
			return out, ErrLimit
		}
		k.Reserved++
		if err = l.SaveKeyset(k); err != nil {
			clear(plain)
			return out, err
		}
		if err = r.point("after-reservation"); err != nil {
			clear(plain)
			return out, err
		}
		if err = ctx.Err(); err != nil {
			clear(plain)
			return out, err
		}
		encrypted, err := primitive.Encrypt(plain, aad(l.Identity(), *profile, 1))
		clear(plain)
		if err != nil {
			return out, ErrRepair
		}
		if err = r.point("after-encryption"); err != nil {
			return out, err
		}
		replacements = append(replacements, config.Ciphertext{Reference: profile.CredentialRef, ConnectionID: profile.ID, Version: 1, Data: encrypted})
	}
	for id := range m.Patches {
		found := false
		for _, p := range p.Connections {
			if p.ID == id {
				found = true
			}
		}
		if !found {
			return out, ErrRepair
		}
	}
	for id := range m.Replacements {
		found := false
		for _, p := range p.Connections {
			if p.ID == id {
				found = true
			}
		}
		if !found {
			return out, ErrRepair
		}
	}
	if m.HostKey != nil {
		if err = transport.SaveHostKey(l, *m.HostKey); err != nil {
			return out, err
		}
	}
	if err = ctx.Err(); err != nil {
		return out, err
	}
	out.Revision, err = l.Publish(p, m.Expected, replacements)
	if err != nil {
		out.PublicationUncertain = errors.Is(err, config.ErrCommitUnknown)
		return out, err
	}
	out.ProfilesSaved = true
	return out, r.point("after-profile-publication")
}
func (r *Repository) ValidateExisting(ctx context.Context, l *config.Lease) error {
	p, _, err := l.ProfileSnapshot()
	if err != nil {
		return err
	}
	for _, c := range p.Connections {
		if _, err = r.credential(l, c); err != nil {
			return err
		}
	}
	return ctx.Err()
}
func (r *Repository) PurgeCredentials(ctx context.Context) error {
	l, e := r.store.PurgeLease(ctx)
	if e != nil {
		return e
	}
	defer l.Release()
	return r.PurgeLocked(ctx, l)
}
func (r *Repository) PurgeLocked(ctx context.Context, l *config.Lease) error {
	if err := l.BeginPurge(); err != nil {
		return err
	}
	if err := r.point("before-key-delete"); err != nil {
		return err
	}
	if err := r.keys.Delete(ctx, l.Identity().KeyAccount); err != nil {
		return providerError(err)
	}
	if err := r.point("after-key-delete"); err != nil {
		return err
	}
	for _, path := range []string{"state/data-mate.db-journal", "state/data-mate.db"} {
		if err := l.Remove(path); err != nil {
			return err
		}
	}
	return nil
}
