package mysql

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
	"github.com/swqa7697/data-mate/internal/database/mysql/sqlguard"
)

const (
	maxGrantLines = database.MaxCatalogObjects
	maxRoles      = 64
)

var versionText = regexp.MustCompile(`^(?:5\.5\.5-)?(\d{1,3})\.(\d{1,3})\.(\d{1,3})`)

// parseVersion reads @@version, such as "8.4.6" or "11.8.3-MariaDB-ubu2404".
func parseVersion(s string) (Flavor, int, bool) {
	m := versionText.FindStringSubmatch(s)
	if m == nil {
		return 0, 0, false
	}
	n := 0
	for _, part := range m[1:] {
		v, _ := strconv.Atoi(part)
		if v > 99 {
			return 0, 0, false
		}
		n = n*100 + v
	}
	flavor := MySQL
	if strings.Contains(strings.ToLower(s), "mariadb") {
		flavor = MariaDB
	}
	return flavor, n, true
}

// identify reads a connection's server once; the connection id addresses
// KILL QUERY on cancellation.
func (d *Driver) identify(ctx context.Context, s *session) (*server, error) {
	r, err := s.query(ctx, "SELECT @@version, CONNECTION_ID()")
	if err != nil {
		return nil, err
	}
	values, err := r.all(1)
	if err != nil {
		return nil, err
	}
	if len(values) != 1 || len(values[0]) != 2 {
		return nil, errMalformed
	}
	flavor, version, ok := parseVersion(str(values[0][0]))
	id, idErr := strconv.ParseUint(str(values[0][1]), 10, 64)
	if !ok || idErr != nil {
		return nil, database.Fail(contracts.QueryUnsupported, "unrecognized "+d.flavor.display()+" server version", false)
	}
	return &server{flavor: flavor, version: version, id: id}, nil
}

// supported rejects older servers and the other flavor, with a driver hint.
func (d *Driver) supported(s *server, trace *database.Trace) error {
	if trace != nil {
		trace.Version = s.version
	}
	if s.flavor != d.flavor {
		return database.Fail(contracts.QueryUnsupported, "server is "+s.flavor.display()+"; use driver "+s.flavor.Name(), false)
	}
	if minimum, label := d.flavor.minimum(); s.version < minimum {
		return database.Fail(contracts.QueryUnsupported, label+" or newer is required", false)
	}
	return nil
}

// sessionNames returns the isolation and read-only variable names, which
// MariaDB renamed from tx_* in 11.1.
func sessionNames(s *server) (string, string) {
	if s.flavor == MariaDB && s.version < 110100 {
		return "tx_isolation", "tx_read_only"
	}
	return "transaction_isolation", "transaction_read_only"
}

// pins builds the per-operation session settings from constants and the
// remaining operation budget only; no profile or agent text enters it.
func (d *Driver) pins(ctx context.Context, s *server) string {
	budget := config.MaxQueryTimeout
	if deadline, ok := ctx.Deadline(); ok {
		budget = time.Until(deadline)
	}
	ms := max(budget.Milliseconds(), 1)
	timeout := "max_execution_time=" + strconv.FormatInt(ms, 10)
	if d.flavor == MariaDB {
		timeout = "max_statement_time=" + strconv.FormatFloat(float64(ms)/1000, 'f', 3, 64)
	}
	isolation, readOnly := sessionNames(s)
	return "SET SESSION sql_mode='" + d.flavor.sqlMode() + "',character_set_client='utf8mb4',character_set_connection='utf8mb4',character_set_results='utf8mb4',time_zone='+00:00',sql_select_limit=18446744073709551615,lock_wait_timeout=1,innodb_lock_wait_timeout=1," + timeout + "," + isolation + "='READ-COMMITTED'," + readOnly + "=1"
}

func modes(s string) []string {
	out := strings.Split(strings.ToUpper(s), ",")
	slices.Sort(out)
	return out
}

// verify reads back what the guard, codecs and read-only boundary depend on.
// The read-only variable is the session default, not the current transaction;
// the transaction was explicitly started READ ONLY under that default.
func (d *Driver) verify(ctx context.Context, s *session, username string) error {
	_, readOnly := sessionNames(s.info)
	r, err := s.query(ctx, "SELECT @@session.sql_mode,@@session.character_set_client,@@session.character_set_connection,@@session.character_set_results,@@session."+readOnly+",CURRENT_USER()")
	if err != nil {
		return err
	}
	values, err := r.all(1)
	if err != nil {
		return err
	}
	if len(values) != 1 || len(values[0]) != 6 {
		return errMalformed
	}
	v := values[0]
	if !slices.Equal(modes(str(v[0])), modes(d.flavor.sqlMode())) || str(v[1]) != "utf8mb4" || str(v[2]) != "utf8mb4" || str(v[3]) != "utf8mb4" {
		return database.Fail(contracts.QueryUnsupported, d.flavor.display()+" session settings could not be pinned", false)
	}
	if str(v[4]) != "1" {
		return database.Fail(contracts.ReadOnlyViolation, "database transaction is not read-only", false)
	}
	account := str(v[5])
	at := strings.LastIndexByte(account, '@')
	if at < 0 || account[:at] != username {
		return database.Fail(contracts.PermissionDenied, "authenticated account does not match the configured username", false)
	}
	s.info.user, s.info.host = account[:at], account[at+1:]
	return nil
}

