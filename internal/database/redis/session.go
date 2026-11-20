package redis

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
	"github.com/swqa7697/data-mate/internal/transport"
)

// A checkout has exclusive use of the session. Cancellation may close the
// route concurrently to interrupt both reads and writes, including over SSH.
type session struct {
	raw    net.Conn
	input  *io.PipeReader
	reader *bufio.Reader
	writer *bufio.Writer
	done   chan struct{}
	once   sync.Once
}

func newSession(raw, conn net.Conn) *session {
	input, output := io.Pipe()
	s := &session{raw: raw, input: input, reader: bufio.NewReader(input), writer: bufio.NewWriter(conn), done: make(chan struct{})}
	// Observe EOF even while the session is idle. Otherwise a disconnected
	// final session could retain the pool's cached account approval.
	go func() {
		_, err := io.Copy(output, conn)
		_ = output.CloseWithError(err)
		s.Close()
	}()
	return s
}

func (s *session) Closed() <-chan struct{} { return s.done }

func (s *session) Reusable() bool {
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}

func (s *session) Close() {
	s.once.Do(func() {
		_ = s.input.Close()
		_ = s.raw.Close()
		close(s.done)
	})
}

func (s *session) command(ctx context.Context, args ...string) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, database.CommonError(err)
	}
	finished := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		s.Close()
		close(finished)
	})
	defer func() {
		if !stop() {
			<-finished
		}
	}()
	err := writeCommand(s.writer, args)
	var result any
	if err == nil {
		r := responseReader{r: s.reader, remaining: maxReplyBytes}
		result, err = r.value(0)
	}
	if err != nil {
		s.Close()
		if ctx.Err() != nil {
			return nil, database.CommonError(ctx.Err())
		}
		if errors.Is(err, errReplyLimit) {
			return nil, database.Fail(contracts.ResourceLimit, "Redis response exceeds receive limit", false)
		}
		return nil, database.Fail(contracts.ConnectFailed, "Redis connection or protocol failed", true)
	}
	if failed, ok := result.(replyError); ok {
		return nil, failed.safe()
	}
	return result, nil
}

type route struct {
	dial func(context.Context, string, string) (net.Conn, error)
	tls  *tls.Config
}

func makeRoute(a database.Access) (route, error) {
	secrets, hosts := a.TransportCredentials()
	dial, err := transport.Dialer(a.Profile.Transport, secrets, hosts)
	if err != nil {
		return route{}, connectError(err)
	}
	t, err := transport.TLSConfig(a.Profile.Connection.Host, a.Profile.Transport.TLS)
	if err != nil {
		return route{}, database.Fail(contracts.ConfigInvalid, "cannot load Redis TLS configuration", false)
	}
	return route{dial: dial, tls: t}, nil
}

func connectError(err error) error {
	if safe := database.CommonError(err); safe != nil {
		return safe
	}
	return database.Fail(contracts.ConnectFailed, "Redis connection failed", true)
}

// HELLO in RESP2 provides version and topology without INFO privileges.
func connect(ctx context.Context, a database.Access, trace *database.Trace) (*session, error) {
	r, err := makeRoute(a)
	if err != nil {
		return nil, err
	}
	setup, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	p := a.Profile.Connection
	raw, err := r.dial(setup, "tcp", net.JoinHostPort(p.Host, strconv.Itoa(p.Port)))
	if err != nil {
		return nil, connectError(err)
	}
	conn := raw
	if r.tls != nil {
		tc := tls.Client(raw, r.tls)
		if err := tc.HandshakeContext(setup); err != nil {
			raw.Close()
			if setup.Err() != nil {
				return nil, database.CommonError(setup.Err())
			}
			return nil, database.Fail(contracts.ConnectFailed, "Redis TLS verification or handshake failed", false)
		}
		conn = tc
	}
	s := newSession(raw, conn)
	success := false
	defer func() {
		if !success {
			s.Close()
		}
	}()
	trace.Pass("dial")
	trace.Start("authentication")
	hello, err := s.command(setup, "HELLO", "2", "AUTH", p.Username, a.Password())
	if err != nil {
		return nil, err
	}
	user, err := s.command(setup, "ACL", "WHOAMI")
	if err != nil {
		return nil, err
	}
	if user != p.Username {
		return nil, database.Fail(contracts.ConnectFailed, "Redis authenticated identity does not match the profile", false)
	}
	trace.Pass("authentication")
	trace.Start("version")
	info, ok := fields(hello)
	versionText, _ := info["version"].(string)
	version, okVersion := redisVersion(versionText)
	if !ok || !okVersion || info["server"] != "redis" || info["proto"] != int64(2) || info["mode"] != "standalone" || version < 70000 {
		return nil, database.Fail(contracts.QueryUnsupported, "standalone Redis 7.0 or later is required", false)
	}
	if trace != nil {
		trace.Version = version
	}
	trace.Pass("version")
	trace.Start("read_only")
	selected, err := s.command(setup, "SELECT", p.Database)
	if err != nil {
		return nil, err
	}
	if selected != "OK" {
		return nil, database.Fail(contracts.ConnectFailed, "Redis database selection failed", false)
	}
	success = true
	return s, nil
}

func redisVersion(s string) (int, bool) {
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return 0, false
	}
	version := 0
	for _, part := range parts {
		if len(part) == 0 || len(part) > 2 {
			return 0, false
		}
		for _, c := range part {
			if c < '0' || c > '9' {
				return 0, false
			}
		}
		n, _ := strconv.Atoi(part)
		version = version*100 + n
	}
	return version, true
}
