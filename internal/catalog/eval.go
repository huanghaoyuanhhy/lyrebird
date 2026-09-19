package catalog

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// The catalog query evaluator. It runs the shapes catalog clients actually
// send — joins over the virtual tables, CASE, LIKE/regex filters, ORDER BY
// over output aliases, bind parameters, a scalar-function whitelist — and
// nothing else. Everything is in-memory: a source's row set is the virtual
// table body built from one Provider snapshot per execution.

// Col is one output column: its name and the PG type OID cells render as.
type Col struct {
	Name string
	Oid  uint32
}

// sourceSpec is one flattened FROM source: its reference, how it joins the
// sources before it, and its join condition.
type sourceSpec struct {
	ref  tableRef
	kind string // "first", "join", "left", "cross"
	on   expr
}

// flattenFrom walks a (left-deep) FROM tree into an ordered source list.
func flattenFrom(te tableExpr) []sourceSpec {
	switch t := te.(type) {
	case tableRef:
		return []sourceSpec{{ref: t, kind: "first"}}
	case joinExpr:
		out := flattenFrom(t.left)
		// RIGHT joins are rejected at validation; only left/inner/cross
		// reach here, all of which extend the left side in order.
		right, ok := t.right.(tableRef)
		if !ok {
			panic("internal: nested join on the right side")
		}
		out = append(out, sourceSpec{ref: right, kind: t.kind, on: t.on})
		return out
	default:
		return nil
	}
}

// boundSource is a source with its rows materialized for one execution.
type boundSource struct {
	spec sourceSpec
	def  *tableDef
	rows []map[string]any
}

// evalCtx evaluates expressions against one row combination.
type evalCtx struct {
	snap    *snapshot
	sources []boundSource
	byAlias map[string]int // alias → index into sources
}

// combo holds one candidate row: the current row (as a column→value map)
// per source, aligned with the source order.
type combo []map[string]any

// Statement is a prepared catalog query: output shape known ahead of the
// rows (the wire's RowDescription precedes them), rows generated per
// execution from a Provider snapshot.
type Statement struct {
	cols    []Col
	st      *stmt
	sources []sourceSpec
	defs    []*tableDef
	special *specialResult
}

// Columns returns the output shape.
func (s *Statement) Columns() []Col { return s.cols }

// Exec materializes the query: provider calls happen here, never at prepare
// time. params carries the bound parameters (strings or nil — catalog bind
// values are client-authored scalars).
func (s *Statement) Exec(ctx context.Context, p Provider, db string, params []any) ([][]any, error) {
	snap := &snapshot{
		db:       db,
		provider: p,
		tables:   map[string]*tableData{},
		settings: map[string]string{},
		params:   params,
	}

	if s.special != nil {
		return s.special.run(ctx, snap)
	}

	sources, err := s.bindSources(ctx, snap)
	if err != nil {
		return nil, err
	}
	ec := &evalCtx{snap: snap, byAlias: map[string]int{}}
	for i, src := range sources {
		ec.sources = append(ec.sources, src)
		ec.byAlias[src.spec.ref.alias] = i
	}

	combos, err := s.joinRows(ec)
	if err != nil {
		return nil, err
	}

	combos, err = s.filterRows(ec, combos)
	if err != nil {
		return nil, err
	}

	proj, err := s.project(ec, combos)
	if err != nil {
		return nil, err
	}
	if s.st.distinct {
		proj = distinctRows(proj)
	}

	if err := s.sortRows(ec, combos, proj); err != nil {
		return nil, err
	}
	proj = windowRows(proj, s.st.offset, s.st.limit, ec)
	return proj, nil
}

// bindSources materializes every FROM source's virtual table once.
func (s *Statement) bindSources(ctx context.Context, snap *snapshot) ([]boundSource, error) {
	out := make([]boundSource, 0, len(s.sources))
	for _, spec := range s.sources {
		def := s.defFor(spec.ref)
		td, err := snap.table(ctx, def)
		if err != nil {
			return nil, err
		}
		rows := make([]map[string]any, 0, len(td.rows))
		for _, r := range td.rows {
			m := make(map[string]any, len(def.cols))
			for i, c := range def.cols {
				m[c.name] = r[i]
			}
			rows = append(rows, m)
		}
		out = append(out, boundSource{spec: spec, def: def, rows: rows})
	}
	return out, nil
}

