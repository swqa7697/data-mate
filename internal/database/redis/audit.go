package redis

import (
	"context"
	"strings"

	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
)

func admissionDenied() error {
	return database.Fail(contracts.ReadOnlyViolation, "Redis ACL does not establish a dedicated read-only account", false)
}

// Only these command grants are safe for the account, regardless of its key
// patterns. Session setup and ACL introspection are private driver operations.
func allowedGrant(command string) bool {
	switch command {
	case "hello", "auth", "ping", "echo", "quit", "select", "acl|whoami", "acl|getuser", "acl|cat", "acl|dryrun",
		"scan", "type", "ttl", "pttl", "exists", "dbsize", "get", "getrange", "strlen", "llen", "lrange", "hlen", "hscan",
		"scard", "sscan", "zcard", "zrange", "xlen", "xrange":
		return true
	}
	return false
}

func auditAccount(ctx context.Context, s *session, username string) error {
	v, err := s.command(ctx, "ACL", "GETUSER", username)
	if err != nil {
		return err
	}
	root, ok := fields(v)
	if !ok || len(root) != 6 {
		return admissionDenied()
	}
	// Discard password hashes immediately. Nothing from the server's ACL or
	// upstream text is included in an error or response.
	delete(root, "passwords")
	flags, ok := root["flags"].([]any)
	if !ok {
		return admissionDenied()
	}
	enabled := false
	for _, v := range flags {
		switch v {
		case "on":
			enabled = true
		case "nopass", "sanitize-payload", "skip-sanitize-payload":
		default:
			return admissionDenied()
		}
	}
	if !enabled {
		return admissionDenied()
	}
	selectors, ok := root["selectors"].([]any)
	if !ok || len(selectors) > 256 {
		return admissionDenied()
	}
	delete(root, "flags")
	delete(root, "selectors")
	cache := make(map[string][]string)
	loadCategory := func(category string) ([]string, error) {
		if names, ok := cache[category]; ok {
			return names, nil
		}
		if len(cache) >= 64 {
			return nil, admissionDenied()
		}
		reply, err := s.command(ctx, "ACL", "CAT", category)
		if err != nil {
			return nil, err
		}
		list, ok := reply.([]any)
		if !ok || len(list) == 0 || len(list) > 8192 {
			return nil, admissionDenied()
		}
		names := make([]string, 0, len(list))
		for _, item := range list {
			name, ok := item.(string)
			if !ok || !commandName(name) {
				return nil, admissionDenied()
			}
			names = append(names, name)
		}
		cache[category] = names
		return names, nil
	}
	// Keep category expansion bounded across selectors. Repeated rules must
	// not turn one bounded wire reply into unbounded local audit work.
	remaining := 1 << 16
	if err := auditSelector(root, loadCategory, &remaining); err != nil {
		return err
	}
	for _, item := range selectors {
		selector, ok := fields(item)
		if !ok {
			return admissionDenied()
		}
		if err := auditSelector(selector, loadCategory, &remaining); err != nil {
			return err
		}
	}
	return nil
}

func commandName(name string) bool {
	if len(name) == 0 || len(name) > 128 {
		return false
	}
	for _, c := range name {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '|' && c != '_' && c != '-' && c != '.' {
			return false
		}
	}
	return true
}

func auditSelector(selector map[string]any, category func(string) ([]string, error), remaining *int) error {
	rules, ok := selector["commands"].(string)
	_, keys := selector["keys"].(string)
	_, channels := selector["channels"].(string)
	if len(selector) != 3 || !ok || !keys || !channels || len(rules) > 128<<10 {
		return admissionDenied()
	}
	items := strings.Fields(rules)
	// +@all grants future commands in addition to today's server commands.
	if len(items) == 0 || len(items) > 4096 || items[0] != "-@all" {
		return admissionDenied()
	}
	granted := make(map[string]bool)
	for _, rule := range items[1:] {
		if len(rule) < 2 || (rule[0] != '+' && rule[0] != '-') {
			return admissionDenied()
		}
		name := rule[1:]
		add := rule[0] == '+'
		commands := []string{name}
		if strings.HasPrefix(name, "@") {
			name = name[1:]
			if !commandName(name) || name == "all" {
				return admissionDenied()
			}
			var err error
			commands, err = category(name)
			if err != nil {
				return err
			}
		} else if !commandName(name) {
			return admissionDenied()
		}
		*remaining -= len(commands)
		if *remaining < 0 {
			return database.Fail(contracts.ResourceLimit, "Redis ACL exceeds audit limit", false)
		}
		for _, cmd := range commands {
			granted[cmd] = add
		}
	}
	for name, enabled := range granted {
		if enabled && !allowedGrant(name) {
			return admissionDenied()
		}
	}
	return nil
}
