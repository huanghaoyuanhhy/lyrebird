package es

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/huanghaoyuanhhy/lyrebird/internal/translate"
)

// buildExpr lowers the (already folded) query tree into the Milvus
// boolean-expression AST. nil means "match everything"; literal quoting and
// field-name validation happen later, in translate.Render.
func buildExpr(n queryNode, schema translate.Schema) (translate.Expr, error) {
	switch q := n.(type) {
	case matchAll:
		return nil, nil
	case matchNone:
		return translate.Never{}, nil
	case boolQuery:
		return boolExpr(q, schema)
	case termQuery:
		v, err := coerceValue(q.field, q.value, schema)
		if err != nil {
			return nil, err
		}
		return translate.Compare{Op: translate.Eq, Field: q.field, Value: v}, nil
	case termsQuery:
		vals := make([]translate.Value, len(q.values))
		for i, v := range q.values {
			var err error
			if vals[i], err = coerceValue(q.field, v, schema); err != nil {
				return nil, err
			}
		}
		return translate.InList{Field: q.field, Values: vals}, nil
	case matchQuery:
		return matchExpr(q, schema)
	case rangeQuery:
		children := make([]translate.Expr, 0, len(q.bounds))
		for _, b := range q.bounds {
			v, err := coerceValue(q.field, b.value, schema)
			if err != nil {
				return nil, err
			}
			children = append(children, translate.Compare{Op: translate.CompareOp(b.op), Field: q.field, Value: v})
		}
		if len(children) == 1 {
			return children[0], nil
		}
		return translate.And{Children: children}, nil
	case existsQuery:
		// nullable-field syntax; re-verify at store integration
		return translate.NotNull{Field: q.field}, nil
	}
	return nil, fmt.Errorf("internal: unhandled query node %T", n)
}

func boolExpr(b boolQuery, schema translate.Schema) (translate.Expr, error) {
	var children []translate.Expr

	andGroup := make([]translate.Expr, 0, len(b.must)+len(b.filter))
	for _, c := range append(append([]queryNode{}, b.must...), b.filter...) {
		e, err := buildExpr(c, schema)
		if err != nil {
			return nil, err
		}
		if e != nil {
			andGroup = append(andGroup, e)
		}
	}
	switch len(andGroup) {
	case 0:
	case 1:
		children = append(children, andGroup[0])
	default:
		children = append(children, translate.And{Children: andGroup})
	}

	if len(b.should) > 0 { // fold() guarantees msm == 1 here
		orGroup := make([]translate.Expr, 0, len(b.should))
		for _, c := range b.should {
			e, err := buildExpr(c, schema)
			if err != nil {
				return nil, err
			}
			if e != nil {
				orGroup = append(orGroup, e)
			}
		}
		switch len(orGroup) {
		case 0:
		case 1:
			children = append(children, orGroup[0])
		default:
			children = append(children, translate.Or{Children: orGroup})
		}
	}

	for _, c := range b.mustNot {
		e, err := buildExpr(c, schema)
		if err != nil {
			return nil, err
		}
		if e != nil {
			children = append(children, negated(e, schema))
		}
	}

	switch len(children) {
	case 0:
		return nil, nil
	case 1:
		return children[0], nil
	default:
		return translate.And{Children: children}, nil
	}
}

// negated complements one must_not clause under Milvus's three-valued
// logic. Elasticsearch asks only "did the clause match" — a doc missing the
// field does not match, so NOT must keep it — while `not (...)` over a null
// field evaluates to unknown and the row is silently dropped. Wherever the
// clause can come out unknown (a comparison on a nullable field), the
// complement rides `field is null or not (...)`; the null checks are total
// predicates and swap operators instead. De Morgan carries the recursion
// through and/or, so a nested bool inside must_not composes the same way,
// and a double negation is the identity, unknown included.
func negated(e translate.Expr, schema translate.Schema) translate.Expr {
	switch n := e.(type) {
	case translate.NotNull:
		return translate.IsNull{Field: n.Field}
	case translate.IsNull:
		return translate.NotNull{Field: n.Field}
	case translate.Not:
		return n.Child
	case translate.And:
		if len(n.Children) == 0 { // renders as match-all; its complement is nothing
			return translate.Never{}
		}
		kids := make([]translate.Expr, len(n.Children))
		for i, c := range n.Children {
			kids[i] = negated(c, schema)
		}
		return translate.Or{Children: kids}
	case translate.Or:
		if len(n.Children) == 0 {
			return translate.Never{}
		}
		kids := make([]translate.Expr, len(n.Children))
		for i, c := range n.Children {
			kids[i] = negated(c, schema)
		}
		return translate.And{Children: kids}
	default:
		if f := fieldOf(e); f != "" && nullableField(schema, f) {
			return translate.Or{Children: []translate.Expr{
				translate.IsNull{Field: f},
				translate.Not{Child: e},
			}}
		}
	}
	return translate.Not{Child: e}
}

// fieldOf reports the one field a comparison leaf references, "" for nodes
// without a field (constants and composites).
func fieldOf(e translate.Expr) string {
	switch n := e.(type) {
	case translate.Compare:
		return n.Field
	case translate.InList:
		return n.Field
	case translate.TextMatch:
		return n.Field
	}
	return ""
}