func (s *Statement) defFor(ref tableRef) *tableDef {
	return tableRegistry[ref.name]
}

// joinRows builds every row combination the FROM clause produces: sources
// extend left to right, each joining by its kind, LEFT keeping unmatched
// combinations with an all-null row.
func (s *Statement) joinRows(ec *evalCtx) ([]combo, error) {
	combos := []combo{{}}
	for i, src := range ec.sources {
		next := []combo{}
		for _, c := range combos {
			matched := false
			for _, row := range src.rows {
				c2 := make(combo, len(c)+1)
				copy(c2, c)
				c2[i] = row
				if src.spec.on != nil {
					v, err := ec.eval(src.spec.on, c2)
					if err != nil {
						return nil, err
					}
					if !truth(v) {
						continue
					}
				}
				matched = true
				next = append(next, c2)
			}
			if src.spec.kind == "left" && !matched {
				c2 := make(combo, len(c)+1)
				copy(c2, c)
				c2[i] = nil
				next = append(next, c2)
			}
		}
		combos = next
	}
	return combos, nil
}

// filterRows applies WHERE.
func (s *Statement) filterRows(ec *evalCtx, combos []combo) ([]combo, error) {
	if s.st.where == nil {
		return combos, nil
	}
	out := make([]combo, 0, len(combos))
	for _, c := range combos {
		v, err := ec.eval(s.st.where, c)
		if err != nil {
			return nil, err
		}
		if truth(v) {
			out = append(out, c)
		}
	}
	return out, nil
}

// project evaluates the select list. The returned slice stays aligned with
// combos so ORDER BY can evaluate expressions against the same rows.
func (s *Statement) project(ec *evalCtx, combos []combo) ([][]any, error) {
	out := make([][]any, 0, len(combos))
	for _, c := range combos {
		row, err := s.projectRow(ec, c)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, nil
}

func (s *Statement) projectRow(ec *evalCtx, c combo) ([]any, error) {
	var row []any
	for _, item := range s.st.items {
		if !item.star {
			v, err := ec.eval(item.e, c)
			if err != nil {
				return nil, err
			}
			row = append(row, v)
			continue
		}
		// star expansion, in source order
		for i, src := range ec.sources {
			if item.tbl != "" && src.spec.ref.alias != item.tbl {
				continue
			}
			for _, col := range src.def.cols {
				var v any
				if c[i] != nil {
					v = c[i][col.name]
				}
				row = append(row, v)
			}
		}
	}
	return row, nil
}

func distinctRows(rows [][]any) [][]any {
	seen := map[string]bool{}
	out := make([][]any, 0, len(rows))
	for _, r := range rows {
		key := fmt.Sprint(r)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, r)
	}
	return out
}

// sortRows orders the projected rows in place. Each ORDER BY term resolves
// to an output column by name first (pgjdbc sorts by quoted aliases like
// "TABLE_SCHEM"), otherwise evaluates as an expression against the
// underlying combination.
func (s *Statement) sortRows(ec *evalCtx, combos []combo, proj [][]any) error {
	if len(s.st.order) == 0 {
		return nil
	}
	keys := make([][]any, len(proj))
	nameToIdx := map[string]int{}
	for i, c := range s.cols {
		nameToIdx[c.Name] = i
	}
	for t, term := range s.st.order {
		ref, isRef := term.e.(colRef)
		if isRef && ref.tbl == "" {
			if idx, ok := nameToIdx[ref.name]; ok {
				for r := range proj {
					if keys[r] == nil {
						keys[r] = make([]any, len(s.st.order))
					}
					keys[r][t] = proj[r][idx]
				}
				continue
			}
		}
		for r := range proj {
			if keys[r] == nil {
				keys[r] = make([]any, len(s.st.order))
			}
			v, err := ec.eval(term.e, combos[r])
			if err != nil {
				return err
			}
			keys[r][t] = v
		}
	}

	idx := make([]int, len(proj))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		ia, ib := idx[a], idx[b]
		for t, term := range s.st.order {
			c := compareValues(keys[ia][t], keys[ib][t])
			if term.desc {
				c = -c
			}
			if c != 0 {
				return c < 0
			}
		}
		return false
	})
	sorted := make([][]any, len(proj))
	for i, id := range idx {
		sorted[i] = proj[id]
	}
	copy(proj, sorted)
	return nil
}

