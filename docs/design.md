# Design Notes

> Scratchpad for decision points — strike through once settled. The ones that most need
> Milvus 3.x capability knowledge are flagged first.

## Decision Points

1. **PG entry: real wire protocol vs SQL-over-HTTP?**
   Leaning wire protocol, using `jeroenrinzema/psql-wire` (pure Go, Apache-2.0, covers
   handshake/auth/SSL/extended query/prepared stmt/COPY/cancel; used by Shopify) to avoid
   protocol details; the translation layer stays decoupled from the entry point, so adding
   SQL-over-HTTP later is just a new shell.
   Note: psql-wire only handles the protocol, no SQL parsing — the translation layer needs
   `pganalyze/pg_query_go` (libpg_query bindings, real PG grammar → AST).
2. **Where do non-vector queries land:** Milvus `query()` does pure scalar filtering, no
   vector needed. Most ES/PG queries are scalar filters — this sets the feasibility ceiling
   of the gateway; filter performance needs validating on real data volumes.
3. **Full-text search:** ES `match` → Milvus 2.5+ BM25 (sparse full-text) is the right
   answer; `LIKE` is only a fallback. Note BM25 requires declaring an analyzer at collection
   creation time, which affects Phase 3's DDL translation.
4. **ORDER BY / deep pagination:** Milvus 3.x support surface for scalar ORDER BY TBD;
   ES `search_after` / PG `LIMIT/OFFSET` map to the query iterator first.
5. **Aggregations / joins:** fail fast; error messages state the equivalent query or an
   explicit unsupported notice. No emulation for now.
6. **Schema catalog storage:** local JSON config / Milvus database properties / live
   describe — three candidates. Leaning "config-first + describe validation"; final call in
   Phase 3.

## Concept Mapping

| PostgreSQL   | Elasticsearch   | Milvus                     |
|--------------|-----------------|----------------------------|
| database     | —               | database                   |
| table        | index           | collection                 |
| row          | document        | entity                     |
| column       | field           | field                      |
| PRIMARY KEY  | `_id`           | PK field (Int64 / VarChar) |
| —            | `dense_vector`  | FloatVector / BinaryVector |
| text + tsvector | text (analyzed) | BM25 text field (2.5+)  |

## Type Mapping (draft)

- PG: `int2/4/8 → Int8/32/64`, `float4/8 → Float/Double`, `numeric → Double`, `bool → Bool`,
  `varchar/text → VarChar(maxLen)`, `json/jsonb → JSON`, `timestamptz → Int64(μs)`
- ES: `keyword → VarChar`, `long → Int64`, `integer → Int32`, `double/float → Double/Float`,
  `boolean → Bool`, `date → Int64(epoch ms)`, `dense_vector → FloatVector(dim from mapping)`,
  `text → BM25 text`

## Semantic Gaps (fail-fast list)

- join / correlated subquery / window function: error out directly
- `GROUP BY` / `count(*)` and other aggregations: decide after confirming Milvus 3.x capability
- ES `_score`: only BM25/vector queries carry real scores; filter-only queries return
  `max_score: 1`, `_score: 1`, keeping every language SDK's parsing path intact
- ES `_source` filtering / `fields`: project onto collection fields; needed in Phase 1
  already (clients widely depend on it)

## Protocol Notes

- ES 8.x clients probe `GET /` on startup and verify the `X-elastic-product: Elasticsearch`
  response header; missing it, they refuse to connect
- The ES error envelope `{"error": {"type", "reason"}, "status": N}` must follow the format —
  SDK error parsing depends on it
- Prior art: Quickwit / Zincsearch implemented ES `_search` subset compatibility — worth
  browsing which endpoints they support
- PG-side low-level fallback: `jackc/pgx` v5's pgproto3 subpackage (if psql-wire falls short)
- Outbound access goes through the official Go client from the milvus repo
  (`milvus-io/milvus/client/v2`), no hand-rolled gRPC

## Settled — Phase 1 translation slice (2026-09-11)

