package pg

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/huanghaoyuanhhy/lyrebird/internal/translate"
)

// The WHERE IR mirrors SQL syntax as written, before schema-aware lowering;
// like the es query tree it is frontend-private (docs/design.md, 2026-09-13).
type whereExpr interface{ isWhere() }

type (
	// wFalse is the constant false predicate: the FALSE keyword in a
	// boolean position. Plans mark it NoMatch. NOT false is true, so a
	// negation resurrects it — unlike wUnknown.
	wFalse struct{}
	// wUnknown is the three-valued unknown constant: a comparison against
	// the NULL literal (unknown on every row, whatever the column holds)
	// or a bare NULL in a boolean position. A WHERE passes only TRUE rows,
	// so it never matches — but NOT unknown is still unknown, so a
	// negation must not resurrect it either (docs/design.md). Inside
	// and/or it behaves exactly like the constant false (`false and e`
	// filters everything, `false or e` is e); only a direct NOT needs
	// special handling, which fold applies.
	wUnknown struct{}
	// wCol is a bare column; PG requires it to be boolean.
	wCol struct{ col string }
	wNot struct{ child whereExpr }
	// wAnd / wOr are n-ary, matching the flat lists parseAnd/parseOr read.
	wAnd struct{ parts []whereExpr }
	wOr  struct{ parts []whereExpr }
	// wCmp is `column op literal` (literal-first comparisons are flipped at
	// parse time). op is the SQL spelling: = <> < <= > >=.
	wCmp struct {
		op  string
		col string
		val literal
	}
	wIn struct {
		col     string
		vals    []literal
		negated bool
		// hadNull records a NULL member in the written list. It changes
		// nothing for a positive IN (unknown rows never pass either way)
		// and makes a negated one never-true: x NOT IN (…, NULL) is false
		// on the member rows and unknown everywhere else.
		hadNull bool
	}
	wBetw struct {
		col     string
		lo, hi  literal
		negated bool
		// nullLo / nullHi record a NULL bound. A NULL bound makes its
		// comparison unknown on every row, so the between never passes —
		// and its negation lowers to the opposite single-bound comparison.
		nullLo, nullHi bool
	}
	// wNull is IS NULL (negated: IS NOT NULL).
	wNull struct {
		col     string
		negated bool
	}
)

func (wFalse) isWhere()   {}
func (wUnknown) isWhere() {}
func (wCol) isWhere()     {}
func (wNot) isWhere()     {}
func (wAnd) isWhere()     {}
func (wOr) isWhere()      {}
func (wCmp) isWhere()     {}
func (wIn) isWhere()      {}
func (wBetw) isWhere()    {}
func (wNull) isWhere()    {}

var _ = []whereExpr{wFalse{}, wUnknown{}, wCol{}, wNot{}, wAnd{}, wOr{}, wCmp{}, wIn{}, wBetw{}, wNull{}}

// literal is a typed SQL literal; text carries the string value or the
// number as written (parsed per field type at lowering time).
type literal struct {
	kind litKind
	text string
}

type litKind int

const (
	litNumber litKind = iota
	litString
	litBool
	litNull
)

// valueNode is one side of a predicate: a column reference or a literal.
type valueNode struct {
	col string  // empty when a literal
	lit literal // zero value when col != ""
}

// columnOf requires a predicate's operand to be a plain column.
func columnOf(v valueNode, clause string) (string, error) {
	if v.col == "" {
		return "", parseErrf("[%s] applies to a column, not a literal", clause)
	}
	return v.col, nil
}

// isFalse reports whether the (already folded) predicate is the constant
// false; nil means constant true.
func isFalse(w whereExpr) bool {
	_, ok := w.(wFalse)
	return ok
}

// isUnknown reports whether the (already folded) predicate is the constant
// unknown — never true on any row, and never resurrected by a NOT.
func isUnknown(w whereExpr) bool {
	_, ok := w.(wUnknown)
	return ok
}

