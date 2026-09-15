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
