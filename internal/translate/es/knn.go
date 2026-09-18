package es

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// knn is the lowered form of the top-level knn clause before it joins the
// plan: the ANN target, the query vector, the k window, and the filter
// clauses (still ES query IR — lowered by the shared buildExpr path).
type knnClause struct {
	field  string
	vector []float32
	k      int
	kSet   bool
	filter []queryNode
}

// parseKnn parses the top-level knn object form (a single clause). ES 8.9's
// array form (several knn sub-searches fused by score) is valid DSL beyond
// what one SearchSpec can carry — unsupported, not a parse error.
//
// Members: field and query_vector are required; k windows the plan (default:
// the top-level size, itself defaulting to 10); filter takes a query clause
// or an array of clauses (ANDed together); num_candidates is accepted and
// ignored — Milvus tunes its own ANN search width, and the member only
// affects recall quality, not result semantics; boost and _name are cosmetic.
// similarity (a score floor) needs Milvus range search — unsupported for now.
// No metric is emitted: ES takes the distance from the dense_vector mapping,
// so the store executes with the field's index metric (the empty-Metric
// contract in translate.SearchSpec).
func parseKnn(raw json.RawMessage) (knnClause, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) > 0 && raw[0] == '[' {
		return knnClause{}, unsupportedErrf("[knn] array form (multiple knn clauses with score fusion) is not supported; send one knn clause per search")
	}
	obj, err := expectObject(raw, "knn")
	if err != nil {
		return knnClause{}, err
	}
	c := knnClause{}
	for name, body := range obj {
		switch name {
		case "field":
			if err := json.Unmarshal(body, &c.field); err != nil {
				return knnClause{}, parseErrf("[knn] field must be a string")
			}
		case "query_vector":
			if c.vector, err = parseQueryVector(body); err != nil {
				return knnClause{}, err
			}
		case "k":
			n, decErr := decodeInt(body)
			if decErr != nil || n < 1 {
				return knnClause{}, illegalErrf("[knn] k must be a positive integer, got %s", bytes.TrimSpace(body))
			}
			c.k, c.kSet = n, true
		case "filter":
			if c.filter, err = parseClauseList(body, "knn filter"); err != nil {
				return knnClause{}, err
			}
		case "num_candidates":
			// accepted but ignored: see the function comment
			if n, decErr := decodeInt(body); decErr != nil || n < 1 {
				return knnClause{}, illegalErrf("[knn] num_candidates must be a positive integer, got %s", bytes.TrimSpace(body))
			}
		case "similarity":
			return knnClause{}, unsupportedErrf("[knn] similarity threshold is not supported (needs Milvus range search)")
		case "boost", "_name":
			// no scoring in Phase 1; cosmetic
		default:
			return knnClause{}, parseErrf("[knn] unknown parameter [%s]", name)
		}
	}
	if c.field == "" {
		return knnClause{}, parseErrf("[knn] query requires a [field]")
	}
	if c.vector == nil {
		return knnClause{}, parseErrf("[knn] query requires a [query_vector]")
	}
	return c, nil
}

// parseQueryVector reads the query_vector member: an array of numbers. A
// non-array is a malformed shape (parsing_exception); a non-numeric element
// is a bad value (illegal_argument_exception) — the package's shape-vs-value
// split. Dimension checking stays with Milvus, which names the expected dim.
func parseQueryVector(raw json.RawMessage) ([]float32, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '[' {
		return nil, parseErrf("[knn] query_vector must be an array of numbers")
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, parseErrf("[knn] query_vector must be an array of numbers: %v", err)
	}
	vec := make([]float32, 0, len(items))
	for i, item := range items {
		n, err := decodeNumber(item)
		if err != nil {
			return nil, illegalErrf("[knn] query_vector element %d must be a number, got %s", i, bytes.TrimSpace(item))
		}
		f, err := n.Float64()
		if err != nil {
			return nil, illegalErrf("[knn] query_vector element %d does not parse as a number", i)
		}
		vec = append(vec, float32(f))
	}
	if len(vec) == 0 {
		return nil, illegalErrf("[knn] query_vector must not be empty")
	}
	return vec, nil
}

// decodeNumber decodes one raw JSON value that must be a number.
func decodeNumber(raw json.RawMessage) (json.Number, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", err
	}
	n, ok := v.(json.Number)
	if !ok {
		return "", fmt.Errorf("not a number")
	}
	return n, nil
}
