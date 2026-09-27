package postgres

import (
	"context"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
)

// Approval is published only after this isolated transaction has been cleaned up.
func validateAccountConnection(ctx context.Context, c *pgx.Conn, a database.Access, trace *diagnosticTrace) (result error) {
	defer func() {
		if exceeded(c) {
			result = database.Fail(contracts.ResourceLimit, "PostgreSQL message exceeds limit", false)
		}
	}()
	trace.pass("dial")
	trace.pass("authentication")
	trace.start("version")
	tx, err := c.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly})
	if err != nil {
		return safeError(err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		err := tx.Rollback(cleanup)
		if err == nil {
			_, err = c.Exec(cleanup, "DISCARD ALL")
		}
		if err != nil {
			closeConn(c)
			if result == nil {
				result = safeError(err)
			}
		}
	}()
	if _, err = tx.Exec(ctx, "SELECT pg_catalog.set_config('statement_timeout',$1,true),pg_catalog.set_config('lock_timeout','1000',true)", strconv.Itoa(a.Profile.Limits.QueryTimeoutMS)); err != nil {
		return safeError(err)
	}
	version, err := checkReadOnlyObserved(ctx, tx, a.Profile.Connection.Username, trace)
	if err != nil {
		return err
	}
	return auditAccount(ctx, tx, version)
}

// This inexpensive check remains per transaction; account audits belong to pools.
func checkReadOnlyObserved(ctx context.Context, tx pgx.Tx, expected string, trace *diagnosticTrace) (int, error) {
	var version int
	var actual, session string
	var readOnly bool
	err := tx.QueryRow(ctx, `SELECT current_setting('server_version_num')::int,current_user::text,session_user::text,current_setting('transaction_read_only')::boolean`).Scan(&version, &actual, &session, &readOnly)
	if err != nil {
		return 0, safeError(err)
	}
	if version < 160000 {
		return 0, database.Fail(contracts.QueryUnsupported, "PostgreSQL 16 or newer is required", false)
	}
	if trace != nil {
		trace.version = version
	}
	trace.pass("version")
	trace.start("read_only")
	if actual != expected || session != expected {
		return 0, database.Fail(contracts.PermissionDenied, "authenticated role does not match the configured account", false)
	}
	if !readOnly {
		return 0, database.Fail(contracts.ReadOnlyViolation, "database transaction is not read-only", false)
	}
	return version, nil
}

// auditAccount inspects effective persistent-write capabilities, not routine bodies.
// PostgreSQL's transaction enforcement and trusted installed code remain the boundary.
func auditAccount(ctx context.Context, tx pgx.Tx, version int) error {
	privileges := "INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER"
	if version >= 170000 {
		privileges += ",MAINTAIN"
	}
	// PostgreSQL grants PUBLIC UPDATE on pg_settings for session SET semantics.
	// It cannot update persistent relation data; other grants remain audited.
	var reason string
	err := tx.QueryRow(ctx, `WITH roles AS MATERIALIZED (
 SELECT oid,rolsuper,rolcreaterole,rolcreatedb,rolreplication,rolname,pg_catalog.pg_has_role(session_user,oid,'SET') AS assumable FROM pg_catalog.pg_roles
 WHERE pg_catalog.pg_has_role(session_user,oid,'SET') OR pg_catalog.pg_has_role(session_user,oid,'USAGE')
), db AS (SELECT oid FROM pg_catalog.pg_database WHERE datname=current_database())
SELECT CASE WHEN EXISTS (SELECT FROM roles WHERE assumable AND (rolsuper OR rolcreaterole OR rolcreatedb OR rolreplication))
 OR EXISTS (SELECT FROM roles r,pg_catalog.pg_roles capability
 WHERE capability.rolname IN ('pg_write_all_data','pg_write_server_files','pg_execute_server_program','pg_maintain')
 AND pg_catalog.pg_has_role(r.oid,capability.oid,'USAGE')) THEN 'administrative role capabilities'
 WHEN EXISTS (SELECT FROM pg_catalog.pg_auth_members m JOIN roles r ON r.oid=m.member WHERE m.admin_option) THEN 'role administration grants'
 WHEN EXISTS (SELECT FROM roles r,pg_catalog.pg_shdepend d,db
 WHERE d.refclassid='pg_catalog.pg_authid'::regclass AND d.deptype='o'
 AND (d.dbid=db.oid OR d.dbid=0) AND pg_catalog.pg_has_role(r.oid,d.refobjid,'USAGE')
 AND NOT EXISTS (SELECT FROM pg_catalog.pg_depend local JOIN pg_catalog.pg_namespace n ON n.oid=local.refobjid
 WHERE local.classid=d.classid AND local.objid=d.objid AND local.refclassid='pg_catalog.pg_namespace'::regclass
 AND (pg_catalog.pg_is_other_temp_schema(n.oid) OR n.oid=pg_catalog.pg_my_temp_schema()))) THEN 'persistent object ownership'
 WHEN EXISTS (SELECT FROM roles r,db WHERE pg_catalog.has_database_privilege(r.oid,db.oid,'CREATE')) THEN 'database creation privileges'
 WHEN EXISTS (SELECT FROM roles r,pg_catalog.pg_namespace n
 WHERE n.nspname NOT LIKE 'pg_temp_%' AND n.nspname NOT LIKE 'pg_toast_temp_%'
 AND pg_catalog.has_schema_privilege(r.oid,n.oid,'CREATE')) THEN 'schema creation privileges'
 WHEN EXISTS (SELECT FROM roles r,pg_catalog.pg_class c WHERE c.relpersistence<>'t'
 AND c.relkind IN ('r','p','v','m','f') AND
 (pg_catalog.has_table_privilege(r.oid,c.oid,CASE WHEN c.oid='pg_catalog.pg_settings'::regclass THEN replace($1,'UPDATE,','') ELSE $1 END)
 OR pg_catalog.has_any_column_privilege(r.oid,c.oid,CASE WHEN c.oid='pg_catalog.pg_settings'::regclass THEN 'INSERT,REFERENCES' ELSE 'INSERT,UPDATE,REFERENCES' END))) THEN 'table or column write privileges'
 WHEN EXISTS (SELECT FROM roles r,pg_catalog.pg_class c WHERE c.relpersistence<>'t' AND c.relkind='S'
 AND pg_catalog.has_sequence_privilege(r.oid,c.oid,'USAGE,UPDATE')) THEN 'sequence write privileges'
 WHEN EXISTS (SELECT FROM pg_catalog.pg_largeobject_metadata l,
 LATERAL pg_catalog.aclexplode(COALESCE(l.lomacl,pg_catalog.acldefault('L',l.lomowner))) a
 WHERE a.privilege_type='UPDATE' AND (a.grantee=0 OR EXISTS
 (SELECT FROM roles r WHERE pg_catalog.pg_has_role(r.oid,a.grantee,'USAGE')))) THEN 'large-object write privileges'
 WHEN EXISTS (SELECT FROM pg_catalog.pg_parameter_acl p,LATERAL pg_catalog.aclexplode(p.paracl) a
 WHERE a.privilege_type='ALTER SYSTEM' AND (a.grantee=0 OR EXISTS
 (SELECT FROM roles r WHERE pg_catalog.pg_has_role(r.oid,a.grantee,'USAGE')))) THEN 'persistent server-setting privileges' ELSE '' END`, privileges).Scan(&reason)
	if err != nil {
		return safeError(err)
	}
	if reason != "" {
		return database.Fail(contracts.ReadOnlyViolation, "account has "+reason+"; use a dedicated read-only account", false)
	}
	return nil
}
