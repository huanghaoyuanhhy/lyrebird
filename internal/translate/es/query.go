package es

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// The query IR mirrors the ES DSL one-to-one; buildExpr renders it as a
// Milvus boolean expression and fold() removes boolean identities first so
// rendering never meets them.
type queryNode interface{ isQuery() }

type (
	matchAll  struct{}
	matchNone struct{}
	boolQuery struct {
		must, filter, should, mustNot []queryNode
		minShouldMatch                int
		minShouldMatchSet             bool
	}
	termQuery struct {
		field string
		value any
	}
	termsQuery struct {
		field  string
		values []any
	}
	// matchQuery keeps the raw scalar; encoding depends on the field type
	// (TEXT_MATCH on analyzed text, exact compare elsewhere).
	matchQuery struct {
		field string
		value any
		op    string // "or" | "and"
	}
	rangeQuery struct {
		field  string
		bounds []rangeBound
	}
	rangeBound struct {
		op    string // ">=" ">" "<=" "<"
		value any
	}
	existsQuery struct{ field string }
)

func (matchAll) isQuery()    {}
func (matchNone) isQuery()   {}
func (boolQuery) isQuery()   {}
func (termQuery) isQuery()   {}
func (termsQuery) isQuery()  {}
func (matchQuery) isQuery()  {}
func (rangeQuery) isQuery()  {}
func (existsQuery) isQuery() {}

func isMatchNone(n queryNode) bool { _, ok := n.(matchNone); return ok }
func isMatchAll(n queryNode) bool  { _, ok := n.(matchAll); return ok }

func containsWhere(ns []queryNode, pred func(queryNode) bool) bool {
	for _, n := range ns {
		if pred(n) {
			return true
		}
	}
	return false
}

func dropWhere(ns []queryNode, pred func(queryNode) bool) []queryNode {
	out := make([]queryNode, 0, len(ns))
	for _, n := range ns {
		if !pred(n) {
			out = append(out, n)
		}
	}
	return out
}

func parseQueryNode(raw json.RawMessage) (queryNode, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return matchAll{}, nil // ES: a missing query matches everything
	}
	obj, err := expectObject(raw, "query")
	if err != nil {
		return nil, err
	}
	if len(obj) == 0 {
		return matchAll{}, nil
	}
	if len(obj) > 1 {
		return nil, parseErrf("[query] must contain exactly one clause, got %d", len(obj))
	}
	for name, body := range obj {
		return parseQueryClause(name, body)
	}
	panic("unreachable: single-key map")
}

func parseQueryClause(name string, body json.RawMessage) (queryNode, error) {
	switch name {
	case "match_all":
		if _, err := expectObject(body, "match_all"); err != nil {
			return nil, err
		}
		return matchAll{}, nil
	case "match_none":
		if _, err := expectObject(body, "match_none"); err != nil {
			return nil, err
		}
		return matchNone{}, nil
	case "bool":
		return parseBool(body)
	case "term":
		return parseTerm(body)
	case "terms":
		return parseTerms(body)
	case "match":
		return parseMatch(body)
	case "range":
		return parseRange(body)
	case "exists":
		return parseExists(body)
	default:
		// Known ES queries (multi_match, query_string, ...) are valid DSL
		// beyond the Phase 1 surface — unsupported_exception, not a parse
		// error. Truly unknown names stay parsing exceptions.
		if knownQueryNames[name] {
			return nil, unsupportedErrf("[%s] query is beyond lyrebird's Phase 1 surface", name)
		}
		return nil, parseErrf("unknown query [%s]", name)
	}
}

// knownQueryNames covers the rest of ES's query family so valid DSL gets the
// "beyond Phase 1" classification while typos still read as parse errors.
var knownQueryNames = map[string]bool{
	"multi_match": true, "query_string": true, "simple_query_string": true,
	"match_phrase": true, "match_phrase_prefix": true, "match_bool_prefix": true,
	"prefix": true, "wildcard": true, "regexp": true, "fuzzy": true,
	"ids": true, "constant_score": true, "dis_max": true, "boosting": true,
	"function_score": true, "nested": true, "geo_distance": true,
	"geo_bounding_box": true, "more_like_this": true, "percolate": true,
	"rank_feature": true, "distance_feature": true, "pinned": true,
	"knn": true, "combined_fields": true, "intervals": true, "span_near": true,
}

func parseBool(body json.RawMessage) (queryNode, error) {
	obj, err := expectObject(body, "bool")
	if err != nil {
		return nil, err
	}
	b := boolQuery{}
	for k, v := range obj {
		switch k {
		case "must", "filter", "should", "must_not":
			clauses, err := parseClauseList(v, k)
			if err != nil {
				return nil, err
			}
			switch k {
			case "must":
				b.must = clauses
			case "filter":
				b.filter = clauses
			case "should":
				b.should = clauses
			case "must_not":
				b.mustNot = clauses
			}
		case "minimum_should_match":
			raw := bytes.TrimSpace(v)
			if len(raw) > 0 && raw[0] == '"' {
				return nil, unsupportedErrf("[bool] minimum_should_match percentage form is not supported")
			}
			n, err := decodeInt(v)
			if err != nil || n < 0 {
				return nil, parseErrf("[bool] minimum_should_match must be a non-negative integer")
			}
			b.minShouldMatch, b.minShouldMatchSet = n, true
		case "boost", "_name", "adjust_pure_negative":
			// no scoring in Phase 1; cosmetic
		default:
			return nil, parseErrf("[bool] unknown parameter [%s]", k)
		}
	}
	return b, nil
}

