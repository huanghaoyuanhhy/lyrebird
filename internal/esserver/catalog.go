package esserver

// Catalog endpoints: the introspection surface ES GUI clients read. The
// concrete target is the DataGrip "ES REST Data Source" plugin, whose full
// probe set is five calls (sourced from its code): GET / (handshake, below
// in server.go), _cat/indices, _cat/aliases, _data_stream, batched
// /{index}/_mapping — with /{index}/_search as the data path, which the
// regular search handler already serves. _cluster/health rides along for
// the clients that gate on it.
//
// Every endpoint is a thin projection of the catalog Provider: cluster →
// one gateway, index → collection, mapping field → column. Names follow
// ES semantics: comma-separated index lists resolve each name (missing
// ones are skipped, like ES), and a single missing index is a native 404
// index_not_found_exception.

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/huanghaoyuanhhy/lyrebird/internal/catalog"
)

// installCatalogRoutes registers the metadata endpoints on the mux. More
// specific patterns (the _cat/… prefixes) win over the {index}/… wildcards
// by ServeMux specificity rules.
func (s *Server) installCatalogRoutes() {
	s.mux.HandleFunc("GET /_cat/indices", s.catIndices)
	s.mux.HandleFunc("GET /_cat/indices/{index}", s.catIndices)
	s.mux.HandleFunc("GET /_cat/aliases", s.catAliases)
	s.mux.HandleFunc("GET /_data_stream", s.dataStreams)
	s.mux.HandleFunc("GET /_cluster/health", s.clusterHealth)
	s.mux.HandleFunc("GET /_cluster/health/{index}", s.clusterHealth)
	s.mux.HandleFunc("GET /_mapping", s.mapping)
	s.mux.HandleFunc("GET /_mappings", s.mapping)
	s.mux.HandleFunc("GET /_all/_mapping", s.mapping)
	s.mux.HandleFunc("GET /_all/_mappings", s.mapping)
	s.mux.HandleFunc("GET /{index}/_mapping", s.mapping)
	s.mux.HandleFunc("GET /{index}/_mappings", s.mapping)
}

// indexNames resolves the request's index selection against the provider:
// a path value names indices explicitly (comma list, _all), no value means
// every index. Unknown names are skipped, the way ES answers wildcard-ish
// selections; a single unknown name that was spelled out verbatim still
// returns empty here — the 404 handling lives in the _mapping handler,
// where ES's contract is strictest.
func (s *Server) indexNames(ctx context.Context, pattern string) ([]string, error) {
	names, err := s.cluster.ListCollections(ctx, "")
	if err != nil {
		return nil, err
	}
	if pattern == "" || pattern == "_all" || pattern == "*" {
		return names, nil
	}
	var out []string
	for _, want := range strings.Split(pattern, ",") {
		for _, name := range names {
			if name == want {
				out = append(out, name)
				break
			}
		}
	}
	return out, nil
}

// ─── _cat/indices ───────────────────────────────────────────────────────────

// catColumns is the full field set _cat/indices can report, in ES's default
// text order. The h= parameter selects a subset (the DataGrip plugin asks
// for index,health,status,docs.count,store.size).
var catColumns = []struct {
	name  string
	value func(s *Server, ctx context.Context, name string) string
}{
	{"index", func(s *Server, ctx context.Context, name string) string { return name }},
	{"health", func(s *Server, ctx context.Context, name string) string { return "green" }},
	{"status", func(s *Server, ctx context.Context, name string) string { return "open" }},
	{"pri", func(s *Server, ctx context.Context, name string) string { return "1" }},
	{"rep", func(s *Server, ctx context.Context, name string) string { return "0" }},
	{"docs.count", func(s *Server, ctx context.Context, name string) string {
		n, err := s.cluster.CollectionStats(ctx, "", name)
		if err != nil {
			return "-1" // ES's unknown-value marker
		}
		return strconv.FormatInt(n, 10)
	}},
	{"docs.deleted", func(s *Server, ctx context.Context, name string) string { return "0" }},
	{"store.size", func(s *Server, ctx context.Context, name string) string { return "0b" }},
	{"pri.store.size", func(s *Server, ctx context.Context, name string) string { return "0b" }},
	{"creation.date", func(s *Server, ctx context.Context, name string) string { return "0" }},
	{"uuid", func(s *Server, ctx context.Context, name string) string {
		return "lyrebird-" + name
	}},
}

