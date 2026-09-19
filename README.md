# lyrebird

A Milvus protocol-translation gateway: let PostgreSQL and Elasticsearch clients
query Milvus **without changing a single line of code**.

The lyrebird is nature's best mimic — it reproduces other birds' calls, and even
chainsaws and camera shutters. lyrebird does the same: it impersonates pg / es at
the protocol layer and translates whatever queries it hears into Milvus-speak.

## Goals

One Milvus cluster, three entry points:

| Entry point  | Protocol              | Target clients            |
|--------------|-----------------------|---------------------------|
| PostgreSQL   | wire protocol + SQL   | psql, JDBC, SQLAlchemy    |
| Elasticsearch| REST + Query DSL      | curl, ES SDKs             |
| Milvus       | native gRPC/SDK       | no gateway, direct access |

```
psql ─── SQL ───┐                 es sdk ─── DSL ───┐
               ▼                                    ▼
         pg.Parse(sql)                 es.Translate(body, schema)
               │                                    │
        Select (pg IR)                      queryNode (es IR)
               │                                    │
             fold                                 fold
               │                                    │
      Select.Plan(schema)                   buildExpr(schema)
               │                                    │
               └──────────────────┬─────────────────┘
                                  ▼
                           translate.Plan
                                  │
               store.Executor.Search(collection, plan)
                                  │
                               Milvus
```

Each frontend parses into its own private IR (pg `Select`, es `queryNode`),
constant-folds it, then lowers schema-aware into the one shared
`translate.Plan` — a plain data structure (filter Expr tree, sort, paging,
`_source`) that deliberately carries no collection name. Schema comes from
`store.Executor.Schema` before lowering (URL index for ES, `Select.Table`
for PG); the store executes plan + collection against Milvus.

## Architecture

```
cmd/gateway              entry point: start listener(s)
internal/pgserver        PG wire protocol server (Phase 2)
internal/esserver        ES-compatible REST server (Phase 1)
internal/catalog         pg_catalog / information_schema projection + its
                         query engine (Phase 3a)
internal/translate
    ├─ pg/               SQL → Select/Insert/Update (IR) → fold → Plan / write rows
    └─ es/               ES Query DSL → queryNode (IR) → fold → Plan; documents → write rows
internal/store           Milvus Executor: Search(collection, plan) +
                         strict-schema Insert/Upsert(collection, rows)
```

Principle: **the translation layer is pure functions** — parse → private IR →
fold → `translate.Plan`, no IO, everything covered by unit tests. The protocol
layer only handles encoding/decoding; the store layer only executes. Queries beyond the supported
capability set (joins, aggregations) fail fast with an error — no degraded emulation.

## Roadmap

- [x] **Phase 0** Skeleton: ES entry answers the `GET /` handshake (with the
  `X-elastic-product` header), `_search` returns 501
- [x] **Phase 1** ES read-only subset: `_search` + bool/term/terms/match/range/exists +
  sort/size/from + `_source` projection
  - [x] translation layer (`internal/translate/es`: DSL → `translate.Plan`, pure
    functions + unit tests)
  - [x] store wiring: Milvus client executes the plan (`internal/store`), `_search`
    endpoint live (`internal/esserver`)
  - [x] ES knn vector search: the top-level `knn` clause runs the Milvus ANN
    path (metric follows the field's index mapping, `k`/`from`/`size` window
    it, scalar `filter` clauses ride along; array form and score fusion with
    `query` fail fast)
