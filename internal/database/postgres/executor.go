package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
	"github.com/swqa7697/data-mate/internal/database/postgres/sqlguard"
)

// Only the executor can signal successful truncation after terminating a socket.
var errResultDiscarded = errors.New("bounded result completed; connection discarded")

// Query executes one scoped read query in a fresh read-only transaction.
func (d *Driver) Query(ctx context.Context, a database.Access, req database.QueryRequest) (database.QueryResult, error) {
	started := time.Now()
	a, rev, err := normalized(a)
	if err != nil {
		return database.QueryResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(a.Profile.Limits.QueryTimeoutMS)*time.Millisecond)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return database.QueryResult{}, safeError(err)
	}
	limit := a.Profile.Limits.MaxRows
	if req.RowLimit < 0 || req.RowLimit > limit {
		return database.QueryResult{}, database.Fail(contracts.InvalidArgument, "row limit exceeds profile bounds", false)
	}
	if req.RowLimit != 0 {
		limit = req.RowLimit
	}
	params, err := queryParameters(req.Parameters)
	if err != nil {
		return database.QueryResult{}, err
	}
	names, err := sqlguard.Inspect(req.SQL, a.Profile.Scope)
	if err != nil {
		return database.QueryResult{}, err
	}
	var out database.QueryResult
	err = d.runNormalized(ctx, a, rev, nil, func(ctx context.Context, tx pgx.Tx, version int) error {
		if e := checkRoutineCalls(ctx, tx, names); e != nil {
			return e
		}
		description, e := tx.Conn().PgConn().Prepare(ctx, "data_mate_query", req.SQL, nil)
		if e != nil {
			return e
		}
		if len(description.ParamOIDs) != len(params) {
			return invalidParameters()
		}
		types, e := describeTypes(ctx, tx, description.Fields)
		if e != nil {
			return e
		}
		out, err = executeQuery(ctx, tx, a, description, types, params, limit, started)
		return err
	})
	if err != nil {
		return database.QueryResult{}, err
	}
	return out, nil
}

// Authorize names, not guessed overloads. A non-core overload in pg_catalog
// makes the entire name unavailable. Core type-conversion syntax is allowed,
// but must not mask a non-core routine. The server and administrator are trusted.
func checkRoutineCalls(ctx context.Context, tx pgx.Tx, names []string) error {
	if len(names) == 0 {
		return nil
	}
	var allowed bool
	err := tx.QueryRow(ctx, `SELECT COALESCE(bool_and(
 (EXISTS (SELECT 1 FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace
   WHERE n.nspname='pg_catalog' AND p.proname=x.name AND p.oid<16384)
  OR EXISTS (SELECT 1 FROM pg_catalog.pg_type t JOIN pg_catalog.pg_namespace n ON n.oid=t.typnamespace
   WHERE n.nspname='pg_catalog' AND t.typname=x.name AND t.oid<16384
   AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_depend d WHERE d.classid='pg_catalog.pg_type'::regclass AND d.objid=t.oid AND d.deptype='e')))
 AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace
   WHERE n.nspname='pg_catalog' AND p.proname=x.name AND (p.oid>=16384 OR EXISTS
    (SELECT 1 FROM pg_catalog.pg_depend d WHERE d.classid='pg_catalog.pg_proc'::regclass AND d.objid=p.oid AND d.deptype='e')))
 ),false) FROM pg_catalog.unnest($1::text[]) x(name)`, names).Scan(&allowed)
	if err != nil {
		return err
	}
	if !allowed {
		return database.Fail(contracts.ReadOnlyViolation, "explicit calls require core PostgreSQL routines", false)
	}
	return nil
}

// QueryPayloadSize counts the complete tool result, including the duplicated
// compact JSON text. P9 must use this same envelope, then separately bound its
// JSON-RPC frame. Values are never converted through floating point here.
func QueryPayloadSize(result database.QueryResult) (int, error) {
	b, err := json.Marshal(result)
	if err != nil {
		return 0, err
	}
	return resultBytes(b), nil
}