func (s *Server) catIndices(w http.ResponseWriter, r *http.Request) {
	names, err := s.indexNames(r.Context(), r.PathValue("index"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list indices: "+err.Error())
		return
	}

	columns := catColumns
	if h := r.URL.Query().Get("h"); h != "" {
		columns = selectColumns(strings.Split(h, ","))
	}

	switch r.URL.Query().Get("format") {
	case "json":
		rows := make([]map[string]string, 0, len(names))
		for _, name := range names {
			row := make(map[string]string, len(columns))
			for _, c := range columns {
				row[c.name] = c.value(s, r.Context(), name)
			}
			rows = append(rows, row)
		}
		writeJSON(w, http.StatusOK, rows)
	default:
		// the human tabular form; ?v (a valueless flag) adds the header row
		var b strings.Builder
		if _, verbose := r.URL.Query()["v"]; verbose {
			headers := make([]string, len(columns))
			for i, c := range columns {
				headers[i] = c.name
			}
			b.WriteString(strings.Join(headers, "\t") + "\n")
		}
		for _, name := range names {
			cells := make([]string, len(columns))
			for i, c := range columns {
				cells[i] = c.value(s, r.Context(), name)
			}
			b.WriteString(strings.Join(cells, "\t") + "\n")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(b.String()))
	}
}

func selectColumns(want []string) (columns []struct {
	name  string
	value func(s *Server, ctx context.Context, name string) string
}) {
	for _, w := range want {
		for _, c := range catColumns {
			if c.name == w {
				columns = append(columns, c)
				break
			}
		}
	}
	return columns
}

// ─── _cat/aliases ───────────────────────────────────────────────────────────

func (s *Server) catAliases(w http.ResponseWriter, r *http.Request) {
	// lyrebird exposes no aliases yet; the empty answer is the ES-native one
	if r.URL.Query().Get("format") == "json" {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	w.WriteHeader(http.StatusOK)
}

// ─── _data_stream ───────────────────────────────────────────────────────────

func (s *Server) dataStreams(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"data_streams": []any{}})
}

// ─── _cluster/health ────────────────────────────────────────────────────────

func (s *Server) clusterHealth(w http.ResponseWriter, r *http.Request) {
	body := map[string]any{
		"cluster_name":                     "milvus",
		"status":                           "green",
		"timed_out":                        false,
		"number_of_nodes":                  1,
		"number_of_data_nodes":             1,
		"active_primary_shards":            0,
		"active_shards":                    0,
		"relocating_shards":                0,
		"initializing_shards":              0,
		"unassigned_shards":                0,
		"delayed_unassigned_shards":        false,
		"number_of_pending_tasks":          0,
		"task_max_waiting_in_queue_millis": 0,
		"active_shards_percent_as_number":  100.0,
	}
	if name := r.PathValue("index"); name != "" {
		metas, err := s.cluster.Collection(r.Context(), "", name)
		if err != nil {
			writeErrorEnvelope(w, http.StatusNotFound, "index_not_found_exception",
				"no such index ["+name+"]", name)
			return
		}
		body["indices"] = map[string]any{
			metas.Name: map[string]any{"status": "green", "number_of_shards": 1, "number_of_replicas": 0},
		}
	}
	writeJSON(w, http.StatusOK, body)
}

// ─── _mapping ───────────────────────────────────────────────────────────────

func (s *Server) mapping(w http.ResponseWriter, r *http.Request) {
	pattern := r.PathValue("index")
	if pattern == "" {
		pattern = "_all"
	}
	names, err := s.indexNames(r.Context(), pattern)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list indices: "+err.Error())
		return
	}
	if len(names) == 0 && pattern != "_all" && pattern != "*" && pattern != "" {
		// one explicit, unknown index: ES's strict 404
		writeErrorEnvelope(w, http.StatusNotFound, "index_not_found_exception",
			"no such index ["+pattern+"]", pattern)
		return
	}

	body := make(map[string]any, len(names))
	for _, name := range names {
		meta, err := s.cluster.Collection(r.Context(), "", name)
		if err != nil {
			continue // vanished mid-request: skip, like ES multi-target answers
		}
		body[name] = map[string]any{
			"mappings": map[string]any{
				"properties": mappingProperties(meta),
			},
		}
	}
	writeJSON(w, http.StatusOK, body)
}

// mappingProperties renders one collection's fields as an ES mapping body.
// Types come from the field's native Milvus type where it is finer than the
// translate vocabulary (long vs double), falling back to translate types.
func mappingProperties(meta catalog.CollectionMeta) map[string]any {
	props := make(map[string]any, len(meta.Fields))
	for _, f := range meta.Fields {
		props[f.Name] = fieldMapping(f)
	}
	return props
}

func fieldMapping(f catalog.FieldMeta) map[string]any {
	switch f.Native {
	// the store's canonical names (store.nativeTypeName)
	case "int8", "int16", "int32", "int64":
		return map[string]any{"type": "long"}
	case "float32":
		return map[string]any{"type": "float"}
	case "float64":
		return map[string]any{"type": "double"}
	case "bool":
		return map[string]any{"type": "boolean"}
	case "string":
		if f.Type == "text" {
			return map[string]any{"type": "text"} // BM25-analyzed
		}
		return map[string]any{"type": "keyword"}
	case "float_vector":
		return map[string]any{"type": "dense_vector", "dim": f.Dim}
	case "json", "array":
		return map[string]any{"type": "object"}
	}
	// translate-vocabulary fallback for native names the mapping does not
	// know (future Milvus types, or providers without Native)
	switch f.Type {
	case "number":
		return map[string]any{"type": "double"}
	case "bool":
		return map[string]any{"type": "boolean"}
	case "vector":
		return map[string]any{"type": "dense_vector", "dim": f.Dim}
	case "text":
		return map[string]any{"type": "text"}
	case "keyword", "date":
		return map[string]any{"type": "keyword"}
	default:
		return map[string]any{"type": "object"}
	}
}
