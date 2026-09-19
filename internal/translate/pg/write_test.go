package pg

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/huanghaoyuanhhy/lyrebird/internal/translate"
)

// writeSchema mirrors the store fixture: two scalars, one vector.
var writeSchema = translate.MapSchema{
	"id": translate.TypeNumber, "name": translate.TypeKeyword,
	"price": translate.TypeNumber, "emb": translate.TypeVector,
}

func TestParseInsertAndLower(t *testing.T) {
	ins, err := ParseInsert("INSERT INTO items (id, name, emb) VALUES (7, 'hi', '[0.1, 0.2]')")
	if err != nil {
		t.Fatalf("ParseInsert: %v", err)
	}
	if ins.Table != "items" {
		t.Fatalf("table = %q", ins.Table)
	}
	rows, err := ins.Rows(writeSchema)
	if err != nil {
		t.Fatalf("Rows: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
	if rows[0]["id"] != json.Number("7") {
		t.Fatalf("id = %#v, want json.Number(7) (no float round-trip)", rows[0]["id"])
	}
	if rows[0]["name"] != "hi" {
		t.Fatalf("name = %#v", rows[0]["name"])
	}
	vec, ok := rows[0]["emb"].([]float32)
	if !ok || len(vec) != 2 || vec[0] != 0.1 {
		t.Fatalf("emb = %#v, want []float32{0.1, 0.2}", rows[0]["emb"])
	}
}

func TestParseInsertMultiRowAndPositional(t *testing.T) {
	// the positional default fills the schema's field order; MapSchema's
	// order is alphabetical: emb, id, name, price
	ins, err := ParseInsert("insert into items values (NULL, 1, 'a', 1.5), (NULL, 2, 'b', 2.5)")
	if err != nil {
		t.Fatalf("ParseInsert: %v", err)
	}
	rows, err := ins.Rows(translate.MapSchema{
		"id": translate.TypeNumber, "name": translate.TypeKeyword,
		"price": translate.TypeNumber, "emb": translate.TypeVector,
	})
	if err != nil {
		t.Fatalf("Rows: %v", err)
	}
	if len(rows) != 2 || rows[1]["name"] != "b" {
		t.Fatalf("rows = %#v", rows)
	}
	if v, ok := rows[0]["emb"]; !ok || v != nil {
		t.Fatalf("positional NULL lowered to %#v, want nil", v)
	}
	if rows[0]["price"] != json.Number("1.5") {
		t.Fatalf("positional price = %#v", rows[0]["price"])
	}
}

func TestParseInsertArityAndCaseFold(t *testing.T) {
	ins, err := ParseInsert("INSERT INTO items (id, name) VALUES (1, 'a', 2)")
	if err == nil {
		// the column list fixes the arity; the extra value trips at lowering
		_, err = ins.Rows(writeSchema)
	}
	if err == nil || !containsFold(err.Error(), "3 values but 2 target columns") {
		t.Fatalf("expected arity error, got %v", err)
	}
	if _, err := ParseInsert("INSERT INTO items (id, id) VALUES (1, 2)"); err == nil {
		t.Fatal("expected duplicate column error")
	}
}

func TestParseInsertNamedRejections(t *testing.T) {
	cases := []struct{ sql, want string }{
		{"INSERT INTO items SELECT * FROM other", "INSERT ... SELECT"},
		{"INSERT INTO items (id) VALUES (1) ON CONFLICT DO NOTHING", "ON CONFLICT"},
		{"INSERT INTO items (id) VALUES (DEFAULT)", "DEFAULT"},
		{"INSERT INTO items (id) VALUES ($1)", "bind parameters"},
		{"INSERT INTO public.items (id) VALUES (1)", "schema-qualified"},
		{"INSERT INTO items (id) VALUES (1) RETURNING id", "RETURNING"},
	}
	for _, tc := range cases {
		_, err := ParseInsert(tc.sql)
		if err == nil {
			t.Fatalf("%q: expected rejection", tc.sql)
		}
		if !containsFold(err.Error(), tc.want) {
			t.Fatalf("%q: error %q does not name [%s]", tc.sql, err.Error(), tc.want)
		}
	}
}

func TestParseInsertVectorLiteralNeedsVectorField(t *testing.T) {
	ins, err := ParseInsert("INSERT INTO items (id, name) VALUES (1, '[0.1]')")
	if err != nil {
		t.Fatalf("ParseInsert: %v", err)
	}
	rows, err := ins.Rows(writeSchema)
	if err != nil {
		t.Fatalf("Rows: %v", err)
	}
	// name is not a vector field: the string stays a string; the store's
	// strict check rejects it there as a type mismatch
	if rows[0]["name"] != "[0.1]" {
		t.Fatalf("name = %#v", rows[0]["name"])
	}

	ins, err = ParseInsert("INSERT INTO items (emb) VALUES ('not numbers')")
	if err != nil {
		t.Fatalf("ParseInsert: %v", err)
	}
	_, err = ins.Rows(writeSchema)
	if err == nil || !containsFold(err.Error(), "does not parse as a number") {
		t.Fatalf("vector literal error = %v", err)
	}
}

func TestParseUpdateAndLower(t *testing.T) {
	upd, err := ParseUpdate("UPDATE items SET price = 3.5, name = 'x' WHERE id = 7")
	if err != nil {
		t.Fatalf("ParseUpdate: %v", err)
	}
	set, plan, err := upd.Lower(writeSchema)
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	if set["price"] != json.Number("3.5") || set["name"] != "x" {
		t.Fatalf("set = %#v", set)
	}
	if plan == nil || plan.Expr == nil {
		t.Fatal("expected a filter plan")
	}
	if plan.Limit != 0 {
		t.Fatalf("plan.Limit = %d, caller bounds it", plan.Limit)
	}
}

func TestParseUpdateQualifiersAndVectorSet(t *testing.T) {
	upd, err := ParseUpdate("UPDATE items SET items.price = 1, emb = '[1, 0]' WHERE items.id = 7")
	if err != nil {
		t.Fatalf("ParseUpdate: %v", err)
	}
	set, _, err := upd.Lower(writeSchema)
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	vec, ok := set["emb"].([]float32)
	if !ok || len(vec) != 2 {
		t.Fatalf("emb = %#v", set["emb"])
	}

	if _, err := ParseUpdate("UPDATE items SET other.x = 1 WHERE id = 7"); err == nil {
		t.Fatal("expected other-table qualifier rejection")
	}
}

func TestParseUpdateNamedRejections(t *testing.T) {
	cases := []struct{ sql, want string }{
		{"UPDATE items SET price = 1", "WHERE"},
		{"UPDATE items SET price = price + 1 WHERE id = 7", "literals only"},
		{"UPDATE items a SET a.price = 1 WHERE a.id = 7", "aliases"},
		{"UPDATE items SET price = 1 FROM other WHERE id = 7", "FROM"},
		{"UPDATE items SET price = 1 WHERE id = 7 RETURNING id", "RETURNING"},
	}
	for _, tc := range cases {
		_, err := ParseUpdate(tc.sql)
		if err == nil {
			t.Fatalf("%q: expected rejection", tc.sql)
		}
		if !containsFold(err.Error(), tc.want) {
			t.Fatalf("%q: error %q does not name [%s]", tc.sql, err.Error(), tc.want)
		}
	}
}

func TestUpdateFalseWhereLowersToNoMatch(t *testing.T) {
	upd, err := ParseUpdate("UPDATE items SET price = 1 WHERE id = 7 AND FALSE")
	if err != nil {
		t.Fatalf("ParseUpdate: %v", err)
	}
	_, plan, err := upd.Lower(writeSchema)
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	if !plan.NoMatch {
		t.Fatal("expected NoMatch for a constant-false WHERE")
	}
}

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}
