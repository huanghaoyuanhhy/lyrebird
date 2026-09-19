package esserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/huanghaoyuanhhy/lyrebird/internal/milvustest"
	"github.com/huanghaoyuanhhy/lyrebird/internal/store"
)

// esAddrEnv carries the address of the reference Elasticsearch the parity
// suite replays the same DSL against; docker-compose.e2e.yml serves one on
// http://127.0.0.1:19200. The suite skips when it is unset.
const esAddrEnv = "LYREBIRD_TEST_ES_ADDR"

// TestEsserverParity replays one search body against a real Elasticsearch
// and against the gateway and compares what a client observes: the status
// code, hits.total.value, and the hit ids — as a sequence where both sides
// order deterministically (sort, knn), as a set where ranking lives on
// different scorers (the gateway answers the constant 1.0 until Phase 5,
// Elasticsearch ranks by BM25). The matrix stays inside the shared
// capability set; the gateway's deliberate divergences (no aggs, constant
// scores) are asserted by the full-stack suite, not here.
func TestEsserverParity(t *testing.T) {
	cfg, ok := milvustest.FromEnv()
	if !ok {
		t.Skipf("set %s and %s to run the parity suite", milvustest.URIEnv, milvustest.TokenEnv)
	}
	esAddr := os.Getenv(esAddrEnv)
	if esAddr == "" {
		t.Skipf("set %s to run the parity suite", esAddrEnv)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	if err := milvustest.Seed(ctx, cfg); err != nil {
		t.Fatalf("seed fixture: %v", err)
	}
	seedElasticsearch(t, esAddr)

	exec, err := store.NewMilvusExecutor(ctx, store.MilvusConfig{URI: cfg.URI, Token: cfg.Token})
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	defer exec.Close(ctx)

	srv := httptest.NewServer(New(exec, zap.NewNop()))
	t.Cleanup(srv.Close)

	cases := []struct {
		name    string
		index   string
		body    string
		ordered bool // compare the _id sequence, not the sorted set
		source  bool // deep-compare the first hit's _source
	}{
		{"term", milvustest.Collection, `{"query":{"term":{"name":"bravo"}}}`, false, false},
		{"terms", milvustest.Collection, `{"query":{"terms":{"qty":[2,4]}}}`, false, false},
		{"range", milvustest.Collection, `{"query":{"range":{"price":{"gte":5}}}}`, false, false},
		{"exists hides the null rows", milvustest.Collection, `{"query":{"exists":{"field":"note"}}}`, false, false},
		{"bool must + must_not", milvustest.Collection,
			`{"query":{"bool":{"must":[{"term":{"active":true}}],"must_not":[{"term":{"name":"charlie"}}]}}}`, false, false},
		{"size 0 counts matches", milvustest.Collection, `{"query":{"term":{"active":true}},"size":0}`, false, false},
		{"sort window with from/size", milvustest.Collection,
			`{"query":{"range":{"price":{"gte":5}}},"sort":[{"price":"asc"}],"from":2,"size":2}`, true, false},
		{"_source projection", milvustest.Collection, `{"query":{"term":{"name":"bravo"}},"_source":["price"]}`, false, true},
		{"knn cosine order", milvustest.VectorCollection,
			`{"knn":{"field":"emb","query_vector":[1,0,0,0],"k":5,"num_candidates":100}}`, true, false},
		{"knn cosine with filter", milvustest.VectorCollection,
			`{"knn":{"field":"emb","query_vector":[1,0,0,0],"k":5,"filter":{"term":{"tag":"odd"}}}}`, true, false},
		{"knn l2 order", milvustest.Collection,
			`{"knn":{"field":"emb","query_vector":[0.1,0.2,0.3,0.4],"k":5,"num_candidates":100}}`, true, false},
		{"match token set", milvustest.TextCollection, `{"query":{"match":{"body":"quick brown"}}}`, false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			refStatus, ref := paritySearch(t, esAddr, tc.index, tc.body)
			gwStatus, gw := paritySearch(t, srv.URL, tc.index, tc.body)
			if gwStatus != refStatus {
				t.Fatalf("status = %d, want %d (reference: %v / gateway: %v)", gwStatus, refStatus, ref, gw)
			}
			if refStatus != http.StatusOK {
				return
			}
			refTotal, refIDs := parityHits(t, ref)
			gwTotal, gwIDs := parityHits(t, gw)
			if gwTotal != refTotal {
				t.Errorf("hits.total.value = %d, want %d", gwTotal, refTotal)
			}
			if tc.ordered {
				if strings.Join(gwIDs, ",") != strings.Join(refIDs, ",") {
					t.Errorf("hit ids = %v, want %v (reference order)", gwIDs, refIDs)
				}
			} else {
				sort.Strings(gwIDs)
				sort.Strings(refIDs)
				if strings.Join(gwIDs, ",") != strings.Join(refIDs, ",") {
					t.Errorf("hit id set = %v, want %v", gwIDs, refIDs)
				}
			}
			if tc.source {
				if src := firstSource(t, gw); !reflect.DeepEqual(src, firstSource(t, ref)) {
					t.Errorf("_source = %v, want %v", src, firstSource(t, ref))
				}
			}
		})
	}
}

