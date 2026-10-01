package mysql

import (
	"strings"

	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
	"github.com/swqa7697/data-mate/internal/database/mysql/sqlguard"
)

// readOnlyPrivileges cannot persistently write data, definitions, files or
// accounts. Every other static or dynamic privilege, including ALL and
// CREATE TEMPORARY TABLES, is rejected: without a session reset, temporary
// tables would persist between operations and could shadow real tables.
var readOnlyPrivileges = map[string]bool{
	"USAGE":               true,
	"SELECT":              true,
	"SHOW VIEW":           true,
	"SHOW DATABASES":      true,
	"EXECUTE":             true,
	"PROCESS":             true,
	"LOCK TABLES":         true,
	"REPLICATION CLIENT":  true,
	"BINLOG MONITOR":      true,
	"SLAVE MONITOR":       true,
	"REPLICA MONITOR":     true,
	"SHOW_ROUTINE":        true,
	"SHOW CREATE ROUTINE": true,
}

func auditFailure(reason string) error {
	return database.Fail(contracts.ReadOnlyViolation, "account has "+reason+"; use a dedicated read-only account", false)
}

func unverifiable() error {
	return database.Fail(contracts.ReadOnlyViolation, "cannot verify account privileges; use a dedicated read-only account", false)
}

// auditGrants inspects SHOW GRANTS lines and returns the first capability
// beyond read-only access. Lines are server output, never agent input; any
// unrecognized shape fails closed. Password hashes after IDENTIFIED are skipped.
func auditGrants(lines []string) error {
	for _, line := range lines {
		tokens, err := sqlguard.Tokens(line)
		if err != nil || len(tokens) == 0 {
			return unverifiable()
		}
		switch first := tokens[0]; {
		case first.Is("REVOKE"):
			// Partial revokes only remove privileges granted on other lines.
			continue
		case first.Is("SET") && len(tokens) > 2 && tokens[1].Is("DEFAULT") && tokens[2].Is("ROLE"):
			continue
		case !first.Is("GRANT"):
			return unverifiable()
		}
		if err := auditGrant(tokens[1:]); err != nil {
			return err
		}
	}
	return nil
}

func auditGrant(tokens []sqlguard.Token) error {
	on, to := -1, -1
	depth := 0
	for i, t := range tokens {
		switch {
		case t.Kind == sqlguard.Symbol && t.Text == "(":
			depth++
		case t.Kind == sqlguard.Symbol && t.Text == ")":
			depth--
		case depth == 0 && on < 0 && to < 0 && t.Is("ON"):
			on = i
		case depth == 0 && to < 0 && t.Is("TO"):
			to = i
		}
	}
	if to < 1 || depth != 0 {
		return unverifiable()
	}
	tail := tokens[to+1:]
	for i := 0; i+1 < len(tail); i++ {
		if tail[i].Is("GRANT") && tail[i+1].Is("OPTION") {
			return auditFailure("privilege administration grants")
		}
		if tail[i].Is("ADMIN") && tail[i+1].Is("OPTION") {
			return auditFailure("role administration grants")
		}
	}
	if on < 0 {
		// GRANT role[, role] TO grantee: a role grant. Its privileges are
		// audited separately through the role's own grants. SHOW GRANTS quotes
		// role names, so a bare word here is not a recognized shape.
		if len(tail) == 0 {
			return unverifiable()
		}
		for _, t := range tokens[:to] {
			if t.Kind == sqlguard.Word {
				return unverifiable()
			}
		}
		return nil
	}
	if on == 0 {
		return unverifiable()
	}
	if tokens[0].Is("PROXY") && on == 1 {
		return auditFailure("proxy privileges")
	}
	return auditPrivileges(tokens[:on])
}

// auditPrivileges checks a comma-separated privilege list, where an entry is
// one or more words optionally followed by a parenthesized column list.
func auditPrivileges(tokens []sqlguard.Token) error {
	var words []string
	flush := func() error {
		if len(words) == 0 {
			return unverifiable()
		}
		name := strings.ToUpper(strings.Join(words, " "))
		words = words[:0]
		if !readOnlyPrivileges[name] {
			return auditFailure(name + " privileges")
		}
		return nil
	}
	depth := 0
	for _, t := range tokens {
		switch {
		case t.Kind == sqlguard.Symbol && t.Text == "(":
			depth++
		case t.Kind == sqlguard.Symbol && t.Text == ")":
			depth--
		case depth > 0:
			// Column lists name columns, not privileges.
		case t.Kind == sqlguard.Symbol && t.Text == ",":
			if err := flush(); err != nil {
				return err
			}
		case t.Kind == sqlguard.Word:
			words = append(words, t.Text)
		default:
			return unverifiable()
		}
	}
	return flush()
}