// windowRows applies OFFSET/LIMIT (constant expressions — pgjdbc uses
// literals; a parameter is accepted). Absent clauses read as -1.
func windowRows(rows [][]any, offsetE, limitE expr, ec *evalCtx) [][]any {
	offset := constInt(offsetE, ec)
	limit := constInt(limitE, ec)
	if offset > 0 {
		if offset >= len(rows) {
			return nil
		}
		rows = rows[offset:]
	}
	if limit >= 0 && limit < len(rows) {
		rows = rows[:limit]
	}
	return rows
}

func constInt(e expr, ec *evalCtx) int {
	if e == nil {
		return -1
	}
	v, err := ec.eval(e, combo{})
	if err != nil {
		return -1
	}
	if f, ok := v.(float64); ok {
		return int(f)
	}
	return -1
}

// ─── expression evaluation ──────────────────────────────────────────────────

// truth interprets a value as a condition: nil (SQL NULL) is false, the
// only non-bool catalog conditions produce is a nil from outer joins.
func truth(v any) bool {
	b, ok := v.(bool)
	return ok && b
}

var pseudoVars = map[string]func(ec *evalCtx) any{
	"current_user":     func(ec *evalCtx) any { return "lyrebird" },
	"session_user":     func(ec *evalCtx) any { return "lyrebird" },
	"current_catalog":  func(ec *evalCtx) any { return ec.snap.db },
	"current_schema":   func(ec *evalCtx) any { return "public" },
	"current_timezone": func(ec *evalCtx) any { return "UTC" },
}

// eval evaluates one expression against a row combination.
func (ec *evalCtx) eval(e expr, c combo) (any, error) {
	switch x := e.(type) {
	case literal:
		return x.v, nil
	case paramRef:
		if x.n < 1 || x.n > len(ec.snap.params) {
			return nil, errf(0, "there is no parameter $%d", x.n)
		}
		return ec.snap.params[x.n-1], nil
	case colRef:
		return ec.evalColumn(x, c)
	case funcCall:
		return ec.evalFunction(x, c)
	case isNullExpr:
		v, err := ec.eval(x.e, c)
		if err != nil {
			return nil, err
		}
		return v == nil != x.not, nil
	case inExpr:
		v, err := ec.eval(x.e, c)
		if err != nil {
			return nil, err
		}
		found := false
		for _, le := range x.list {
			lv, err := ec.eval(le, c)
			if err != nil {
				return nil, err
			}
			if compareValues(v, lv) == 0 {
				found = true
				break
			}
		}
		return found != x.not, nil
	case binExpr:
		return ec.evalBinary(x, c)
	case unaryExpr:
		switch x.op {
		case "NOT":
			v, err := ec.eval(x.e, c)
			if err != nil {
				return nil, err
			}
			return !truth(v), nil
		case "NEG":
			v, err := ec.eval(x.e, c)
			if err != nil {
				return nil, err
			}
			f, ok := toFloat(v)
			if !ok {
				return nil, errf(0, "cannot negate %v", v)
			}
			return -f, nil
		}
		return nil, errf(0, "unknown unary operator %s", x.op)
	case caseExpr:
		return ec.evalCase(x, c)
	case castExpr:
		v, err := ec.eval(x.e, c)
		if err != nil {
			return nil, err
		}
		return coerceCast(v, x.target)
	case subscriptExpr:
		base, err := ec.eval(x.base, c)
		if err != nil {
			return nil, err
		}
		idx, err := ec.eval(x.idx, c)
		if err != nil {
			return nil, err
		}
		arr, ok := base.([]any)
		if !ok {
			return nil, errf(0, "cannot subscript a non-array value")
		}
		i, ok := toFloat(idx)
		if !ok {
			return nil, errf(0, "array subscript must be a number")
		}
		// PostgreSQL arrays are 1-based
		n := int(i)
		if n < 1 || n > len(arr) {
			return nil, nil
		}
		return arr[n-1], nil
	}
	return nil, errf(0, "cannot evaluate expression node %T", e)
}

// evalFunction checks the whitelist, evaluates arguments eagerly and calls
// the implementation.
func (ec *evalCtx) evalFunction(x funcCall, c combo) (any, error) {
	fd, ok := lookupFunction(x.name)
	if !ok {
		return nil, &Error{Type: "42883", Reason: fmt.Sprintf("function %s is not available in catalog queries", x.name)}
	}
	if fd.args >= 0 && len(x.args) != fd.args {
		return nil, errf(0, "function %s expects %d argument(s), got %d", x.name, fd.args, len(x.args))
	}
	args := make([]any, len(x.args))
	for i, arg := range x.args {
		v, err := ec.eval(arg, c)
		if err != nil {
			return nil, err
		}
		args[i] = v
	}
	return fd.fn(ec, args)
}

