package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/milvus-io/milvus/client/v3/column"
	"github.com/milvus-io/milvus/client/v3/entity"
	"github.com/milvus-io/milvus/client/v3/milvusclient"

	"github.com/huanghaoyuanhhy/lyrebird/internal/translate"
)

// MilvusConfig holds the connection settings for MilvusExecutor. Token is
// either an API key (Zilliz Cloud) or a "user:password" pair. DB names the
// Milvus database the executor reads ("" = the server default).
//
// URI forms: "https://host:19530" (TLS on) or "host:19530" (TLS off).
type MilvusConfig struct {
	URI   string
	Token string
	DB    string
}

// MilvusExecutor is the real Executor: it renders plans into Milvus boolean
// expressions and runs them through the official milvus Go client
// (milvus-io/milvus/client/v3). It is the only place in lyrebird that
// speaks to Milvus.
//
// The struct doubles as the introspection half of store.Cluster: Database
// hands out per-database views sharing one client pool, so concurrent PG
// connections attached to different Milvus databases each get their own
// client instead of racing UseDatabase's mutation of a shared one.
type MilvusExecutor struct {
	cfg    MilvusConfig
	cli    *milvusclient.Client
	db     string
	poolMu sync.Mutex
	pool   map[string]*milvusclient.Client
}

// compile-time check that MilvusExecutor satisfies Executor.
var _ Executor = (*MilvusExecutor)(nil)

// NewMilvusExecutor connects to Milvus and returns an executor. The returned
// Close releases the connection.
func NewMilvusExecutor(ctx context.Context, cfg MilvusConfig) (*MilvusExecutor, error) {
	cli, err := milvusclient.New(ctx, &milvusclient.ClientConfig{
		Address:       cfg.URI,
		APIKey:        cfg.Token,
		EnableTLSAuth: strings.HasPrefix(cfg.URI, "https://"),
		DBName:        cfg.DB,
	})
	if err != nil {
		return nil, fmt.Errorf("connect to milvus %q: %w", cfg.URI, err)
	}
	db := cfg.DB
	if db == "" {
		db = "default"
	}
	return &MilvusExecutor{cfg: cfg, cli: cli, db: db, pool: map[string]*milvusclient.Client{}}, nil
}

// Close releases the Milvus connection.
func (e *MilvusExecutor) Close(ctx context.Context) error {
	return e.cli.Close(ctx)
}

// Schema implements Executor: it describes the collection and maps Milvus
// field types onto the protocol-agnostic translate.FieldType vocabulary.
func (e *MilvusExecutor) Schema(ctx context.Context, collection string) (translate.Schema, error) {
	coll, err := e.describe(ctx, collection)
	if err != nil {
		return nil, err
	}
	funcOutputs := functionOutputFields(coll.Schema)
	schema := collectionSchema{types: translate.MapSchema{}}
	for _, f := range coll.Schema.Fields {
		if isFunctionOutput(f.Name, funcOutputs) {
			continue // Milvus refuses to return their raw data (see buildProjection)
		}
		schema.names = append(schema.names, f.Name)
		schema.types[f.Name] = translateFieldType(f)
	}
	return schema, nil
}

// collectionSchema is a described collection's field view: types follow the
// translate vocabulary, names keep the collection's storage order so SELECT *
// expands the way the fields were declared.
type collectionSchema struct {
	names []string
	types translate.MapSchema
}

func (s collectionSchema) FieldType(field string) translate.FieldType {
	return s.types.FieldType(field)
}

// Fields implements translate.Schema, in storage order.
func (s collectionSchema) Fields() []string {
	return append([]string{}, s.names...)
}

