package esserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/huanghaoyuanhhy/lyrebird/internal/catalog"
	"github.com/huanghaoyuanhhy/lyrebird/internal/store"
	"github.com/huanghaoyuanhhy/lyrebird/internal/translate"
)

// fakeExecutor returns canned results and records what it was asked. It
// also carries the cluster surface the catalog endpoints read: collections
// (fixed metas) and a canned per-index stats map.
type fakeExecutor struct {
	schema translate.Schema
	result *store.SearchResult
	err    error
	metas  []catalog.CollectionMeta
	stats  map[string]int64

	gotIndex string
	gotPlan  *translate.Plan

	// writeCalls records every Insert/Upsert the fake was asked to run.
	writeCalls []writeCall
	insertErr  error
	// insertIDs plays the store-assigned primary keys (auto-id inserts).
	insertIDs []string
}

// writeCall is one recorded write.
type writeCall struct {
	Collection string
	Rows       []translate.WriteRow
	Upsert     bool
}

// Describe implements store.Executor from the fake's metas.
func (f *fakeExecutor) Describe(ctx context.Context, collection string) (catalog.CollectionMeta, error) {
	for _, m := range f.metas {
		if m.Name == collection {
			return m, nil
		}
	}
	return catalog.CollectionMeta{}, fmt.Errorf("%w: %s", store.ErrCollectionNotFound, collection)
}

// Insert implements store.Executor: the same strict validation the real
// executor runs (so handler tests exercise the full rejection path), then
// records the call.
func (f *fakeExecutor) Insert(ctx context.Context, collection string, rows []translate.WriteRow) (store.WriteResult, error) {
	meta, err := f.Describe(ctx, collection)
	if err != nil {
		return store.WriteResult{}, err
	}
	if err := store.ValidateWriteRows(meta, rows); err != nil {
		return store.WriteResult{}, err
	}
	if f.insertErr != nil {
		return store.WriteResult{}, f.insertErr
	}
	f.writeCalls = append(f.writeCalls, writeCall{Collection: collection, Rows: rows})
	return store.WriteResult{Count: int64(len(rows)), IDs: f.insertIDs}, nil
}

// Upsert implements store.Executor, validating and recording like Insert.
func (f *fakeExecutor) Upsert(ctx context.Context, collection string, rows []translate.WriteRow) (store.WriteResult, error) {
	meta, err := f.Describe(ctx, collection)
	if err != nil {
		return store.WriteResult{}, err
	}
	if err := store.ValidateWriteRows(meta, rows); err != nil {
		return store.WriteResult{}, err
	}
	if f.insertErr != nil {
		return store.WriteResult{}, f.insertErr
	}
	f.writeCalls = append(f.writeCalls, writeCall{Collection: collection, Rows: rows, Upsert: true})
	return store.WriteResult{Count: int64(len(rows))}, nil
}

func (f *fakeExecutor) Search(ctx context.Context, collection string, plan *translate.Plan) (*store.SearchResult, error) {
	f.gotIndex = collection
	f.gotPlan = plan
	if f.result != nil {
		return f.result, nil
	}
	return &store.SearchResult{}, f.err
}

func (f *fakeExecutor) Schema(ctx context.Context, collection string) (translate.Schema, error) {
	return f.schema, nil
}

// Database implements store.Cluster: one fake, any database.
func (f *fakeExecutor) Database(db string) (store.Executor, error) { return f, nil }

// DefaultDatabase implements store.Cluster.
func (f *fakeExecutor) DefaultDatabase() string { return "default" }

// Databases implements store.Cluster.
func (f *fakeExecutor) Databases(ctx context.Context) ([]string, error) {
	return []string{"default"}, nil
}

// ListCollections implements store.Cluster from the fake's metas.
func (f *fakeExecutor) ListCollections(ctx context.Context, db string) ([]string, error) {
	names := make([]string, 0, len(f.metas))
	for _, m := range f.metas {
		names = append(names, m.Name)
	}
	return names, nil
}

// Collection implements store.Cluster by name lookup.
func (f *fakeExecutor) Collection(ctx context.Context, db, name string) (catalog.CollectionMeta, error) {
	for _, m := range f.metas {
		if m.Name == name {
			return m, nil
		}
	}
	return catalog.CollectionMeta{}, fmt.Errorf("%w: %s", store.ErrCollectionNotFound, name)
}

