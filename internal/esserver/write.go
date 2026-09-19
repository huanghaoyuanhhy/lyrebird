package esserver

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"time"

	"go.uber.org/zap"

	"github.com/milvus-io/milvus/client/v3/entity"

	"github.com/huanghaoyuanhhy/lyrebird/internal/catalog"
	"github.com/huanghaoyuanhhy/lyrebird/internal/store"
	"github.com/huanghaoyuanhhy/lyrebird/internal/translate"
	"github.com/huanghaoyuanhhy/lyrebird/internal/translate/es"
)

// installWriteRoutes registers the document write APIs: the index API
// (PUT/POST _doc, _create) and the partial-update API (_update). All of
// them run the strict write path: a document that does not match the
// index's live mapping is rejected, never coerced, never dynamically
// mapped (docs/design.md, write path).
func (s *Server) installWriteRoutes() {
	s.mux.HandleFunc("PUT /{index}/_doc", s.indexDocument)
	s.mux.HandleFunc("POST /{index}/_doc", s.indexDocument)
	s.mux.HandleFunc("PUT /{index}/_doc/{id}", s.indexDocument)
	s.mux.HandleFunc("POST /{index}/_doc/{id}", s.indexDocument)
	s.mux.HandleFunc("PUT /{index}/_create/{id}", s.createDocument)
	s.mux.HandleFunc("POST /{index}/_create/{id}", s.createDocument)
	s.mux.HandleFunc("PUT /{index}/_update/{id}", s.updateDocument)
	s.mux.HandleFunc("POST /{index}/_update/{id}", s.updateDocument)

	// _bulk would batch the same calls, but its ndjson envelope deserves
	// its own pass; fail with the name rather than a 404.
	s.mux.HandleFunc("POST /_bulk", func(w http.ResponseWriter, r *http.Request) {
		writeUnsupported(w, "_bulk (use the per-document APIs)")
	})
	s.mux.HandleFunc("POST /{index}/_bulk", func(w http.ResponseWriter, r *http.Request) {
		writeUnsupported(w, "_bulk (use the per-document APIs)")
	})
}

// indexDocument handles PUT/POST /{index}/_doc[/{id}]: index a document,
// replacing whatever row the primary key holds (the ES index API's put
// semantics — an upsert). With no URL id, the id comes from the schema: a
// VarChar primary key gets a generated one, an auto-generated Int64 key is
// filled by Milvus and reported.
func (s *Server) indexDocument(w http.ResponseWriter, r *http.Request) {
	s.writeDocument(w, r, writePlan{
		id:     r.PathValue("id"),
		create: r.URL.Query().Get("op_type") == "create",
	})
}

// createDocument handles PUT/POST /{index}/_create/{id}: index only if the
// id is free, 409 otherwise.
func (s *Server) createDocument(w http.ResponseWriter, r *http.Request) {
	s.writeDocument(w, r, writePlan{id: r.PathValue("id"), create: true})
}

// writePlan carries the URL-shaped facts the index API resolves per route.
type writePlan struct {
	id     string
	create bool
}

