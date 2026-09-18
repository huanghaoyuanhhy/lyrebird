package es

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/huanghaoyuanhhy/lyrebird/internal/translate"
)

var testSchema = translate.MapSchema{
	"title":   translate.TypeText,
	"status":  translate.TypeKeyword,
	"views":   translate.TypeNumber,
	"active":  translate.TypeBool,
	"created": translate.TypeDate,
}

// basePlan is the ES-default plan without a filter; the expected filter of a
// case goes into its expr field, compared against Render(plan.Expr).
func basePlan() *translate.Plan {
	return &translate.Plan{Limit: 10, Source: translate.SourceFilter{FetchSource: true}}
}

func baseNoMatch() *translate.Plan {
	p := basePlan()
	p.NoMatch = true
	return p
}

// knnBase is the plan a minimal knn clause produces: ES defaults (limit 10,
// source on) plus the vector search. The expected filter of a case rides the
// expr field, same as the query-plan cases.
func knnBase() *translate.Plan {
	p := basePlan()
	p.Search = &translate.SearchSpec{Field: "emb", Vector: []float32{0.1, 0.2, 0.3}}
	return p
}

func TestTranslate(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		schema translate.Schema
		want   *translate.Plan
		expr   string // expected Render(plan.Expr); "" = match everything
	}{
		{
			name: "empty body is match_all with ES defaults",
			body: ``,
			want: basePlan(),
		},
		{
			name: "explicit match_all",
			body: `{"query": {"match_all": {}}}`,
			want: basePlan(),
		},
		{
			name: "term string shorthand",
			body: `{"query": {"term": {"status": "ok"}}}`,
			want: basePlan(),
			expr: `status == "ok"`,
		},
		{
			name: "term object form with value",
			body: `{"query": {"term": {"status": {"value": "ok"}}}}`,
			want: basePlan(),
			expr: `status == "ok"`,
		},
		{
			name: "term number",
			body: `{"query": {"term": {"views": 3}}}`,
			want: basePlan(),
			expr: `views == 3`,
		},
		{
			name: "term bool",
			body: `{"query": {"term": {"active": true}}}`,
			want: basePlan(),
			expr: `active == true`,
		},
		{
			name:   "bool 1/0 coerced on numeric field",
			body:   `{"query": {"term": {"views": true}}}`,
			schema: testSchema,
			want:   basePlan(),
			expr:   `views == 1`,
		},
		{
			name:   "bool coerced to string on keyword field",
			body:   `{"query": {"term": {"status": true}}}`,
			schema: testSchema,
			want:   basePlan(),
			expr:   `status == "true"`,
		},
		{
			name:   "number quoted on keyword field",
			body:   `{"query": {"term": {"status": 42}}}`,
			schema: testSchema,
			want:   basePlan(),
			expr:   `status == "42"`,
		},
		{
			name: "terms",
			body: `{"query": {"terms": {"status": ["a", "b"]}}}`,
			want: basePlan(),
			expr: `status in ["a", "b"]`,
		},
		{
			name: "empty terms matches nothing",
			body: `{"query": {"terms": {"status": []}}}`,
			want: baseNoMatch(),
		},
		{
			name:   "match on text is TEXT_MATCH",
			body:   `{"query": {"match": {"title": "quick brown"}}}`,
			schema: testSchema,
			want:   basePlan(),
			expr:   `TEXT_MATCH(title, "quick brown")`,
		},
		{
			name: "match defaults to TEXT_MATCH without schema",
			body: `{"query": {"match": {"title": "quick"}}}`,
			want: basePlan(),
			expr: `TEXT_MATCH(title, "quick")`,
		},
		{
			name:   "match on keyword is exact",
			body:   `{"query": {"match": {"status": "live"}}}`,
			schema: testSchema,
			want:   basePlan(),
			expr:   `status == "live"`,
		},
		{
			name:   "match and-operator ANDs per token",
			body:   `{"query": {"match": {"title": {"query": "quick brown", "operator": "and"}}}}`,
			schema: testSchema,
			want:   basePlan(),
			expr:   `(TEXT_MATCH(title, "quick")) and (TEXT_MATCH(title, "brown"))`,
		},
		{
			name:   "match coerces numeric string on number field",
			body:   `{"query": {"match": {"views": "5"}}}`,
			schema: testSchema,
			want:   basePlan(),
			expr:   `views == 5`,
		},
		{
			name:   "range with two bounds",
			body:   `{"query": {"range": {"views": {"gte": 10, "lt": 100}}}}`,
			schema: testSchema,
			want:   basePlan(),
			expr:   `(views >= 10) and (views < 100)`,
		},
		{
			name: "range single bound",
			body: `{"query": {"range": {"views": {"gt": 3}}}}`,
			want: basePlan(),
			expr: `views > 3`,
		},
		{
			name:   "range date string becomes epoch millis",
			body:   `{"query": {"range": {"created": {"gte": "2024-01-02"}}}}`,
			schema: testSchema,
			want:   basePlan(),
			expr:   `created >= 1704153600000`,
		},
		{
			name:   "range RFC-3339 date with time",
			body:   `{"query": {"range": {"created": {"lt": "2024-06-01T12:30:00Z"}}}}`,
			schema: testSchema,
			want:   basePlan(),
			expr:   `created < 1717245000000`,
		},
		{
			name: "exists",
			body: `{"query": {"exists": {"field": "status"}}}`,
			want: basePlan(),
			expr: `status is not null`,
		},
		{
			name: "bool combines must, filter and must_not",
			body: `{"query": {"bool": {"must": [{"term": {"status": "ok"}}], "filter": [{"range": {"views": {"gte": 1}}}], "must_not": [{"term": {"status": "deleted"}}]}}}`,
			want: basePlan(),
			expr: `((status == "ok") and (views >= 1)) and (not (status == "deleted"))`,
		},
		{
			name: "must_not alone",
			body: `{"query": {"bool": {"must_not": [{"term": {"status": "deleted"}}]}}}`,
			want: basePlan(),
			expr: `not (status == "deleted")`,
		},
		{
			name: "should-only defaults to or (implicit msm 1)",
			body: `{"query": {"bool": {"should": [{"term": {"status": "a"}}, {"term": {"status": "b"}}]}}}`,
			want: basePlan(),
			expr: `(status == "a") or (status == "b")`,
		},
		{
			name: "should next to must is optional and drops out",
			body: `{"query": {"bool": {"must": [{"term": {"views": 1}}], "should": [{"term": {"status": "a"}}]}}}`,
			want: basePlan(),
			expr: `views == 1`,
		},
		{
			name: "nested bool keeps the or-group intact under and",
			body: `{"query": {"bool": {"must": [{"bool": {"should": [{"term": {"status": "a"}}, {"term": {"status": "b"}}]}}, {"term": {"views": 2}}]}}}`,
			want: basePlan(),
			expr: `((status == "a") or (status == "b")) and (views == 2)`,
		},
		{
			name: "match_all folds out of must",
			body: `{"query": {"bool": {"must": [{"match_all": {}}, {"term": {"views": 1}}]}}}`,
			want: basePlan(),
			expr: `views == 1`,
		},
		{
			name: "empty bool matches all",
			body: `{"query": {"bool": {}}}`,
			want: basePlan(),
		},
		{
			name: "match_none matches nothing",
			body: `{"query": {"match_none": {}}}`,
			want: baseNoMatch(),
		},
		{
			name: "match_none inside must folds to NoMatch",
			body: `{"query": {"bool": {"must": [{"match_none": {}}, {"term": {"views": 1}}]}}}`,
			want: baseNoMatch(),
		},
		{
			name: "msm above the should count matches nothing",
			body: `{"query": {"bool": {"should": [{"term": {"status": "a"}}], "minimum_should_match": 2}}}`,
			want: baseNoMatch(),
		},
		{
			name: "explicit msm over an impossible should fails closed even with must",
			body: `{"query": {"bool": {"must": [{"term": {"status": "a"}}], "should": [{"match_none": {}}], "minimum_should_match": 1}}}`,
			want: baseNoMatch(),
		},
		{
			name: "default msm ignores impossible optional should",
			body: `{"query": {"bool": {"must": [{"term": {"status": "a"}}], "should": [{"match_none": {}}]}}}`,
			want: basePlan(),
			expr: `status == "a"`,
		},
		{
			name: "term values escape quotes and backslashes",
			body: `{"query": {"term": {"status": "a\"b\\c"}}}`,
			want: basePlan(),
			expr: `status == "a\"b\\c"`,
		},
		{
			name: "from and size",
			body: `{"from": 5, "size": 0}`,
			want: &translate.Plan{Offset: 5, Limit: 0, Source: translate.SourceFilter{FetchSource: true}},
		},
		{
			name: "sort shorthand, object and options forms",
			body: `{"sort": ["views", {"status": "desc"}, {"created": {"order": "asc", "missing": "_last"}}]}`,
			want: &translate.Plan{
				Limit:  10,
				Source: translate.SourceFilter{FetchSource: true},
				Sort: []translate.SortClause{
					{Field: "views"},
					{Field: "status", Desc: true},
					{Field: "created"},
				},
			},
		},
		{
			name: "sort single string form",
			body: `{"sort": "views"}`,
			want: &translate.Plan{
				Limit:  10,
				Source: translate.SourceFilter{FetchSource: true},
				Sort:   []translate.SortClause{{Field: "views"}},
			},
		},
		{
			name: "sort single object form",
			body: `{"sort": {"views": "desc"}}`,
			want: &translate.Plan{
				Limit:  10,
				Source: translate.SourceFilter{FetchSource: true},
				Sort:   []translate.SortClause{{Field: "views", Desc: true}},
			},
		},
		{
			name: "sort on _score is dropped",
			body: `{"sort": ["_score", {"views": "desc"}]}`,
			want: &translate.Plan{
				Limit:  10,
				Source: translate.SourceFilter{FetchSource: true},
				Sort:   []translate.SortClause{{Field: "views", Desc: true}},
			},
		},
		{
			name: "_source false",
			body: `{"_source": false}`,
			want: &translate.Plan{Limit: 10, Source: translate.SourceFilter{FetchSource: false}},
		},
		{
			name: "_source include list",
			body: `{"_source": ["title", "views"]}`,
			want: &translate.Plan{
				Limit:  10,
				Source: translate.SourceFilter{FetchSource: true, Includes: []string{"title", "views"}},
			},
		},
		{
			name: "_source includes/excludes with prefix pattern",
			body: `{"_source": {"includes": ["title", "user.*"], "excludes": ["internal", "*"]}}`,
			want: &translate.Plan{
				Limit: 10,
				Source: translate.SourceFilter{
					FetchSource: true,
					Includes:    []string{"title", "user.*"},
					Excludes:    []string{"internal"}, // "*" drops out: same as absent
				},
			},
		},
		{
			name: "from+size at the window limit is accepted",
			body: `{"from": 9000, "size": 1000}`,
			want: &translate.Plan{Offset: 9000, Limit: 1000, Source: translate.SourceFilter{FetchSource: true}},
		},
		{
			name: "knn minimal clause",
			body: `{"knn": {"field": "emb", "query_vector": [0.1, 0.2, 0.3]}}`,
			want: knnBase(),
		},
		{
			name: "knn k windows the plan",
			body: `{"knn": {"field": "emb", "query_vector": [0.1, 0.2, 0.3], "k": 3}}`,
			want: func() *translate.Plan { p := knnBase(); p.Limit = 3; return p }(),
		},
		{
			name: "knn without k defaults to size",
			body: `{"size": 4, "knn": {"field": "emb", "query_vector": [0.1, 0.2, 0.3]}}`,
			want: func() *translate.Plan { p := knnBase(); p.Limit = 4; return p }(),
		},
		{
			name: "knn from applies",
			body: `{"from": 2, "knn": {"field": "emb", "query_vector": [0.1, 0.2, 0.3]}}`,
			want: func() *translate.Plan { p := knnBase(); p.Offset = 2; return p }(),
		},
		{
			name: "knn filter narrows the ANN set",
			body: `{"knn": {"field": "emb", "query_vector": [0.1, 0.2, 0.3], "filter": {"term": {"status": "ok"}}}}`,
			want: knnBase(),
			expr: `status == "ok"`,
		},
		{
			name: "knn filter array is AND",
			body: `{"knn": {"field": "emb", "query_vector": [0.1, 0.2, 0.3], "filter": [{"term": {"status": "ok"}}, {"range": {"views": {"gte": 1}}}]}}`,
			want: knnBase(),
			expr: `(status == "ok") and (views >= 1)`,
		},
		{
			name: "knn num_candidates, boost and _name are ignored",
			body: `{"knn": {"field": "emb", "query_vector": [0.1, 0.2, 0.3], "num_candidates": 100, "boost": 2.0, "_name": "my search"}}`,
			want: knnBase(),
		},
		{
			name: "knn _source and from ride the normal path",
			body: `{"from": 1, "knn": {"field": "emb", "query_vector": [0.1, 0.2, 0.3]}, "_source": ["title"]}`,
			want: func() *translate.Plan {
				p := knnBase()
				p.Offset = 1
				p.Source = translate.SourceFilter{FetchSource: true, Includes: []string{"title"}}
				return p
			}(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Translate([]byte(tt.body), tt.schema)
			if err != nil {
				t.Fatalf("Translate() error = %v", err)
			}
			rendered, err := translate.Render(got.Expr)
			if err != nil {
				t.Fatalf("Render(got.Expr) error = %v", err)
			}
			if rendered != tt.expr {
				t.Fatalf("Render(plan.Expr) = %q, want %q", rendered, tt.expr)
			}
			got.Expr = nil // compared via rendering above
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Translate() =\n  got  %+v\n  want %+v", got, tt.want)
			}
		})
	}
}