// CollectionStats implements store.Cluster from the canned stats map.
func (f *fakeExecutor) CollectionStats(ctx context.Context, db, name string) (int64, error) {
	return f.stats[name], nil
}

func serve(t *testing.T, exec store.Cluster) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(New(exec, zap.NewNop()))
	t.Cleanup(srv.Close)
	return srv
}

func searchRequest(t *testing.T, srv *httptest.Server, method, path, body string) (int, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, srv.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("response is not JSON: %v (%s)", err, raw)
	}
	return resp.StatusCode, out
}

func TestSearchEnvelope(t *testing.T) {
	exec := &fakeExecutor{
		result: &store.SearchResult{
			Total: 5,
			Hits: []store.Hit{
				{ID: "1", Source: map[string]any{"name": "alpha", "price": 10.5}},
				{ID: "2", Source: map[string]any{"name": "bravo"}},
			},
		},
	}
	srv := serve(t, exec)

	status, body := searchRequest(t, srv, http.MethodPost, "/products/_search", `{"query":{"match_all":{}}}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %v", status, body)
	}
	if v := body["took"]; v == nil {
		t.Error("took missing from the envelope")
	}
	if body["timed_out"] != false {
		t.Error("timed_out should be present and false")
	}
	if _, ok := body["_shards"]; !ok {
		t.Error("_shards missing from the envelope")
	}

	hits := body["hits"].(map[string]any)
	total := hits["total"].(map[string]any)
	if total["value"] != float64(5) || total["relation"] != "eq" {
		t.Errorf("hits.total = %v, want {5 eq}", total)
	}
	if hits["max_score"] != 1.0 {
		t.Errorf("max_score = %v, want 1.0", hits["max_score"])
	}
	items := hits["hits"].([]any)
	first := items[0].(map[string]any)
	if first["_index"] != "products" || first["_id"] != "1" || first["_score"] != 1.0 {
		t.Errorf("hit metadata = %v", first)
	}
	src := first["_source"].(map[string]any)
	if src["name"] != "alpha" {
		t.Errorf("hit _source = %v", src)
	}
}

func TestSearchSortedHitsCarryNullScores(t *testing.T) {
	exec := &fakeExecutor{
		result: &store.SearchResult{
			Total: 2,
			Hits:  []store.Hit{{ID: "1", Source: map[string]any{"price": 20.0}}},
		},
	}
	srv := serve(t, exec)

	_, body := searchRequest(t, srv, http.MethodPost, "/products/_search",
		`{"query":{"match_all":{}},"sort":[{"price":"desc"}]}`)
	if exec.gotPlan.Sort[0].Field != "price" || !exec.gotPlan.Sort[0].Desc {
		t.Errorf("sort did not reach the executor plan: %v", exec.gotPlan.Sort)
	}
	hits := body["hits"].(map[string]any)
	if hits["max_score"] != nil {
		t.Errorf("max_score = %v, want null for a sorted search", hits["max_score"])
	}
	item := hits["hits"].([]any)[0].(map[string]any)
	if item["_score"] != nil {
		t.Errorf("_score = %v, want null for a sorted search", item["_score"])
	}
}

func TestSearchOmitsSourceWhenFetchSourceFalse(t *testing.T) {
	exec := &fakeExecutor{
		result: &store.SearchResult{
			Total: 1,
			Hits:  []store.Hit{{ID: "7", Source: nil}},
		},
	}
	srv := serve(t, exec)

	_, body := searchRequest(t, srv, http.MethodPost, "/products/_search",
		`{"query":{"match_all":{}},"_source":false}`)
	item := body["hits"].(map[string]any)["hits"].([]any)[0].(map[string]any)
	if _, ok := item["_source"]; ok {
		t.Errorf("hit should not carry a _source member: %v", item)
	}
}

func TestSearchTranslateErrorsRenderNatively(t *testing.T) {
	exec := &fakeExecutor{}
	srv := serve(t, exec)

	status, body := searchRequest(t, srv, http.MethodPost, "/products/_search", `{"from":-1}`)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d", status)
	}
	errBody := body["error"].(map[string]any)
	if errBody["type"] != "illegal_argument_exception" {
		t.Errorf("error.type = %v, want illegal_argument_exception", errBody["type"])
	}
	if body["status"] != float64(400) {
		t.Errorf("status member = %v", body["status"])
	}
	if errBody["root_cause"] == nil {
		t.Error("root_cause missing from the error envelope")
	}
}

func TestSearchUnsupportedFeatureFailsFast(t *testing.T) {
	exec := &fakeExecutor{}
	srv := serve(t, exec)

	status, body := searchRequest(t, srv, http.MethodPost, "/products/_search", `{"aggs":{"by":{"terms":{"field":"x"}}}}`)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d", status)
	}
	if body["error"].(map[string]any)["type"] != "unsupported_exception" {
		t.Errorf("error.type = %v", body["error"].(map[string]any)["type"])
	}
}

func TestSearchMissingIndexRendersIndexNotFound(t *testing.T) {
	exec := &fakeExecutor{err: store.ErrCollectionNotFound}
	srv := serve(t, exec)

	status, body := searchRequest(t, srv, http.MethodPost, "/ghosts/_search", `{}`)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d", status)
	}
	errBody := body["error"].(map[string]any)
	if errBody["type"] != "index_not_found_exception" {
		t.Errorf("error.type = %v", errBody["type"])
	}
	if errBody["index"] != "ghosts" {
		t.Errorf("error.index = %v", errBody["index"])
	}
	if !strings.Contains(errBody["reason"].(string), "no such index [ghosts]") {
		t.Errorf("reason = %v", errBody["reason"])
	}
}

func TestSearchExecutorFailureRendersPhaseError(t *testing.T) {
	exec := &fakeExecutor{err: errors.New("boom")}
	srv := serve(t, exec)

	status, body := searchRequest(t, srv, http.MethodPost, "/products/_search", `{}`)
	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d", status)
	}
	if body["error"].(map[string]any)["type"] != "search_phase_execution_exception" {
		t.Errorf("error.type = %v", body["error"].(map[string]any)["type"])
	}
}

func TestSearchKnnRidesTheVectorPath(t *testing.T) {
	exec := &fakeExecutor{
		result: &store.SearchResult{
			Total: 2,
			Hits:  []store.Hit{{ID: "1", Source: map[string]any{"name": "alpha"}}, {ID: "2"}},
		},
	}
	srv := serve(t, exec)

	status, body := searchRequest(t, srv, http.MethodPost, "/products/_search",
		`{"knn":{"field":"emb","query_vector":[0.1,0.2],"k":2,"num_candidates":100,"filter":{"term":{"status":"ok"}}}}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %v", status, body)
	}

	spec := exec.gotPlan.Search
	if spec == nil {
		t.Fatal("knn did not reach the executor as plan.Search")
	}
	if spec.Field != "emb" {
		t.Errorf("spec.Field = %q, want emb", spec.Field)
	}
	if len(spec.Vector) != 2 || spec.Vector[0] != 0.1 || spec.Vector[1] != 0.2 {
		t.Errorf("spec.Vector = %v, want [0.1 0.2]", spec.Vector)
	}
	if spec.Metric != "" {
		t.Errorf("spec.Metric = %q, want empty (ES takes the metric from the mapping)", spec.Metric)
	}
	if exec.gotPlan.Limit != 2 {
		t.Errorf("limit = %d, want k=2", exec.gotPlan.Limit)
	}
	rendered, err := translate.Render(exec.gotPlan.Expr)
	if err != nil {
		t.Fatal(err)
	}
	if rendered != `status == "ok"` {
		t.Errorf("filter expr = %q, want the knn filter", rendered)
	}

	hits := body["hits"].(map[string]any)
	if hits["total"].(map[string]any)["value"] != float64(2) {
		t.Errorf("total = %v, want the top-k row count", hits["hits"])
	}
	first := hits["hits"].([]any)[0].(map[string]any)
	if first["_score"] != 1.0 {
		t.Errorf("_score = %v, want the constant 1.0 until Phase 5", first["_score"])
	}
}

