package redis

import (
	"context"
	"strconv"
	"strings"

	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
)

// DescribeKey fetches read-authorized metadata without retrieving a value.
func (d *Driver) DescribeKey(ctx context.Context, a database.Access, req database.KeyRequest) (database.KeyDescription, error) {
	key, err := decodeKey(req.Key)
	if err != nil {
		return database.KeyDescription{}, err
	}
	a, rev, err := database.Normalize(a)
	if err != nil {
		return database.KeyDescription{}, err
	}
	out := database.KeyDescription{Connection: a.Profile.Alias, Driver: a.Profile.Driver, Key: keyBytes(key), TTLMS: -2}
	err = d.runNormalized(ctx, a, rev, func(ctx context.Context, s *session) error {
		kind, err := keyKind(ctx, s, a.Profile.Connection.Username, key, false)
		if err != nil {
			return err
		}
		out.Type = kind
		if kind == "none" {
			return nil
		}
		lengths := map[string]string{"string": "STRLEN", "list": "LLEN", "set": "SCARD", "hash": "HLEN", "zset": "ZCARD", "stream": "XLEN"}
		v, err := s.command(ctx, lengths[kind], key)
		if err != nil {
			return err
		}
		length, ok := v.(int64)
		if !ok || length < 0 {
			return badKeyReply()
		}
		v, err = s.command(ctx, "PTTL", key)
		if err != nil {
			return err
		}
		ttl, ok := v.(int64)
		if !ok || ttl < -2 {
			return badKeyReply()
		}
		if ttl == -2 {
			out.Type = "none"
		} else {
			out.TTLMS, out.Length = ttl, &length
		}
		return nil
	})
	if err == nil {
		err = database.PayloadBound(out, a)
	}
	if err != nil {
		return database.KeyDescription{}, err
	}
	return out, nil
}

type fieldValue struct {
	Field database.KeyBytes `json:"field"`
	Value database.KeyBytes `json:"value"`
}

type scoredMember struct {
	Member database.KeyBytes `json:"member"`
	Score  string            `json:"score"`
}

type streamEntry struct {
	ID     string       `json:"id"`
	Fields []fieldValue `json:"fields"`
}

// ReadKey pages through the six core Redis value types. The page size counts
// bytes for strings and entries for all collection types.
func (d *Driver) ReadKey(ctx context.Context, a database.Access, req database.KeyReadRequest) (database.KeyReadPage, error) {
	key, err := decodeKey(req.Key)
	if err != nil {
		return database.KeyReadPage{}, err
	}
	a, rev, err := database.Normalize(a)
	if err != nil {
		return database.KeyReadPage{}, err
	}
	size, err := pageSize(req.PageSize, a)
	if err != nil {
		return database.KeyReadPage{}, err
	}
	pos, err := d.position(a, rev, "read_key", key, req.Cursor)
	if err != nil {
		return database.KeyReadPage{}, err
	}
	out := database.KeyReadPage{Connection: a.Profile.Alias, Driver: a.Profile.Driver, Key: keyBytes(key), Items: []any{}}
	err = d.runNormalized(ctx, a, rev, func(ctx context.Context, s *session) error {
		kind, err := keyKind(ctx, s, a.Profile.Connection.Username, key, false)
		if err != nil {
			return err
		}
		out.Type = kind
		if kind == "none" {
			return nil
		}
		if req.Cursor != "" && pos.Kind != kind {
			return database.Fail(contracts.StaleCursor, "Redis key type changed during pagination", false)
		}
		pos.Kind = kind
		items, more, err := d.readPage(ctx, s, key, size, &pos, a.Profile.Limits.MaxResultBytes)
		if err != nil {
			return err
		}
		out.Items = items
		if more {
			out.NextCursor = d.continuation(pos)
		}
		return nil
	})
	if err == nil {
		err = database.PayloadBound(out, a)
	}
	if err != nil {
		return database.KeyReadPage{}, err
	}
	return out, nil
}

