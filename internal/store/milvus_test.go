package store

import (
	"strings"
	"testing"

	"github.com/milvus-io/milvus-sdk-go/v2/client"
	"github.com/milvus-io/milvus-sdk-go/v2/entity"

	"github.com/huanghaoyuanhhy/lyrebird/internal/translate"
)

func fixtureFields() []*entity.Field {
	return []*entity.Field{
		entity.NewField().WithName("id").WithDataType(entity.FieldTypeInt64).WithIsPrimaryKey(true),
		entity.NewField().WithName("name").WithDataType(entity.FieldTypeVarChar).WithMaxLength(64),
		entity.NewField().WithName("price").WithDataType(entity.FieldTypeDouble),
		entity.NewField().WithName("meta").WithDataType(entity.FieldTypeJSON),
	}
}

func TestBuildProjection(t *testing.T) {
	fields := fixtureFields()

	t.Run("default fetches and returns every field", func(t *testing.T) {
		p, err := buildProjection(fields, translate.SourceFilter{FetchSource: true}, nil)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"id", "name", "price", "meta"}
		if got := strings.Join(p.fetch, ","); got != strings.Join(want, ",") {
			t.Errorf("fetch = %v, want %v", p.fetch, want)
		}
		if got := strings.Join(p.source, ","); got != strings.Join(want, ",") {
			t.Errorf("source = %v, want %v", p.source, want)
		}
	})

	t.Run("_source false fetches only the primary key", func(t *testing.T) {
		p, err := buildProjection(fields, translate.SourceFilter{FetchSource: false}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(p.source) != 0 {
			t.Errorf("source = %v, want none", p.source)
		}
		if len(p.fetch) != 1 || p.fetch[0] != "id" {
			t.Errorf("fetch = %v, want [id]", p.fetch)
		}
	})

	t.Run("includes match exact names and drop unknowns", func(t *testing.T) {
		src := translate.SourceFilter{FetchSource: true, Includes: []string{"price", "nope"}}
		p, err := buildProjection(fields, src, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(p.source, ","); got != "price" {
			t.Errorf("source = %v, want [price]", p.source)
		}
		// unknown includes vanish, but the primary key still comes back for _id
		if got := strings.Join(p.fetch, ","); got != "price,id" {
			t.Errorf("fetch = %v, want [price id]", p.fetch)
		}
	})

	t.Run("prefix includes expand against the schema", func(t *testing.T) {
		src := translate.SourceFilter{FetchSource: true, Includes: []string{"p.*"}}
		p, err := buildProjection(fields, src, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(p.source) != 0 {
			t.Errorf("source = %v, want none (nothing matches p.*)", p.source)
		}
	})

	t.Run("excludes remove exact and prefix matches", func(t *testing.T) {
		src := translate.SourceFilter{FetchSource: true, Excludes: []string{"meta", "price"}}
		p, err := buildProjection(fields, src, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(p.source, ","); got != "id,name" {
			t.Errorf("source = %v, want [id name]", p.source)
		}
	})

	t.Run("sort fields join fetch but stay out of source", func(t *testing.T) {
		src := translate.SourceFilter{FetchSource: true, Includes: []string{"name"}}
		p, err := buildProjection(fields, src, []translate.SortClause{{Field: "price", Desc: true}})
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(p.fetch, ","); got != "name,price,id" {
			t.Errorf("fetch = %v, want [name price id]", p.fetch)
		}
		if got := strings.Join(p.source, ","); got != "name" {
			t.Errorf("source = %v, want [name]", p.source)
		}
	})

	t.Run("sort on a non-scalar field fails fast", func(t *testing.T) {
		src := translate.SourceFilter{FetchSource: true}
		if _, err := buildProjection(fields, src, []translate.SortClause{{Field: "meta"}}); err == nil {
			t.Fatal("expected error sorting on a JSON field")
		}
		if _, err := buildProjection(fields, src, []translate.SortClause{{Field: "ghost"}}); err == nil {
			t.Fatal("expected error sorting on an unknown field")
		}
	})
}

func TestCompareRows(t *testing.T) {
	sorts := []translate.SortClause{
		{Field: "price", Desc: true},
		{Field: "name"},
	}
	a := row{"price": 10.0, "name": "a"}
	b := row{"price": 20.0, "name": "z"}
	c := row{"price": 10.0, "name": "b"}

	if compareRows(a, b, sorts) <= 0 {
		t.Errorf("a (price 10) should sort after b (price 20) under desc price")
	}
	if compareRows(a, c, sorts) >= 0 {
		t.Errorf("tie on price should fall through to name asc")
	}

	t.Run("missing values sort last regardless of direction", func(t *testing.T) {
		missing := row{"name": "x"}
		full := row{"price": 1.0, "name": "x"}
		if compareRows(missing, full, sorts) <= 0 {
			t.Error("row missing price should sort after one with price")
		}
		if compareRows(full, missing, sorts) >= 0 {
			t.Error("row with price should sort before one missing price")
		}
	})

	t.Run("numeric int64 vs float64 compare consistently", func(t *testing.T) {
		single := []translate.SortClause{{Field: "v"}}
		if compareRows(row{"v": int64(2)}, row{"v": 1.5}, single) <= 0 {
			t.Error("int64 2 should sort after float64 1.5")
		}
	})
}

func TestWindow(t *testing.T) {
	rows := []row{{"id": int64(1)}, {"id": int64(2)}, {"id": int64(3)}}
	if got := window(rows, 0, 0); got != nil {
		t.Errorf("limit 0 = %v, want nil", got)
	}
	if got := window(rows, 5, 2); got != nil {
		t.Errorf("offset past end = %v, want nil", got)
	}
	if got := window(rows, 2, 5); len(got) != 1 || got[0]["id"] != int64(3) {
		t.Errorf("short tail = %v, want last row", got)
	}
	if got := window(rows, 1, 2); len(got) != 2 || got[0]["id"] != int64(2) {
		t.Errorf("middle window = %v, want rows 2,3", got)
	}
}

func TestTranslateFieldType(t *testing.T) {
	cases := []struct {
		field *entity.Field
		want  translate.FieldType
	}{
		{entity.NewField().WithName("k").WithDataType(entity.FieldTypeVarChar), translate.TypeKeyword},
		{entity.NewField().WithName("t").WithDataType(entity.FieldTypeVarChar).WithTypeParams("enable_analyzer", "true"), translate.TypeText},
		{entity.NewField().WithName("n").WithDataType(entity.FieldTypeInt64), translate.TypeNumber},
		{entity.NewField().WithName("f").WithDataType(entity.FieldTypeFloat), translate.TypeNumber},
		{entity.NewField().WithName("b").WithDataType(entity.FieldTypeBool), translate.TypeBool},
		{entity.NewField().WithName("j").WithDataType(entity.FieldTypeJSON), translate.TypeUnknown},
		{entity.NewField().WithName("v").WithDataType(entity.FieldTypeFloatVector).WithDim(4), translate.TypeUnknown},
	}
	for _, tc := range cases {
		if got := translateFieldType(tc.field); got != tc.want {
			t.Errorf("translateFieldType(%s) = %s, want %s", tc.field.Name, got, tc.want)
		}
	}
}

func TestHitID(t *testing.T) {
	pk := entity.NewField().WithName("id").WithDataType(entity.FieldTypeInt64).WithIsPrimaryKey(true)
	if id, err := hitID(row{"id": int64(42)}, pk); err != nil || id != "42" {
		t.Errorf("int64 pk id = %q, err %v", id, err)
	}

	strPk := entity.NewField().WithName("sku").WithDataType(entity.FieldTypeVarChar).WithIsPrimaryKey(true)
	if id, err := hitID(row{"sku": "a-1"}, strPk); err != nil || id != "a-1" {
		t.Errorf("varchar pk id = %q, err %v", id, err)
	}

	if _, err := hitID(row{}, pk); err == nil {
		t.Error("missing pk should error")
	}
}

func TestJoinAnd(t *testing.T) {
	if got := joinAnd("", "x > 1"); got != "x > 1" {
		t.Errorf("joinAnd with empty filter = %q", got)
	}
	if got := joinAnd("a > 1", "b > 2"); got != "(a > 1) and (b > 2)" {
		t.Errorf("joinAnd = %q", got)
	}
}

func TestRowsOf(t *testing.T) {
	rs := client.ResultSet{
		entity.NewColumnInt64("id", []int64{1, 2}),
		entity.NewColumnVarChar("name", []string{"a", "b"}),
	}
	rows := rowsOf(rs, []string{"id", "name"})
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if rows[1]["name"] != "b" || rows[1]["id"] != int64(2) {
		t.Errorf("row 1 = %v", rows[1])
	}
	if rowsOf(nil, []string{"id"}) != nil {
		t.Error("empty result set should yield no rows")
	}
}