// Search implements Executor. Execution shape per plan:
//
//   - NoMatch: guaranteed empty, nothing is sent to Milvus.
//   - Plan.Search != nil: the ANN path — one Milvus search() with the query
//     vector, the filter riding along, limit+offset as topk window. Rows
//     come back nearest-first, the ordering every distance operator promises.
//   - Limit == 0 (ES size:0): count only — Milvus rejects limit 0.
//   - No sort: one query with offset+limit; the underfilled-window check
//     (fewer rows than the limit came back) makes the total exact without
//     a second roundtrip.
//   - With sort: Milvus (2.6-compatible servers) has no query-side ORDER BY
//     (verified 2026-09-14, docs/design.md), so all matches are streamed via
//     a primary-key cursor and sorted client-side. Rows are ordered by the
//     cursor between batches, which sort.SliceStable turns into a
//     deterministic tie-break.
func (e *MilvusExecutor) Search(ctx context.Context, collection string, plan *translate.Plan) (*SearchResult, error) {
	if plan.NoMatch {
		return &SearchResult{}, nil
	}
	if plan.Search != nil {
		return e.vectorSearch(ctx, collection, plan)
	}

	coll, err := e.describe(ctx, collection)
	if err != nil {
		return nil, err
	}
	fields := coll.Schema.Fields
	pk := primaryKey(fields)
	if pk == nil {
		return nil, fmt.Errorf("collection %q has no primary key field", collection)
	}

	proj, err := buildProjection(fields, functionOutputFields(coll.Schema), plan.Source, plan.Sort)
	if err != nil {
		return nil, err
	}

	// nil Expr renders as "" — Milvus reads an empty expression as match-all.
	expr, err := translate.Render(plan.Expr)
	if err != nil {
		return nil, fmt.Errorf("render filter for collection %q: %w", collection, err)
	}

	var rows []row
	switch {
	case plan.Limit == 0:
		// count-only search: skip the page fetch entirely
	case len(plan.Sort) == 0:
		rows, err = e.queryPage(ctx, collection, expr, proj.fetch, pk.Name, plan.Offset, plan.Limit)
		if err != nil {
			return nil, err
		}
	default:
		rows, err = e.scanAll(ctx, collection, expr, proj.fetch, pk, plan.Sort)
		if err != nil {
			return nil, err
		}
	}

	total, err := e.totalMatches(ctx, collection, expr, rows, plan)
	if err != nil {
		return nil, err
	}

	if len(plan.Sort) > 0 {
		sort.SliceStable(rows, func(i, j int) bool {
			return compareRows(rows[i], rows[j], plan.Sort) < 0
		})
		// the sorted scan fetched from offset 0; every other path had the
		// server page with offset+limit already
		rows = window(rows, plan.Offset, plan.Limit)
	}

	hits := make([]Hit, len(rows))
	for i, r := range rows {
		id, err := hitID(r, pk)
		if err != nil {
			return nil, err
		}
		hits[i] = Hit{ID: id, Source: r.project(proj.source)}
	}
	return &SearchResult{Total: total, Hits: hits}, nil
}

// row is one fetched entity keyed by field name. A field absent from the map
// (not fetched, or null) reads as a missing value.
type row map[string]any

