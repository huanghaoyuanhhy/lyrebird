// Package esserver implements the Elasticsearch-compatible REST entry point:
// it accepts ES clients' Query DSL requests, translates them via translate/es,
// and executes them against Milvus via store.
package esserver

import (
	"encoding/json"
	"net/http"
)

// Server serves the endpoint subset that ES clients can talk to directly.
type Server struct {
	mux *http.ServeMux
}

// New constructs the ES-compatible server. Currently a skeleton: only the
// client handshake works, query endpoints all return 501.
func New() *Server {
	s := &Server{mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /{$}", s.clusterInfo)
	s.mux.HandleFunc("POST /{index}/_search", s.notImplemented)
	s.mux.HandleFunc("GET /{index}/_search", s.notImplemented)
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "no such endpoint: "+r.Method+" "+r.URL.Path)
	})
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-elastic-product", "Elasticsearch")
	w.Header().Set("Content-Type", "application/json")
	s.mux.ServeHTTP(w, r)
}

// clusterInfo is the first request most ES SDKs make on startup: GET /
// probing the cluster and version. The X-elastic-product response header is
// the product check for 8.x clients — without it they refuse to connect.
func (s *Server) clusterInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"name":         "lyrebird",
		"cluster_name": "milvus",
		"cluster_uuid": "lyrebird-dev",
		"version": map[string]any{
			"number":         "8.11.0",
			"build_flavor":   "default",
			"build_type":     "docker",
			"lucene_version": "9.8.0",
		},
		"tagline": "You Know, for Search (over Milvus)",
	})
}

func (s *Server) notImplemented(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotImplemented, "endpoint recognized but translation not implemented yet: "+r.URL.Path)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeError returns errors in the ES error envelope format, keeping ES SDK
// error parsing intact.
func writeError(w http.ResponseWriter, status int, reason string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"type":   "lyrebird_error",
			"reason": reason,
		},
		"status": status,
	})
}
