package pg

import (
	"testing"

	"github.com/huanghaoyuanhhy/lyrebird/internal/translate"
)

// TestThreeValuedLogic pins the SQL truth table over the NULL literal: a
// comparison against NULL is unknown on every row (whatever the column
// holds), a WHERE passes only TRUE rows, and — the part that makes a
// dedicated node necessary — NOT unknown is still unknown, so a negation
// must never resurrect a never-true predicate into "match everything".
// Every expectation below is the answer real PostgreSQL gives.
func TestThreeValuedLogic(t *testing.T) {
	tests := []struct {
		name    string
		sql     string
		noMatch bool   // plan marks NoMatch (zero rows)
		expr    string // expected Render(plan.Expr); "" = match everything
	}{
		// — comparing against the NULL literal is unknown on every row —
		{name: "equals null matches nothing",
			sql: `SELECT * FROM items WHERE views = NULL LIMIT 1`, noMatch: true},
		{name: "not equals null matches nothing",
			sql: `SELECT * FROM items WHERE views <> NULL LIMIT 1`, noMatch: true},
		{name: "less than null matches nothing",
			sql: `SELECT * FROM items WHERE views < NULL LIMIT 1`, noMatch: true},
		{name: "null on the left matches nothing",
			sql: `SELECT * FROM items WHERE NULL = views LIMIT 1`, noMatch: true},
		{name: "not (equals null) still matches nothing",
			sql: `SELECT * FROM items WHERE NOT (views = NULL) LIMIT 1`, noMatch: true},
		{name: "not (null on the left) still matches nothing",
			sql: `SELECT * FROM items WHERE NOT (NULL = views) LIMIT 1`, noMatch: true},

		// — a bare NULL in a boolean position —
		{name: "bare null matches nothing",
			sql: `SELECT * FROM items WHERE NULL LIMIT 1`, noMatch: true},
		{name: "not (bare null) still matches nothing",
			sql: `SELECT * FROM items WHERE NOT NULL LIMIT 1`, noMatch: true},

		// — IN with a NULL member —
		{name: "in with null member drops it",
			sql: `SELECT * FROM items WHERE views IN (10, NULL) LIMIT 1`, expr: `views in [10]`},
		{name: "not in with null member matches nothing",
			sql: `SELECT * FROM items WHERE views NOT IN (10, NULL) LIMIT 1`, noMatch: true},
		{name: "not (not in with null member) is the positive in",
			sql: `SELECT * FROM items WHERE NOT (views NOT IN (10, NULL)) LIMIT 1`, expr: `views in [10]`},
		{name: "not (in with null member) matches nothing",
			sql: `SELECT * FROM items WHERE NOT (views IN (10, NULL)) LIMIT 1`, noMatch: true},

		// — BETWEEN with a NULL bound —
		{name: "between with null upper bound matches nothing",
			sql: `SELECT * FROM items WHERE views BETWEEN 10 AND NULL LIMIT 1`, noMatch: true},
		{name: "between with null lower bound matches nothing",
			sql: `SELECT * FROM items WHERE views BETWEEN NULL AND 20 LIMIT 1`, noMatch: true},
		{name: "between with both bounds null matches nothing",
			sql: `SELECT * FROM items WHERE views BETWEEN NULL AND NULL LIMIT 1`, noMatch: true},
		{name: "not (between with null upper bound) keeps the lower rejects",
			sql: `SELECT * FROM items WHERE NOT (views BETWEEN 10 AND NULL) LIMIT 1`, expr: `views < 10`},
		{name: "not (between with null lower bound) keeps the upper rejects",
			sql: `SELECT * FROM items WHERE NOT (views BETWEEN NULL AND 20) LIMIT 1`, expr: `views > 20`},
		{name: "not (between with both bounds null) still matches nothing",
			sql: `SELECT * FROM items WHERE NOT (views BETWEEN NULL AND NULL) LIMIT 1`, noMatch: true},

		// — unknown inside boolean combinations renders as the constant
		// false: `false and e` filters everything, `false or e` is e, and
		// the negations are pushed through De Morgan so the constant never
		// sits under a NOT (where false would resurrect). —
		{name: "unknown and e filters everything",
			sql:  `SELECT * FROM items WHERE views = NULL AND active = true LIMIT 1`,
			expr: `(1 != 1) and (active == true)`},
		{name: "unknown or e is e",
			sql:  `SELECT * FROM items WHERE views = NULL OR active = true LIMIT 1`,
			expr: `(1 != 1) or (active == true)`},
		{name: "not (unknown or e) filters everything via the de morgan and",
			sql:  `SELECT * FROM items WHERE NOT (views = NULL OR active = true) LIMIT 1`,
			expr: `(1 != 1) and (not (active == true))`},
		{name: "not (unknown and e) keeps the not-e rows via the de morgan or",
			sql:  `SELECT * FROM items WHERE NOT (views = NULL AND active = true) LIMIT 1`,
			expr: `(1 != 1) or (not (active == true))`},
		{name: "e or unknown is e",
			sql:  `SELECT * FROM items WHERE views = 10 OR NULL LIMIT 1`,
			expr: `(views == 10) or (1 != 1)`},

		// — regressions: the plain FALSE keyword is a real false (its
		// negation matches everything), null checks keep their own
		// operators, and predicates without an unknown fold as before. —
		{name: "false keyword matches nothing",
			sql: `SELECT * FROM items WHERE FALSE LIMIT 1`, noMatch: true},
		{name: "not false matches everything",
			sql: `SELECT * FROM items WHERE NOT FALSE LIMIT 1`, expr: ""},
		{name: "not (is null) flips the operator",
			sql: `SELECT * FROM items WHERE NOT (views IS NULL) LIMIT 1`, expr: `views is not null`},
		{name: "not not clears",
			sql: `SELECT * FROM items WHERE NOT NOT (views = 1) LIMIT 1`, expr: `views == 1`},
		{name: "not over and without unknown keeps the not wrapper",
			sql:  `SELECT * FROM items WHERE NOT (views = 1 AND active = true) LIMIT 1`,
			expr: `not ((views == 1) and (active == true))`},
		{name: "not over a plain comparison",
			sql: `SELECT * FROM items WHERE NOT (views = 1) LIMIT 1`, expr: `not (views == 1)`},
		{name: "not over a bare boolean column",
			sql: `SELECT * FROM items WHERE NOT active LIMIT 1`, expr: `not (active == true)`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, plan := planOf(t, tc.sql, testSchema)
			if plan.NoMatch != tc.noMatch {
				t.Fatalf("NoMatch = %v, want %v", plan.NoMatch, tc.noMatch)
			}
			if tc.noMatch {
				return
			}
			got, err := translate.Render(plan.Expr)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.expr {
				t.Fatalf("expr = %q, want %q", got, tc.expr)
			}
		})
	}
}