// vectorSearch executes the ANN path: one Milvus search() carrying the
// query vector, the scalar filter (if any) and the limit+offset window.
// Total is the returned row count — the only honest number a top-k search
// has; count(*) over the filter would answer a different question.
func (e *MilvusExecutor) vectorSearch(ctx context.Context, collection string, plan *translate.Plan) (*SearchResult, error) {
	if len(plan.Sort) > 0 {
		return nil, fmt.Errorf("vector search on %q cannot combine a distance ordering with scalar sort terms", collection)
	}
	if plan.Limit == 0 {
		return &SearchResult{}, nil // LIMIT 0: an empty top-k, nothing to send
	}

	coll, err := e.describe(ctx, collection)
	if err != nil {
		return nil, err
	}
	fields := coll.Schema.Fields
	pk := primaryKey(fields)
	if pk == nil {
		return nil, fmt.Errorf("collection %q has no primary key field", collection)
	}
	proj, err := buildProjection(fields, functionOutputFields(coll.Schema), plan.Source, nil)
	if err != nil {
		return nil, err
	}

	// nil Expr renders as "" — Milvus reads an empty expression as unfiltered.
	expr, err := translate.Render(plan.Expr)
	if err != nil {
		return nil, fmt.Errorf("render filter for collection %q: %w", collection, err)
	}

	spec := plan.Search
	opt := milvusclient.NewSearchOption(collection, plan.Limit, []entity.Vector{entity.FloatVector(spec.Vector)}).
		WithANNSField(spec.Field).
		WithFilter(expr).
		WithOutputFields(proj.fetch...).
		WithOffset(plan.Offset)
	if spec.Metric != "" {
		// A filled metric is a semantic statement the protocol made: the
		// pgvector operators name their metric (`<=>` means cosine, not
		// whatever the collection was indexed with), so the server rejects
		// a metric that disagrees with the field's index and a cosine
		// question never comes back answered by L2 distances. ES knn stays
		// empty instead — its metric lives in the mapping, so the search
		// rides the index default (docs/design.md, ES knn notes).
		opt.WithSearchParam("metric_type", string(spec.Metric))
	}

	rss, err := e.searchOpt(ctx, collection, opt)
	if err != nil {
		return nil, err
	}
	if len(rss) == 0 {
		return &SearchResult{}, nil
	}

	rows := searchRows(rss[0], proj.fetch, pk.Name)
	hits := make([]Hit, len(rows))
	for i, r := range rows {
		id, err := hitID(r, pk)
		if err != nil {
			return nil, err
		}
		hits[i] = Hit{ID: id, Source: r.project(proj.source)}
	}
	return &SearchResult{Total: int64(len(hits)), Hits: hits}, nil
}

// project keeps only the wanted field names, in schema order.
// A nil/empty list yields an empty (non-nil) source, matching ES _source: {}
// for includes that match nothing.
func (r row) project(keep []string) map[string]any {
	out := make(map[string]any, len(keep))
	for _, f := range keep {
		if v, ok := r[f]; ok {
			out[f] = v
		}
	}
	return out
}

// totalMatches resolves hits.total without guessing: the full-scan and
// underfilled-window cases already know the exact count, everything else
// asks Milvus with a count(*) query.
func (e *MilvusExecutor) totalMatches(ctx context.Context, collection, expr string, rows []row, plan *translate.Plan) (int64, error) {
	switch {
	case len(plan.Sort) > 0:
		return int64(len(rows)), nil // the scan saw every match
	case plan.Limit == 0:
		// fall through to count(*)
	case int64(len(rows)) < int64(plan.Limit):
		// the page came back underfilled, so offset+len is the exact total
		return int64(plan.Offset) + int64(len(rows)), nil
	default:
		// fall through to count(*)
	}
	return e.count(ctx, collection, expr)
}

// queryPage fetches one window of matches.
func (e *MilvusExecutor) queryPage(ctx context.Context, collection, expr string, outputFields []string, pkField string, offset, limit int) ([]row, error) {
	opt := milvusclient.NewQueryOption(collection).
		WithFilter(expr).
		WithOutputFields(outputFields...).
		WithOffset(offset).
		WithLimit(limit)
	rs, err := e.queryOpt(ctx, collection, opt)
	if err != nil {
		return nil, err
	}
	return rowsOf(rs, outputFields), nil
}

// scanBatchSize is how many entities one cursor step of scanAll pulls.
const scanBatchSize = 1000

