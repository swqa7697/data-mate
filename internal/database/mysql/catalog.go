package mysql

import (
	"context"
	"database/sql/driver"
	"math"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
	"github.com/swqa7697/data-mate/internal/database/mysql/sqlguard"
	"github.com/swqa7697/data-mate/internal/database/pool"
)

// Catalog reads use information_schema, which shows the objects the account has
// some privilege on. Binary casts give exact, byte-ordered keyset comparisons;
// plain equality first lets the server use its catalog indexes.
const (
	tableKindSQL = `CASE TABLE_TYPE WHEN 'BASE TABLE' THEN 'table' WHEN 'VIEW' THEN 'view' WHEN 'SYSTEM VIEW' THEN 'system_view' WHEN 'SYSTEM VERSIONED' THEN 'system_versioned_table' ELSE 'other' END`
	relationSQL  = `TABLE_SCHEMA=? AND TABLE_NAME=? AND CAST(TABLE_SCHEMA AS BINARY)=CAST(? AS BINARY) AND CAST(TABLE_NAME AS BINARY)=CAST(? AS BINARY)`
)

// validName accepts a MySQL identifier: at most 64 characters.
func validName(s string) bool {
	return s != "" && utf8.ValidString(s) && utf8.RuneCountInString(s) <= 64 && !strings.ContainsRune(s, 0)
}

func catalogLimit() error {
	return database.Fail(contracts.ResourceLimit, "metadata exceeds catalog or payload limit", false)
}

func relationArgs(t config.Table) []any { return []any{t.Schema, t.Name, t.Schema, t.Name} }

// ListTables returns a keyset page of catalog-visible relations in every
// visible database, including system databases.
func (d *Driver) ListTables(ctx context.Context, a database.Access, req database.PageRequest) (database.TablePage, error) {
	a, rev, err := database.Normalize(a)
	if err != nil {
		return database.TablePage{}, err
	}
	if err = d.checkProfile(a.Profile); err != nil {
		return database.TablePage{}, err
	}
	if req.PageSize == 0 {
		req.PageSize = 100
	}
	if req.PageSize < 1 || req.PageSize == math.MaxInt || (req.Schema != "" && !validName(req.Schema)) {
		return database.TablePage{}, database.Fail(contracts.InvalidArgument, "invalid metadata page request", false)
	}
	pos := pool.Cursor{Version: 1, Tool: "list_tables", ID: a.Profile.ID, Revision: rev, Filter: req.Schema, Epoch: d.pools.Epoch()}
	if req.Cursor != "" {
		if pos, err = d.pools.Decode(req.Cursor, pos, validName); err != nil {
			return database.TablePage{}, err
		}
	}
	out := database.TablePage{Connection: a.Profile.Alias, Driver: a.Profile.Driver, Tables: []database.Table{}}
	err = d.runNormalized(ctx, a, rev, func(ctx context.Context, s *session) error {
		r, err := s.query(ctx, `SELECT TABLE_SCHEMA,TABLE_NAME,`+tableKindSQL+` FROM information_schema.TABLES
 WHERE TABLE_TYPE<>'SEQUENCE' AND (?='' OR CAST(TABLE_SCHEMA AS BINARY)=CAST(? AS BINARY))
 AND (CAST(TABLE_SCHEMA AS BINARY),CAST(TABLE_NAME AS BINARY))>(CAST(? AS BINARY),CAST(? AS BINARY))
 ORDER BY CAST(TABLE_SCHEMA AS BINARY),CAST(TABLE_NAME AS BINARY) LIMIT ?`, req.Schema, req.Schema, pos.Schema, pos.Name, int64(req.PageSize+1))
		if err != nil {
			return err
		}
		budget := database.MetadataBudget{Limit: a.Profile.Limits.MaxResultBytes}
		err = r.each(func(v []driver.Value) error {
			if len(out.Tables) == req.PageSize {
				if out.NextCursor != nil {
					return catalogLimit()
				}
				last := out.Tables[len(out.Tables)-1]
				pos.Schema, pos.Name = last.Schema, last.Name
				next := d.pools.Encode(pos)
				out.NextCursor = &next
				return nil
			}
			item := database.Table{Schema: str(v[0]), Name: str(v[1]), Kind: str(v[2])}
			if err := budget.Add(item); err != nil {
				return err
			}
			out.Tables = append(out.Tables, item)
			return nil
		})
		if err != nil {
			return err
		}
		return database.PayloadBound(out, a)
	})
	if err != nil {
		return database.TablePage{}, err
	}
	return out, nil
}

