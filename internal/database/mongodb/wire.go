// Package mongodb implements authenticated read-only access to MongoDB through
// a bounded OP_MSG client owned by Data Mate. Each session reaches one
// standalone server, replica-set data member or mongos directly; the client
// never discovers, monitors or re-dials other members and reads no connection
// string, environment or option file.
package mongodb

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"strconv"
	"unicode/utf8"

	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// OP_MSG framing. Replies carry exactly one kind-0 section; compression,
// exhaust (moreToCome) and the legacy OP_REPLY are never negotiated, so any
// other opcode or required flag bit is a protocol error.
const (
	opMsg        = 2013
	flagChecksum = 1 << 0
	headerSize   = 16
	// A request is the header, flag bits, a section kind and one document.
	requestOverhead = headerSize + 4 + 1
	// A reply may also end with a CRC-32C checksum.
	replyOverhead = requestOverhead + 4
	// A server command document may exceed maxBsonObjectSize by this much.
	documentSlack = 16 << 10
	// Control replies (hello, SASL, connectionStatus, probe, buildInfo,
	// killCursors) are small; results use the server's advertised sizes.
	controlReplyLimit = 8 << 20
	// maxReplyDepth bounds the validating walker and the Extended JSON writer.
	maxReplyDepth = 256
)

// MongoDB server error codes interpreted in more than one place.
const (
	codeUnauthorized              int32 = 13
	codeNamespaceNotFound         int32 = 26
	codeCommandNotSupportedOnView int32 = 166
)

var (
	errProtocol   = errors.New("invalid MongoDB response")
	errReplyLimit = errors.New("response exceeds MongoDB receive limit")
	errCount      = errors.New("MongoDB reply exceeds an element bound")
	castagnoli    = crc32.MakeTable(crc32.Castagnoli)
)

func kv(key string, value any) bson.E { return bson.E{Key: key, Value: value} }

// readReply reads one OP_MSG reply to request id. Server-controlled lengths
// are checked against limit before any allocation, and the returned document
// has passed structural validation.
func readReply(r *bufio.Reader, id int32, limit int) (bson.Raw, error) {
	var header [headerSize]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	length := int64(int32(binary.LittleEndian.Uint32(header[0:])))
	if length < requestOverhead+5 {
		return nil, errProtocol
	}
	if length > int64(limit) {
		return nil, errReplyLimit
	}
	responseTo := int32(binary.LittleEndian.Uint32(header[8:]))
	opcode := int32(binary.LittleEndian.Uint32(header[12:]))
	if responseTo != id || opcode != opMsg {
		return nil, errProtocol
	}
	body := make([]byte, length-headerSize)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	flags := binary.LittleEndian.Uint32(body)
	// Bits 0-15 are required to be understood; only the checksum is.
	if flags&0xfffe != 0 {
		return nil, errProtocol
	}
	if flags&flagChecksum != 0 {
		n := len(body) - 4
		if n < 4+1+5 {
			return nil, errProtocol
		}
		sum := crc32.Update(crc32.Checksum(header[:], castagnoli), castagnoli, body[:n])
		if sum != binary.LittleEndian.Uint32(body[n:]) {
			return nil, errProtocol
		}
		body = body[:n]
	}
	if body[4] != 0 {
		return nil, errProtocol
	}
	doc := body[5:]
	if size, ok := length32(doc); !ok || size != len(doc) {
		return nil, errProtocol
	}
	if err := validateDocument(doc, 0, false); err != nil {
		return nil, err
	}
	return bson.Raw(doc), nil
}

func length32(b []byte) (int, bool) {
	if len(b) < 4 {
		return 0, false
	}
	return int(int32(binary.LittleEndian.Uint32(b))), true
}

// stringLength sizes a BSON string, JavaScript or symbol value: a positive
// length prefix that counts a terminating NUL.
func stringLength(b []byte) (int, error) {
	n, ok := length32(b)
	if !ok || n < 1 || n > len(b)-4 || b[3+n] != 0 {
		return 0, errProtocol
	}
	return 4 + n, nil
}

