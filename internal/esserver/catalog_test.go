package esserver

// Catalog endpoint tests against the DataGrip ES REST plugin's probe set:
// _cat/indices (the exact h= column request it makes), _cat/aliases,
// _data_stream, batched /{index}/_mapping, and _cluster/health.

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/huanghaoyuanhhy/lyrebird/internal/catalog"
	"github.com/huanghaoyuanhhy/lyrebird/internal/translate"
)

func catalogFake() *fakeExecutor {
	return &fakeExecutor{
		metas: []catalog.CollectionMeta{
			{Name: "products", Fields: []catalog.FieldMeta{
				{Name: "id", Type: translate.TypeNumber, PrimaryKey: true, Native: "int64"},
				{Name: "name", Type: translate.TypeKeyword, Native: "string", MaxLength: 512},
				{Name: "price", Type: translate.TypeNumber, Native: "float32", Nullable: true},
				{Name: "body", Type: translate.TypeText, Native: "string"},
				{Name: "emb", Type: translate.TypeVector, Native: "float_vector", Dim: 8},
				{Name: "meta", Type: translate.TypeUnknown, Native: "json"},
			}},
			{Name: "orders", Fields: []catalog.FieldMeta{
				{Name: "id", Type: translate.TypeNumber, PrimaryKey: true, Native: "int64"},
			}},
		},
		stats: map[string]int64{"products": 42, "orders": 7},
	}
}

func getBody(t *testing.T, srvURL, path string) (int, string) {
	t.Helper()
	resp, err := http.Get(srvURL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(raw)
}

func TestCatIndicesJSONForPlugin(t *testing.T) {
	// the exact request the DataGrip ES REST plugin makes, cached per refresh
	srv := serve(t, catalogFake())
	status, raw := getBody(t, srv.URL,
		"/_cat/indices?format=json&h=index,health,status,docs.count,store.size&expand_wildcards=all")
	if status != http.StatusOK {
		t.Fatalf("status = %d (%s)", status, raw)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(raw), &rows); err != nil {
		t.Fatalf("body: %s", raw)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if rows[0]["index"] != "products" || rows[1]["index"] != "orders" {
		t.Errorf("indices = %v", rows)
	}
	if rows[0]["health"] != "green" || rows[0]["status"] != "open" {
		t.Errorf("health/status = %v", rows[0])
	}
	if rows[0]["docs.count"] != "42" {
		t.Errorf("docs.count = %v, want 42", rows[0]["docs.count"])
	}
}

func TestCatIndicesSubsets(t *testing.T) {
	srv := serve(t, catalogFake())

	// single index selection
	status, raw := getBody(t, srv.URL, "/_cat/indices/orders?format=json")
	if status != http.StatusOK || !strings.Contains(raw, `"orders"`) || strings.Contains(raw, "products") {
		t.Fatalf("single index: status=%d body=%s", status, raw)
	}

	// comma list skips missing names like ES multi-targets
	_, raw = getBody(t, srv.URL, "/_cat/indices/products,ghost?format=json")
	if strings.Contains(raw, "ghost") {
		t.Errorf("ghost should be skipped: %s", raw)
	}

	// human tabular form with header
	_, raw = getBody(t, srv.URL, "/_cat/indices?v")
	if !strings.HasPrefix(raw, "index\t") {
		t.Errorf("text format header missing: %q", raw)
	}
	if !strings.Contains(raw, "products\tgreen\topen\t") {
		t.Errorf("text row malformed: %q", raw)
	}
}

func TestCatAliasesAndDataStreams(t *testing.T) {
	srv := serve(t, catalogFake())

	status, raw := getBody(t, srv.URL, "/_cat/aliases?format=json&h=alias,index")
	if status != http.StatusOK || strings.TrimSpace(raw) != "[]" {
		t.Errorf("aliases = (%d) %q, want empty list", status, raw)
	}

	status, raw = getBody(t, srv.URL, "/_data_stream")
	if status != http.StatusOK || !strings.Contains(raw, `"data_streams": []`) && !strings.Contains(raw, `"data_streams":[]`) {
		t.Errorf("data streams = (%d) %q", status, raw)
	}
}

func TestClusterHealth(t *testing.T) {
	srv := serve(t, catalogFake())

	status, raw := getBody(t, srv.URL, "/_cluster/health")
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != "green" || body["cluster_name"] != "milvus" {
		t.Errorf("health body = %v", body)
	}

	// named index: green with an indices section; missing → native 404
	status, _ = getBody(t, srv.URL, "/_cluster/health/products")
	if status != http.StatusOK {
		t.Errorf("health(products) status = %d", status)
	}
	status, raw = getBody(t, srv.URL, "/_cluster/health/ghost")
	if status != http.StatusNotFound || !strings.Contains(raw, "index_not_found_exception") {
		t.Errorf("health(ghost) = (%d) %s", status, raw)
	}
}

func TestMappingEndpoint(t *testing.T) {
	srv := serve(t, catalogFake())

	// the plugin's batched call: /{,...}/_mapping with real mappings
	status, raw := getBody(t, srv.URL, "/products,_orders/_mapping")
	if status != http.StatusOK {
		t.Fatalf("status = %d (%s)", status, raw)
	}
	var body map[string]map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("body: %s", raw)
	}
	if len(body) != 1 || body["products"] == nil {
		t.Fatalf("mapping indices = %v", body)
	}
	props := body["products"]["mappings"].(map[string]any)["properties"].(map[string]any)
	if props["id"].(map[string]any)["type"] != "long" {
		t.Errorf("id mapping = %v, want long", props["id"])
	}
	if props["name"].(map[string]any)["type"] != "keyword" {
		t.Errorf("name mapping = %v, want keyword", props["name"])
	}
	if props["body"].(map[string]any)["type"] != "text" {
		t.Errorf("body mapping = %v, want text (BM25-analyzed)", props["body"])
	}
	if props["price"].(map[string]any)["type"] != "float" {
		t.Errorf("price mapping = %v, want float (native Float)", props["price"])
	}
	if props["emb"].(map[string]any)["type"] != "dense_vector" ||
		props["emb"].(map[string]any)["dim"] != float64(8) {
		t.Errorf("emb mapping = %v, want dense_vector dim 8", props["emb"])
	}
	if props["meta"].(map[string]any)["type"] != "object" {
		t.Errorf("meta mapping = %v, want object", props["meta"])
	}

	// all indices via _mapping
	status, raw = getBody(t, srv.URL, "/_mapping")
	if status != http.StatusOK || !strings.Contains(raw, "orders") {
		t.Errorf("_mapping = (%d) %s", status, raw)
	}

	// one unknown index → native 404 envelope
	status, raw = getBody(t, srv.URL, "/ghost/_mapping")
	if status != http.StatusNotFound || !strings.Contains(raw, "index_not_found_exception") {
		t.Errorf("ghost mapping = (%d) %s", status, raw)
	}
}