func TestSearchRoutesAndParams(t *testing.T) {
	exec := &fakeExecutor{}
	srv := serve(t, exec)

	t.Run("GET with source parameter", func(t *testing.T) {
		status, _ := searchRequest(t, srv, http.MethodGet,
			"/products/_search?source="+`%7B%22from%22%3A3%7D`, "")
		if status != http.StatusOK {
			t.Fatalf("status = %d", status)
		}
		if exec.gotPlan.Offset != 3 {
			t.Errorf("offset = %d, want 3 from the source parameter", exec.gotPlan.Offset)
		}
	})

	t.Run("q parameter fails fast", func(t *testing.T) {
		status, body := searchRequest(t, srv, http.MethodGet, "/products/_search?q=price:10", "")
		if status != http.StatusBadRequest {
			t.Fatalf("status = %d", status)
		}
		if body["error"].(map[string]any)["type"] != "unsupported_exception" {
			t.Errorf("error.type = %v", body["error"].(map[string]any)["type"])
		}
	})

	t.Run("multi-index path fails fast", func(t *testing.T) {
		status, body := searchRequest(t, srv, http.MethodPost, "/a,b/_search", `{}`)
		if status != http.StatusBadRequest {
			t.Fatalf("status = %d", status)
		}
		if body["error"].(map[string]any)["type"] != "unsupported_exception" {
			t.Errorf("error.type = %v", body["error"].(map[string]any)["type"])
		}
	})

	t.Run("size zero search keeps the total", func(t *testing.T) {
		exec.result = &store.SearchResult{Total: 9}
		status, body := searchRequest(t, srv, http.MethodPost, "/products/_search", `{"size":0}`)
		if status != http.StatusOK {
			t.Fatalf("status = %d", status)
		}
		hits := body["hits"].(map[string]any)
		if hits["total"].(map[string]any)["value"] != float64(9) {
			t.Errorf("total = %v, want 9", hits["total"])
		}
		if got := len(hits["hits"].([]any)); got != 0 {
			t.Errorf("hits = %d, want 0", got)
		}
		if hits["max_score"] != nil {
			t.Errorf("max_score = %v, want null with no hits", hits["max_score"])
		}
	})
}