// writeDocument is the index API body: resolve the id against the primary
// key, check existence, insert or upsert. Every failure renders in ES
// vocabulary so SDK error parsing keeps working.
func (s *Server) writeDocument(w http.ResponseWriter, r *http.Request, plan writePlan) {
	logger := s.zapLogger()
	index := r.PathValue("index")
	start := time.Now()

	if err := rejectWriteParams(r); err != nil {
		writeUnsupported(w, err.Error())
		return
	}
	meta, err := s.cluster.Collection(r.Context(), "", index)
	if err != nil {
		s.writeExecError(logger, w, index, err)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read the request body: "+err.Error())
		return
	}
	row, err := es.DecodeDocument(body, schemaOf(meta))
	if err != nil {
		s.writeTranslateError(logger, w, err)
		return
	}

	pk := primaryKeyOf(meta)
	id, err := resolveDocumentID(meta, row, plan.id)
	if err != nil {
		s.writeRequestError(logger, w, err, index)
		return
	}

	// An auto-generated key has no id to look up yet: the write is always
	// an insert, and Milvus reports the id it assigned.
	exists := false
	if !pk.AutoID {
		exists, err = s.documentExists(r, index, pk, id)
		if err != nil {
			s.writeExecError(logger, w, index, err)
			return
		}
		if plan.create && exists {
			reason := fmt.Sprintf("[%s][%s]: version conflict, document already exists", index, id)
			writeErrorEnvelope(w, http.StatusConflict, "version_conflict_exception", reason, index)
			return
		}
		row[pk.Name] = id
	}

	write := s.cluster.Insert
	result := "created"
	if exists {
		write = s.cluster.Upsert
		result = "updated"
	}
	wrote, err := write(r.Context(), index, []translate.WriteRow{row})
	if err != nil {
		logger.Error("index execution failed", zap.String("index", index), zap.Error(err))
		s.writeWriteError(logger, w, err, index)
		return
	}
	// an auto-generated key carries the id the store assigned
	idStr := idString(id)
	if idStr == "" && len(wrote.IDs) > 0 {
		idStr = wrote.IDs[0]
	}
	logger.Info("es index",
		zap.String("index", index),
		zap.String("id", idStr),
		zap.String("result", result),
		zap.Duration("took", time.Since(start)))
	writeIndexResponse(w, index, idStr, result)
}

// updateDocument handles PUT/POST /{index}/_update/{id}: merge the doc
// member into the stored row and write it back whole. The script form is
// beyond the surface; a missing document is a 404 (the ES default without
// doc_as_upsert).
func (s *Server) updateDocument(w http.ResponseWriter, r *http.Request) {
	logger := s.zapLogger()
	index := r.PathValue("index")
	id := r.PathValue("id")
	start := time.Now()

	meta, err := s.cluster.Collection(r.Context(), "", index)
	if err != nil {
		s.writeExecError(logger, w, index, err)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read the request body: "+err.Error())
		return
	}
	doc, err := parseUpdateBody(body)
	if err != nil {
		s.writeRequestError(logger, w, err, index)
		return
	}
	docRow, err := es.DecodeDocument(doc, schemaOf(meta))
	if err != nil {
		s.writeTranslateError(logger, w, err)
		return
	}

	pk := primaryKeyOf(meta)
	// the URL id parses into the key's storage shape; the primary key is an
	// address, not a mutable field
	parsed, err := parseDocumentID(pk, id)
	if err != nil {
		s.writeRequestError(logger, w, err, index)
		return
	}
	if v, ok := docRow[pk.Name]; ok && !valuesEqual(parsed, v) {
		s.writeRequestError(logger, w, illegalArgf("the document's primary key value [%v] disagrees with the URL id [%s]", v, id), index)
		return
	}

	old, exists, err := s.fetchDocument(r, index, pk, pkValue(pk, parsed))
	if err != nil {
		s.writeExecError(logger, w, index, err)
		return
	}
	if !exists {
		reason := fmt.Sprintf("[%s][%s]: document missing", index, id)
		writeErrorEnvelope(w, http.StatusNotFound, "document_missing_exception", reason, index)
		return
	}

	merged := make(translate.WriteRow, len(old)+len(docRow))
	for k, v := range old {
		merged[k] = v
	}
	for k, v := range docRow {
		merged[k] = v
	}
	result := "updated"
	if rowsEqual(old, merged) {
		// the ES detect_noop default: nothing changed, nothing written
		result = "noop"
	} else if _, err := s.cluster.Upsert(r.Context(), index, []translate.WriteRow{merged}); err != nil {
		logger.Error("update execution failed", zap.String("index", index), zap.Error(err))
		s.writeWriteError(logger, w, err, index)
		return
	}
	logger.Info("es update",
		zap.String("index", index),
		zap.String("id", id),
		zap.String("result", result),
		zap.Duration("took", time.Since(start)))
	writeIndexResponse(w, index, id, result)
}

// parseDocumentID turns the URL id into the primary key's storage shape:
// a string key takes the id as-is, a numeric key parses (and a
// non-numeric id is a 400, the same answer resolveDocumentID gives).
func parseDocumentID(pk catalog.FieldMeta, id string) (any, error) {
	if pk.Native != "string" {
		n, err := strconv.ParseInt(id, 10, 64)
		if err != nil {
			return nil, illegalArgf("[%s] does not parse as the numeric primary key of the index", id)
		}
		return n, nil
	}
	return id, nil
}

