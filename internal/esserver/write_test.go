package esserver

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/huanghaoyuanhhy/lyrebird/internal/catalog"
	"github.com/huanghaoyuanhhy/lyrebird/internal/store"
	"github.com/huanghaoyuanhhy/lyrebird/internal/translate"
)

// writeMeta is the write-test fixture: a numeric-pk collection (the scalar
// shape) with a nullable field and a two-dim vector.
var writeMeta = catalog.CollectionMeta{
	Name: "items",
	Fields: []catalog.FieldMeta{
		{Name: "id", Type: translate.TypeNumber, PrimaryKey: true, Native: "int64"},
		{Name: "name", Type: translate.TypeKeyword, Native: "string"},
		{Name: "price", Type: translate.TypeNumber, Native: "float64"},
		{Name: "active", Type: translate.TypeBool, Native: "bool"},
		{Name: "note", Type: translate.TypeKeyword, Native: "string", Nullable: true},
		{Name: "emb", Type: translate.TypeVector, Native: "float_vector", Dim: 2},
	},
}

// genMeta is the auto-id twin: Milvus assigns the key.
var genMeta = catalog.CollectionMeta{
	Name: "gen",
	Fields: []catalog.FieldMeta{
		{Name: "id", Type: translate.TypeNumber, PrimaryKey: true, AutoID: true, Native: "int64"},
		{Name: "v", Type: translate.TypeKeyword, Native: "string"},
	},
}

// strMeta is the string-pk twin: the URL id is the key.
var strMeta = catalog.CollectionMeta{
	Name: "strs",
	Fields: []catalog.FieldMeta{
		{Name: "id", Type: translate.TypeKeyword, PrimaryKey: true, Native: "string"},
		{Name: "v", Type: translate.TypeKeyword, Native: "string"},
	},
}

func writeServer(t *testing.T, exec *fakeExecutor) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(New(exec, nil))
	t.Cleanup(srv.Close)
	return srv
}

func doJSON(t *testing.T, srv *httptest.Server, method, path, body string) (int, map[string]any) {
	t.Helper()
	resp, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	w, err := http.DefaultClient.Do(resp)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Body.Close()
	raw, _ := io.ReadAll(w.Body)
	var top map[string]any
	_ = json.Unmarshal(raw, &top)
	return w.StatusCode, top
}

func TestIndexDocumentCreatesAndUpdates(t *testing.T) {
	exec := &fakeExecutor{metas: []catalog.CollectionMeta{writeMeta}}
	srv := writeServer(t, exec)

	// absent id: an insert, 201 created (non-nullable fields all present —
	// Milvus requires the whole row, and the strict path says so up front)
	status, body := doJSON(t, srv, "PUT", "/items/_doc/7",
		`{"id": 7, "name": "hi", "price": 1.5, "active": true, "emb": [0.1, 0.2]}`)
	if status != http.StatusCreated || body["result"] != "created" {
		t.Fatalf("status=%d body=%v", status, body)
	}
	call := exec.writeCalls[0]
	if call.Upsert {
		t.Fatal("expected an insert")
	}
	if call.Rows[0]["id"] != int64(7) { // the URL id is authoritative
		t.Fatalf("id = %#v", call.Rows[0]["id"])
	}
	if vec, ok := call.Rows[0]["emb"].([]float32); !ok || len(vec) != 2 {
		t.Fatalf("emb = %#v, want the decoded fp32 pair", call.Rows[0]["emb"])
	}

	// present id: an upsert, 200 updated
	exec2 := &fakeExecutor{
		metas: []catalog.CollectionMeta{writeMeta},
		result: &store.SearchResult{Total: 1, Hits: []store.Hit{
			{ID: "7", Source: map[string]any{"id": int64(7), "name": "old", "price": float64(9)}},
		}},
	}
	srv2 := writeServer(t, exec2)
	status, body = doJSON(t, srv2, "PUT", "/items/_doc/7", `{"id": 7, "name": "new", "price": 1, "active": true, "emb": [1, 0]}`)
	if status != http.StatusOK || body["result"] != "updated" {
		t.Fatalf("status=%d body=%v", status, body)
	}
	if !exec2.writeCalls[0].Upsert {
		t.Fatal("expected an upsert when the document exists")
	}

	// _create on an existing id: 409
	status, body = doJSON(t, srv2, "PUT", "/items/_create/7", `{"id": 7, "name": "n", "price": 1, "active": true, "emb": [1, 0]}`)
	if status != http.StatusConflict || body["error"].(map[string]any)["type"] != "version_conflict_exception" {
		t.Fatalf("status=%d body=%v", status, body)
	}
}

