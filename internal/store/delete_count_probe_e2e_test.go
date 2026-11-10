package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/milvus-io/milvus/client/v3/column"
	"github.com/milvus-io/milvus/client/v3/entity"
	"github.com/milvus-io/milvus/client/v3/index"
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

	// The growing-segment half of the probe: same questions, but nothing
	// flushed since the inserts — the state every live client actually
	// operates in between flushes. Measured earlier (first CI run): a
	// re-delete over a tombstone in the growing state reported 1 where the
	// flushed probe reports 0, and matrix deletes of a never-existing key
	// reported 1 where the flushed probe reports 0. These assertions pin
	// the flushed-exact hypothesis; a failure prints the growing numbers.
	t.Run("growing: delete a never-existing key", func(t *testing.T) {
		if n := countByFilter("id in [9998]"); n != 0 {
			t.Fatalf("precondition: count = %d, want 0", n)
		}
		if got := deleteByExpr("id in [9998]"); got != 0 {
			t.Fatalf("growing DeleteCount = %d, want 0 (flushed probe reports 0; growing reported this number instead)", got)
		}
	})

	t.Run("growing: delete a never-existing key, equality form", func(t *testing.T) {
		// run 3's matrix anomaly: the ES term translation renders `id == 999`
		// (equality), the probe so far only measured `id in [9998]`. Measure
		// the equality form directly in the same growing state.
		if got := deleteByExpr("id == 9998"); got != 0 {
			t.Fatalf("growing equality DeleteCount = %d, want 0 (in-form reports 0; equality reported this instead)", got)
		}
	})

	t.Run("growing: re-delete over a growing tombstone", func(t *testing.T) {
		if n := countByFilter("id in [9001]"); n != 1 {
			t.Fatalf("precondition: count = %d, want 1", n)
		}
		if got := deleteByExpr("id in [9001]"); got != 1 {
			t.Fatalf("growing delete = %d, want 1", got)
		}
		if n := countByFilter("id in [9001]"); n != 0 {
			t.Fatalf("post-delete count = %d, want 0", n)
		}
		// no flush in between: the tombstone lives in a growing segment
		if got := deleteByExpr("id in [9001]"); got != 0 {
			t.Fatalf("growing re-delete DeleteCount = %d, want 0 (flushed probe reports 0; growing reported this number instead)", got)
		}
	})

	t.Run("flushed again: exactness returns", func(t *testing.T) {
		if _, err := cli.Flush(ctx, milvusclient.NewFlushOption(coll)); err != nil {
			t.Fatalf("flush: %v", err)
		}
		if got := deleteByExpr("id in [9001]"); got != 0 {
			t.Fatalf("flushed re-delete DeleteCount = %d, want 0", got)
		}
		if got := deleteByExpr("id in [9998]"); got != 0 {
			t.Fatalf("flushed never-existing DeleteCount = %d, want 0", got)
		}
		if got := deleteByExpr("id == 9998"); got != 0 {
			t.Fatalf("flushed never-existing equality DeleteCount = %d, want 0", got)
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

// reproShape recreates name with the write-path fixture shape (the same
// schema milvustest.Seed builds for lyrebird_e2e_write) and leaves it
// empty, flushed, indexed, and loaded — the state a matrix scenario's
// reset produces before its first unflushed insert.
func reproShape(ctx context.Context, cli *milvusclient.Client, name string) error {
	has, err := cli.HasCollection(ctx, milvusclient.NewHasCollectionOption(name))
	if err != nil {
		return err
	}
	if has {
		if err := cli.DropCollection(ctx, milvusclient.NewDropCollectionOption(name)); err != nil {
			return err
		}
	}
	schema := entity.NewSchema().
		WithName(name).
		WithField(entity.NewField().WithName("id").WithDataType(entity.FieldTypeInt64).WithIsPrimaryKey(true)).
		WithField(entity.NewField().WithName("name").WithDataType(entity.FieldTypeVarChar).WithMaxLength(32)).
		WithField(entity.NewField().WithName("price").WithDataType(entity.FieldTypeDouble)).
		WithField(entity.NewField().WithName("active").WithDataType(entity.FieldTypeBool)).
		WithField(entity.NewField().WithName("note").WithDataType(entity.FieldTypeVarChar).WithMaxLength(32).WithNullable(true)).
		WithField(entity.NewField().WithName("emb").WithDataType(entity.FieldTypeFloatVector).WithDim(2))
	if err := cli.CreateCollection(ctx, milvusclient.NewCreateCollectionOption(name, schema).WithShardNum(1)); err != nil {
		return err
	}
	if _, err := cli.Flush(ctx, milvusclient.NewFlushOption(name)); err != nil {
		return err
	}
	_, err = cli.CreateIndex(ctx, milvusclient.NewCreateIndexOption(name, "emb", index.NewAutoIndex(entity.L2)))
	if err != nil {
		return err
	}
	task, err := cli.LoadCollection(ctx, milvusclient.NewLoadCollectionOption(name))
	if err != nil {
		return err
	}
	return task.Await(ctx)
}

// TestMilvusDeleteAbsentKeyRepro reproduces the matrix anomaly's exact
// conditions, which the main probe does not cover: WriteCollection's data
// segments are flushed by the probe, so its "growing" measurements only
// ever had unflushed delete deltas. The matrix ran against a freshly
// recreated collection holding one *unflushed insert*, and deleting a
// never-existing key reported DeleteCount=1 there once. This test rebuilds
// exactly that state three times (fresh WriteShape collection, unflushed
// insert made visible via Strong queries, delete an absent key) and fails
// with the observed numbers if the anomaly reproduces.
func TestMilvusDeleteAbsentKeyRepro(t *testing.T) {
	cfg, ok := milvustest.FromEnv()
	if !ok {
		t.Skipf("set %s and %s to run the Milvus e2e suite", milvustest.URIEnv, milvustest.TokenEnv)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cli, err := milvustest.Connect(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer cli.Close(ctx)

	const coll = "lyrebird_e2e_wp_repro"
	t.Cleanup(func() {
		_ = cli.DropCollection(context.Background(), milvusclient.NewDropCollectionOption(coll))
	})

	insertOne := func(id int64) {
		t.Helper()
		_, err := cli.Insert(ctx, milvusclient.NewColumnBasedInsertOption(coll,
			column.NewColumnInt64("id", []int64{id}),
			column.NewColumnVarChar("name", []string{"repro"}),
			column.NewColumnDouble("price", []float64{1}),
			column.NewColumnBool("active", []bool{true}),
			column.NewColumnVarChar("note", []string{""}),
			column.NewColumnFloatVector("emb", 2, [][]float32{{1, 1}})))
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
		// deliberately no flush: the row must live in the growing segment
		deadline := time.Now().Add(15 * time.Second)
		for {
			rs, err := cli.Query(ctx, milvusclient.NewQueryOption(coll).
				WithFilter(fmt.Sprintf("id == %d", id)).
				WithOutputFields("count(*)").
				WithConsistencyLevel(entity.ClStrong))
			if err != nil {
				t.Fatalf("visibility poll: %v", err)
			}
			col := rs.GetColumn("count(*)")
			if col != nil && col.Len() > 0 {
				if v, err := col.Get(0); err == nil {
					if n, _ := v.(int64); n == 1 {
						return
					}
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("inserted row never became visible")
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	deleteExpr := func(expr string) int64 {
		t.Helper()
		res, err := cli.Delete(ctx, milvusclient.NewDeleteOption(coll).WithExpr(expr))
		if err != nil {
			t.Fatalf("delete %q: %v", expr, err)
		}
		return res.DeleteCount
	}

	for i := 0; i < 3; i++ {
		if err := reproShape(ctx, cli, coll); err != nil {
			t.Fatalf("iter %d: rebuild fixture: %v", i, err)
		}
		insertOne(int64(100 + i))

		if got := deleteExpr("id == 999"); got != 0 {
			t.Fatalf("iter %d: absent key, equality form, over an unflushed insert: DeleteCount = %d, want 0 (the matrix anomaly)", i, got)
		}
		if got := deleteExpr("id in [999]"); got != 0 {
			t.Fatalf("iter %d: absent key, in form, over an unflushed insert: DeleteCount = %d, want 0", i, got)
		}
		if got := deleteExpr(fmt.Sprintf("id == %d", 100+i)); got != 1 {
			t.Fatalf("iter %d: existing unflushed row: DeleteCount = %d, want 1", i, got)
		}
	}
}
