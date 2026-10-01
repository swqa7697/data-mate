package postgres

import (
	"context"
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

// Query executes one read query in a fresh read-only transaction.
func (d *Driver) Query(ctx context.Context, a database.Access, req database.QueryRequest) (database.QueryResult, error) {
	started := time.Now()
	a, rev, err := database.Normalize(a)
	if err != nil {
		return database.QueryResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, database.Timeout(a))
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
	params, err := database.Parameters(req.Parameters)
	if err != nil {
		return database.QueryResult{}, err
	}
	if err := sqlguard.Check(req.SQL); err != nil {
		return database.QueryResult{}, err
	}
	var out database.QueryResult
	err = d.runNormalized(ctx, a, rev, func(ctx context.Context, tx pgx.Tx, version int) error {
		description, e := tx.Conn().PgConn().Prepare(ctx, "data_mate_query", req.SQL, nil)
		if e != nil {
			return e
		}
		if len(description.ParamOIDs) != len(params) {
			return database.InvalidParameters()
		}
		types, e := describeTypes(ctx, tx, description.Fields)
		if e != nil {
			return e
		}
		out, err = executeQuery(ctx, tx, a, description, types, textParameters(params), limit, started)
		return err
	})
	if err != nil {
		return database.QueryResult{}, err
	}
	return out, nil
}

func executeQuery(ctx context.Context, tx pgx.Tx, a database.Access, description *pgconn.StatementDescription, types resultTypes, values [][]byte, rowLimit int, started time.Time) (out database.QueryResult, err error) {
	columns := []database.ResultColumn{}
	for _, f := range description.Fields {
		if !database.ValidResultName(f.Name) {
			return out, database.UnsupportedValue()
		}
		t := types[f.DataTypeOID]
		encoding, e := types.representation(f.DataTypeOID)
		if e != nil {
			return out, e
		}
		columns = append(columns, database.ResultColumn{Name: f.Name, Type: t.name, Encoding: encoding})
	}
	result, err := database.NewResult(a, columns, rowLimit, started)
	if err != nil {
		return out, err
	}
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
			return out, database.UnsupportedValue()
		}
		// Preserve timeout/wire/server errors when no row description arrived.
		_, e := rr.Close()
		concluded = true
		if e != nil {
			return out, e
		}
		return out, database.UnsupportedValue()
	}
	for i, f := range fields {
		if f.DataTypeOID != description.Fields[i].DataTypeOID || f.Name != description.Fields[i].Name || f.Format != 0 {
			return out, database.UnsupportedValue()
		}
	}
	for rr.NextRow() {
		if err = ctx.Err(); err != nil {
			return out, err
		}
		if !result.Next() {
			closeConn(tx.Conn())
			return result.Result(), errResultDiscarded
		}
		raw := rr.Values()
		if len(raw) != len(fields) {
			return out, database.UnsupportedValue()
		}
		row := make([]any, len(raw))
		for i, b := range raw {
			row[i], err = types.decode(fields[i].DataTypeOID, b, 0)
			if err != nil {
				return out, err
			}
		}
		added, e := result.Add(row)
		if e != nil {
			return out, e
		}
		if !added {
			closeConn(tx.Conn())
			return result.Result(), errResultDiscarded
		}
	}
	_, err = rr.Close()
	concluded = true
	return result.Result(), err
}