// scanAll streams every match through a primary-key cursor: each step asks
// for the next batchSize entities after the last-seen PK. The SDK's own
// QueryIterator composes the same cursor but uses server-side iterator
// parameters that Zilliz Cloud 2.6-compatible servers reject (verified
// 2026-09-14), so lyrebird drives plain queries instead.
func (e *MilvusExecutor) scanAll(ctx context.Context, collection, expr string, outputFields []string, pk *entity.Field, sorts []translate.SortClause) ([]row, error) {
	var all []row
	for {
		step := expr
		if len(all) > 0 {
			last := all[len(all)-1][pk.Name]
			if last == nil {
				return nil, fmt.Errorf("cannot page past a null primary key in %q", collection)
			}
			// The cursor reuses Render so PK quoting/escaping has one home
			// (translate), not a second string builder here.
			cursor, err := translate.Render(translate.Compare{
				Op: translate.Gt, Field: pk.Name, Value: scalarValue(last),
			})
			if err != nil {
				return nil, err
			}
			step = joinAnd(expr, cursor)
		}

		batch, err := e.queryPage(ctx, collection, step, outputFields, pk.Name, 0, scanBatchSize)
		if err != nil {
			return nil, err
		}
		all = append(all, batch...)
		if len(batch) < scanBatchSize {
			return all, nil
		}
	}
}

// joinAnd merges the filter and the cursor into one expression.
func joinAnd(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	default:
		return "(" + a + ") and (" + b + ")"
	}
}

// count runs the count(*) aggregation, which Milvus only accepts without
// pagination (verified 2026-09-14).
func (e *MilvusExecutor) count(ctx context.Context, collection, expr string) (int64, error) {
	rs, err := e.queryOpt(ctx, collection, milvusclient.NewQueryOption(collection).
		WithFilter(expr).
		WithOutputFields("count(*)"))
	if err != nil {
		return 0, err
	}
	col := rs.GetColumn("count(*)")
	if col == nil || col.Len() == 0 {
		return 0, fmt.Errorf("count(*) for collection %q: no result", collection)
	}
	v, err := col.Get(0)
	if err != nil {
		return 0, fmt.Errorf("count(*) for collection %q: %w", collection, err)
	}
	n, ok := v.(int64)
	if !ok {
		return 0, fmt.Errorf("count(*) for collection %q: unexpected result type %T", collection, v)
	}
	return n, nil
}

// query runs one query call, retrying once behind an auto-load: ES clients
// expect every index to be searchable, but Milvus refuses queries against
// unloaded collections, so a "not loaded" failure triggers LoadCollection
// (which blocks until progress reaches 100%) and one retry.
// queryOpt runs one query call; collection is passed separately because the
// option type keeps its name private.
func (e *MilvusExecutor) queryOpt(ctx context.Context, collection string, opt milvusclient.QueryOption) (milvusclient.ResultSet, error) {
	rs, err := e.cli.Query(ctx, opt)
	if err == nil {
		return rs, nil
	}
	if !strings.Contains(err.Error(), "not loaded") {
		return milvusclient.ResultSet{}, fmt.Errorf("query collection %q: %w", collection, err)
	}
	if err := e.loadAndWait(ctx, collection); err != nil {
		return milvusclient.ResultSet{}, fmt.Errorf("auto-load collection %q: %w", collection, err)
	}
	rs, err = e.cli.Query(ctx, opt)
	if err != nil {
		return milvusclient.ResultSet{}, fmt.Errorf("query collection %q after auto-load: %w", collection, err)
	}
	return rs, nil
}

// searchOpt runs one search call, retrying once behind an auto-load — the
// search twin of queryOpt: Milvus refuses searches against unloaded
// collections, ES clients expect every index searchable.
func (e *MilvusExecutor) searchOpt(ctx context.Context, collection string, opt milvusclient.SearchOption) ([]milvusclient.ResultSet, error) {
	rss, err := e.cli.Search(ctx, opt)
	if err == nil {
		return rss, nil
	}
	if !strings.Contains(err.Error(), "not loaded") {
		return nil, fmt.Errorf("search collection %q: %w", collection, err)
	}
	if err := e.loadAndWait(ctx, collection); err != nil {
		return nil, fmt.Errorf("auto-load collection %q: %w", collection, err)
	}
	rss, err = e.cli.Search(ctx, opt)
	if err != nil {
		return nil, fmt.Errorf("search collection %q after auto-load: %w", collection, err)
	}
	return rss, nil
}

