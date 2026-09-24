package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
	"github.com/swqa7697/data-mate/internal/database/postgres/sqlpolicy"
)

// Compile validates SQL through the same admission, role and catalog boundary as
// metadata. It never prepares, plans or executes agent SQL. The resulting plan is
// not a lease: Query independently compiles, locks and rechecks before execution.
func (d *Driver) Compile(ctx context.Context, a database.Access, sql string, parameters []sqlpolicy.Parameter) (sqlpolicy.Compiled, error) {
	parsed, err := sqlpolicy.Parse(sql)
	if err != nil {
		return sqlpolicy.Compiled{}, err
	}
	var out sqlpolicy.Compiled
	err = d.run(ctx, a, func(ctx context.Context, tx pgx.Tx, version int) error {
		var e error
		out, e = compileSQL(ctx, tx, a.Profile.Scope, version, parsed, parameters)
		return e
	})
	if err != nil {
		return sqlpolicy.Compiled{}, err
	}
	return out, nil
}
func compileSQL(ctx context.Context, tx pgx.Tx, scope config.Scope, version int, p *sqlpolicy.Parsed, parameters []sqlpolicy.Parameter) (sqlpolicy.Compiled, error) {
	if err := verifyCatalog(ctx, tx, version); err != nil {
		return sqlpolicy.Compiled{}, err
	}
	return p.Compile(ctx, version/10000, &compilerCatalog{tx: tx, scope: scope}, parameters)
}

func verifyCatalog(ctx context.Context, tx pgx.Tx, version int) error {
	major := version / 10000
	if _, err := sqlpolicy.CatalogFingerprint(major); err != nil {
		return err
	}
	var size *int64
	var fingerprint []byte
	if err := tx.QueryRow(ctx, sqlpolicy.CatalogFingerprintSQL, major).Scan(&size, &fingerprint); err != nil {
		var scan pgx.ScanArgError
		if errors.Is(err, pgx.ErrNoRows) || errors.As(err, &scan) {
			return database.Fail(contracts.QueryUnsupported, "invalid PostgreSQL catalog fingerprint", false)
		}
		return err
	}
	if size == nil {
		return database.Fail(contracts.QueryUnsupported, "missing PostgreSQL catalog size", false)
	}
	return sqlpolicy.VerifyCatalogFingerprint(major, *size, fingerprint)
}

type compilerCatalog struct {
	tx          pgx.Tx
	scope       config.Scope
	objects     int
	columnCount int
}

