package pg

import (
	"errors"
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

// vectorSchema is a collection with one float-vector field, enough to lower
// the pgvector distance operators.
var vectorSchema = translate.MapSchema{
	"embedding": translate.TypeVector,
}

// limitPlan is the plan of a bare SELECT * with the given LIMIT: match-all,
// every field, no sort.
func limitPlan(limit int) *translate.Plan {
	return &translate.Plan{Limit: limit, Source: translate.SourceFilter{FetchSource: true}}
}

func noMatchPlan(limit int) *translate.Plan {
	p := limitPlan(limit)
	p.NoMatch = true
	return p
}

// planOf runs the two-step flow the pgserver shell will use:
// Parse (no cluster access) → Plan(schema).
func planOf(t *testing.T, sql string, schema translate.Schema) (*Select, *translate.Plan) {
	t.Helper()
	sel, err := Parse(sql)
	if err != nil {
		t.Fatalf("Parse(%q) error = %v", sql, err)
	}
	plan, err := sel.Plan(schema)
	if err != nil {
		t.Fatalf("Plan(%q) error = %v", sql, err)
	}
	return sel, plan
}

func TestParseAndPlan(t *testing.T) {
	tests := []struct {
		name      string
		sql       string
		schema    translate.Schema
		wantTable string
		want      *translate.Plan
		expr      string // expected Render(plan.Expr); "" = match everything
	}{
		{
			name:      "select star with limit",
			sql:       `SELECT * FROM items LIMIT 10`,
			wantTable: "items",
			want:      limitPlan(10),
		},
		{
			name:      "keywords are case-insensitive",
			sql:       `select * from items limit 10`,
			wantTable: "items",
			want:      limitPlan(10),
		},
		{
			name:      "column list projects fields",
			sql:       `SELECT id, title FROM docs LIMIT 5`,
			wantTable: "docs",
			want: &translate.Plan{
				Limit:  5,
				Source: translate.SourceFilter{FetchSource: true, Includes: []string{"id", "title"}},
			},
		},
		{
			name:      "limit then offset",
			sql:       `SELECT * FROM items LIMIT 5 OFFSET 10`,
			wantTable: "items",
			want:      &translate.Plan{Offset: 10, Limit: 5, Source: translate.SourceFilter{FetchSource: true}},
		},
		{
			name:      "offset then limit",
			sql:       `SELECT * FROM items OFFSET 10 LIMIT 5`,
			wantTable: "items",
			want:      &translate.Plan{Offset: 10, Limit: 5, Source: translate.SourceFilter{FetchSource: true}},
		},
		{
			name:      "offset noise words",
			sql:       `SELECT * FROM items OFFSET 3 ROWS LIMIT 2`,
			wantTable: "items",
			want:      &translate.Plan{Offset: 3, Limit: 2, Source: translate.SourceFilter{FetchSource: true}},
		},
		{
			name:      "limit zero is a valid plan",
			sql:       `SELECT * FROM items LIMIT 0`,
			wantTable: "items",
			want:      limitPlan(0),
		},
		{
			name:      "trailing semicolon",
			sql:       `SELECT * FROM items LIMIT 10;`,
			wantTable: "items",
			want:      limitPlan(10),
		},
		{
			name:      "identifiers keep their written case",
			sql:       `SELECT * FROM Items WHERE Views > 1 LIMIT 10`,
			wantTable: "Items",
			want:      limitPlan(10),
			expr:      `Views > 1`,
		},
		{
			name:      "where equals",
			sql:       `SELECT * FROM items WHERE status = 'ok' LIMIT 10`,
			wantTable: "items",
			want:      limitPlan(10),
			expr:      `status == "ok"`,
		},
		{
			name:      "not-equals, both spellings",
			sql:       `SELECT * FROM items WHERE status <> 'x' LIMIT 10`,
			wantTable: "items",
			want:      limitPlan(10),
			expr:      `status != "x"`,
		},
		{
			name:      "literal on the left flips",
			sql:       `SELECT * FROM items WHERE 100 < views LIMIT 10`,
			wantTable: "items",
			want:      limitPlan(10),
			expr:      `views > 100`,
		},
		{
			name:      "literal-first ge flips",
			sql:       `SELECT * FROM items WHERE 5 >= views LIMIT 10`,
			wantTable: "items",
			want:      limitPlan(10),
			expr:      `views <= 5`,
		},
		{
			name:      "negative number",
			sql:       `SELECT * FROM items WHERE views = -5 LIMIT 10`,
			wantTable: "items",
			want:      limitPlan(10),
			expr:      `views == -5`,
		},
		{
			name:      "and binds tighter than or",
			sql:       `SELECT * FROM items WHERE status = 'a' OR status = 'b' AND views > 1 LIMIT 10`,
			wantTable: "items",
			want:      limitPlan(10),
			expr:      `(status == "a") or ((status == "b") and (views > 1))`,
		},
		{
			name:      "or of and flat form",
			sql:       `SELECT * FROM items WHERE status = 'a' AND views > 1 OR active = false LIMIT 10`,
			wantTable: "items",
			want:      limitPlan(10),
			expr:      `((status == "a") and (views > 1)) or (active == false)`,
		},
		{
			name:      "parentheses group the or",
			sql:       `SELECT * FROM items WHERE (status = 'a' OR status = 'b') AND views > 1 LIMIT 10`,
			wantTable: "items",
			want:      limitPlan(10),
			expr:      `((status == "a") or (status == "b")) and (views > 1)`,
		},
		{
			name:      "bare column is a boolean predicate",
			sql:       `SELECT * FROM items WHERE NOT active LIMIT 10`,
			wantTable: "items",
			want:      limitPlan(10),
			expr:      `not (active == true)`,
		},
		{
			name:      "boolean equality",
			sql:       `SELECT * FROM items WHERE active = false LIMIT 10`,
			wantTable: "items",
			want:      limitPlan(10),
			expr:      `active == false`,
		},
		{
			name:      "in list",
			sql:       `SELECT * FROM items WHERE status IN ('a', 'b') LIMIT 10`,
			wantTable: "items",
			want:      limitPlan(10),
			expr:      `status in ["a", "b"]`,
		},
		{
			name:      "not in",
			sql:       `SELECT * FROM items WHERE status NOT IN ('a', 'b') LIMIT 10`,
			wantTable: "items",
			want:      limitPlan(10),
			expr:      `not (status in ["a", "b"])`,
		},
		{
			name:      "between",
			sql:       `SELECT * FROM items WHERE views BETWEEN 10 AND 20 LIMIT 10`,
			wantTable: "items",
			want:      limitPlan(10),
			expr:      `(views >= 10) and (views <= 20)`,
		},
		{
			name:      "not between",
			sql:       `SELECT * FROM items WHERE views NOT BETWEEN 10 AND 20 LIMIT 10`,
			wantTable: "items",
			want:      limitPlan(10),
			expr:      `not ((views >= 10) and (views <= 20))`,
		},
		{
			name:      "is null",
			sql:       `SELECT * FROM items WHERE deleted IS NULL LIMIT 10`,
			wantTable: "items",
			want:      limitPlan(10),
			expr:      `not (deleted is not null)`,
		},
		{
			name:      "is not null",
			sql:       `SELECT * FROM items WHERE deleted IS NOT NULL LIMIT 10`,
			wantTable: "items",
			want:      limitPlan(10),
			expr:      `deleted is not null`,
		},
		{
			name:      "TRUE is match-all",
			sql:       `SELECT * FROM items WHERE TRUE LIMIT 10`,
			wantTable: "items",
			want:      limitPlan(10),
		},
		{
			name:      "FALSE matches nothing",
			sql:       `SELECT * FROM items WHERE FALSE LIMIT 10`,
			wantTable: "items",
			want:      noMatchPlan(10),
		},
		{
			name:      "NOT TRUE matches nothing",
			sql:       `SELECT * FROM items WHERE NOT TRUE LIMIT 10`,
			wantTable: "items",
			want:      noMatchPlan(10),
		},
		{
			name:      "NOT FALSE is match-all",
			sql:       `SELECT * FROM items WHERE NOT FALSE LIMIT 10`,
			wantTable: "items",
			want:      limitPlan(10),
		},
		{
			name:      "false branch drops out of or",
			sql:       `SELECT * FROM items WHERE FALSE OR status = 'a' LIMIT 10`,
			wantTable: "items",
			want:      limitPlan(10),
			expr:      `status == "a"`,
		},
		{
			name:      "true branch drops out of and",
			sql:       `SELECT * FROM items WHERE TRUE AND status = 'a' LIMIT 10`,
			wantTable: "items",
			want:      limitPlan(10),
			expr:      `status == "a"`,
		},
		{
			name:      "comparison against NULL never passes",
			sql:       `SELECT * FROM items WHERE status = NULL LIMIT 10`,
			wantTable: "items",
			want:      noMatchPlan(10),
		},
		{
			name:      "NULL member adds nothing to in",
			sql:       `SELECT * FROM items WHERE status IN ('a', NULL) LIMIT 10`,
			wantTable: "items",
			want:      limitPlan(10),
			expr:      `status in ["a"]`,
		},
		{
			name:      "NOT IN over NULL member never passes",
			sql:       `SELECT * FROM items WHERE status NOT IN ('a', NULL) LIMIT 10`,
			wantTable: "items",
			want:      noMatchPlan(10),
		},
		{
			name:      "number takes the text form on keyword fields",
			sql:       `SELECT * FROM items WHERE status = 42 LIMIT 10`,
			schema:    testSchema,
			wantTable: "items",
			want:      limitPlan(10),
			expr:      `status == "42"`,
		},
		{
			name:      "date-only string becomes epoch millis",
			sql:       `SELECT * FROM items WHERE created > '2024-01-02' LIMIT 10`,
			schema:    testSchema,
			wantTable: "items",
			want:      limitPlan(10),
			expr:      `created > 1704153600000`,
		},
		{
			name:      "PG timestamp form becomes epoch millis",
			sql:       `SELECT * FROM items WHERE created < '2024-01-02 10:00:00' LIMIT 10`,
			schema:    testSchema,
			wantTable: "items",
			want:      limitPlan(10),
			expr:      `created < 1704189600000`,
		},
		{
			name:      "text field compares exactly",
			sql:       `SELECT * FROM items WHERE title = 'quick' LIMIT 10`,
			schema:    testSchema,
			wantTable: "items",
			want:      limitPlan(10),
			expr:      `title == "quick"`,
		},
		{
			name:      "alias-qualified and table-qualified columns strip",
			sql:       `SELECT * FROM items i WHERE i.status = 'x' AND items.views > 1 LIMIT 10`,
			wantTable: "items",
			want:      limitPlan(10),
			expr:      `(status == "x") and (views > 1)`,
		},
		{
			name:      "quoted identifiers, reserved word as table",
			sql:       `SELECT * FROM "order" o WHERE o."status" = 'x' LIMIT 10`,
			wantTable: "order",
			want:      limitPlan(10),
			expr:      `status == "x"`,
		},
		{
			name:      "order by asc and desc",
			sql:       `SELECT * FROM items ORDER BY created DESC, views LIMIT 3`,
			wantTable: "items",
			want: &translate.Plan{
				Limit:  3,
				Source: translate.SourceFilter{FetchSource: true},
				Sort:   []translate.SortClause{{Field: "created", Desc: true}, {Field: "views"}},
			},
		},
		{
			name:      "where and order by and offset together",
			sql:       `SELECT id, status FROM items WHERE views > 1 ORDER BY views DESC OFFSET 2 ROWS LIMIT 1`,
			wantTable: "items",
			want: &translate.Plan{
				Offset: 2,
				Limit:  1,
				Source: translate.SourceFilter{FetchSource: true, Includes: []string{"id", "status"}},
				Sort:   []translate.SortClause{{Field: "views", Desc: true}},
			},
			expr: `views > 1`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sel, got := planOf(t, tt.sql, tt.schema)
			if sel.Table != tt.wantTable {
				t.Fatalf("Select.Table = %q, want %q", sel.Table, tt.wantTable)
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
				t.Fatalf("Plan() =\n  got  %+v\n  want %+v", got, tt.want)
			}
		})
	}
}

