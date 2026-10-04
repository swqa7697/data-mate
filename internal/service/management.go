package service

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"slices"
	"sync"
	"time"

	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
	"github.com/swqa7697/data-mate/internal/transport"
	"github.com/swqa7697/data-mate/internal/vault"
	"golang.org/x/crypto/ssh"
)

const managementLimit = 8 << 20

// diagnosticConcurrency bounds service-wide test fan-out, leaving shared
// admission capacity for MCP work while batches run.
const diagnosticConcurrency = 16

// ManagementRequest is the private CLI protocol. No operation returns saved secrets.
type ManagementRequest struct {
	Operation   string          `json:"operation"`
	Interactive bool            `json:"interactive"`
	Mutation    *vault.Mutation `json:"mutation,omitempty"`
	Expected    config.Revision `json:"expected,omitempty"`
	ProfileID   string          `json:"profile_id,omitempty"`
	Alias       string          `json:"alias,omitempty"`
	Pin         *HostPin        `json:"pin,omitempty"`
	Targets     []ProfileTarget `json:"targets,omitempty"`
	// Report receives each test result after its state lease is released. Calls
	// may be concurrent; it is process-local and never serialized.
	Report func(DiagnosticResult) error `json:"-"`
}

// ProfileTarget binds one diagnostic to a profile identity and its displayed alias.
type ProfileTarget struct {
	ProfileID string `json:"profile_id"`
	Alias     string `json:"alias"`
}

// HostPin carries only a newly confirmed SSH public key, never a caller path.
type HostPin struct {
	Address string `json:"address"`
	Key     []byte `json:"key"`
}

// DiagnosticResult preserves the CLI's reached-stage contract.
type DiagnosticResult struct {
	Alias  string             `json:"alias"`
	OK     bool               `json:"ok"`
	Stage  string             `json:"stage"`
	Stages []database.Stage   `json:"stages"`
	Error  *contracts.Failure `json:"error,omitempty"`
}

// CredentialPresence reports which saved secret fields are nonempty, never their values.
type CredentialPresence struct {
	ProfileID        string `json:"profile_id"`
	Password         bool   `json:"password"`
	SSHPassword      bool   `json:"ssh_password"`
	SSHPrivateKey    bool   `json:"ssh_private_key"`
	SSHKeyPassphrase bool   `json:"ssh_key_passphrase"`
	ProxyPassword    bool   `json:"proxy_password"`
}

// ManagementReply contains bounded nonsecret results and fixed safe error identifiers.
type ManagementReply struct {
	Outcome     *vault.Outcome                `json:"outcome,omitempty"`
	Diagnostic  *DiagnosticResult             `json:"diagnostic,omitempty"`
	Description *database.DatabaseDescription `json:"description,omitempty"`
	Credentials []CredentialPresence          `json:"credentials,omitempty"`
	Error       string                        `json:"error,omitempty"`
	Failure     *contracts.Failure            `json:"failure,omitempty"`
	MCPEnabled  bool                          `json:"mcp_enabled"`
	KeysetState string                        `json:"keyset_state"`
}

// ResultError decodes the service's redacted database and fixed local failures.
func (r ManagementReply) ResultError() error {
	if r.Failure != nil {
		return &database.Error{Failure: *r.Failure}
	}
	return ManagementError(r.Error)
}

var managementErrors = []error{context.Canceled, context.DeadlineExceeded, config.ErrRevision, config.ErrCommitUnknown, config.ErrObsolete, config.ErrRecovery, config.ErrState, config.ErrOwnership, config.ErrStale, config.ErrPurging, vault.ErrTerminal, vault.ErrPassword, vault.ErrUserBus, vault.ErrUnsupported, vault.ErrProviderChanged, vault.ErrProviderUnavailable, vault.ErrMissing, vault.ErrDenied, vault.ErrLocked, vault.ErrUnavailable, vault.ErrRepair, vault.ErrLimit, vault.ErrBinding, vault.ErrCredentialMissing, transport.ErrChangedHost, transport.ErrKnownHosts, ErrState, ErrConflict, ErrRestart, ErrUnavailable}