// valueLength returns the encoded size of a value of type t at the start of b,
// checking every length against the enclosing bytes. Nested documents are not
// walked; validateDocument does that.
func valueLength(t byte, b []byte) (int, error) {
	fixed := func(n int) (int, error) {
		if len(b) < n {
			return 0, errProtocol
		}
		return n, nil
	}
	switch t {
	case 0x06, 0x0a, 0x7f, 0xff: // undefined, null, max key, min key
		return 0, nil
	case 0x08: // boolean
		if len(b) < 1 || b[0] > 1 {
			return 0, errProtocol
		}
		return 1, nil
	case 0x10: // int32
		return fixed(4)
	case 0x01, 0x09, 0x11, 0x12: // double, date, timestamp, int64
		return fixed(8)
	case 0x07: // ObjectId
		return fixed(12)
	case 0x13: // Decimal128
		return fixed(16)
	case 0x02, 0x0d, 0x0e: // string, JavaScript, symbol
		return stringLength(b)
	case 0x0c: // DBPointer: namespace string and ObjectId
		n, err := stringLength(b)
		if err != nil || len(b)-n < 12 {
			return 0, errProtocol
		}
		return n + 12, nil
	case 0x03, 0x04: // document, array
		n, ok := length32(b)
		if !ok || n < 5 || n > len(b) || b[n-1] != 0 {
			return 0, errProtocol
		}
		return n, nil
	case 0x05: // binary
		n, ok := length32(b)
		if !ok || n < 0 || n > len(b)-5 {
			return 0, errProtocol
		}
		// The legacy subtype repeats its length inside the data. A reader that
		// trusts the inner length would emit the remaining bytes as sibling
		// fields, so both lengths must agree.
		if b[4] == 0x02 {
			inner, _ := length32(b[5:])
			if n < 4 || inner != n-4 {
				return 0, errProtocol
			}
		}
		return 5 + n, nil
	case 0x0b: // regular expression: pattern and options cstrings
		p := bytes.IndexByte(b, 0)
		if p < 0 {
			return 0, errProtocol
		}
		o := bytes.IndexByte(b[p+1:], 0)
		if o < 0 {
			return 0, errProtocol
		}
		return p + o + 2, nil
	case 0x0f: // code with scope: total length, code string, scope document
		n, ok := length32(b)
		if !ok || n < 14 || n > len(b) {
			return 0, errProtocol
		}
		s, err := stringLength(b[4:n])
		if err != nil {
			return 0, errProtocol
		}
		scope := b[4+s : n]
		if m, ok := length32(scope); !ok || m < 5 || m != len(scope) {
			return 0, errProtocol
		}
		return n, nil
	}
	return 0, errProtocol
}

// validateDocument walks a document and every nested document within the depth
// bound. Structure mode checks framing only. Text mode, used before encoding
// results, also requires UTF-8 keys and text and DBPointer namespaces that the
// Extended JSON writer can emit without escaping; its failures are never
// distinguished from malformed framing.
func validateDocument(b []byte, depth int, text bool) error {
	if depth > maxReplyDepth {
		return errReplyLimit
	}
	if n, ok := length32(b); !ok || n < 5 || n != len(b) || b[n-1] != 0 {
		return errProtocol
	}
	rest := b[4 : len(b)-1]
	for len(rest) > 0 {
		t := rest[0]
		end := bytes.IndexByte(rest[1:], 0)
		if end < 0 {
			return errProtocol
		}
		if text && !utf8.Valid(rest[1:1+end]) {
			return errProtocol
		}
		rest = rest[2+end:]
		n, err := valueLength(t, rest)
		if err != nil {
			return err
		}
		if err := validateValue(t, rest[:n], depth, text); err != nil {
			return err
		}
		rest = rest[n:]
	}
	return nil
}