// DescribeTable reads stored definitions without evaluating them.
func (d *Driver) DescribeTable(ctx context.Context, a database.Access, name config.Table) (Description, error) {
	if !validName(name.Schema) || !validName(name.Name) {
		return Description{}, database.Fail(contracts.InvalidArgument, "invalid relation name", false)
	}
	a, rev, err := database.Normalize(a)
	if err != nil {
		return Description{}, err
	}
	if err = d.checkProfile(a.Profile); err != nil {
		return Description{}, err
	}
	out := Description{RelationCore: database.NewRelationCore(a, name.Schema, name.Name), Columns: []Column{}}
	err = d.runNormalized(ctx, a, rev, func(ctx context.Context, s *session) error {
		r, err := s.query(ctx, `SELECT `+tableKindSQL+`,ENGINE FROM information_schema.TABLES WHERE TABLE_TYPE<>'SEQUENCE' AND `+relationSQL, relationArgs(name)...)
		if err != nil {
			return err
		}
		values, err := r.all(1)
		if err != nil {
			return err
		}
		if len(values) == 0 {
			return database.Fail(contracts.PermissionDenied, "relation is unavailable or inaccessible", false)
		}
		out.Kind, out.Engine = str(values[0][0]), optional(values[0][1])
		budget := database.MetadataBudget{Limit: a.Profile.Limits.MaxResultBytes}
		if err = d.columns(ctx, s, name, &out, &budget); err != nil {
			return err
		}
		if err = d.keys(ctx, s, name, &out, &budget); err != nil {
			return err
		}
		if err = d.checks(ctx, s, name, &out, &budget); err != nil {
			return err
		}
		if err = d.indexes(ctx, s, name, &out, &budget); err != nil {
			return err
		}
		slices.SortFunc(out.Constraints, func(x, y database.Definition) int { return strings.Compare(x.Name, y.Name) })
		if out.Kind == "view" || out.Kind == "system_view" {
			r, err := s.query(ctx, `SELECT VIEW_DEFINITION FROM information_schema.VIEWS WHERE `+relationSQL, relationArgs(name)...)
			if err != nil {
				return err
			}
			values, err := r.all(1)
			if err != nil {
				return err
			}
			if len(values) == 1 {
				if v := optional(values[0][0]); v != nil && *v != "" {
					out.ViewDefinition = v
				}
			}
		}
		return database.PayloadBound(out, a)
	})
	if err != nil {
		return Description{}, err
	}
	return out, nil
}

func (s *session) singleRow(ctx context.Context, query string, args ...any) ([][]driver.Value, error) {
	r, err := s.query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return r.all(1)
}

var numericTypes = []string{"tinyint", "smallint", "mediumint", "int", "bigint", "decimal", "float", "double", "bit", "year"}

// columnDefault reports a default as SQL expression text. MariaDB already
// reports expressions; MySQL reports literal values unquoted.
func (d *Driver) columnDefault(value *string, extra, dataType string) *string {
	if value == nil {
		return nil
	}
	if d.flavor == MariaDB {
		if *value == "NULL" {
			return nil
		}
		return value
	}
	if strings.Contains(extra, "default_generated") || slices.Contains(numericTypes, strings.ToLower(dataType)) {
		return value
	}
	quoted := "'" + strings.NewReplacer(`\`, `\\`, `'`, `''`).Replace(*value) + "'"
	return &quoted
}

func (d *Driver) columns(ctx context.Context, s *session, name config.Table, out *Description, budget *database.MetadataBudget) error {
	r, err := s.query(ctx, `SELECT COLUMN_NAME,COLUMN_TYPE,IS_NULLABLE,COLUMN_DEFAULT,EXTRA,GENERATION_EXPRESSION,DATA_TYPE FROM information_schema.COLUMNS WHERE `+relationSQL+` ORDER BY ORDINAL_POSITION`, relationArgs(name)...)
	if err != nil {
		return err
	}
	return r.each(func(v []driver.Value) error {
		extra := str(v[4])
		lower := strings.ToLower(extra)
		c := Column{Column: database.Column{Name: str(v[0]), Type: str(v[1]), Nullable: str(v[2]) == "YES"}, AutoIncrement: strings.Contains(lower, "auto_increment")}
		if generated := optional(v[5]); generated != nil && *generated != "" {
			c.Generated = generated
		} else {
			c.Default = d.columnDefault(optional(v[3]), lower, str(v[6]))
		}
		if i := strings.Index(lower, "on update "); i >= 0 {
			// The timestamp token may include a precision, followed by
			// space- or comma-separated column attributes.
			update := extra[i+len("on update "):]
			if end := strings.IndexAny(update, " ,\t\r\n"); end >= 0 {
				update = update[:end]
			}
			c.OnUpdate = &update
		}
		if err := budget.Add(c); err != nil {
			return err
		}
		out.Columns = append(out.Columns, c)
		return nil
	})
}

func quoteList(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = sqlguard.Quote(n)
	}
	return "(" + strings.Join(quoted, ",") + ")"
}

