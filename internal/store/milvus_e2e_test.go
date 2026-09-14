package store

import (
	"context"
	"fmt"

	"testing"
	"time"

	"github.com/huanghaoyuanhhy/lyrebird/internal/milvustest"
	"github.com/huanghaoyuanhhy/lyrebird/internal/translate"
)

// The e2e suite runs against a real Milvus/Zilliz Cloud instance and is
// guarded by environment variables — credentials never live in this repo
// (see internal/milvustest). Unset variables skip the suite, so plain
// `go test ./...` stays green.

// fetchSource is the plain "return _source" filter most plans carry; the
// struct's zero value means _source: false by contract.
func fetchSource() translate.SourceFilter {
	return translate.SourceFilter{FetchSource: true}
}

func TestMilvusExecutorE2E(t *testing.T) {
	cfg, ok := milvustest.FromEnv()
	if !ok {
		t.Skipf("set %s and %s to run the Milvus e2e suite", milvustest.URIEnv, milvustest.TokenEnv)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	if err := milvustest.Seed(ctx, cfg); err != nil {
		t.Fatalf("seed fixture: %v", err)
	}

	exec, err := NewMilvusExecutor(ctx, MilvusConfig{URI: cfg.URI, Token: cfg.Token})
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	defer exec.Close(ctx)

	t.Run("match_all returns every entity", func(t *testing.T) {
		res := mustSearch(t, ctx, exec, &translate.Plan{Limit: 10, Source: fetchSource()})
		if res.Total != 5 || len(res.Hits) != 5 {
			t.Fatalf("total=%d hits=%d, want 5/5", res.Total, len(res.Hits))
		}
		hit := res.Hits[0]
		if hit.ID == "" {
			t.Error("hit should carry the primary key as _id")
		}
		if _, ok := hit.Source["name"]; !ok {
			t.Errorf("default _source should carry all fields, got %v", hit.Source)
		}
	})

	t.Run("paging windows without sort", func(t *testing.T) {
		res := mustSearch(t, ctx, exec, &translate.Plan{Offset: 2, Limit: 2})
		if res.Total != 5 {
			t.Errorf("total=%d, want 5 (window knows it is underfilled)", res.Total)
		}
		if len(res.Hits) != 2 {
			t.Fatalf("hits=%d, want 2", len(res.Hits))
		}
	})

	t.Run("range filter counts exactly", func(t *testing.T) {
		plan := &translate.Plan{
			Expr:  translate.Compare{Op: translate.Gt, Field: "price", Value: translate.FloatValue(10)},
			Limit: 10,
		}
		if res := mustSearch(t, ctx, exec, plan); res.Total != 4 {
			t.Errorf("total=%d, want 4 matches for price > 10", res.Total)
		}
	})

	t.Run("terms filter maps to IN", func(t *testing.T) {
		plan := &translate.Plan{
			Expr: translate.InList{Field: "id", Values: []translate.Value{
				translate.IntValue(1), translate.IntValue(2), translate.IntValue(3),
			}},
			Limit:  10,
			Source: fetchSource(),
		}
		res := mustSearch(t, ctx, exec, plan)
		if res.Total != 3 || len(res.Hits) != 3 {
			t.Fatalf("total=%d hits=%d, want 3/3", res.Total, len(res.Hits))
		}
		for _, h := range res.Hits {
			if _, ok := h.Source["price"]; !ok {
				t.Errorf("hit %s missing price in _source: %v", h.ID, h.Source)
			}
		}
	})

	t.Run("sort orders globally through the pk cursor", func(t *testing.T) {
		plan := &translate.Plan{
			Sort:   []translate.SortClause{{Field: "price", Desc: true}},
			Offset: 1,
			Limit:  2,
			Source: fetchSource(),
		}
		res := mustSearch(t, ctx, exec, plan)
		if res.Total != 5 {
			t.Errorf("total=%d, want 5 (sort scans all matches)", res.Total)
		}
		prices := []float64{}
		for _, h := range res.Hits {
			p, ok := h.Source["price"].(float64)
			if !ok {
				t.Fatalf("hit %s has no price: %v", h.ID, h.Source)
			}
			prices = append(prices, p)
		}
		// fixture prices desc: 30, 20, 15, 10.5, 5 → window [1,3) is 20, 15
		if len(prices) != 2 || prices[0] != 20 || prices[1] != 15 {
			t.Errorf("window prices = %v, want [20 15]", prices)
		}
	})

	t.Run("multi-key sort with tie-break", func(t *testing.T) {
		plan := &translate.Plan{
			Sort: []translate.SortClause{
				{Field: "active"}, {Field: "price", Desc: true},
			},
			Limit:  10,
			Source: fetchSource(),
		}
		res := mustSearch(t, ctx, exec, plan)
		// active false rows: id 2 (20), id 5 (5) → first two after sort
		if len(res.Hits) < 2 {
			t.Fatalf("hits=%d, want at least 2", len(res.Hits))
		}
		if res.Hits[0].Source["active"] != false || res.Hits[1].Source["active"] != false {
			t.Fatalf("active=false rows should come first: %v", res.Hits)
		}
		if res.Hits[0].Source["price"] != 20.0 {
			t.Errorf("tie on active should break on price desc, got %v", res.Hits[0].Source["price"])
		}
	})

	t.Run("_source includes project the hit", func(t *testing.T) {
		plan := &translate.Plan{
			Limit: 1,
			Source: translate.SourceFilter{
				FetchSource: true,
				Includes:    []string{"name"},
			},
		}
		res := mustSearch(t, ctx, exec, plan)
		src := res.Hits[0].Source
		if _, ok := src["name"]; !ok {
			t.Errorf("name should survive includes: %v", src)
		}
		if _, ok := src["price"]; ok {
			t.Errorf("price should be projected away: %v", src)
		}
		if res.Hits[0].ID == "" {
			t.Error("_id must survive projection")
		}
	})

	t.Run("_source false returns hits without source", func(t *testing.T) {
		plan := &translate.Plan{
			Limit:  1,
			Source: translate.SourceFilter{FetchSource: false},
		}
		res := mustSearch(t, ctx, exec, plan)
		if len(res.Hits[0].Source) != 0 {
			t.Errorf("source = %v, want empty", res.Hits[0].Source)
		}
	})

	t.Run("size zero counts without hits", func(t *testing.T) {
		res := mustSearch(t, ctx, exec, &translate.Plan{})
		if res.Total != 5 || len(res.Hits) != 0 {
			t.Errorf("total=%d hits=%d, want 5/0", res.Total, len(res.Hits))
		}
	})

	t.Run("exists maps to is not null", func(t *testing.T) {
		res := mustSearch(t, ctx, exec, &translate.Plan{
			Expr:  translate.NotNull{Field: "price"},
			Limit: 10,
		})
		if res.Total != 5 {
			t.Errorf("total=%d, want 5 (all rows have price)", res.Total)
		}
	})

	t.Run("bool compare", func(t *testing.T) {
		res := mustSearch(t, ctx, exec, &translate.Plan{
			Expr:  translate.Compare{Op: translate.Eq, Field: "active", Value: translate.BoolValue(true)},
			Limit: 10,
		})
		if res.Total != 3 {
			t.Errorf("total=%d, want 3 active rows", res.Total)
		}
	})

	t.Run("NoMatch short-circuits to empty", func(t *testing.T) {
		res := mustSearch(t, ctx, exec, &translate.Plan{NoMatch: true, Limit: 10})
		if res.Total != 0 || len(res.Hits) != 0 {
			t.Errorf("total=%d hits=%d, want 0/0", res.Total, len(res.Hits))
		}
	})

	t.Run("Schema view maps milvus types", func(t *testing.T) {
		schema, err := exec.Schema(ctx, milvustest.Collection)
		if err != nil {
			t.Fatal(err)
		}
		want := translate.MapSchema{
			"id":         translate.TypeNumber,
			"name":       translate.TypeKeyword,
			"price":      translate.TypeNumber,
			"qty":        translate.TypeNumber,
			"active":     translate.TypeBool,
			"created_ms": translate.TypeNumber,
			"emb":        translate.TypeUnknown,
		}
		for f, w := range want {
			if got := schema.FieldType(f); got != w {
				t.Errorf("field %s = %s, want %s", f, got, w)
			}
		}
	})

	t.Run("missing collection maps to ErrCollectionNotFound", func(t *testing.T) {
		_, err := exec.Search(ctx, "lyrebird_e2e_missing", &translate.Plan{Limit: 1})
		if err == nil {
			t.Fatal("expected an error for a missing collection")
		}
		want := fmt.Sprintf("%v: lyrebird_e2e_missing", ErrCollectionNotFound)
		if err.Error() != want {
			t.Errorf("err = %v, want %q", err, want)
		}
	})
}

func mustSearch(t *testing.T, ctx context.Context, exec *MilvusExecutor, plan *translate.Plan) *SearchResult {
	t.Helper()
	res, err := exec.Search(ctx, milvustest.Collection, plan)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	return res
}