- Contract: `internal/translate` owns `Plan` / `Schema` / `Error`. Both translators emit
  `Plan`, store consumes it. `Error` carries ES-style `type`/`status` so endpoints render
  native envelopes without re-parsing reason strings.
- Plan.Expr is a Milvus boolean-expression **tree** (`translate.Expr`), not a string
  (2026-09-12): plan-level rewrites — Phase 3 field renames, Phase 5 BM25 scoring —
  transform nodes directly instead of reparsing text. `translate.Render` turns the tree
  into the executed string and is the shared dialect home for both translators; field-name
  validation (`[A-Za-z_][A-Za-z0-9_]*`) and literal quoting live there, closing the
  field-injection hole. The `1 != 1` constant-false guard is isolated in `Never`'s
  rendering. Values enter the tree already coerced to storage encoding; render is syntax
  only. Store must call `Render` right before execution.
- Error taxonomy: `parsing_exception` (malformed DSL), `illegal_argument_exception` (bad
  from/size, unparsable values), `unsupported_exception` (valid DSL beyond the Phase 1
  surface: aggs, knn, search_after, fuzziness, msm>1, date math, …) — all 400.
- `match` on analyzed text → `TEXT_MATCH` (OR across terms = ES default operator;
  `operator: and` → AND of per-term calls); `match`/`term` on keyword → `==`.
  `_score` stays a constant 1.0 until Phase 5 aligns BM25 scoring.
- `exists` → `is not null` — Milvus nullable-field syntax, re-verify when wiring the store.
- `sort` is parsed into `Plan.Sort`; the store sorts after fetch until Milvus server-side
  ORDER BY is confirmed (decision 4 above). `_score`/`_doc` sorts drop out (constant
  scores, no stable index order).
- Dates: RFC-3339 / `yyyy-MM-dd` strings → epoch millis at translation time; `||` date
  math fails fast.
- `_source` wildcards: exact names and `prefix.*` only; other patterns fail fast.
- from+size capped at 10000 like ES `max_result_window` — the store will fetch
  offset+size and sort client-side within that window.
- Known approximations (documented in code): match tokenization is whitespace-only until
  Phase 5; `minimum_should_match > 1` needs k-of-n matching, unsupported for now.

## Settled — store capability facts on the target cluster (2026-09-14)