// keys reads primary, unique and foreign keys with their column order.
func (d *Driver) keys(ctx context.Context, s *session, name config.Table, out *Description, budget *database.MetadataBudget) error {
	args := relationArgs(name)
	r, err := s.query(ctx, `SELECT tc.CONSTRAINT_NAME,tc.CONSTRAINT_TYPE,k.COLUMN_NAME,k.REFERENCED_TABLE_SCHEMA,k.REFERENCED_TABLE_NAME,k.REFERENCED_COLUMN_NAME,rc.UPDATE_RULE,rc.DELETE_RULE
 FROM information_schema.TABLE_CONSTRAINTS tc
 JOIN information_schema.KEY_COLUMN_USAGE k ON k.CONSTRAINT_SCHEMA=tc.CONSTRAINT_SCHEMA AND k.CONSTRAINT_NAME=tc.CONSTRAINT_NAME
 AND CAST(k.TABLE_SCHEMA AS BINARY)=CAST(tc.TABLE_SCHEMA AS BINARY) AND CAST(k.TABLE_NAME AS BINARY)=CAST(tc.TABLE_NAME AS BINARY)
 LEFT JOIN information_schema.REFERENTIAL_CONSTRAINTS rc ON CAST(rc.CONSTRAINT_SCHEMA AS BINARY)=CAST(tc.CONSTRAINT_SCHEMA AS BINARY) AND rc.CONSTRAINT_NAME=tc.CONSTRAINT_NAME
 AND CAST(rc.TABLE_NAME AS BINARY)=CAST(tc.TABLE_NAME AS BINARY)
 WHERE tc.TABLE_SCHEMA=? AND tc.TABLE_NAME=? AND CAST(tc.TABLE_SCHEMA AS BINARY)=CAST(? AS BINARY) AND CAST(tc.TABLE_NAME AS BINARY)=CAST(? AS BINARY)
 AND tc.CONSTRAINT_TYPE IN ('PRIMARY KEY','UNIQUE','FOREIGN KEY')
 ORDER BY tc.CONSTRAINT_TYPE,CAST(tc.CONSTRAINT_NAME AS BINARY),k.ORDINAL_POSITION`, args...)
	if err != nil {
		return err
	}
	var constraint, kind, update, del string
	var target config.Table
	var columns, targets []string
	finish := func() error {
		if len(columns) == 0 {
			return nil
		}
		var item any
		var def database.Definition
		switch kind {
		case "PRIMARY KEY", "UNIQUE":
			keyKind := "unique"
			if kind == "PRIMARY KEY" {
				keyKind = "primary"
			}
			key := database.Key{Kind: keyKind, Columns: columns}
			out.Keys, item = append(out.Keys, key), key
			def = database.Definition{Name: constraint, Kind: keyKind, Definition: kind + " " + quoteList(columns)}
		default:
			rel := database.Relationship{Columns: columns, Target: target, TargetColumns: targets}
			out.Relationships, item = append(out.Relationships, rel), rel
			clause := "FOREIGN KEY " + quoteList(columns) + " REFERENCES " + sqlguard.Quote(target.Schema) + "." + sqlguard.Quote(target.Name) + " " + quoteList(targets)
			if update != "" {
				clause += " ON UPDATE " + update
			}
			if del != "" {
				clause += " ON DELETE " + del
			}
			def = database.Definition{Name: constraint, Kind: "foreign", Definition: clause}
		}
		if err := budget.Add(item); err != nil {
			return err
		}
		if err := budget.Add(def); err != nil {
			return err
		}
		out.Constraints = append(out.Constraints, def)
		return nil
	}
	// Bound the current constraint as each column arrives. On completion
	// charge its exact output to the cumulative budget.
	partsBudget := *budget
	err = r.each(func(v []driver.Value) error {
		if c, k := str(v[0]), str(v[1]); constraint != c || kind != k {
			if err := finish(); err != nil {
				return err
			}
			partsBudget = *budget
			constraint, kind = c, k
			target = config.Table{Schema: str(v[3]), Name: str(v[4])}
			update, del = str(v[6]), str(v[7])
			columns, targets = nil, nil
		}
		col := str(v[2])
		if err := partsBudget.Add(col); err != nil {
			return err
		}
		columns = append(columns, col)
		if kind == "FOREIGN KEY" {
			ref := str(v[5])
			if err := partsBudget.Add(ref); err != nil {
				return err
			}
			targets = append(targets, ref)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return finish()
}

// checks reads CHECK constraints. MariaDB names them per table, MySQL per schema.
func (d *Driver) checks(ctx context.Context, s *session, name config.Table, out *Description, budget *database.MetadataBudget) error {
	join := ""
	if d.flavor == MariaDB {
		join = " AND CAST(cc.TABLE_NAME AS BINARY)=CAST(tc.TABLE_NAME AS BINARY)"
	}
	r, err := s.query(ctx, `SELECT tc.CONSTRAINT_NAME,cc.CHECK_CLAUSE FROM information_schema.TABLE_CONSTRAINTS tc
 JOIN information_schema.CHECK_CONSTRAINTS cc ON CAST(cc.CONSTRAINT_SCHEMA AS BINARY)=CAST(tc.CONSTRAINT_SCHEMA AS BINARY) AND cc.CONSTRAINT_NAME=tc.CONSTRAINT_NAME`+join+`
 WHERE tc.TABLE_SCHEMA=? AND tc.TABLE_NAME=? AND CAST(tc.TABLE_SCHEMA AS BINARY)=CAST(? AS BINARY) AND CAST(tc.TABLE_NAME AS BINARY)=CAST(? AS BINARY)
 AND tc.CONSTRAINT_TYPE='CHECK' ORDER BY CAST(tc.CONSTRAINT_NAME AS BINARY)`, relationArgs(name)...)
	if err != nil {
		return err
	}
	return r.each(func(v []driver.Value) error {
		def := database.Definition{Name: str(v[0]), Kind: "check", Definition: "CHECK (" + str(v[1]) + ")"}
		if err := budget.Add(def); err != nil {
			return err
		}
		out.Constraints = append(out.Constraints, def)
		return nil
	})
}

// indexes reads index parts in order; MySQL also reports functional parts.
func (d *Driver) indexes(ctx context.Context, s *session, name config.Table, out *Description, budget *database.MetadataBudget) error {
	expression := "EXPRESSION"
	if d.flavor == MariaDB {
		expression = "NULL"
	}
	r, err := s.query(ctx, `SELECT INDEX_NAME,NON_UNIQUE,INDEX_TYPE,COLUMN_NAME,SUB_PART,COLLATION,`+expression+` FROM information_schema.STATISTICS WHERE `+relationSQL+` ORDER BY CAST(INDEX_NAME AS BINARY),SEQ_IN_INDEX`, relationArgs(name)...)
	if err != nil {
		return err
	}
	var index, kind, prefix string
	var parts []string
	finish := func() error {
		if len(parts) == 0 {
			return nil
		}
		clause := prefix + " (" + strings.Join(parts, ",") + ")"
		if kind == "BTREE" || kind == "HASH" {
			clause += " USING " + kind
		}
		def := database.Definition{Name: index, Kind: strings.ToLower(kind), Definition: clause}
		if err := budget.Add(def); err != nil {
			return err
		}
		out.Indexes = append(out.Indexes, def)
		return nil
	}
	partsBudget := *budget
	err = r.each(func(v []driver.Value) error {
		if n := str(v[0]); index != n {
			if err := finish(); err != nil {
				return err
			}
			partsBudget = *budget
			index, kind = n, strings.ToUpper(str(v[2]))
			prefix = "KEY " + sqlguard.Quote(index)
			switch {
			case index == "PRIMARY":
				prefix = "PRIMARY KEY"
			case kind == "FULLTEXT" || kind == "SPATIAL":
				prefix = kind + " KEY " + sqlguard.Quote(index)
			case str(v[1]) == "0":
				prefix = "UNIQUE KEY " + sqlguard.Quote(index)
			}
			parts = nil
		}
		var part string
		if column, ok := text(v[3]); ok {
			part = sqlguard.Quote(column)
			if size, ok := text(v[4]); ok {
				part += "(" + size + ")"
			}
		} else {
			part = "(" + str(v[6]) + ")"
		}
		if str(v[5]) == "D" {
			part += " DESC"
		}
		if err := partsBudget.Add(part); err != nil {
			return err
		}
		parts = append(parts, part)
		return nil
	})
	if err != nil {
		return err
	}
	return finish()
}
