package service

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
	"github.com/swqa7697/data-mate/internal/vault"
)

type managedKeys struct {
	mu             sync.Mutex
	key            []byte
	loads, creates int
	denied         bool
	terminal       bool
	entered        chan struct{}
	release        chan struct{}
}

func (k *managedKeys) Load(context.Context, string) ([]byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.loads++
	if k.denied {
		return nil, vault.ErrDenied
	}
	if k.key == nil {
		return nil, vault.ErrMissing
	}
	return bytes.Clone(k.key), nil
}
func (k *managedKeys) CreateIfAbsent(ctx context.Context, _ string, key []byte) ([]byte, error) {
	k.mu.Lock()
	k.creates++
	entered, release := k.entered, k.release
	terminal := k.terminal
	k.entered = nil
	k.mu.Unlock()
	if terminal {
		for attempt := 1; attempt <= 3; attempt++ {
			password, err := vault.AskKeyring(ctx, vault.KeyringChallenge{Kind: "create", Attempt: attempt})
			if err != nil {
				return nil, err
			}
			accepted := string(password) == "synthetic-keyring-password"
			clear(password)
			if accepted {
				break
			}
			if attempt == 3 {
				return nil, vault.ErrPassword
			}
		}
	}
	if entered != nil {
		close(entered)
		<-release
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.key == nil {
		k.key = bytes.Clone(key)
	}
	return bytes.Clone(k.key), nil
}
func (k *managedKeys) Delete(context.Context, string) error { return nil }
func (k *managedKeys) calls() int                           { k.mu.Lock(); defer k.mu.Unlock(); return k.loads + k.creates }

type blockedDiagnostics struct {
	block           atomic.Bool
	mu              sync.Mutex
	validationError error
	validated       []database.Access
	validationDone  chan struct{}
	database.Driver
	entered chan struct{}
	release chan struct{}
}

func (d *blockedDiagnostics) Invalidate(string)                    {}
func (d *blockedDiagnostics) Close()                               {}
func (d *blockedDiagnostics) ValidateProfile(config.Profile) error { return nil }
func (d *blockedDiagnostics) DescribeDatabase(ctx context.Context, a database.Access) (database.DatabaseDescription, error) {
	_, err := d.Test(ctx, a)
	return database.DatabaseDescription{Version: 1, Alias: a.Profile.Alias, Database: a.Profile.Connection.Database, Schemas: []database.SchemaDescription{}}, err
}
func (d *blockedDiagnostics) Test(ctx context.Context, a database.Access) (database.Readiness, error) {
	d.mu.Lock()
	d.validated = append(d.validated, a)
	err := d.validationError
	done := d.validationDone
	d.mu.Unlock()
	if done != nil {
		defer func() { close(done) }()
	}
	if err != nil {
		return database.Readiness{}, err
	}
	if !d.block.Load() {
		return database.Readiness{Stage: "read_only"}, nil
	}
	d.entered <- struct{}{}
	select {
	case <-ctx.Done():
		return database.Readiness{}, ctx.Err()
	case <-d.release:
		return database.Readiness{Stage: "read_only", Stages: []database.Stage{{Stage: "read_only", OK: true}}}, nil
	}
}

// No previous regression exercised the new private socket. This scenario owns
// management-only admission, patches, independent clients, framing and cancellation.
func TestPrivateManagement(t *testing.T) {
	c, f, _ := controllerFixture(t)
	keys := &managedKeys{terminal: true}
	f.keys = keys
	driver := &blockedDiagnostics{entered: make(chan struct{}, 4), release: make(chan struct{})}
	f.driver = driver
	state, e := c.EnsureManagement(t.Context())
	if e != nil || state.MCPEnabled || state.KeysetState != "absent" {
		t.Fatal("management bootstrap", state, e)
	}
	if e = c.ProbeSession(t.Context()); e == nil {
		t.Fatal("management exposed MCP")
	}
	p, rev, e := config.Preview(t.Context(), c.Root)
	if e != nil {
		t.Fatal(e)
	}
	profile := fixtureProfile()
	p.Connections = append(p.Connections, profile)
	requestCtx := t.Context()
	interactive := false
	apply := func(p config.Profiles, rev config.Revision, patch vault.Patch) (ManagementReply, error) {
		q := ManagementRequest{Operation: "mutate", Interactive: interactive, ProfileID: profile.ID, Mutation: &vault.Mutation{Expected: rev, Profiles: p}}
		if patch != nil {
			q.Mutation.Patches = map[string]vault.Patch{profile.ID: patch}
		}
		return c.Request(requestCtx, q)
	}
	// Extend the authenticated request scenario: preparation precedes validation,
	// canceled/invalid answers publish nothing, and one request resumes one save.
	if r, e := apply(p, rev, vault.Patch{"password": "synthetic-management-secret"}); !errors.Is(e, vault.ErrTerminal) || r.Outcome == nil || r.Outcome.ProfilesSaved {
		t.Fatal("noninteractive authentication", r, e)
	}
	interactive = true
	requestCtx = vault.WithKeyringPrompt(t.Context(), func(context.Context, vault.KeyringChallenge) ([]byte, error) { return nil, context.Canceled })
	if r, e := apply(p, rev, vault.Patch{"password": "synthetic-management-secret"}); !errors.Is(e, context.Canceled) || r.Outcome == nil || r.Outcome.ProfilesSaved {
		t.Fatal("canceled preparation", r, e)
	}
	for _, mode := range []string{"unsolicited", "wrong-id", "empty", "cancel-with-password", "oversized", "disconnect"} {
		store, e := config.OpenExisting(t.Context(), c.Root)
		if e != nil {
			t.Fatal(e)
		}
		lease, e := store.ReadLease(t.Context())
		if e != nil {
			t.Fatal(e)
		}
		record, e := readRecord(lease.Read, c.Root, lease.Identity())
		lease.Release()
		store.Close()
		if e != nil {
			t.Fatal(e)
		}
		conn, _, e := connect(t.Context(), c.Root, record, c.Build, "management")
		if e != nil {
			t.Fatal(e)
		}
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		if mode == "unsolicited" {
			_ = writeFrame(conn, keyringAnswer{ID: profile.ID, Password: []byte("synthetic-keyring-password")})
		} else {
			q := ManagementRequest{Operation: "mutate", Interactive: true, ProfileID: profile.ID, Mutation: &vault.Mutation{Expected: rev, Profiles: p, Patches: map[string]vault.Patch{profile.ID: {"password": "synthetic-management-secret"}}}}
			if e = writeFrame(conn, q); e != nil {
				t.Fatal(e)
			}
			var frame managementFrame
			if e = readFrame(conn, &frame); e != nil || frame.Challenge == nil {
				t.Fatal("missing challenge", e)
			}
			a := keyringAnswer{ID: frame.ID}
			switch mode {
			case "wrong-id":
				a.ID, _ = config.NewID()
				a.Password = []byte("synthetic-keyring-password")
			case "cancel-with-password":
				a.Cancel = true
				a.Password = []byte("synthetic-keyring-password")
			case "oversized":
				var size [4]byte
				binary.BigEndian.PutUint32(size[:], 2*vault.MaxSecretBytes+257)
				_, _ = conn.Write(size[:])
			case "disconnect":
				conn.Close()
			}
			if mode != "oversized" && mode != "disconnect" {
				_ = writeFrame(conn, a)
			}
		}
		var frame managementFrame
		e = readFrame(conn, &frame)
		conn.Close()
		if e == nil && frame.Reply != nil && frame.Reply.Outcome != nil && frame.Reply.Outcome.ProfilesSaved {
			t.Fatal("invalid answer published", mode)
		}
	}
	driver.mu.Lock()
	validated := len(driver.validated)
	driver.mu.Unlock()
	if validated != 0 {
		t.Fatal("database work preceded authentication")
	}
	prompts := 0
	requestCtx = vault.WithKeyringPrompt(t.Context(), func(ctx context.Context, challenge vault.KeyringChallenge) ([]byte, error) {
		prompts++
		if challenge.Attempt != prompts {
			t.Error("wrong attempt", challenge)
		}
		// No SQLite/state lease may be retained while the user thinks.
		read, stop := context.WithTimeout(ctx, time.Second)
		defer stop()
		current, currentRev, e := config.Preview(read, c.Root)
		if e != nil || currentRev != rev || len(current.Connections) != 0 {
			t.Error("preparation locked/published state", e)
		}
		if prompts == 1 {
			return []byte("synthetic-wrong-password"), nil
		}
		return []byte("synthetic-keyring-password"), nil
	})
	if r, e := apply(p, rev, vault.Patch{"password": "synthetic-management-secret"}); e != nil || !r.Outcome.ProfilesSaved {
		t.Fatal("initial save", r, e)
	}
	if prompts != 2 {
		t.Fatal("retry count", prompts)
	}
	driver.mu.Lock()
	validated = len(driver.validated)
	driver.mu.Unlock()
	if validated != 1 {
		t.Fatal("request validated more than once", validated)
	}
	requestCtx = t.Context()
	calls := keys.calls()
	keys.mu.Lock()
	keys.denied = true
	keys.mu.Unlock()
	for i := 0; i < 3; i++ {
		p, rev, e = config.Preview(t.Context(), c.Root)
		if e != nil {
			t.Fatal(e)
		}
		r, e := apply(p, rev, vault.Patch{"password": "synthetic-edit"})
		if e != nil || !r.Outcome.ProfilesSaved {
			t.Fatal("cached save", r, e)
		}
		if _, e = apply(p, rev, nil); !errors.Is(e, config.ErrRevision) {
			t.Fatal("credential revision", e)
		}
	}
	if keys.calls() != calls {
		t.Fatal("independent clients reloaded OS keyset")
	}
	staleRevision := rev
	// Extend this mutation scenario: validation uses candidate secrets, runs for
	// unchanged edits, and failures publish neither profiles nor credentials.
	p, rev, e = config.Preview(t.Context(), c.Root)
	if e != nil {
		t.Fatal(e)
	}
	for _, failure := range []error{database.Fail(contracts.ReadOnlyViolation, "account is writable", false), database.Fail(contracts.ConnectFailed, "connection unavailable", true)} {
		driver.mu.Lock()
		driver.validationError = failure
		before := len(driver.validated)
		driver.mu.Unlock()
		for _, patch := range []vault.Patch{nil, {"password": "synthetic-candidate"}} {
			r, err := apply(p, rev, patch)
			var safe *database.Error
			if !errors.As(err, &safe) || r.Outcome == nil || r.Outcome.ProfilesSaved {
				t.Fatal("validation failure not propagated", err)
			}
		}
		driver.mu.Lock()
		count := len(driver.validated)
		candidate := driver.validated[count-1].Password()
		driver.mu.Unlock()
		if count != before+2 || candidate != "synthetic-candidate" {
			t.Fatal("unchanged edit skipped validation or candidate credentials lost")
		}
		_, after, err := config.Preview(t.Context(), c.Root)
		if err != nil || after != rev {
			t.Fatal("rejected mutation changed state", err)
		}
	}
	driver.mu.Lock()
	driver.validationError = nil
	driver.mu.Unlock()
	// Both edits can validate without a state lease. Only the first publication
	// wins; the other must reject its stale snapshot after network work completes.
	driver.block.Store(true)
	racing := make(chan error, 2)
	for _, alias := range []string{"candidate-one", "candidate-two"} {
		candidate := p
		candidate.Connections = append([]config.Profile(nil), p.Connections...)
		candidate.Connections[0].Alias = alias
		go func() { _, err := apply(candidate, rev, nil); racing <- err }()
		select {
		case <-driver.entered:
		case <-time.After(3 * time.Second):
			t.Fatal("save validation did not start")
		}
	}
	passive, cancelPassive := context.WithTimeout(t.Context(), time.Second)
	_, unchanged, err := config.Preview(passive, c.Root)
	cancelPassive()
	if err != nil || unchanged != rev {
		t.Fatal("validation held state or published early", err)
	}
	driver.release <- struct{}{}
	if err := <-racing; err != nil {
		t.Fatal("first validated edit", err)
	}
	driver.release <- struct{}{}
	if err := <-racing; !errors.Is(err, config.ErrRevision) {
		t.Fatal("stale validated edit published", err)
	}
	driver.block.Store(false)
	p, rev, e = config.Preview(t.Context(), c.Root)
	if e != nil {
		t.Fatal(e)
	}
	p.Connections[0].Alias = profile.Alias
	if _, e = apply(p, rev, nil); e != nil {
		t.Fatal(e)
	}
	// Describe shares admission and loaded keys but requires the exact selection
	// revision; malformed/stale requests must never reach the driver or enable MCP.
	describe := ManagementRequest{Operation: "describe", ProfileID: profile.ID, Alias: profile.Alias, Expected: staleRevision}
	if _, e = c.Request(t.Context(), describe); !errors.Is(e, config.ErrRevision) {
		t.Fatal("description accepted stale selection", e)
	}
	_, rev, e = config.Preview(t.Context(), c.Root)
	if e != nil {
		t.Fatal(e)
	}
	// Disconnect during network validation cannot publish a later save.
	driver.block.Store(true)
	validationDone := make(chan struct{})
	driver.mu.Lock()
	driver.validationDone = validationDone
	driver.mu.Unlock()
	cancelled, stopValidation := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() {
		_, err := c.Request(cancelled, ManagementRequest{Operation: "mutate", ProfileID: profile.ID, Mutation: &vault.Mutation{Expected: rev, Profiles: p}})
		result <- err
	}()
	select {
	case <-driver.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("cancellable validation did not start")
	}
	stopValidation()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatal("save cancellation", err)
	}
	select {
	case <-validationDone:
	case <-time.After(3 * time.Second):
		t.Fatal("validation did not observe cancellation")
	}
	driver.mu.Lock()
	driver.validationDone = nil
	driver.mu.Unlock()
	_, afterCancel, err := config.Preview(t.Context(), c.Root)
	if err != nil || afterCancel != rev {
		t.Fatal("cancelled validation published", err)
	}
	// A second, passwordless profile lets one test batch span several targets.
	driver.block.Store(false)
	second := fixtureProfile()
	second.Alias = "second"
	if second.ID, e = config.NewID(); e != nil {
		t.Fatal(e)
	}
	p.Connections = append(p.Connections, second)
	if _, e = apply(p, rev, nil); e != nil {
		t.Fatal("add batch profile", e)
	}
	if _, rev, e = config.Preview(t.Context(), c.Root); e != nil {
		t.Fatal(e)
	}
	describe.Expected = rev
	target := ProfileTarget{ProfileID: profile.ID, Alias: profile.Alias}
	for _, bad := range []ManagementRequest{
		{Operation: "describe", ProfileID: profile.ID, Alias: profile.Alias},
		{Operation: "browse", ProfileID: profile.ID, Alias: profile.Alias, Expected: rev},
		{Operation: "describe", ProfileID: profile.ID, Alias: profile.Alias, Expected: rev, Targets: []ProfileTarget{target}},
		{Operation: "test", ProfileID: profile.ID, Alias: profile.Alias},
		{Operation: "test", ProfileID: profile.ID, Targets: []ProfileTarget{target}},
		{Operation: "test", Targets: []ProfileTarget{target, target}},
		{Operation: "test", Targets: []ProfileTarget{{Alias: profile.Alias}}},
		{Operation: "credential-presence"},
		{Operation: "credential-presence", Expected: rev, Targets: []ProfileTarget{target}},
		{Operation: "credential-presence", Expected: rev, ProfileID: profile.ID},
		{Operation: "credential-presence", Expected: rev, Mutation: &vault.Mutation{Expected: rev, Profiles: p}},
	} {
		if _, e = c.Request(t.Context(), bad); !errors.Is(e, ErrState) {
			t.Fatal("invalid database request", bad, e)
		}
	}
	// CSV export learns which secret fields exist at the exact revision; only
	// flags cross the protocol, and the passwordless profile has no bundle.
	presence, e := c.Request(t.Context(), ManagementRequest{Operation: "credential-presence", Expected: rev})
	if e != nil || len(presence.Credentials) != 1 || presence.Credentials[0] != (CredentialPresence{ProfileID: profile.ID, Password: true}) {
		t.Fatal("credential presence", presence.Credentials, e)
	}
	if raw, _ := json.Marshal(presence); bytes.Contains(raw, []byte("synthetic")) {
		t.Fatal("credential presence exposed a secret")
	}
	if _, e = c.Request(t.Context(), ManagementRequest{Operation: "credential-presence", Expected: staleRevision}); !errors.Is(e, config.ErrRevision) {
		t.Fatal("stale credential presence", e)
	}
	// Four blocked database management requests consume the entire admission budget;
	// a fifth must fail promptly instead of waiting behind them. A test batch holds
	// one slot while its targets run concurrently, and every streamed result
	// arrives before its final reply.
	driver.block.Store(true)
	pending := make(chan error, 4)
	for i := 0; i < 4; i++ {
		go func() {
			var mu sync.Mutex
			var reported []string
			q := ManagementRequest{Operation: "test", Targets: []ProfileTarget{target}, Report: func(r DiagnosticResult) error {
				mu.Lock()
				defer mu.Unlock()
				reported = append(reported, r.Alias)
				return nil
			}}
			want := []string{profile.Alias}
			switch i {
			case 0, 2:
				q = describe
			case 1:
				q.Targets = append(q.Targets, ProfileTarget{ProfileID: second.ID, Alias: second.Alias})
				want = append(want, second.Alias)
			}
			r, e := c.Request(t.Context(), q)
			if e == nil && q.Operation == "describe" && (r.Description == nil || r.Description.Alias != profile.Alias || r.MCPEnabled) {
				e = errors.New("management description lost snapshot or enabled MCP")
			}
			mu.Lock()
			slices.Sort(reported)
			slices.Sort(want)
			if e == nil && q.Operation == "test" && !slices.Equal(reported, want) {
				e = errors.New("test results did not precede the final reply")
			}
			mu.Unlock()
			pending <- e
		}()
	}
	for i := 0; i < 5; i++ {
		select {
		case <-driver.entered:
		case <-time.After(3 * time.Second):
			t.Fatal("management request not admitted")
		}
	}
	if keys.calls() != calls {
		t.Fatal("description reloaded unlocked keyset")
	}
	bounded, finishAdmission := context.WithTimeout(t.Context(), time.Second)
	_, overload := c.Request(bounded, ManagementRequest{Operation: "test", Targets: []ProfileTarget{target}, Report: func(DiagnosticResult) error { return nil }})
	finishAdmission()
	if !errors.Is(overload, ErrUnavailable) {
		t.Fatal("management overload queued", overload)
	}
	close(driver.release)
	for i := 0; i < 4; i++ {
		if e := <-pending; e != nil {
			t.Fatal(e)
		}
	}
	p, rev, e = config.Preview(t.Context(), c.Root)
	if e != nil {
		t.Fatal(e)
	}
	p.Connections = slices.DeleteFunc(p.Connections, func(c config.Profile) bool { return c.ID == second.ID })
	if _, e = apply(p, rev, nil); e != nil {
		t.Fatal("remove batch profile", e)
	}
	if state, e = c.Start(t.Context()); e != nil || !state.MCPEnabled {
		t.Fatal("enable", state, e)
	}
	// Management operations sent to the MCP socket are rejected during handshake.
	s, e := config.OpenExisting(t.Context(), c.Root)
	if e != nil {
		t.Fatal(e)
	}
	l, e := s.ReadLease(t.Context())
	if e != nil {
		t.Fatal(e)
	}
	r, e := readRecord(l.Read, c.Root, l.Identity())
	l.Release()
	s.Close()
	if e != nil {
		t.Fatal(e)
	}
	conn, e := net.DialUnix("unix", nil, &net.UnixAddr{Name: filepath.Join(socketDir(c.Root), "s"), Net: "unix"})
	if e != nil {
		t.Fatal(e)
	}
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	_ = writeHello(conn, hello{Protocol: 3, Purpose: "management", Identity: r.Identity, Build: c.Build, PID: os.Getpid(), Nonce: r.Nonce})
	if _, e = readHello(conn); e == nil {
		t.Fatal("MCP dispatched management")
	}
	conn.Close()
	// Header length is rejected before allocating a request body.
	conn, _, e = connect(t.Context(), c.Root, r, c.Build, "management")
	if e != nil {
		t.Fatal(e)
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], managementLimit+1)
	_, _ = conn.Write(header[:])
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	var reply ManagementReply
	if e = readFrame(conn, &reply); e == nil {
		t.Fatal("oversized frame accepted")
	}
	conn.Close()
	for _, raw := range []string{`{"operation":"enable","operation":"mutate","interactive":false}`, `{"operation":"enable","interactive":false,"export":true}`, `{"operation":"get_password","interactive":false}`} {
		malformed, _, e := connect(t.Context(), c.Root, r, c.Build, "management")
		if e != nil {
			t.Fatal(e)
		}
		_ = malformed.SetDeadline(time.Now().Add(time.Second))
		var size [4]byte
		binary.BigEndian.PutUint32(size[:], uint32(len(raw)))
		_, _ = malformed.Write(size[:])
		_, _ = malformed.Write([]byte(raw))
		var frame managementFrame
		e = readFrame(malformed, &frame)
		malformed.Close()
		if e == nil && (frame.Reply == nil || frame.Reply.Error == "") {
			t.Fatal("invalid/private operation accepted")
		}
	}
	if _, e = c.Stop(t.Context()); e != nil {
		t.Fatal(e)
	}
	if _, e = c.EnsureManagement(t.Context()); e != nil {
		t.Fatal("locked management restart", e)
	}
	if state, e = c.Start(t.Context()); !errors.Is(e, vault.ErrDenied) || state.MCPEnabled {
		t.Fatal("enable denial", state, e)
	}
	if state, e = c.Inspect(t.Context()); e != nil || state.State != "running" || state.MCPEnabled {
		t.Fatal("denial killed management", state, e)
	}
	// Edits now require live validation and credentials; deletion remains available while locked.
	p, rev, e = config.Preview(t.Context(), c.Root)
	if e != nil {
		t.Fatal(e)
	}
	p.Connections[0].Alias = "renamed"
	if _, e = apply(p, rev, nil); !errors.Is(e, vault.ErrDenied) {
		t.Fatal("locked edit bypassed validation", e)
	}
	p, rev, e = config.Preview(t.Context(), c.Root)
	if e != nil {
		t.Fatal(e)
	}
	p.Connections = []config.Profile{}
	if _, e = apply(p, rev, nil); e != nil {
		t.Fatal("locked delete", e)
	}

	beforeStart := keys.calls()
	if state, e = c.Start(t.Context()); e != nil || !state.MCPEnabled || keys.calls() != beforeStart {
		t.Fatal("credential-free MCP start accessed locked keyset", e)
	}

	if err := filepath.WalkDir(c.Root.Path, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			raw, e := os.ReadFile(path)
			if e != nil {
				return e
			}
			for _, secret := range []string{"synthetic-keyring-password", "synthetic-management-secret"} {
				if bytes.Contains(raw, []byte(secret)) {
					t.Error("secret persisted", path)
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// An uninterruptible provider may complete late, but cannot publish a canceled save.
	c2, f2, _ := controllerFixture(t)
	blocked := &managedKeys{entered: make(chan struct{}), release: make(chan struct{})}
	entered := blocked.entered
	f2.keys = blocked
	if _, e = c2.EnsureManagement(t.Context()); e != nil {
		t.Fatal(e)
	}
	p, rev, e = config.Preview(t.Context(), c2.Root)
	if e != nil {
		t.Fatal(e)
	}
	p.Connections = append(p.Connections, profile)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	q := ManagementRequest{Operation: "mutate", Mutation: &vault.Mutation{Expected: rev, Profiles: p, Patches: map[string]vault.Patch{profile.ID: {"password": "synthetic-canceled"}}}}
	go func() { _, e := c2.Request(ctx, q); done <- e }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("provider not reached")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("provider blocked cancellation")
	}
	// State was not locked across the OS prompt, so passive reads remain available.
	inspect, finish := context.WithTimeout(t.Context(), time.Second)
	defer finish()
	current, _, e := config.Preview(inspect, c2.Root)
	if e != nil || len(current.Connections) != 0 {
		t.Fatal("prompt held state or published", e)
	}
	close(blocked.release)
	if _, e = c2.Stop(t.Context()); e != nil {
		t.Fatal("shutdown waited for late provider", e)
	}
	current, _, e = config.Preview(t.Context(), c2.Root)
	if e != nil || len(current.Connections) != 0 {
		t.Fatal("late publication", e)
	}
	// A service shutdown while the terminal is waiting must cancel the client
	// prompt too, even when the caller itself has no deadline or cancellation.
	f2.keys = &managedKeys{terminal: true}
	if _, e = c2.EnsureManagement(t.Context()); e != nil {
		t.Fatal(e)
	}
	promptEntered := make(chan struct{})
	terminalCtx := vault.WithKeyringPrompt(t.Context(), func(ctx context.Context, _ vault.KeyringChallenge) ([]byte, error) {
		close(promptEntered)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	q.Interactive = true
	go func() { _, e := c2.Request(terminalCtx, q); done <- e }()
	select {
	case <-promptEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("terminal prompt not reached")
	}
	f2.mu.Lock()
	shutdown := f2.cancel
	f2.mu.Unlock()
	shutdown()
	select {
	case e = <-done:
		if e == nil {
			t.Fatal("shutdown published")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("disconnection left terminal waiting")
	}
	current, _, e = config.Preview(t.Context(), c2.Root)
	if e != nil || len(current.Connections) != 0 {
		t.Fatal("disconnected preparation published", e)
	}
}
