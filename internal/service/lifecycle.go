package service

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/swqa7697/data-mate/internal/agent"
	"github.com/swqa7697/data-mate/internal/config"
)

// AgentStatus is independent user-scope registration readiness.
type AgentStatus = agent.Status

// Status is the versioned passive lifecycle report.
type Status struct {
	Version     int           `json:"version"`
	State       string        `json:"state"`
	Agents      []AgentStatus `json:"agents"`
	MCPEnabled  bool          `json:"mcp_enabled"`
	KeysetState string        `json:"keyset_state"`
}

func status(state string) Status {
	return Status{Version: 1, State: state, KeysetState: "absent", Agents: []AgentStatus{{Name: "codex", State: "pending"}, {Name: "claude", State: "pending"}}}
}

// Controller serializes lifecycle mutations through the installation store.
type Controller struct {
	Root        config.Root
	Build       Build
	launcher    launchManager
	readiness   time.Duration
	Agents      *agent.Manager
	Interactive bool
}

// New constructs a native lifecycle controller without accessing state.
func New(root config.Root, build Build) *Controller {
	return &Controller{Root: root, Build: build, launcher: launchd{}, readiness: 30 * time.Second}
}
func (c *Controller) inspect(ctx context.Context, r record, l *config.LifecycleLease) (job, error) {
	j, err := c.launcher.Inspect(ctx, c.Root)
	if err != nil {
		return j, err
	}
	if j.Present {
		if !matching(j, r) {
			return j, ErrConflict
		}
		b, err := l.Read("state/service.plist", 16384)
		if err != nil || !bytes.Equal(b, plist(c.Root, r)) {
			return j, ErrConflict
		}
	}
	return j, nil
}
func (c *Controller) clearRuntime(r record) error {
	d, err := openRuntime(c.Root, r.Identity, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer d.file.Close()
	return d.cleanup()
}
func (c *Controller) stopLocked(ctx context.Context, l *config.LifecycleLease, r record) error {
	j, err := c.inspect(ctx, r, l)
	if err != nil {
		return err
	}
	if j.Present {
		// bootout targets the verified launchd job, never a PID from a state file.
		// ExitTimeOut=5 gives the service time to cancel requests and close resources.
		bootErr := c.launcher.Bootout(ctx, c.Root)
		wait, cancel := context.WithTimeout(ctx, 6*time.Second)
		defer cancel()
		tick := time.NewTicker(25 * time.Millisecond)
		defer tick.Stop()
		for {
			j, err = c.launcher.Inspect(wait, c.Root)
			if err != nil {
				return err
			}
			if !j.Present {
				break
			}
			if !matching(j, r) {
				return ErrConflict
			}
			if bootErr != nil {
				return bootErr
			}
			select {
			case <-wait.Done():
				return ErrUnavailable
			case <-tick.C:
			}
		}
	}
	if err = c.clearRuntime(r); err != nil {
		return err
	}
	for _, path := range []string{"state/service.plist.tmp", "state/service.plist", "state/service.json.tmp", "state/service.json"} {
		if err = l.Remove(path); err != nil {
			return err
		}
	}
	return nil
}

// Start starts or reuses this exact installation, then ensures CLI agent registrations.
func (c *Controller) EnsureManagement(parent context.Context) (Status, error) {
	ctx, cancel := context.WithTimeout(parent, 40*time.Second)
	defer cancel()
	if !safePath(c.Root) {
		return status("stale"), ErrState
	}
	if _, _, err := config.Preview(ctx, c.Root); err != nil {
		return status("stale"), ErrState
	}
	s, err := config.Open(ctx, c.Root, nil)
	if err != nil {
		return status("stale"), ErrState
	}
	defer s.Close()
	l, err := s.Lifecycle(ctx)
	if err != nil {
		return status("stale"), err
	}
	defer l.Release()
	hash, err := binaryHash(filepath.Join(c.Root.Path, "bin/data-mate"))
	if err != nil {
		return status("stale"), err
	}
	if hash != c.Build.Fingerprint {
		return status("stale"), ErrRestart
	}
	r, err := readRecord(l.Read, c.Root, l.Identity())
	if err == nil {
		j, e := c.inspect(ctx, r, l)
		if e != nil {
			return status("stale"), e
		}
		if j.Present && j.PID > 0 {
			conn, h, e := connect(ctx, c.Root, r, c.Build, "probe")
			if e == nil {
				conn.Close()
				if h.PID != j.PID {
					return status("stale"), ErrConflict
				}
				if h.State != "running" {
					return status(h.State), ErrStartup
				}
				return helloStatus(h), nil
			}
			// Never replace a live but unresponsive process on a repeated start.
			return status("stale"), e
		}
		if e = c.stopLocked(ctx, l, r); e != nil {
			return status("stale"), e
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return status("stale"), err
	} else {
		j, e := c.launcher.Inspect(ctx, c.Root)
		if e != nil {
			return status("stale"), e
		}
		if j.Present {
			return status("stale"), ErrConflict
		}
	}
	nonce, err := config.NewID()
	if err != nil {
		return status("stopped"), ErrStartup
	}
	r = record{2, installation(c.Root, l.Identity()), c.Build, nonce, 0}
	runtime, err := openRuntime(c.Root, r.Identity, true)
	if err != nil {
		return status("stale"), err
	}
	runtime.name = "m"
	err = runtime.removeStaleSocket()
	if err == nil {
		runtime.name = "s"
		err = runtime.removeStaleSocket()
	}
	runtime.file.Close()
	if err != nil {
		return status("stale"), err
	}
	if err = saveRecord(l, r); err != nil {
		return status("stopped"), err
	}
	if err = l.Replace("state/service.plist", plist(c.Root, r)); err != nil {
		return status("stopped"), err
	}
	bootErr := c.launcher.Bootstrap(ctx, c.Root)
	if bootErr == nil {
		ready, done := context.WithTimeout(ctx, c.readiness)
		defer done()
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for ready.Err() == nil {
			j, e := c.inspect(ready, r, l)
			if e != nil {
				if ready.Err() == nil {
					bootErr = e
				}
				break
			}
			if j.Present && j.PID > 0 {
				conn, h, e := connect(ready, c.Root, r, c.Build, "probe")
				if e == nil {
					conn.Close()
					if h.PID != j.PID {
						bootErr = ErrConflict
						break
					}
					if h.State == "running" {
						r.PID = h.PID
						if e = saveRecord(l, r); e == nil {
							return helloStatus(h), nil
						}
						bootErr = e
						break
					}
				} else if errors.Is(e, ErrConflict) || errors.Is(e, ErrRestart) {
					bootErr = e
					break
				}
			}
			select {
			case <-ready.Done():
			case <-tick.C:
			}
		}
	}
	// Cancellation/timeouts still clean only this verified partial launch.
	cleanup, finish := context.WithTimeout(context.Background(), 10*time.Second)
	defer finish()
	if err = c.stopLocked(cleanup, l, r); err != nil {
		return status("stale"), err
	}
	if parent.Err() != nil {
		return status("stopped"), parent.Err()
	}
	if bootErr != nil && !errors.Is(bootErr, ErrStartup) {
		return status("stopped"), bootErr
	}
	return status("stopped"), ErrStartup
}

// Stop is idempotent. Invalid profile data does not prevent owned job cleanup.
func (c *Controller) Stop(ctx context.Context) (Status, error) {
	s, err := config.OpenLifecycle(ctx, c.Root)
	if errors.Is(err, os.ErrNotExist) {
		j, e := c.launcher.Inspect(ctx, c.Root)
		if e != nil {
			return status("stale"), e
		}
		if j.Present {
			return status("stale"), ErrConflict
		}
		return status("stopped"), nil
	}
	if err != nil {
		return status("stale"), ErrState
	}
	defer s.Close()
	l, err := s.Lifecycle(ctx)
	if err != nil {
		return status("stale"), err
	}
	defer l.Release()
	r, err := readRecord(l.Read, c.Root, l.Identity())
	if errors.Is(err, os.ErrNotExist) {
		j, e := c.launcher.Inspect(ctx, c.Root)
		if e != nil {
			return status("stale"), e
		}
		if j.Present {
			return status("stale"), ErrConflict
		}
		return stoppedStatus(ctx, s), nil
	}
	if err != nil {
		return status("stale"), err
	}
	if err = c.stopLocked(ctx, l, r); err != nil {
		return status("stale"), err
	}
	return stoppedStatus(ctx, s), nil
}

// Inspect reads profiles and probes an already-running job. No secrets, database
// connections, registration commands, process startup or state repair occur.
func (c *Controller) Inspect(ctx context.Context) (result Status, resultErr error) {
	defer func() {
		if c.Agents != nil {
			states, err := c.Agents.Inspect(ctx)
			result.Agents = states
			if resultErr == nil {
				resultErr = err
			}
		}
	}()
	configErr := error(nil)
	if _, _, err := config.Preview(ctx, c.Root); err != nil {
		configErr = ErrState
	}
	s, err := config.OpenExisting(ctx, c.Root)
	if errors.Is(err, os.ErrNotExist) {
		j, e := c.launcher.Inspect(ctx, c.Root)
		if e != nil {
			return status("stale"), e
		}
		if j.Present {
			return status("stale"), ErrConflict
		}
		return status("stopped"), configErr
	}
	if err != nil {
		return status("stale"), ErrState
	}
	defer s.Close()
	l, err := s.ReadLease(ctx)
	if err != nil {
		return status("stale"), ErrState
	}
	k, keyErr := l.Keyset()
	defer func() {
		if result.State == "stopped" {
			if keyErr != nil {
				result.KeysetState = "unavailable"
			} else if k.Phase != "" {
				result.KeysetState = "locked"
			}
		}
	}()
	r, recordErr := readRecord(l.Read, c.Root, l.Identity())
	l.Release()
	j, err := c.launcher.Inspect(ctx, c.Root)
	if err != nil {
		return status("stale"), err
	}
	if errors.Is(recordErr, os.ErrNotExist) && !j.Present {
		return status("stopped"), configErr
	}
	if recordErr != nil {
		return status("stale"), ErrState
	}
	if !j.Present || j.PID == 0 {
		return status("stale"), configErr
	}
	if !matching(j, r) {
		return status("stale"), ErrConflict
	}
	conn, h, err := connect(ctx, c.Root, r, c.Build, "probe")
	if err != nil {
		if r.PID == 0 && errors.Is(err, ErrUnavailable) {
			return status("starting"), configErr
		}
		return status("stale"), err
	}
	conn.Close()
	if h.PID != j.PID {
		return status("stale"), ErrConflict
	}
	if configErr != nil {
		return status("degraded"), configErr
	}
	if h.State != "running" && h.State != "degraded" && h.State != "stopped" {
		return status("stale"), ErrState
	}
	return helloStatus(h), nil
}

// OpenSession authenticates a socket without starting a stopped service.
func (c *Controller) OpenSession(ctx context.Context) (*net.UnixConn, error) {
	s, err := config.OpenExisting(ctx, c.Root)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer s.Close()
	l, err := s.ReadLease(ctx)
	if err != nil {
		return nil, ErrUnavailable
	}
	r, err := readRecord(l.Read, c.Root, l.Identity())
	l.Release()
	if err != nil {
		return nil, ErrUnavailable
	}
	conn, _, err := connect(ctx, c.Root, r, c.Build, "session")
	return conn, err
}

// ProbeSession checks the authenticated session path and immediately disconnects.
func (c *Controller) ProbeSession(ctx context.Context) error {
	conn, err := c.OpenSession(ctx)
	if conn != nil {
		conn.Close()
	}
	return err
}

func helloStatus(h hello) Status {
	s := status(h.State)
	s.MCPEnabled = h.MCPEnabled
	s.KeysetState = h.KeysetState
	return s
}

// Start enables MCP on the persistent management process, then ensures registrations.
func (c *Controller) Start(ctx context.Context) (Status, error) {
	state, err := c.EnsureManagement(ctx)
	if err != nil {
		return state, err
	}
	reply, err := c.Request(ctx, ManagementRequest{Operation: "enable", Interactive: c.Interactive})
	if reply.KeysetState != "" {
		state.MCPEnabled = reply.MCPEnabled
		state.KeysetState = reply.KeysetState
	}
	if err != nil {
		return state, err
	}
	state.MCPEnabled = reply.MCPEnabled
	state.KeysetState = reply.KeysetState
	s, err := config.OpenExisting(ctx, c.Root)
	if err != nil {
		return state, err
	}
	defer s.Close()
	l, err := s.Lifecycle(ctx)
	if err != nil {
		return state, err
	}
	defer l.Release()
	if c.Agents != nil {
		state.Agents, err = c.Agents.Ensure(ctx, l)
	}
	return state, err
}

func stoppedStatus(ctx context.Context, s *config.Store) Status {
	result := status("stopped")
	l, e := s.ReadLease(ctx)
	if e != nil {
		result.KeysetState = "unavailable"
		return result
	}
	defer l.Release()
	k, e := l.Keyset()
	if e != nil {
		result.KeysetState = "unavailable"
	} else if k.Phase != "" {
		result.KeysetState = "locked"
	}
	return result
}
