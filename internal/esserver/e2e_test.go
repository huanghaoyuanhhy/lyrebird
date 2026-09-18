package esserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/huanghaoyuanhhy/lyrebird/internal/milvustest"
	"github.com/huanghaoyuanhhy/lyrebird/internal/store"
)

// TestEsserverFullStackE2E drives the real ES surface: an httptest server
// whose handler is the production esserver over a real MilvusExecutor. It
// skips unless LYREBIRD_TEST_MILVUS_URI / _TOKEN are set.
func TestEsserverFullStackE2E(t *testing.T) {
	cfg, ok := milvustest.FromEnv()
	if !ok {
		t.Skipf("set %s and %s to run the full-stack e2e suite", milvustest.URIEnv, milvustest.TokenEnv)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	if err := milvustest.Seed(ctx, cfg); err != nil {
		t.Fatalf("seed fixture: %v", err)
	}

	exec, err := store.NewMilvusExecutor(ctx, store.MilvusConfig{URI: cfg.URI, Token: cfg.Token})
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	defer exec.Close(ctx)

	srv := httptest.NewServer(New(exec, zap.NewNop()))
	t.Cleanup(srv.Close)

	// query POSTs a body to the fixture index and returns the raw envelope.
	query := func(t *testing.T, body string) map[string]any {
		t.Helper()
		return roundtrip(t, srv, http.MethodPost, milvustest.Collection+"/_search", body, http.StatusOK)
	}

	t.Run("match_all envelope", func(t *testing.T) {
		res := query(t, `{"query":{"match_all":{}}}`)
		hits := res["hits"].(map[string]any)
		if hits["total"].(map[string]any)["value"] != float64(5) {
			t.Errorf("total = %v, want 5", hits["total"])
		}
		items := hits["hits"].([]any)
		if len(items) != 5 {
			t.Fatalf("hits = %d, want 5", len(items))
		}
		first := items[0].(map[string]any)
		if first["_index"] != milvustest.Collection {
			t.Errorf("_index = %v", first["_index"])
		}
		if first["_score"] != 1.0 {
			t.Errorf("_score = %v, want 1.0", first["_score"])
		}
		src := first["_source"].(map[string]any)
		if _, ok := src["name"]; !ok {
			t.Errorf("_source missing name: %v", src)
		}
	})

	t.Run("term filter through the gateway", func(t *testing.T) {
		res := query(t, `{"query":{"term":{"name":"bravo"}},"_source":["price"]}`)
		items := res["hits"].(map[string]any)["hits"].([]any)
		if len(items) != 1 {
			t.Fatalf("hits = %d, want 1", len(items))
		}
		first := items[0].(map[string]any)
		if first["_id"] != "2" {
			t.Errorf("_id = %v, want 2", first["_id"])
		}
		src := first["_source"].(map[string]any)
		if src["price"] != 20.0 {
			t.Errorf("_source = %v, want price 20", src)
		}
	})

	t.Run("sorted search windows globally", func(t *testing.T) {
		res := query(t, `{"query":{"range":{"price":{"gte":5}}},"sort":[{"price":"asc"}],"from":2,"size":2}`)
		hits := res["hits"].(map[string]any)
		if hits["total"].(map[string]any)["value"] != float64(5) {
			t.Errorf("total = %v, want 5", hits["total"])
		}
		if hits["max_score"] != nil {
			t.Errorf("max_score = %v, want null under sort", hits["max_score"])
		}
		items := hits["hits"].([]any)
		if len(items) != 2 {
			t.Fatalf("hits = %d, want 2", len(items))
		}
		want := []float64{15.0, 20.0} // price asc: 5, 10.5 | 15, 20 | 30
		for i, item := range items {
			hit := item.(map[string]any)
			if hit["_score"] != nil {
				t.Errorf("hit %d carries a score under sort: %v", i, hit["_score"])
			}
			src := hit["_source"].(map[string]any)
			if got := src["price"].(float64); got != want[i] {
				t.Errorf("hit %d price = %v, want %v", i, got, want[i])
			}
		}
	})

	t.Run("size zero counts matches", func(t *testing.T) {
		res := query(t, `{"query":{"term":{"active":true}},"size":0}`)
		hits := res["hits"].(map[string]any)
		if hits["total"].(map[string]any)["value"] != float64(3) {
			t.Errorf("total = %v, want 3", hits["total"])
		}
		if got := len(hits["hits"].([]any)); got != 0 {
			t.Errorf("hits = %d, want 0", got)
		}
	})

	t.Run("terms and exists filters", func(t *testing.T) {
		res := query(t, `{"query":{"terms":{"qty":[2,4]}}}`)
		if res["hits"].(map[string]any)["total"].(map[string]any)["value"] != float64(2) {
			t.Errorf("terms total = %v, want 2", res["hits"])
		}
		res = query(t, `{"query":{"exists":{"field":"price"}},"size":0}`)
		if res["hits"].(map[string]any)["total"].(map[string]any)["value"] != float64(5) {
			t.Errorf("exists total = %v, want 5", res["hits"])
		}
	})

	t.Run("bool query combining clauses", func(t *testing.T) {
		res := query(t, `{"query":{"bool":{"must":[{"term":{"active":true}}],"must_not":[{"term":{"name":"charlie"}}]}}}`)
		total := res["hits"].(map[string]any)["total"].(map[string]any)["value"]
		if total != float64(2) {
			t.Errorf("total = %v, want 2 (delta and alpha)", total)
		}
	})

	t.Run("knn returns nearest-first on the cosine fixture", func(t *testing.T) {
		res := roundtrip(t, srv, http.MethodPost, milvustest.VectorCollection+"/_search",
			`{"knn":{"field":"emb","query_vector":[1,0,0,0],"k":5,"num_candidates":100}}`, http.StatusOK)
		hits := res["hits"].(map[string]any)
		if hits["total"].(map[string]any)["value"] != float64(5) {
			t.Errorf("total = %v, want 5 (the top-k row count)", hits["total"])
		}
		items := hits["hits"].([]any)
		if len(items) != 5 {
			t.Fatalf("hits = %d, want 5", len(items))
		}
		for i, want := range []string{"1", "2", "3", "4", "5"} {
			hit := items[i].(map[string]any)
			if hit["_id"] != want {
				t.Errorf("hit %d = %v, want %s (nearest-first)", i, hit["_id"], want)
			}
			if hit["_score"] != 1.0 {
				t.Errorf("hit %d _score = %v, want the constant 1.0 until Phase 5", i, hit["_score"])
			}
		}
	})

	t.Run("knn filter narrows the candidates", func(t *testing.T) {
		res := roundtrip(t, srv, http.MethodPost, milvustest.VectorCollection+"/_search",
			`{"knn":{"field":"emb","query_vector":[1,0,0,0],"k":5,"filter":{"term":{"tag":"odd"}}}}`, http.StatusOK)
		items := res["hits"].(map[string]any)["hits"].([]any)
		var ids []string
		for _, item := range items {
			ids = append(ids, item.(map[string]any)["_id"].(string))
		}
		if strings.Join(ids, ",") != "1,3,5" {
			t.Errorf("hits = %v, want [1 3 5] (odd tags only, nearest-first)", ids)
		}
	})

	t.Run("knn rides the L2 fixture with paging", func(t *testing.T) {
		// k windows the ANN search and from offsets into it; an explicit k
		// wins over size (size only seeds k's default — the v1 approximation
		// of ES's from/size-over-top-k windowing, docs/design.md)
		res := roundtrip(t, srv, http.MethodPost, milvustest.Collection+"/_search",
			`{"knn":{"field":"emb","query_vector":[0.1,0.2,0.3,0.4],"k":2},"from":1}`, http.StatusOK)
		items := res["hits"].(map[string]any)["hits"].([]any)
		if len(items) != 2 {
			t.Fatalf("hits = %d, want 2", len(items))
		}
		// L2 to row 1's vector grows strictly with id: window [1,3) is ids 2,3
		for i, want := range []string{"2", "3"} {
			if items[i].(map[string]any)["_id"] != want {
				t.Errorf("hit %d = %v, want %s", i, items[i].(map[string]any)["_id"], want)
			}
		}
	})

	t.Run("knn errors keep the ES classification", func(t *testing.T) {
		knnErr := func(body string, wantType string) {
			t.Helper()
			res := roundtrip(t, srv, http.MethodPost, milvustest.Collection+"/_search", body, http.StatusBadRequest)
			if got := res["error"].(map[string]any)["type"]; got != wantType {
				t.Errorf("error.type = %v, want %s (body: %s)", got, wantType, body)
			}
		}
		knnErr(`{"knn":[{"field":"emb","query_vector":[1,0,0,0]}]}`, "unsupported_exception")                        // array form
		knnErr(`{"query":{"match_all":{}},"knn":{"field":"emb","query_vector":[1,0,0,0]}}`, "unsupported_exception") // + query
		knnErr(`{"knn":{"field":"emb","query_vector":[1,0,0,0],"similarity":0.9}}`, "unsupported_exception")         // range search
		knnErr(`{"knn":{"field":"emb","query_vector":0.5}}`, "parsing_exception")                                    // vector shape
		knnErr(`{"knn":{"query_vector":[1,0,0,0]}}`, "parsing_exception")                                            // missing field
		knnErr(`{"knn":{"field":"emb","query_vector":[1,"x"]}}`, "illegal_argument_exception")                       // element value
	})

	t.Run("missing index renders index_not_found", func(t *testing.T) {
		res := roundtrip(t, srv, http.MethodPost, "no_such_index/_search", `{}`, http.StatusNotFound)
		errBody := res["error"].(map[string]any)
		if errBody["type"] != "index_not_found_exception" {
			t.Errorf("error.type = %v", errBody["type"])
		}
	})

	t.Run("malformed requests render ES errors", func(t *testing.T) {
		res := roundtrip(t, srv, http.MethodPost, milvustest.Collection+"/_search", `{"from":-1}`, http.StatusBadRequest)
		if res["error"].(map[string]any)["type"] != "illegal_argument_exception" {
			t.Errorf("error.type = %v", res["error"].(map[string]any)["type"])
		}
		res = roundtrip(t, srv, http.MethodPost, milvustest.Collection+"/_search", `{"aggs":{"a":{"terms":{"field":"x"}}}}`, http.StatusBadRequest)
		if res["error"].(map[string]any)["type"] != "unsupported_exception" {
			t.Errorf("error.type = %v", res["error"].(map[string]any)["type"])
		}
	})

	t.Run("match on analyzed text goes through the gateway", func(t *testing.T) {
		res := roundtrip(t, srv, http.MethodPost, milvustest.TextCollection+"/_search",
			`{"query":{"match":{"body":"quick brown"}}}`, http.StatusOK)
		hits := res["hits"].(map[string]any)
		// TEXT_MATCH ORs tokens: quick → docs 1,4,5; brown → docs 1,2
		if hits["total"].(map[string]any)["value"] != float64(4) {
			t.Errorf("total = %v, want 4", hits["total"])
		}
		items := hits["hits"].([]any)
		first := items[0].(map[string]any)
		if first["_score"] != 1.0 {
			t.Errorf("_score = %v, want the constant 1.0 until Phase 5", first["_score"])
		}
		body := first["_source"].(map[string]any)["body"].(string)
		if !strings.Contains(body, "quick") && !strings.Contains(body, "brown") {
			t.Errorf("hit body = %q, should contain a matched token", body)
		}
	})

	t.Run("GET search with source parameter", func(t *testing.T) {
		path := milvustest.Collection + "/_search?source=" + `%7B%22size%22%3A0%7D`
		res := roundtrip(t, srv, http.MethodGet, path, "", http.StatusOK)
		if res["hits"].(map[string]any)["total"].(map[string]any)["value"] != float64(5) {
			t.Errorf("total = %v, want 5", res["hits"])
		}
	})

	t.Run("cluster handshake survives", func(t *testing.T) {
		res := roundtrip(t, srv, http.MethodGet, "", "", http.StatusOK)
		if res["tagline"] == nil {
			t.Errorf("cluster info = %v", res)
		}
	})
}

// roundtrip fires one request and decodes the envelope, asserting the status.
func roundtrip(t *testing.T, srv *httptest.Server, method, path, body string, wantStatus int) map[string]any {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, srv.URL+"/"+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want %d (body: %s)", resp.StatusCode, wantStatus, raw)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return out
}
