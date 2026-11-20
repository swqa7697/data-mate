package mongodb

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"runtime"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
	"github.com/swqa7697/data-mate/internal/transport"
	"go.mongodb.org/mongo-driver/v2/bson"
)

const (
	setupTimeout   = 10 * time.Second
	cleanupTimeout = 2 * time.Second
	// Sizes assumed before hello reports the server's own.
	defaultMaxDocument = 16 << 20
	defaultMaxMessage  = 48_000_000
	// minWireVersion is MongoDB 7.0.
	minWireVersion = 21
	minVersion     = 70000
)

// A checkout has exclusive use of the session. Cancellation may close the
// route concurrently to interrupt both reads and writes, including over SSH.
// The session never sends lsid, so no server state is bound to it.
type session struct {
	raw         net.Conn
	input       *io.PipeReader
	reader      *bufio.Reader
	writer      *bufio.Writer
	done        chan struct{}
	once        sync.Once
	id          int32
	member      bool
	maxDocument int
	maxMessage  int
}

func newSession(raw, conn net.Conn) *session {
	input, output := io.Pipe()
	s := &session{raw: raw, input: input, reader: bufio.NewReader(input), writer: bufio.NewWriter(conn), done: make(chan struct{}), maxDocument: defaultMaxDocument, maxMessage: defaultMaxMessage}
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

// control sends a driver command whose reply is small and bounded.
func (s *session) control(ctx context.Context, db string, cmd bson.D) (bson.Raw, error) {
	return s.roundTrip(ctx, db, cmd, controlReplyLimit)
}

// result sends a command whose reply can carry documents up to the server's
// advertised object size.
func (s *session) result(ctx context.Context, db string, cmd bson.D) (bson.Raw, error) {
	return s.roundTrip(ctx, db, cmd, min(s.maxMessage, replyOverhead+s.maxDocument+documentSlack))
}

// roundTrip sends one command and reads its reply. Transport, framing and
// limit failures close the session and return safe errors. A server failure
// returns serverError for the caller to classify.
func (s *session) roundTrip(ctx context.Context, db string, cmd bson.D, limit int) (bson.Raw, error) {
	if err := ctx.Err(); err != nil {
		return nil, database.CommonError(err)
	}
	full := append(slices.Clip(cmd), kv("$db", db))
	if s.member {
		full = append(full, kv("$readPreference", bson.D{kv("mode", "primaryPreferred")}))
	}
	body, err := bson.Marshal(full)
	if err != nil {
		return nil, database.Fail(contracts.InvalidArgument, "invalid MongoDB command document", false)
	}
	// Nothing has been written, so the session remains usable.
	if len(body) > s.maxDocument+documentSlack || requestOverhead+len(body) > s.maxMessage {
		return nil, database.Fail(contracts.ResourceLimit, "MongoDB command exceeds the server message size limit", false)
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
	s.id++
	if s.id == 0 {
		s.id = 1
	}
	err = s.write(body)
	var reply bson.Raw
	if err == nil {
		reply, err = readReply(s.reader, s.id, limit)
	}
	if err != nil {
		s.Close()
		if ctx.Err() != nil {
			return nil, database.CommonError(ctx.Err())
		}
		if errors.Is(err, errReplyLimit) {
			return nil, receiveLimit()
		}
		return nil, protocolFailure()
	}
	if err := status(reply); err != nil {
		var server serverError
		if !errors.As(err, &server) {
			s.Close()
			return nil, protocolFailure()
		}
		if server.closes() {
			s.Close()
		}
		return nil, server
	}
	return reply, nil
}

func (s *session) write(body []byte) error {
	var header [requestOverhead]byte
	binary.LittleEndian.PutUint32(header[0:], uint32(requestOverhead+len(body)))
	binary.LittleEndian.PutUint32(header[4:], uint32(s.id))
	binary.LittleEndian.PutUint32(header[12:], opMsg)
	_, _ = s.writer.Write(header[:])
	_, _ = s.writer.Write(body)
	// bufio.Writer retains the first error and reports it from Flush.
	return s.writer.Flush()
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
		return route{}, database.Fail(contracts.ConfigInvalid, "cannot load MongoDB TLS configuration", false)
	}
	return route{dial: dial, tls: t}, nil
}

func connectError(err error) error {
	if safe := database.CommonError(err); safe != nil {
		return safe
	}
	return database.Fail(contracts.ConnectFailed, "MongoDB connection failed", true)
}

// setupFailure classifies a server failure of a handshake command.
func setupFailure(err error, message string) error {
	var server serverError
	if errors.As(err, &server) {
		return database.Fail(contracts.ConnectFailed, message, true)
	}
	return err
}

func unsupportedServer() error {
	return database.Fail(contracts.QueryUnsupported, "MongoDB 7.0 or later on a standalone server, replica-set data member or mongos is required", false)
}

// connect opens one direct session. Each step maps to a diagnostic stage: dial
// covers the route, TLS and hello; an unsupported topology or wire version
// fails at version before any credential is sent; authentication is SCRAM and
// the identity check; version is buildInfo. The account audit runs later, when
// the pool approves the session.
func connect(ctx context.Context, a database.Access, trace *database.Trace) (*session, error) {
	r, err := makeRoute(a)
	if err != nil {
		return nil, err
	}
	setup, cancel := context.WithTimeout(ctx, setupTimeout)
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
			return nil, database.Fail(contracts.ConnectFailed, "MongoDB TLS verification or handshake failed", false)
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
	reply, err := s.control(setup, "admin", helloCommand(p.Database, p.Username))
	if err != nil {
		return nil, setupFailure(err, "MongoDB handshake failed")
	}
	info, err := parseHello(reply)
	if err != nil {
		return nil, protocolFailure()
	}
	s.maxDocument, s.maxMessage, s.member = info.maxDocument, info.maxMessage, info.member
	trace.Pass("dial")
	if !info.supported {
		trace.Start("version")
		return nil, unsupportedServer()
	}
	trace.Start("authentication")
	if err := s.authenticate(setup, a, info.mechanisms); err != nil {
		return nil, err
	}
	if err := s.verifyIdentity(setup, p.Username, p.Database); err != nil {
		return nil, err
	}
	trace.Pass("authentication")
	trace.Start("version")
	version, err := s.serverVersion(setup)
	if err != nil {
		return nil, err
	}
	if trace != nil {
		trace.Version = version
	}
	trace.Pass("version")
	trace.Start("read_only")
	success = true
	return s, nil
}

// helloCommand reports only a fixed client name and the OS type. It requests
// no compression, load balancing or speculative authentication; the
// saslSupportedMechs lookup names the account without sending a credential.
func helloCommand(authDB, user string) bson.D {
	return bson.D{
		kv("hello", int32(1)),
		kv("client", bson.D{
			kv("driver", bson.D{kv("name", "data-mate"), kv("version", "unreported")}),
			kv("os", bson.D{kv("type", runtime.GOOS)}),
		}),
		kv("compression", bson.A{}),
		kv("saslSupportedMechs", authDB+"."+user),
	}
}

type helloInfo struct {
	maxDocument, maxMessage int
	member, supported       bool
	// mechanisms is nil when the server did not report the account's mechanisms.
	mechanisms []string
}

// parseHello classifies the server from its own hello reply only. Replica-set
// members are used directly with primaryPreferred and are never failed over.
func parseHello(reply bson.Raw) (helloInfo, error) {
	top, err := fields(reply)
	if err != nil {
		return helloInfo{}, err
	}
	wire, ok := top["maxWireVersion"].Int32OK()
	maxDocument, okDocument := top["maxBsonObjectSize"].Int32OK()
	maxMessage, okMessage := top["maxMessageSizeBytes"].Int32OK()
	if !ok || !okDocument || !okMessage || maxDocument < 1<<20 || maxDocument > 16<<20 || maxMessage < maxDocument || maxMessage > defaultMaxMessage {
		return helloInfo{}, errProtocol
	}
	info := helloInfo{maxDocument: int(maxDocument), maxMessage: int(maxMessage)}
	if v, ok := top["compression"]; ok {
		if list, err := values(v, 0); err != nil || len(list) != 0 {
			return helloInfo{}, errProtocol
		}
	}
	if v, ok := top["saslSupportedMechs"]; ok {
		list, err := values(v, 16)
		if err != nil {
			return helloInfo{}, errProtocol
		}
		info.mechanisms = make([]string, 0, len(list))
		for _, item := range list {
			mechanism, ok := item.StringValueOK()
			if !ok {
				return helloInfo{}, errProtocol
			}
			info.mechanisms = append(info.mechanisms, mechanism)
		}
	}
	var flags [4]bool
	for i, key := range []string{"isreplicaset", "arbiterOnly", "isWritablePrimary", "secondary"} {
		if flags[i], err = boolField(top, key); err != nil {
			return helloInfo{}, err
		}
	}
	ghost, arbiter, primary, secondary := flags[0], flags[1], flags[2], flags[3]
	var text [2]string
	for i, key := range []string{"msg", "setName"} {
		v, ok := top[key]
		if !ok {
			continue
		}
		if text[i], ok = v.StringValueOK(); !ok {
			return helloInfo{}, errProtocol
		}
	}
	router, set := text[0] == "isdbgrid", text[1] != ""
	_, loadBalanced := top["serviceId"]
	switch {
	case loadBalanced, router && set, ghost:
	case router:
		info.supported = true
	case set && !arbiter && (primary || secondary):
		info.supported, info.member = true, true
	case !set:
		info.supported = true
	}
	if wire < minWireVersion {
		info.supported = false
	}
	return info, nil
}

// serverVersion reads buildInfo's versionArray as major*10000+minor*100+patch.
func (s *session) serverVersion(ctx context.Context) (int, error) {
	reply, err := s.control(ctx, "admin", bson.D{kv("buildInfo", int32(1))})
	if err != nil {
		return 0, setupFailure(err, "MongoDB connection setup failed")
	}
	top, err := fields(reply)
	if err != nil {
		return 0, protocolFailure()
	}
	parts, err := values(top["versionArray"], 8)
	if err != nil || len(parts) < 3 {
		return 0, unsupportedServer()
	}
	version := 0
	for i, part := range parts[:3] {
		limit := int32(99)
		if i == 0 {
			limit = 9999
		}
		n, ok := part.Int32OK()
		if !ok || n < 0 || n > limit {
			return 0, unsupportedServer()
		}
		version = version*100 + int(n)
	}
	if version < minVersion {
		return 0, unsupportedServer()
	}
	return version, nil
}
