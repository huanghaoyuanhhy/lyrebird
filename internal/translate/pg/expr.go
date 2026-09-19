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
	// wFalse is the constant false predicate: the FALSE keyword, NULL in a
	// boolean position (only TRUE rows pass a WHERE, so UNKNOWN behaves
	// like FALSE), and comparisons against NULL. Plans mark it NoMatch.
	wFalse struct{}
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
	}
	wBetw struct {
		col     string
		lo, hi  literal
		negated bool
	}
	// wNull is IS NULL (negated: IS NOT NULL).
	wNull struct {
		col     string
		negated bool
	}
)

func (wFalse) isWhere() {}
func (wCol) isWhere()   {}
func (wNot) isWhere()   {}
func (wAnd) isWhere()   {}
func (wOr) isWhere()    {}
func (wCmp) isWhere()   {}
func (wIn) isWhere()    {}
func (wBetw) isWhere()  {}
func (wNull) isWhere()  {}

var _ = []whereExpr{wFalse{}, wCol{}, wNot{}, wAnd{}, wOr{}, wCmp{}, wIn{}, wBetw{}, wNull{}}

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

// fold constant-folds the boolean skeleton so plans stay minimal, the way
// es/fold removes match_all/match_none:
//
//   - false in AND kills the expression; in OR it drops out
//   - NOT false is true, NOT true is false
//
// NULL folds like false: a WHERE passes only TRUE rows, so an UNKNOWN
// operand is filtered either way.
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
		child := fold(e.child)
		switch {
		case isFalse(child):
			return nil
		case child == nil:
			return wFalse{}
		}
		// NOT pushes into a null check (NOT (x IS NULL) is x IS NOT NULL):
		// leaving the `not` in place would render `not (x is null)`, which
		// Milvus evaluates under three-valued logic and mis-filters.
		if n, ok := child.(wNull); ok {
			return wNull{col: n.col, negated: !n.negated}
		}
		return wNot{child}
	}
	return w
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
