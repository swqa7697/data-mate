// Package sqlguard checks read statement kinds and explicit relation schemas.
package sqlguard

import (
	"strings"
	"unicode/utf8"

	pg "github.com/pganalyze/pg_query_go/v6"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func invalid() error { return database.Fail(contracts.InvalidArgument, "invalid SQL", false) }
func readOnly() error {
	return database.Fail(contracts.ReadOnlyViolation, "only one read query is allowed", false)
}

// Check validates one read statement without reproducing PostgreSQL semantics.
func Check(sql string) error {
	if !utf8.ValidString(sql) || strings.ContainsRune(sql, 0) {
		return invalid()
	}
	tree, err := pg.Parse(sql)
	if err != nil {
		return invalid()
	}
	if len(tree.Stmts) != 1 {
		return readOnly()
	}
	statement := tree.Stmts[0].Stmt
	if explain := statement.GetExplainStmt(); explain != nil {
		statement = explain.Query
		if statement.GetSelectStmt() == nil {
			return readOnly()
		}
	}
	if statement.GetSelectStmt() == nil && statement.GetVariableShowStmt() == nil {
		return readOnly()
	}

	// Explicit tasks avoid a Go stack limit. Binding tasks make each CTE visible
	// only after its body, while recursive groups share all names from the start.
	type task struct {
		message protoreflect.Message
		ctes    map[string]bool
		bind    string
	}
	stack := []task{{message: tree.ProtoReflect(), ctes: map[string]bool{}}}
	for len(stack) > 0 {
		next := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if next.bind != "" {
			next.ctes[next.bind] = true
			continue
		}
		m, ctes := next.message, next.ctes
		if len(m.GetUnknown()) != 0 {
			return invalid()
		}
		if n, ok := m.Interface().(*pg.Node); ok {
			switch n.Node.(type) {
			case *pg.Node_InsertStmt, *pg.Node_UpdateStmt, *pg.Node_DeleteStmt, *pg.Node_MergeStmt:
				return readOnly()
			}
		}
		var bindings []*pg.CommonTableExpr
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
					bindings = append(bindings, c)
				}
			}
			ctes = local
		}
		if r, ok := m.Interface().(*pg.RangeVar); ok {
			if r.Schemaname == "" {
				if ctes[r.Relname] {
					continue
				}
				return database.Fail(contracts.QueryUnsupported, "physical relations must use schema-qualified names", false)
			}
			continue
		}
		m.Range(func(f protoreflect.FieldDescriptor, v protoreflect.Value) bool {
			if _, ok := m.Interface().(*pg.SelectStmt); ok && f.Name() == "with_clause" {
				return true
			}
			if f.Kind() == protoreflect.MessageKind {
				if f.IsList() {
					for i := 0; i < v.List().Len(); i++ {
						stack = append(stack, task{message: v.List().Get(i).Message(), ctes: ctes})
					}
				} else {
					stack = append(stack, task{message: v.Message(), ctes: ctes})
				}
			}
			return true
		})
		for i := len(bindings) - 1; i >= 0; i-- {
			c := bindings[i]
			stack = append(stack, task{ctes: ctes, bind: c.Ctename}, task{message: c.ProtoReflect(), ctes: ctes})
		}
	}
	return nil
}
