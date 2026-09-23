package sqlpolicy

import (
	"slices"
	"strings"

	pg "github.com/pganalyze/pg_query_go/v6"
	"google.golang.org/protobuf/reflect/protoreflect"
)

type relationName struct {
	schema, name string
	only         bool
}

// Collect every physical reference with lexical CTE visibility before requesting
// any catalog types. Neither aliases nor CTE references become physical lookups.
func collectRelations(stmt *pg.SelectStmt) ([]relationName, error) {
	found := map[relationName]bool{}
	var walk func(protoreflect.Message, map[string]bool) error
	walk = func(m protoreflect.Message, ctes map[string]bool) error {
		if s, ok := m.Interface().(*pg.SelectStmt); ok {
			local := map[string]bool{}
			for n, v := range ctes {
				local[n] = v
			}
			if w := s.WithClause; w != nil {
				seen := map[string]bool{}
				for _, n := range w.Ctes {
					cte := n.GetCommonTableExpr()
					if cte == nil || seen[cte.Ctename] {
						return unsupported()
					}
					seen[cte.Ctename] = true
					if err := walk(cte.Ctequery.ProtoReflect(), local); err != nil {
						return err
					}
					local[cte.Ctename] = true
				}
			}
			ctes = local
		}
		if r, ok := m.Interface().(*pg.RangeVar); ok {
			if r.Schemaname == "" {
				if !ctes[r.Relname] {
					return unsupported()
				}
				return nil
			}
			found[relationName{r.Schemaname, r.Relname, !r.Inh}] = true
			return nil
		}
		var err error
		m.Range(func(f protoreflect.FieldDescriptor, v protoreflect.Value) bool {
			if f.Name() == "with_clause" {
				return true
			} // Already traversed in lexical order.
			if f.Kind() == protoreflect.MessageKind {
				if f.IsList() {
					for i := 0; i < v.List().Len(); i++ {
						if err = walk(v.List().Get(i).Message(), ctes); err != nil {
							return false
						}
					}
				} else {
					err = walk(v.Message(), ctes)
				}
			}
			return err == nil
		})
		return err
	}
	if err := walk(stmt.ProtoReflect(), map[string]bool{}); err != nil {
		return nil, err
	}
	out := make([]relationName, 0, len(found))
	for r := range found {
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b relationName) int {
		if n := strings.Compare(a.schema, b.schema); n != 0 {
			return n
		}
		if n := strings.Compare(a.name, b.name); n != 0 {
			return n
		}
		if a.only == b.only {
			return 0
		}
		if a.only {
			return -1
		}
		return 1
	})
	return out, nil
}