- [x] **Phase 2** PG read-only subset: wire protocol up, psql connects,
  `SELECT .. WHERE .. LIMIT` translation
  - [x] translation layer (`internal/translate/pg`: SQL → `Select` →
    `translate.Plan`, pure functions + unit tests)
  - [x] wiring: PG wire protocol server (`internal/pgserver`, on
    jeroenrinzema/psql-wire) executes plans through `internal/store`;
    translate errors render as native SQLSTATEs, LIMIT stays mandatory
  - [x] pgvector distance search: `ORDER BY emb <=> '[0.1, …]' LIMIT n` runs
    the Milvus ANN path (`<->` L2, `<=>` cosine, `<#>` inner product; the
    metric must agree with the field's index), scalar WHERE filters ride along
- [x] **Phase 3a** Catalog introspection: GUI clients (DataGrip, psql's `\d`
  family) browse the cluster through a live pg_catalog / information_schema
  projection
  - [x] `internal/catalog`: virtual catalog tables generated from a
    Provider (Milvus database → PG database, collection → table, field →
    column) plus a small dedicated engine for the query shapes catalog
    clients use — joins, CASE, LIKE/regex filters, ORDER BY over output
    aliases, bind parameters, a scalar-function whitelist; pgjdbc shapes
    that outgrow it (derived tables, `row_number() OVER`,
    `_pg_expandarray`, …) run as named special cases pinned by real-SQL
    fixtures from PgDatabaseMetaData.java
  - [x] `internal/pgserver` dispatches per statement: session acks
    (SET/SHOW/RESET/DISCARD/BEGIN/COMMIT/ROLLBACK), catalog SELECTs,
    collection reads (unchanged pipeline); the startup database selects the
    Milvus database ("postgres" aliases onto the configured default)
  - [x] `internal/esserver` catalog surface: `_cat/indices`,
    `_cat/aliases`, `_data_stream`, `/{index}/_mapping`,
    `_cluster/health` — the probe set the DataGrip ES REST plugin makes on
    connect
- [ ] **Phase 3b** Schema catalog config: external-name mapping (config-first
  + describe validation, design.md decision 6); DDL (CREATE TABLE →
  CreateCollection)
- [x] **Phase 4a** Write path (strict): inserts and partial updates over both
  protocols, against existing collections
  - [x] `internal/store` write lane: `Executor.Insert` / `Executor.Upsert`
    validate every row against the live schema before anything is sent —
    unknown fields, type mismatches, range/length/dimension overruns, NULLs
    into non-nullable fields, auto-id keys and function-owned fields are all
    rejected, never coerced, never dynamically mapped (one rejection
    classification, rendered as SQLSTATEs or ES exception types per protocol)
  - [x] ES: PUT/POST `/{index}/_doc[/{id}]` (insert 201 / upsert 200),
    `/{index}/_create/{id}` (409 on collision), `/{index}/_update/{id}`
    (read-merge-write, detect_noop, 404 on missing); ES `_id` = the primary
    key, auto-id collections get store-assigned ids; `_bulk` and scripts
    refuse by name
  - [x] PG: `INSERT INTO t [(cols)] VALUES …` (multi-row, positional
    default in storage order) and `UPDATE t SET … WHERE …` (mandatory WHERE,
    literals only, read-modify-write upsert capped at the read window);
    `INSERT 0 n` / `UPDATE n` command tags
- [ ] **Phase 4b** Write path (rest): DELETE, `_bulk`, `_update_by_query`,
  DDL (CREATE TABLE → CreateCollection)
- [ ] **Phase 5** Full-text alignment: ES match → Milvus BM25, score semantics aligned

See [docs/design.md](docs/design.md) for design details and decision points.

## Non-goals

- No general-purpose OLTP: joins / transactions / window functions raise explicit errors, no emulation
- No data sync (CDC) from external PG/ES clusters: the write path means clients writing to
  Milvus through the gateway, not migrating data from an old database

## Development

```bash
make run     # ES entry point at 127.0.0.1:9200 by default
curl 127.0.0.1:9200/          # handshake
# without --milvus-uri the gateway logs searches and returns empty results
go run ./cmd/gateway --milvus-uri 'https://host:19530' --milvus-token 'KEY'
curl -XPOST 127.0.0.1:9200/idx/_search \
  -d '{"query":{"range":{"price":{"gte":10}}},"sort":[{"price":"desc"}],"from":0,"size":5}'
```

The e2e suites skip unless configured, so credentials stay out of the repo:

- `LYREBIRD_TEST_MILVUS_URI` / `LYREBIRD_TEST_MILVUS_TOKEN` — any real Milvus
  (or Zilliz Cloud); the full-stack suites (store executor, ES surface, PG
  wire) run against it.
- `LYREBIRD_TEST_ES_ADDR` — a real Elasticsearch; the ES parity suite
  replays the same search bodies against both and compares totals and hit
  ids.
- `LYREBIRD_TEST_PG_DSN` — a real Postgres with pgvector; the PG parity
  suite replays the same SQL against both and compares columns and rows.

`make e2e` boots all three stacks (`docker-compose.e2e.yml`: Milvus
standalone, Elasticsearch 8.x, Postgres+pgvector), seeds the same fixtures
into each, and runs everything; the `E2E` GitHub Actions workflow runs the
same thing on every push and pull request.

## Connecting GUI tools

The gateway reads your Milvus cluster's live metadata, so any catalog-aware
client sees database → table → column without extra setup:

- **DataGrip (PostgreSQL source)**: connect to `jdbc:postgresql://host:5433/postgres`
  (any username/password). The tree shows one database per Milvus database —
  use the database name from the catalog list to browse another one. If
  introspection stalls, set the data source's introspection level to
  **JDBC metadata** (right-click the schema → Introspection Level); the
  Queried/Raw levels send wider pg_catalog queries and are covered on a
  best-effort basis.
- **DataGrip (Elasticsearch)**: install the *ES REST Data Source* plugin,
  connect to `jdbc:es-rest://host:9200`. Indices browse as tables; queries
  run through `/{index}/_search`.
- **psql**: `psql 'postgres://user@host:5433/postgres'` — `\l` lists the
  Milvus databases, `\dt` the collections, and data queries run as usual.
  The per-column describe (`\d table`, `\d+ table`) walks psql's own
  version-specific PRM SQL and is covered best-effort: it works when the
  engine parses psql's query shapes, and fails with a clean 42601/42883
  where it does not (no silent wrong answers).

Types follow the pg-wire contract (docs/design.md): every number reads as
`float8`, strings as `text`, JSON/array/vector fields as `json`.

