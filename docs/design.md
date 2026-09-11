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
- Outbound access goes through `milvus-sdk-go`, no hand-rolled gRPC

## Settled — Phase 1 translation slice (2026-09-11)

- Contract: `internal/translate` owns `Plan` / `Schema` / `Error`. Both translators emit
  `Plan`, store consumes it. `Error` carries ES-style `type`/`status` so endpoints render
  native envelopes without re-parsing reason strings.
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
