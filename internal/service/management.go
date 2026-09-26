package service

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"time"

	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
	"github.com/swqa7697/data-mate/internal/transport"
	"github.com/swqa7697/data-mate/internal/vault"
	"golang.org/x/crypto/ssh"
)

const managementLimit = 8 << 20

// ManagementRequest is the private CLI protocol. No operation returns saved secrets.
type ManagementRequest struct {
	Operation   string                 `json:"operation"`
	Interactive bool                   `json:"interactive"`
	Mutation    *vault.Mutation        `json:"mutation,omitempty"`
	Expected    config.Revision        `json:"expected,omitempty"`
	ProfileID   string                 `json:"profile_id,omitempty"`
	Alias       string                 `json:"alias,omitempty"`
	Scope       *database.ScopeRequest `json:"scope,omitempty"`
	Pin         *HostPin               `json:"pin,omitempty"`
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

// ManagementReply contains bounded nonsecret results and fixed safe error identifiers.
type ManagementReply struct {
	Outcome     *vault.Outcome                `json:"outcome,omitempty"`
	Diagnostic  *DiagnosticResult             `json:"diagnostic,omitempty"`
	Page        *database.ScopePage           `json:"page,omitempty"`
	Description *database.DatabaseDescription `json:"description,omitempty"`
	Error       string                        `json:"error,omitempty"`
	MCPEnabled  bool                          `json:"mcp_enabled"`
	KeysetState string                        `json:"keyset_state"`
}

var managementErrors = []error{context.Canceled, context.DeadlineExceeded, config.ErrRevision, config.ErrCommitUnknown, config.ErrObsolete, config.ErrRecovery, config.ErrState, config.ErrOwnership, config.ErrStale, config.ErrPurging, vault.ErrMissing, vault.ErrDenied, vault.ErrLocked, vault.ErrUnavailable, vault.ErrRepair, vault.ErrLimit, vault.ErrBinding, vault.ErrCredentialMissing, transport.ErrChangedHost, transport.ErrKnownHosts, ErrState, ErrConflict, ErrRestart, ErrUnavailable}

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
func readFrame(r io.Reader, dst any) error {
	var size [4]byte
	if _, err := io.ReadFull(r, size[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(size[:])
	if n == 0 || n > managementLimit {
		return ErrState
	}
	raw := make([]byte, n)
	defer clear(raw)
	if _, err := io.ReadFull(r, raw); err != nil {
		return err
	}
	if config.DecodeStrict(raw, managementLimit, dst) != nil {
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
	if err = readFrame(conn, &reply); err != nil {
		if ctx.Err() != nil {
			return reply, ctx.Err()
		}
		return reply, ErrUnavailable
	}
	return reply, ManagementError(reply.Error)
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
			reply.Error = safeManagementError(err)
		}
	}()
	switch q.Operation {
	case "mutate":
		ctx, finish := context.WithTimeout(ctx, 30*time.Second)
		defer finish()
		if q.Mutation == nil || q.Scope != nil || q.ProfileID != "" || q.Expected != "" || q.Alias != "" {
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
		if q.Mutation != nil || q.Scope != nil || q.Pin != nil || q.ProfileID != "" || q.Expected != "" || q.Alias != "" {
			err = ErrState
			return
		}
		ctx, finish := context.WithTimeout(ctx, 30*time.Second)
		defer finish()
		if err = m.repo.Unlock(ctx, false, ""); err != nil {
			return
		}
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
	case "test", "browse", "describe":
		if q.Mutation != nil || q.Pin != nil || q.ProfileID == "" || q.Alias == "" || (q.Operation == "browse" && q.Scope == nil) || (q.Operation != "test" && q.Expected == "") || (q.Operation != "browse" && q.Scope != nil) {
			err = ErrState
			return
		}
		var description database.DatabaseDescription
		reply.Diagnostic, reply.Page, err = m.managementDatabase(ctx, q, &description)
		if q.Operation == "describe" && err == nil && reply.Diagnostic == nil {
			reply.Description = &description
		}
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
func (m *Manager) managementDatabase(parent context.Context, q ManagementRequest, description *database.DatabaseDescription) (*DiagnosticResult, *database.ScopePage, error) {
	started := time.Now()
	parent, finish := context.WithTimeout(parent, config.MaxQueryTimeout)
	defer finish()
	l, err := m.store.ReadLease(parent)
	if err != nil {
		return nil, nil, err
	}
	profiles, revision, err := l.ProfileSnapshot()
	l.Release()
	if err != nil {
		return nil, nil, err
	}
	var p *config.Profile
	for i := range profiles.Connections {
		if profiles.Connections[i].ID == q.ProfileID && profiles.Connections[i].Alias == q.Alias {
			p = &profiles.Connections[i]
		}
	}
	if q.Operation != "test" && revision != q.Expected {
		return nil, nil, config.ErrRevision
	}
	if p == nil {
		return nil, nil, config.ErrRevision
	}
	ctx, cancel := context.WithDeadline(parent, started.Add(time.Duration(p.Limits.QueryTimeoutMS)*time.Millisecond))
	defer cancel()
	result := &DiagnosticResult{Alias: p.Alias, Stage: "config", Stages: []database.Stage{{Stage: "config", OK: true}}}
	fail := func(stage string, code contracts.Code, e error) (*DiagnosticResult, *database.ScopePage, error) {
		if q.Operation == "describe" && errors.Is(e, context.Canceled) {
			return nil, nil, context.Canceled
		}
		if parent.Err() != nil {
			return nil, nil, parent.Err()
		}
		if ctx.Err() != nil {
			code = contracts.QueryTimeout
			e = context.DeadlineExceeded
		}
		if q.Operation == "browse" || errors.Is(e, config.ErrRevision) {
			return nil, nil, e
		}
		result.Stage = stage
		message := safeManagementError(e)
		var safe *database.Error
		if errors.As(e, &safe) {
			message = safe.Message
		}
		result.Error = &contracts.Failure{Code: code, Message: message, Retryable: false}
		result.Stages = append(result.Stages, database.Stage{Stage: stage, Error: result.Error})
		return result, nil, nil
	}
	validator, validates := m.driver.(interface{ ValidateProfile(config.Profile) error })
	if validates {
		if e := validator.ValidateProfile(*p); e != nil {
			if q.Operation == "browse" {
				return nil, nil, config.ErrState
			}
			result.Stage = "config"
			result.Error = &contracts.Failure{Code: contracts.ConfigInvalid, Message: "invalid driver profile settings; repair with db edit"}
			result.Stages = []database.Stage{{Stage: "config", Error: result.Error}}
			return result, nil, nil
		}
	}
	if p.CredentialRef != "" {
		if err = m.repo.Unlock(ctx, false, ""); err != nil {
			return fail("vault", contracts.VaultUnavailable, err)
		}
	}
	// Work admits before acquiring a fresh snapshot, preserving query-vs-mutation leases.
	var page database.ScopePage
	err = m.Work(ctx, p.Alias, func(ctx context.Context, d database.Driver, a database.Access) error {
		if a.Profile.ID != q.ProfileID {
			return config.ErrRevision
		}
		if q.Operation != "test" {
			// Work holds the state lease, so a second passive read observes the same generation.
			// Avoid acquiring a second application lease while a writer is waiting.
			current := m.currentRevision()
			if current != q.Expected {
				return config.ErrRevision
			}
		}
		if q.Operation == "describe" {
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
		if q.Operation == "browse" {
			browser, ok := d.(interface {
				BrowseScope(context.Context, database.Access, database.ScopeRequest) (database.ScopePage, error)
			})
			if !ok {
				return ErrUnavailable
			}
			var e error
			page, e = browser.BrowseScope(ctx, a, *q.Scope)
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
	if q.Operation == "browse" {
		return nil, &page, nil
	}
	if q.Operation == "describe" {
		return nil, nil, nil
	}
	return result, nil, nil
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
	operation, cancel := context.WithCancel(ctx)
	defer cancel()
	// A client keeps its write half open until completion; EOF cancels the operation.
	go func() { var b [1]byte; _, _ = c.Read(b[:]); cancel() }()
	reply := m.HandleManagement(operation, q)
	_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_ = writeFrame(c, reply)
}
