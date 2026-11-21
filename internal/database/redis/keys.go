package redis

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"unicode/utf8"

	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
	"github.com/swqa7697/data-mate/internal/database/pool"
)

const (
	defaultPageSize = 100
	maxPageSize     = 1000
)

// Redis keys, fields and values are byte strings, including the empty string.
func keyBytes(raw string) database.KeyBytes {
	if utf8.ValidString(raw) {
		return database.KeyBytes{Value: raw, Encoding: "utf8"}
	}
	return database.KeyBytes{Value: base64.StdEncoding.EncodeToString([]byte(raw)), Encoding: "base64"}
}

func decodeKey(b database.KeyBytes) (string, error) {
	switch b.Encoding {
	case "utf8":
		if utf8.ValidString(b.Value) {
			return b.Value, nil
		}
	case "base64":
		raw, err := base64.StdEncoding.Strict().DecodeString(b.Value)
		if err == nil && base64.StdEncoding.EncodeToString(raw) == b.Value {
			return string(raw), nil
		}
	}
	return "", database.Fail(contracts.InvalidArgument, "invalid Redis key encoding", false)
}

func pageSize(requested int, a database.Access) (int, error) {
	limit := min(maxPageSize, a.Profile.Limits.MaxRows)
	if requested == 0 {
		return min(defaultPageSize, limit), nil
	}
	if requested < 1 || requested > limit {
		return 0, database.Fail(contracts.InvalidArgument, "Redis page size exceeds the connection limit", false)
	}
	return requested, nil
}

// cursor keeps Redis-specific continuation state inside the shared signed envelope.
type cursor struct {
	pool.Cursor
	nativePosition
}

type nativePosition struct {
	ScanOffset int    `json:",omitempty"`
	ScanDigest string `json:",omitempty"`
	StreamID   string `json:",omitempty"`
}

func (d *Driver) position(a database.Access, rev config.Revision, tool, binding, token string) (cursor, error) {
	// Cursor sizes stay bounded even for long keys and MATCH patterns. Hash
	// the original bytes so keys with different invalid UTF-8 cannot collide.
	hash := sha256.Sum256([]byte(binding))
	pos := pool.Cursor{Version: 1, Tool: tool, ID: a.Profile.ID, Revision: rev, Epoch: d.pools.Epoch(), Filter: hex.EncodeToString(hash[:])}
	if token == "" {
		return cursor{Cursor: pos}, nil
	}
	decoded, err := d.pools.Decode(token, pos, func(string) bool { return true })
	if err != nil {
		return cursor{}, err
	}
	c := cursor{Cursor: decoded}
	if json.Unmarshal(decoded.Position, &c.nativePosition) != nil {
		return cursor{}, database.Fail(contracts.StaleCursor, "invalid Redis cursor position", false)
	}
	return c, nil
}

func (d *Driver) continuation(pos cursor) *string {
	// nativePosition contains only strings and integers.
	pos.Position, _ = json.Marshal(pos.nativePosition)
	token := d.pools.Encode(pos.Cursor)
	return &token
}

func badKeyReply() error {
	return database.Fail(contracts.QueryFailed, "invalid Redis key response", false)
}

func inaccessibleKey() error {
	return database.Fail(contracts.PermissionDenied, "Redis key is unavailable or inaccessible", false)
}

// A denied value read and an inaccessible missing key have the same public
// error, even when the account can run TYPE on both names.
func (s *session) keyCommand(ctx context.Context, args ...string) (any, error) {
	value, err := s.command(ctx, args...)
	if permissionDenied(err) {
		return nil, inaccessibleKey()
	}
	return value, err
}

