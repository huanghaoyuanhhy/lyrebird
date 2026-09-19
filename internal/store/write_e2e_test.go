package store

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/huanghaoyuanhhy/lyrebird/internal/milvustest"
	"github.com/huanghaoyuanhhy/lyrebird/internal/translate"
)

// TestMilvusExecutorWriteE2E drives the strict write path against the real
// store: matching rows land, mismatching input is rejected before anything
// is sent, and upsert replaces whole rows. Skips without credentials.
func TestMilvusExecutorWriteE2E(t *testing.T) {
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

	fullRow := func(id int64, name string) translate.WriteRow {
		return translate.WriteRow{
			"id":     json.Number(itoa(id)),
			"name":   name,
			"price":  json.Number("2.5"),
			"active": true,
			"emb":    []float32{0.5, 0.5},
		}
	}

	t.Run("describe answers the strict schema", func(t *testing.T) {
		meta, err := exec.Describe(ctx, milvustest.WriteCollection)
		if err != nil {
			t.Fatalf("describe: %v", err)
		}
		if len(meta.Fields) == 0 {
			t.Fatal("no fields")
		}
		pk := false
		for _, f := range meta.Fields {
			if f.PrimaryKey {
				pk = true
			}
			if f.Name == "emb" && f.Dim != 2 {
				t.Errorf("emb dim = %d, want 2", f.Dim)
			}
		}
		if !pk {
			t.Error("no primary key field")
		}
	})

	t.Run("insert, then read back", func(t *testing.T) {
		res, err := exec.Insert(ctx, milvustest.WriteCollection,
			[]translate.WriteRow{fullRow(1, "one"), fullRow(2, "two")})
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
		if res.Count != 2 {
			t.Fatalf("count = %d", res.Count)
		}

		out := pollHits(t, ctx, exec, "name", "one")
		if len(out.Hits) != 1 || out.Hits[0].Source["price"] != float64(2.5) {
			t.Fatalf("read back = %v", out.Hits)
		}
	})

	t.Run("strict rejections before anything is sent", func(t *testing.T) {
		cases := []struct {
			name  string
			row   translate.WriteRow
			class WriteErrClass
		}{
			{"unknown field", merge2(fullRow(3, "three"), "junk", "x"), ClassUnknownField},
			{"type mismatch", merge2(fullRow(3, "three"), "price", "cheap"), ClassType},
			{"dim mismatch", merge2(fullRow(3, "three"), "emb", []float32{1}), ClassDim},
			{"null into required", merge2(fullRow(3, "three"), "name", nil), ClassNull},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				_, err := exec.Insert(ctx, milvustest.WriteCollection, []translate.WriteRow{tc.row})
				var we *WriteError
				if !errors.As(err, &we) || we.Class != tc.class {
					t.Fatalf("err = %v, want class %s", err, tc.class)
				}
			})
		}
	})

	t.Run("upsert replaces the row whole", func(t *testing.T) {
		row := fullRow(1, "one")
		row["price"] = json.Number("9.5")
		row["note"] = "annotated"
		if _, err := exec.Upsert(ctx, milvustest.WriteCollection, []translate.WriteRow{row}); err != nil {
			t.Fatalf("upsert: %v", err)
		}

		out := pollHits(t, ctx, exec, "name", "one")
		if len(out.Hits) != 1 {
			t.Fatalf("hits = %d", len(out.Hits))
		}
		src := out.Hits[0].Source
		if src["price"] != float64(9.5) || src["note"] != "annotated" {
			t.Fatalf("source = %v", src)
		}
	})

	t.Run("auto id insert reports the assigned key", func(t *testing.T) {
		res, err := exec.Insert(ctx, milvustest.AutoIDCollection,
			[]translate.WriteRow{{"v": "generated"}})
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
		if res.Count != 1 || len(res.IDs) != 1 || res.IDs[0] == "" {
			t.Fatalf("result = %#v, want one assigned id", res)
		}
	})

	t.Run("string pk insert", func(t *testing.T) {
		res, err := exec.Insert(ctx, milvustest.StringPKCollection,
			[]translate.WriteRow{{"id": "alpha", "v": "x"}})
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
		if res.Count != 1 {
			t.Fatalf("count = %d", res.Count)
		}
	})

	t.Run("missing collection", func(t *testing.T) {
		if _, err := exec.Insert(ctx, "nope_nope", []translate.WriteRow{{"id": json.Number("1")}}); !errors.Is(err, ErrCollectionNotFound) {
			t.Fatalf("err = %v, want ErrCollectionNotFound", err)
		}
	})
}

func itoa(n int64) string {
	return strconv.FormatInt(n, 10)
}

// pollHits waits out Milvus's growing-segment visibility window: a row is
// queryable shortly after its insert lands, and Strong consistency narrows
// that to (typically) sub-second — but it is a platform fact, not a
// guarantee, so the read-backs poll instead of assuming.
func pollHits(t *testing.T, ctx context.Context, exec *MilvusExecutor, field, value string) *SearchResult {
	t.Helper()
	plan := &translate.Plan{
		Limit:  10,
		Expr:   translate.Compare{Op: translate.Eq, Field: field, Value: translate.StringValue(value)},
		Source: fetchSource(),
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		out, err := exec.Search(ctx, milvustest.WriteCollection, plan)
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if len(out.Hits) > 0 || time.Now().After(deadline) {
			return out
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func merge2(base translate.WriteRow, key string, val any) translate.WriteRow {
	out := translate.WriteRow{}
	for k, v := range base {
		out[k] = v
	}
	out[key] = val
	return out
}