// loadAndWait loads a collection and blocks until it is queryable.
func (e *MilvusExecutor) loadAndWait(ctx context.Context, collection string) error {
	task, err := e.cli.LoadCollection(ctx, milvusclient.NewLoadCollectionOption(collection))
	if err != nil {
		return err
	}
	return task.Await(ctx)
}

// describe fetches the collection schema, mapping a missing collection to
// ErrCollectionNotFound so protocol shells can render native 404s.
func (e *MilvusExecutor) describe(ctx context.Context, collection string) (*entity.Collection, error) {
	coll, err := e.cli.DescribeCollection(ctx, milvusclient.NewDescribeCollectionOption(collection))
	if err != nil {
		if strings.Contains(err.Error(), "can't find collection") || strings.Contains(err.Error(), "not found") {
			return nil, fmt.Errorf("%w: %s", ErrCollectionNotFound, collection)
		}
		return nil, fmt.Errorf("describe collection %q: %w", collection, err)
	}
	return coll, nil
}

// ErrCollectionNotFound marks a search against an absent collection.
var ErrCollectionNotFound = errors.New("collection not found")

// translateFieldType maps a Milvus field onto the translator's type
// vocabulary. Milvus has no date type (dates are stored as Int64 epoch
// millis per docs/design.md), so TypeDate never comes from here — real
// ES-side date typing needs the Phase 3 schema catalog.
func translateFieldType(f *entity.Field) translate.FieldType {
	switch f.DataType {
	case entity.FieldTypeVarChar:
		if f.TypeParams["enable_analyzer"] == "true" {
			return translate.TypeText // BM25-analyzed text (match → TEXT_MATCH)
		}
		return translate.TypeKeyword
	case entity.FieldTypeInt8, entity.FieldTypeInt16, entity.FieldTypeInt32,
		entity.FieldTypeInt64, entity.FieldTypeFloat, entity.FieldTypeDouble:
		return translate.TypeNumber
	case entity.FieldTypeBool:
		return translate.TypeBool
	case entity.FieldTypeFloatVector:
		return translate.TypeVector // fp32: the only vector family searches send (SearchSpec.Vector is []float32)
	default:
		// JSON, Array, and non-fp32 vector families (fp16/bf16, binary,
		// sparse): no scalar vocabulary fits, and a search targeting them
		// must fail at translation time (unknown field type) instead of
		// mismatching the server-side storage type. Translators treat
		// unknown fields permissively; Milvus rejects nonsense filters.
		return translate.TypeUnknown
	}
}

// projection is the two-sided field list a search needs: fetch goes to
// Milvus output_fields, source names what lands in Hit.Source (the future
// ES _source). Fetch is always a superset of source — sort keys must come
// back even when they are not part of the projection.
type projection struct {
	fetch  []string
	source []string
}

// functionOutputFields lists fields generated by schema functions (e.g. the
// sparse vector a BM25 function produces). Milvus refuses to return their raw
// data, so they never join the default "every field" projection.
func functionOutputFields(schema *entity.Schema) []string {
	var out []string
	for _, fn := range schema.Functions {
		out = append(out, fn.OutputFieldNames...)
	}
	return out
}

