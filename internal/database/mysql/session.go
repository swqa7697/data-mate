package mysql

import (
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"strconv"
	"time"
)

// errMalformed reports a library failure on unexpected server data.
var errMalformed = errors.New("malformed server response")

// guarded runs one client-library call. A panic on malformed server data
// closes this connection and becomes an error; it never reaches the service.
func guarded[T any](w *wire, f func() (T, error)) (out T, err error) {
	defer func() {
		if r := recover(); r != nil {
			if w != nil {
				_ = w.Close()
			}
			err = errMalformed
		}
	}()
	return f()
}

// server describes a physical connection's server, read once per connection.
type server struct {
	flavor  Flavor
	version int
	id      uint64
	user    string
	host    string
}

// session adapts one go-sql-driver connection to the shared pool.
type session struct {
	conn driver.Conn
	wire *wire
	info *server
}

func (s *session) Reusable() bool {
	if s.wire.closed() {
		return false
	}
	v, ok := s.conn.(driver.Validator)
	return ok && v.IsValid()
}

func (s *session) Closed() <-chan struct{} { return s.wire.done }

// Close sends COM_QUIT under a write deadline, then closes the route.
func (s *session) Close() {
	if !s.wire.closed() {
		s.wire.bound(2 * time.Second)
		_, _ = guarded(s.wire, func() (struct{}, error) { return struct{}{}, s.conn.Close() })
	}
	_ = s.wire.Close()
}

// exec runs one statement without parameters through the text protocol.
func (s *session) exec(ctx context.Context, query string) error {
	_, err := guarded(s.wire, func() (driver.Result, error) {
		return s.conn.(driver.ExecerContext).ExecContext(ctx, query, nil)
	})
	return err
}

// rows is an open result; close it before the next statement.
type rows struct {
	s     *session
	rows  driver.Rows
	stmt  driver.Stmt
	names []string
	dest  []driver.Value
}

// query runs a statement. With arguments it uses a server-side prepared
// statement and the binary protocol; parameters are never interpolated.
func (s *session) query(ctx context.Context, query string, args ...any) (*rows, error) {
	if len(args) == 0 {
		r, err := guarded(s.wire, func() (driver.Rows, error) {
			return s.conn.(driver.QueryerContext).QueryContext(ctx, query, nil)
		})
		if err != nil {
			return nil, err
		}
		return s.wrap(r, nil), nil
	}
	stmt, err := s.prepare(ctx, query)
	if err != nil {
		return nil, err
	}
	if stmt.NumInput() != len(args) {
		_ = s.closeStatement(stmt)
		return nil, errors.New("parameter count mismatch")
	}
	values := make([]driver.NamedValue, len(args))
	for i, a := range args {
		values[i] = driver.NamedValue{Ordinal: i + 1, Value: a}
	}
	r, err := s.execute(ctx, stmt, values)
	if err != nil {
		_ = s.closeStatement(stmt)
		return nil, err
	}
	return s.wrap(r, stmt), nil
}

func (s *session) prepare(ctx context.Context, query string) (driver.Stmt, error) {
	return guarded(s.wire, func() (driver.Stmt, error) {
		return s.conn.(driver.ConnPrepareContext).PrepareContext(ctx, query)
	})
}

func (s *session) execute(ctx context.Context, stmt driver.Stmt, values []driver.NamedValue) (driver.Rows, error) {
	return guarded(s.wire, func() (driver.Rows, error) {
		return stmt.(driver.StmtQueryContext).QueryContext(ctx, values)
	})
}

// closeStatement releases a server-side prepared statement. A statement that
// cannot be closed would remain allocated, so its connection is discarded.
func (s *session) closeStatement(stmt driver.Stmt) error {
	_, err := guarded(s.wire, func() (struct{}, error) { return struct{}{}, stmt.Close() })
	if err != nil {
		_ = s.wire.Close()
	}
	return err
}

func (s *session) wrap(r driver.Rows, stmt driver.Stmt) *rows {
	names := r.Columns()
	return &rows{s: s, rows: r, stmt: stmt, names: names, dest: make([]driver.Value, len(names))}
}

// next reads one row into r.dest; false with nil error means no more rows.
func (r *rows) next() (bool, error) {
	_, err := guarded(r.s.wire, func() (struct{}, error) { return struct{}{}, r.rows.Next(r.dest) })
	if err == io.EOF {
		return false, nil
	}
	return err == nil, err
}

// databaseType returns the library's type name for column i.
func (r *rows) databaseType(i int) string {
	if t, ok := r.rows.(driver.RowsColumnTypeDatabaseTypeName); ok {
		name, _ := guarded(r.s.wire, func() (string, error) { return t.ColumnTypeDatabaseTypeName(i), nil })
		return name
	}
	return ""
}

// close drains nothing on an already-closed socket; callers close the socket
// first when abandoning unread rows, since the library would read them all.
func (r *rows) close() error {
	_, err := guarded(r.s.wire, func() (struct{}, error) { return struct{}{}, r.rows.Close() })
	if r.stmt != nil {
		if e := r.s.closeStatement(r.stmt); err == nil {
			err = e
		}
	}
	return err
}

// all reads every row of a small catalog result, bounded by limit rows.
func (r *rows) all(limit int) ([][]driver.Value, error) {
	var out [][]driver.Value
	for {
		ok, err := r.next()
		if err != nil {
			_ = r.close()
			return nil, err
		}
		if !ok {
			return out, r.close()
		}
		if len(out) == limit {
			_ = r.s.wire.Close()
			_ = r.close()
			return nil, catalogLimit()
		}
		out = append(out, append([]driver.Value(nil), r.dest...))
	}
}

// text converts a catalog value to a string; NULL becomes "" and false.
func text(v driver.Value) (string, bool) {
	switch x := v.(type) {
	case nil:
		return "", false
	case []byte:
		return string(x), true
	case string:
		return x, true
	case int64:
		return strconv.FormatInt(x, 10), true
	case uint64:
		return strconv.FormatUint(x, 10), true
	}
	return "", false
}

// optional converts a nullable catalog value.
func optional(v driver.Value) *string {
	if s, ok := text(v); ok {
		return &s
	}
	return nil
}

func str(v driver.Value) string { s, _ := text(v); return s }