// evalColumn resolves a column reference: pseudo variables, then the
// qualified or (uniquely) unqualified source column.
func (ec *evalCtx) evalColumn(x colRef, c combo) (any, error) {
	if x.tbl == "" {
		if fn, ok := pseudoVars[x.name]; ok {
			return fn(ec), nil
		}
		matches := 0
		var hit any
		for i, src := range ec.sources {
			if src.def.colIndex(x.name) < 0 {
				continue
			}
			matches++
			if matches > 1 {
				return nil, &Error{Type: "42702", Reason: fmt.Sprintf("column reference %q is ambiguous", x.name)}
			}
			if c[i] != nil {
				hit = c[i][x.name]
			} else {
				hit = nil
			}
		}
		if matches == 0 {
			return nil, &Error{Type: "42703", Reason: fmt.Sprintf("column %q does not exist", x.name)}
		}
		return hit, nil
	}
	src, ok := ec.byAlias[x.tbl]
	if !ok {
		return nil, &Error{Type: "42P01", Reason: fmt.Sprintf("missing FROM-clause entry for table %q", x.tbl)}
	}
	bound := ec.sources[src]
	if bound.def.colIndex(x.name) < 0 {
		return nil, &Error{Type: "42703", Reason: fmt.Sprintf("column %s.%s does not exist", x.tbl, x.name)}
	}
	if c[src] == nil {
		return nil, nil
	}
	return c[src][x.name], nil
}

// evalBinary implements the comparison family, the logic operators and
// arithmetic. Comparisons follow SQL three-valued logic loosely: NULL
// operands make the predicate false, which catalog queries never notice.
func (ec *evalCtx) evalBinary(x binExpr, c combo) (any, error) {
	switch x.op {
	case "AND":
		l, err := ec.eval(x.l, c)
		if err != nil {
			return nil, err
		}
		if !truth(l) {
			return false, nil
		}
		r, err := ec.eval(x.r, c)
		if err != nil {
			return nil, err
		}
		return truth(r), nil
	case "OR":
		l, err := ec.eval(x.l, c)
		if err != nil {
			return nil, err
		}
		if truth(l) {
			return true, nil
		}
		r, err := ec.eval(x.r, c)
		if err != nil {
			return nil, err
		}
		return truth(r), nil
	}

	l, err := ec.eval(x.l, c)
	if err != nil {
		return nil, err
	}
	r, err := ec.eval(x.r, c)
	if err != nil {
		return nil, err
	}

	switch x.op {
	case "=", "<>", "<", ">", "<=", ">=":
		cmp := compareValues(l, r)
		var res bool
		switch x.op {
		case "=":
			res = cmp == 0
		case "<>":
			res = cmp != 0
		case "<":
			res = cmp < 0
		case ">":
			res = cmp > 0
		case "<=":
			res = cmp <= 0
		case ">=":
			res = cmp >= 0
		}
		return res != x.not, nil
	case "like", "ilike":
		res, err := likeMatch(l, r, x.op == "ilike")
		if err != nil {
			return nil, err
		}
		return res != x.not, nil
	case "~", "!~", "~*", "!~*":
		res, err := regexMatch(l, r, strings.HasSuffix(x.op, "*"))
		if err != nil {
			return nil, err
		}
		return res != strings.HasPrefix(x.op, "!"), nil
	case "BETWEEN":
		tail, ok := x.r.(betweenTail)
		if !ok {
			return nil, errf(0, "internal: malformed BETWEEN")
		}
		lo, err := ec.eval(tail.lo, c)
		if err != nil {
			return nil, err
		}
		hi, err := ec.eval(tail.hi, c)
		if err != nil {
			return nil, err
		}
		res := compareValues(l, lo) >= 0 && compareValues(l, hi) <= 0
		return res != x.not, nil
	case "+", "-", "*", "/", "||":
		if x.op == "||" {
			ls, _ := toString(l)
			rs, _ := toString(r)
			return ls + rs, nil
		}
		lf, lok := toFloat(l)
		rf, rok := toFloat(r)
		if !lok || !rok {
			return nil, errf(0, "arithmetic over non-numeric values")
		}
		switch x.op {
		case "+":
			return lf + rf, nil
		case "-":
			return lf - rf, nil
		case "*":
			return lf * rf, nil
		default:
			if rf == 0 {
				return nil, errf(0, "division by zero")
			}
			return lf / rf, nil
		}
	}
	return nil, errf(0, "unknown operator %s", x.op)
}