func safeManagementError(err error) string {
	for _, e := range managementErrors {
		if errors.Is(err, e) {
			return e.Error()
		}
	}
	return ErrUnavailable.Error()
}

// ManagementError decodes only the fixed private protocol error vocabulary.
func ManagementError(message string) error {
	if message == "" {
		return nil
	}
	for _, e := range managementErrors {
		if message == e.Error() {
			return e
		}
	}
	return ErrUnavailable
}
func readFrame(r io.Reader, dst any) error { return readFrameLimit(r, dst, managementLimit) }
func readFrameLimit(r io.Reader, dst any, limit int) error {
	var size [4]byte
	if _, err := io.ReadFull(r, size[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(size[:])
	if n == 0 || n > uint32(limit) {
		return ErrState
	}
	raw := make([]byte, n)
	defer clear(raw)
	if _, err := io.ReadFull(r, raw); err != nil {
		return err
	}
	if config.DecodeStrict(raw, limit, dst) != nil {
		return ErrState
	}
	return nil
}
func writeFrame(w io.Writer, v any) error {
	raw, err := json.Marshal(v)
	if err != nil || len(raw) > managementLimit {
		return ErrState
	}
	defer clear(raw)
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(raw)))
	if _, err = w.Write(size[:]); err != nil {
		return err
	}
	_, err = w.Write(raw)
	return err
}

// Request sends one request after verifying the service's native and application identity.
func (c *Controller) Request(ctx context.Context, request ManagementRequest) (ManagementReply, error) {
	var reply ManagementReply
	s, err := config.OpenExisting(ctx, c.Root)
	if err != nil {
		return reply, err
	}
	defer s.Close()
	l, err := s.ReadLease(ctx)
	if err != nil {
		return reply, err
	}
	r, err := readRecord(l.Read, c.Root, l.Identity())
	l.Release()
	if err != nil {
		return reply, err
	}
	conn, _, err := connect(ctx, c.Root, r, c.Build, "management")
	if err != nil {
		return reply, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err = writeFrame(conn, request); err != nil {
		return reply, ErrUnavailable
	}
	_ = conn.SetWriteDeadline(time.Time{})
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetReadDeadline(deadline)
	}
	return exchangeManagement(ctx, conn, request)
}