// neverTrue reports whether the folded predicate is structurally
// never-true: a negated IN with a NULL member, or a BETWEEN with a NULL
// bound, is false on some rows and unknown on the rest, so no row passes.
// fold keeps these shapes (their negation lowers to a matchable
// predicate), so the plan recognizes them at the top level — where no NOT
// wraps them anymore.
func neverTrue(w whereExpr) bool {
	switch e := w.(type) {
	case wIn:
		return e.negated && e.hadNull
	case wBetw:
		return e.nullLo || e.nullHi
	}
	return false
}

// fold constant-folds the boolean skeleton so plans stay minimal, the way
// es/fold removes match_all/match_none:
//
//   - false in AND kills the expression; in OR it drops out
//   - NOT false is true, NOT true is false, NOT unknown is unknown
//
// Unknown (a comparison against the NULL literal, a bare NULL) never
// matches, but it is not false: inside and/or it acts like the constant
// false (`false and e` filters everything, `false or e` is e), while a NOT
// must not turn it into match-everything. foldNegation therefore resolves
// every NOT against the three-valued rules and, when an unknown sits under
// a NOT through an and/or, pushes the negation through De Morgan so the
// unknown never ends up directly negated.
func fold(w whereExpr) whereExpr {
	switch e := w.(type) {
	case wAnd:
		var keep []whereExpr
		for _, part := range e.parts {
			f := fold(part)
			if isFalse(f) {
				return wFalse{}
			}
			if f != nil {
				keep = append(keep, f)
			}
		}
		switch len(keep) {
		case 0:
			return nil
		case 1:
			return keep[0]
		}
		return wAnd{keep}
	case wOr:
		var keep []whereExpr
		for _, part := range e.parts {
			f := fold(part)
			switch {
			case f == nil:
				return nil // one true branch makes the whole OR true
			case isFalse(f):
				// impossible branches drop out
			default:
				keep = append(keep, f)
			}
		}
		switch len(keep) {
		case 0:
			return wFalse{} // every branch impossible
		case 1:
			return keep[0]
		}
		return wOr{keep}
	case wNot:
		return foldNegation(fold(e.child))
	}
	return w
}

// foldNegation negates an already folded predicate under three-valued
// logic. See fold for why the unknown cases are spelled out.
func foldNegation(child whereExpr) whereExpr {
	switch {
	case isFalse(child):
		return nil
	case child == nil:
		return wFalse{}
	case isUnknown(child):
		return wUnknown{}
	}
	// NOT pushes into a null check (NOT (x IS NULL) is x IS NOT NULL):
	// leaving the `not` in place would render `not (x is null)`, which
	// Milvus evaluates under three-valued logic and mis-filters.
	if n, ok := child.(wNull); ok {
		return wNull{col: n.col, negated: !n.negated}
	}
	if in, ok := child.(wIn); ok {
		// The negation bit flips; hadNull rides along. A list with a NULL
		// member is never-true when negated (unknown on the non-member
		// rows) and matches exactly the member rows once un-negated, which
		// is what x IN (…) renders either way.
		return wIn{col: in.col, vals: in.vals, negated: !in.negated, hadNull: in.hadNull}
	}
	if b, ok := child.(wBetw); ok {
		return negatedBetween(b)
	}
	if containsUnknown(child) {
		// not (unknown and e) is not e, and not (unknown or e) is
		// unknown and not e — both follow from pushing the NOT through
		// De Morgan; leaving the NOT in place would render `not (false
		// and e)`, which matches everything instead of e's rejects.
		switch c := child.(type) {
		case wAnd:
			return fold(wOr{parts: negateParts(c.parts)})
		case wOr:
			return fold(wAnd{parts: negateParts(c.parts)})
		}
	}
	if n, ok := child.(wNot); ok {
		return n.child // double negation clears
	}
	return wNot{child}
}

