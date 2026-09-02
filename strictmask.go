package main

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/pingcap/tidb/pkg/parser"
	"github.com/pingcap/tidb/pkg/parser/ast"
	// test_driver registers the value-expression driver the standalone parser
	// needs to parse literals. Despite the name it is the reference driver for
	// using pkg/parser outside TiDB; we only parse and walk the AST, never
	// evaluate, so nothing test-specific is exercised.
	_ "github.com/pingcap/tidb/pkg/parser/test_driver"
)

// queryPlan is the masking decision for one query. useWire means the query is
// simple enough that the wire-protocol origin tag is trustworthy, so the
// caller falls back to per-column rule matching (Masker.Masked); otherwise
// mask[i] says whether result column i must be hidden.
type queryPlan struct {
	useWire bool
	mask    []bool
}

// maxTraceDepth bounds recursion into nested sub-queries so a pathologically
// nested statement can't blow the stack; past it the query is refused.
const maxTraceDepth = 40

// parserPool avoids re-allocating a parser per query. parser.Parser is not safe
// for concurrent use, so each query borrows one.
var parserPool = sync.Pool{New: func() any { return parser.New() }}

func parseOne(sql string) (ast.StmtNode, error) {
	p := parserPool.Get().(*parser.Parser)
	defer parserPool.Put(p)
	return p.ParseOneStmt(sql, "", "")
}

func refusef(format string, args ...any) error {
	return fmt.Errorf("PII masking refused this query: "+format, args...)
}

// planQuery decides masking for one statement by reading it, not by trusting
// the wire tag. It is called for every query while masking is enabled.
func (m *Masker) planQuery(sql string) (*queryPlan, error) {
	stmt, err := parseOne(sql)
	if err != nil {
		if m.bestEffort {
			// Unparseable is unclassifiable — it may be valid MariaDB syntax
			// the parser's MySQL grammar rejects. Best effort runs it with
			// wire-tag masking rather than blocking staging work.
			return &queryPlan{useWire: true}, nil
		}
		return nil, refusef("could not parse it to verify masking — list columns explicitly or simplify it (%v)", err)
	}
	switch s := stmt.(type) {
	case *ast.ShowStmt, *ast.ExplainStmt:
		// Schema/plan metadata, not table rows — no personal data to mask.
		return &queryPlan{useWire: true}, nil
	case *ast.SelectStmt:
		// TABLE t and VALUES ROW(...) parse as a SelectStmt but return rows
		// without a normal column list to reason about; TABLE t is SELECT *
		// FROM t on MySQL 8.0.19+, so trusting the wire tag there is unverified.
		// Refuse both and make the caller use an explicit SELECT.
		if s.Kind != ast.SelectStmtKindSelect {
			return nil, refusef("only a plain SELECT is supported here, not the TABLE or VALUES form — list the columns in a SELECT")
		}
		if wireSafe(s) {
			return &queryPlan{useWire: true}, nil
		}
		return (&tracer{m: m}).plan(s)
	case *ast.SetOprStmt:
		return (&tracer{m: m}).plan(s)
	default:
		if m.bestEffort {
			// Writes and DDL pass unchecked by design (full_access); any rows
			// they do return get wire-tag masking.
			return &queryPlan{useWire: true}, nil
		}
		return nil, refusef("only SELECT/SHOW/DESCRIBE/EXPLAIN are allowed (got %T)", stmt)
	}
}

// wireSafe reports whether every result column's wire origin tag can be
// trusted: the query reads only base tables (single or joined), with no CTE,
// set operation, derived table, or computed column. In that shape aliases and
// stars keep a correct origin on the wire, so the ordinary Masker suffices.
func wireSafe(s *ast.SelectStmt) bool {
	if s.With != nil || s.From == nil {
		return false
	}
	if !allBaseTables(s.From.TableRefs) {
		return false
	}
	for _, f := range s.Fields.Fields {
		if f.WildCard != nil {
			continue
		}
		if _, ok := f.Expr.(*ast.ColumnNameExpr); !ok {
			return false
		}
	}
	return true
}

func allBaseTables(rs ast.ResultSetNode) bool {
	switch n := rs.(type) {
	case *ast.Join:
		if n.Left != nil && !allBaseTables(n.Left) {
			return false
		}
		if n.Right != nil && !allBaseTables(n.Right) {
			return false
		}
		return true
	case *ast.TableSource:
		_, ok := n.Source.(*ast.TableName)
		return ok
	default:
		return false
	}
}

// --- origin model ---------------------------------------------------------

type originKind int

const (
	originConst   originKind = iota // literal / count / no column input — never personal
	originColumn                    // resolves to a base table.column
	originExpr                      // computed from one or more inputs
	originUnknown                   // could not be traced — treated as personal (fail closed)
)

