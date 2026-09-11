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
psql ─── SQL ───┐
                │  translate(pure fn)   ┌──────────┐
es sdk ── DSL ──┼──────────────────────▶│  Milvus  │
                │                       └──────────┘
```

## Architecture

```
cmd/gateway              entry point: start listener(s)
internal/pgserver        PG wire protocol server (Phase 2)
internal/esserver        ES-compatible REST server (Phase 1)
internal/translate
    ├─ pg/               SQL → Milvus query/search
    └─ es/               ES Query DSL → Milvus query/search
internal/store           Milvus client wrapper + schema catalog (table/index → collection)
```

Principle: **the translation layer is a pure function** `(protocol request, schema) → Milvus
operations` — no IO, everything covered by golden-file unit tests. The protocol layer only
handles encoding/decoding; the store layer only executes. Queries beyond the supported
capability set (joins, aggregations) fail fast with an error — no degraded emulation.

## Roadmap

- [x] **Phase 0** Skeleton: ES entry answers the `GET /` handshake (with the
  `X-elastic-product` header), `_search` returns 501
- [ ] **Phase 1** ES read-only subset: `_search` + bool/term/terms/match/range/exists +
  sort/size/from + `_source` projection
  - [x] translation layer (`internal/translate/es`: DSL → `translate.Plan`, pure
    functions + unit tests)
  - [ ] store wiring: Milvus client executes the plan; `_search` endpoint activation
- [ ] **Phase 2** PG read-only subset: wire protocol up, psql connects,
  `SELECT .. WHERE .. LIMIT` translation
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
curl -XPOST 127.0.0.1:9200/idx/_search -d '{"query":{"match_all":{}}}'   # 501
```
