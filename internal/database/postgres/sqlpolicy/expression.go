package sqlpolicy

import (
	"strconv"
	"strings"

	pg "github.com/pganalyze/pg_query_go/v6"
)

func (c *compiler) concrete(e expression, want Type) (expression, error) {
	if e.typ == 0 {
		if want == 0 {
			return e, unsupported()
		}
		e.typ = want
	}
	if want != 0 && e.typ != want {
		// Only the documented integer-to-numeric widening chain is implicit.
		rank := map[Type]int{21: 1, 23: 2, 20: 3, 1700: 4}
		if rank[e.typ] == 0 || rank[want] <= rank[e.typ] || !c.signatures.canCast(e.typ, want) {
			return e, unsupported()
		}
		e, err := c.concrete(e, 0)
		if err != nil {
			return e, err
		}
		e.sql = "(" + e.sql + ")::" + typeSQL(want)
		e.typ = want
		return e, nil
	}
	if e.literal != nil {
		p := *e.literal
		p.Type = e.typ
		if len(c.params) >= 8192 {
			return e, resource()
		}
		c.params = append(c.params, p)
		e.sql = paramSQL(len(c.params), e.typ)
		e.literal = nil
	}
	return e, nil
}
func (c *compiler) pair(a, b expression) (expression, expression, error) {
	want := a.typ
	if want == 0 {
		want = b.typ
	}
	if a.typ != 0 && b.typ != 0 && a.typ != b.typ {
		rank := map[Type]int{21: 1, 23: 2, 20: 3, 1700: 4}
		if rank[a.typ] == 0 || rank[b.typ] == 0 {
			return a, b, unsupported()
		}
		if rank[b.typ] > rank[a.typ] {
			want = b.typ
		}
	}
	var err error
	a, err = c.concrete(a, want)
	if err != nil {
		return a, b, err
	}
	b, err = c.concrete(b, want)
	return a, b, err
}
func (c *compiler) binary(op string, a, b expression) (expression, error) {
	if !strings.Contains(" = <> < <= > >= + - * / ", " "+op+" ") {
		return expression{}, unsupported()
	}
	a, b, err := c.pair(a, b)
	if err != nil {
		return expression{}, err
	}
	if strings.Contains(" + - * / ", " "+op+" ") && !numeric(a.typ) {
		return expression{}, unsupported()
	}
	t, err := c.signatures.op(op, a.typ, b.typ)
	if err != nil {
		return expression{}, err
	}
	return expression{sql: "(" + a.sql + " OPERATOR(pg_catalog." + op + ") " + b.sql + ")", typ: t, aggregate: a.aggregate || b.aggregate, refs: append(append([]string{}, a.refs...), b.refs...)}, nil
}
func (c *compiler) expr(n *pg.Node, env scope, aggregates bool) (out expression, result error) {
	defer func() {
		c.emittedBytes += len(out.sql)
		if c.emittedBytes > 1<<20 {
			out = expression{}
			result = resource()
		}
	}()
	if n == nil {
		return expression{}, unsupported()
	}
	if aggregates && len(env.groups) > 0 {
		if e, ok := env.groups[expressionKey(n)]; ok {
			e.refs = []string{e.sql}
			return e, nil
		}
	}
	if a := n.GetAConst(); a != nil {
		e := expression{literal: &Parameter{}}
		if a.Isnull {
			return e, nil
		}
		var value string
		switch v := a.Val.(type) {
		case *pg.A_Const_Ival:
			e.typ = 23
			value = strconv.Itoa(int(v.Ival.Ival))
		case *pg.A_Const_Fval:
			value = v.Fval.Fval
			e.typ = 1700
			if _, err := strconv.ParseInt(value, 10, 64); err == nil {
				e.typ = 20
			}
		case *pg.A_Const_Sval:
			value = v.Sval.Sval
		case *pg.A_Const_Boolval:
			e.typ = 16
			value = strconv.FormatBool(v.Boolval.Boolval)
		default:
			return expression{}, unsupported()
		}
		e.literal.Value = &value
		return e, nil
	}
	if p := n.GetParamRef(); p != nil {
		idx := int(p.Number) - 1
		if idx < 0 || idx >= len(c.supplied) {
			return expression{}, unsupported()
		}
		c.used[idx] = true
		v := c.supplied[idx]
		return expression{typ: v.Type, literal: &v}, nil
	}
	if r := n.GetColumnRef(); r != nil {
		ns, err := names(r.Fields)
		if err != nil || len(ns) < 1 || len(ns) > 3 {
			return expression{}, unsupported()
		}
		found := []expression{}
		for _, b := range env.bindings {
			if len(ns) == 2 && ns[0] != b.alias {
				continue
			}
			if len(ns) == 3 && (ns[0] != b.schema || ns[1] != b.name || b.alias != b.name || b.explicitAlias) {
				continue
			}
			for i, col := range b.columns {
				if col.Name == ns[len(ns)-1] {
					sql := boundColumn(b, i)
					found = append(found, expression{sql: sql, typ: col.Type, label: col.Name, strongLabel: true, refs: []string{sql}})
				}
			}
		}
		if len(found) != 1 {
			return expression{}, unsupported()
		}
		return found[0], nil
	}
	if t := n.GetTypeCast(); t != nil {
		name, err := identifier(t.TypeName.Names)
		to := NamedType(name)
		if err != nil || to == 0 || t.TypeName.Typemod != -1 {
			return expression{}, unsupported()
		}
		a, err := c.expr(t.Arg, env, aggregates)
		if err != nil {
			return a, err
		}
		if a.typ == 0 {
			a, err = c.concrete(a, to)
			a.label = name
			return a, err
		}
		a, err = c.concrete(a, 0)
		if err != nil || !c.signatures.canCast(a.typ, to) {
			return expression{}, unsupported()
		}
		a.sql = "(" + a.sql + ")::" + typeSQL(to)
		a.typ = to
		if !a.strongLabel {
			a.label = name
		}
		return a, nil
	}
	if b := n.GetBoolExpr(); b != nil {
		op := ""
		switch b.Boolop {
		case pg.BoolExprType_AND_EXPR:
			op = " AND "
		case pg.BoolExprType_OR_EXPR:
			op = " OR "
		case pg.BoolExprType_NOT_EXPR:
			op = "NOT "
		default:
			return expression{}, unsupported()
		}
		if len(b.Args) < 1 || op == "NOT " && len(b.Args) != 1 {
			return expression{}, unsupported()
		}
		e := expression{typ: 16}
		parts := []string{}
		for _, arg := range b.Args {
			a, err := c.expr(arg, env, aggregates)
			if err != nil {
				return e, err
			}
			a, err = c.concrete(a, 16)
			if err != nil {
				return e, err
			}
			parts = append(parts, a.sql)
			e.aggregate = e.aggregate || a.aggregate
			e.refs = append(e.refs, a.refs...)
		}
		if op == "NOT " {
			e.sql = "(NOT " + parts[0] + ")"
		} else {
			e.sql = "(" + strings.Join(parts, op) + ")"
		}
		return e, nil
	}
	if v := n.GetNullTest(); v != nil {
		a, err := c.expr(v.Arg, env, aggregates)
		if err != nil {
			return a, err
		}
		a, err = c.concrete(a, 0)
		if err != nil {
			return a, err
		}
		op := " IS NULL"
		if v.Nulltesttype == pg.NullTestType_IS_NOT_NULL {
			op = " IS NOT NULL"
		} else if v.Nulltesttype != pg.NullTestType_IS_NULL {
			return a, unsupported()
		}
		a.sql = "(" + a.sql + op + ")"
		a.typ = 16
		a.label = ""
		a.strongLabel = false
		return a, nil
	}
	if a := n.GetAExpr(); a != nil {
		op, err := identifier(a.Name)
		if err != nil {
			return expression{}, err
		}
		if a.Kind == pg.A_Expr_Kind_AEXPR_OP {
			r, err := c.expr(a.Rexpr, env, aggregates)
			if err != nil {
				return r, err
			}
			if a.Lexpr == nil {
				if op != "-" || !numeric(r.typ) {
					return r, unsupported()
				}
				r, err = c.concrete(r, 0)
				if err != nil {
					return r, err
				}
				t, err := c.signatures.op("-", 0, r.typ)
				r.sql = "(OPERATOR(pg_catalog.-) " + r.sql + ")"
				r.typ = t
				r.label = ""
				r.strongLabel = false
				return r, err
			}
			l, err := c.expr(a.Lexpr, env, aggregates)
			if err != nil {
				return l, err
			}
			return c.binary(op, l, r)
		}
		l, err := c.expr(a.Lexpr, env, aggregates)
		if err != nil {
			return l, err
		}
		list := a.Rexpr.GetList()
		if list == nil || len(list.Items) == 0 {
			return l, unsupported()
		}
		if a.Kind == pg.A_Expr_Kind_AEXPR_IN {
			if op != "=" && op != "<>" {
				return l, unsupported()
			}
			operands := []expression{l}
			for _, n := range list.Items {
				r, err := c.expr(n, env, aggregates)
				if err != nil {
					return expression{}, err
				}
				operands = append(operands, r)
			}
			var common Type
			for _, e := range operands {
				if e.typ == 0 {
					continue
				}
				if common == 0 {
					common = e.typ
					continue
				}
				if common != e.typ {
					rank := map[Type]int{21: 1, 23: 2, 20: 3, 1700: 4}
					if rank[common] == 0 || rank[e.typ] == 0 {
						return expression{}, unsupported()
					}
					if rank[e.typ] > rank[common] {
						common = e.typ
					}
				}
			}
			for i, e := range operands {
				operands[i], err = c.concrete(e, common)
				if err != nil {
					return expression{}, err
				}
			}
			parts := []string{}
			e := expression{typ: 16}
			for _, r := range operands[1:] {
				x, err := c.binary(op, operands[0], r)
				if err != nil {
					return e, err
				}
				c.emittedBytes += len(x.sql)
				if c.emittedBytes > 1<<20 {
					return expression{}, resource()
				}
				parts = append(parts, x.sql)
				e.aggregate = e.aggregate || x.aggregate
				e.refs = append(e.refs, x.refs...)
			}

			join := " OR "
			if op == "<>" {
				join = " AND "
			}
			e.sql = "(" + strings.Join(parts, join) + ")"
			return e, nil
		}
		if a.Kind == pg.A_Expr_Kind_AEXPR_BETWEEN || a.Kind == pg.A_Expr_Kind_AEXPR_NOT_BETWEEN {
			if len(list.Items) != 2 {
				return l, unsupported()
			}
			lo, err := c.expr(list.Items[0], env, aggregates)
			if err != nil {
				return lo, err
			}
			hi, err := c.expr(list.Items[1], env, aggregates)
			if err != nil {
				return hi, err
			}
			x, err := c.binary(">=", l, lo)
			if err != nil {
				return x, err
			}
			y, err := c.binary("<=", l, hi)
			if err != nil {
				return y, err
			}
			e := expression{sql: "(" + x.sql + " AND " + y.sql + ")", typ: 16, aggregate: x.aggregate || y.aggregate, refs: append(x.refs, y.refs...)}
			if a.Kind == pg.A_Expr_Kind_AEXPR_NOT_BETWEEN {
				e.sql = "(NOT " + e.sql + ")"
			}
			return e, nil
		}
		return expression{}, unsupported()
	}
	if f := n.GetFuncCall(); f != nil {
		if !aggregates || f.Funcformat != pg.CoercionForm_COERCE_EXPLICIT_CALL {
			return expression{}, unsupported()
		}
		name, err := identifier(f.Funcname)
		if err != nil || name != "count" && name != "sum" && name != "avg" && name != "min" && name != "max" {
			return expression{}, unsupported()
		}
		arg := "*"
		var typ Type
		if f.AggStar {
			if name != "count" || len(f.Args) != 0 || f.AggDistinct {
				return expression{}, unsupported()
			}
		} else {
			if len(f.Args) != 1 {
				return expression{}, unsupported()
			}
			a, err := c.expr(f.Args[0], env, false)
			if err != nil {
				return a, err
			}
			a, err = c.concrete(a, 0)
			if err != nil {
				return a, err
			}
			typ = a.typ
			arg = a.sql
			if (name == "sum" || name == "avg") && !numeric(typ) || (name == "min" || name == "max" || f.AggDistinct) && !c.signatures.ordered(typ) {
				return expression{}, unsupported()
			}
		}
		result, err := c.signatures.aggregate(name, typ, f.AggStar)
		if err != nil {
			return expression{}, err
		}
		if f.AggDistinct {
			arg = "DISTINCT " + arg
		}
		return expression{sql: "pg_catalog." + quote(name) + "(" + arg + ")", typ: result, label: name, strongLabel: true, aggregate: true}, nil
	}
	if s := n.GetSubLink(); s != nil {
		q, err := c.selectQuery(s.Subselect.GetSelectStmt(), env.ctes)
		if err != nil {
			return expression{}, err
		}
		if s.SubLinkType == pg.SubLinkType_EXISTS_SUBLINK && s.Testexpr == nil {
			return expression{sql: "EXISTS (" + q.sql + ")", typ: 16, label: "exists", strongLabel: true}, nil
		}
		if s.SubLinkType != pg.SubLinkType_ANY_SUBLINK || len(q.columns) != 1 || s.Testexpr == nil {
			return expression{}, unsupported()
		}
		a, err := c.expr(s.Testexpr, env, aggregates)
		if err != nil {
			return a, err
		}
		a, err = c.concrete(a, q.columns[0].Type)
		if err != nil {
			return a, err
		}
		if _, err = c.signatures.op("=", a.typ, a.typ); err != nil {
			return a, err
		}
		a.sql = "(" + a.sql + " OPERATOR(pg_catalog.=) ANY (" + q.sql + "))"
		a.typ = 16
		a.label = ""
		a.strongLabel = false
		return a, nil
	}
	return expression{}, unsupported()
}
