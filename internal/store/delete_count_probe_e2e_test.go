package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/milvus-io/milvus/client/v3/column"
	"github.com/milvus-io/milvus/client/v3/entity"
	"github.com/milvus-io/milvus/client/v3/milvusclient"

	"github.com/huanghaoyuanhhy/lyrebird/internal/milvustest"
)

// TestMilvusDeleteCountProbe asks Milvus directly (raw SDK, no lyrebird
// machinery) what its delete statistic reports, on settled data. The
// exact-count hypothesis under test: an existing key deletes 1, an absent
// key (re-delete) deletes 0, an N-row filter deletes N. A failing assertion
// prints the observed number — the point of the probe is the measurement.
func TestMilvusDeleteCountProbe(t *testing.T) {
	cfg, ok := milvustest.FromEnv()
	if !ok {
		t.Skipf("set %s and %s to run the Milvus e2e suite", milvustest.URIEnv, milvustest.TokenEnv)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if err := milvustest.Seed(ctx, cfg); err != nil {
		t.Fatalf("seed fixture: %v", err)
	}
	cli, err := milvustest.Connect(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer cli.Close(ctx)

	const coll = milvustest.WriteCollection
	ids := []int64{9001, 9002, 9003, 9004, 9005}
	_, err = cli.Insert(ctx, milvusclient.NewColumnBasedInsertOption(coll,
		column.NewColumnInt64("id", ids),
		column.NewColumnVarChar("name", []string{"p1", "p2", "p3", "p4", "p5"}),
		column.NewColumnDouble("price", []float64{1, 2, 3, 4, 5}),
		column.NewColumnBool("active", []bool{true, true, true, true, true}),
		column.NewColumnVarChar("note", []string{"", "", "", "", ""}),
		column.NewColumnFloatVector("emb", 2, [][]float32{
			{1, 1}, {1, 1}, {1, 1}, {1, 1}, {1, 1},
		})))
	if err != nil {
		t.Fatalf("insert probe rows: %v", err)
	}
	if _, err := cli.Flush(ctx, milvusclient.NewFlushOption(coll)); err != nil {
		t.Fatalf("flush: %v", err)
	}

	// countByFilter runs a count(*) and retries out of the visibility window.
	countByFilter := func(expr string) int64 {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for {
			rs, err := cli.Query(ctx, milvusclient.NewQueryOption(coll).
				WithFilter(expr).
				WithOutputFields("count(*)").
				WithConsistencyLevel(entity.ClStrong))
			if err != nil {
				t.Fatalf("count(%q): %v", expr, err)
			}
			col := rs.GetColumn("count(*)")
			if col != nil && col.Len() > 0 {
				if v, err := col.Get(0); err == nil {
					if n, ok := v.(int64); ok {
						return n
					}
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("count(%q): never became visible", expr)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	deleteByExpr := func(expr string) int64 {
		t.Helper()
		res, err := cli.Delete(ctx, milvusclient.NewDeleteOption(coll).WithExpr(expr))
		if err != nil {
			t.Fatalf("delete %q: %v", expr, err)
		}
		return res.DeleteCount
	}

	// settled: all five rows visible to the query path
	if n := countByFilter(fmt.Sprintf("id in [%s]", joinInts(ids))); n != 5 {
		t.Fatalf("probe precondition: count(*) = %d, want 5 visible rows", n)
	}

	t.Run("delete one existing key", func(t *testing.T) {
		if got := deleteByExpr("id in [9003]"); got != 1 {
			t.Fatalf("DeleteCount = %d, want 1 (exact hypothesis)", got)
		}
		if n := countByFilter("id in [9003]"); n != 0 {
			t.Fatalf("post-delete count = %d, want 0 (row really gone)", n)
		}
	})

	t.Run("re-delete the removed key", func(t *testing.T) {
		if got := deleteByExpr("id in [9003]"); got != 0 {
			t.Fatalf("DeleteCount = %d, want 0 (exact hypothesis: the key is absent)", got)
		}
	})

	t.Run("delete three rows by filter", func(t *testing.T) {
		// remaining probe rows: 9001, 9002, 9004, 9005 — the filter matches 3
		if n := countByFilter("id > 9001"); n != 3 {
			t.Fatalf("pre-delete count = %d, want 3", n)
		}
		if got := deleteByExpr("id > 9001"); got != 3 {
			t.Fatalf("DeleteCount = %d, want 3 (exact hypothesis)", got)
		}
		if n := countByFilter("id > 9001"); n != 0 {
			t.Fatalf("post-delete count = %d, want 0", n)
		}
	})
}

func joinInts(ids []int64) string {
	parts := make([]string, len(ids))
	for i, v := range ids {
		parts[i] = fmt.Sprint(v)
	}
	return strings.Join(parts, ",")
}
