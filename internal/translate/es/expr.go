package es

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"lyrebird/internal/translate"
)

// neverExpr is the constant-false guard. fold() removes every match_none
// before expressions are built, so this is only reached through corner cases
// (e.g. a match whose text tokenizes to nothing — ES zero_terms_query:
// none). Milvus acceptance of a constant comparison gets re-verified when
// the store layer is wired up.
const neverExpr = "1 != 1"

// buildExpr renders the (already folded) query tree as a Milvus boolean
// expression. Empty string means "match everything".
func buildExpr(n queryNode, schema translate.Schema) (string, error) {
	switch q := n.(type) {
	case matchAll:
		return "", nil
	case matchNone:
		return neverExpr, nil
	case boolQuery:
		return boolExpr(q, schema)
	case termQuery:
		v, err := renderValue(q.field, q.value, schema)
		if err != nil {
			return "", err
		}
		return q.field + " == " + v, nil
	case termsQuery:
		vals := make([]string, len(q.values))
		for i, v := range q.values {
			var err error
			if vals[i], err = renderValue(q.field, v, schema); err != nil {
				return "", err
			}
		}
		return q.field + " in [" + strings.Join(vals, ", ") + "]", nil
	case matchQuery:
		return matchExpr(q, schema)
	case rangeQuery:
		items := make([]string, len(q.bounds))
		for i, b := range q.bounds {
			v, err := renderValue(q.field, b.value, schema)
			if err != nil {
				return "", err
			}
			items[i] = q.field + " " + b.op + " " + v
		}
		return joinExpr(items, " and "), nil
	case existsQuery:
		// nullable-field syntax; re-verify at store integration
		return q.field + " is not null", nil
	}
	return "", fmt.Errorf("internal: unhandled query node %T", n)
}

func boolExpr(b boolQuery, schema translate.Schema) (string, error) {
	andItems := make([]string, 0, len(b.must)+len(b.filter))
	for _, c := range append(append([]queryNode{}, b.must...), b.filter...) {
		e, err := buildExpr(c, schema)
		if err != nil {
			return "", err
		}
		andItems = append(andItems, e)
	}
	notItems := make([]string, 0, len(b.mustNot))
	for _, c := range b.mustNot {
		e, err := buildExpr(c, schema)
		if err != nil {
			return "", err
		}
		notItems = append(notItems, "not ("+e+")")
	}

	var parts []string
	if p := joinExpr(andItems, " and "); p != "" {
		parts = append(parts, p)
	}
	if len(b.should) > 0 { // fold() guarantees msm == 1 here
		items := make([]string, len(b.should))
		for i, c := range b.should {
			e, err := buildExpr(c, schema)
			if err != nil {
				return "", err
			}
			items[i] = e
		}
		parts = append(parts, joinExpr(items, " or "))
	}
	if p := joinExpr(notItems, " and "); p != "" {
		parts = append(parts, p)
	}
	return joinExpr(parts, " and "), nil
}

// joinExpr combines sub-expressions; every member of a multi-item join is
// parenthesized so nested and/or groups cannot change meaning, while a
// single item passes through unwrapped to keep simple queries readable.
func joinExpr(items []string, op string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	}
	wrapped := make([]string, len(items))
	for i, s := range items {
		wrapped[i] = "(" + s + ")"
	}
	return strings.Join(wrapped, op)
}

// matchExpr maps ES match onto Milvus capabilities: token match on analyzed
// text (TEXT_MATCH, OR across tokens = the ES default operator), exact
// compare elsewhere. Scores stay a constant 1.0 until Phase 5 aligns BM25.
func matchExpr(q matchQuery, schema translate.Schema) (string, error) {
	switch schema.FieldType(q.field) {
	case translate.TypeKeyword, translate.TypeNumber, translate.TypeBool:
		v, err := renderValue(q.field, q.value, schema)
		if err != nil {
			return "", err
		}
		return q.field + " == " + v, nil
	default: // TypeText, TypeUnknown
		text, err := scalarText(q.value)
		if err != nil {
			return "", err
		}
		tokens := tokenize(text)
		if len(tokens) == 0 {
			return neverExpr, nil
		}
		if q.op == "and" {
			items := make([]string, len(tokens))
			for i, tok := range tokens {
				items[i] = "TEXT_MATCH(" + q.field + ", " + quote(tok) + ")"
			}
			return joinExpr(items, " and "), nil
		}
		// Milvus ORs the tokens within one TEXT_MATCH call — ES's default.
		return "TEXT_MATCH(" + q.field + ", " + quote(strings.Join(tokens, " ")) + ")", nil
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
		if x {
			return "true", nil
		}
		return "false", nil
	}
	return "", parseErrf("expected a scalar match value, got %T", v)
}

// renderValue formats a query literal for its field's Milvus type, applying
// the coercions ES performs (numeric strings on numeric fields, bool ↔ 1/0)
// and converting date strings to epoch millis — the storage encoding chosen
// in docs/design.md.
func renderValue(field string, v any, schema translate.Schema) (string, error) {
	switch x := v.(type) {
	case json.Number:
		if schema.FieldType(field) == translate.TypeKeyword {
			return quote(x.String()), nil // ES coerces numbers to strings on keyword
		}
		return x.String(), nil
	case string:
		switch schema.FieldType(field) {
		case translate.TypeDate:
			return renderDate(field, x)
		case translate.TypeNumber:
			f, err := strconv.ParseFloat(x, 64)
			if err != nil {
				return "", illegalErrf("field [%s] is numeric but value [%s] does not parse as a number", field, x)
			}
			return strconv.FormatFloat(f, 'f', -1, 64), nil
		case translate.TypeBool:
			if x == "true" || x == "false" {
				return x, nil
			}
			return "", illegalErrf("field [%s] is boolean but value [%s] is not true/false", field, x)
		default:
			return quote(x), nil
		}
	case bool:
		if schema.FieldType(field) == translate.TypeNumber {
			if x {
				return "1", nil
			}
			return "0", nil
		}
		if x {
			return "true", nil
		}
		return "false", nil
	}
	return "", parseErrf("unsupported value type %T for field [%s]", v, field)
}

var dateLayouts = []string{
	time.RFC3339Nano,      // 2024-01-02T15:04:05(.999)Z07:00
	"2006-01-02T15:04:05", // no zone → UTC
	"2006-01-02",          // date only → midnight UTC
}

func renderDate(field, s string) (string, error) {
	if strings.Contains(s, "||") {
		return "", unsupportedErrf("date math [%s] on field [%s] is not supported", s, field)
	}
	for _, layout := range dateLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return strconv.FormatInt(t.UnixMilli(), 10), nil
		}
	}
	return "", illegalErrf("cannot parse date [%s] for field [%s]; supported forms: RFC-3339 or yyyy-MM-dd (epoch-millis numbers pass through)", s, field)
}

var exprEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`)

func quote(s string) string { return `"` + exprEscaper.Replace(s) + `"` }