// validateAccount audits a pool's first connection in an isolated read-only
// transaction. Approval is published only after cleanup succeeds.
func (d *Driver) validateAccount(ctx context.Context, s *session, a database.Access, trace *database.Trace) (result error) {
	defer func() {
		if s.wire.exceeded.Load() {
			result = d.budgetError()
		}
	}()
	trace.Pass("dial")
	trace.Pass("authentication")
	trace.Start("version")
	s.wire.reset()
	if err := d.begin(ctx, s, a, trace); err != nil {
		s.Close()
		return d.operationError(err)
	}
	defer func() {
		if !d.cleanup(s) {
			s.Close()
			if result == nil {
				result = database.Fail(contracts.ConnectFailed, d.flavor.display()+" session cleanup failed", true)
			}
		}
	}()
	return d.audit(ctx, s)
}

// audit inspects the account's effective grants, including every role it can
// activate. It never attempts a write; unreadable grants fail closed.
func (d *Driver) audit(ctx context.Context, s *session) error {
	var lines []string
	collect := func(query string) error {
		r, err := s.query(ctx, query)
		if err != nil {
			return err
		}
		values, err := r.all(maxGrantLines - len(lines))
		if err != nil {
			return err
		}
		for _, row := range values {
			if len(row) != 1 {
				return unverifiable()
			}
			lines = append(lines, str(row[0]))
		}
		return nil
	}
	err := collect("SHOW GRANTS")
	if err == nil {
		err = d.collectRoles(ctx, s, collect)
	}
	if err != nil {
		var my *mysql.MySQLError
		if errors.As(err, &my) {
			// The server refused to show some grants: they cannot be verified.
			return unverifiable()
		}
		return err
	}
	return auditGrants(lines)
}

// collectRoles adds the grants of every role the account can activate.
func (d *Driver) collectRoles(ctx context.Context, s *session, collect func(string) error) error {
	if d.flavor == MariaDB {
		return d.collectMariaDBRoles(ctx, s, collect)
	}
	r, err := s.query(ctx, "SELECT GRANTEE,GRANTEE_HOST,ROLE_NAME,ROLE_HOST,IS_GRANTABLE,IS_MANDATORY FROM information_schema.APPLICABLE_ROLES")
	if err != nil {
		return err
	}
	roles, err := r.all(maxRoles)
	if err != nil {
		return err
	}
	var using []string
	for _, role := range roles {
		if str(role[4]) == "YES" {
			return auditFailure("role administration grants")
		}
		// USING names roles granted directly or mandatorily; the server expands
		// the roles those roles grant.
		direct := str(role[0]) == s.info.user && str(role[1]) == s.info.host
		if spec := sqlguard.Quote(str(role[2])) + "@" + sqlguard.Quote(str(role[3])); (direct || str(role[5]) == "YES") && !slices.Contains(using, spec) {
			using = append(using, spec)
		}
	}
	if len(using) == 0 {
		return nil
	}
	return collect("SHOW GRANTS FOR CURRENT_USER() USING " + strings.Join(using, ","))
}

// collectMariaDBRoles audits each role the account can activate. MariaDB shows
// a role's grants to an ordinary account only while the role is active, so each
// directly granted role (or one granted to PUBLIC) is set in turn; its grants
// include the roles it was granted. The session's original role is restored.
func (d *Driver) collectMariaDBRoles(ctx context.Context, s *session, collect func(string) error) error {
	r, err := s.query(ctx, "SELECT GRANTEE,ROLE_NAME,IS_GRANTABLE,CURRENT_ROLE() FROM information_schema.APPLICABLE_ROLES")
	if err != nil {
		return err
	}
	roles, err := r.all(maxRoles)
	if err != nil {
		return err
	}
	account := s.info.user + "@" + s.info.host
	var direct []string
	original := "NONE"
	for _, role := range roles {
		if str(role[2]) == "YES" {
			return auditFailure("role administration grants")
		}
		if grantee := str(role[0]); (grantee == account || grantee == "PUBLIC") && !slices.Contains(direct, str(role[1])) {
			direct = append(direct, str(role[1]))
		}
		if current, ok := text(role[3]); ok {
			original = sqlguard.Quote(current)
		}
	}
	for _, role := range direct {
		if err = s.exec(ctx, "SET ROLE "+sqlguard.Quote(role)); err != nil {
			return err
		}
		if err = collect("SHOW GRANTS FOR CURRENT_ROLE"); err != nil {
			return err
		}
	}
	if len(direct) > 0 {
		// A connection whose role cannot be restored is closed by the caller.
		if err = s.exec(ctx, "SET ROLE "+original); err != nil {
			return err
		}
	}
	// MariaDB 10.11 introduced grants to PUBLIC, which apply to every account.
	return collect("SHOW GRANTS FOR PUBLIC")
}
