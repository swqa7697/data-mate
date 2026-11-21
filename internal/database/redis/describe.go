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
	describeSampleSize = 8
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

func sampleKeyspace(ctx context.Context, s *session, limit int) (database.RedisKeyspace, error) {
	reply, err := s.command(ctx, "SCAN", "0", "COUNT", strconv.Itoa(limit))
	if err != nil {
		return database.RedisKeyspace{}, err
	}
	_, keys, err := scanReply(reply)
	if err != nil {
		return database.RedisKeyspace{}, err
	}
	type groupID struct{ prefix, kind string }
	type observation struct {
		group  groupID
		key    string
		tagged bool
	}
	var observations []observation
	prefixCounts := make(map[string]int)
	seen := make(map[string]bool)
	// COUNT is a hint. Bound work even when the first batch is oversized or all keys are denied.
	for _, raw := range keys[:min(len(keys), limit)] {
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
			prefixCounts[prefix]++
			observations = append(observations, observation{groupID{prefix, kind}, key, tagged})
		}
	}

	groups := make(map[groupID]*database.RedisKeyGroup)
	for _, item := range observations {
		id := item.group
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
		if id.prefix == "" && len(group.Examples) < groupExampleLimit {
			group.Examples = append(group.Examples, keyExample(item.key))
		}
	}
	sample := database.RedisKeyspace{ObservedKeys: len(observations), Groups: make([]database.RedisKeyGroup, 0, len(groups))}
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
	return sample, nil
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