type colOrigin struct {
	kind   originKind
	table  string
	column string
	inputs []colOrigin
}

// fieldResult is one output column of a query: its label, whether it is a
// wildcard (which can't be position-aligned without a schema), and the origin
// its value reduces to.
type fieldResult struct {
	name     string
	wildcard bool
	origin   colOrigin
}

// relation resolves a column name within one scope entry. A base table
// resolves any name to itself; a derived table/CTE resolves only its known
// output names, and anything else is unknown (and thus hidden).
type relation struct {
	base    string
	derived map[string]colOrigin
}

func (r relation) resolve(col string) colOrigin {
	if r.base != "" {
		return colOrigin{kind: originColumn, table: r.base, column: col}
	}
	if o, ok := r.derived[col]; ok {
		return o
	}
	return colOrigin{kind: originUnknown}
}

type scope struct {
	byName map[string]relation
	order  []string
	ctes   map[string]relation
}

type tracer struct {
	m      *Masker
	depth  int
	refuse string // set on depth overflow; surfaced by plan
}

// plan traces a top-level query into a queryPlan.
func (tr *tracer) plan(rsn ast.ResultSetNode) (*queryPlan, error) {
	frs := tr.traceRSN(rsn, nil)
	if tr.refuse != "" {
		return nil, refusef("%s", tr.refuse)
	}
	mask := make([]bool, len(frs))
	for i, fr := range frs {
		if fr.wildcard {
			return nil, refusef("a SELECT * inside a sub-query, join, or union can't be verified — list the columns explicitly")
		}
		mask[i] = tr.personal(fr.origin)
	}
	return &queryPlan{mask: mask}, nil
}

func (tr *tracer) personal(o colOrigin) bool {
	switch o.kind {
	case originColumn:
		return tr.m.Masked([]byte(o.table), []byte(o.column))
	case originExpr:
		return slices.ContainsFunc(o.inputs, tr.personal)
	case originUnknown:
		// Couldn't trace it, so we can't vouch for it: hide it.
		return true
	default:
		return false
	}
}

// traceRSN traces a SELECT or set-operation (both ResultSetNode) into its
// output columns.
func (tr *tracer) traceRSN(rsn ast.ResultSetNode, ctes map[string]relation) []fieldResult {
	return tr.traceNode(rsn, ctes)
}

// traceNode dispatches over any query node. A set-operation's branches are
// plain ast.Node (a *SelectStmt or a nested *SetOprSelectList, which is not a
// ResultSetNode), so the internal path takes ast.Node rather than
// ResultSetNode.
func (tr *tracer) traceNode(n ast.Node, ctes map[string]relation) []fieldResult {
	tr.depth++
	defer func() { tr.depth-- }()
	if tr.depth > maxTraceDepth {
		tr.refuse = "the query nests too deeply to verify — simplify it"
		return nil
	}
	switch s := n.(type) {
	case *ast.SelectStmt:
		return tr.traceSelect(s, ctes)
	case *ast.SetOprStmt:
		return tr.traceSetOprList(s.SelectList, ctes)
	case *ast.SetOprSelectList:
		return tr.traceSetOprList(s, ctes)
	default:
		return nil
	}
}

func (tr *tracer) traceSetOprList(list *ast.SetOprSelectList, ctes map[string]relation) []fieldResult {
	if list == nil {
		return nil
	}
	ctes = tr.withCTEs(list.With, ctes)
	var merged []fieldResult
	for i, sel := range list.Selects {
		branch := tr.traceNode(sel, ctes)
		if i == 0 {
			merged = branch
			continue
		}
		for j := range merged {
			if j >= len(branch) {
				break
			}
			// A set-operation column is personal if any branch feeds it a
			// personal column; a wildcard in any branch taints the position.
			merged[j].wildcard = merged[j].wildcard || branch[j].wildcard
			merged[j].origin = colOrigin{
				kind:   originExpr,
				inputs: []colOrigin{merged[j].origin, branch[j].origin},
			}
		}
	}
	return merged
}

func (tr *tracer) traceSelect(n *ast.SelectStmt, ctes map[string]relation) []fieldResult {
	ctes = tr.withCTEs(n.With, ctes)
	sc := &scope{byName: map[string]relation{}, ctes: ctes}
	if n.From != nil {
		tr.buildScope(n.From.TableRefs, sc)
	}
	out := make([]fieldResult, 0, len(n.Fields.Fields))
	for _, f := range n.Fields.Fields {
		if f.WildCard != nil {
			out = append(out, fieldResult{name: "*", wildcard: true})
			continue
		}
		out = append(out, fieldResult{name: fieldName(f), origin: tr.reduceField(f, sc)})
	}
	return out
}

