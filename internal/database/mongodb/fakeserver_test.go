package mongodb

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/database/pool"
	"github.com/xdg-go/scram"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// Protocol values a client must reject; the driver never sends them.
const (
	opCompressed   = 2012
	flagMoreToCome = 1 << 1
)

// Synthetic account. The secret also appears in every failure the fake server
// sends, so each case proves that server text is never surfaced.
const (
	fakeUser      = "reader"
	fakeAuthDB    = "app"
	fakeSecret    = "synthetic-secret-7f3a"
	fakeProfileID = "12345678-1234-1234-1234-123456789abc"
)

func fakeProfile(port int, limits config.Limits) config.Profile {
	return config.Profile{Driver: "mongodb", Alias: "fixture", ID: fakeProfileID, Connection: config.Connection{Host: "127.0.0.1", Port: port, Username: fakeUser, Database: fakeAuthDB}, Transport: config.Transport{TLS: config.TLS{Mode: "disabled"}}, Limits: &limits}
}

func fakeRegistry(t *testing.T) *pool.Registry {
	t.Helper()
	r, err := pool.New(pool.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	return r
}

// exchange expects one command and answers it. reply builds the complete
// response frame for the request id; nil cancels the client while the peer
// holds the response.
type exchange struct {
	command string
	check   func(req bson.Raw) error
	reply   func(req bson.Raw, id int32) []byte
}

// fakePeer serves one scripted exchange list per accepted connection, in
// order, then holds each connection until the client closes it. Requests are
// parsed independently of the driver's readReply.
type fakePeer struct {
	ln       net.Listener
	cancel   context.CancelFunc
	member   bool
	deadline time.Time
	done     chan error
}

// newPeer starts a peer. cancel is called for a nil reply; member requires
// primaryPreferred on every command after hello.
func newPeer(t *testing.T, cancel context.CancelFunc, member bool, scripts ...[]exchange) *fakePeer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	p := &fakePeer{ln: ln, cancel: cancel, member: member, deadline: time.Now().Add(5 * time.Second), done: make(chan error, 1)}
	// A connection the driver never opens fails the test instead of hanging it.
	if err := ln.(*net.TCPListener).SetDeadline(p.deadline); err != nil {
		t.Fatal(err)
	}
	go func() { p.done <- p.serve(scripts) }()
	return p
}

func (p *fakePeer) port() int { return p.ln.Addr().(*net.TCPAddr).Port }

func (p *fakePeer) wait(t *testing.T) {
	t.Helper()
	if err := <-p.done; err != nil {
		t.Fatal(err)
	}
}

