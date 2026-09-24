// Package sqlguard checks statement kind and direct relation scope, not SQL semantics.
package sqlguard

import (
	"strings"
	"unicode/utf8"

	pg "github.com/pganalyze/pg_query_go/v6"
	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func invalid() error { return database.Fail(contracts.InvalidArgument, "invalid SQL", false) }
func resource() error {
	return database.Fail(contracts.ResourceLimit, "SQL complexity exceeds limits", false)
}
func readOnly() error {
	return database.Fail(contracts.ReadOnlyViolation, "only one read query is allowed", false)
}
func scopeDenied() error {
	return database.Fail(contracts.ScopeDenied, "direct relation is outside the configured application scope", false)
}

// Check accepts one SELECT-family statement and checks every direct reference.
// Database functions and view definitions are trusted; PostgreSQL owns semantics.
func Check(sql string, scope config.Scope) error {
	if len(sql) > 64<<10 {
		return resource()
	}
	if !utf8.ValidString(sql) || strings.ContainsRune(sql, 0) {
		return invalid()
	}
	// The native scanner understands escaped/dollar strings and comments. Bound
	// tokens and nesting before asking the native parser to build a syntax tree.
	tokens, err := pg.Scan(sql)
	if err != nil {
		return invalid()
	}
	if len(tokens.Tokens) > 8192 {
		return resource()
	}
	depth := 0
	for _, t := range tokens.Tokens {
		switch t.Token {
		case pg.Token_ASCII_40, pg.Token_ASCII_91:
			depth++
			if depth > 64 {
				return resource()
			}
		case pg.Token_ASCII_41, pg.Token_ASCII_93:
			depth--
		}
	}
	tree, err := pg.Parse(sql)
	if err != nil {
		return invalid()
	}
	if len(tree.Stmts) != 1 || tree.Stmts[0].Stmt.GetSelectStmt() == nil {
		return readOnly()
	}
	count := 0
	var walk func(protoreflect.Message, map[string]bool, int) error
	walk = func(m protoreflect.Message, ctes map[string]bool, depth int) error {
		count++
		if count > 8192 || depth > 64 {
			return resource()
		}
		if len(m.GetUnknown()) != 0 {
			return invalid()
		}
		if n, ok := m.Interface().(*pg.Node); ok {
			switch n.Node.(type) {
			case *pg.Node_InsertStmt, *pg.Node_UpdateStmt, *pg.Node_DeleteStmt, *pg.Node_MergeStmt:
				return readOnly()
			}
		}
		if s, ok := m.Interface().(*pg.SelectStmt); ok {
			if s.IntoClause != nil || len(s.LockingClause) > 0 {
				return readOnly()
			}
			local := make(map[string]bool, len(ctes))
			for n, v := range ctes {
				local[n] = v
			}
			if w := s.WithClause; w != nil {
				// Recursive WITH makes every CTE name visible throughout the WITH group.
				if w.Recursive {
					for _, n := range w.Ctes {
						c := n.GetCommonTableExpr()
						if c == nil {
							return invalid()
						}
						local[c.Ctename] = true
					}
				}
				for _, n := range w.Ctes {
					c := n.GetCommonTableExpr()
					if c == nil || c.Ctequery.GetSelectStmt() == nil {
						return readOnly()
					}
					if err := walk(c.ProtoReflect(), local, depth+1); err != nil {
						return err
					}
					local[c.Ctename] = true
				}
			}
			ctes = local
		}
		if r, ok := m.Interface().(*pg.RangeVar); ok {
			if r.Catalogname != "" {
				return scopeDenied()
			}
			if r.Schemaname == "" {
				if ctes[r.Relname] {
					return nil
				}
				return database.Fail(contracts.ScopeDenied, "physical relations must use schema-qualified names", false)
			}
			if strings.HasPrefix(r.Schemaname, "pg_") || r.Schemaname == "information_schema" || !scope.ContainsName(r.Schemaname, r.Relname) {
				return scopeDenied()
			}
			return nil
		}
		var err error
		m.Range(func(f protoreflect.FieldDescriptor, v protoreflect.Value) bool {
			if _, ok := m.Interface().(*pg.SelectStmt); ok && f.Name() == "with_clause" {
				return true
			}
			if f.Kind() == protoreflect.MessageKind {
				if f.IsList() {
					for i := 0; i < v.List().Len(); i++ {
						if err = walk(v.List().Get(i).Message(), ctes, depth+1); err != nil {
							return false
						}
					}
				} else {
					err = walk(v.Message(), ctes, depth+1)
				}
			}
			return err == nil
		})
		return err
	}
	return walk(tree.ProtoReflect(), map[string]bool{}, 0)
}