func unsupportedRelation() error {
	return database.Fail(contracts.QueryUnsupported, "relation execution features are unsupported", false)
}
func (c *compilerCatalog) Resolve(ctx context.Context, schema, name string, only bool) (sqlpolicy.Relation, error) {
	out := sqlpolicy.Relation{Schema: schema, Name: name, Only: only}
	if !validName(schema) || !validName(name) || strings.HasPrefix(schema, "pg_") || schema == "information_schema" || !c.scope.ContainsName(schema, name) {
		return out, database.Fail(contracts.ScopeDenied, "relation is outside the accessible scope", false)
	}
	var kind string
	err := c.tx.QueryRow(ctx, `SELECT c.oid,c.relkind::text FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND c.relname=$2 AND pg_catalog.has_schema_privilege(n.oid,'USAGE') AND pg_catalog.has_table_privilege(c.oid,'SELECT')`, schema, name).Scan(&out.OID, &kind)
	if err == pgx.ErrNoRows {
		return out, database.Fail(contracts.ScopeDenied, "relation is outside the accessible scope", false)
	}
	if err != nil {
		return out, err
	}
	if kind != "r" && kind != "p" || only && kind == "p" {
		return out, unsupportedRelation()
	}
	rows, err := c.tx.Query(ctx, `WITH RECURSIVE tree(oid) AS (SELECT $1::oid UNION SELECT i.inhrelid FROM pg_catalog.pg_inherits i JOIN tree t ON i.inhparent=t.oid WHERE NOT $2::boolean)
 SELECT c.oid,n.nspname,c.relname,c.relkind::text,c.relrowsecurity,
 c.relpersistence<>'t' AND (c.relkind='p' OR c.relam=2) AND c.relkind IN ('r','p')
 AND pg_catalog.has_schema_privilege(n.oid,'USAGE') AND pg_catalog.has_table_privilege(c.oid,'SELECT')
 AND NOT EXISTS(SELECT FROM pg_catalog.pg_rewrite r WHERE r.ev_class=c.oid)
 AND NOT EXISTS(SELECT FROM pg_catalog.pg_inherits i WHERE i.inhrelid=c.oid AND i.inhdetachpending),
 EXISTS(SELECT FROM pg_catalog.pg_inherits i WHERE i.inhrelid=c.oid)
 FROM tree JOIN pg_catalog.pg_class c ON c.oid=tree.oid JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace ORDER BY c.oid LIMIT 4097`, out.OID, only)
	if err != nil {
		return out, err
	}
	type node struct {
		id                 uint32
		schema, name, kind string
		rls, valid, parent bool
	}
	nodes := []node{}
	for rows.Next() {
		var n node
		if err = rows.Scan(&n.id, &n.schema, &n.name, &n.kind, &n.rls, &n.valid, &n.parent); err != nil {
			rows.Close()
			return out, err
		}
		nodes = append(nodes, n)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return out, err
	}
	c.objects += len(nodes)
	if len(nodes) == 0 || c.objects > maxCatalogObjects {
		return out, database.Fail(contracts.ResourceLimit, "compiler relation limit exceeded", false)
	}
	columnsByID := map[uint32][]sqlpolicy.Column{}
	for _, n := range nodes {
		if !n.valid || strings.HasPrefix(n.schema, "pg_") || n.schema == "information_schema" || n.rls && (len(nodes) > 1 || n.kind == "p" || n.parent) || kind != "p" && !c.scope.ContainsName(n.schema, n.name) {
			return out, unsupportedRelation()
		}
		cols, err := c.columns(ctx, n.id)
		if err != nil {
			return out, err
		}
		columnsByID[n.id] = cols
		out.Dependencies = append(out.Dependencies, sqlpolicy.Identity{OID: n.id, Schema: n.schema, Name: n.name})
	}
	out.Columns = columnsByID[out.OID]
	for _, n := range nodes {
		if n.kind == "p" {
			continue
		}
		cols := columnsByID[n.id]
		for _, want := range out.Columns {
			found := false
			for _, col := range cols {
				if col == want {
					found = true
				}
			}
			if !found {
				return out, unsupportedRelation()
			}
		}
		out.Scans = append(out.Scans, sqlpolicy.Identity{OID: n.id, Schema: n.schema, Name: n.name})
	}
	// Capture structural facts separately from semantic built-in signatures.
	// Include child column layouts and exact edges, not just the closure's OIDs.
	ids := make([]uint32, 0, len(out.Dependencies))
	for _, dep := range out.Dependencies {
		ids = append(ids, dep.OID)
	}
	var state string
	err = c.tx.QueryRow(ctx, `SELECT pg_catalog.json_agg(s ORDER BY s.oid)::text FROM (
 SELECT r.oid,r.relkind,r.relrowsecurity,r.relforcerowsecurity,r.relam,r.relpersistence,
 (SELECT pg_catalog.json_agg(a ORDER BY a.attnum) FROM (
 SELECT attnum,attname,atttypid,atttypmod,attcollation,attnotnull,attisdropped,attgenerated
 FROM pg_catalog.pg_attribute WHERE attrelid=r.oid AND attnum>0) a) AS columns,
 (SELECT pg_catalog.json_agg(i ORDER BY i.inhparent,i.inhseqno) FROM (
 SELECT inhparent,inhseqno,inhdetachpending FROM pg_catalog.pg_inherits WHERE inhrelid=r.oid) i) AS parents
 FROM pg_catalog.pg_class r WHERE r.oid=ANY($1::oid[])) s`, ids).Scan(&state)
	if err != nil {
		return out, err
	}
	digest := sha256.Sum256([]byte(state))
	out.State = hex.EncodeToString(digest[:])
	return out, nil
}
func (c *compilerCatalog) columns(ctx context.Context, oid uint32) ([]sqlpolicy.Column, error) {
	// Only built-in scalar types and default/C/POSIX collations can reach the
	// emitter. The semantic manifest verifies their I/O and comparison functions.
	rows, err := c.tx.Query(ctx, `SELECT a.attname,a.atttypid,t.typnamespace=11 AND t.typtype='b' AND a.attgenerated='' AND a.attcollation IN (0,100,950,951)
 FROM pg_catalog.pg_attribute a JOIN pg_catalog.pg_type t ON t.oid=a.atttypid WHERE a.attrelid=$1 AND a.attnum>0 AND NOT a.attisdropped ORDER BY a.attnum LIMIT 1601`, oid)
	if err != nil {
		return nil, err
	}
	cols := []sqlpolicy.Column{}
	for rows.Next() {
		var col sqlpolicy.Column
		var tid uint32
		var valid bool
		if err = rows.Scan(&col.Name, &tid, &valid); err != nil {
			rows.Close()
			return nil, err
		}
		col.Type = sqlpolicy.Type(tid)
		if !valid || col.Type.Name() == "" {
			rows.Close()
			return nil, unsupportedRelation()
		}
		c.columnCount++
		if c.columnCount > 8192 {
			rows.Close()
			return nil, database.Fail(contracts.ResourceLimit, "compiler column limit exceeded", false)
		}
		cols = append(cols, col)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(cols) > 1600 {
		return nil, unsupportedRelation()
	}
	var safe bool
	err = c.tx.QueryRow(ctx, `SELECT NOT EXISTS(SELECT FROM pg_catalog.pg_index i JOIN pg_catalog.pg_class idx ON idx.oid=i.indexrelid WHERE i.indrelid=$1 AND
 (idx.relam<>403 OR i.indexprs IS NOT NULL OR i.indpred IS NOT NULL OR
 EXISTS(SELECT FROM pg_catalog.unnest(i.indclass::oid[]) cls LEFT JOIN pg_catalog.pg_opclass op ON op.oid=cls WHERE op.oid IS NULL OR op.opcnamespace<>11 OR op.opcmethod<>403 OR op.opcintype NOT IN (16,17,20,21,23,25,114,700,701,1042,1043,1082,1083,1114,1184,1700,2950,3802)) OR
 EXISTS(SELECT FROM pg_catalog.unnest(i.indcollation::oid[]) coll WHERE coll NOT IN (0,100,950,951))))`, oid).Scan(&safe)
	if err != nil {
		return nil, err
	}
	if !safe {
		return nil, unsupportedRelation()
	}
	return cols, nil
}
