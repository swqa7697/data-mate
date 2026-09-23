package sqlpolicy

import (
	"context"
	"strconv"
	"strings"

	pg "github.com/pganalyze/pg_query_go/v6"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

type compiler struct {
	ctx          context.Context
	resolved     map[relationName]Relation
	signatures   signatures
	params       []Parameter
	supplied     []Parameter
	used         map[int]bool
	relations    []Relation
	next         int
	emittedBytes int
}
type binding struct {
	explicitAlias                bool
	schema, name, alias, emitted string
	columns                      []Column
}
type scope struct {
	bindings []binding
	ctes     map[string]query
	groups   map[string]expression
}
type query struct {
	sql     string
	columns []Column
}
type expression struct {
	sql         string
	typ         Type
	label       string
	strongLabel bool
	literal     *Parameter
	aggregate   bool
	refs        []string
}

// Compile resolves lexical scopes and types, and emits only validated constructs.
// The Catalog implementation is a trusted driver seam, never an agent input.
func (p *Parsed) Compile(ctx context.Context, major int, catalog Catalog, parameters []Parameter) (Compiled, error) {
	if p == nil || p.stmt == nil || catalog == nil || len(parameters) > 256 {
		return Compiled{}, unsupported()
	}
	sig, err := loadSignatures(major)
	if err != nil {
		return Compiled{}, err
	}
	c := compiler{ctx: ctx, resolved: map[relationName]Relation{}, signatures: sig, used: map[int]bool{}}
	parameterBytes := 0
	for _, p := range parameters {
		if p.Value != nil {
			value := *p.Value
			parameterBytes += len(value)
			p.Value = &value
		}
		if p.Type.Name() == "" || parameterBytes > 256<<10 || p.Value != nil && len(*p.Value) > 64<<10 {
			return Compiled{}, unsupported()
		}
		c.supplied = append(c.supplied, p)
	}
	references, err := collectRelations(p.stmt)
	if err != nil {
		return Compiled{}, err
	}
	for _, ref := range references {
		if err = ctx.Err(); err != nil {
			return Compiled{}, err
		}
		rel, e := catalog.Resolve(ctx, ref.schema, ref.name, ref.only)
		if e != nil {
			return Compiled{}, e
		}
		c.resolved[ref] = rel
		c.relations = append(c.relations, rel)
	}
	q, err := c.selectQuery(p.stmt, map[string]query{})
	if err != nil {
		return Compiled{}, err
	}
	if len(c.used) != len(parameters) {
		return Compiled{}, unsupported()
	}
	if len(q.sql) > 1<<20 || len(c.params) > 8192 {
		return Compiled{}, resource()
	}
	return Compiled{q.sql, c.params, q.columns, c.relations}, nil
}
func (c *compiler) unique() string { c.next++; return "_dm" + strconv.Itoa(c.next) }
func cloneCTEs(m map[string]query) map[string]query {
	n := map[string]query{}
	for k, v := range m {
		n[k] = v
	}
	return n
}
func names(nodes []*pg.Node) ([]string, error) {
	out := []string{}
	for _, n := range nodes {
		v := n.GetString_()
		if v == nil {
			return nil, unsupported()
		}
		out = append(out, v.Sval)
	}
	return out, nil
}
func identifier(nodes []*pg.Node) (string, error) {
	n, e := names(nodes)
	if e != nil || len(n) < 1 || len(n) > 2 || len(n) == 2 && n[0] != "pg_catalog" {
		return "", unsupported()
	}
	return n[len(n)-1], nil
}
func rename(cols []Column, nodes []*pg.Node) ([]Column, error) {
	if len(nodes) == 0 {
		return cols, nil
	}
	ns, err := names(nodes)
	if err != nil || len(ns) > len(cols) {
		return nil, unsupported()
	}
	out := append([]Column(nil), cols...)
	for i, n := range ns {
		out[i].Name = n
	}
	return out, nil
}
func columnList(cols []Column) string {
	a := []string{}
	for _, col := range cols {
		a = append(a, quote(col.Name))
	}
	return strings.Join(a, ",")
}

func (c *compiler) selectQuery(s *pg.SelectStmt, outer map[string]query) (query, error) {
	if err := c.ctx.Err(); err != nil {
		return query{}, err
	}
	if s == nil || s.Op != pg.SetOperation_SETOP_NONE || s.LimitOption != pg.LimitOption_LIMIT_OPTION_DEFAULT && s.LimitOption != pg.LimitOption_LIMIT_OPTION_COUNT {
		return query{}, unsupported()
	}
	env := scope{ctes: cloneCTEs(outer), groups: map[string]expression{}}
	prefix := ""
	if w := s.WithClause; w != nil {
		seen := map[string]bool{}
		parts := []string{}
		for _, n := range w.Ctes {
			cte := n.GetCommonTableExpr()
			if cte == nil || seen[cte.Ctename] || cte.Ctematerialized != pg.CTEMaterialize_CTEMaterializeDefault {
				return query{}, unsupported()
			}
			seen[cte.Ctename] = true
			q, err := c.selectQuery(cte.Ctequery.GetSelectStmt(), env.ctes)
			if err != nil {
				return query{}, err
			}
			q.columns, err = rename(q.columns, cte.Aliascolnames)
			if err != nil {
				return query{}, err
			}
			emitted := c.unique()
			parts = append(parts, quote(emitted)+" ("+columnList(q.columns)+") AS ("+q.sql+")")
			q.sql = quote(emitted)
			env.ctes[cte.Ctename] = q
		}
		prefix = "WITH " + strings.Join(parts, ",") + " "
	}
	from := []string{}
	for _, n := range s.FromClause {
		sql, bs, err := c.from(n, env.ctes)
		if err != nil {
			return query{}, err
		}
		from = append(from, sql)
		env.bindings = append(env.bindings, bs...)
	}
	for i, b := range env.bindings {
		for _, a := range env.bindings[:i] {
			if b.alias == a.alias {
				return query{}, unsupported()
			}
		}
	}
	groups := []string{}
	groupRefs := map[string]bool{}
	for _, n := range s.GroupClause {
		if n.GetAConst() != nil {
			return query{}, unsupported()
		}
		e, err := c.expr(n, env, false)
		if err != nil {
			return query{}, err
		}
		e, err = c.concrete(e, 0)
		if err != nil || !c.signatures.ordered(e.typ) {
			return query{}, unsupported()
		}
		groups = append(groups, e.sql)
		groupRefs[e.sql] = true
		env.groups[expressionKey(n)] = e
	}
	targets := []expression{}
	out := query{}
	for _, n := range s.TargetList {
		r := n.GetResTarget()
		if r == nil {
			return query{}, unsupported()
		}
		if ref := r.Val.GetColumnRef(); ref != nil && len(ref.Fields) > 0 && ref.Fields[len(ref.Fields)-1].GetAStar() != nil {
			if r.Name != "" {
				return query{}, unsupported()
			}
			ns, err := names(ref.Fields[:len(ref.Fields)-1])
			if err != nil || len(ns) > 1 {
				return query{}, unsupported()
			}
			matched := false
			for _, b := range env.bindings {
				if len(ns) == 1 && ns[0] != b.alias {
					continue
				}
				matched = true
				for i, col := range b.columns {
					x := boundColumn(b, i)
					targets = append(targets, expression{sql: x, typ: col.Type, label: col.Name, refs: []string{x}})
				}
			}
			if !matched {
				return query{}, unsupported()
			}
			continue
		}
		e, err := c.expr(r.Val, env, true)
		if err != nil {
			return query{}, err
		}
		e, err = c.concrete(e, 0)
		if err != nil {
			return query{}, err
		}
		if r.Name != "" {
			e.label = r.Name
		}
		if e.label == "" {
			e.label = "?column?"
		}
		targets = append(targets, e)
	}
	if len(targets) == 0 || len(targets) > 1600 {
		return query{}, unsupported()
	}
	where := ""
	if s.WhereClause != nil {
		e, err := c.expr(s.WhereClause, env, false)
		if err != nil {
			return query{}, err
		}
		e, err = c.concrete(e, 16)
		if err != nil {
			return query{}, err
		}
		where = " WHERE " + e.sql
	}
	having := ""
	var hav expression
	if s.HavingClause != nil {
		e, err := c.expr(s.HavingClause, env, true)
		if err != nil {
			return query{}, err
		}
		hav, err = c.concrete(e, 16)
		if err != nil {
			return query{}, err
		}
		having = " HAVING " + hav.sql
	}
	aggregate := len(groups) > 0 || s.HavingClause != nil || hav.aggregate
	for _, e := range targets {
		aggregate = aggregate || e.aggregate
	}
	orders := []string{}
	orderExpr := []expression{}
	for _, n := range s.SortClause {
		v := n.GetSortBy()
		if v == nil {
			return query{}, unsupported()
		}
		e, err := c.orderExpression(v.Node, env, targets)
		if err != nil {
			return query{}, err
		}
		if !c.signatures.ordered(e.typ) {
			return query{}, unsupported()
		}
		orderExpr = append(orderExpr, e)
		aggregate = aggregate || e.aggregate
		dir := " ASC"
		if v.SortbyDir == pg.SortByDir_SORTBY_DESC {
			dir = " DESC"
		} else if v.SortbyDir != pg.SortByDir_SORTBY_DEFAULT && v.SortbyDir != pg.SortByDir_SORTBY_ASC {
			return query{}, unsupported()
		}
		nulls := ""
		switch v.SortbyNulls {
		case pg.SortByNulls_SORTBY_NULLS_DEFAULT:
		case pg.SortByNulls_SORTBY_NULLS_FIRST:
			nulls = " NULLS FIRST"
		case pg.SortByNulls_SORTBY_NULLS_LAST:
			nulls = " NULLS LAST"
		default:
			return query{}, unsupported()
		}
		orders = append(orders, e.sql+dir+nulls)
	}
	if aggregate {
		for _, e := range append(append(append([]expression{}, targets...), hav), orderExpr...) {
			if groupRefs[e.sql] {
				continue
			}
			for _, ref := range e.refs {
				if !groupRefs[ref] {
					return query{}, unsupported()
				}
			}
		}
	}
	projection := []string{}
	for _, e := range targets {
		projection = append(projection, e.sql+" AS "+quote(e.label))
		out.columns = append(out.columns, Column{e.label, e.typ})
	}
	out.sql = prefix + "SELECT " + strings.Join(projection, ",")
	if len(from) > 0 {
		out.sql += " FROM " + strings.Join(from, ",")
	}
	out.sql += where
	if len(groups) > 0 {
		out.sql += " GROUP BY " + strings.Join(groups, ",")
	}
	out.sql += having
	if len(orders) > 0 {
		out.sql += " ORDER BY " + strings.Join(orders, ",")
	}
	for _, v := range []struct {
		n  *pg.Node
		kw string
	}{{s.LimitCount, " LIMIT "}, {s.LimitOffset, " OFFSET "}} {
		if v.n == nil {
			continue
		}
		a := v.n.GetAConst()
		if a == nil || a.GetIval() == nil || a.GetIval().Ival < 0 {
			return query{}, unsupported()
		}
		value := strconv.Itoa(int(a.GetIval().Ival))
		bound, err := c.concrete(expression{typ: 20, literal: &Parameter{Value: &value}}, 20)
		if err != nil {
			return query{}, err
		}
		out.sql += v.kw + bound.sql
	}
	return out, nil
}

func (c *compiler) from(n *pg.Node, ctes map[string]query) (string, []binding, error) {
	if r := n.GetRangeVar(); r != nil {
		b := binding{schema: r.Schemaname, name: r.Relname, alias: r.Relname, emitted: c.unique()}
		base := ""
		if r.Schemaname == "" {
			q, ok := ctes[r.Relname]
			if !ok || !r.Inh {
				return "", nil, unsupported()
			}
			base = q.sql
			b.columns = q.columns
		} else {
			rel, ok := c.resolved[relationName{r.Schemaname, r.Relname, !r.Inh}]
			if !ok {
				return "", nil, unsupported()
			}
			if len(rel.Columns) == 0 {
				return "", nil, unsupported()
			}
			b.columns = rel.Columns
			base = relationSQL(rel)
		}
		if r.Alias != nil {
			b.alias = r.Alias.Aliasname
			b.explicitAlias = true
			var err error
			b.columns, err = rename(b.columns, r.Alias.Colnames)
			if err != nil {
				return "", nil, err
			}
		}
		return base + " AS " + quote(b.emitted) + " (" + internalColumns(b.columns) + ")", []binding{b}, nil
	}
	if r := n.GetRangeSubselect(); r != nil {
		if r.Alias == nil {
			return "", nil, unsupported()
		}
		q, err := c.selectQuery(r.Subquery.GetSelectStmt(), ctes)
		if err != nil {
			return "", nil, err
		}
		cols, err := rename(q.columns, r.Alias.Colnames)
		if err != nil {
			return "", nil, err
		}
		b := binding{alias: r.Alias.Aliasname, emitted: c.unique(), columns: cols}
		return "(" + q.sql + ") AS " + quote(b.emitted) + " (" + internalColumns(cols) + ")", []binding{b}, nil
	}
	if j := n.GetJoinExpr(); j != nil {
		l, lb, err := c.from(j.Larg, ctes)
		if err != nil {
			return "", nil, err
		}
		r, rb, err := c.from(j.Rarg, ctes)
		if err != nil {
			return "", nil, err
		}
		bs := append(lb, rb...)
		join := ""
		switch j.Jointype {
		case pg.JoinType_JOIN_INNER:
			join = "INNER"
		case pg.JoinType_JOIN_LEFT:
			join = "LEFT"
		case pg.JoinType_JOIN_RIGHT:
			join = "RIGHT"
		case pg.JoinType_JOIN_FULL:
			join = "FULL"
		default:
			return "", nil, unsupported()
		}
		on := ""
		if j.Quals == nil {
			if join != "INNER" {
				return "", nil, unsupported()
			}
			join = "CROSS"
		} else {
			e, err := c.expr(j.Quals, scope{bindings: bs, ctes: ctes}, false)
			if err != nil {
				return "", nil, err
			}
			e, err = c.concrete(e, 16)
			if err != nil {
				return "", nil, err
			}
			on = " ON " + e.sql
		}
		return "(" + l + " " + join + " JOIN " + r + on + ")", bs, nil
	}
	return "", nil, unsupported()
}

func relationSQL(r Relation) string {
	if len(r.Dependencies) == 0 || len(r.Scans) == 1 && r.Scans[0].OID == r.OID {
		return "ONLY " + quote(r.Schema) + "." + quote(r.Name)
	}
	projection := []string{}
	for _, col := range r.Columns {
		projection = append(projection, quote(col.Name))
	}
	scans := []string{}
	for _, scan := range r.Scans {
		scans = append(scans, "SELECT "+strings.Join(projection, ",")+" FROM ONLY "+quote(scan.Schema)+"."+quote(scan.Name))
	}
	if len(scans) == 0 {
		projection = nil
		for _, col := range r.Columns {
			projection = append(projection, "NULL::"+typeSQL(col.Type)+" AS "+quote(col.Name))
		}
		scans = append(scans, "SELECT "+strings.Join(projection, ",")+" WHERE false")
	}
	return "(" + strings.Join(scans, " UNION ALL ") + ")"
}

// Every range variable gets unique internal column names. Public duplicate
// labels remain in projections without making expanded stars ambiguous.
func boundColumn(b binding, i int) string {
	return quote(b.emitted) + "." + quote("_c"+strconv.Itoa(i))
}
func internalColumns(cols []Column) string {
	names := make([]string, len(cols))
	for i := range cols {
		names[i] = quote("_c" + strconv.Itoa(i))
	}
	return strings.Join(names, ",")
}

func expressionKey(n *pg.Node) string {
	copy := proto.Clone(n)
	var clearLocations func(protoreflect.Message)
	clearLocations = func(m protoreflect.Message) {
		m.Range(func(f protoreflect.FieldDescriptor, v protoreflect.Value) bool {
			if f.Name() == "location" {
				m.Clear(f)
				return true
			}
			if f.Kind() == protoreflect.MessageKind {
				if f.IsList() {
					for i := 0; i < v.List().Len(); i++ {
						clearLocations(v.List().Get(i).Message())
					}
				} else {
					clearLocations(v.Message())
				}
			}
			return true
		})
	}
	clearLocations(copy.ProtoReflect())
	b, _ := proto.MarshalOptions{Deterministic: true}.Marshal(copy)
	return string(b)
}

// ORDER BY resolves bare output names before input names, unlike GROUP BY.
// Emit an ordinal for that binding so internal range renames cannot alter it.
func (c *compiler) orderExpression(n *pg.Node, env scope, targets []expression) (expression, error) {
	if a := n.GetAConst(); a != nil {
		if a.GetIval() == nil {
			return expression{}, unsupported()
		}
		pos := int(a.GetIval().Ival)
		if pos < 1 || pos > len(targets) {
			return expression{}, unsupported()
		}
		e := targets[pos-1]
		e.sql = strconv.Itoa(pos)
		return e, nil
	}
	if ref := n.GetColumnRef(); ref != nil {
		ns, err := names(ref.Fields)
		if err != nil {
			return expression{}, err
		}
		if len(ns) == 1 {
			index := -1
			for i, e := range targets {
				if e.label != ns[0] {
					continue
				}
				if index >= 0 && targets[index].sql != e.sql {
					return expression{}, unsupported()
				}
				index = i
			}
			if index >= 0 {
				e := targets[index]
				e.sql = strconv.Itoa(index + 1)
				return e, nil
			}
		}
	}
	e, err := c.expr(n, env, true)
	if err != nil {
		return e, err
	}
	return c.concrete(e, 0)
}
