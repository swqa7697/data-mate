package redis

import (
	"cmp"
	"context"
	"encoding/base64"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/swqa7697/data-mate/internal/database"
)

const (
	describeSampleSize = 64
	describeScanCount  = 16
	describeScanLimit  = 8
	groupExampleLimit  = 3
	exampleByteLimit   = 96
)

// Escape Redis glob metacharacters so a namespace can be passed to SCAN MATCH.
var escapeGlob = strings.NewReplacer(`\`, `\\`, "*", `\*`, "?", `\?`, "[", `\[`, "]", `\]`)

// DescribeDatabase summarizes one small sample of readable keys in the selected database.
func (d *Driver) DescribeDatabase(ctx context.Context, a database.Access) (database.DatabaseDescription, error) {
	a, rev, err := database.Normalize(a)
	if err != nil {
		return database.DatabaseDescription{}, err
	}
	var sample database.RedisKeyspace
	err = d.runNormalized(ctx, a, rev, func(ctx context.Context, s *session) error {
		var err error
		sample, err = sampleKeyspace(ctx, s, min(describeSampleSize, a.Profile.Limits.MaxRows))
		return err
	})
	if err != nil {
		return database.DatabaseDescription{}, err
	}
	out := database.DatabaseDescription{Version: 2, Alias: a.Profile.Alias, Driver: a.Profile.Driver, Database: a.Profile.Connection.Database, Keyspace: &sample}
	if err := out.CheckSize(a.Profile.Limits.MaxResultBytes); err != nil {
		return database.DatabaseDescription{}, err
	}
	return out, nil
}

type keyObservation struct {
	key, kind, prefix string
	tagged            bool
}

func sampleKeyspace(ctx context.Context, s *session, limit int) (database.RedisKeyspace, error) {
	var observations []keyObservation
	seen := make(map[string]bool)
	var cursor uint64
	complete := false
	for range describeScanLimit {
		reply, err := s.command(ctx, "SCAN", strconv.FormatUint(cursor, 10), "COUNT", strconv.Itoa(min(describeScanCount, limit)))
		if err != nil {
			return database.RedisKeyspace{}, err
		}
		next, keys, err := scanReply(reply)
		if err != nil {
			return database.RedisKeyspace{}, err
		}
		// COUNT is a hint. Duplicates and denied keys also consume the work budget.
		n := min(len(keys), limit)
		for _, raw := range keys[:n] {
			key, ok := raw.(string)
			if !ok {
				return database.RedisKeyspace{}, badKeyReply()
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			kind, err := listedKeyKind(ctx, s, key)
			if err != nil {
				return database.RedisKeyspace{}, err
			}
			if kind != "" {
				prefix, tagged := keyPrefix(key)
				observations = append(observations, keyObservation{key: key, kind: kind, prefix: prefix, tagged: tagged})
			}
		}
		limit -= n
		complete = next == 0 && n == len(keys)
		if next == 0 || limit == 0 {
			break
		}
		cursor = next
	}
	return summarizeKeys(observations, complete), nil
}

func summarizeKeys(keys []keyObservation, complete bool) database.RedisKeyspace {
	prefixCounts := make(map[string]int)
	for _, key := range keys {
		prefixCounts[key.prefix]++
	}
	type groupID struct{ prefix, kind string }
	groups := make(map[groupID]*database.RedisKeyGroup)
	for _, item := range keys {
		id := groupID{item.prefix, item.kind}
		if !item.tagged && prefixCounts[id.prefix] < 2 {
			id.prefix = ""
		}
		group := groups[id]
		if group == nil {
			group = &database.RedisKeyGroup{Type: id.kind}
			if id.prefix != "" {
				group.Pattern = escapeGlob.Replace(id.prefix) + ":*"
			}
			groups[id] = group
		}
		group.Count++
		if len(group.Examples) < groupExampleLimit {
			group.Examples = append(group.Examples, keyExample(item.key))
		}
	}
	sample := database.RedisKeyspace{ObservedKeys: len(keys), ScanComplete: complete, Groups: make([]database.RedisKeyGroup, 0, len(groups))}
	for _, group := range groups {
		sample.Groups = append(sample.Groups, *group)
	}
	slices.SortFunc(sample.Groups, func(a, b database.RedisKeyGroup) int {
		if a.Pattern != b.Pattern {
			if a.Pattern == "" {
				return 1
			}
			if b.Pattern == "" {
				return -1
			}
			return strings.Compare(a.Pattern, b.Pattern)
		}
		return cmp.Compare(a.Type, b.Type)
	})
	return sample
}

// Preserve the full key's encoding even if its bounded prefix looks like text.
func keyExample(key string) database.RedisKeyExample {
	if len(key) <= exampleByteLimit {
		return database.RedisKeyExample{KeyBytes: keyBytes(key)}
	}
	prefix := key[:exampleByteLimit]
	if !utf8.ValidString(key) {
		return database.RedisKeyExample{
			KeyBytes:  database.KeyBytes{Value: base64.StdEncoding.EncodeToString([]byte(prefix)), Encoding: "base64"},
			Truncated: true,
		}
	}
	end := exampleByteLimit
	for !utf8.RuneStart(key[end]) {
		end--
	}
	return database.RedisKeyExample{KeyBytes: database.KeyBytes{Value: key[:end], Encoding: "utf8"}, Truncated: true}
}

// keyPrefix preserves a leading {tag}: as a namespace even when the tag
// contains colons. Only the prefix needs to fit the JSON string used by list_keys.
func keyPrefix(key string) (string, bool) {
	if strings.HasPrefix(key, "{") {
		end := strings.IndexByte(key, '}')
		if end > 1 && strings.HasPrefix(key[end+1:], ":") {
			prefix := key[:end+1]
			if utf8.ValidString(prefix) {
				return prefix, true
			}
			return "", false
		}
	}
	prefix, _, ok := strings.Cut(key, ":")
	if !ok || !utf8.ValidString(prefix) {
		return "", false
	}
	return prefix, false
}