func negateParts(parts []whereExpr) []whereExpr {
	out := make([]whereExpr, len(parts))
	for i, p := range parts {
		out[i] = fold(wNot{p})
	}
	return out
}

// negatedBetween negates a BETWEEN under three-valued logic. A NULL bound
// makes its comparison unknown on every row, so:
//
//	NOT (x BETWEEN lo AND hi)     → the bounds flip, as in SQL
//	NOT (x BETWEEN NULL AND hi)   → x > hi  (the rows the negation rescues
//	                                are exactly the ones the upper bound
//	                                rejects; the x >= NULL leg is unknown
//	                                everywhere)
//	NOT (x BETWEEN lo AND NULL)   → x < lo
//	NOT (x BETWEEN NULL AND NULL) → unknown on every row: never passes
func negatedBetween(b wBetw) whereExpr {
	switch {
	case b.nullLo && b.nullHi:
		return wUnknown{}
	case b.nullLo:
		return wCmp{op: ">", col: b.col, val: b.hi}
	case b.nullHi:
		return wCmp{op: "<", col: b.col, val: b.lo}
	}
	b.negated = !b.negated
	return b
}

// containsUnknown reports whether the folded subtree carries a wUnknown
// constant anywhere.
func containsUnknown(w whereExpr) bool {
	switch e := w.(type) {
	case wUnknown:
		return true
	case wNot:
		return containsUnknown(e.child)
	case wAnd:
		return anyPartUnknown(e.parts)
	case wOr:
		return anyPartUnknown(e.parts)
	}
	return false
}

func anyPartUnknown(parts []whereExpr) bool {
	for _, p := range parts {
		if containsUnknown(p) {
			return true
		}
	}
	return false
}

// compareMilvusOps maps the SQL comparison spelling onto the Milvus dialect.
var compareMilvusOps = map[string]translate.CompareOp{
	"=":  translate.Eq,
	"<>": translate.Ne,
	"!=": translate.Ne,
	"<":  translate.Lt,
	"<=": translate.Le,
	">":  translate.Gt,
	">=": translate.Ge,
}

// distanceMetrics maps the pgvector distance operators onto Milvus metrics —
// the pgvector compatibility surface is exactly this table (docs/design.md).
// <+> is absent on purpose: L1 distance has no Milvus metric.
var distanceMetrics = map[string]translate.Metric{
	"<->": translate.MetricL2,
	"<=>": translate.MetricCosine,
	"<#>": translate.MetricIP,
}

// searchSpec lowers one ORDER BY distance term into the plan's vector
// search: the operator picks the metric, the literal becomes the query
// vector, and the field must be a vector field of this collection. `<#>`
// (negated inner product) orders identically to IP — pgvector's negation is
// an ascending-sort trick, and nearest-first is the ANN contract either way,
// so the sign flip never reaches the plan.
func searchSpec(d *orderDistance, schema translate.Schema) (*translate.SearchSpec, error) {
	metric, ok := distanceMetrics[d.op]
	if !ok {
		return nil, unsupportedErrf("distance operator [%s] has no Milvus metric: use <-> (L2), <=> (cosine) or <#> (inner product)", d.op)
	}
	if ft := schema.FieldType(d.column); ft != translate.TypeVector {
		if ft == translate.TypeUnknown {
			return nil, illegalErrf("field [%s] is not a vector field of this collection", d.column)
		}
		return nil, illegalErrf("field [%s] is a %s field: distance operators order on vector fields", d.column, ft)
	}
	vec, err := parseVector(d.vector)
	if err != nil {
		return nil, err
	}
	return &translate.SearchSpec{Field: d.column, Vector: vec, Metric: metric}, nil
}

