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

// TestEsserverWriteE2E drives the write surface against real Milvus:
// index/create/update on the write fixtures, strict-schema rejections
// rendered in ES vocabulary, and read-back through the search path.
func TestEsserverWriteE2E(t *testing.T) {
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

	// do sends one request and decodes the envelope.
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
		raw, _ := io.ReadAll(resp.Body)
		var top map[string]any
		_ = json.Unmarshal(raw, &top)
		return resp.StatusCode, top
	}

	errType := func(t *testing.T, body map[string]any) string {
		t.Helper()
		e, _ := body["error"].(map[string]any)
		ty, _ := e["type"].(string)
		return ty
	}

	// searchOne reads a row back by id, waiting out Milvus's growing-segment
	// visibility window (sub-second in practice, polled to be sure).
	searchOne := func(t *testing.T, id string) map[string]any {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			status, body := do(t, "POST", "/"+milvustest.WriteCollection+"/_search",
				`{"query":{"term":{"id":`+id+`}}}`)
			if status != http.StatusOK {
				t.Fatalf("search status=%d body=%v", status, body)
			}
			items := body["hits"].(map[string]any)["hits"].([]any)
			if len(items) == 1 {
				return items[0].(map[string]any)["_source"].(map[string]any)
			}
			if time.Now().After(deadline) {
				t.Fatalf("row %s never became visible; hits = %d", id, len(items))
			}
			time.Sleep(200 * time.Millisecond)
		}
	}

	t.Run("index, create, conflict, update", func(t *testing.T) {
		status, body := do(t, "PUT", "/"+milvustest.WriteCollection+"/_doc/101",
			`{"id": 101, "name": "alpha", "price": 1.5, "active": true, "emb": [0.5, 0.5]}`)
		if status != http.StatusCreated || body["result"] != "created" {
			t.Fatalf("status=%d body=%v", status, body)
		}

		// read back through the search path
		src := searchOne(t, "101")
		if src["name"] != "alpha" || src["price"] != 1.5 {
			t.Fatalf("source = %v", src)
		}

		// same id: an update (upsert), 200
		status, body = do(t, "PUT", "/"+milvustest.WriteCollection+"/_doc/101",
			`{"id": 101, "name": "alpha2", "price": 2.5, "active": true, "emb": [1, 0]}`)
		if status != http.StatusOK || body["result"] != "updated" {
			t.Fatalf("status=%d body=%v", status, body)
		}
		if src = searchOne(t, "101"); src["name"] != "alpha2" {
			t.Fatalf("source after update = %v", src)
		}

		// _create on the taken id: 409
		status, body = do(t, "PUT", "/"+milvustest.WriteCollection+"/_create/101",
			`{"id": 101, "name": "x", "price": 1, "active": true, "emb": [1, 0]}`)
		if status != http.StatusConflict || errType(t, body) != "version_conflict_exception" {
			t.Fatalf("status=%d body=%v", status, body)
		}

		// _create on a free id: 201
		status, _ = do(t, "PUT", "/"+milvustest.WriteCollection+"/_create/102",
			`{"id": 102, "name": "beta", "price": 3, "active": false, "emb": [0, 1]}`)
		if status != http.StatusCreated {
			t.Fatalf("create status=%d", status)
		}
	})

	t.Run("partial update merges and detects noop", func(t *testing.T) {
		status, body := do(t, "POST", "/"+milvustest.WriteCollection+"/_update/101", `{"doc": {"price": 9.75}}`)
		if status != http.StatusOK || body["result"] != "updated" {
			t.Fatalf("status=%d body=%v", status, body)
		}
		if src := searchOne(t, "101"); src["price"] != 9.75 || src["name"] != "alpha2" {
			t.Fatalf("merged source = %v", src)
		}

		status, body = do(t, "POST", "/"+milvustest.WriteCollection+"/_update/101", `{"doc": {"price": 9.75}}`)
		if status != http.StatusOK || body["result"] != "noop" {
			t.Fatalf("noop status=%d body=%v", status, body)
		}

		status, body = do(t, "POST", "/"+milvustest.WriteCollection+"/_update/999", `{"doc": {"price": 1}}`)
		if status != http.StatusNotFound || errType(t, body) != "document_missing_exception" {
			t.Fatalf("status=%d body=%v", status, body)
		}
	})

	t.Run("strict mapping rejects", func(t *testing.T) {
		cases := []struct {
			name string
			body string
			want string
		}{
			{"unknown field", `{"id": 103, "name": "x", "price": 1, "active": true, "emb": [1, 1], "junk": 1}`, "strict_dynamic_mapping_exception"},
			{"type mismatch", `{"id": 103, "name": "x", "price": "cheap", "active": true, "emb": [1, 1]}`, "document_parsing_exception"},
			{"dim mismatch", `{"id": 103, "name": "x", "price": 1, "active": true, "emb": [1, 2, 3]}`, "illegal_argument_exception"},
			{"missing required", `{"id": 103, "price": 1}`, "illegal_argument_exception"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				status, body := do(t, "PUT", "/"+milvustest.WriteCollection+"/_doc/103", tc.body)
				if status != http.StatusBadRequest || errType(t, body) != tc.want {
					t.Fatalf("status=%d type=%s body=%v", status, errType(t, body), body)
				}
			})
		}
	})

	t.Run("auto id and string pk", func(t *testing.T) {
		status, body := do(t, "POST", "/"+milvustest.AutoIDCollection+"/_doc", `{"v": "auto"}`)
		if status != http.StatusCreated {
			t.Fatalf("status=%d body=%v", status, body)
		}
		if id, _ := body["_id"].(string); id == "" || id == "0" {
			t.Fatalf("_id = %v, want the assigned key", body["_id"])
		}

		status, _ = do(t, "PUT", "/"+milvustest.StringPKCollection+"/_doc/hello", `{"id": "hello", "v": "world"}`)
		if status != http.StatusCreated {
			t.Fatalf("string pk status=%d", status)
		}
		status, body = do(t, "PUT", "/"+milvustest.StringPKCollection+"/_create/hello", `{"id": "hello", "v": "again"}`)
		if status != http.StatusConflict {
			t.Fatalf("string pk conflict status=%d body=%v", status, body)
		}
	})
}
