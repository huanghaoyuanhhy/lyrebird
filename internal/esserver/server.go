// Package esserver implements the Elasticsearch-compatible REST entry point:
// it accepts ES clients' Query DSL requests, translates them via translate/es,
// and executes them against Milvus via store.
package esserver

import (
	"encoding/json"
	"net/http"
	"runtime/debug"
	"time"

	"go.uber.org/zap"

	"github.com/huanghaoyuanhhy/lyrebird/internal/store"
)

// Server serves the endpoint subset that ES clients can talk to directly.
type Server struct {
	cluster store.Cluster
	logger  *zap.Logger
	mux     *http.ServeMux
}

// New constructs the ES-compatible server around a cluster (the real
// MilvusExecutor, or the LogExecutor for development without a cluster).
// A nil logger falls back to the global zap logger.
func New(cluster store.Cluster, logger *zap.Logger) *Server {
	s := &Server{cluster: cluster, logger: logger, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /{$}", s.clusterInfo)
	s.mux.HandleFunc("POST /{index}/_search", s.search)
	s.mux.HandleFunc("GET /{index}/_search", s.search)
	s.installCatalogRoutes()
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "no such endpoint: "+r.Method+" "+r.URL.Path)
	})
	return s
}

func (s *Server) zapLogger() *zap.Logger {
	if s.logger == nil {
		return zap.L()
	}
	return s.logger
}

// ServeHTTP wraps the mux with the small cross-cutting layer the ES surface
// needs: product headers on every response, panic recovery, and one access
// log entry per request. Kept hand-rolled — there is no middleware chain to
// grow into yet (docs/design.md, HTTP layer note).
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	logger := s.zapLogger()
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

	defer func() {
		if p := recover(); p != nil {
			logger.Error("panic serving request",
				zap.String("method", r.Method),
				zap.String("path", r.URL.Path),
				zap.Any("panic", p),
				zap.String("stack", string(debug.Stack())))
			// Best effort: if the handler already wrote, this is a no-op.
			writeError(rec, http.StatusInternalServerError, "internal error while handling the request")
		}
		logger.Info("http request",
			zap.String("method", r.Method),
			zap.String("path", r.URL.Path),
			zap.Int("status", rec.status),
			zap.Duration("took", time.Since(start)))
	}()

	rec.Header().Set("X-elastic-product", "Elasticsearch")
	rec.Header().Set("Content-Type", "application/json")
	s.mux.ServeHTTP(rec, r)
}

// statusRecorder captures the response status for the access log.
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wrote {
		r.status = code
		r.wrote = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.wrote = true
	return r.ResponseWriter.Write(b)
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

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeError returns errors in the ES error envelope format, keeping ES SDK
// error parsing intact.
func writeError(w http.ResponseWriter, status int, reason string) {
	writeErrorEnvelope(w, status, "lyrebird_error", reason, "")
}

// writeErrorEnvelope emits the full ES error shape: type, reason, a
// root_cause array (SDKs and Kibana read it) and the numeric status.
func writeErrorEnvelope(w http.ResponseWriter, status int, errType, reason, index string) {
	cause := map[string]any{"type": errType, "reason": reason}
	errBody := map[string]any{
		"type":       errType,
		"reason":     reason,
		"root_cause": []any{cause},
	}
	if index != "" {
		errBody["index"] = index
		cause["index"] = index
	}
	writeJSON(w, status, map[string]any{"error": errBody, "status": status})
}
