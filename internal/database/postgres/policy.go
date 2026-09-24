package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
)

// Account grants are an operator responsibility. This checks the actual transaction.
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
