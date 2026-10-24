package esserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/huanghaoyuanhhy/lyrebird/internal/milvustest"
	"github.com/huanghaoyuanhhy/lyrebird/internal/store"
)

// matrixCase is one point of the bool cartesian product.
type matrixCase struct {
	name  string
	index string
	body  string
}

// TestEsserverParityBoolMatrix sweeps the cartesian product of bool-query
// shapes — bucket subsets × clause counts × minimum_should_match × nesting —
// replaying every shape against a real Elasticsearch as the oracle. The
// judge is what a client observes: the status code, hits.total.value, and
// the hit ids as a set (no case sorts, so ranking order stays out of
// scope). Both sides agreeing to reject counts as green. A mismatch is one
// silent-wrong-answer candidate; the failing set is the semantic backlog
// this suite exists to enumerate, and fixes land here in triaged order.
func TestEsserverParityBoolMatrix(t *testing.T) {
	cfg, ok := milvustest.FromEnv()
	if !ok {
		t.Skipf("set %s and %s to run the parity suite", milvustest.URIEnv, milvustest.TokenEnv)
	}
	esAddr := os.Getenv(esAddrEnv)
	if esAddr == "" {
		t.Skipf("set %s to run the parity suite", esAddrEnv)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	if err := milvustest.Seed(ctx, cfg); err != nil {
		t.Fatalf("seed fixture: %v", err)
	}
	seedElasticsearch(t, esAddr)

	exec, err := store.NewMilvusExecutor(ctx, store.MilvusConfig{URI: cfg.URI, Token: cfg.Token})
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	defer exec.Close(ctx)

	srv := httptest.NewServer(New(exec, zap.NewNop()))
	t.Cleanup(srv.Close)

	for _, tc := range boolMatrixCases() {
		t.Run(tc.name, func(t *testing.T) {
			refStatus, ref := paritySearch(t, esAddr, tc.index, tc.body)
			gwStatus, gw := paritySearch(t, srv.URL, tc.index, tc.body)
			if gwStatus != refStatus {
				t.Fatalf("status = %d, want %d\nquery: %s\nreference: %v\ngateway: %v", gwStatus, refStatus, tc.body, ref, gw)
			}
			if refStatus != http.StatusOK {
				return // both sides reject the shape: agreed, green
			}
			refTotal, refIDs := parityHits(t, ref)
			gwTotal, gwIDs := parityHits(t, gw)
			if gwTotal != refTotal {
				t.Errorf("hits.total.value = %d, want %d\nquery: %s", gwTotal, refTotal, tc.body)
			}
			sort.Strings(gwIDs)
			sort.Strings(refIDs)
			if strings.Join(gwIDs, ",") != strings.Join(refIDs, ",") {
				t.Errorf("hit id set = %v, want %v\nquery: %s", gwIDs, refIDs, tc.body)
			}
		})
	}
}

// boolLeaves is the clause pool the matrix rotates over. Each leaf's match
// set over the scalar fixture is chosen so the bucket algebra shows: terms
// on keyword/bool/nullable fields, two ranges, a terms set. The nullable
// note field is placed into the must_not rotation on purpose — not over a
// missing value is the known three-valued trap (docs/design.md).
var boolLeaves = []struct{ label, clause string }{
	{"name=bravo", `{"term":{"name":"bravo"}}`},
	{"active=true", `{"term":{"active":true}}`},
	{"price>=15", `{"range":{"price":{"gte":15}}}`},
	{"qty<3", `{"range":{"qty":{"lt":3}}}`},
	{"qty in [2,4]", `{"terms":{"qty":[2,4]}}`},
	{"note=alpha note", `{"term":{"note":"alpha note"}}`},
	{"note exists", `{"exists":{"field":"note"}}`},
}

// boolBuckets fixes the subset bit order and each bucket's two rotated
// leaves, so clause assignment is deterministic without any map iteration.
var boolBuckets = []struct {
	key     string // subset label letter and double-variant suffix
	field   string // bool body key
	clauses [2]int // indexes into boolLeaves
}{
	{"M", "must", [2]int{0, 1}},
	{"S", "should", [2]int{2, 3}},
	{"F", "filter", [2]int{4, 5}},
	{"N", "must_not", [2]int{5, 6}},
}

func boolClause(bucket, clause int) string {
	return boolLeaves[boolBuckets[bucket].clauses[clause]].clause
}

// boolBucketBody renders one bool body: one clause per present bucket, an
// optional second clause in exactly one bucket, an optional explicit msm.
func boolBucketBody(present []int, double, msm int) string {
	var parts []string
	if msm >= 0 {
		parts = append(parts, `"minimum_should_match":`+strconv.Itoa(msm))
	}
	for _, b := range present {
		clauses := []string{boolClause(b, 0)}
		if b == double {
			clauses = append(clauses, boolClause(b, 1))
		}
		parts = append(parts, `"`+boolBuckets[b].field+`":[`+strings.Join(clauses, ",")+`]`)
	}
	return `{"query":{"bool":{` + strings.Join(parts, ",") + `}}}`
}

func boolSubsetLabel(present []int) string {
	keys := make([]string, len(present))
	for i, b := range present {
		keys[i] = boolBuckets[b].key
	}
	return strings.Join(keys, "+")
}

// boolMatrixCases enumerates the sweep: every non-empty bucket subset with
// one clause per bucket, a second clause tried in each bucket in turn, and
// the explicit msm controls on every subset carrying a should; then the
// hand-picked nesting, degenerate, and analyzed-text shapes that generation
// cannot reach.
func boolMatrixCases() []matrixCase {
	var cases []matrixCase
	for mask := 1; mask < 1<<len(boolBuckets); mask++ {
		var present []int
		for b := range boolBuckets {
			if mask&(1<<b) != 0 {
				present = append(present, b)
			}
		}
		label := boolSubsetLabel(present)
		cases = append(cases, matrixCase{label + "-x1", milvustest.Collection, boolBucketBody(present, -1, -1)})
		for _, b := range present {
			cases = append(cases, matrixCase{label + "-" + boolBuckets[b].key + "2", milvustest.Collection, boolBucketBody(present, b, -1)})
		}
		if mask&(1<<1) != 0 { // subsets carrying a should bucket
			cases = append(cases,
				matrixCase{label + "-x1-msm0", milvustest.Collection, boolBucketBody(present, -1, 0)},
				matrixCase{label + "-x1-msm1", milvustest.Collection, boolBucketBody(present, -1, 1)},
				matrixCase{label + "-S2-msm1", milvustest.Collection, boolBucketBody(present, 1, 1)},
			)
		}
	}

	var hand []matrixCase
	hand = append(hand,
		// Nesting: an inner bool is itself a clause, and its own msm default
		// is computed per bool, not inherited from the outer shape.
		matrixCase{"nested-should-in-must", milvustest.Collection,
			`{"query":{"bool":{"must":[{"bool":{"should":[{"term":{"name":"bravo"}},{"term":{"name":"echo"}}]}}],"should":[{"term":{"active":true}}]}}}`},
		matrixCase{"nested-msm1-in-must-inner-empty", milvustest.Collection,
			`{"query":{"bool":{"must":[{"bool":{"must":[{"term":{"active":true}}],"should":[{"term":{"name":"bravo"}}],"minimum_should_match":1}}]}}}`},
		matrixCase{"nested-should-in-filter", milvustest.Collection,
			`{"query":{"bool":{"filter":[{"bool":{"should":[{"term":{"name":"bravo"}},{"term":{"name":"echo"}}]}}],"should":[{"term":{"active":true}}]}}}`},
		matrixCase{"nested-must-in-should", milvustest.Collection,
			`{"query":{"bool":{"should":[{"bool":{"must":[{"term":{"active":true}}]}},{"term":{"name":"echo"}}]}}}`},
		matrixCase{"nested-should-in-must_not", milvustest.Collection,
			`{"query":{"bool":{"must_not":[{"bool":{"should":[{"term":{"name":"bravo"}},{"term":{"name":"echo"}}]}}]}}}`},
		matrixCase{"nested-must_not-exists-in-must", milvustest.Collection,
			`{"query":{"bool":{"must":[{"bool":{"must_not":[{"exists":{"field":"note"}}]}}]}}}`},
		matrixCase{"double-nested-should-in-must", milvustest.Collection,
			`{"query":{"bool":{"must":[{"bool":{"must":[{"bool":{"should":[{"term":{"name":"bravo"}},{"term":{"name":"echo"}}]}}]}}]}}}`},

		// Degenerate shapes: empty buckets and msm without a should.
		matrixCase{"empty-bool-is-match_all", milvustest.Collection, `{"query":{"bool":{}}}`},
		matrixCase{"empty-should-array-is-match_all", milvustest.Collection, `{"query":{"bool":{"should":[]}}}`},
		matrixCase{"empty-must-array-is-match_all", milvustest.Collection, `{"query":{"bool":{"must":[]}}}`},
		matrixCase{"msm-without-should", milvustest.Collection, `{"query":{"bool":{"minimum_should_match":1}}}`},

		// Analyzed text inside bool: match leaves tokenize (OR across
		// tokens), and the bucket algebra around them must not change the
		// matched token sets.
		matrixCase{"text-should-ors-two-matches", milvustest.TextCollection,
			`{"query":{"bool":{"should":[{"match":{"body":"quick"}},{"match":{"body":"brown"}}]}}}`},
		matrixCase{"text-must-plus-should-default", milvustest.TextCollection,
			`{"query":{"bool":{"must":[{"match":{"body":"quick"}}],"should":[{"match":{"body":"brown"}}]}}}`},
		matrixCase{"text-must-plus-should-msm1", milvustest.TextCollection,
			`{"query":{"bool":{"must":[{"match":{"body":"quick"}}],"should":[{"match":{"body":"brown"}}],"minimum_should_match":1}}}`},
		matrixCase{"text-must_not-excludes-by-token", milvustest.TextCollection,
			`{"query":{"bool":{"must_not":[{"match":{"body":"quick"}}]}}}`},
		matrixCase{"text-filter-plus-must_not", milvustest.TextCollection,
			`{"query":{"bool":{"filter":[{"match":{"body":"brown"}}],"must_not":[{"match":{"body":"quick"}}]}}}`},
		matrixCase{"text-should2-over-tokens", milvustest.TextCollection,
			`{"query":{"bool":{"should":[{"match":{"body":"barks"}},{"match":{"body":"turtle"}}]}}}`},
	)
	cases = append(cases, hand...)

	return cases
}

// TestBoolMatrixGeneratorSanity keeps the generated sweep honest without
// any services: the case list is deterministic, names are unique, every
// body parses as JSON, and the sweep stays large enough to deserve the
// name cartesian product.
func TestBoolMatrixGeneratorSanity(t *testing.T) {
	first := boolMatrixCases()
	second := boolMatrixCases()
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("case generation is not deterministic: %d vs %d cases", len(first), len(second))
	}
	if len(first) < 80 {
		t.Fatalf("only %d cases generated; the sweep lost combinations", len(first))
	}
	t.Logf("matrix: %d cases", len(first))
	seen := map[string]bool{}
	for _, tc := range first {
		if seen[tc.name] {
			t.Errorf("duplicate case name %q", tc.name)
		}
		seen[tc.name] = true
		var body map[string]any
		if err := json.Unmarshal([]byte(tc.body), &body); err != nil {
			t.Errorf("case %s: body does not parse: %v", tc.name, err)
		}
		if tc.index != milvustest.Collection && tc.index != milvustest.TextCollection {
			t.Errorf("case %s: unknown index %q", tc.name, tc.index)
		}
	}
}
