package translate

import (
	"fmt"
	"sort"
)

// Plan is the protocol-agnostic result of translation: everything the store
// layer needs to execute a read against Milvus, and nothing protocol-specific.
// Both translators (es, pg) produce it; store consumes it.
type Plan struct {
	// Expr filters entities, as a Milvus boolean-expression tree (see Expr).
	// nil means "no filter" (match everything). The tree — not a rendered
	// string — is the contract, so rewrites (field renames, scoring) can
	// transform plans without parsing text; the store renders it via Render
	// right before execution, which is also where field names are validated.
	Expr Expr

	// Offset/Limit correspond to ES from/size (PG OFFSET/LIMIT).
	Offset int
	Limit  int

	// Sort orders the results. Milvus scalar queries have no server-side
	// ORDER BY today (docs/design.md decision 4), so the store sorts after
	// fetching; the plan only records the requested order.
	Sort []SortClause

	// Source controls which fields are fetched and returned.
	//
	// FetchSource false means hits carry no source at all (ES _source: false).
	// The boolean's zero value is false, so translators MUST default it to
	// true (the ES default) — a Plan built from a bare Plan{} literal yields
	// _id-only hits. parseSource does this; keep the pg translator honest too.
	Source SourceFilter

	// NoMatch marks a query that cannot match anything (e.g. bool with
	// minimum_should_match above the surviving should-clause count, or an
	// empty terms list). The store returns an empty result without executing.
	NoMatch bool
}

// SortClause is one ordering term; Field is an external (client-facing) name.
type SortClause struct {
	Field string
	Desc  bool
}

// SourceFilter mirrors the ES _source body parameter.
type SourceFilter struct {
	// FetchSource false means hits carry no _source at all (_source: false).
	FetchSource bool
	// Includes / Excludes hold exact field names or "prefix.*" patterns.
	// Empty Includes means all fields.
	Includes []string
	Excludes []string
}

// FieldType is the protocol-agnostic view of a field's type: the minimum the
// translators need to choose between equivalent encodings (e.g. an ES match
// on a text field is a token match, on a keyword field an exact compare).
// The real catalog lands in Phase 3; until then tests feed fixed maps and
// callers may pass nil (every field unknown).
type FieldType string

const (
	TypeKeyword FieldType = "keyword"
	TypeText    FieldType = "text"
	TypeNumber  FieldType = "number"
	TypeBool    FieldType = "bool"
	TypeDate    FieldType = "date"
	// TypeUnknown covers fields absent from the catalog.
	TypeUnknown FieldType = "unknown"
)

// Schema answers field questions for the translators: types for WHERE
// lowering, and the field list for expanding SELECT * (whose columns must
// be known before execution — the PG wire describes them ahead of the rows).
// Fields returns names in storage order when the implementation has one;
// implementations without order (MapSchema) sort alphabetically.
type Schema interface {
	FieldType(field string) FieldType
	Fields() []string
}

// MapSchema is a Schema backed by a plain map; absent fields are unknown.
type MapSchema map[string]FieldType

var _ Schema = MapSchema{}

func (m MapSchema) FieldType(field string) FieldType {
	if t, ok := m[field]; ok {
		return t
	}
	return TypeUnknown
}

// Fields implements Schema: the map's keys, sorted so consumers get a
// stable order out of an unordered type.
func (m MapSchema) Fields() []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Error is a translation failure: one shared three-way classification,
// rendered in each frontend's native vocabulary, so protocol layers can
// build native error envelopes without re-parsing reason strings.
type Error struct {
	// Type is the frontend's native error name. The es translator fills ES
	// exception names: parsing_exception (malformed DSL),
	// illegal_argument_exception (bad values), unsupported_exception
	// (lyrebird's own: valid request, capability beyond the current phase).
	// The pg translator fills SQLSTATE codes over the same classification:
	// 42601 (syntax), 22023 (invalid value), 0A000 (beyond the subset).
	Type   string
	Reason string
	Status int
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s", e.Type, e.Reason) }
