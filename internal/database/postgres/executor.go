package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
	"github.com/swqa7697/data-mate/internal/database/postgres/sqlpolicy"
)

// Only the executor can signal successful truncation after terminating a socket.
var errResultDiscarded = errors.New("bounded result completed; connection discarded")

// Query compiles and executes one bounded SELECT in a fresh read-only transaction.
// Authorization is request-local; neither compiled plans nor successful checks
// can be supplied by callers or reused to bypass the lock/recheck boundary.
func (d *Driver) Query(ctx context.Context, a database.Access, req database.QueryRequest) (database.QueryResult, error) {
	started := time.Now()
	a, _, err := normalized(a)
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
	parsed, err := sqlpolicy.Parse(req.SQL)
	if err != nil {
		return database.QueryResult{}, err
	}
	var out database.QueryResult
	err = d.run(ctx, a, func(ctx context.Context, tx pgx.Tx, version int) error {
		if err := verifyCatalog(ctx, tx, version); err != nil {
			return err
		}
		plan, err := parsed.CompileBounded(ctx, version/10000, &compilerCatalog{tx: tx, scope: a.Profile.Scope}, params, limit+1)
		if err != nil {
			return err
		}
		if err = recheckPlan(ctx, tx, a, version, plan); err != nil {
			return err
		}
		out, err = executePlan(ctx, tx, a, plan, limit, started)
		return err
	})
	if err != nil {
		return database.QueryResult{}, err
	}
	return out, nil
}

func stalePlan() error {
	return database.Fail(contracts.QueryUnsupported, "relation changed during authorization; retry the query", true)
}

func recheckPlan(ctx context.Context, tx pgx.Tx, a database.Access, version int, plan sqlpolicy.Compiled) error {
	unique := map[uint32]sqlpolicy.Identity{}
	for _, rel := range plan.Relations {
		for _, dep := range rel.Dependencies {
			if prior, ok := unique[dep.OID]; ok && prior != dep {
				return stalePlan()
			}
			unique[dep.OID] = dep
		}
	}
	ids := make([]uint32, 0, len(unique))
	for oid := range unique {
		ids = append(ids, oid)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, oid := range ids {
		dep := unique[oid]
		// ONLY prevents PostgreSQL from implicitly locking newly attached children.
		if _, err := tx.Exec(ctx, "LOCK TABLE ONLY "+pgx.Identifier{dep.Schema, dep.Name}.Sanitize()+" IN ACCESS SHARE MODE"); err != nil {
			return err
		}
	}
	// READ COMMITTED gives every read a fresh snapshot after any lock wait.
	if _, err := checkRole(ctx, tx, a.Profile.Connection.Username); err != nil {
		return err
	}
	catalog := &compilerCatalog{tx: tx, scope: a.Profile.Scope}
	for _, want := range plan.Relations {
		got, err := catalog.Resolve(ctx, want.Schema, want.Name, want.Only)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(got, want) {
			return stalePlan()
		}
	}
	// Mandatory even for relation-free SELECTs. No prepare/execute precedes this.
	return verifyCatalog(ctx, tx, version)
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

func executePlan(ctx context.Context, tx pgx.Tx, a database.Access, plan sqlpolicy.Compiled, rowLimit int, started time.Time) (out database.QueryResult, err error) {
	out = database.QueryResult{Connection: a.Profile.Alias, Columns: []database.ResultColumn{}, Rows: [][]any{}}
	for _, col := range plan.Columns {
		if col.Type.Name() == "" || !validName(col.Name) {
			return out, unsupportedRelation()
		}
		c := database.ResultColumn{Name: col.Name, Type: col.Type.Name()}
		if col.Type == 17 {
			c.Encoding = "base64"
		}
		out.Columns = append(out.Columns, c)
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
	values := make([][]byte, len(plan.Parameters))
	oids := make([]uint32, len(values))
	for i, p := range plan.Parameters {
		oids[i] = uint32(p.Type)
		if p.Value != nil {
			values[i] = []byte(*p.Value)
		}
	}
	rr := tx.Conn().PgConn().ExecParams(ctx, plan.SQL, values, oids, nil, []int16{0})
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
	if len(fields) != len(plan.Columns) {
		if len(fields) > 0 {
			return out, unsupportedRelation()
		}
		// Preserve timeout/wire/server errors when no row description arrived.
		_, e := rr.Close()
		concluded = true
		if e != nil {
			return out, e
		}
		return out, unsupportedRelation()
	}
	for i, f := range fields {
		if f.DataTypeOID != uint32(plan.Columns[i].Type) || f.Name != plan.Columns[i].Name || f.Format != 0 {
			return out, unsupportedRelation()
		}
	}
	for rr.NextRow() {
		if err = ctx.Err(); err != nil {
			return out, err
		}
		if len(out.Rows) == rowLimit {
			out.Truncated = true
			break
		}
		raw := rr.Values()
		if len(raw) != len(fields) {
			return out, unsupportedRelation()
		}
		row := make([]any, len(raw))
		for i, b := range raw {
			row[i], err = decodeValue(plan.Columns[i].Type, b)
			if err != nil {
				return out, err
			}
		}
		b, e := json.Marshal(row)
		if e != nil {
			return out, unsupportedRelation()
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
