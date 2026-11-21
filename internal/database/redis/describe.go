package redis

import (
	"cmp"
	"context"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/swqa7697/data-mate/internal/database"
)

const describeSampleSize = 8

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
	type observation struct{ pattern, kind string }
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
			observations = append(observations, observation{pattern, kind})
		}
	}

	groups := make(map[observation]int)
	for _, item := range observations {
		if patternCounts[item.pattern] < 2 {
			item.pattern = ""
		}
		groups[item]++
	}
	sample := database.RedisKeyspace{ObservedKeys: len(observations), Groups: make([]database.RedisKeyGroup, 0, len(groups))}
	for item, count := range groups {
		sample.Groups = append(sample.Groups, database.RedisKeyGroup{Pattern: item.pattern, Type: item.kind, Count: count})
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
