package esserver

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/huanghaoyuanhhy/lyrebird/internal/store"
	"github.com/huanghaoyuanhhy/lyrebird/internal/translate"
	"github.com/huanghaoyuanhhy/lyrebird/internal/translate/es"
)

// writeUnsupported reports a valid-but-unimplemented ES capability in the
// translator's unsupported_exception vocabulary.
func writeUnsupported(w http.ResponseWriter, what string) {
	writeErrorEnvelope(w, http.StatusBadRequest, "unsupported_exception",
		what+" is beyond lyrebird's Phase 1 surface", "")
}

// search handles GET/POST /{index}/_search: ES Query DSL in, the ES search
// response envelope out. The pipeline is translate (pure) → execute (store) →
// envelope, with translate.Error statuses rendered natively so ES SDK error
// parsing keeps working.
func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	logger := s.zapLogger()
	index := r.PathValue("index")

	if index == "_all" || containsComma(index) {
		writeUnsupported(w, "_all / multi-index search")
		return
	}

	body, err := requestBody(r)
	if err != nil {
		writeUnsupported(w, err.Error())
		return
	}

	schema, err := s.exec.Schema(r.Context(), index)
	if err != nil {
		s.writeExecError(logger, w, index, err)
		return
	}

	plan, err := es.Translate(body, schema)
	if err != nil {
		s.writeTranslateError(logger, w, err)
		return
	}

	start := time.Now()
	result, err := s.exec.Search(r.Context(), index, plan)
	if err != nil {
		s.writeExecError(logger, w, index, err)
		return
	}

	s.writeSearchResult(w, index, plan, result, time.Since(start))
}

// requestBody reads the query DSL. GET searches may carry the body in the
// ?source= query parameter (URL-encoded) instead of the request body — some
// ES clients and curl users rely on it. ?q= (query-string query syntax) is a
// separate mini-language and fails fast rather than degrading to match_all.
func requestBody(r *http.Request) ([]byte, error) {
	q := r.URL.Query()
	if q.Has("q") {
		return nil, fmt.Errorf("the q parameter (query-string query syntax)")
	}
	if src := q.Get("source"); src != "" && r.ContentLength == 0 {
		return []byte(src), nil
	}
	body := make([]byte, 0, 4096)
	buf := make([]byte, 4096)
	for {
		n, err := r.Body.Read(buf)
		body = append(body, buf[:n]...)
		if err != nil {
			break // Read returns its error together with the final bytes
		}
	}
	return body, nil
}

// searchEnvelope mirrors the ES _search response: field order matches real
// ES output and every member an SDK touches is present.
type searchEnvelope struct {
	Took     int64  `json:"took"`
	TimedOut bool   `json:"timed_out"`
	Shards   shards `json:"_shards"`
	Hits     hits   `json:"hits"`
}

type shards struct {
	Total      int `json:"total"`
	Successful int `json:"successful"`
	Skipped    int `json:"skipped"`
	Failed     int `json:"failed"`
}

type hits struct {
	Total    total     `json:"total"`
	MaxScore *float64  `json:"max_score"`
	Hits     []hitItem `json:"hits"`
}

// total renders as the 8.x object form; relation is always eq because the
// executor resolves exact counts (docs/design.md store notes).
type total struct {
	Value    int64  `json:"value"`
	Relation string `json:"relation"`
}

type hitItem struct {
	Index  string         `json:"_index"`
	ID     string         `json:"_id"`
	Score  *float64       `json:"_score"`
	Source map[string]any `json:"_source,omitempty"`
}

func (s *Server) writeSearchResult(w http.ResponseWriter, index string, plan *translate.Plan, result *store.SearchResult, took time.Duration) {
	// Phase 1 scores are constant: 1.0 without a sort, null with one — the
	// same shape ES emits for sort-only searches. knn hits ride the same
	// constant even though they have real distances: ES's metric-specific
	// _score normalization (cosine / l2_norm / dot_product formulas) is a
	// Phase 5 job, to be written against the ES docs, not from memory. The
	// nearest-first ordering is already correct — the store preserves it.
	sorted := len(plan.Sort) > 0
	one := 1.0
	score := func() *float64 {
		if sorted {
			return nil
		}
		return &one
	}

	items := make([]hitItem, len(result.Hits))
	for i, h := range result.Hits {
		items[i] = hitItem{
			Index:  index,
			ID:     h.ID,
			Score:  score(),
			Source: h.Source,
		}
	}

	var maxScore *float64
	if !sorted && len(items) > 0 {
		maxScore = &one
	}

	writeJSON(w, http.StatusOK, searchEnvelope{
		Took:   took.Milliseconds(),
		Shards: shards{Total: 1, Successful: 1},
		Hits: hits{
			Total:    total{Value: result.Total, Relation: "eq"},
			MaxScore: maxScore,
			Hits:     items,
		},
	})
}

// writeTranslateError renders translate errors in the ES error envelope,
// keeping the translator's own classification (type + status).
func (s *Server) writeTranslateError(logger *zap.Logger, w http.ResponseWriter, err error) {
	te := &translate.Error{}
	if !errors.As(err, &te) {
		te = &translate.Error{Type: "parsing_exception", Status: http.StatusBadRequest, Reason: err.Error()}
	}
	logger.Warn("search rejected",
		zap.String("error_type", te.Type), zap.String("reason", te.Reason))
	writeErrorEnvelope(w, te.Status, te.Type, te.Reason, "")
}

// writeExecError maps store/execution failures: a missing collection is the
// ES index_not_found_exception (404); anything else is a failed search phase.
func (s *Server) writeExecError(logger *zap.Logger, w http.ResponseWriter, index string, err error) {
	if errors.Is(err, store.ErrCollectionNotFound) {
		reason := fmt.Sprintf("no such index [%s]", index)
		logger.Info("search on missing index", zap.String("index", index))
		writeErrorEnvelope(w, http.StatusNotFound, "index_not_found_exception", reason, index)
		return
	}
	logger.Error("search execution failed", zap.String("index", index), zap.Error(err))
	writeErrorEnvelope(w, http.StatusInternalServerError, "search_phase_execution_exception",
		"search against milvus failed: "+err.Error(), index)
}

func containsComma(s string) bool {
	for _, r := range s {
		if r == ',' {
			return true
		}
	}
	return false
}