// Probe with the same command as a value read. Empty ranges and the empty
// glob character class keep values out of metadata and discovery replies,
// including when Redis scans an entire compact hash or set despite COUNT.
func probeKey(ctx context.Context, s *session, kind, key string) error {
	var args []string
	switch kind {
	case "string":
		args = []string{"GETRANGE", key, "1", "0"}
	case "list":
		args = []string{"LRANGE", key, "1", "0"}
	case "set":
		args = []string{"SSCAN", key, "0", "MATCH", "[]", "COUNT", "1"}
	case "hash":
		args = []string{"HSCAN", key, "0", "MATCH", "[]", "COUNT", "1"}
	case "zset":
		args = []string{"ZRANGE", key, "1", "0", "WITHSCORES"}
	case "stream":
		args = []string{"XRANGE", key, "+", "-", "COUNT", "1"}
	default:
		return inaccessibleKey()
	}
	value, err := s.keyCommand(ctx, args...)
	if err != nil {
		return err
	}
	if kind == "string" {
		if value != "" {
			return badKeyReply()
		}
		return nil
	}
	if kind == "set" || kind == "hash" {
		pair, ok := value.([]any)
		if !ok || len(pair) != 2 {
			return badKeyReply()
		}
		cursor, ok := pair[0].(string)
		if _, err := strconv.ParseUint(cursor, 10, 64); !ok || err != nil {
			return badKeyReply()
		}
		value = pair[1]
	}
	if items, ok := value.([]any); !ok || len(items) != 0 {
		return badKeyReply()
	}
	return nil
}

func permissionDenied(err error) bool {
	var fail *database.Error
	return errors.As(err, &fail) && fail.Code == contracts.PermissionDenied
}

func keyKind(ctx context.Context, s *session, key string) (string, error) {
	v, err := s.keyCommand(ctx, "TYPE", key)
	if err != nil {
		return "", err
	}
	kind, ok := v.(string)
	if !ok {
		return "", badKeyReply()
	}
	return kind, nil
}

func checkKeyRead(ctx context.Context, s *session, kind, key string) error {
	if kind != "none" {
		return probeKey(ctx, s, kind, key)
	}
	// A missing key has no type. Reveal its absence only if at least one
	// of our value commands is permitted on this name.
	for _, candidate := range []string{"string", "list", "set", "hash", "zset", "stream"} {
		err := probeKey(ctx, s, candidate, key)
		if !permissionDenied(err) {
			return err
		}
	}
	return inaccessibleKey()
}

func listedKeyKind(ctx context.Context, s *session, key string) (string, error) {
	kind, err := keyKind(ctx, s, key)
	if err == nil {
		if kind == "none" {
			return "", nil
		}
		err = probeKey(ctx, s, kind, key)
	}
	// A concurrent writer can change the type between TYPE and the probe.
	if permissionDenied(err) || errors.Is(err, errWrongType) {
		return "", nil
	}
	return kind, err
}

// ListKeys returns one bounded scan page. Redis may return an empty page with
// a continuation, including when no candidates pass the account's key ACL.
func (d *Driver) ListKeys(ctx context.Context, a database.Access, req database.KeyPageRequest) (database.KeyPage, error) {
	a, rev, err := database.Normalize(a)
	if err != nil {
		return database.KeyPage{}, err
	}
	page, err := d.listKeys(ctx, a, rev, req)
	if err == nil {
		err = database.PayloadBound(page, a)
	}
	if err != nil {
		return database.KeyPage{}, err
	}
	return page, nil
}