// parseUpdateBody extracts the doc member of an _update body, rejecting the
// script form and doc_as_upsert by name.
func parseUpdateBody(body []byte) ([]byte, error) {
	var top map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&top); err != nil {
		return nil, parseArgf("the _update body is not valid JSON: %v", err)
	}
	if _, ok := top["script"]; ok {
		return nil, illegalArgf("[script] updates are beyond lyrebird's surface: send the fields to merge under [doc]")
	}
	if _, ok := top["scripted_upsert"]; ok {
		return nil, illegalArgf("[scripted_upsert] is beyond lyrebird's surface: it only matters with [script]")
	}
	if _, ok := top["doc_as_upsert"]; ok {
		return nil, illegalArgf("[doc_as_upsert] is not supported: a missing document is a 404, create it first")
	}
	doc, ok := top["doc"]
	if !ok {
		return nil, parseArgf("the _update body needs a [doc] member naming the fields to merge")
	}
	return []byte(doc), nil
}

// resolveDocumentID reconciles the URL id, the document's primary-key value
// and the schema's key shape into the id to write:
//
//   - VarChar pk: the URL id, generated when absent; a pk value inside the
//     document must agree with it.
//   - Int64 pk (client-assigned): the URL id parsed as a number; same
//     agreement rule.
//   - Int64 pk (auto-generated): no URL id may be given; the store assigns
//     and reports the id.
func resolveDocumentID(meta catalog.CollectionMeta, row translate.WriteRow, urlID string) (any, error) {
	pk := primaryKeyOf(meta)
	v, inDoc := row[pk.Name]

	switch {
	case pk.AutoID:
		if urlID != "" {
			return nil, illegalArgf("document id [%s] given for index [%s] whose primary key is auto-generated: POST without an id", urlID, meta.Name)
		}
		return nil, nil

	case pk.Native == "string":
		if urlID == "" {
			urlID = generateDocumentID()
		}
		if inDoc {
			s, ok := v.(string)
			if !ok || s != urlID {
				return nil, mismatchErrf(meta.Name, v, urlID)
			}
		}
		return urlID, nil

	default: // Int64 pk, client-assigned
		if urlID == "" {
			return nil, illegalArgf("index [%s] primary key is numeric: PUT with an explicit id (the generated ES id is not a number)", meta.Name)
		}
		n, err := strconv.ParseInt(urlID, 10, 64)
		if err != nil {
			return nil, illegalArgf("[%s] does not parse as the numeric primary key of index [%s]", urlID, meta.Name)
		}
		if inDoc {
			d, ok := docNumber(v)
			if !ok || int64(d) != n {
				return nil, mismatchErrf(meta.Name, v, urlID)
			}
		}
		return n, nil
	}
}

// generateDocumentID produces a generated id: 20 url-safe base64
// characters, the same shape ES's auto ids have.
func generateDocumentID() string {
	b := make([]byte, 15)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("lyrebird-%d", time.Now().UnixNano())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func mismatchErrf(index string, got any, want string) error {
	g := "<null>"
	if got != nil {
		g = fmt.Sprintf("%v", got)
	}
	return illegalArgf("document primary key value [%s] disagrees with the URL id [%s] of index [%s]", g, want, index)
}

// docNumber reads a numeric document value for the pk agreement check.
func docNumber(v any) (float64, bool) {
	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case int64:
		return float64(x), true
	case float64:
		return x, true
	default:
		return 0, false
	}
}

// documentExists answers whether a primary key already has a row.
func (s *Server) documentExists(r *http.Request, index string, pk catalog.FieldMeta, id any) (bool, error) {
	_, exists, err := s.fetchDocument(r, index, pk, pkValue(pk, id))
	return exists, err
}

// fetchDocument fetches one row by primary key, whole. exists distinguishes
// "no such row" from other failures.
func (s *Server) fetchDocument(r *http.Request, index string, pk catalog.FieldMeta, id translate.Value) (map[string]any, bool, error) {
	plan := &translate.Plan{
		Limit:  1,
		Source: translate.SourceFilter{FetchSource: true},
		Expr:   translate.Compare{Op: translate.Eq, Field: pk.Name, Value: id},
	}
	result, err := s.cluster.Search(r.Context(), index, plan)
	if err != nil {
		return nil, false, err
	}
	if len(result.Hits) == 0 {
		return nil, false, nil
	}
	return result.Hits[0].Source, true, nil
}