// parseVector reads a pgvector literal body — the text between the SQL
// quotes — into float32s. pgvector's text form wraps the numbers in square
// brackets; the brackets are accepted missing. The vector's dimension is the
// server's call: Milvus rejects a mismatching length by name.
func parseVector(s string) ([]float32, error) {
	body := strings.TrimSpace(s)
	body = strings.TrimSuffix(strings.TrimPrefix(body, "["), "]")
	if strings.TrimSpace(body) == "" {
		return nil, illegalErrf("vector literal is empty: write the query vector as comma-separated numbers in brackets, e.g. '[0.1, 0.2, 0.3]'")
	}
	parts := strings.Split(body, ",")
	vec := make([]float32, 0, len(parts))
	for i, p := range parts {
		p = strings.TrimSpace(p)
		f, err := strconv.ParseFloat(p, 32)
		if err != nil {
			return nil, illegalErrf("vector literal is %d comma-separated numbers, e.g. '[0.1, 0.2, 0.3]': entry %d ([%s]) does not parse as a number", len(parts), i+1, p)
		}
		vec = append(vec, float32(f))
	}
	return vec, nil
}

// buildWhere lowers the folded WHERE tree into the Milvus expression AST.
// nil means "no filter"; literal quoting and field-name validation happen
// later, in translate.Render.
func buildWhere(w whereExpr, schema translate.Schema) (translate.Expr, error) {
	switch e := w.(type) {
	case nil:
		return nil, nil
	case wFalse:
		return translate.Never{}, nil
	case wUnknown:
		// Unknown on every row: no row passes. fold has already pushed
		// every NOT down, so reaching here means the constant stands in a
		// positive and/or position, where the constant false renders
		// exactly like it.
		return translate.Never{}, nil
	case wCol:
		return translate.Compare{Op: translate.Eq, Field: e.col, Value: translate.BoolValue(true)}, nil
	case wNot:
		child, err := buildWhere(e.child, schema)
		if err != nil {
			return nil, err
		}
		return translate.Not{Child: child}, nil
	case wAnd:
		children, err := buildChildren(e.parts, schema)
		if err != nil {
			return nil, err
		}
		return translate.And{Children: children}, nil
	case wOr:
		children, err := buildChildren(e.parts, schema)
		if err != nil {
			return nil, err
		}
		return translate.Or{Children: children}, nil
	case wCmp:
		v, err := coerceValue(e.col, e.val, schema)
		if err != nil {
			return nil, err
		}
		op, ok := compareMilvusOps[e.op]
		if !ok {
			return nil, fmt.Errorf("internal: unknown comparison operator %q", e.op)
		}
		return translate.Compare{Op: op, Field: e.col, Value: v}, nil
	case wIn:
		if e.negated && e.hadNull {
			// x NOT IN (…, NULL) is false on the member rows and unknown
			// everywhere else: never passes, and its negation was already
			// lowered to the positive IN by fold.
			return translate.Never{}, nil
		}
		vals := make([]translate.Value, len(e.vals))
		for i, l := range e.vals {
			var err error
			if vals[i], err = coerceValue(e.col, l, schema); err != nil {
				return nil, err
			}
		}
		list := translate.InList{Field: e.col, Values: vals}
		if e.negated {
			return translate.Not{Child: list}, nil
		}
		return list, nil
	case wBetw:
		if e.nullLo || e.nullHi {
			// A NULL bound is unknown on every row: the between never
			// passes, and its negation was already lowered to the opposite
			// single-bound comparison by fold.
			return translate.Never{}, nil
		}
		lo, err := coerceValue(e.col, e.lo, schema)
		if err != nil {
			return nil, err
		}
		hi, err := coerceValue(e.col, e.hi, schema)
		if err != nil {
			return nil, err
		}
		bounds := translate.And{Children: []translate.Expr{
			translate.Compare{Op: translate.Ge, Field: e.col, Value: lo},
			translate.Compare{Op: translate.Le, Field: e.col, Value: hi},
		}}
		if e.negated {
			return translate.Not{Child: bounds}, nil
		}
		return bounds, nil
	case wNull:
		// IS NOT NULL rides the NotNull node; IS NULL gets its own node —
		// wrapping it in Not would render `not (x is not null)`, which
		// Milvus's three-valued `not` mis-filters (docs/design.md).
		if e.negated {
			return translate.NotNull{Field: e.col}, nil
		}
		return translate.IsNull{Field: e.col}, nil
	}
	return nil, fmt.Errorf("internal: unhandled where node %T", w)
}