// HandleManagement owns independent admission and service-side input validation.
func (m *Manager) HandleManagement(parent context.Context, q ManagementRequest) (reply ManagementReply) {
	select {
	case m.management <- struct{}{}:
		defer func() { <-m.management }()
	default:
		reply.Error = ErrUnavailable.Error()
		return
	}
	ctx, cancel := context.WithCancel(vault.WithInteraction(parent, q.Interactive))
	defer cancel()
	stop := context.AfterFunc(m.ctx, cancel)
	defer stop()
	var err error
	defer func() {
		m.mu.Lock()
		reply.MCPEnabled = m.enabled
		m.mu.Unlock()
		reply.KeysetState = m.keysetState(ctx)
		if err != nil {
			var safe *database.Error
			if errors.As(err, &safe) {
				reply.Failure = &safe.Failure
			} else {
				reply.Error = safeManagementError(err)
			}
		}
	}()
	switch q.Operation {
	case "mutate":
		if q.Mutation == nil || q.Expected != "" || q.Alias != "" || len(q.Targets) != 0 {
			err = ErrState
			return
		}
		if q.Pin != nil {
			key, e := ssh.ParsePublicKey(q.Pin.Key)
			if e != nil || len(q.Pin.Key) > 16384 || len(q.Pin.Address) > 1024 {
				err = ErrState
				return
			}
			q.Mutation.HostKey = &transport.HostKey{Address: q.Pin.Address, Key: key}
		}
		deadline, validationError := m.validateMutation(ctx, q)
		err = validationError
		if err != nil {
			reply.Outcome = &vault.Outcome{}
			return
		}
		ctx, finish := context.WithDeadline(ctx, deadline)
		defer finish()
		out, e := m.repo.Apply(ctx, *q.Mutation)
		reply.Outcome = &out
		err = e
		if err == nil {
			l, e := m.store.ReadLease(ctx)
			if e == nil {
				_, e = m.refresh(l)
				l.Release()
			}
			err = e
		}
	case "enable":
		if q.Mutation != nil || q.Pin != nil || q.ProfileID != "" || q.Expected != "" || q.Alias != "" || len(q.Targets) != 0 {
			err = ErrState
			return
		}
		profiles, _, e := m.repo.Snapshot(ctx)
		if e != nil {
			err = e
			return
		}
		if slices.ContainsFunc(profiles.Connections, func(p config.Profile) bool { return p.CredentialRef != "" }) {
			if err = m.unlock(ctx); err != nil {
				return
			}
		}
		ctx, finish := context.WithTimeout(ctx, 30*time.Second)
		defer finish()
		l, e := m.store.ReadLease(ctx)
		if e != nil {
			err = e
			return
		}
		err = m.repo.ValidateExisting(ctx, l)
		l.Release()
		if err != nil {
			return
		}
		if m.enable == nil {
			err = ErrUnavailable
			return
		}
		err = m.enable(ctx)
	case "test":
		if q.Mutation != nil || q.Pin != nil || q.ProfileID != "" || q.Alias != "" || q.Expected != "" || q.Report == nil || !validTargets(q.Targets) {
			err = ErrState
			return
		}
		err = m.testProfiles(ctx, q)
	case "describe":
		if q.Mutation != nil || q.Pin != nil || q.ProfileID == "" || q.Alias == "" || q.Expected == "" || len(q.Targets) != 0 {
			err = ErrState
			return
		}
		var description database.DatabaseDescription
		reply.Diagnostic, err = m.describeProfile(ctx, q, &description)
		if err == nil && reply.Diagnostic == nil {
			reply.Description = &description
		}
	case "credential-presence":
		if q.Mutation != nil || q.Pin != nil || q.ProfileID != "" || q.Alias != "" || q.Expected == "" || len(q.Targets) != 0 {
			err = ErrState
			return
		}
		reply.Credentials, err = m.credentialPresence(ctx, q.Expected)
	default:
		err = ErrState
	}
	return
}
func (m *Manager) keysetState(ctx context.Context) string {
	l, e := m.store.ReadLease(ctx)
	if e != nil {
		return "unavailable"
	}
	k, e := l.Keyset()
	l.Release()
	if e != nil {
		return "unavailable"
	}
	if k.Phase == "" {
		return "absent"
	}
	return m.repo.State()
}

// describeProfile describes one profile bound to the expected store revision.
func (m *Manager) describeProfile(parent context.Context, q ManagementRequest, description *database.DatabaseDescription) (*DiagnosticResult, error) {
	l, err := m.store.ReadLease(parent)
	if err != nil {
		return nil, err
	}
	profiles, revision, err := l.ProfileSnapshot()
	l.Release()
	if err != nil {
		return nil, err
	}
	p := findProfile(profiles, q.ProfileID, q.Alias)
	if revision != q.Expected || p == nil {
		return nil, config.ErrRevision
	}
	unlockErr := m.prepareCredentials(parent, []config.Profile{*p})
	return m.checkProfile(parent, q.Operation, q.Expected, *p, unlockErr, description)
}