// pkValue wraps an id as the filter literal Render quotes.
func pkValue(pk catalog.FieldMeta, id any) translate.Value {
	if n, ok := id.(int64); ok {
		return translate.IntValue(n)
	}
	s, _ := id.(string)
	return translate.StringValue(s)
}

func idString(id any) string {
	if id == nil {
		return ""
	}
	if n, ok := id.(int64); ok {
		return strconv.FormatInt(n, 10)
	}
	s, _ := id.(string)
	return s
}

func primaryKeyOf(meta catalog.CollectionMeta) catalog.FieldMeta {
	for _, f := range meta.Fields {
		if f.PrimaryKey {
			return f
		}
	}
	return catalog.FieldMeta{}
}

// schemaOf projects the collection metadata onto the translate vocabulary
// the document decoder reads.
func schemaOf(meta catalog.CollectionMeta) translate.MapSchema {
	m := make(translate.MapSchema, len(meta.Fields))
	for _, f := range meta.Fields {
		m[f.Name] = f.Type
	}
	return m
}

// rejectWriteParams refuses the query parameters whose silence would lie:
// versioning would promise an optimistic-concurrency guarantee the store
// does not provide. refresh/routing/timeout are accepted and ignored —
// they shape timing and placement, not semantics.
func rejectWriteParams(r *http.Request) error {
	q := r.URL.Query()
	for _, p := range []string{"version", "if_seq_no", "if_primary_term"} {
		if q.Has(p) {
			return fmt.Errorf("the %s parameter (optimistic concurrency control)", p)
		}
	}
	return nil
}

// writeIndexResponse emits the index/update response envelope. A fresh
// insert reports result=created with status 201, an update 200, a noop
// 200. The store has no per-row versioning, so _version is always 1 and
// _seq_no/_primary_term hold neutral placeholders.
func writeIndexResponse(w http.ResponseWriter, index, id, result string) {
	status := http.StatusOK
	if result == "created" {
		status = http.StatusCreated
	}
	writeJSON(w, status, map[string]any{
		"_index":        index,
		"_id":           id,
		"_version":      1,
		"result":        result,
		"_shards":       map[string]any{"total": 1, "successful": 1, "failed": 0},
		"_seq_no":       0,
		"_primary_term": 1,
		"status":        status,
	})
}

// writeRequestError renders a request-shaped (4xx) failure carrying its own
// translate.Error classification.
func (s *Server) writeRequestError(logger *zap.Logger, w http.ResponseWriter, err error, index string) {
	te := &translate.Error{}
	if !errors.As(err, &te) {
		te = &translate.Error{Type: "illegal_argument_exception", Status: http.StatusBadRequest, Reason: err.Error()}
	}
	logger.Warn("write rejected", zap.String("error_type", te.Type), zap.String("reason", te.Reason))
	writeErrorEnvelope(w, te.Status, te.Type, te.Reason, index)
}

// writeWriteError renders a write-path failure in ES vocabulary: strict
// schema classes map onto the exceptions a real ES server sends for the
// same mistake.
func (s *Server) writeWriteError(logger *zap.Logger, w http.ResponseWriter, err error, index string) {
	var we *store.WriteError
	if !errors.As(err, &we) {
		logger.Error("write execution failed", zap.String("index", index), zap.Error(err))
		writeErrorEnvelope(w, http.StatusInternalServerError, "internal_error_exception",
			"write against milvus failed: "+err.Error(), index)
		return
	}
	logger.Warn("write rejected",
		zap.String("class", string(we.Class)), zap.String("field", we.Field), zap.String("reason", we.Reason))
	errType, status := esExceptionOf(we.Class)
	writeErrorEnvelope(w, status, errType, we.Reason, index)
}