// buildProjection resolves the plan's SourceFilter against the collection
// schema. Includes that name unknown fields are dropped silently, the way ES
// returns an empty _source rather than an error; patterns are limited to the
// exact / "prefix.*" forms the translator already accepts.
func buildProjection(fields []*entity.Field, funcOutputs []string, src translate.SourceFilter, sorts []translate.SortClause) (projection, error) {
	names := make([]string, 0, len(fields))
	sortable := make(map[string]bool, len(fields))
	for _, f := range fields {
		if isFunctionOutput(f.Name, funcOutputs) {
			continue // not part of any default projection
		}
		names = append(names, f.Name)
		sortable[f.Name] = isSortableType(f)
	}

	source, err := resolveSourceFields(names, src)
	if err != nil {
		return projection{}, err
	}

	fetch := append([]string{}, source...)
	for _, c := range sorts {
		if !sortable[c.Field] {
			return projection{}, fmt.Errorf("cannot sort on field [%s]: not a sortable scalar field of this collection", c.Field)
		}
		fetch = appendUnique(fetch, c.Field)
	}
	fetch = appendUnique(fetch, primaryKey(fields).Name)

	return projection{fetch: fetch, source: source}, nil
}

// resolveSourceFields applies FetchSource plus include/exclude patterns to
// the schema's field names; nil means no source at all (_source: false).
func resolveSourceFields(names []string, src translate.SourceFilter) ([]string, error) {
	if !src.FetchSource {
		return nil, nil
	}
	var keep []string
	if len(src.Includes) == 0 {
		keep = append(keep, names...) // default: every field
	} else {
		for _, p := range src.Includes {
			keep = append(keep, matchPattern(names, p)...)
		}
	}

	kept := make([]string, 0, len(keep))
excl:
	for _, name := range keep {
		for _, p := range src.Excludes {
			if len(matchPattern([]string{name}, p)) > 0 {
				continue excl
			}
		}
		kept = appendUnique(kept, name)
	}
	return kept, nil
}

// matchPattern matches one field name against an include/exclude pattern:
// exact name, or "prefix.*" matching everything under that prefix.
func matchPattern(names []string, pattern string) []string {
	switch {
	case strings.HasSuffix(pattern, ".*"):
		prefix := strings.TrimSuffix(pattern, ".*")
		var out []string
		for _, n := range names {
			if strings.HasPrefix(n, prefix+".") {
				out = append(out, n)
			}
		}
		return out
	case pattern == "*":
		return names
	default:
		for _, n := range names {
			if n == pattern {
				return []string{n}
			}
		}
		return nil
	}
}

func isFunctionOutput(name string, funcOutputs []string) bool {
	for _, f := range funcOutputs {
		if f == name {
			return true
		}
	}
	return false
}

func isSortableType(f *entity.Field) bool {
	switch f.DataType {
	case entity.FieldTypeInt8, entity.FieldTypeInt16, entity.FieldTypeInt32,
		entity.FieldTypeInt64, entity.FieldTypeFloat, entity.FieldTypeDouble,
		entity.FieldTypeBool, entity.FieldTypeVarChar, entity.FieldTypeString:
		return true
	default:
		return false
	}
}

func appendUnique(list []string, v string) []string {
	for _, s := range list {
		if s == v {
			return list
		}
	}
	return append(list, v)
}

func primaryKey(fields []*entity.Field) *entity.Field {
	for _, f := range fields {
		if f.PrimaryKey {
			return f
		}
	}
	return nil
}

// rowsOf flattens Milvus's column-major result set into row maps.
func rowsOf(rs milvusclient.ResultSet, outputFields []string) []row {
	if rs.Len() == 0 {
		return nil
	}
	cols := make(map[string]column.Column, len(rs.Fields))
	for _, col := range rs.Fields {
		cols[col.Name()] = col
	}
	rows := make([]row, rs.Len())
	for i := range rows {
		r := row{}
		for _, name := range outputFields {
			col, ok := cols[name]
			if !ok {
				continue // count(*) and similar computed columns
			}
			v, err := col.Get(i)
			if err != nil {
				v = nil // treat unreadable cell as missing
			} else if isNull(col, i) {
				v = nil // nullable column, null cell → JSON null, not zero value
			}
			r[name] = v
		}
		rows[i] = r
	}
	return rows
}

