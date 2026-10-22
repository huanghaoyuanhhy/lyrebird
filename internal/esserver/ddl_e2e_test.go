package esserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/huanghaoyuanhhy/lyrebird/internal/milvustest"
	"github.com/huanghaoyuanhhy/lyrebird/internal/store"
)

// TestEsserverDDLE2E pins the index-level DDL boundary of the ES surface.
// Lyrebird's collections come from the control plane, so index creation,
// deletion and mapping/settings updates answer unsupported_exception by
// name (see installDDLRoutes). The cases matter as much for what they leave
// behind as for the error: a refused DELETE must not remove the index, a
// refused mapping update must not change the mapping, and a refused create
// must not leave a half-created index in the cluster.
//
// When LYREBIRD_TEST_ES_ADDR names a reference Elasticsearch, the same calls
// run there first (on a throwaway index), so the matrix is known to be real
// ES API and not a typo the gateway would refuse for the wrong reason.
//
// When Phase 4b's index DDL lands, these cases flip from refusal to a
// create/mapping/search/delete round-trip.
func TestEsserverDDLE2E(t *testing.T) {
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

	probeReferenceESDDL(t)

	srv := httptest.NewServer(New(exec, zap.NewNop()))
	t.Cleanup(srv.Close)

	do := func(t *testing.T, method, path, body string) (int, map[string]any) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, method, srv.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("%s %s: decode response: %v", method, path, err)
		}
		return resp.StatusCode, out
	}

	index := milvustest.Collection

	// capture the mapping before any DDL call, to compare after the refusals
	status, before := do(t, http.MethodGet, "/"+index+"/_mapping", "")
	if status != http.StatusOK {
		t.Fatalf("mapping status = %d, body = %v", status, before)
	}

	refusals := []struct{ name, method, path, body string }{
		{"index creation", http.MethodPut, "/lyrebird_e2e_ddl",
			`{"mappings":{"properties":{"id":{"type":"long"},"name":{"type":"keyword"}}}}`},
		{"mapping update", http.MethodPut, "/" + index + "/_mapping",
			`{"properties":{"extra":{"type":"keyword"}}}`},
		{"settings update", http.MethodPut, "/" + index + "/_settings",
			`{"index":{"number_of_replicas":1}}`},
		{"index deletion", http.MethodDelete, "/" + index, ""},
	}
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			status, body := do(t, tc.method, tc.path, tc.body)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %v", status, body)
			}
			e, _ := body["error"].(map[string]any)
			if e["type"] != "unsupported_exception" {
				t.Errorf("error.type = %v, want unsupported_exception", e["type"])
			}
			if reason, _ := e["reason"].(string); !strings.Contains(reason, "control plane") {
				t.Errorf("reason = %q, want the control-plane explanation", reason)
			}
		})
	}

	t.Run("refused creation leaves no index", func(t *testing.T) {
		status, body := do(t, http.MethodPost, "/lyrebird_e2e_ddl/_search", `{"query":{"match_all":{}},"size":0}`)
		if status != http.StatusNotFound {
			t.Fatalf("status = %d, body = %v", status, body)
		}
		if e, _ := body["error"].(map[string]any); e["type"] != "index_not_found_exception" {
			t.Errorf("error.type = %v, want index_not_found_exception", e["type"])
		}
		for _, name := range catIndexNames(t, srv.URL) {
			if name == "lyrebird_e2e_ddl" {
				t.Error("the refused PUT /lyrebird_e2e_ddl created the index anyway")
			}
		}
	})

	t.Run("refused mapping update leaves the mapping unchanged", func(t *testing.T) {
		status, after := do(t, http.MethodGet, "/"+index+"/_mapping", "")
		if status != http.StatusOK {
			t.Fatalf("mapping status = %d, body = %v", status, after)
		}
		if !reflect.DeepEqual(after, before) {
			t.Errorf("mapping changed: before %v, after %v", before, after)
		}
	})

	t.Run("refused deletion leaves the index and its documents", func(t *testing.T) {
		status, body := do(t, http.MethodPost, "/"+index+"/_search", `{"query":{"match_all":{}},"size":0}`)
		if status != http.StatusOK {
			t.Fatalf("search status = %d, body = %v", status, body)
		}
		hits := body["hits"].(map[string]any)
		if total := hits["total"].(map[string]any)["value"]; total != float64(5) {
			t.Errorf("hits.total.value = %v, want 5", total)
		}
		found := false
		for _, name := range catIndexNames(t, srv.URL) {
			found = found || name == index
		}
		if !found {
			t.Errorf("the refused DELETE removed index %q", index)
		}
	})
}

// catIndexNames lists the gateway's indices through _cat/indices, the
// catalog view a client would check.
func catIndexNames(t *testing.T, base string) []string {
	t.Helper()
	resp, err := http.Get(base + "/_cat/indices?format=json&h=index")
	if err != nil {
		t.Fatalf("cat indices: %v", err)
	}
	defer resp.Body.Close()
	var rows []map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		t.Fatalf("cat indices: decode response: %v", err)
	}
	names := make([]string, len(rows))
	for i, row := range rows {
		names[i] = row["index"]
	}
	return names
}

// probeReferenceESDDL runs the matrix's calls against the reference
// Elasticsearch on a throwaway index: create, mapping update, settings
// update, delete. A refused call proves nothing unless the reference accepts
// the same request.
func probeReferenceESDDL(t *testing.T) {
	t.Helper()
	addr := os.Getenv(esAddrEnv)
	if addr == "" {
		t.Logf("%s unset: skipping the reference-API probe", esAddrEnv)
		return
	}
	const probe = "lyrebird_e2e_ddl_probe"
	esAdmin(t, addr, http.MethodDelete, probe, "", []int{http.StatusOK, http.StatusNotFound})
	esAdmin(t, addr, http.MethodPut, probe,
		`{"mappings":{"properties":{"id":{"type":"long"},"name":{"type":"keyword"}}}}`, []int{http.StatusOK})
	esAdmin(t, addr, http.MethodPut, probe+"/_mapping",
		`{"properties":{"extra":{"type":"keyword"}}}`, []int{http.StatusOK})
	esAdmin(t, addr, http.MethodPut, probe+"/_settings",
		`{"index":{"number_of_replicas":1}}`, []int{http.StatusOK})
	esAdmin(t, addr, http.MethodDelete, probe, "", []int{http.StatusOK})
}