// esExceptionOf maps a write rejection class onto the ES exception name a
// real server uses for the same mistake.
func esExceptionOf(c store.WriteErrClass) (string, int) {
	switch c {
	case store.ClassUnknownField:
		return "strict_dynamic_mapping_exception", http.StatusBadRequest
	case store.ClassType, store.ClassRange, store.ClassNull:
		return "document_parsing_exception", http.StatusBadRequest
	case store.ClassDim, store.ClassLength, store.ClassMissing,
		store.ClassAutoID, store.ClassFunctionField:
		return "illegal_argument_exception", http.StatusBadRequest
	default:
		return "unsupported_exception", http.StatusBadRequest
	}
}

// illegalArgf / parseArgf build the ES error classifications the write path
// raises before the store is ever touched.
func illegalArgf(format string, args ...any) *translate.Error {
	return &translate.Error{Type: "illegal_argument_exception", Status: http.StatusBadRequest, Reason: fmt.Sprintf(format, args...)}
}

func parseArgf(format string, args ...any) *translate.Error {
	return &translate.Error{Type: "parsing_exception", Status: http.StatusBadRequest, Reason: fmt.Sprintf(format, args...)}
}

// rowsEqual compares a fetched row with its merged form for the noop check.
// Numbers compare by value across the two encodings (stored int64/float64
// vs written json.Number); JSON fields compare as parsed values, so
// re-sending byte-different but semantically identical JSON is still a noop.
func rowsEqual(a, b translate.WriteRow) bool {
	if len(a) != len(b) {
		return false
	}
	for k, va := range a {
		vb, ok := b[k]
		if !ok || !valuesEqual(va, vb) {
			return false
		}
	}
	return true
}

func valuesEqual(a, b any) bool {
	// reads hand back the SDK's named float-vector slice; compare it as the
	// plain slice the write vocabulary uses
	if fv, ok := a.(entity.FloatVector); ok {
		return valuesEqual([]float32(fv), b)
	}
	if fv, ok := b.(entity.FloatVector); ok {
		return valuesEqual(a, []float32(fv))
	}
	switch x := a.(type) {
	case nil:
		return b == nil
	case string:
		y, ok := b.(string)
		return ok && x == y
	case bool:
		y, ok := b.(bool)
		return ok && x == y
	case json.Number:
		return numberEqual(x, b)
	case int64:
		return numberEqual(json.Number(strconv.FormatInt(x, 10)), b)
	case float64:
		return numberEqual(json.Number(strconv.FormatFloat(x, 'g', -1, 64)), b)
	case []float32:
		y, ok := b.([]float32)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if math.Float32bits(x[i]) != math.Float32bits(y[i]) {
				return false
			}
		}
		return true
	case []byte:
		return jsonEqual(x, b)
	case json.RawMessage:
		return jsonEqual([]byte(x), b)
	default:
		return false
	}
}

func numberEqual(a json.Number, b any) bool {
	af, err := a.Float64()
	if err != nil {
		return false
	}
	switch y := b.(type) {
	case json.Number:
		bf, err := y.Float64()
		return err == nil && af == bf
	case int64:
		return af == float64(y)
	case float64:
		return af == y
	default:
		return false
	}
}

func jsonEqual(a []byte, b any) bool {
	var pa, pb any
	decA := json.NewDecoder(bytes.NewReader(a))
	decA.UseNumber()
	if err := decA.Decode(&pa); err != nil {
		return false
	}
	decB := json.NewDecoder(bytes.NewReader(jsonBytesOf(b)))
	decB.UseNumber()
	if err := decB.Decode(&pb); err != nil {
		return false
	}
	return jsonValueEqual(pa, pb)
}

func jsonBytesOf(v any) []byte {
	switch x := v.(type) {
	case []byte:
		return x
	case json.RawMessage:
		return []byte(x)
	default:
		return nil
	}
}

func jsonValueEqual(a, b any) bool {
	switch x := a.(type) {
	case nil:
		return b == nil
	case bool:
		y, ok := b.(bool)
		return ok && x == y
	case string:
		y, ok := b.(string)
		return ok && x == y
	case json.Number:
		y, ok := b.(json.Number)
		return ok && x.String() == y.String()
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !jsonValueEqual(x[i], y[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, v := range x {
			w, ok := y[k]
			if !ok || !jsonValueEqual(v, w) {
				return false
			}
		}
		return true
	default:
		return false
	}
}