func (ec *evalCtx) evalCase(x caseExpr, c combo) (any, error) {
	var operand any
	hasOperand := x.operand != nil
	if hasOperand {
		v, err := ec.eval(x.operand, c)
		if err != nil {
			return nil, err
		}
		operand = v
	}
	for _, w := range x.whens {
		if hasOperand {
			v, err := ec.eval(w.cond, c)
			if err != nil {
				return nil, err
			}
			if compareValues(operand, v) == 0 {
				return ec.eval(w.then, c)
			}
			continue
		}
		v, err := ec.eval(w.cond, c)
		if err != nil {
			return nil, err
		}
		if truth(v) {
			return ec.eval(w.then, c)
		}
	}
	if x.elseE != nil {
		return ec.eval(x.elseE, c)
	}
	return nil, nil
}

// compareValues orders two cells: numbers numerically, strings
// lexicographically, booleans false<true, NULL sorts last, and a numeric
// string against a number compares numerically (bound parameters arrive as
// text even when the column is numeric). Incomparable pairs fall back to
// their string forms so the ordering stays total.
func compareValues(a, b any) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return 1
	case b == nil:
		return -1
	}
	af, aok := toFloat(a)
	bf, bok := toFloat(b)
	if aok && bok && (isNumber(a) || isNumber(b)) {
		switch {
		case af < bf:
			return -1
		case af > bf:
			return 1
		default:
			return 0
		}
	}
	as, _ := toString(a)
	bs, _ := toString(b)
	return strings.Compare(as, bs)
}

func isNumber(v any) bool {
	switch v.(type) {
	case float64, int, int64, int32, uint32:
		return true
	}
	return false
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case int32:
		return float64(x), true
	case uint32:
		return float64(x), true
	case string:
		f, err := strconv.ParseFloat(x, 64)
		return f, err == nil
	}
	return 0, false
}

func toString(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case nil:
		return "", true
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64), true
	case bool:
		if x {
			return "t", true
		}
		return "f", true
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i], _ = toString(e)
		}
		return "{" + strings.Join(parts, ",") + "}", true
	}
	return fmt.Sprint(v), true
}

// likeMatch translates a SQL LIKE pattern (% and _ wildcards) into a regexp.
func likeMatch(v, pattern any, insensitive bool) (bool, error) {
	if v == nil || pattern == nil {
		return false, nil
	}
	s, _ := toString(v)
	p, _ := toString(pattern)
	var b strings.Builder
	b.WriteString("^")
	for _, r := range p {
		switch r {
		case '%':
			b.WriteString(".*")
		case '_':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString("$")
	expr := b.String()
	if insensitive {
		expr = "(?i)" + expr
	}
	return regexp.MatchString(expr, s)
}

func regexMatch(v, pattern any, insensitive bool) (bool, error) {
	if v == nil || pattern == nil {
		return false, nil
	}
	s, _ := toString(v)
	p, _ := toString(pattern)
	if insensitive {
		p = "(?i)" + p
	}
	return regexp.MatchString(p, s)
}

// coerceCast is CAST's best-effort conversion: the text family renders,
// numeric targets parse, bool accepts its literals. Failure renders as text
// rather than erroring — catalog casts (typname::text and friends) are
// display helpers, not semantics.
func coerceCast(v any, target string) (any, error) {
	switch target {
	case "text", "varchar", "bpchar", "char", "name", "string":
		s, _ := toString(v)
		return s, nil
	case "int2", "int4", "int8", "integer", "smallint", "bigint",
		"float4", "float8", "real", "double precision", "numeric":
		if v == nil {
			return nil, nil
		}
		f, ok := toFloat(v)
		if !ok {
			return nil, nil
		}
		return f, nil
	case "bool", "boolean":
		if v == nil {
			return nil, nil
		}
		return truth(v), nil
	default:
		// regclass/regtype/oidvector and everything else: identity text
		s, _ := toString(v)
		return s, nil
	}
}
