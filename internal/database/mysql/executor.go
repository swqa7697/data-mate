package mysql

import (
	"context"
	"database/sql/driver"
	"errors"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
	"github.com/swqa7697/data-mate/internal/database/mysql/sqlguard"
)

// Query executes one guarded read statement in a fresh read-only transaction.
func (d *Driver) Query(ctx context.Context, a database.Access, req database.QueryRequest) (database.QueryResult, error) {
	started := time.Now()
	a, rev, err := database.Normalize(a)
	if err != nil {
		return database.QueryResult{}, err
	}
	if err = d.checkProfile(a.Profile); err != nil {
		return database.QueryResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, database.Timeout(a))
	defer cancel()
	if err := ctx.Err(); err != nil {
		return database.QueryResult{}, database.CommonError(err)
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
	statement, err := sqlguard.Check(req.SQL)
	if err != nil {
		return database.QueryResult{}, err
	}
	values := bindParameters(params)
	var out database.QueryResult
	err = d.runNormalized(ctx, a, rev, func(ctx context.Context, s *session) error {
		var e error
		out, e = d.execute(ctx, s, a, statement, values, limit, started)
		return e
	})
	if err != nil {
		return database.QueryResult{}, err
	}
	return out, nil
}

// execute prepares the statement, checks the server's parameter count and
// streams complete rows within the row and byte budgets.
func (d *Driver) execute(ctx context.Context, s *session, a database.Access, statement string, values []any, rowLimit int, started time.Time) (database.QueryResult, error) {
	var r *rows
	stmt, err := s.prepare(ctx, statement)
	var my *mysql.MySQLError
	switch {
	case err == nil:
		if stmt.NumInput() != len(values) {
			_ = s.closeStatement(stmt)
			return database.QueryResult{}, database.InvalidParameters()
		}
		named := make([]driver.NamedValue, len(values))
		for i, v := range values {
			named[i] = driver.NamedValue{Ordinal: i + 1, Value: v}
		}
		res, e := s.execute(ctx, stmt, named)
		if e != nil {
			_ = s.closeStatement(stmt)
			return database.QueryResult{}, e
		}
		r = s.wrap(res, stmt)
	case errors.As(err, &my) && my.Number == 1295 && len(values) == 0:
		// Some SHOW and EXPLAIN forms cannot be prepared; without parameters they
		// run once through the text protocol, still guarded and read-only.
		if r, err = s.query(ctx, statement); err != nil {
			return database.QueryResult{}, err
		}
	default:
		return database.QueryResult{}, err
	}
	concluded := false
	defer func() {
		if !concluded {
			// Closing the socket first prevents draining unread rows.
			_ = s.wire.Close()
		}
		_ = r.close()
	}()
	codecs := make([]codec, len(r.names))
	columns := make([]database.ResultColumn, len(r.names))
	for i, name := range r.names {
		if !database.ValidResultName(name) {
			return database.QueryResult{}, database.UnsupportedValue()
		}
		codecs[i] = codecFor(r.databaseType(i))
		columns[i] = database.ResultColumn{Name: name, Type: codecs[i].name, Encoding: codecs[i].encoding}
	}
	result, err := database.NewResult(a, columns, rowLimit, started)
	if err != nil {
		return database.QueryResult{}, err
	}
	for {
		if err = ctx.Err(); err != nil {
			return database.QueryResult{}, err
		}
		more, err := r.next()
		if err != nil {
			return database.QueryResult{}, err
		}
		if !more {
			break
		}
		// One extra row detects truncation without decoding it.
		if !result.Next() {
			_ = s.wire.Close()
			return result.Result(), errResultDiscarded
		}
		row := make([]any, len(r.dest))
		for i, v := range r.dest {
			if row[i], err = codecs[i].decode(v); err != nil {
				return database.QueryResult{}, err
			}
		}
		added, err := result.Add(row)
		if err != nil {
			return database.QueryResult{}, err
		}
		if !added {
			_ = s.wire.Close()
			return result.Result(), errResultDiscarded
		}
	}
	concluded = true
	return result.Result(), nil
}
