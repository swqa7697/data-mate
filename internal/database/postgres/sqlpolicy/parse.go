// Package sqlpolicy compiles a finite SELECT subset without executing SQL.
package sqlpolicy

import (
	"strings"
	"unicode/utf8"

	pg "github.com/pganalyze/pg_query_go/v6"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func unsupported() error {
	return database.Fail(contracts.QueryUnsupported, "SQL form or type is unsupported; use the documented SELECT subset", false)
}
func resource() error {
	return database.Fail(contracts.ResourceLimit, "SQL complexity exceeds policy limits", false)
}

// Parsed holds an immutable, structurally checked tree. No agent text is retained.
type Parsed struct{ stmt *pg.SelectStmt }

// Parse rejects unsupported fields as well as nodes before any catalog access.
func Parse(sql string) (*Parsed, error) {
	if err := lexicalBudget(sql); err != nil {
		return nil, err
	}
	tree, err := pg.Parse(sql)
	if err != nil {
		return nil, unsupported()
	}
	count := 0
	if err = checkTree(tree.ProtoReflect(), 0, &count); err != nil {
		return nil, err
	}
	if len(tree.Stmts) != 1 || tree.Stmts[0].Stmt.GetSelectStmt() == nil {
		return nil, unsupported()
	}
	return &Parsed{tree.Stmts[0].Stmt.GetSelectStmt()}, nil
}

// Each populated protobuf field must be explicitly understood. In particular,
// flags on otherwise allowed nodes cannot silently bypass the semantic walker.
var fields = map[string]string{
	"ParseResult": "version stmts", "RawStmt": "stmt stmt_location stmt_len",
	"SelectStmt": "target_list from_clause where_clause group_clause having_clause sort_clause limit_offset limit_count limit_option with_clause op",
	"ResTarget":  "name val location", "RangeVar": "schemaname relname inh relpersistence alias location",
	"Alias": "aliasname colnames", "JoinExpr": "jointype larg rarg quals",
	"RangeSubselect": "subquery alias", "WithClause": "ctes location",
	"CommonTableExpr": "ctename aliascolnames ctematerialized ctequery location",
	"ColumnRef":       "fields location", "A_Star": "", "A_Const": "ival fval boolval sval isnull location",
	"Integer": "ival", "Float": "fval", "Boolean": "boolval", "String": "sval",
	"ParamRef": "number location", "A_Expr": "kind name lexpr rexpr location",
	"BoolExpr": "boolop args location", "NullTest": "arg nulltesttype location",
	"FuncCall": "funcname args agg_star agg_distinct funcformat location",
	"TypeCast": "arg type_name location", "TypeName": "names typemod location",
	"SubLink": "sub_link_type testexpr subselect location",
	"SortBy":  "node sortby_dir sortby_nulls location", "List": "items",
}

func checkTree(m protoreflect.Message, depth int, count *int) error {
	*count++
	if depth > 64 || *count > 8192 {
		return resource()
	}
	name := string(m.Descriptor().Name())
	allowed, ok := fields[name]
	if name != "Node" && !ok {
		return unsupported()
	}
	if len(m.GetUnknown()) != 0 {
		return unsupported()
	}
	var err error
	m.Range(func(f protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if name != "Node" && !strings.Contains(" "+allowed+" ", " "+string(f.Name())+" ") {
			err = unsupported()
			return false
		}
		if f.Kind() == protoreflect.MessageKind {
			if f.IsList() {
				l := v.List()
				for i := 0; i < l.Len(); i++ {
					if err = checkTree(l.Get(i).Message(), depth+1, count); err != nil {
						return false
					}
				}
			} else {
				err = checkTree(v.Message(), depth+1, count)
			}
		}
		return err == nil
	})
	return err
}

// This scanner is deliberately independent of the native parser. Quotes and
// nested comments are bounded too; long operator/unary chains consume tokens.
func lexicalBudget(s string) error {
	if len(s) > 64<<10 {
		return resource()
	}
	if !utf8.ValidString(s) || strings.IndexByte(s, 0) >= 0 {
		return unsupported()
	}
	tokens, depth := 0, 0
	for i := 0; i < len(s); {
		c := s[i]
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
			i++
			continue
		}
		tokens++
		if tokens > 8192 {
			return resource()
		}
		if i+1 < len(s) && s[i:i+2] == "--" {
			i += 2
			for i < len(s) && s[i] != '\n' {
				i++
			}
			continue
		}
		if i+1 < len(s) && s[i:i+2] == "/*" {
			i += 2
			n := 1
			for i < len(s) && n > 0 {
				if i+1 < len(s) && s[i:i+2] == "/*" {
					n++
					i += 2
					if n > 64 {
						return resource()
					}
				} else if i+1 < len(s) && s[i:i+2] == "*/" {
					n--
					i += 2
				} else {
					i++
				}
			}
			if n != 0 {
				return unsupported()
			}
			continue
		}
		if c == '\'' || c == '"' {
			q := c
			i++
			closed := false
			for i < len(s) {
				if s[i] == '\\' {
					return unsupported()
				}
				if s[i] == q {
					i++
					if i < len(s) && s[i] == q {
						i++
						continue
					}
					closed = true
					break
				}
				i++
			}
			if !closed {
				return unsupported()
			}
			continue
		}
		if c == '$' {
			j := i + 1
			for j < len(s) && ((s[j] >= 'a' && s[j] <= 'z') || (s[j] >= 'A' && s[j] <= 'Z') || (s[j] >= '0' && s[j] <= '9') || s[j] == '_') {
				j++
			}
			if j < len(s) && s[j] == '$' {
				tag := s[i : j+1]
				end := strings.Index(s[j+1:], tag)
				if end < 0 {
					return unsupported()
				}
				i = j + 1 + end + len(tag)
				continue
			}
		}
		if c == '(' || c == '[' {
			depth++
			if depth > 64 {
				return resource()
			}
		}
		if c == ')' || c == ']' {
			depth--
			if depth < 0 {
				return unsupported()
			}
		}
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_' || c >= 128 || c >= '0' && c <= '9' {
			i++
			for i < len(s) {
				c = s[i]
				if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_' || c >= 128 || c >= '0' && c <= '9') {
					break
				}
				i++
			}
		} else {
			i++
		}
	}
	if depth != 0 {
		return unsupported()
	}
	return nil
}