// withCTEs layers a WITH clause's definitions onto the inherited CTE map.
func (tr *tracer) withCTEs(with *ast.WithClause, ctes map[string]relation) map[string]relation {
	if with == nil {
		return ctes
	}
	merged := maps.Clone(ctes)
	if merged == nil {
		merged = map[string]relation{}
	}
	for _, cte := range with.CTEs {
		frs := tr.traceRSN(cte.Query.Query, merged)
		merged[cte.Name.L] = relationFromFields(frs)
	}
	return merged
}

func (tr *tracer) buildScope(rsn ast.ResultSetNode, sc *scope) {
	switch n := rsn.(type) {
	case *ast.Join:
		if n.Left != nil {
			tr.buildScope(n.Left, sc)
		}
		if n.Right != nil {
			tr.buildScope(n.Right, sc)
		}
	case *ast.TableSource:
		name := n.AsName.L
		switch src := n.Source.(type) {
		case *ast.TableName:
			if name == "" {
				name = src.Name.L
			}
			// A bare name may reference a CTE rather than a real table.
			if cte, ok := sc.ctes[src.Name.L]; ok && n.AsName.L == "" {
				sc.addRelation(name, cte)
			} else {
				sc.addRelation(name, relation{base: src.Name.L})
			}
		case *ast.SelectStmt, *ast.SetOprStmt:
			sc.addRelation(name, relationFromFields(tr.traceRSN(src, sc.ctes)))
		}
	}
}

func (sc *scope) addRelation(name string, r relation) {
	sc.byName[name] = r
	sc.order = append(sc.order, name)
}

func relationFromFields(frs []fieldResult) relation {
	der := map[string]colOrigin{}
	for _, fr := range frs {
		if fr.wildcard || fr.name == "" {
			continue // an unnamed/wildcard output can't be referenced upward
		}
		der[fr.name] = fr.origin
	}
	return relation{derived: der}
}

// reduceField collapses a select field's expression to the single origin its
// value carries. A plain COUNT is a number (never personal); a plain column
// keeps its column origin so table-qualified rules apply; anything else is an
// expression over whatever columns it references.
func (tr *tracer) reduceField(f *ast.SelectField, sc *scope) colOrigin {
	if agg, ok := f.Expr.(*ast.AggregateFuncExpr); ok && strings.EqualFold(agg.F, "count") {
		return colOrigin{kind: originConst}
	}
	if col, ok := f.Expr.(*ast.ColumnNameExpr); ok {
		return tr.resolveColumn(col.Name, sc)
	}
	cc := &colCollector{tr: tr, sc: sc}
	f.Expr.Accept(cc)
	if len(cc.origins) == 0 {
		return colOrigin{kind: originConst}
	}
	return colOrigin{kind: originExpr, inputs: cc.origins}
}

func (tr *tracer) resolveColumn(name *ast.ColumnName, sc *scope) colOrigin {
	if tbl := name.Table.L; tbl != "" {
		if r, ok := sc.byName[tbl]; ok {
			return r.resolve(name.Name.L)
		}
		return colOrigin{kind: originUnknown}
	}
	// Unqualified: unambiguous only when there is exactly one relation in scope.
	if len(sc.order) == 1 {
		return sc.byName[sc.order[0]].resolve(name.Name.L)
	}
	return colOrigin{kind: originUnknown}
}

func fieldName(f *ast.SelectField) string {
	if f.AsName.L != "" {
		return f.AsName.L
	}
	if col, ok := f.Expr.(*ast.ColumnNameExpr); ok {
		return col.Name.Name.L
	}
	return ""
}

// colCollector gathers the origins of every column reference and scalar
// sub-query inside one expression, without descending into a sub-query's own
// scope. Used for computed columns: the field is personal if any collected
// origin is.
type colCollector struct {
	tr      *tracer
	sc      *scope
	origins []colOrigin
}

func (c *colCollector) Enter(n ast.Node) (ast.Node, bool) {
	switch e := n.(type) {
	case *ast.ColumnNameExpr:
		c.origins = append(c.origins, c.tr.resolveColumn(e.Name, c.sc))
		return n, true
	case *ast.WindowFuncExpr:
		// A window function's value comes from its arguments; the PARTITION BY
		// / ORDER BY in its Spec only choose rows, so a column used only there
		// (ROW_NUMBER() OVER (ORDER BY phone)) does not put the value in the
		// output. Collect the args, skip the spec.
		for _, a := range e.Args {
			a.Accept(c)
		}
		return n, true
	case *ast.SubqueryExpr:
		for _, fr := range c.tr.traceRSN(e.Query, c.sc.ctes) {
			if fr.wildcard {
				c.origins = append(c.origins, colOrigin{kind: originUnknown})
			} else {
				c.origins = append(c.origins, fr.origin)
			}
		}
		return n, true
	}
	return n, false
}

func (c *colCollector) Leave(n ast.Node) (ast.Node, bool) { return n, true }
