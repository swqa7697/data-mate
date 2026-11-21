package redis

import (
	"cmp"
	"context"
	"encoding/base64"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/swqa7697/data-mate/internal/database"
)

const (
	describeSampleSize = 8
	groupExampleLimit  = 3
	exampleByteLimit   = 96
)

var literalPrefix = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,32}$`)

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
	type groupID struct{ pattern, kind string }
	type observation struct {
		group groupID
		key   string
	}
	var observations []observation
	patternCounts := make(map[string]int)
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
			pattern := keyPattern(key)
			patternCounts[pattern]++
			observations = append(observations, observation{groupID{pattern, kind}, key})
		}
	}

	groups := make(map[groupID]*database.RedisKeyGroup)
	for _, item := range observations {
		id := item.group
		if patternCounts[id.pattern] < 2 {
			id.pattern = ""
		}
		group := groups[id]
		if group == nil {
			group = &database.RedisKeyGroup{Pattern: id.pattern, Type: id.kind}
			groups[id] = group
		}
		group.Count++
		if id.pattern == "" && len(group.Examples) < groupExampleLimit {
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

// Only expose a literal first segment that users can paste into SCAN MATCH.
func keyPattern(key string) string {
	prefix, _, ok := strings.Cut(key, ":")
	if !ok || !literalPrefix.MatchString(prefix) || !utf8.ValidString(key) {
		return ""
	}
	for _, r := range key {
		if !unicode.IsPrint(r) {
			return ""
		}
	}
	return prefix + ":*"
}