Verified against the development Zilliz Cloud endpoint ("Compatible with
Milvus 2.6"): first probed on classic milvus-sdk-go v2.4.2, then
re-confirmed by the e2e suite now running on `client/v2` v2.6.5:

- **No query-side ORDER BY.** `order by` suffixes in the query expression are
  rejected by the plan parser, and neither client exposes an orderBy option,
  so sorts stay client-side: the executor streams every match through a
  primary-key cursor (`(filter) and pk > last`, 1000/batch), sorts, windows.
  Decision 4 flips only when a 3.x server is the target. The classic SDK's
  QueryIterator composes the same cursor but sends iterator parameters this
  server rejects (EOF) — lyrebird drives plain queries and relies on no
  SDK iterator.
- **count(*) works, but not with pagination** ("count entities with pagination
  is not allowed"), so totals are a separate no-paging query. It is skipped
  when the answer is already exact: full-scan (sort) paths and underfilled
  pages (`offset + len` is the total).
- **limit 0 is rejected server-side**, so ES `size: 0` becomes a count-only
  search (no page fetch at all).
- Empty expression = match-all, with offset/limit, works. `is null` /
  `is not null` work on nullable and non-nullable fields alike — verified
  against a fixture with a nullable column (null side included), so the
  `exists` mapping holds both ways.
- Query results always include the primary key column, requested or not.
- Offset past the total returns zero rows (ES-compatible), not an error.
- **SDK: `milvus-io/milvus/client/v2` v2.6.5** (settled 2026-09-15). Started
  on classic milvus-sdk-go v2.4.2 for its lean dependency tree; switched at
  the user's call once it mattered more to track the live SDK — the server
  monorepo arrives as indirect deps (etcd, raft, k8s, otel, ~120 modules),
  accepted. Gains exercised for real: nullable-field creation and
  description (exists e2e now covers the null side), BM25 Function schema
  (match → TEXT_MATCH is e2e-verified). Two DDL facts surfaced: TEXT_MATCH
  needs `enable_analyzer` AND `enable_match` on the text field, and Milvus
  refuses raw retrieval of function outputs (sparse), so those fields are
  excluded from the default projection. The SDK stays behind Executor —
  only internal/store imports it.
- The plan contract's `Source.FetchSource` zero value means "no source";
  translators must default it to true (parseSource does). Documented on the
  Plan to keep the pg translator honest.

## Settled — pg wire entry point (2026-09-16)

Facts locked in while wiring `internal/pgserver` (psql + pgx verified against
the UAT cluster):

- **RowDescription precedes the rows**, so `SELECT *` cannot wait for the
  store: `Executor.Schema` feeds `Select.Columns` before execution
  (store-side: the described schema keeps storage order via `Fields()`).
- All numeric fields render as **float8** on the wire — the one numeric OID
  that covers Int and Float storage alike; cells narrow to float64.
- **translate.Error.Type carries the SQLSTATE verbatim**; the wire shell maps
  classified failures with no table beyond `wireError`. Missing collection →
  native 42P01. Server-side execution errors (e.g. metric mismatch) stay
  XX000 (internal) — classifying Milvus message text is a later concern.
- **`standard_conforming_strings=on` must be broadcast** in ParameterStatus:
  pgx refuses simple-protocol queries without it. psql-wire sends no
  parameters by default; lyrebird seeds server_version/encoding/DateStyle
  too.
- **pgx shares one connection across queries: every `Rows` must be Closed**
  before the next statement, else the conn stays busy (e2e does this).
- Identifiers keep their written case (no lower folding — ES-created
  mixed-case fields must stay reachable); quote to escape keywords.
- Dates stay epoch millis on both fronts; PG-native micros is a Phase 3
  catalog decision.
- LIMIT is mandatory on every SELECT: the alternative is streaming the
  whole collection through the client-side sort path.

## Settled — pg vector search: the pgvector distance path (2026-09-18)

`Plan.Search *SearchSpec` (field + `[]float32` + metric) is the vector
carrier; store routes non-nil plans through Milvus `search()` instead of
`query()`.

- **Form**: only `ORDER BY vec <op> '[0.1, …]' LIMIT n` — the canonical
  pgvector query. The distance term must stand alone (it IS the sort), ASC
  only (DESC = farthest-first has no ANN answer). Scalar WHERE rides along
  as the search filter; OFFSET maps to the search offset.
- **Operator → metric** (the pgvector compat surface):
  `<->`→L2, `<=>`→COSINE, `<#>`→IP (the negation is a sort-direction trick;
  nearest-first is the contract either way), `<+>` rejected (no L1 metric).
- **Metric validation rides the search**: the plan's metric goes to the
  server via the `metric_type` search param; Milvus rejects a mismatch with
  the field's index ("metric type not match: expected=L2 actual=COSINE",
  verified on UAT 2026-09-18). pgvector operators have fixed semantics — a
  cosine question must never come back answered by L2 distances, so silent
  index-default behavior is off the table. Known rough edge: that server
  rejection surfaces as SQLSTATE XX000, not 22023.
- **Scores don't cross the store boundary yet**: pgvector only surfaces
  distances when projected, which the SELECT subset rejects anyway; ES
  `_score` parity is Phase 5. `SearchResult.Total` on this path is the
  returned row count — count(*) over the filter answers a different
  question.
- Dimension validation is the server's (it names the expected dim);
  `TypeVector` covers fp32 FloatVector only since the ES knn work (2026-09-19
  note below) — fp16/bf16/binary/sparse stay unknown.
- Fixtures: `lyrebird_e2e_store` emb is L2 (nearest-first + mismatch
  coverage); `lyrebird_e2e_vec` is COSINE with strictly-decreasing
  similarity so order asserts are tie-free.

## Settled — ES knn: the top-level knn clause (2026-09-19)

The ES twin of the pgvector path (2026-09-18 note): the top-level `knn`
object lowers into the same `Plan.Search *SearchSpec`, and esserver keeps
emitting the plain search envelope — Plan stays the only contract, the
envelope assembly is untouched.

- **Form**: object only (one knn clause). The array form (ES 8.9's multiple
  knn sub-searches fused by score) is `unsupported_exception` — one
  SearchSpec cannot carry it. `knn` next to `query` is also unsupported
  (score fusion is Phase 5), and so is `knn` next to `sort` (the ordering is
  the ANN nearest-first order; the store rejects the combination anyway, and
  a valid-but-unhonorable request must be a 400, not the store's 500).
- **Members**: `field` + `query_vector` (required) → SearchSpec;
  `k` → `plan.Limit`, defaulting to the top-level `size`, itself defaulting
  to 10 (ES's own default chain); top-level `from` → `plan.Offset` via the
  existing paging path; `filter` (single clause
  or array = AND) lowers through the same query IR → fold → buildExpr
  machinery into `plan.Expr` as the scalar pre-filter (a filter that folds to
  match_none makes the whole plan NoMatch — nothing for ANN to return);
  `num_candidates` accepted but ignored (Milvus tunes its own ANN width; the
  member only affects recall quality, not semantics); `boost`/`_name`
  silently ignored (project convention); `similarity` (score floor) is
  `unsupported_exception` — it needs Milvus range search, v1 does not.
  Known approximation: ES windows from/size over the k results; here an
  explicit `k` replaces `size` as the window (the store ANN-fetches
  offset+k rows, not offset+size of a top-k), so `size` only seeds k's
  default — pinned by an e2e case.
- **Error split** follows the package taxonomy: shape errors
  (query_vector not an array, missing required members, unknown parameters)
  → `parsing_exception`; bad values (non-numeric vector elements, empty
  vector, `k` below 1 or non-integer) → `illegal_argument_exception`;
  capability gaps → `unsupported_exception`; all 400.
- **Metric stays empty**: ES semantics take the distance from the
  `dense_vector` mapping's `similarity` parameter, never from the query, so
  the search rides the index metric (the empty-Metric store contract — no
  `metric_type` search param; that is the pgvector operator's
  point-at-the-metric semantics).
- **Vector kind fp32-only**: `translateFieldType` now maps only FloatVector
  → `TypeVector`; fp16/bf16/binary/sparse fall back to `TypeUnknown`, so a
  knn targeting them fails with a clean local translation error instead of a
  server-side type mismatch (the store only ever sends `[]float32`).
- **Known approximations**: `_score` stays the constant 1.0 — ES's real knn
  scores need the metric-specific normalization formulas
  (cosine / l2_norm / dot_product), a Phase 5 job to be written against the
  ES docs, not from memory. `hits.total.value` is the returned row count with
  relation `eq` — a knn hit count has no cheap truth (same as the pg side's
  Total=topk), so the number is honest about what came back, not about the
  collection. The query-level `knn` (inside `query`) stays rejected.

## Settled — catalog introspection surface (2026-09-19)

Goal: DataGrip and friends browse the cluster (database → table → column)
over both protocols. Evidence chain: pgjdbc's PgDatabaseMetaData.java and
the DataGrip ES REST plugin's source were read for the exact query set
before any design; the fixtures in internal/catalog/pgjdbc_test.go are
those queries' real text.

- **Shape**: new `internal/catalog` — virtual pg_catalog /
  information_schema tables projected live from a Provider
  (ListCollections / Collection / Databases; no cache: one metadata query
  costs one roundtrip, catalog queries are rare). Milvus database → PG
  database (user decision 2026-09-19: not a single-db alias tree);
  collection → table (relkind 'r', one synthetic OID from 16384); field →
  column. One schema per database: `public` (+ the pg_catalog /
  information_schema / pg_toast rows real PG has — getMaxNameLength joins
  on nspname='pg_catalog').
- **Engine**: a dedicated small evaluator, not the translate/pg pipeline.
  Catalog clients need joins, CASE, LIKE/regex, ORDER BY over quoted
  output aliases, bind parameters and a function whitelist — none of which
  the data path's SELECT subset wants. The engine parses only
  FROM-catalog-table statements; pgserver routes by registry lookup, so a
  collection query can never land here (and catalog queries keep their own
  SQLSTATEs: 42601/42703/42883/42P01). No mandatory LIMIT here — catalog
  row sets are the cluster's own metadata.
- **Named special cases**: shapes beyond the engine (derived tables,
  `row_number() OVER`, `information_schema._pg_expandarray`,
  generate_series, correlated subqueries) are per-method generators keyed
  on verbatim SQL landmarks from PgDatabaseMetaData.java — getColumns,
  getPrimaryKeys (the pg_constraint path), getPrimaryUniqueKeys,
  getBestRowIdentifier, getImportedExportedKeys (always empty: no FKs),
  getTypeInfo, getUDTs (empty), getIndexInfo, getFunctionColumns (empty).
  Their `?` parameters are located by the filter each feeds — pgjdbc sends
  JDBC markers that arrive as `$n` after client-side substitution — with
  case-insensitive aliases because the outer queries select through
  derived-table aliases (TABLE_NAME vs relname).
- **Session surface**: SET / RESET / DISCARD ack (values accepted, not
  applied — there is no session state to change), BEGIN/COMMIT/ROLLBACK
  ack (no transaction state; pgjdbc opens them under autocommit=off),
  SHOW from the same settings table pg_settings projects (incl. PG's
  two-word forms: `SHOW TIME ZONE`), scalar SELECTs (version(),
  current_database(), set_config('search_path',…), SELECT 1) answered by
  the engine's FROM-less path.
- **Per-connection database**: the startup `database` parameter names the
  Milvus database; `""` and `postgres` alias onto --milvus-db (every PG
  client's URL default is postgres). One pooled milvus client per database
  — SDK UseDatabase mutates the shared client, so per-call switching would
  race across connections. Catalog data AND collection reads follow the
  connection's database.
- **Type naming**: catalog type names/OIDs and RowDescription share one
  table (catalog.FieldTypeOID): numbers float8, strings text, JSON/array/
  vector json — the settled pg-wire decision, so a column never describes
  itself as one type and delivers another. int8/timestamp refinement
  would re-type the wire too — left as a later decision. FieldMeta carries
  the native Milvus type name so the ES mapping can say long vs float.
- **ES surface** (the plugin's five probes): `_cat/indices`
  (+`/{index}`, format=json/text, h= subset; docs.count from
  GetCollectionStats, -1 on failure like ES's unknown marker),
  `_cat/aliases` (empty), `_data_stream` (empty), `/{index}/_mapping`
  (batched comma lists skip missing names; a single unknown name is a
  native 404 index_not_found_exception; dense_vector carries dim; the
  mapping type comes from the native Milvus type — long/float/double/
  boolean/keyword/text/dense_vector/object), `_cluster/health` (+`/{index}`).
- **DataGrip caveat**: the JDBC metadata introspection level is the pinned
  target (public query set). DataGrip's Queried/Raw levels send its own
  PRM queries — broader, undocumented, versioned — and are best-effort:
  new shapes land as engine additions or named cases after capture.
  Switching levels is a per-schema right-click.
- **psql boundary**: `\l` and `\dt` are covered end-to-end (verified
  against psql 16's real query sequence), which forced the engine's last
  round of features: multi-statement splitting (psql sends its describe
  family as one string; psql-wire does not split), `E''` escape strings,
  ORDER BY output position, `COLLATE` no-ops, `OPERATOR(pg_catalog.~)`,
  `= ANY(…)`, `ARRAY(SELECT …)`, scalar correlated subqueries with a
  parent-context fallback, UNION branches, and the always-empty system
  tables psql joins (pg_policy, pg_trigger, pg_statistic_ext, pg_collation,
  …). Per-column describe (`\d`/`\d+`) additionally needs correlated
  table-functions (FROM generate_series/unnest) plus aggregates
  (string_agg) — not built; those statements fail with the engine's own
  syntax error rather than a wrong answer. DataGrip never sends them.
- **Not done here**: aliases (ES), schemas beyond public, description
  columns (all null), privilege emulation (has_*_privilege always true),
  multi-statement batches (psql-wire does not split). Decision 6 (external
  name → collection mapping config) stays open — Phase 3b territory, this
  work is pure describe-based discovery.

## Settled — IR placement: query-level IR stays frontend-private (2026-09-13)

- Layering, top to bottom: **query-level IR is per-frontend** (es: the hand-rolled
  8-node tree in translate/es; pg: the pg_query_go AST when Phase 2 lands) → **the
  shared Rex-style expression layer is `translate.Expr`** (Compare/InList/TextMatch/
  NotNull/Not/And/Or/Never — see the 2026-09-12 note above) → **`translate.Render`**
  emits the dialect string → Milvus lifts it server-side into a typed planpb.Expr tree
  (pkg/proto/plan.proto). The "shared IR" role is effectively outsourced to Milvus.
- No shared *query*-level IR, deliberately: with a single backend the N+M-vs-N×M
  argument pays nothing, and the two languages' semantics don't unite cleanly (match
  type-dispatch vs SQL `=`, LIKE vs prefix, expression projections vs `_source`
  patterns; msm / date-math / fuzziness have no SQL counterpart, CASE / arithmetic no
  ES counterpart). Forcing a union yields a lowest-common-denominator IR full of escape
  hatches — two IRs in a trenchcoat.
- What IS shared and why: the expression tree + Render (dialect cannot drift; field
  validation has one home), and the store-facing `Plan` contract (store would otherwise
  be duplicated per protocol). Query IR stays private because each language's parse
  quirks (ES clause dual-forms, msm defaults; SQL grammar) are contained there.
- Further-unification triggers — any ONE of these justifies merging the query level
  too (or growing the expression layer):
  1. a second backend appears (an in-memory test executor counts);
  2. structured plan rewrites beyond expression scope (e.g. pushdown + residual split
     needs sub-query trees, not just predicate trees);
  3. plan-level transforms start needing language-neutral query shape (sort/source
     rewrites across both frontends).

## Settled — write path: strict-schema inserts and updates (2026-09-19)

Goal: INSERT/UPDATE on the pg entry, the index/update APIs on the ES entry,
against existing collections only — no DDL, no dynamic fields. The user's
call for this slice: **strict** — an input that does not match the live
collection schema is rejected, never coerced, never auto-mapped. This
settles the 2026-09-18 proposal's value-conversion question (no lossy
widening; exactness or refusal) and its dynamic-mapping question (refused;
DDL remains a separate later slice).

- **Shape**: the write path is a parallel narrow lane, not a Plan — write
  rows, not read plans. Each frontend parses its own statements
  (translate/pg: Insert/Update ASTs; translate/es: document decoding)
  into `translate.WriteRow` (field → value), and store executes
  `Executor.Insert` / `Executor.Upsert` after validating against
  `Executor.Describe` (the catalog.CollectionMeta the introspection
  surface already serves — writes are checked against exactly what
  clients are told the schema is).
- **Write value vocabulary** (translate.WriteRow): string; numbers as
  json.Number (the digits as written — Int64 keeps full 64-bit precision,
  no float round-trip); bool; nil (explicit NULL); []float32 (the fp32
  vector family); json.RawMessage/[]byte for JSON fields. Reads return
  the storage encodings (int64/float64/...), so validation also accepts
  them — a read-modify-write can upsert exactly what it fetched.
- **Strict rejection classes** (store.WriteError, one classification both
  protocols render natively): unknown field (ES strict_dynamic_mapping
  / pg 42703), function-owned field (BM25 outputs — the store computes
  them), auto-id key supplied, missing non-nullable field (23502), NULL
  into non-nullable (23502), type mismatch (document_parsing / 42804),
  numeric range (22003), string > max_length (22001), vector ≠ dim
  (illegal_argument / 22023), storage type with no write encoding —
  arrays, fp16/bf16/binary/int8/sparse vectors, timestamptz — refused
  with the type named (0A000). Nullable-but-unwritable fields ride as
  omitted columns instead of blocking writes on collections that have
  them.
- **ES surface**: PUT/POST `/{index}/_doc[/{id}]` (index API = whole-row
  put: insert when the key is free → 201 created, upsert when taken →
  200 updated; `?op_type=create` and `/{index}/_create/{id}` → 409
  version_conflict on collision), `/{index}/_update/{id}` (read-merge-
  write; ES's detect_noop default is honored — an unchanged doc answers
  result=noop and writes nothing; a missing doc is 404
  document_missing_exception; script/scripted_upsert/doc_as_upsert
  refused by name). `_bulk` refuses by name (ndjson envelope is its own
  later slice). `_id` semantics: VarChar pk — the URL id is the key, a
  pk value inside the document must agree; generated ids (20 url-safe
  chars, ES's shape) when no URL id; Int64 pk without auto-id — the URL
  id parses to the key (an ES-shaped generated id is not a number →
  refused); auto-id pk — no id may be supplied, POST without one, the
  store-assigned key is reported as _id. `_version` is always 1 with
  neutral _seq_no/_primary_term (the store has no row versioning);
  version/if_seq_no/if_primary_term parameters refuse by name (silence
  would promise concurrency control lyrebird does not provide).
- **PG surface**: INSERT INTO t [(cols)] VALUES (…)[, (…)] — the
  positional default fills every writable field in storage order
  (function outputs are not writable and never counted); multi-row is
  one statement, one Milvus insert; ON CONFLICT / INSERT…SELECT /
  DEFAULT / RETURNING refuse by name; bind parameters stay unwired
  (inline the literals). UPDATE t SET col = literal, … WHERE … — WHERE
  is mandatory (a bare UPDATE would rewrite the collection, refused the
  way a read without LIMIT is); SET holds literals only (self-referencing
  expressions are beyond the lane); qualifiers strip against the table
  name. Execution is read-modify-write: fetch every match (whole rows,
  capped at the same 10000 window reads sort in), apply SET, upsert each
  back, `UPDATE n` completes. `INSERT 0 n` keeps PG's tag shape.
- **pk/autoID policy**: this settles the 2026-09-18 proposal's key
  question for the current surface — pk is client-owned unless the
  schema says AutoID (then it is store-owned, always); ES _id and the
  pk column are one and the same value, cross-checked when both are
  given (a disagreement is a 400, not a silent pick). Server-side
  defaults and Function-owned column defaults stay open questions for
  the DDL slice.
- **Noop/equality**: _update's noop check compares values across the two
  encodings (stored int64/float64 vs written json.Number) numerically,
  and JSON fields as parsed values — re-sending byte-different but
  semantically identical JSON is still a noop.
- **Consistency, precisely**: all store reads (query, count, search) now
  request Strong explicitly — Milvus's default is Bounded (≤2s visibility),
  which would let an ES/PG client's read-your-writes fail silently. Strong
  narrows the window to sub-second; growing-segment propagation is still a
  platform fact, so the e2e read-backs poll (≤10s) instead of assuming.
  Observed while testing (v3.0.2-compatible server): a search whose metric
  disagrees with the field's index (`<=>` cosine over an L2 index) comes
  back **empty** rather than erroring — the read path's "server rejects a
  disagreeing metric" assumption no longer holds on this server; a local
  metric-vs-index check is a candidate follow-up.
- **Not done here**: DELETE (doc or by_query), _bulk, _update_by_query,
  scripts, DDL (create index/mapping), array/sparse write encodings,
  upsert-by-ES-of-partial-doc (the ES index API stays whole-row put, as
  ES defines it).