func resultBytes(b []byte) int {
	quoted, _ := json.Marshal(string(b))
	return len(`{"content":[{"type":"text","text":`) + len(quoted) + len(`}],"structuredContent":`) + len(b) + len(`}`)
}

func executeQuery(ctx context.Context, tx pgx.Tx, a database.Access, description *pgconn.StatementDescription, types resultTypes, values [][]byte, rowLimit int, started time.Time) (out database.QueryResult, err error) {
	out = database.QueryResult{Connection: a.Profile.Alias, Columns: []database.ResultColumn{}, Rows: [][]any{}}
	for _, f := range description.Fields {
		if !validResultName(f.Name) {
			return out, codecError()
		}
		t := types[f.DataTypeOID]
		encoding, e := types.representation(f.DataTypeOID)
		if e != nil {
			return out, e
		}
		out.Columns = append(out.Columns, database.ResultColumn{Name: f.Name, Type: t.name, Encoding: encoding})
	}
	// Reserve maximum field widths; each appended row is counted exactly once.
	// false is one byte longer than true. RowCount can never exceed the profile.
	out.RowCount = rowLimit
	out.ElapsedMS = 1<<63 - 1
	header, _ := json.Marshal(out)
	used := resultBytes(header)
	if used > a.Profile.Limits.MaxResultBytes {
		return out, database.Fail(contracts.ResourceLimit, "column metadata exceeds result budget", false)
	}
	out.RowCount = 0
	out.ElapsedMS = 0
	rr := tx.Conn().PgConn().ExecPrepared(ctx, description.Name, values, nil, []int16{0})
	// On every early exit, close the socket before closing the reader. Reader.Close
	// alone drains unread results and could run arbitrarily much remaining work.
	concluded := false
	defer func() {
		if !concluded {
			closeConn(tx.Conn())
		}
		_, closeErr := rr.Close()
		if err == nil && closeErr != nil {
			err = closeErr
		}
	}()
	fields := rr.FieldDescriptions()
	if len(fields) != len(description.Fields) {
		if len(fields) > 0 {
			return out, codecError()
		}
		// Preserve timeout/wire/server errors when no row description arrived.
		_, e := rr.Close()
		concluded = true
		if e != nil {
			return out, e
		}
		return out, codecError()
	}
	for i, f := range fields {
		if f.DataTypeOID != description.Fields[i].DataTypeOID || f.Name != description.Fields[i].Name || f.Format != 0 {
			return out, codecError()
		}
	}
	for rr.NextRow() {
		if err = ctx.Err(); err != nil {
			return out, err
		}
		if len(out.Rows) == rowLimit {
			out.Truncated = true
			out.RowCount = len(out.Rows)
			out.ElapsedMS = time.Since(started).Milliseconds()
			closeConn(tx.Conn())
			return out, errResultDiscarded
		}
		raw := rr.Values()
		if len(raw) != len(fields) {
			return out, codecError()
		}
		row := make([]any, len(raw))
		for i, b := range raw {
			row[i], err = types.decode(fields[i].DataTypeOID, b, 0)
			if err != nil {
				return out, err
			}
		}
		b, e := json.Marshal(row)
		if e != nil {
			return out, codecError()
		}
		// Relative to empty [], a row contributes its JSON and its JSON-string
		// escaped form (without the latter's two quotes), plus two commas after row 1.
		quoted, _ := json.Marshal(string(b))
		cost := len(b) + len(quoted) - 2
		if len(out.Rows) > 0 {
			cost += 2
		}
		if used+cost > a.Profile.Limits.MaxResultBytes {
			out.Truncated = true
			out.RowCount = len(out.Rows)
			out.ElapsedMS = time.Since(started).Milliseconds()
			closeConn(tx.Conn())
			return out, errResultDiscarded
		}
		used += cost
		out.Rows = append(out.Rows, row)
	}
	_, err = rr.Close()
	concluded = true
	out.RowCount = len(out.Rows)
	out.ElapsedMS = time.Since(started).Milliseconds()
	return out, err
}