func TestExprRewrite(t *testing.T) {
	// The reason Plan.Expr is a tree: plan-level rewrites transform nodes
	// directly. renameFields is the shape every rewrite takes (Phase 3 field
	// renames, Phase 5 scoring); with a string Expr this needed reparsing.
	plan, err := Translate([]byte(
		`{"query": {"bool": {"must": [{"term": {"status": "ok"}}], "filter": [{"range": {"views": {"gte": 1}}}], "must_not": [{"match": {"title": "quick brown"}}]}}}`),
		testSchema)
	if err != nil {
		t.Fatalf("Translate() error = %v", err)
	}
	renamed := renameFields(plan.Expr, func(f string) string { return "ext_" + f })
	got, err := translate.Render(renamed)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	want := `((ext_status == "ok") and (ext_views >= 1)) and (not (TEXT_MATCH(ext_title, "quick brown")))`
	if got != want {
		t.Fatalf("rewritten expr = %q, want %q", got, want)
	}
}

func renameFields(e translate.Expr, f func(string) string) translate.Expr {
	switch x := e.(type) {
	case nil:
		return nil
	case translate.Compare:
		x.Field = f(x.Field)
		return x
	case translate.InList:
		x.Field = f(x.Field)
		return x
	case translate.TextMatch:
		x.Field = f(x.Field)
		return x
	case translate.NotNull:
		x.Field = f(x.Field)
		return x
	case translate.Not:
		x.Child = renameFields(x.Child, f)
		return x
	case translate.And:
		for i, c := range x.Children {
			x.Children[i] = renameFields(c, f)
		}
		return x
	case translate.Or:
		for i, c := range x.Children {
			x.Children[i] = renameFields(c, f)
		}
		return x
	case translate.Never:
		return x
	default:
		panic(fmt.Sprintf("unhandled expr node %T", e))
	}
}

