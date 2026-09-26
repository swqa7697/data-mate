package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/swqa7697/data-mate/internal/database"
)

func readDefinitions(ctx context.Context, tx pgx.Tx, budget *metadataBudget, sql string, args ...any) ([]database.Definition, error) {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []database.Definition{}
	for rows.Next() {
		var v database.Definition
		if err = rows.Scan(&v.Name, &v.Kind, &v.Definition); err != nil {
			return nil, err
		}
		if err = budget.add(v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func tableDefinitions(ctx context.Context, tx pgx.Tx, oid uint32, a database.Access, out *database.Description, budget *metadataBudget) error {
	// Preserve the existing FK endpoint policy even for deparsed constraints.
	args := append(scopeArgs(a.Profile.Scope), oid)
	var err error
	out.Constraints, err = readDefinitions(ctx, tx, budget, `SELECT k.conname::text,
 CASE k.contype WHEN 'p' THEN 'primary' WHEN 'u' THEN 'unique' WHEN 'f' THEN 'foreign' WHEN 'c' THEN 'check' WHEN 'x' THEN 'exclusion' WHEN 'n' THEN 'not_null' ELSE k.contype::text END,
 pg_catalog.pg_get_constraintdef(k.oid)
 FROM pg_catalog.pg_constraint k LEFT JOIN pg_catalog.pg_class c ON c.oid=k.confrelid LEFT JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
 WHERE k.conrelid=$3 AND (k.contype<>'f' OR (`+visibleSQL+`)) ORDER BY k.conname::text COLLATE "C" LIMIT 4097`, args...)
	if err != nil {
		return err
	}
	out.Indexes, err = readDefinitions(ctx, tx, budget, `SELECT c.relname::text,am.amname::text,pg_catalog.pg_get_indexdef(i.indexrelid)
 FROM pg_catalog.pg_index i JOIN pg_catalog.pg_class c ON c.oid=i.indexrelid JOIN pg_catalog.pg_am am ON am.oid=c.relam
 WHERE i.indrelid=$1 ORDER BY c.relname::text COLLATE "C" LIMIT 4097`, oid)
	if err != nil {
		return err
	}
	if err = tableTriggers(ctx, tx, oid, out, budget); err != nil {
		return err
	}
	return tablePolicies(ctx, tx, oid, out, budget)
}

func tableTriggers(ctx context.Context, tx pgx.Tx, oid uint32, out *database.Description, budget *metadataBudget) error {
	rows, err := tx.Query(ctx, `SELECT tgname::text,pg_catalog.pg_get_triggerdef(oid),
 CASE tgenabled WHEN 'O' THEN 'origin' WHEN 'D' THEN 'disabled' WHEN 'R' THEN 'replica' ELSE 'always' END,tgisinternal
 FROM pg_catalog.pg_trigger WHERE tgrelid=$1 ORDER BY tgname::text COLLATE "C" LIMIT 4097`, oid)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var v database.Trigger
		if err = rows.Scan(&v.Name, &v.Definition, &v.Enabled, &v.Internal); err != nil {
			return err
		}
		if err = budget.add(v); err != nil {
			return err
		}
		out.Triggers = append(out.Triggers, v)
	}
	return rows.Err()
}

func tablePolicies(ctx context.Context, tx pgx.Tx, oid uint32, out *database.Description, budget *metadataBudget) error {
	rows, err := tx.Query(ctx, `SELECT polname::text,
 CASE polcmd WHEN '*' THEN 'all' WHEN 'r' THEN 'select' WHEN 'a' THEN 'insert' WHEN 'w' THEN 'update' ELSE 'delete' END,polpermissive,
 ARRAY(SELECT CASE WHEN role=0 THEN 'public' ELSE pg_catalog.pg_get_userbyid(role)::text END FROM pg_catalog.unnest(polroles) role ORDER BY role),
 pg_catalog.pg_get_expr(polqual,polrelid),pg_catalog.pg_get_expr(polwithcheck,polrelid)
 FROM pg_catalog.pg_policy WHERE polrelid=$1 ORDER BY polname::text COLLATE "C" LIMIT 4097`, oid)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var v database.Policy
		if err = rows.Scan(&v.Name, &v.Command, &v.Permissive, &v.Roles, &v.Using, &v.Check); err != nil {
			return err
		}
		if err = budget.add(v); err != nil {
			return err
		}
		out.Policies = append(out.Policies, v)
	}
	return rows.Err()
}