// validateValue checks one value already sized by valueLength.
func validateValue(t byte, v []byte, depth int, text bool) error {
	switch t {
	case 0x03, 0x04:
		return validateDocument(v, depth+1, text)
	case 0x0f:
		s, _ := stringLength(v[4:])
		if text && !utf8.Valid(v[8:3+s]) {
			return errProtocol
		}
		return validateDocument(v[4+s:], depth+1, text)
	}
	if !text {
		return nil
	}
	switch t {
	case 0x02, 0x0d, 0x0e:
		if !utf8.Valid(v[4 : len(v)-1]) {
			return errProtocol
		}
	case 0x0b:
		if !utf8.Valid(v) {
			return errProtocol
		}
	case 0x0c:
		ns := v[4 : len(v)-13]
		if !utf8.Valid(ns) || bytes.ContainsAny(ns, `"\`) || bytes.ContainsFunc(ns, func(r rune) bool { return r < 0x20 }) {
			return errProtocol
		}
	}
	return nil
}

// each visits the top-level elements of a document in order.
func each(doc []byte, fn func(string, bson.RawValue) error) error {
	if n, ok := length32(doc); !ok || n < 5 || n != len(doc) || doc[n-1] != 0 {
		return errProtocol
	}
	rest := doc[4 : len(doc)-1]
	for len(rest) > 0 {
		t := rest[0]
		end := bytes.IndexByte(rest[1:], 0)
		if end < 0 {
			return errProtocol
		}
		key := string(rest[1 : 1+end])
		rest = rest[2+end:]
		n, err := valueLength(t, rest)
		if err != nil {
			return err
		}
		if err := fn(key, bson.RawValue{Type: bson.Type(t), Value: rest[:n]}); err != nil {
			return err
		}
		rest = rest[n:]
	}
	return nil
}

// fields indexes a reply document the driver interprets and rejects duplicate
// keys, so the driver and the server cannot read different values. User
// documents keep duplicates and are never indexed this way. Every value has
// been sized by valueLength, so a RawValue ...OK accessor fails only on a type
// mismatch, including the zero RawValue of a missing key.
func fields(doc bson.Raw) (map[string]bson.RawValue, error) {
	m := make(map[string]bson.RawValue)
	err := each(doc, func(key string, v bson.RawValue) error {
		if _, dup := m[key]; dup {
			return errProtocol
		}
		m[key] = v
		return nil
	})
	return m, err
}

// values returns at most limit elements of an array; more is errCount.
func values(v bson.RawValue, limit int) ([]bson.RawValue, error) {
	if v.Type != bson.TypeArray {
		return nil, errProtocol
	}
	var out []bson.RawValue
	err := each(v.Value, func(_ string, item bson.RawValue) error {
		if len(out) == limit {
			return errCount
		}
		out = append(out, item)
		return nil
	})
	return out, err
}

// boolField reads an optional boolean; any other type is a protocol error.
func boolField(m map[string]bson.RawValue, key string) (bool, error) {
	v, ok := m[key]
	if !ok {
		return false, nil
	}
	b, ok := v.BooleanOK()
	if !ok {
		return false, errProtocol
	}
	return b, nil
}

// status checks a reply's ok field, which must be a number equal to 0 or 1. A
// failure carries only the numeric code; errmsg and codeName are never read.
func status(body bson.Raw) error {
	top, err := fields(body)
	if err != nil {
		return err
	}
	ok := top["ok"]
	var value float64
	switch ok.Type {
	case bson.TypeDouble:
		value = ok.Double()
	case bson.TypeInt32:
		value = float64(ok.Int32())
	case bson.TypeInt64:
		value = float64(ok.Int64())
	default:
		return errProtocol
	}
	switch value {
	case 1:
		return nil
	case 0:
		code, _ := top["code"].Int32OK()
		return serverError{code: code}
	}
	return errProtocol
}

// serverError is a command failure reported with ok:0.
type serverError struct{ code int32 }

func (e serverError) Error() string {
	return "MongoDB command failed with code " + strconv.Itoa(int(e.code))
}

// closing is the one list of replica-set state and shutdown failures, after
// which the session must not serve another operation, with the public message
// for each.
func (e serverError) closing() (string, bool) {
	switch e.code {
	case 10107, 13435, 13436, 189, 11602:
		return "MongoDB member cannot serve this read in its current replica-set state", true
	case 91, 11600:
		return "MongoDB server is shutting down", true
	}
	return "", false
}

// closes reports whether the session must close after this failure.
func (e serverError) closes() bool {
	_, closes := e.closing()
	return closes
}

// safe maps a server code to a fixed public failure.
func (e serverError) safe() error {
	if message, ok := e.closing(); ok {
		return database.Fail(contracts.ConnectFailed, message, true)
	}
	switch e.code {
	case codeUnauthorized:
		return database.Fail(contracts.PermissionDenied, "MongoDB account lacks permission for this operation", false)
	case codeNamespaceNotFound:
		return inaccessible()
	case 50, 262:
		return database.Fail(contracts.QueryTimeout, "database operation timed out", true)
	case 2, 9, 14, 15, 72, 73, 168:
		return database.Fail(contracts.InvalidArgument, "MongoDB rejected the command arguments", false)
	case 59, 115, codeCommandNotSupportedOnView, 167, 31082:
		return database.Fail(contracts.QueryUnsupported, "MongoDB does not support this operation for the target", false)
	case 146, 292, 10334:
		return database.Fail(contracts.ResourceLimit, "MongoDB operation exceeded a server resource limit", false)
	case 43, 175, 237:
		return database.Fail(contracts.QueryFailed, "MongoDB cursor ended before the result was complete", true)
	case 11601, 279:
		return database.Fail(contracts.QueryFailed, "MongoDB interrupted the operation", true)
	}
	return database.Fail(contracts.QueryFailed, "MongoDB could not execute the command", false)
}

func inaccessible() error {
	return database.Fail(contracts.PermissionDenied, "MongoDB collection is unavailable or inaccessible", false)
}

func protocolFailure() error {
	return database.Fail(contracts.ConnectFailed, "MongoDB connection or protocol failed", true)
}

func receiveLimit() error {
	return database.Fail(contracts.ResourceLimit, "MongoDB response exceeds receive limit", false)
}

// safeError translates any operation failure into a public error.
func safeError(err error) error {
	if err == nil {
		return nil
	}
	if safe := database.CommonError(err); safe != nil {
		return safe
	}
	var server serverError
	if errors.As(err, &server) {
		return server.safe()
	}
	if errors.Is(err, errReplyLimit) {
		return receiveLimit()
	}
	return protocolFailure()
}
