package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/database"
	"github.com/swqa7697/data-mate/internal/transport"
	"github.com/swqa7697/data-mate/internal/vault"
)

// validateMutation resolves the same candidate patches as publication, but releases
// state access before network work. It returns the post-preparation mutation
// deadline so Apply shares the same budget and rechecks Expected before publication.
func (m *Manager) validateMutation(ctx context.Context, q ManagementRequest) (time.Time, error) {
	mutation := q.Mutation
	if len(mutation.Replacements) != 0 {
		return time.Time{}, ErrState
	}
	raw, err := json.Marshal(mutation.Profiles)
	if err != nil {
		return time.Time{}, ErrState
	}
	candidates, _, err := config.DecodeProfiles(bytes.NewReader(raw))
	if err != nil {
		return time.Time{}, ErrState
	}
	previous, rev, err := m.repo.Snapshot(ctx)
	if err != nil {
		return time.Time{}, err
	}
	if rev != mutation.Expected {
		return time.Time{}, config.ErrRevision
	}
	old := make(map[string]config.Profile)
	for _, p := range previous.Connections {
		old[p.ID] = p
	}
	var changed []config.Profile
	targetFound := q.ProfileID == ""
	if _, exists := old[q.ProfileID]; exists {
		targetFound = true
	}
	needsUnlock := false
	budget := config.MaxQueryTimeout
	for _, p := range candidates.Connections {
		if p.ID == q.ProfileID {
			targetFound = true
		}
		before, exists := old[p.ID]
		if p.CredentialRef != before.CredentialRef {
			return time.Time{}, vault.ErrBinding
		}
		if !exists || p.ID == q.ProfileID || !reflect.DeepEqual(p, before) || len(mutation.Patches[p.ID]) != 0 {
			changed = append(changed, p)
			limit := time.Duration(p.Limits.QueryTimeoutMS) * time.Millisecond
			if limit < budget {
				budget = limit
			}
			needsUnlock = needsUnlock || before.CredentialRef != ""
		}
	}
	if !targetFound || (q.ProfileID == "" && len(changed) == 0 && len(candidates.Connections) >= len(previous.Connections)) {
		return time.Time{}, ErrState
	}
	create, err := mutation.NeedsKey()
	if err != nil {
		return time.Time{}, err
	}
	if needsUnlock || create {
		prep, finish := context.WithTimeout(ctx, vault.PreparationTimeout)
		err = m.repo.Unlock(prep, create, mutation.Expected)
		finish()
		if err != nil {
			return time.Time{}, err
		}
	}
	started := time.Now()
	deadline := started.Add(30 * time.Second)
	ctx, cancel := context.WithDeadline(ctx, started.Add(min(budget, 30*time.Second)))
	defer cancel()
	accesses, err := func() ([]database.Access, error) {
		l, err := m.store.ReadLease(ctx)
		if err != nil {
			return nil, err
		}
		defer l.Release()
		_, current, err := l.ProfileSnapshot()
		if err != nil {
			return nil, err
		}
		if current != rev {
			return nil, config.ErrRevision
		}
		var accesses []database.Access
		for _, p := range changed {
			var secrets vault.Secrets
			if _, exists := old[p.ID]; exists {
				secrets, err = m.repo.Credential(ctx, l, p.ID)
				if errors.Is(err, vault.ErrCredentialMissing) && mutation.Patches[p.ID].Complete(p) {
					err = nil
				}
				if err != nil {
					return nil, err
				}
			}
			if err = mutation.Patches[p.ID].Apply(&secrets); err != nil {
				return nil, err
			}
			var hosts []byte
			if p.Transport.SSH != nil {
				hosts, err = transport.ReadKnownHosts(l)
				if err != nil {
					return nil, err
				}
				if mutation.HostKey != nil {
					hosts, err = transport.WithHostKey(hosts, *mutation.HostKey)
					if err != nil {
						return nil, err
					}
				}
			}
			accesses = append(accesses, database.NewAccess(p, secrets.Password).WithTransport(transport.Credentials{SSHPassword: secrets.SSHPassword, SSHPrivateKey: secrets.SSHPrivateKey, SSHKeyPassphrase: secrets.SSHKeyPassphrase, ProxyPassword: secrets.ProxyPassword}, hosts))
		}
		return accesses, nil
	}()
	if err != nil {
		return time.Time{}, err
	}
	for _, a := range accesses {
		leave, admissionError := m.admit(ctx)
		if admissionError != nil {
			return time.Time{}, admissionError
		}
		_, err = m.driver.Test(ctx, a)
		leave()
		if err != nil {
			return time.Time{}, err
		}
	}
	return deadline, ctx.Err()
}