// nullableField asks the schema whether the field admits nulls; a schema
// without the NullableSchema extension reports nothing, keeping the plain
// `not (...)` rendering.
func nullableField(schema translate.Schema, field string) bool {
	ns, ok := schema.(translate.NullableSchema)
	return ok && ns.Nullable(field)
}

// matchExpr maps ES match onto Milvus capabilities: token match on analyzed
// text (TEXT_MATCH, OR across tokens = the ES default operator), exact
// compare elsewhere. Scores stay a constant 1.0 until Phase 5 aligns BM25.
func matchExpr(q matchQuery, schema translate.Schema) (translate.Expr, error) {
	switch schema.FieldType(q.field) {
	case translate.TypeKeyword, translate.TypeNumber, translate.TypeBool:
		v, err := coerceValue(q.field, q.value, schema)
		if err != nil {
			return nil, err
		}
		return translate.Compare{Op: translate.Eq, Field: q.field, Value: v}, nil
	default: // TypeText, TypeUnknown
		text, err := scalarText(q.value)
		if err != nil {
			return nil, err
		}
		tokens := tokenize(text)
		if len(tokens) == 0 {
			return translate.Never{}, nil
		}
		if q.op == "and" {
			items := make([]translate.Expr, len(tokens))
			for i, tok := range tokens {
				items[i] = translate.TextMatch{Field: q.field, Query: tok}
			}
			return translate.And{Children: items}, nil
		}
		// Milvus ORs the tokens within one TEXT_MATCH call — ES's default.
		return translate.TextMatch{Field: q.field, Query: strings.Join(tokens, " ")}, nil
	}
}

// tokenize splits match text into terms. Phase 5 replaces this with real
// analyzer alignment; until then it is whitespace splitting only (punctuation
// stays inside terms — a documented approximation).
func tokenize(s string) []string {
	return strings.FieldsFunc(s, unicode.IsSpace)
}

func scalarText(v any) (string, error) {
	switch x := v.(type) {
	case string:
		return x, nil
	case json.Number:
		return x.String(), nil
	case bool:
		return strconv.FormatBool(x), nil
	}
	return "", parseErrf("expected a scalar match value, got %T", v)
}

// coerceValue applies the coercions ES performs (numeric strings on numeric
// fields, numbers to strings on keyword fields, bools to "true"/"false"
// strings on string fields, bool to 1/0 on numeric fields) and converts date
// strings to epoch millis — the storage encoding chosen in docs/design.md.
// The value enters the AST already in storage encoding; Render adds quoting.
func coerceValue(field string, v any, schema translate.Schema) (translate.Value, error) {
	switch x := v.(type) {
	case json.Number:
		if schema.FieldType(field) == translate.TypeKeyword {
			return translate.StringValue(x.String()), nil // ES coerces numbers to strings on keyword
		}
		if i, err := x.Int64(); err == nil {
			return translate.IntValue(i), nil
		}
		f, err := x.Float64()
		if err != nil {
			return translate.Value{}, illegalErrf("field [%s] is numeric but value [%s] does not parse as a number", field, x)
		}
		return translate.FloatValue(f), nil
	case string:
		switch schema.FieldType(field) {
		case translate.TypeDate:
			return dateValue(field, x)
		case translate.TypeNumber:
			f, err := strconv.ParseFloat(x, 64)
			if err != nil {
				return translate.Value{}, illegalErrf("field [%s] is numeric but value [%s] does not parse as a number", field, x)
			}
			return translate.FloatValue(f), nil
		case translate.TypeBool:
			if x != "true" && x != "false" {
				return translate.Value{}, illegalErrf("field [%s] is boolean but value [%s] is not true/false", field, x)
			}
			return translate.BoolValue(x == "true"), nil
		default:
			return translate.StringValue(x), nil
		}
	case bool:
		switch schema.FieldType(field) {
		case translate.TypeNumber:
			if x {
				return translate.IntValue(1), nil
			}
			return translate.IntValue(0), nil
		case translate.TypeKeyword, translate.TypeText:
			// ES coerces bools to strings on string fields; a bare true in
			// the expression would only ever match a real Bool column
			return translate.StringValue(strconv.FormatBool(x)), nil
		default: // TypeBool, TypeUnknown
			return translate.BoolValue(x), nil
		}
	}
	return translate.Value{}, parseErrf("unsupported value type %T for field [%s]", v, field)
}

var dateLayouts = []string{
	time.RFC3339Nano,      // 2024-01-02T15:04:05(.999)Z07:00
	"2006-01-02T15:04:05", // no zone → UTC
	"2006-01-02",          // date only → midnight UTC
}

func dateValue(field, s string) (translate.Value, error) {
	if strings.Contains(s, "||") {
		return translate.Value{}, unsupportedErrf("date math [%s] on field [%s] is not supported", s, field)
	}
	for _, layout := range dateLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return translate.IntValue(t.UnixMilli()), nil
		}
	}
	return translate.Value{}, illegalErrf("cannot parse date [%s] for field [%s]; supported forms: RFC-3339 or yyyy-MM-dd (epoch-millis numbers pass through)", s, field)
}