// parseClauseList accepts the ES singular-object or array-of-objects forms.
func parseClauseList(raw json.RawMessage, what string) ([]queryNode, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '[' {
		// wrap any single JSON value into an array — always valid JSON
		raw = []byte("[" + string(raw) + "]")
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, parseErrf("[bool] %s must be a query object or an array of queries", what)
	}
	out := make([]queryNode, 0, len(items))
	for _, item := range items {
		q, err := parseQueryNode(item)
		if err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, nil
}

func parseTerm(body json.RawMessage) (queryNode, error) {
	field, spec, err := expectSingleField(body, "term")
	if err != nil {
		return nil, err
	}
	spec = bytes.TrimSpace(spec)
	if spec[0] != '{' {
		value, err := decodeScalar(spec, "[term] value")
		if err != nil {
			return nil, err
		}
		return termQuery{field: field, value: value}, nil
	}
	obj, err := expectObject(spec, "term")
	if err != nil {
		return nil, err
	}
	var value any
	found := false
	for k, v := range obj {
		switch k {
		case "value":
			if value, err = decodeScalar(v, "[term] value"); err != nil {
				return nil, err
			}
			found = true
		case "boost", "_name":
			// no scoring in Phase 1; cosmetic
		case "case_insensitive":
			return nil, unsupportedErrf("[term] case_insensitive is not supported (ES applies the keyword normalizer)")
		default:
			return nil, parseErrf("[term] unknown parameter [%s]", k)
		}
	}
	if !found {
		return nil, parseErrf("[term] query requires a [value]")
	}
	return termQuery{field: field, value: value}, nil
}

func parseTerms(body json.RawMessage) (queryNode, error) {
	field, spec, err := expectSingleField(body, "terms")
	if err != nil {
		return nil, err
	}
	spec = bytes.TrimSpace(spec)
	if len(spec) > 0 && spec[0] == '{' {
		return nil, unsupportedErrf("[terms] lookup by index/id is not supported")
	}
	var raws []json.RawMessage
	if err := json.Unmarshal(spec, &raws); err != nil {
		return nil, parseErrf("[terms] expects an array of values")
	}
	values := make([]any, 0, len(raws))
	for i, r := range raws {
		v, err := decodeScalar(r, fmt.Sprintf("[terms] value %d", i))
		if err != nil {
			return nil, err
		}
		values = append(values, v)
	}
	if len(values) == 0 {
		return matchNone{}, nil // ES: an empty terms list matches nothing
	}
	return termsQuery{field: field, values: values}, nil
}

func parseMatch(body json.RawMessage) (queryNode, error) {
	field, spec, err := expectSingleField(body, "match")
	if err != nil {
		return nil, err
	}
	q := matchQuery{field: field, op: "or"}
	spec = bytes.TrimSpace(spec)
	if spec[0] != '{' {
		if q.value, err = decodeScalar(spec, "[match] query"); err != nil {
			return nil, err
		}
		return q, nil
	}
	obj, err := expectObject(spec, "match")
	if err != nil {
		return nil, err
	}
	found := false
	for k, v := range obj {
		switch k {
		case "query":
			if q.value, err = decodeScalar(v, "[match] query"); err != nil {
				return nil, err
			}
			found = true
		case "operator":
			var op string
			if err := json.Unmarshal(v, &op); err != nil || (op != "and" && op != "or") {
				return nil, parseErrf(`[match] operator must be "and" or "or"`)
			}
			q.op = op
		case "boost", "_name":
			// no scoring in Phase 1; cosmetic
		default:
			// fuzziness, analyzer, minimum_should_match, ... — none fit the
			// Phase 1 TEXT_MATCH mapping
			return nil, unsupportedErrf("[match] parameter [%s] is not supported", k)
		}
	}
	if !found {
		return nil, parseErrf("[match] query requires a [query] parameter")
	}
	return q, nil
}

func parseRange(body json.RawMessage) (queryNode, error) {
	field, spec, err := expectSingleField(body, "range")
	if err != nil {
		return nil, err
	}
	obj, err := expectObject(spec, "range")
	if err != nil {
		return nil, err
	}
	for k := range obj {
		switch k {
		case "gt", "gte", "lt", "lte", "boost", "_name":
		case "format", "time_zone", "relation":
			return nil, unsupportedErrf("[range] parameter [%s] is not supported", k)
		default:
			return nil, parseErrf("[range] unknown parameter [%s]", k)
		}
	}
	rq := rangeQuery{field: field}
	// canonical bound order keeps the emitted expression deterministic
	for _, bound := range []struct{ key, symbol string }{
		{"gt", ">"}, {"gte", ">="}, {"lt", "<"}, {"lte", "<="},
	} {
		raw, ok := obj[bound.key]
		if !ok {
			continue
		}
		v, err := decodeScalar(raw, fmt.Sprintf("[range] %s", bound.key))
		if err != nil {
			return nil, err
		}
		rq.bounds = append(rq.bounds, rangeBound{op: bound.symbol, value: v})
	}
	if len(rq.bounds) == 0 {
		return nil, parseErrf("[range] query requires at least one bound (gt, gte, lt, lte)")
	}
	return rq, nil
}

func parseExists(body json.RawMessage) (queryNode, error) {
	obj, err := expectObject(body, "exists")
	if err != nil {
		return nil, err
	}
	field := ""
	for k, v := range obj {
		switch k {
		case "field":
			if err := json.Unmarshal(v, &field); err != nil {
				return nil, parseErrf("[exists] field must be a string")
			}
		case "boost", "_name":
			// no scoring in Phase 1; cosmetic
		default:
			return nil, parseErrf("[exists] unknown parameter [%s]", k)
		}
	}
	if field == "" {
		return nil, parseErrf("[exists] query requires a [field]")
	}
	return existsQuery{field: field}, nil
}

// fold applies the boolean identities buildExpr relies on, bottom-up:
//   - match_none anywhere in must/filter kills the bool;
//   - match_all in must_not kills it too (NOT everything = nothing);
//   - match_all leaves in must/filter drop out;
//   - a match_all in should satisfies minimum_should_match outright;
//   - minimum_should_match above the surviving should count matches nothing.
func fold(n queryNode) (queryNode, error) {
	b, ok := n.(boolQuery)
	if !ok {
		return n, nil
	}
	groups := []*[]queryNode{&b.must, &b.filter, &b.should, &b.mustNot}
	for _, g := range groups {
		for i, c := range *g {
			child, err := fold(c)
			if err != nil {
				return nil, err
			}
			(*g)[i] = child
		}
	}

	if containsWhere(b.must, isMatchNone) || containsWhere(b.filter, isMatchNone) ||
		containsWhere(b.mustNot, isMatchAll) {
		return matchNone{}, nil
	}
	b.must = dropWhere(b.must, isMatchAll)
	b.filter = dropWhere(b.filter, isMatchAll)
	b.mustNot = dropWhere(b.mustNot, isMatchNone)

	// effective minimum_should_match; ES default is 1 when should is the
	// only constraint present, 0 otherwise
	msm := 0
	if b.minShouldMatchSet {
		msm = b.minShouldMatch
	} else if len(b.must) == 0 && len(b.filter) == 0 {
		msm = 1
	}

	if containsWhere(b.should, isMatchAll) && msm >= 1 {
		b.should = nil // the OR block is satisfied unconditionally
	} else {
		hadNone := containsWhere(b.should, isMatchNone)
		b.should = dropWhere(b.should, isMatchNone)
		switch {
		case len(b.should) > 0 && msm == 0:
			// should clauses are optional boost-only matches in Phase 1
			b.should = nil
		case len(b.should) > 0 && msm > len(b.should):
			return matchNone{}, nil
		case len(b.should) > 0 && msm > 1:
			return nil, unsupportedErrf("[bool] minimum_should_match > 1 is not supported yet (needs k-of-n matching)")
		case len(b.should) == 0 && hadNone && msm >= 1:
			// every should clause was impossible, so msm can never be met —
			// even alongside must/filter, whose match cannot rescue the OR
			return matchNone{}, nil
		}
	}

	if len(b.must) == 0 && len(b.filter) == 0 && len(b.should) == 0 && len(b.mustNot) == 0 {
		return matchAll{}, nil
	}
	return b, nil
}

func expectObject(raw json.RawMessage, what string) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var obj map[string]json.RawMessage
	if err := dec.Decode(&obj); err != nil || obj == nil {
		return nil, parseErrf("[%s] must be a JSON object", what)
	}
	return obj, nil
}

// expectSingleField handles the ES leaf-query shape {"term": {"field": spec}}.
func expectSingleField(body json.RawMessage, query string) (string, json.RawMessage, error) {
	obj, err := expectObject(body, query)
	if err != nil {
		return "", nil, err
	}
	if len(obj) == 0 {
		return "", nil, parseErrf("[%s] query requires a field", query)
	}
	if len(obj) > 1 {
		return "", nil, parseErrf("[%s] query does not support multiple fields, got %d", query, len(obj))
	}
	for field, spec := range obj {
		return field, spec, nil
	}
	panic("unreachable: single-key map")
}

// decodeScalar returns a string, json.Number or bool; null, containers and
// anything unparsable are request errors.
func decodeScalar(raw json.RawMessage, what string) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, parseErrf("%s: invalid JSON value", what)
	}
	switch v.(type) {
	case string, json.Number, bool:
		return v, nil
	case nil:
		return nil, parseErrf("%s must not be null", what)
	default:
		return nil, parseErrf("%s must be a scalar (string, number or boolean)", what)
	}
}