// paritySearch POSTs one search body to either side of the comparison and
// decodes the envelope, returning the status so a mismatch can dump both
// bodies.
func paritySearch(t *testing.T, base, index, body string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Post(base+"/"+index+"/_search", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s/%s/_search: %v", base, index, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response from %s: %v", base, err)
	}
	return resp.StatusCode, out
}

// parityHits extracts hits.total.value and the _id sequence.
func parityHits(t *testing.T, res map[string]any) (int, []string) {
	t.Helper()
	hits, ok := res["hits"].(map[string]any)
	if !ok {
		t.Fatalf("no hits object: %v", res)
	}
	total, ok := hits["total"].(map[string]any)["value"].(float64)
	if !ok {
		t.Fatalf("no hits.total.value: %v", res)
	}
	var ids []string
	for _, item := range hits["hits"].([]any) {
		ids = append(ids, item.(map[string]any)["_id"].(string))
	}
	return int(total), ids
}

// firstSource returns the first hit's _source for the projection case.
func firstSource(t *testing.T, res map[string]any) map[string]any {
	t.Helper()
	items := res["hits"].(map[string]any)["hits"].([]any)
	if len(items) == 0 {
		t.Fatalf("no hits to compare _source on: %v", res)
	}
	return items[0].(map[string]any)["_source"].(map[string]any)
}

