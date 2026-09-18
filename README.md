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
internal/translate
    ├─ pg/               SQL → Select (IR) → fold → Plan
    └─ es/               ES Query DSL → queryNode (IR) → fold → Plan
internal/store           Milvus Executor: Schema(collection) + Search(collection, plan)
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
- [ ] **Phase 3** Schema catalog: config-driven mapping + describe-based discovery;
  DDL (CREATE TABLE → CreateCollection)
- [ ] **Phase 4** Write path: insert/update/delete → upsert/delete
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

The e2e suites (`internal/store`, `internal/esserver`) run against a real
Milvus/Zilliz Cloud instance when `LYREBIRD_TEST_MILVUS_URI` and
`LYREBIRD_TEST_MILVUS_TOKEN` are set, and skip otherwise — credentials stay
out of the repo.
