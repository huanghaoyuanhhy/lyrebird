package es

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"lyrebird/internal/translate"
)

const (
	defaultSize     = 10 // a bare ES _search returns 10 hits
	maxResultWindow = 10000
)

// unsupportedFeatures lists top-level _search body members that change the
// result set or response shape in ways Phase 1 cannot honor. Failing fast
// with a named member beats silently returning a wrong-shaped response.
var unsupportedFeatures = []string{
	"aggs", "aggregations", "knn", "suggest", "collapse", "rescore",
	"post_filter", "search_after", "scroll", "pit", "slice", "min_score",
	"indices_boost", "script_fields", "runtime_mappings", "highlight",
	"stored_fields", "docvalue_fields", "fields", "terminate_after",
}

func parseErrf(format string, args ...any) *translate.Error {
	return &translate.Error{Type: "parsing_exception", Status: 400, Reason: fmt.Sprintf(format, args...)}
}

func illegalErrf(format string, args ...any) *translate.Error {
	return &translate.Error{Type: "illegal_argument_exception", Status: 400, Reason: fmt.Sprintf(format, args...)}
}

func unsupportedErrf(format string, args ...any) *translate.Error {
	return &translate.Error{Type: "unsupported_exception", Status: 400, Reason: fmt.Sprintf(format, args...)}
}

// Translate compiles an ES _search request body into a store-executable plan.
// It is a pure function: (request body, schema) → plan. A nil schema treats
// every field as unknown — enough for Phase 1, replaced by the schema catalog
// in Phase 3.
//
// Supported queries: match_all, match_none, bool (must/filter/should/must_not,
// minimum_should_match), term, terms, match, range, exists. Supported body
// members: from, size, sort, _source. Anything else that would change results
// or response shape fails fast as unsupported_exception naming the member.
func Translate(body []byte, schema translate.Schema) (*translate.Plan, error) {
	if schema == nil {
		schema = translate.MapSchema{}
	}
	top, err := decodeBody(body)
	if err != nil {
		return nil, err
	}
	for _, feat := range unsupportedFeatures {
		if _, ok := top[feat]; ok {
			return nil, unsupportedErrf("[%s] is beyond lyrebird's Phase 1 surface; drop it or query Milvus directly", feat)
		}
	}

	from, size, err := parsePaging(top)
	if err != nil {
		return nil, err
	}
	sorts, err := parseSort(top["sort"])
	if err != nil {
		return nil, err
	}
	source, err := parseSource(top["_source"])
	if err != nil {
		return nil, err
	}

	root, err := parseQueryNode(top["query"])
	if err != nil {
		return nil, err
	}
	root, err = fold(root)
	if err != nil {
		return nil, err
	}

	plan := &translate.Plan{Offset: from, Limit: size, Sort: sorts, Source: source}
	if isMatchNone(root) {
		plan.NoMatch = true
		return plan, nil
	}
	plan.Expr, err = buildExpr(root, schema)
	if err != nil {
		return nil, err
	}
	return plan, nil
}

func decodeBody(body []byte) (map[string]json.RawMessage, error) {
	raw := bytes.TrimSpace(body)
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var top map[string]json.RawMessage
	if err := dec.Decode(&top); err != nil {
		return nil, parseErrf("request body is not valid JSON: %v", err)
	}
	if top == nil { // body was literally `null`
		top = map[string]json.RawMessage{}
	}
	return top, nil
}

func parsePaging(top map[string]json.RawMessage) (from, size int, err error) {
	from, size = 0, defaultSize
	if raw, ok := top["from"]; ok {
		n, decErr := decodeInt(raw)
		if decErr != nil || n < 0 {
			return 0, 0, illegalErrf("[from] must be a non-negative integer, got %s", bytes.TrimSpace(raw))
		}
		from = n
	}
	if raw, ok := top["size"]; ok {
		n, decErr := decodeInt(raw)
		if decErr != nil || n < 0 {
			return 0, 0, illegalErrf("[size] must be a non-negative integer, got %s", bytes.TrimSpace(raw))
		}
		size = n
	}
	if from+size > maxResultWindow {
		return 0, 0, illegalErrf("Result window is too large, from + size must be less than or equal to: [%d] but was [%d]. See the max_result_window parameter; search_after is not available in lyrebird yet.", maxResultWindow, from+size)
	}
	return from, size, nil
}

func decodeInt(raw json.RawMessage) (int, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return 0, err
	}
	n, ok := v.(json.Number)
	if !ok {
		return 0, fmt.Errorf("not a number")
	}
	i, err := n.Int64()
	if err != nil {
		return 0, err
	}
	return int(i), nil
}

// parseSort accepts the ES sort shorthand forms:
//
//	"field"                       → asc
//	["a", {"b": "desc"}]
//	{"b": {"order": "desc"}}
//
// Sorting on _score / _doc is dropped: Phase 1 scores are constant and Milvus
// has no stable index order to preserve, so any deterministic order equals no
// order at all.
func parseSort(raw json.RawMessage) ([]translate.SortClause, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	items := []json.RawMessage{raw}
	if raw[0] == '[' {
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, parseErrf("[sort] must be an array or a single sort clause: %v", err)
		}
	}
	var out []translate.SortClause
	for _, item := range items {
		item = bytes.TrimSpace(item)
		switch {
		case item[0] == '"':
			var field string
			if err := json.Unmarshal(item, &field); err != nil {
				return nil, parseErrf("[sort] invalid field name")
			}
			clause, keep, err := sortClause(field, "asc")
			if err != nil {
				return nil, err
			}
			if keep {
				out = append(out, clause)
			}
		case item[0] == '{':
			clause, keep, err := parseSortObject(item)
			if err != nil {
				return nil, err
			}
			if keep {
				out = append(out, clause)
			}
		default:
			return nil, parseErrf("[sort] clauses must be strings or objects")
		}
	}
	return out, nil
}