func (p *fakePeer) serve(scripts [][]exchange) error {
	var wg sync.WaitGroup
	errs := make([]error, len(scripts))
	for i, script := range scripts {
		c, err := p.ln.Accept()
		if err != nil {
			errs[i] = fmt.Errorf("accept %d: %w", i, err)
			break
		}
		wg.Go(func() {
			defer c.Close()
			if err := p.session(c, script); err != nil {
				errs[i] = fmt.Errorf("connection %d: %w", i, err)
			}
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}

func (p *fakePeer) session(c net.Conn, script []exchange) error {
	if err := c.SetDeadline(p.deadline); err != nil {
		return err
	}
	r := bufio.NewReader(c)
	for i, step := range script {
		id, req, err := readRequest(r)
		if err != nil {
			return fmt.Errorf("exchange %d: %w", i, err)
		}
		name := ""
		if first, err := req.IndexErr(0); err == nil {
			name = first.Key()
		}
		if name != step.command {
			return fmt.Errorf("exchange %d: command %q, want %q", i, name, step.command)
		}
		if err := p.common(req, name); err != nil {
			return fmt.Errorf("exchange %d (%s): %w", i, name, err)
		}
		if step.check != nil {
			if err := step.check(req); err != nil {
				return fmt.Errorf("exchange %d (%s): %w", i, name, err)
			}
		}
		if step.reply == nil {
			p.cancel()
			break
		}
		if _, err := c.Write(step.reply(req, id)); err != nil {
			return err
		}
	}
	// Keep the connection open until the driver or its pool closes it.
	_, err := r.ReadByte()
	if errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET) {
		return nil
	}
	if err == nil {
		return errors.New("unexpected request after the script")
	}
	return fmt.Errorf("cleanup: %w", err)
}

// common checks generic arguments: the driver never sends session, transaction
// or read-concern state, never asks for credentials, and routes reads on
// replica-set members with primaryPreferred.
func (p *fakePeer) common(req bson.Raw, name string) error {
	if _, ok := req.Lookup("$db").StringValueOK(); !ok {
		return errors.New("missing $db")
	}
	for _, key := range []string{"lsid", "txnNumber", "readConcern", "showCredentials"} {
		if _, err := req.LookupErr(key); err == nil {
			return fmt.Errorf("unexpected %s", key)
		}
	}
	pref, err := req.LookupErr("$readPreference")
	if want := p.member && name != "hello"; want != (err == nil) {
		return fmt.Errorf("$readPreference present %t, want %t", err == nil, want)
	}
	if err == nil && !bytes.Equal(pref.Value, marshal(bson.D{kv("mode", "primaryPreferred")})) {
		return errors.New("unexpected $readPreference")
	}
	return nil
}

func readRequest(r *bufio.Reader) (int32, bson.Raw, error) {
	var header [16]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil, err
	}
	length := int(binary.LittleEndian.Uint32(header[0:]))
	id := int32(binary.LittleEndian.Uint32(header[4:]))
	if length < 26 || length > 1<<20 || binary.LittleEndian.Uint32(header[8:]) != 0 || binary.LittleEndian.Uint32(header[12:]) != opMsg {
		return 0, nil, errors.New("invalid request header")
	}
	body := make([]byte, length-16)
	if _, err := io.ReadFull(r, body); err != nil {
		return 0, nil, err
	}
	doc := bson.Raw(body[5:])
	if binary.LittleEndian.Uint32(body) != 0 || body[4] != 0 || int(binary.LittleEndian.Uint32(doc)) != len(doc) || doc.Validate() != nil {
		return 0, nil, errors.New("invalid request body")
	}
	return id, doc, nil
}

func marshal(v any) []byte {
	b, err := bson.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// frame builds a reply message with an optional CRC-32C checksum.
func frame(opcode int32, responseTo int32, flags uint32, body []byte, checksum bool) []byte {
	size := 16 + 4 + 1 + len(body)
	if checksum {
		size += 4
		flags |= flagChecksum
	}
	out := binary.LittleEndian.AppendUint32(nil, uint32(size))
	out = binary.LittleEndian.AppendUint32(out, uint32(responseTo)+1000)
	out = binary.LittleEndian.AppendUint32(out, uint32(responseTo))
	out = binary.LittleEndian.AppendUint32(out, uint32(opcode))
	out = binary.LittleEndian.AppendUint32(out, flags)
	out = append(append(out, 0), body...)
	if checksum {
		out = binary.LittleEndian.AppendUint32(out, crc32.Checksum(out, crc32.MakeTable(crc32.Castagnoli)))
	}
	return out
}

func opMsgFrame(responseTo int32, flags uint32, body []byte, checksum bool) []byte {
	return frame(opMsg, responseTo, flags, body, checksum)
}

func send(body bson.D) func(bson.Raw, int32) []byte {
	return func(_ bson.Raw, id int32) []byte { return opMsgFrame(id, 0, marshal(body), false) }
}

func respond(command string, body bson.D) exchange {
	return exchange{command: command, reply: send(body)}
}

func ok(elements ...bson.E) bson.D { return append(bson.D(elements), kv("ok", 1.0)) }

func failure(code int32) bson.D {
	return bson.D{kv("ok", 0.0), kv("errmsg", "synthetic failure for "+fakeSecret), kv("code", code), kv("codeName", "Synthetic")}
}

func helloReply(wire int32, topology ...bson.E) bson.D {
	return ok(append(topology,
		kv("maxBsonObjectSize", int32(16<<20)), kv("maxMessageSizeBytes", int32(48_000_000)), kv("maxWriteBatchSize", int32(100_000)),
		kv("maxWireVersion", wire), kv("minWireVersion", int32(0)), kv("saslSupportedMechs", bson.A{"SCRAM-SHA-256"}))...)
}

var standalone = helloReply(25, kv("isWritablePrimary", true))

// hello requires exactly the fixed client metadata: no application name,
// environment, compression, load balancing or speculative authentication.
func hello(reply func(bson.Raw, int32) []byte) exchange {
	want := marshal(bson.D{
		kv("hello", int32(1)),
		kv("client", bson.D{kv("driver", bson.D{kv("name", "data-mate"), kv("version", "unreported")}), kv("os", bson.D{kv("type", runtime.GOOS)})}),
		kv("compression", bson.A{}),
		kv("saslSupportedMechs", fakeAuthDB+"."+fakeUser),
		kv("$db", "admin"),
	})
	return exchange{command: "hello", reply: reply, check: func(req bson.Raw) error {
		if !bytes.Equal(req, want) {
			return errors.New("hello carries unexpected fields")
		}
		return nil
	}}
}

func scramCredentials(t *testing.T) scram.StoredCredentials {
	t.Helper()
	client, err := scram.SHA256.NewClient(fakeUser, fakeSecret, "")
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := client.GetStoredCredentialsWithError(scram.KeyFactors{Salt: "synthetic-salt", Iters: 4096})
	if err != nil {
		t.Fatal(err)
	}
	return credentials
}

// scramPeer is the server side of one SCRAM-SHA-256 conversation.
type scramPeer struct{ conversation *scram.ServerConversation }

func newScramPeer(credentials scram.StoredCredentials) *scramPeer {
	server, err := scram.SHA256.NewServer(func(user string) (scram.StoredCredentials, error) {
		if user != fakeUser {
			return scram.StoredCredentials{}, errors.New("unknown user")
		}
		return credentials, nil
	})
	if err != nil {
		panic(err)
	}
	return &scramPeer{conversation: server.NewConversation()}
}

func payload(req bson.Raw) string {
	_, data, _ := req.Lookup("payload").BinaryOK()
	return string(data)
}

// checkSASL requires SASL commands in the profile's authentication database.
func checkSASL(req bson.Raw) error {
	if db, _ := req.Lookup("$db").StringValueOK(); db != fakeAuthDB {
		return errors.New("SASL command outside the authentication database")
	}
	if mechanism, ok := req.Lookup("mechanism").StringValueOK(); ok && mechanism != "SCRAM-SHA-256" {
		return errors.New("unexpected SASL mechanism")
	}
	return nil
}

// step answers a SASL command with the server conversation's next message,
// optionally rewritten to model a hostile server.
func (sp *scramPeer) step(command string, done bool, rewrite func(string) string) exchange {
	return exchange{command: command, check: checkSASL, reply: func(req bson.Raw, id int32) []byte {
		out, err := sp.conversation.Step(payload(req))
		if err != nil {
			return opMsgFrame(id, 0, marshal(failure(18)), false)
		}
		if rewrite != nil {
			out = rewrite(out)
		}
		return opMsgFrame(id, 0, marshal(ok(kv("conversationId", int32(1)), kv("done", done), kv("payload", bson.Binary{Data: []byte(out)}))), false)
	}}
}

func (sp *scramPeer) start(rewrite func(string) string) exchange {
	return sp.step("saslStart", false, rewrite)
}

func (sp *scramPeer) finish(rewrite func(string) string) exchange {
	return sp.step("saslContinue", true, rewrite)
}

func authInfo(user, db string, extra ...bson.E) bson.E {
	users := bson.A{bson.D{kv("user", user), kv("db", db)}}
	return kv("authInfo", append(bson.D{kv("authenticatedUsers", users)}, extra...))
}

// identity answers the post-authentication identity check, which never asks
// for privileges.
func identity(user, db string) exchange {
	return exchange{command: "connectionStatus", reply: send(ok(authInfo(user, db, kv("authenticatedUserRoles", bson.A{})))), check: func(req bson.Raw) error {
		if _, err := req.LookupErr("showPrivileges"); err == nil {
			return errors.New("identity check requested privileges")
		}
		return nil
	}}
}

var buildInfo = respond("buildInfo", ok(kv("version", "7.0.48"), kv("versionArray", bson.A{int32(7), int32(0), int32(48), int32(0)})))

func privilege(resource bson.D, actions ...string) bson.D {
	list := bson.A{}
	for _, action := range actions {
		list = append(list, action)
	}
	return bson.D{kv("resource", resource), kv("actions", list)}
}

// readOnlyPrivileges covers each admissible resource shape with read and
// monitoring actions, including a non-data action on every resource.
func readOnlyPrivileges() bson.A {
	return bson.A{
		privilege(bson.D{kv("db", "app"), kv("collection", "")}, "find", "listCollections", "listIndexes", "listSearchIndexes", "collStats", "dbStats", "dbHash", "killCursors", "changeStream", "planCacheRead"),
		privilege(bson.D{kv("db", ""), kv("collection", "system.js")}, "find"),
		privilege(bson.D{kv("db", "app"), kv("system_buckets", "")}, "find", "performRawDataOperations"),
		privilege(bson.D{kv("system_buckets", "metrics")}, "find"),
		privilege(bson.D{kv("cluster", true)}, "listDatabases", "serverStatus", "top", "inprog", "listSessions", "connPoolStats", "getCmdLineOpts", "replSetGetStatus", "getDefaultRWConcern"),
		privilege(bson.D{kv("anyResource", true)}, "listCollections", "indexStats", "useUUID"),
	}
}

// audit answers the admission connectionStatus with expanded privileges.
func audit(privileges bson.A) exchange {
	roles := bson.A{bson.D{kv("role", "read"), kv("db", fakeAuthDB)}}
	reply := ok(authInfo(fakeUser, fakeAuthDB, kv("authenticatedUserRoles", roles), kv("authenticatedUserPrivileges", privileges)))
	return exchange{command: "connectionStatus", reply: send(reply), check: func(req bson.Raw) error {
		if show, _ := req.Lookup("showPrivileges").BooleanOK(); !show {
			return errors.New("audit did not request privileges")
		}
		return nil
	}}
}

// probe answers the authorization probe for a random user.
func probe(reply bson.D) exchange {
	return exchange{command: "usersInfo", reply: send(reply), check: func(req bson.Raw) error {
		user, _ := req.Lookup("usersInfo", "user").StringValueOK()
		db, _ := req.Lookup("usersInfo", "db").StringValueOK()
		if !strings.HasPrefix(user, "data-mate-probe-") || len(user) != len("data-mate-probe-")+32 || db != "admin" {
			return errors.New("unexpected authorization probe")
		}
		return nil
	}}
}

// handshake is a connection without the account audit, as a side connection uses.
func handshake(sp *scramPeer, rest ...exchange) []exchange {
	return append([]exchange{hello(send(standalone)), sp.start(nil), sp.finish(nil), identity(fakeUser, fakeAuthDB), buildInfo}, rest...)
}

// ready is a pooled connection's handshake plus a passing admission.
func ready(sp *scramPeer, rest ...exchange) []exchange {
	return handshake(sp, append([]exchange{audit(readOnlyPrivileges()), probe(failure(13))}, rest...)...)
}