// seedElasticsearch mirrors the milvus fixtures into reference indices:
// same documents, same field names, mappings chosen so term/range/knn see
// the same shapes (keyword for equality fields, dense_vector carrying the
// fixture's metric).
func seedElasticsearch(t *testing.T, addr string) {
	t.Helper()
	for index, mapping := range map[string]string{
		milvustest.Collection: `{"mappings":{"properties":{
			"name":{"type":"keyword"},"price":{"type":"double"},"qty":{"type":"integer"},
			"active":{"type":"boolean"},"created_ms":{"type":"long"},"note":{"type":"keyword"},
			"emb":{"type":"dense_vector","dims":4,"index":true,"similarity":"l2_norm"}}}}`,
		milvustest.TextCollection: `{"mappings":{"properties":{"body":{"type":"text"}}}}`,
		milvustest.VectorCollection: `{"mappings":{"properties":{
			"tag":{"type":"keyword"},
			"emb":{"type":"dense_vector","dims":4,"index":true,"similarity":"cosine"}}}}`,
	} {
		esAdmin(t, addr, http.MethodDelete, index, "", []int{http.StatusOK, http.StatusNotFound})
		esAdmin(t, addr, http.MethodPut, index, mapping, []int{http.StatusOK})
	}

	// documents mirror milvustest.seedDocs / seedText / seedVec exactly;
	// note stays absent (not null) on ids 2 and 5, as ES stores missing
	// fields.
	bulk := strings.Join([]string{
		`{"index":{"_index":"` + milvustest.Collection + `","_id":"1"}}`,
		`{"id":1,"name":"alpha","price":10.5,"qty":1,"active":true,"created_ms":1700000000000,"note":"alpha note","emb":[0.1,0.2,0.3,0.4]}`,
		`{"index":{"_index":"` + milvustest.Collection + `","_id":"2"}}`,
		`{"id":2,"name":"bravo","price":20.0,"qty":2,"active":false,"created_ms":1700000100000,"emb":[0.5,0.6,0.7,0.8]}`,
		`{"index":{"_index":"` + milvustest.Collection + `","_id":"3"}}`,
		`{"id":3,"name":"charlie","price":5.0,"qty":3,"active":true,"created_ms":1700000200000,"note":"charlie note","emb":[0.9,1.0,1.1,1.2]}`,
		`{"index":{"_index":"` + milvustest.Collection + `","_id":"4"}}`,
		`{"id":4,"name":"delta","price":30.0,"qty":4,"active":true,"created_ms":1700000300000,"note":"delta note","emb":[1.3,1.4,1.5,1.6]}`,
		`{"index":{"_index":"` + milvustest.Collection + `","_id":"5"}}`,
		`{"id":5,"name":"echo","price":15.0,"qty":5,"active":false,"created_ms":1700000400000,"emb":[1.7,1.8,1.9,2.0]}`,
		`{"index":{"_index":"` + milvustest.TextCollection + `","_id":"1"}}`,
		`{"id":1,"body":"the quick brown fox jumps over the lazy dog"}`,
		`{"index":{"_index":"` + milvustest.TextCollection + `","_id":"2"}}`,
		`{"id":2,"body":"a fast brown dog barks at night"}`,
		`{"index":{"_index":"` + milvustest.TextCollection + `","_id":"3"}}`,
		`{"id":3,"body":"completely unrelated filler text lives here"}`,
		`{"index":{"_index":"` + milvustest.TextCollection + `","_id":"4"}}`,
		`{"id":4,"body":"quick quick slow turtle"}`,
		`{"index":{"_index":"` + milvustest.TextCollection + `","_id":"5"}}`,
		`{"id":5,"body":"foxes and quick rabbits play"}`,
		`{"index":{"_index":"` + milvustest.VectorCollection + `","_id":"1"}}`,
		`{"id":1,"tag":"odd","emb":[1.0,0.0,0.0,0.0]}`,
		`{"index":{"_index":"` + milvustest.VectorCollection + `","_id":"2"}}`,
		`{"id":2,"tag":"even","emb":[0.8,0.2,0.0,0.0]}`,
		`{"index":{"_index":"` + milvustest.VectorCollection + `","_id":"3"}}`,
		`{"id":3,"tag":"odd","emb":[0.6,0.4,0.0,0.0]}`,
		`{"index":{"_index":"` + milvustest.VectorCollection + `","_id":"4"}}`,
		`{"id":4,"tag":"even","emb":[0.4,0.6,0.0,0.0]}`,
		`{"index":{"_index":"` + milvustest.VectorCollection + `","_id":"5"}}`,
		`{"id":5,"tag":"odd","emb":[0.2,0.8,0.0,0.0]}`,
	}, "\n") + "\n"
	res := esAdmin(t, addr, http.MethodPost, "/_bulk?refresh=wait_for", bulk, []int{http.StatusOK})
	if res["errors"] == true {
		t.Fatalf("bulk indexing failed: %v", res)
	}
}

// esAdmin fires one management request (index create/delete, bulk) and
// asserts the status lands in the allowed set, returning the envelope.
func esAdmin(t *testing.T, addr, method, path, body string, wantStatus []int) map[string]any {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, addr+"/"+strings.TrimPrefix(path, "/"), reader)
	if err != nil {
		t.Fatal(err)
	}
	contentType := "application/json"
	if strings.Contains(path, "_bulk") {
		contentType = "application/x-ndjson"
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: read body: %v", method, path, err)
	}
	allowed := false
	for _, s := range wantStatus {
		if resp.StatusCode == s {
			allowed = true
		}
	}
	if !allowed {
		t.Fatalf("%s %s = %d, want one of %v (body: %s)", method, path, resp.StatusCode, wantStatus, raw)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s %s: decode response: %v", method, path, err)
	}
	return out
}
