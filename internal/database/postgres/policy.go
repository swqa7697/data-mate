package postgres

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
)

// Privilege functions include PUBLIC ACLs and inherited privileges. SET-reachable
// roles are evaluated separately so NOINHERIT cannot hide elevated capabilities.
// Both membership options exist on the supported PostgreSQL 16+ boundary.
const roleSQL = `WITH reachable AS MATERIALIZED (
 SELECT r.* FROM pg_catalog.pg_roles r
 WHERE r.rolname=current_user OR pg_catalog.pg_has_role(current_user,r.oid,'USAGE') OR pg_catalog.pg_has_role(current_user,r.oid,'SET')
), app AS MATERIALIZED (
 SELECT c.oid,c.relowner,c.relkind FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname NOT LIKE 'pg\_%' AND n.nspname<>'information_schema'
)
SELECT
 EXISTS (SELECT FROM reachable WHERE rolsuper OR rolbypassrls OR rolcreaterole OR rolcreatedb OR rolreplication)
 OR EXISTS (SELECT FROM reachable r JOIN pg_catalog.pg_auth_members m ON m.member=r.oid WHERE m.admin_option)
 OR EXISTS (SELECT FROM reachable r,pg_catalog.pg_roles elevated WHERE elevated.rolname IN
 ('pg_read_server_files','pg_write_server_files','pg_execute_server_program','pg_write_all_data','pg_maintain') AND pg_catalog.pg_has_role(r.oid,elevated.oid,'USAGE'))
 OR EXISTS (SELECT FROM reachable r,pg_catalog.pg_database b WHERE b.datname=current_database() AND (b.datdba=r.oid OR pg_catalog.has_database_privilege(r.oid,b.oid,'CREATE')))
 OR EXISTS (SELECT FROM reachable r,pg_catalog.pg_namespace n WHERE n.nspname NOT LIKE 'pg\_%' AND n.nspname<>'information_schema' AND (n.nspowner=r.oid OR pg_catalog.has_schema_privilege(r.oid,n.oid,'CREATE')))
 OR EXISTS (SELECT FROM reachable r,app c WHERE c.relowner=r.oid OR
 (c.relkind IN ('r','p','v','m','f') AND (pg_catalog.has_table_privilege(r.oid,c.oid,CASE WHEN c.relkind IN ('r','p','m') THEN $1::text ELSE 'INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER' END) OR pg_catalog.has_any_column_privilege(r.oid,c.oid,'INSERT,UPDATE,REFERENCES')))
 OR (c.relkind='S' AND pg_catalog.has_sequence_privilege(r.oid,c.oid,'USAGE,UPDATE')))
 OR EXISTS (SELECT FROM reachable r,pg_catalog.pg_proc f JOIN pg_catalog.pg_namespace n ON n.oid=f.pronamespace
 WHERE n.nspname='pg_catalog' AND f.proname IN ('pg_read_file','pg_read_binary_file','pg_write_file','pg_ls_dir','pg_stat_file','lo_import','lo_export')
 AND pg_catalog.has_function_privilege(r.oid,f.oid,'EXECUTE'))`

func checkRole(ctx context.Context, tx pgx.Tx, expected string) (int, error) {
	return checkRoleObserved(ctx, tx, expected, nil)
}
func checkRoleObserved(ctx context.Context, tx pgx.Tx, expected string, trace *diagnosticTrace) (int, error) {
	var version int
	var actual, session string
	var connect bool
	err := tx.QueryRow(ctx, `SELECT current_setting('server_version_num')::int,current_user::text,session_user::text,pg_catalog.has_database_privilege(current_database(),'CONNECT')`).Scan(&version, &actual, &session, &connect)
	if err != nil {
		return 0, safeError(err)
	}
	if version < 160000 {
		return 0, database.Fail(contracts.QueryUnsupported, "PostgreSQL 16 or newer is required", false)
	}
	trace.pass("version")
	trace.start("policy")
	if actual != expected || session != expected || !connect {
		return 0, database.Fail(contracts.PolicyUnsafe, "authenticated role does not match the configured read-only role", false)
	}
	// Share the fresh role/relation scan with MAINTAIN checks on supported servers.
	// PostgreSQL 16 must never receive MAINTAIN, even on an unevaluated SQL branch.
	privileges := "INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER"
	if version >= 170000 {
		privileges += ",MAINTAIN"
	}
	var unsafe bool
	if err = tx.QueryRow(ctx, roleSQL, privileges).Scan(&unsafe); err != nil {
		return 0, safeError(err)
	}
	if unsafe {
		return 0, database.Fail(contracts.PolicyUnsafe, "use a non-owner role with CONNECT, USAGE and SELECT only; remove elevated, creation and write privileges, including reachable roles and PUBLIC grants", false)
	}
	return version, nil
}