func buildChildren(parts []whereExpr, schema translate.Schema) ([]translate.Expr, error) {
	children := make([]translate.Expr, len(parts))
	for i, part := range parts {
		var err error
		if children[i], err = buildWhere(part, schema); err != nil {
			return nil, err
		}
	}
	return children, nil
}

// coerceValue converts an SQL literal into the storage encoding for the
// field's type — the same coercions the ES front applies, since both
// protocols read one storage: numbers keep their kind (keyword fields take
// the text form), date fields take epoch millis from date strings (PG-native
// microseconds are a Phase 3 catalog decision; see the package docs). The
// PG front is stricter than ES where PG itself has no implicit cast: boolean
// literals compare only against boolean fields.
func coerceValue(field string, l literal, schema translate.Schema) (translate.Value, error) {
	switch l.kind {
	case litNumber:
		switch schema.FieldType(field) {
		case translate.TypeKeyword:
			return translate.StringValue(l.text), nil
		case translate.TypeBool:
			return translate.Value{}, illegalErrf("field [%s] is boolean but [%s] is a number", field, l.text)
		}
		if i, err := strconv.ParseInt(l.text, 10, 64); err == nil {
			return translate.IntValue(i), nil
		}
		f, err := strconv.ParseFloat(l.text, 64)
		if err != nil {
			return translate.Value{}, illegalErrf("field [%s] is numeric but [%s] does not parse as a number", field, l.text)
		}
		return translate.FloatValue(f), nil
	case litString:
		switch schema.FieldType(field) {
		case translate.TypeDate:
			return dateValue(field, l.text)
		case translate.TypeNumber:
			f, err := strconv.ParseFloat(l.text, 64)
			if err != nil {
				return translate.Value{}, illegalErrf("field [%s] is numeric but [%s] does not parse as a number", field, l.text)
			}
			return translate.FloatValue(f), nil
		case translate.TypeBool:
			if l.text != "true" && l.text != "false" {
				return translate.Value{}, illegalErrf("field [%s] is boolean but [%s] is not true/false", field, l.text)
			}
			return translate.BoolValue(l.text == "true"), nil
		default:
			return translate.StringValue(l.text), nil
		}
	case litBool:
		switch schema.FieldType(field) {
		case translate.TypeBool, translate.TypeUnknown:
			return translate.BoolValue(l.text == "true"), nil
		default:
			return translate.Value{}, illegalErrf("field [%s] does not compare against a boolean literal", field)
		}
	case litNull:
		// unreachable: the parser folds comparisons against NULL to wFalse
		return translate.Value{}, parseErrf("NULL is not a comparable value for field [%s]", field)
	}
	return translate.Value{}, parseErrf("unsupported literal for field [%s]", field)
}

var dateLayouts = []string{
	time.RFC3339Nano,          // 2024-01-02T15:04:05(.999)Z07:00
	"2006-01-02T15:04:05",     // no zone → UTC
	"2006-01-02 15:04:05.999", // PG space form, optional fraction → UTC
	"2006-01-02",              // date only → midnight UTC
}

func dateValue(field, s string) (translate.Value, error) {
	for _, layout := range dateLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return translate.IntValue(t.UnixMilli()), nil
		}
	}
	return translate.Value{}, illegalErrf("cannot parse date [%s] for field [%s]: use yyyy-MM-dd, yyyy-MM-dd HH:MM:SS or RFC-3339", s, field)
}