// searchRows flattens a search ResultSet into row maps. Output fields come
// from rs.Fields; the primary key rides rs.IDs (Milvus returns it separately
// from the output projection) and fills the gap where the projection skipped
// it.
func searchRows(rs milvusclient.ResultSet, outputFields []string, pkField string) []row {
	rows := rowsOf(rs, outputFields)
	if rs.IDs == nil {
		return rows
	}
	for i, r := range rows {
		if _, ok := r[pkField]; ok {
			continue
		}
		if v, err := rs.IDs.Get(i); err == nil {
			r[pkField] = v
		}
	}
	return rows
}

// nullableColumn is the optional null introspection columns carry; Get
// returns the zero value for a null cell, so nullness needs IsNull.
type nullableColumn interface {
	IsNull(idx int) (bool, error)
}

func isNull(col column.Column, idx int) bool {
	nc, ok := col.(nullableColumn)
	if !ok {
		return false
	}
	isNull, err := nc.IsNull(idx)
	return err == nil && isNull
}

// hitID renders the primary key as the string an ES client sees as _id.
func hitID(r row, pk *entity.Field) (string, error) {
	v := r[pk.Name]
	if v == nil {
		return "", fmt.Errorf("primary key %q is missing from the fetched row", pk.Name)
	}
	switch x := v.(type) {
	case string:
		return x, nil
	case int64:
		return strconv.FormatInt(x, 10), nil
	default:
		return fmt.Sprintf("%v", x), nil
	}
}

// scalarValue re-wraps a fetched cell as a translate.Value so the PK cursor
// can ride Render's quoting and validation.
func scalarValue(v any) translate.Value {
	switch x := v.(type) {
	case string:
		return translate.StringValue(x)
	case int64:
		return translate.IntValue(x)
	case float64:
		return translate.FloatValue(x)
	case bool:
		return translate.BoolValue(x)
	default:
		return translate.StringValue(fmt.Sprintf("%v", x))
	}
}

// window applies ES from/size to fetched rows.
func window(rows []row, offset, limit int) []row {
	if limit <= 0 {
		return nil
	}
	if offset >= len(rows) {
		return nil
	}
	end := offset + limit
	if end > len(rows) {
		end = len(rows)
	}
	return rows[offset:end]
}

// compareRows orders two rows by the plan's sort clauses: first clause that
// differs wins; ties fall through to the next clause (and keep fetch order).
// Missing values sort last regardless of direction — the ES missing:_last
// default the translator assumes.
func compareRows(a, b row, sorts []translate.SortClause) int {
	for _, c := range sorts {
		av, bv := a[c.Field], b[c.Field]
		switch {
		case av == nil && bv == nil:
			continue
		case av == nil:
			return 1
		case bv == nil:
			return -1
		}
		cmp := compareScalar(av, bv)
		if c.Desc {
			cmp = -cmp
		}
		if cmp != 0 {
			return cmp
		}
	}
	return 0
}

// compareScalar orders two fetched cells by Go type. Columns are typed per
// field, so mixed types cannot occur in practice; the fallback keeps the
// comparison total anyway.
func compareScalar(a, b any) int {
	switch av := a.(type) {
	case string:
		if bv, ok := b.(string); ok {
			return strings.Compare(av, bv)
		}
	case int64:
		switch bv := b.(type) {
		case int64:
			switch {
			case av < bv:
				return -1
			case av > bv:
				return 1
			}
			return 0
		case float64:
			return compareFloat(float64(av), bv)
		}
	case float64:
		switch bv := b.(type) {
		case float64:
			return compareFloat(av, bv)
		case int64:
			return compareFloat(av, float64(bv))
		}
	case bool:
		if bv, ok := b.(bool); ok {
			switch {
			case !av && bv:
				return -1
			case av && !bv:
				return 1
			}
			return 0
		}
	}
	return strings.Compare(fmt.Sprintf("%v", a), fmt.Sprintf("%v", b))
}

func compareFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