func parseSortObject(item json.RawMessage) (translate.SortClause, bool, error) {
	obj, err := expectObject(item, "sort")
	if err != nil {
		return translate.SortClause{}, false, err
	}
	if len(obj) != 1 {
		return translate.SortClause{}, false, parseErrf("[sort] each clause must target exactly one field, got %d", len(obj))
	}
	for field, spec := range obj {
		order := "asc"
		spec = bytes.TrimSpace(spec)
		switch {
		case spec[0] == '"':
			if err := json.Unmarshal(spec, &order); err != nil {
				return translate.SortClause{}, false, parseErrf(`[sort] order for [%s] must be "asc" or "desc"`, field)
			}
		case spec[0] == '{':
			opts, err := expectObject(spec, "sort")
			if err != nil {
				return translate.SortClause{}, false, err
			}
			for k, v := range opts {
				switch k {
				case "order":
					if err := json.Unmarshal(v, &order); err != nil || (order != "asc" && order != "desc") {
						return translate.SortClause{}, false, parseErrf(`[sort] order must be "asc" or "desc"`)
					}
				case "missing":
					// _last is the ES default for both directions; anything
					// else needs null-aware sorting, deferred to the store.
					var missing string
					_ = json.Unmarshal(v, &missing)
					if missing != "_last" {
						return translate.SortClause{}, false, unsupportedErrf("[sort] missing=%s is not supported (null-aware ordering)", missing)
					}
				case "_name":
				default:
					return translate.SortClause{}, false, unsupportedErrf("[sort] option [%s] is not supported", k)
				}
			}
		default:
			return translate.SortClause{}, false, parseErrf("[sort] value for [%s] must be \"asc\"/\"desc\" or an options object", field)
		}
		return sortClause(field, order)
	}
	panic("unreachable: single-key map")
}

func sortClause(field, order string) (translate.SortClause, bool, error) {
	switch field {
	case "_score":
		return translate.SortClause{}, false, nil
	case "_doc":
		return translate.SortClause{}, false, nil
	case "_geo_distance", "_script":
		return translate.SortClause{}, false, unsupportedErrf("[sort] %s is not supported", field)
	}
	if order != "asc" && order != "desc" {
		return translate.SortClause{}, false, parseErrf(`[sort] order must be "asc" or "desc", got %q`, order)
	}
	return translate.SortClause{Field: field, Desc: order == "desc"}, true, nil
}

func parseSource(raw json.RawMessage) (translate.SourceFilter, error) {
	src := translate.SourceFilter{FetchSource: true}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" || string(raw) == "true" {
		return src, nil
	}
	if string(raw) == "false" {
		src.FetchSource = false
		return src, nil
	}
	switch raw[0] {
	case '"':
		var one string
		if err := json.Unmarshal(raw, &one); err != nil {
			return src, parseErrf("[_source] invalid string form")
		}
		src.Includes = []string{one}
	case '[':
		var list []string
		if err := json.Unmarshal(raw, &list); err != nil {
			return src, parseErrf("[_source] array form must contain field names")
		}
		src.Includes = list
	case '{':
		obj, err := expectObject(raw, "_source")
		if err != nil {
			return src, err
		}
		var listErr error
		for k, v := range obj {
			var list []string
			switch k {
			case "includes", "include":
				if list, listErr = sourceList(v); listErr != nil {
					return src, listErr
				}
				src.Includes = list
			case "excludes", "exclude":
				if list, listErr = sourceList(v); listErr != nil {
					return src, listErr
				}
				src.Excludes = list
			default:
				return src, unsupportedErrf("[_source] option [%s] is not supported", k)
			}
		}
	default:
		return src, parseErrf("[_source] must be a boolean, string, array or object")
	}
	return filterSourcePatterns(src)
}

func sourceList(raw json.RawMessage) ([]string, error) {
	raw = bytes.TrimSpace(raw)
	if raw[0] == '"' {
		var one string
		if err := json.Unmarshal(raw, &one); err != nil {
			return nil, parseErrf("[_source] invalid pattern")
		}
		return []string{one}, nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, parseErrf("[_source] must be a string or an array of strings")
	}
	return list, nil
}

// filterSourcePatterns narrows wildcard support to what Phase 1 can honor
// exactly: "*" (all fields, same as absent) and "prefix.*". Anything else
// errors out instead of quietly returning wrong fields.
func filterSourcePatterns(src translate.SourceFilter) (translate.SourceFilter, error) {
	clean := func(patterns []string, what string) ([]string, error) {
		var out []string // stays nil when everything drops, so plans compare equal
		for _, p := range patterns {
			switch {
			case p == "*":
				// all fields — same as omitting the pattern
			case strings.HasSuffix(p, ".*") && !strings.ContainsAny(strings.TrimSuffix(p, ".*"), "*?"):
				out = append(out, p)
			case strings.ContainsAny(p, "*?"):
				return nil, unsupportedErrf("[_source] pattern [%s] in %s is not supported: only \"*\" and \"prefix.*\"", p, what)
			default:
				out = append(out, p)
			}
		}
		return out, nil
	}
	var err error
	if src.Includes, err = clean(src.Includes, "includes"); err != nil {
		return src, err
	}
	if src.Excludes, err = clean(src.Excludes, "excludes"); err != nil {
		return src, err
	}
	return src, nil
}