func (d *Driver) readPage(ctx context.Context, s *session, key string, size int, pos *cursor, byteLimit int) ([]any, bool, error) {
	offset := strconv.FormatUint(pos.Key, 10)
	end := strconv.FormatUint(pos.Key+uint64(size), 10) // inclusive final index fetches one extra
	var args []string
	switch pos.Kind {
	case "string":
		args = []string{"GETRANGE", key, offset, end}
	case "list":
		args = []string{"LRANGE", key, offset, end}
	case "zset":
		args = []string{"ZRANGE", key, offset, end, "WITHSCORES"}
	case "set", "hash":
		cmd := "SSCAN"
		if pos.Kind == "hash" {
			cmd = "HSCAN"
		}
		args = []string{cmd, key, offset, "COUNT", strconv.Itoa(defaultPageSize)}
	case "stream":
		start := "-"
		if pos.StreamID != "" {
			start = "(" + pos.StreamID
		}
		args = []string{"XRANGE", key, start, "+", "COUNT", strconv.Itoa(size + 1)}
	default:
		return nil, false, inaccessibleKey()
	}
	reply, err := s.command(ctx, args...)
	if err != nil {
		return nil, false, err
	}
	if pos.Kind == "string" {
		raw, ok := reply.(string)
		if !ok || len(raw) > size+1 {
			return nil, false, badKeyReply()
		}
		chunk := raw[:min(size, len(raw))]
		pos.Key += uint64(len(chunk))
		return []any{keyBytes(chunk)}, len(raw) > size, nil
	}
	width := 1
	if pos.Kind == "hash" || pos.Kind == "zset" {
		width = 2
	}
	var portion []any
	var more bool
	if pos.Kind == "set" || pos.Kind == "hash" {
		portion, more, err = d.scanWindow(reply, pos, size, width)
		if err != nil {
			return nil, false, err
		}
	} else {
		items, ok := reply.([]any)
		if !ok || len(items)%width != 0 || len(items)/width > size+1 {
			return nil, false, badKeyReply()
		}
		more = len(items)/width > size
		portion = items[:min(len(items), size*width)]
		pos.Key += uint64(len(portion) / width)
	}
	out := make([]any, 0, len(portion)/width)
	budget := database.MetadataBudget{Limit: byteLimit}
	for i := 0; i < len(portion); i += width {
		item, err := decodeEntry(pos, portion[i:i+width])
		if err != nil {
			return nil, false, err
		}
		if err := budget.Add(item); err != nil {
			return nil, false, err
		}
		out = append(out, item)
	}
	return out, more, nil
}

func decodeEntry(pos *cursor, raw []any) (any, error) {
	if pos.Kind == "stream" {
		record, ok := raw[0].([]any)
		if !ok || len(record) != 2 {
			return nil, badKeyReply()
		}
		id, ok := record[0].(string)
		pairs, pairsOK := record[1].([]any)
		if !ok || !streamID(id) || !pairsOK || len(pairs)%2 != 0 {
			return nil, badKeyReply()
		}
		entry := streamEntry{ID: id, Fields: make([]fieldValue, 0, len(pairs)/2)}
		for i := 0; i < len(pairs); i += 2 {
			field, ok := pairs[i].(string)
			value, valueOK := pairs[i+1].(string)
			if !ok || !valueOK {
				return nil, badKeyReply()
			}
			entry.Fields = append(entry.Fields, fieldValue{keyBytes(field), keyBytes(value)})
		}
		pos.StreamID = id
		return entry, nil
	}
	first, ok := raw[0].(string)
	if !ok {
		return nil, badKeyReply()
	}
	if len(raw) == 1 {
		return keyBytes(first), nil
	}
	second, ok := raw[1].(string)
	if !ok {
		return nil, badKeyReply()
	}
	if pos.Kind == "hash" {
		return fieldValue{keyBytes(first), keyBytes(second)}, nil
	}
	return scoredMember{keyBytes(first), second}, nil
}

func streamID(id string) bool {
	ms, seq, ok := strings.Cut(id, "-")
	if !ok || len(ms) > 20 || len(seq) > 20 {
		return false
	}
	_, err1 := strconv.ParseUint(ms, 10, 64)
	_, err2 := strconv.ParseUint(seq, 10, 64)
	return err1 == nil && err2 == nil
}