// The old string Expr spliced client-supplied field names straight into the
// expression; the tree carries them as data, and Render is the single place
// that validates them.
func TestRenderRejectsInvalidFieldNames(t *testing.T) {
	plan, err := Translate([]byte(`{"query": {"term": {"status == \"evil\" or 1 == 1": true}}}`), nil)
	if err != nil {
		t.Fatalf("Translate() error = %v", err)
	}
	if _, err := translate.Render(plan.Expr); err == nil {
		t.Fatalf("Render() accepted a field name that splices operators: %v", err)
	}
}

func TestTranslateErrors(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		schema     translate.Schema
		wantType   string
		wantReason string // substring
	}{
		{
			name:     "malformed json",
			body:     `{"query":`,
			wantType: "parsing_exception",
		},
		{
			name:       "unknown query type",
			body:       `{"query": {"no_such_query": {"title": "x"}}}`,
			wantType:   "parsing_exception",
			wantReason: "unknown query [no_such_query]",
		},
		{
			name:       "known but unsupported query type",
			body:       `{"query": {"multi_match": {"query": "x", "fields": ["a", "b"]}}}`,
			wantType:   "unsupported_exception",
			wantReason: "[multi_match]",
		},
		{
			name:     "fuzzy query is unsupported, not a parse error",
			body:     `{"query": {"fuzzy": {"title": "x"}}}`,
			wantType: "unsupported_exception",
		},
		{
			name:       "aggregations fail fast",
			body:       `{"aggs": {"by_status": {"terms": {"field": "status"}}}}`,
			wantType:   "unsupported_exception",
			wantReason: "[aggs]",
		},
		{
			name:       "knn array form (score fusion) fails fast",
			body:       `{"knn": [{"field": "v", "query_vector": [0.1]}, {"field": "v", "query_vector": [0.2]}]}`,
			wantType:   "unsupported_exception",
			wantReason: "array form",
		},
		{
			name:       "knn with query fails fast",
			body:       `{"query": {"match_all": {}}, "knn": {"field": "v", "query_vector": [0.1]}}`,
			wantType:   "unsupported_exception",
			wantReason: "[query]",
		},
		{
			name:     "knn with sort fails fast",
			body:     `{"knn": {"field": "v", "query_vector": [0.1]}, "sort": [{"views": "asc"}]}`,
			wantType: "unsupported_exception",
		},
		{
			name:       "knn similarity threshold fails fast",
			body:       `{"knn": {"field": "v", "query_vector": [0.1], "similarity": 0.9}}`,
			wantType:   "unsupported_exception",
			wantReason: "similarity",
		},
		{
			name:       "knn missing field",
			body:       `{"knn": {"query_vector": [0.1]}}`,
			wantType:   "parsing_exception",
			wantReason: "[field]",
		},
		{
			name:       "knn missing query_vector",
			body:       `{"knn": {"field": "v"}}`,
			wantType:   "parsing_exception",
			wantReason: "[query_vector]",
		},
		{
			name:     "knn query_vector not an array",
			body:     `{"knn": {"field": "v", "query_vector": 0.1}}`,
			wantType: "parsing_exception",
		},
		{
			name:       "knn query_vector element not a number",
			body:       `{"knn": {"field": "v", "query_vector": [0.1, "x"]}}`,
			wantType:   "illegal_argument_exception",
			wantReason: "element 1",
		},
		{
			name:     "knn empty query_vector",
			body:     `{"knn": {"field": "v", "query_vector": []}}`,
			wantType: "illegal_argument_exception",
		},
		{
			name:     "knn k below one",
			body:     `{"knn": {"field": "v", "query_vector": [0.1], "k": 0}}`,
			wantType: "illegal_argument_exception",
		},
		{
			name:     "knn k not a number",
			body:     `{"knn": {"field": "v", "query_vector": [0.1], "k": "ten"}}`,
			wantType: "illegal_argument_exception",
		},
		{
			name:       "knn unknown parameter",
			body:       `{"knn": {"field": "v", "query_vector": [0.1], "fuzziness": 2}}`,
			wantType:   "parsing_exception",
			wantReason: "[fuzziness]",
		},
		{
			name:     "knn filter beyond the surface",
			body:     `{"knn": {"field": "v", "query_vector": [0.1], "filter": {"fuzzy": {"title": "x"}}}}`,
			wantType: "unsupported_exception",
		},
		{
			name:     "search_after fails fast",
			body:     `{"search_after": [1], "sort": [{"views": "asc"}]}`,
			wantType: "unsupported_exception",
		},
		{
			name:     "fields fails fast",
			body:     `{"fields": ["title"]}`,
			wantType: "unsupported_exception",
		},
		{
			name:     "negative size",
			body:     `{"size": -1}`,
			wantType: "illegal_argument_exception",
		},
		{
			name:       "result window too large",
			body:       `{"size": 20000}`,
			wantType:   "illegal_argument_exception",
			wantReason: "max_result_window",
		},
		{
			name:     "term null value",
			body:     `{"query": {"term": {"status": null}}}`,
			wantType: "parsing_exception",
		},
		{
			name:       "term object without value",
			body:       `{"query": {"term": {"status": {}}}}`,
			wantType:   "parsing_exception",
			wantReason: "[value]",
		},
		{
			name:       "term with two fields",
			body:       `{"query": {"term": {"a": 1, "b": 2}}}`,
			wantType:   "parsing_exception",
			wantReason: "multiple fields",
		},
		{
			name:       "two clauses in query",
			body:       `{"query": {"term": {"a": 1}, "match_all": {}}}`,
			wantType:   "parsing_exception",
			wantReason: "exactly one clause",
		},
		{
			name:       "match fuzziness",
			body:       `{"query": {"match": {"title": {"query": "x", "fuzziness": 2}}}}`,
			wantType:   "unsupported_exception",
			wantReason: "[fuzziness]",
		},
		{
			name:     "terms lookup",
			body:     `{"query": {"terms": {"status": {"index": "other", "id": "1", "path": "p"}}}}`,
			wantType: "unsupported_exception",
		},
		{
			name:       "minimum_should_match percent form",
			body:       `{"query": {"bool": {"should": [{"term": {"status": "a"}}], "minimum_should_match": "50%"}}}`,
			wantType:   "unsupported_exception",
			wantReason: "percentage",
		},
		{
			name:       "minimum_should_match between 2 and count unsupported",
			body:       `{"query": {"bool": {"should": [{"term": {"status": "a"}}, {"term": {"status": "b"}}, {"term": {"status": "c"}}], "minimum_should_match": 2}}}`,
			wantType:   "unsupported_exception",
			wantReason: "minimum_should_match > 1",
		},
		{
			name:     "sort unknown option",
			body:     `{"sort": [{"views": {"mode": "avg"}}]}`,
			wantType: "unsupported_exception",
		},
		{
			name:       "sort bad order value",
			body:       `{"sort": [{"views": "up"}]}`,
			wantType:   "parsing_exception",
			wantReason: "asc",
		},
		{
			name:     "geo distance sort",
			body:     `{"sort": [{"_geo_distance": {"loc": "1,1"}}]}`,
			wantType: "unsupported_exception",
		},
		{
			name:     "mid-pattern _source wildcard",
			body:     `{"_source": ["a*b"]}`,
			wantType: "unsupported_exception",
		},
		{
			name:       "range format option",
			body:       `{"query": {"range": {"created": {"gte": "2024", "format": "yyyy"}}}}`,
			wantType:   "unsupported_exception",
			wantReason: "[format]",
		},
		{
			name:       "unparsable date string",
			body:       `{"query": {"range": {"created": {"gte": "not-a-date"}}}}`,
			schema:     testSchema,
			wantType:   "illegal_argument_exception",
			wantReason: "cannot parse date",
		},
		{
			name:       "non-numeric string on numeric field",
			body:       `{"query": {"term": {"views": "many"}}}`,
			schema:     testSchema,
			wantType:   "illegal_argument_exception",
			wantReason: "numeric",
		},
		{
			name:       "match_all must be an object",
			body:       `{"query": {"match_all": 1}}`,
			wantType:   "parsing_exception",
			wantReason: "[match_all]",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Translate([]byte(tt.body), tt.schema)
			if err == nil {
				t.Fatalf("Translate() expected error, got nil")
			}
			var terr *translate.Error
			if !errors.As(err, &terr) {
				t.Fatalf("Translate() error = %T (%v), want *translate.Error", err, err)
			}
			if terr.Type != tt.wantType {
				t.Errorf("error type = %q, want %q (reason: %s)", terr.Type, tt.wantType, terr.Reason)
			}
			if terr.Status != 400 {
				t.Errorf("error status = %d, want 400", terr.Status)
			}
			if tt.wantReason != "" && !strings.Contains(terr.Reason, tt.wantReason) {
				t.Errorf("reason %q does not contain %q", terr.Reason, tt.wantReason)
			}
		})
	}
}