func TestClusterInfoKeepsProductHeader(t *testing.T) {
	srv := serve(t, &fakeExecutor{})
	resp, err := srv.Client().Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("X-elastic-product") != "Elasticsearch" {
		t.Errorf("X-elastic-product = %q", resp.Header.Get("X-elastic-product"))
	}
}

func TestPanicRecoveryReturnsEnvelope(t *testing.T) {
	// A panicking executor must surface as a 500 envelope, not a dropped
	// connection.
	exec := panickyExecutor{}
	srv := serve(t, exec)

	status, body := searchRequest(t, srv, http.MethodPost, "/products/_search", `{}`)
	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d", status)
	}
	if body["error"].(map[string]any)["type"] != "lyrebird_error" {
		t.Errorf("error.type = %v", body["error"].(map[string]any)["type"])
	}
}

type panickyExecutor struct{}

func (panickyExecutor) Search(ctx context.Context, collection string, plan *translate.Plan) (*store.SearchResult, error) {
	panic("boom")
}

func (panickyExecutor) Schema(ctx context.Context, collection string) (translate.Schema, error) {
	return translate.MapSchema{}, nil
}

// Database implements store.Cluster: the panic test only reads.
func (panickyExecutor) Database(db string) (store.Executor, error) { return nil, nil }

// DefaultDatabase implements store.Cluster.
func (panickyExecutor) DefaultDatabase() string { return "default" }

// Databases implements store.Cluster.
func (panickyExecutor) Databases(ctx context.Context) ([]string, error) { return nil, nil }

// ListCollections implements store.Cluster.
func (panickyExecutor) ListCollections(ctx context.Context, db string) ([]string, error) {
	return nil, nil
}

// Collection implements store.Cluster.
func (panickyExecutor) Collection(ctx context.Context, db, name string) (catalog.CollectionMeta, error) {
	return catalog.CollectionMeta{}, fmt.Errorf("%w: %s", store.ErrCollectionNotFound, name)
}

// CollectionStats implements store.Cluster.
func (panickyExecutor) CollectionStats(ctx context.Context, db, name string) (int64, error) {
	return 0, nil
}

// Describe implements store.Executor: the panic test only searches.
func (panickyExecutor) Describe(ctx context.Context, collection string) (catalog.CollectionMeta, error) {
	return catalog.CollectionMeta{}, fmt.Errorf("%w: %s", store.ErrCollectionNotFound, collection)
}

// Insert implements store.Executor: the panic test only searches.
func (panickyExecutor) Insert(ctx context.Context, collection string, rows []translate.WriteRow) (store.WriteResult, error) {
	return store.WriteResult{}, nil
}

// Upsert implements store.Executor: the panic test only searches.
func (panickyExecutor) Upsert(ctx context.Context, collection string, rows []translate.WriteRow) (store.WriteResult, error) {
	return store.WriteResult{}, nil
}