// credentialPresence reports which secret fields each credentialed profile has
// at the expected revision; values never leave the service. A profile whose
// bundle is missing is omitted so callers treat its presence as unknown.
func (m *Manager) credentialPresence(ctx context.Context, expected config.Revision) ([]CredentialPresence, error) {
	l, err := m.store.ReadLease(ctx)
	if err != nil {
		return nil, err
	}
	// Release before unlocking: a waiting writer blocks the vault's own lease.
	profiles, revision, err := l.ProfileSnapshot()
	l.Release()
	if err != nil {
		return nil, err
	}
	if revision != expected {
		return nil, config.ErrRevision
	}
	if !slices.ContainsFunc(profiles.Connections, func(p config.Profile) bool { return p.CredentialRef != "" }) {
		return nil, nil
	}
	if err = m.unlock(ctx); err != nil {
		return nil, err
	}
	l, err = m.store.ReadLease(ctx)
	if err != nil {
		return nil, err
	}
	defer l.Release()
	if _, revision, err = l.ProfileSnapshot(); err != nil {
		return nil, err
	}
	if revision != expected {
		return nil, config.ErrRevision
	}
	var presence []CredentialPresence
	for _, p := range profiles.Connections {
		if p.CredentialRef == "" {
			continue
		}
		s, err := m.repo.Credential(ctx, l, p.ID)
		if errors.Is(err, vault.ErrCredentialMissing) {
			continue
		}
		if err != nil {
			return nil, err
		}
		presence = append(presence, CredentialPresence{ProfileID: p.ID, Password: s.Password != "", SSHPassword: s.SSHPassword != "", SSHPrivateKey: s.SSHPrivateKey != "", SSHKeyPassphrase: s.SSHKeyPassphrase != "", ProxyPassword: s.ProxyPassword != ""})
	}
	return presence, nil
}

// testProfiles checks targets concurrently and reports each result as it
// completes. Ordinary failures are results; revision changes, cancellation and
// undeliverable reports abort the batch.
func (m *Manager) testProfiles(parent context.Context, q ManagementRequest) error {
	l, err := m.store.ReadLease(parent)
	if err != nil {
		return err
	}
	// Release before unlocking: a waiting writer blocks the vault's own lease.
	profiles, _, err := l.ProfileSnapshot()
	l.Release()
	if err != nil {
		return err
	}
	selected := make([]config.Profile, len(q.Targets))
	for i, t := range q.Targets {
		p := findProfile(profiles, t.ProfileID, t.Alias)
		if p == nil {
			return config.ErrRevision
		}
		selected[i] = *p
	}
	unlockErr := m.prepareCredentials(parent, selected)
	if errors.Is(unlockErr, context.Canceled) {
		return unlockErr
	}
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	var wg sync.WaitGroup
	for _, p := range selected {
		wg.Go(func() {
			// Profile deadlines start only after a fan-out slot is held.
			select {
			case m.diagnostics <- struct{}{}:
				defer func() { <-m.diagnostics }()
			case <-ctx.Done():
				return
			}
			r, err := m.checkProfile(ctx, "test", "", p, unlockErr, nil)
			if err == nil && ctx.Err() == nil {
				err = q.Report(*r)
			}
			if err != nil {
				cancel(err)
			}
		})
	}
	// Every report precedes the final reply.
	wg.Wait()
	return context.Cause(ctx)
}

// validTargets requires a nonempty selection of distinct profile bindings.
func validTargets(targets []ProfileTarget) bool {
	ids, aliases := map[string]bool{}, map[string]bool{}
	for _, t := range targets {
		if t.ProfileID == "" || t.Alias == "" || ids[t.ProfileID] || aliases[t.Alias] {
			return false
		}
		ids[t.ProfileID], aliases[t.Alias] = true, true
	}
	return len(targets) > 0
}

func findProfile(profiles config.Profiles, id, alias string) *config.Profile {
	for i := range profiles.Connections {
		if profiles.Connections[i].ID == id && profiles.Connections[i].Alias == alias {
			return &profiles.Connections[i]
		}
	}
	return nil
}

// prepareCredentials unlocks the keyset once for profiles whose checks will use
// it. Callers report its error per profile instead of repeating the unlock.
func (m *Manager) prepareCredentials(ctx context.Context, profiles []config.Profile) error {
	for _, p := range profiles {
		if p.CredentialRef != "" && m.validProfile(p) {
			return m.unlock(ctx)
		}
	}
	return nil
}
func (m *Manager) unlock(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, vault.PreparationTimeout)
	defer cancel()
	return m.repo.Unlock(ctx, false, "")
}

