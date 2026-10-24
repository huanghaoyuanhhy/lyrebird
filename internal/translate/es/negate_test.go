package es

import (
	"testing"

	"github.com/huanghaoyuanhhy/lyrebird/internal/translate"
)

// nullableTestSchema extends the plain map with the nullability facts the
// must_not rendering keys on: note and price admit nulls, the rest do not.
type nullableTestSchema struct {
	translate.MapSchema
	nulls map[string]bool
}

func (s nullableTestSchema) Nullable(field string) bool { return s.nulls[field] }

var negateSchema = nullableTestSchema{
	MapSchema: translate.MapSchema{
		"name":  translate.TypeKeyword,
		"note":  translate.TypeKeyword,
		"price": translate.TypeNumber,
		"title": translate.TypeText,
	},
	nulls: map[string]bool{"note": true, "price": true},
}

// TestMustNotNullable pins the three-valued negation contract: a must_not
// clause over a nullable field must keep the docs whose field is null —
// Elasticsearch counts a missing field as "did not match", so NOT includes
// it, while Milvus's `not (...)` renders unknown there and drops the row.
func TestMustNotNullable(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		schema translate.Schema
		want   string
	}{
		{
			name:   "term over nullable field rides is null",
			body:   `{"query":{"bool":{"must_not":[{"term":{"note":"alpha"}}]}}}`,
			schema: negateSchema,
			want:   `(note is null) or (not (note == "alpha"))`,
		},
		{
			name:   "term over non-nullable field stays plain",
			body:   `{"query":{"bool":{"must_not":[{"term":{"name":"bravo"}}]}}}`,
			schema: negateSchema,
			want:   `not (name == "bravo")`,
		},
		{
			name:   "single-bound range over nullable field",
			body:   `{"query":{"bool":{"must_not":[{"range":{"price":{"gte":15}}}]}}}`,
			schema: negateSchema,
			want:   `(price is null) or (not (price >= 15))`,
		},
		{
			name:   "two-bound range de morgans into per-bound rescues",
			body:   `{"query":{"bool":{"must_not":[{"range":{"price":{"gte":15,"lt":100}}}]}}}`,
			schema: negateSchema,
			want:   `((price is null) or (not (price >= 15))) or ((price is null) or (not (price < 100)))`,
		},
		{
			name:   "terms over nullable field",
			body:   `{"query":{"bool":{"must_not":[{"terms":{"note":["a","b"]}}]}}}`,
			schema: negateSchema,
			want:   `(note is null) or (not (note in ["a", "b"]))`,
		},
		{
			name:   "exists still lowers to is null",
			body:   `{"query":{"bool":{"must_not":[{"exists":{"field":"note"}}]}}}`,
			schema: negateSchema,
			want:   `note is null`,
		},
		{
			name:   "exists and term compose with and",
			body:   `{"query":{"bool":{"must_not":[{"exists":{"field":"note"}},{"term":{"note":"alpha"}}]}}}`,
			schema: negateSchema,
			want:   `(note is null) and ((note is null) or (not (note == "alpha")))`,
		},
		{
			name:   "nested bool with a nullable must rides the rescue too",
			body:   `{"query":{"bool":{"must_not":[{"bool":{"must":[{"term":{"note":"alpha"}}]}}]}}}`,
			schema: negateSchema,
			want:   `(note is null) or (not (note == "alpha"))`,
		},
		{
			name:   "nested bool with shoulds in must_not de morgans",
			body:   `{"query":{"bool":{"must_not":[{"bool":{"should":[{"term":{"note":"a"}},{"term":{"note":"b"}}]}}]}}}`,
			schema: negateSchema,
			want:   `((note is null) or (not (note == "a"))) and ((note is null) or (not (note == "b")))`,
		},
		{
			name:   "double negation collapses",
			body:   `{"query":{"bool":{"must_not":[{"bool":{"must_not":[{"term":{"name":"bravo"}}]}}]}}}`,
			schema: negateSchema,
			want:   `name == "bravo"`,
		},
		{
			name:   "text match over non-nullable text stays plain",
			body:   `{"query":{"bool":{"must_not":[{"match":{"title":"quick"}}]}}}`,
			schema: negateSchema,
			want:   `not (TEXT_MATCH(title, "quick"))`,
		},
		{
			name:   "a schema without the nullable extension keeps the plain rendering",
			body:   `{"query":{"bool":{"must_not":[{"term":{"note":"alpha"}}]}}}`,
			schema: translate.MapSchema{"note": translate.TypeKeyword},
			want:   `not (note == "alpha")`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := Translate([]byte(tt.body), tt.schema)
			if err != nil {
				t.Fatalf("Translate() error = %v", err)
			}
			rendered, err := translate.Render(plan.Expr)
			if err != nil {
				t.Fatalf("Render(plan.Expr) error = %v", err)
			}
			if rendered != tt.want {
				t.Fatalf("Render(plan.Expr) =\n  got  %s\n  want %s", rendered, tt.want)
			}
		})
	}
}