func (d *Driver) listKeys(ctx context.Context, a database.Access, rev config.Revision, req database.KeyPageRequest) (database.KeyPage, error) {
	size, err := pageSize(req.PageSize, a)
	if err != nil {
		return database.KeyPage{}, err
	}
	pos, err := d.position(a, rev, "list_keys", req.Pattern, req.Cursor)
	if err != nil {
		return database.KeyPage{}, err
	}
	out := database.KeyPage{Connection: a.Profile.Alias, Driver: a.Profile.Driver, Keys: []database.KeyInfo{}}
	err = d.runNormalized(ctx, a, rev, func(ctx context.Context, s *session) error {
		args := []string{"SCAN", strconv.FormatUint(pos.Key, 10), "COUNT", strconv.Itoa(defaultPageSize)}
		if req.Pattern != "" {
			args = append(args, "MATCH", req.Pattern)
		}
		v, err := s.command(ctx, args...)
		if err != nil {
			return err
		}
		window, more, err := d.scanWindow(v, &pos, size, 1)
		if err != nil {
			return err
		}
		budget := database.MetadataBudget{Limit: a.Profile.Limits.MaxResultBytes}
		for _, raw := range window {
			key, ok := raw.(string)
			if !ok {
				return badKeyReply()
			}
			kind, err := listedKeyKind(ctx, s, key)
			if err != nil {
				return err
			}
			if kind != "" {
				item := database.KeyInfo{Key: keyBytes(key), Type: kind}
				if err := budget.Add(item); err != nil {
					return err
				}
				out.Keys = append(out.Keys, item)
			}
		}
		if more {
			out.NextCursor = d.continuation(pos)
		}
		return nil
	})
	return out, err
}

// scanWindow retains the input cursor and consumed offset when Redis returns
// more than the requested count. Replaying uses a fixed COUNT independent of
// page size. Reject a changed replay before applying its consumed offset.
func (d *Driver) scanWindow(reply any, pos *cursor, size, width int) ([]any, bool, error) {
	pair, ok := reply.([]any)
	if !ok || len(pair) != 2 {
		return nil, false, badKeyReply()
	}
	next, ok := pair[0].(string)
	items, arrayOK := pair[1].([]any)
	rawCursor, err := strconv.ParseUint(next, 10, 64)
	if !ok || !arrayOK || err != nil || len(items)%width != 0 {
		return nil, false, badKeyReply()
	}
	start := pos.ScanOffset
	stale := func() error {
		return database.Fail(contracts.StaleCursor, "Redis collection changed during pagination", false)
	}
	if start < 0 || start > len(items)/width {
		return nil, false, stale()
	}
	end := min(start+size, len(items)/width)
	if start > 0 || end < len(items)/width {
		h := sha256.New()
		var frame [8]byte
		binary.BigEndian.PutUint64(frame[:], rawCursor)
		h.Write(frame[:])
		for _, raw := range items {
			s, ok := raw.(string)
			if !ok {
				return nil, false, badKeyReply()
			}
			binary.BigEndian.PutUint64(frame[:], uint64(len(s)))
			h.Write(frame[:])
			h.Write([]byte(s))
		}
		digest := d.pools.CursorDigest([sha256.Size]byte(h.Sum(nil)))
		if start > 0 && pos.ScanDigest != digest {
			return nil, false, stale()
		}
		pos.ScanDigest = digest
	}
	result := items[start*width : end*width]
	if end < len(items)/width {
		pos.ScanOffset = end
		return result, true, nil
	}
	pos.Key, pos.ScanOffset, pos.ScanDigest = rawCursor, 0, ""
	return result, rawCursor != 0, nil
}

// DescribeDatabase returns the first page of visible native keys for the
// configured logical database, without reading values.
func (d *Driver) DescribeDatabase(ctx context.Context, a database.Access) (database.DatabaseDescription, error) {
	a, rev, err := database.Normalize(a)
	if err != nil {
		return database.DatabaseDescription{}, err
	}
	page, err := d.listKeys(ctx, a, rev, database.KeyPageRequest{})
	if err != nil {
		return database.DatabaseDescription{}, err
	}
	out := database.DatabaseDescription{Version: 1, Alias: a.Profile.Alias, Driver: a.Profile.Driver, Database: a.Profile.Connection.Database, RedisKeys: &page}
	if err := out.CheckSize(a.Profile.Limits.MaxResultBytes); err != nil {
		return database.DatabaseDescription{}, err
	}
	return out, nil
}