func TestIndexDocumentStringPkAndGeneratedID(t *testing.T) {
	exec := &fakeExecutor{metas: []catalog.CollectionMeta{strMeta}}
	srv := writeServer(t, exec)

	// no URL id: one is generated (20 url-safe chars) and used as the key
	status, body := doJSON(t, srv, "POST", "/strs/_doc", `{"v": "x"}`)
	if status != http.StatusCreated {
		t.Fatalf("status=%d body=%v", status, body)
	}
	id, _ := body["_id"].(string)
	if len(id) != 20 {
		t.Fatalf("generated id = %q, want 20 chars", id)
	}
	if exec.writeCalls[0].Rows[0]["id"] != id {
		t.Fatalf("pk column = %#v, want the generated id", exec.writeCalls[0].Rows[0]["id"])
	}

	// a pk value inside the document must agree with the URL id
	status, _ = doJSON(t, srv, "PUT", "/strs/_doc/other", `{"id": "mine", "v": "x"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("mismatch status=%d", status)
	}

	// agreeing values are fine
	status, _ = doJSON(t, srv, "PUT", "/strs/_doc/mine", `{"id": "mine", "v": "x"}`)
	if status != http.StatusCreated {
		t.Fatalf("agreeing status=%d", status)
	}
}

func TestIndexDocumentAutoIDPk(t *testing.T) {
	exec := &fakeExecutor{
		metas:     []catalog.CollectionMeta{genMeta},
		insertIDs: []string{"42"},
	}
	srv := writeServer(t, exec)

	// POST without id: insert, the store-assigned key becomes _id
	status, body := doJSON(t, srv, "POST", "/gen/_doc", `{"v": "x"}`)
	if status != http.StatusCreated || body["_id"] != "42" {
		t.Fatalf("status=%d body=%v", status, body)
	}
	if _, has := exec.writeCalls[0].Rows[0]["id"]; has {
		t.Fatal("the auto key must not be sent")
	}

	// a URL id on an auto-id index is rejected
	status, _ = doJSON(t, srv, "PUT", "/gen/_doc/1", `{"v": "x"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("status=%d", status)
	}

	// a body value for the auto key too (the store class, rendered natively)
	exec2 := &fakeExecutor{
		metas:     []catalog.CollectionMeta{genMeta},
		insertIDs: []string{"42"},
	}
	srv2 := writeServer(t, exec2)
	status, body = doJSON(t, srv2, "POST", "/gen/_doc", `{"id": 5, "v": "x"}`)
	if status != http.StatusBadRequest ||
		body["error"].(map[string]any)["type"] != "illegal_argument_exception" {
		t.Fatalf("status=%d body=%v", status, body)
	}

	// numeric pk without an id: no ES-shaped id can fill it
	str := &fakeExecutor{metas: []catalog.CollectionMeta{writeMeta}}
	srv3 := writeServer(t, str)
	status, _ = doJSON(t, srv3, "POST", "/items/_doc", `{"name": "x"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("numeric-pk status=%d", status)
	}
}

func TestIndexDocumentStrictSchemaErrors(t *testing.T) {
	exec := &fakeExecutor{metas: []catalog.CollectionMeta{writeMeta}}
	srv := writeServer(t, exec)

	cases := []struct {
		name     string
		body     string
		wantType string
	}{
		{"unknown field", `{"id": 1, "nope": "x"}`, "strict_dynamic_mapping_exception"},
		{"string into number", `{"id": 1, "price": "x"}`, "document_parsing_exception"},
		{"null into required", `{"id": 1, "name": null, "price": 1}`, "document_parsing_exception"},
		{"missing required", `{"id": 1, "price": 1}`, "illegal_argument_exception"},
		{"dim mismatch", `{"id": 1, "name": "x", "price": 1, "emb": [1, 2, 3]}`, "illegal_argument_exception"},
		{"non-array vector", `{"id": 1, "name": "x", "price": 1, "emb": 5}`, "document_parsing_exception"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := doJSON(t, srv, "PUT", "/items/_doc/1", tc.body)
			if status != http.StatusBadRequest {
				t.Fatalf("status=%d", status)
			}
			errBody := body["error"].(map[string]any)
			if errBody["type"] != tc.wantType {
				t.Fatalf("type=%v reason=%v", errBody["type"], errBody["reason"])
			}
		})
	}

	// missing index keeps its 404 shape
	status, body := doJSON(t, srv, "PUT", "/absent/_doc/1", `{}`)
	if status != http.StatusNotFound ||
		body["error"].(map[string]any)["type"] != "index_not_found_exception" {
		t.Fatalf("status=%d body=%v", status, body)
	}

	// optimistic-concurrency parameters refuse by name
	status, body = doJSON(t, srv, "PUT", "/items/_doc/1?if_seq_no=1", `{"id": 1}`)
	if status != http.StatusBadRequest ||
		body["error"].(map[string]any)["type"] != "unsupported_exception" {
		t.Fatalf("status=%d body=%v", status, body)
	}
}

func TestUpdateDocumentMergeNoopAndMissing(t *testing.T) {
	// the stored row is whole, exactly what a read-modify-write fetches
	exec := &fakeExecutor{
		metas: []catalog.CollectionMeta{writeMeta},
		result: &store.SearchResult{Total: 1, Hits: []store.Hit{
			{ID: "7", Source: map[string]any{
				"id": int64(7), "name": "old", "price": float64(9), "active": true,
				"note": nil, "emb": []float32{0.5, 0.5},
			}},
		}},
	}
	srv := writeServer(t, exec)

	// a real change: the upserted row carries old values plus the merge
	status, body := doJSON(t, srv, "POST", "/items/_update/7", `{"doc": {"price": 10}}`)
	if status != http.StatusOK || body["result"] != "updated" {
		t.Fatalf("status=%d body=%v", status, body)
	}
	if len(exec.writeCalls) != 1 || !exec.writeCalls[0].Upsert {
		t.Fatalf("calls = %#v", exec.writeCalls)
	}
	row := exec.writeCalls[0].Rows[0]
	if row["price"] != json.Number("10") || row["name"] != "old" || row["id"] != int64(7) {
		t.Fatalf("merged row = %#v", row)
	}

	// a no-change doc: result noop, nothing written (the fake still serves
	// the stored row with price 9; the re-sent value must not disturb it)
	status, body = doJSON(t, srv, "POST", "/items/_update/7", `{"doc": {"price": 9}}`)
	if status != http.StatusOK || body["result"] != "noop" {
		t.Fatalf("status=%d body=%v", status, body)
	}
	if len(exec.writeCalls) != 1 {
		t.Fatalf("noop must not write, calls = %d", len(exec.writeCalls))
	}

	// missing document: 404
	exec2 := &fakeExecutor{metas: []catalog.CollectionMeta{writeMeta}}
	srv2 := writeServer(t, exec2)
	status, body = doJSON(t, srv2, "POST", "/items/_update/9", `{"doc": {"price": 10}}`)
	if status != http.StatusNotFound ||
		body["error"].(map[string]any)["type"] != "document_missing_exception" {
		t.Fatalf("status=%d body=%v", status, body)
	}

	// script form refuses by name
	status, body = doJSON(t, srv, "POST", "/items/_update/7", `{"script": {"source": "1"}}`)
	if status != http.StatusBadRequest ||
		body["error"].(map[string]any)["type"] != "illegal_argument_exception" {
		t.Fatalf("status=%d body=%v", status, body)
	}

	// the primary key is an address, not a field to change
	status, _ = doJSON(t, srv, "POST", "/items/_update/7", `{"doc": {"id": 8}}`)
	if status != http.StatusBadRequest {
		t.Fatalf("pk-rebind status=%d", status)
	}
}

func TestBulkRefused(t *testing.T) {
	exec := &fakeExecutor{metas: []catalog.CollectionMeta{writeMeta}}
	srv := writeServer(t, exec)
	for _, path := range []string{"/_bulk", "/items/_bulk"} {
		status, body := doJSON(t, srv, "POST", path, "{}\n")
		if status != http.StatusBadRequest ||
			body["error"].(map[string]any)["type"] != "unsupported_exception" {
			t.Fatalf("%s status=%d body=%v", path, status, body)
		}
	}
}
