// Package redis implements authenticated read-only access to standalone Redis.
package redis

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strconv"

	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
)

// Redis collection scans may return an entire compact collection despite a
// small COUNT. Bound each wire reply before applying the profile's separate
// public result budget, even when that budget is disabled.
const (
	maxReplyBytes  = 8 << 20
	maxReplyValues = 1 << 18
	maxReplyDepth  = 12
)

var errProtocol = errors.New("invalid Redis response")
var errReplyLimit = errors.New("response exceeds Redis receive limit")
var errCommandDenied = database.Fail(contracts.ReadOnlyViolation, "Redis driver blocked an unsupported command", false)
var errWrongType = database.Fail(contracts.QueryFailed, "Redis key type changed", false)

// responseReader checks all server-controlled lengths before allocating. RESP2
// bulk and simple strings remain byte-exact Go strings until public encoding.
type responseReader struct {
	r         *bufio.Reader
	remaining int
	nodes     int
}

func (r *responseReader) value(depth int) (any, error) {
	if depth > maxReplyDepth || r.nodes >= maxReplyValues || r.remaining < 3 {
		return nil, errReplyLimit
	}
	r.nodes++
	line, err := r.r.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		return nil, errReplyLimit
	}
	if err != nil {
		return nil, err
	}
	if len(line) > r.remaining {
		return nil, errReplyLimit
	}
	r.remaining -= len(line)
	if len(line) < 3 || line[len(line)-2] != '\r' {
		return nil, errProtocol
	}
	kind, field := line[0], line[1:len(line)-2]
	switch kind {
	case '+':
		return string(field), nil
	case '-':
		// Upstream error text may contain keys, ACLs or credentials.
		code, _, _ := bytes.Cut(field, []byte(" "))
		return safeReplyError(string(code)), nil
	case ':', '$', '*':
	default:
		return nil, errProtocol
	}
	n, err := strconv.ParseInt(string(field), 10, 64)
	if err != nil {
		return nil, errProtocol
	}
	if kind == ':' {
		return n, nil
	}
	if n == -1 {
		return nil, nil
	}
	if n < 0 {
		return nil, errProtocol
	}
	if kind == '$' {
		if n > int64(r.remaining)-2 {
			return nil, errReplyLimit
		}
		b := make([]byte, int(n)+2)
		if _, err := io.ReadFull(r.r, b); err != nil {
			return nil, err
		}
		r.remaining -= len(b)
		if b[n] != '\r' || b[n+1] != '\n' {
			return nil, errProtocol
		}
		return string(b[:n]), nil
	}
	if n > int64(r.remaining/3) || n > int64(maxReplyValues-r.nodes) {
		return nil, errReplyLimit
	}
	out := make([]any, 0, min(int(n), 256))
	for range int(n) {
		v, err := r.value(depth + 1)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func safeReplyError(code string) error {
	switch code {
	case "NOPERM":
		return database.Fail(contracts.PermissionDenied, "Redis account lacks permission for this operation", false)
	case "NOAUTH", "WRONGPASS":
		return database.Fail(contracts.ConnectFailed, "Redis authentication failed", false)
	case "MOVED", "ASK":
		return database.Fail(contracts.QueryUnsupported, "Redis Cluster redirects are unsupported", false)
	case "WRONGTYPE":
		return errWrongType
	default:
		return database.Fail(contracts.QueryFailed, "Redis command failed", false)
	}
}

// Allow only session setup and the key tools' read commands. Check before
// buffering any bytes, then frame every argument separately, including binary keys.
func writeCommand(w *bufio.Writer, args []string) error {
	if len(args) == 0 {
		return errCommandDenied
	}
	switch args[0] {
	case "HELLO":
		if len(args) != 5 || args[1] != "2" || args[2] != "AUTH" {
			return errCommandDenied
		}
	case "SELECT":
		if len(args) != 2 {
			return errCommandDenied
		}
	case "SCAN", "TYPE", "PTTL", "STRLEN", "LLEN", "SCARD", "HLEN", "ZCARD", "XLEN",
		"GETRANGE", "LRANGE", "SSCAN", "HSCAN", "ZRANGE", "XRANGE":
		// These commands only read application data.
	default:
		return errCommandDenied
	}
	w.WriteString("*" + strconv.Itoa(len(args)) + "\r\n")
	for _, arg := range args {
		w.WriteString("$" + strconv.Itoa(len(arg)) + "\r\n")
		w.WriteString(arg)
		w.WriteString("\r\n")
	}
	// bufio.Writer retains the first error and reports it from Flush.
	return w.Flush()
}

// fields decodes a RESP2 map and rejects duplicate keys.
func fields(v any) (map[string]any, bool) {
	items, ok := v.([]any)
	if !ok || len(items)%2 != 0 {
		return nil, false
	}
	m := make(map[string]any, len(items)/2)
	for i := 0; i < len(items); i += 2 {
		name, ok := items[i].(string)
		if !ok {
			return nil, false
		}
		if _, exists := m[name]; exists {
			return nil, false
		}
		m[name] = items[i+1]
	}
	return m, true
}
