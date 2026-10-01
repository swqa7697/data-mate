package mysql

import (
	"bytes"
	"crypto/sha1"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeServer speaks the MySQL protocol's handshake, OK/ERR packets and text
// result sets: enough for diagnostics, with no database behind it.
type fakeServer struct {
	listener net.Listener
	version  string
	// respond answers one COM_QUERY; nil rows with no columns means OK.
	respond func(query string) (columns []string, rows [][]string, failure uint16)
	// stall stops responding after the handshake response arrives.
	stall bool
	// oversized sends this many bytes in the first result value.
	oversized int
	mu        sync.Mutex
	queries   []string
	user      string
	auth      []byte
	salt      []byte
	done      chan error
}

const fakeCapabilities = 1 | 4 | 1<<9 | 1<<13 | 1<<15 | 1<<19 // long password, long flag, protocol 41, transactions, secure connection, plugin auth

func newFakeServer(t *testing.T, version string, respond func(string) ([]string, [][]string, uint16)) *fakeServer {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeServer{listener: l, version: version, respond: respond, salt: []byte("01234567890123456789"), done: make(chan error, 1)}
	t.Cleanup(func() { l.Close() })
	return s
}

func (s *fakeServer) port() int { return s.listener.Addr().(*net.TCPAddr).Port }

// serve handles one connection.
func (s *fakeServer) serve() {
	conn, err := s.listener.Accept()
	if err != nil {
		s.done <- err
		return
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	s.done <- s.session(conn)
}

func writePacket(w io.Writer, seq byte, payload []byte) error {
	header := []byte{byte(len(payload)), byte(len(payload) >> 8), byte(len(payload) >> 16), seq}
	_, err := w.Write(append(header, payload...))
	return err
}

func readPacket(r io.Reader) (byte, []byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil, err
	}
	n := int(header[0]) | int(header[1])<<8 | int(header[2])<<16
	body := make([]byte, n)
	_, err := io.ReadFull(r, body)
	return header[3], body, err
}

func lenenc(b []byte) []byte {
	if len(b) < 251 {
		return append([]byte{byte(len(b))}, b...)
	}
	out := []byte{0xfd, byte(len(b)), byte(len(b) >> 8), byte(len(b) >> 16)}
	return append(out, b...)
}

func (s *fakeServer) session(conn net.Conn) error {
	greeting := []byte{10}
	greeting = append(greeting, s.version...)
	greeting = append(greeting, 0, 7, 0, 0, 0)
	greeting = append(greeting, s.salt[:8]...)
	caps := uint32(fakeCapabilities)
	greeting = append(greeting, 0, byte(caps), byte(caps>>8), 45, 2, 0, byte(caps>>16), byte(caps>>24), 21)
	greeting = append(greeting, make([]byte, 10)...)
	greeting = append(greeting, s.salt[8:]...)
	greeting = append(greeting, 0)
	greeting = append(greeting, "mysql_native_password\x00"...)
	if err := writePacket(conn, 0, greeting); err != nil {
		return err
	}
	_, response, err := readPacket(conn)
	if err != nil || len(response) < 33 {
		return errors.New("handshake response")
	}
	rest := response[32:]
	end := bytes.IndexByte(rest, 0)
	if end < 0 || end+1 >= len(rest) {
		return errors.New("handshake user")
	}
	s.mu.Lock()
	s.user = string(rest[:end])
	size := int(rest[end+1])
	if end+2+size <= len(rest) {
		s.auth = append([]byte(nil), rest[end+2:end+2+size]...)
	}
	s.mu.Unlock()
	if s.stall {
		// Hold the connection open until the client gives up.
		_, _, _ = readPacket(conn)
		return nil
	}
	if err = writePacket(conn, 2, []byte{0, 0, 0, 2, 0, 0, 0}); err != nil {
		return err
	}
	for {
		_, command, err := readPacket(conn)
		if err != nil || len(command) == 0 || command[0] == 1 {
			return nil
		}
		if command[0] != 3 {
			return errors.New("unexpected command")
		}
		query := string(command[1:])
		s.mu.Lock()
		s.queries = append(s.queries, query)
		s.mu.Unlock()
		if err = s.answer(conn, query); err != nil {
			return err
		}
	}
}

func (s *fakeServer) answer(conn net.Conn, query string) error {
	columns, rows, failure := s.respond(query)
	if s.oversized > 0 {
		columns, rows, failure, s.oversized = []string{"v"}, [][]string{{strings.Repeat("x", s.oversized)}}, 0, 0
	}
	if failure != 0 {
		payload := []byte{0xff, byte(failure), byte(failure >> 8), '#'}
		payload = append(payload, "42000synthetic failure"...)
		return writePacket(conn, 1, payload)
	}
	if columns == nil {
		return writePacket(conn, 1, []byte{0, 0, 0, 2, 0, 0, 0})
	}
	seq := byte(1)
	var out bytes.Buffer
	next := func(payload []byte) {
		_ = writePacket(&out, seq, payload)
		seq++
	}
	next([]byte{byte(len(columns))})
	for _, name := range columns {
		def := lenenc([]byte("def"))
		for _, part := range []string{"", "", "", name, name} {
			def = append(def, lenenc([]byte(part))...)
		}
		def = append(def, 0x0c, 45, 0, 0, 0, 1, 0, 0xfd, 0, 0, 0, 0, 0)
		next(def)
	}
	next([]byte{0xfe, 0, 0, 2, 0})
	for _, row := range rows {
		var payload []byte
		for _, v := range row {
			payload = append(payload, lenenc([]byte(v))...)
		}
		next(payload)
	}
	next([]byte{0xfe, 0, 0, 2, 0})
	_, err := conn.Write(out.Bytes())
	return err
}

// nativePassword is the mysql_native_password response for a password.
func nativePassword(salt []byte, password string) []byte {
	one := sha1.Sum([]byte(password))
	two := sha1.Sum(one[:])
	h := sha1.New()
	h.Write(salt)
	h.Write(two[:])
	mix := h.Sum(nil)
	out := make([]byte, len(mix))
	for i := range mix {
		out[i] = one[i] ^ mix[i]
	}
	return out
}

// readyServer answers the driver's diagnostic statements for a valid account.
func readyResponses(version, readOnly string, grants ...string) func(string) ([]string, [][]string, uint16) {
	return func(q string) ([]string, [][]string, uint16) {
		switch {
		case strings.HasPrefix(q, "SELECT @@version"):
			return []string{"version", "id"}, [][]string{{version, "7"}}, 0
		case strings.HasPrefix(q, "SELECT @@session.sql_mode"):
			return []string{"mode", "client", "connection", "results", "ro", "user"}, [][]string{{MySQL.sqlMode(), "utf8mb4", "utf8mb4", "utf8mb4", readOnly, "reader@%"}}, 0
		case q == "SHOW GRANTS":
			rows := [][]string{{"GRANT USAGE ON *.* TO `reader`@`%`"}}
			for _, g := range grants {
				rows = append(rows, []string{g})
			}
			return []string{"grants"}, rows, 0
		case strings.Contains(q, "APPLICABLE_ROLES"):
			return []string{"a", "b", "c", "d", "e", "f"}, nil, 0
		case strings.HasPrefix(q, "SET SESSION"), q == "START TRANSACTION READ ONLY", q == "ROLLBACK", q == "DO RELEASE_ALL_LOCKS()":
			return nil, nil, 0
		}
		return nil, nil, 1064
	}
}