func TestParseAndPlanErrors(t *testing.T) {
	tests := []struct {
		name       string
		sql        string
		schema     translate.Schema
		wantType   string // SQLSTATE code
		wantReason string // substring
	}{
		// syntax (42601)
		{name: "empty statement", sql: "   ", wantType: "42601"},
		{name: "not a statement", sql: `items`, wantType: "42601"},
		{name: "empty select list", sql: `SELECT FROM items LIMIT 1`, wantType: "42601", wantReason: "column or *"},
		{name: "missing table", sql: `SELECT status FROM LIMIT 1`, wantType: "42601", wantReason: "table name"},
		{name: "missing comparison operand", sql: `SELECT * FROM items WHERE = 5 LIMIT 1`, wantType: "42601"},
		{name: "dangling where", sql: `SELECT * FROM items WHERE`, wantType: "42601", wantReason: "end of statement"},
		{name: "unterminated string", sql: `SELECT * FROM items WHERE status = 'ok`, wantType: "42601", wantReason: "unterminated"},
		{name: "dollar quoting", sql: `SELECT * FROM items WHERE status = $$x$$ LIMIT 1`, wantType: "42601", wantReason: "dollar"},
		{name: "limit needs an integer", sql: `SELECT * FROM items LIMIT 1e3`, wantType: "42601", wantReason: "integer"},
		{name: "limit needs a number", sql: `SELECT * FROM items LIMIT`, wantType: "42601", wantReason: "integer"},
		{name: "duplicate limit", sql: `SELECT * FROM items LIMIT 1 LIMIT 2`, wantType: "42601", wantReason: "duplicate"},
		{name: "duplicate where", sql: `SELECT * FROM items WHERE a = 1 WHERE b = 2 LIMIT 1`, wantType: "42601", wantReason: "once"},
		{name: "dotted quoted identifier", sql: `SELECT "a.b" FROM t LIMIT 1`, wantType: "42601", wantReason: "dot"},
		{name: "two statements", sql: `SELECT * FROM t LIMIT 1; SELECT * FROM t LIMIT 1`, wantType: "42601", wantReason: "one statement"},

		// bad values (22023)
		{name: "negative limit", sql: `SELECT * FROM items LIMIT -1`, wantType: "22023"},
		{name: "negative offset", sql: `SELECT * FROM items OFFSET -5 LIMIT 1`, wantType: "22023"},
		{name: "result window", sql: `SELECT * FROM items LIMIT 20000`, wantType: "22023", wantReason: "window"},
		{name: "result window via offset", sql: `SELECT * FROM items OFFSET 9999 LIMIT 10`, wantType: "22023", wantReason: "window"},
		{name: "non-numeric string on number field", sql: `SELECT * FROM items WHERE views = 'abc' LIMIT 1`, schema: testSchema, wantType: "22023", wantReason: "numeric"},
		{name: "unparsable date", sql: `SELECT * FROM items WHERE created > 'not-a-date' LIMIT 1`, schema: testSchema, wantType: "22023", wantReason: "cannot parse date"},
		{name: "number on boolean field", sql: `SELECT * FROM items WHERE active = 5 LIMIT 1`, schema: testSchema, wantType: "22023"},
		{name: "string on boolean field", sql: `SELECT * FROM items WHERE active = 'maybe' LIMIT 1`, schema: testSchema, wantType: "22023"},
		{name: "boolean on number field", sql: `SELECT * FROM items WHERE views = true LIMIT 1`, schema: testSchema, wantType: "22023"},

		// beyond the subset (0A000)
		{name: "write statement", sql: `UPDATE items SET status = 'x'`, wantType: "0A000", wantReason: "only SELECT"},
		{name: "insert statement", sql: `INSERT INTO items VALUES (1)`, wantType: "0A000", wantReason: "only SELECT"},
		{name: "distinct", sql: `SELECT DISTINCT status FROM items LIMIT 1`, wantType: "0A000", wantReason: "DISTINCT"},
		{name: "aggregate projection", sql: `SELECT count(*) FROM items LIMIT 1`, wantType: "0A000", wantReason: "count"},
		{name: "expression projection", sql: `SELECT views + 1 FROM items LIMIT 1`, wantType: "0A000", wantReason: "expression"},
		{name: "alias", sql: `SELECT title AS t FROM items LIMIT 1`, wantType: "0A000", wantReason: "aliases"},
		{name: "star combined with columns", sql: `SELECT *, id FROM items LIMIT 1`, wantType: "0A000", wantReason: "combined"},
		{name: "qualified star", sql: `SELECT i.* FROM items i LIMIT 1`, wantType: "0A000", wantReason: "qualified star"},
		{name: "multiple tables", sql: `SELECT * FROM a, b LIMIT 1`, wantType: "0A000", wantReason: "single-table"},
		{name: "join", sql: `SELECT * FROM a JOIN b ON a.x = b.x LIMIT 1`, wantType: "0A000", wantReason: "joins"},
		{name: "schema-qualified table", sql: `SELECT * FROM public.items LIMIT 1`, wantType: "0A000", wantReason: "schema-qualified"},
		{name: "group by", sql: `SELECT status FROM items GROUP BY status LIMIT 1`, wantType: "0A000", wantReason: "GROUP"},
		{name: "union", sql: `SELECT status FROM items UNION SELECT status FROM other LIMIT 1`, wantType: "0A000", wantReason: "UNION"},
		{name: "for update", sql: `SELECT * FROM items FOR UPDATE LIMIT 1`, wantType: "0A000", wantReason: "FOR"},
		{name: "like", sql: `SELECT * FROM items WHERE title LIKE '%a%' LIMIT 1`, wantType: "0A000", wantReason: "LIKE"},
		{name: "not like", sql: `SELECT * FROM items WHERE NOT title LIKE '%a%' LIMIT 1`, wantType: "0A000", wantReason: "LIKE"},
		{name: "cast", sql: `SELECT * FROM items WHERE created > '2024-01-01'::date LIMIT 1`, schema: testSchema, wantType: "0A000", wantReason: "casts"},
		{name: "subquery", sql: `SELECT * FROM items WHERE status IN (SELECT status FROM other LIMIT 1) LIMIT 1`, wantType: "0A000", wantReason: "subquer"},
		{name: "query parameter", sql: `SELECT * FROM items WHERE views = $1 LIMIT 1`, wantType: "0A000", wantReason: "parameters"},
		{name: "two constant comparison", sql: `SELECT * FROM items WHERE 1 = 1 LIMIT 1`, wantType: "0A000", wantReason: "constants"},
		{name: "column-to-column comparison", sql: `SELECT * FROM items WHERE a = b LIMIT 1`, wantType: "0A000", wantReason: "column-to-column"},
		{name: "unknown table qualifier", sql: `SELECT * FROM items x WHERE other.status = 'a' LIMIT 1`, wantType: "0A000", wantReason: "another table"},
		{name: "distance predicate filters nothing", sql: `SELECT * FROM items WHERE embedding <=> '[0.1]' < 0.5 LIMIT 5`, wantType: "0A000", wantReason: "does not filter in WHERE"},
		{name: "l1 distance has no milvus metric", sql: `SELECT * FROM items ORDER BY embedding <+> '[0.1, 0.2]' LIMIT 5`, schema: vectorSchema, wantType: "0A000", wantReason: "no Milvus metric"},
		{name: "distance ordering desc has no ann answer", sql: `SELECT * FROM items ORDER BY embedding <-> '[0.1, 0.2]' DESC LIMIT 5`, schema: vectorSchema, wantType: "0A000", wantReason: "farthest-first"},
		{name: "distance ordering takes a literal", sql: `SELECT * FROM items ORDER BY embedding <-> other LIMIT 5`, schema: vectorSchema, wantType: "42601", wantReason: "vector literal"},
		{name: "distance ordering is the whole sort", sql: `SELECT * FROM items ORDER BY embedding <-> '[0.1, 0.2]', id LIMIT 5`, schema: vectorSchema, wantType: "0A000", wantReason: "whole sort"},
		{name: "second term before distance ordering", sql: `SELECT * FROM items ORDER BY id, embedding <-> '[0.1, 0.2]' LIMIT 5`, schema: vectorSchema, wantType: "0A000", wantReason: "whole sort"},
		{name: "distance select-list expression", sql: `SELECT embedding <=> '[0.1, 0.2]' FROM items LIMIT 5`, schema: vectorSchema, wantType: "0A000", wantReason: "distance expressions"},
		{name: "distance on non-vector field", sql: `SELECT * FROM items ORDER BY views <-> '[0.1, 0.2]' LIMIT 5`, schema: testSchema, wantType: "22023", wantReason: "distance operators order on vector fields"},
		{name: "distance on unknown field", sql: `SELECT * FROM items ORDER BY nosuch <-> '[0.1, 0.2]' LIMIT 5`, schema: testSchema, wantType: "22023", wantReason: "not a vector field"},
		{name: "unparsable vector literal", sql: `SELECT * FROM items ORDER BY embedding <-> '[0.1, oops]' LIMIT 5`, schema: vectorSchema, wantType: "22023", wantReason: "does not parse"},
		{name: "empty vector literal", sql: `SELECT * FROM items ORDER BY embedding <-> '' LIMIT 5`, schema: vectorSchema, wantType: "22023", wantReason: "empty"},
		{name: "limit all", sql: `SELECT * FROM items LIMIT ALL`, wantType: "0A000", wantReason: "row cap"},
		{name: "missing limit", sql: `SELECT * FROM items`, wantType: "0A000", wantReason: "row cap"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sel, parseErr := Parse(tt.sql)
			var err error
			if parseErr != nil {
				err = parseErr
			} else {
				_, err = sel.Plan(tt.schema)
			}
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			var terr *translate.Error
			if !errors.As(err, &terr) {
				t.Fatalf("error = %T (%v), want *translate.Error", err, err)
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

// Select exposes what the shell needs beyond the plan: the collection name
// (for Executor.Schema and Executor.Search) — the piece translate.Plan
// deliberately does not carry.
func TestSelectCarriesCollection(t *testing.T) {
	sel, err := Parse(`SELECT * FROM "mixed-Case" LIMIT 1`)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if sel.Table != "mixed-Case" {
		t.Fatalf("Select.Table = %q, want %q", sel.Table, "mixed-Case")
	}
}

func TestVectorOrdering(t *testing.T) {
	tests := []struct {
		name      string
		sql       string
		wantTable string
		want      *translate.Plan
		expr      string
	}{
		{
			name:      "cosine ordering, the canonical pgvector query",
			sql:       `SELECT * FROM items ORDER BY embedding <=> '[0.1, 0.2, 0.3, 0.4]' LIMIT 5`,
			wantTable: "items",
			want: &translate.Plan{
				Limit:  5,
				Source: translate.SourceFilter{FetchSource: true},
				Search: &translate.SearchSpec{
					Field:  "embedding",
					Vector: []float32{0.1, 0.2, 0.3, 0.4},
					Metric: translate.MetricCosine,
				},
			},
		},
		{
			name:      "l2 and inner product operators map their metrics",
			sql:       `SELECT id FROM items ORDER BY embedding <-> '[1, 2]' LIMIT 3 OFFSET 2`,
			wantTable: "items",
			want: &translate.Plan{
				Offset: 2,
				Limit:  3,
				Source: translate.SourceFilter{FetchSource: true, Includes: []string{"id"}},
				Search: &translate.SearchSpec{
					Field:  "embedding",
					Vector: []float32{1, 2},
					Metric: translate.MetricL2,
				},
			},
		},
		{
			name:      "negated inner product drops its sign in the plan",
			sql:       `SELECT * FROM items ORDER BY embedding <#> '[0.5]' LIMIT 1`,
			wantTable: "items",
			want: &translate.Plan{
				Limit:  1,
				Source: translate.SourceFilter{FetchSource: true},
				Search: &translate.SearchSpec{
					Field:  "embedding",
					Vector: []float32{0.5},
					Metric: translate.MetricIP,
				},
			},
		},
		{
			name:      "asc is accepted beside the distance",
			sql:       `SELECT * FROM items ORDER BY embedding <=> '[1]' ASC LIMIT 2`,
			wantTable: "items",
			want: &translate.Plan{
				Limit:  2,
				Source: translate.SourceFilter{FetchSource: true},
				Search: &translate.SearchSpec{Field: "embedding", Vector: []float32{1}, Metric: translate.MetricCosine},
			},
		},
		{
			name:      "scalar filter rides the vector search",
			sql:       `SELECT name FROM items WHERE active = true ORDER BY embedding <-> '[0, 0]' LIMIT 10`,
			wantTable: "items",
			expr:      `active == true`,
			want: &translate.Plan{
				Limit:  10,
				Source: translate.SourceFilter{FetchSource: true, Includes: []string{"name"}},
				Search: &translate.SearchSpec{Field: "embedding", Vector: []float32{0, 0}, Metric: translate.MetricL2},
			},
		},
		{
			name:      "brackets are optional in the vector literal",
			sql:       `SELECT * FROM items ORDER BY embedding <=> '1, 2, 3' LIMIT 5`,
			wantTable: "items",
			want: &translate.Plan{
				Limit:  5,
				Source: translate.SourceFilter{FetchSource: true},
				Search: &translate.SearchSpec{Field: "embedding", Vector: []float32{1, 2, 3}, Metric: translate.MetricCosine},
			},
		},
		{
			name:      "qualifiers strip on the distance field",
			sql:       `SELECT * FROM items i ORDER BY i.embedding <=> '[1]' LIMIT 1`,
			wantTable: "items",
			want: &translate.Plan{
				Limit:  1,
				Source: translate.SourceFilter{FetchSource: true},
				Search: &translate.SearchSpec{Field: "embedding", Vector: []float32{1}, Metric: translate.MetricCosine},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sel, got := planOf(t, tt.sql, vectorSchema)
			if sel.Table != tt.wantTable {
				t.Fatalf("Select.Table = %q, want %q", sel.Table, tt.wantTable)
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
				t.Fatalf("Plan() =\n  got  %+v\n  want %+v", got, tt.want)
			}
		})
	}
}