// validProfile applies driver-specific settings validation before credential access.
func (m *Manager) validProfile(p config.Profile) bool {
	validator, ok := m.driver.(interface{ ValidateProfile(config.Profile) error })
	return !ok || validator.ValidateProfile(p) == nil
}

// checkProfile runs one prepared profile's diagnostic or description. Staged
// failures become results; revision changes and cancellation return errors.
func (m *Manager) checkProfile(parent context.Context, operation string, expected config.Revision, p config.Profile, unlockErr error, description *database.DatabaseDescription) (*DiagnosticResult, error) {
	ctx := parent
	result := &DiagnosticResult{Alias: p.Alias, Stage: "config", Stages: []database.Stage{{Stage: "config", OK: true}}}
	fail := func(stage string, code contracts.Code, e error) (*DiagnosticResult, error) {
		if errors.Is(e, context.Canceled) {
			return nil, context.Canceled
		}
		if parent.Err() != nil {
			return nil, parent.Err()
		}
		if ctx.Err() != nil {
			code = contracts.QueryTimeout
			e = context.DeadlineExceeded
		}
		if errors.Is(e, config.ErrRevision) {
			return nil, e
		}
		result.Stage = stage
		message := safeManagementError(e)
		var safe *database.Error
		if errors.As(e, &safe) {
			message = safe.Message
		}
		result.Error = &contracts.Failure{Code: code, Message: message, Retryable: false}
		result.Stages = append(result.Stages, database.Stage{Stage: stage, Error: result.Error})
		return result, nil
	}
	if !m.validProfile(p) {
		result.Error = &contracts.Failure{Code: contracts.ConfigInvalid, Message: "invalid driver profile settings; repair with db edit"}
		result.Stages = []database.Stage{{Stage: "config", Error: result.Error}}
		return result, nil
	}
	if p.CredentialRef != "" && unlockErr != nil {
		return fail("vault", contracts.VaultUnavailable, unlockErr)
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(p.Limits.QueryTimeoutMS)*time.Millisecond)
	defer cancel()
	// Work admits before acquiring a fresh snapshot, preserving query-vs-mutation leases.
	err := m.Work(ctx, p.Alias, func(ctx context.Context, d database.Driver, a database.Access) error {
		if a.Profile.ID != p.ID {
			return config.ErrRevision
		}
		if operation != "test" {
			// Work holds the state lease, so a second passive read observes the same generation.
			// Avoid acquiring a second application lease while a writer is waiting.
			current := m.currentRevision()
			if current != expected {
				return config.ErrRevision
			}
		}
		if operation == "describe" {
			describer, ok := d.(interface {
				DescribeDatabase(context.Context, database.Access) (database.DatabaseDescription, error)
			})
			if !ok {
				return ErrUnavailable
			}
			var e error
			*description, e = describer.DescribeDatabase(ctx, a)
			return e
		}
		result.Stages = append(result.Stages, database.Stage{Stage: "vault", OK: true})
		ready, e := d.Test(ctx, a)
		result.Stage = ready.Stage
		for _, stage := range ready.Stages {
			if stage.Stage != "config" {
				result.Stages = append(result.Stages, stage)
			}
		}
		if e != nil {
			var safe *database.Error
			if errors.As(e, &safe) {
				result.Error = &safe.Failure
			} else {
				result.Error = &contracts.Failure{Code: contracts.ConnectFailed, Message: "database diagnostic failed"}
			}
		}
		result.OK = e == nil
		return nil
	})
	if err != nil {
		code := contracts.VaultUnavailable
		var safe *database.Error
		if errors.As(err, &safe) {
			code = safe.Code
		}
		return fail("vault", code, err)
	}
	if operation == "describe" {
		return nil, nil
	}
	return result, nil
}
func (m *Manager) currentRevision() config.Revision {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.revision
}

func serveManagement(ctx context.Context, c *net.UnixConn, m *Manager) {
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	var q ManagementRequest
	if readFrame(c, &q) != nil {
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	serveManagementExchange(ctx, c, m, q)
}
