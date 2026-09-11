// Package es translates Elasticsearch Query DSL (_search request bodies)
// into Milvus query/search plans.
//
// Entry point: Translate(body, schema) → *translate.Plan — a pure function,
// (request, schema) → plan, fully covered by unit tests.
//
// Phase 1 subset:
//
//   - queries: match_all, match_none, bool (must/filter/should/must_not,
//     minimum_should_match), term, terms, match, range, exists
//   - body members: from, size (max_result_window enforced), sort, _source
//
// Mapping highlights (see docs/design.md):
//
//   - term/terms/range/exists → Milvus boolean expressions
//   - match on analyzed text → TEXT_MATCH (OR across terms = ES default
//     operator); scores stay a constant 1.0 until Phase 5 aligns BM25
//   - date strings → epoch millis, the chosen storage encoding
//   - sort and _source are carried in the plan for the store layer to apply
package es
