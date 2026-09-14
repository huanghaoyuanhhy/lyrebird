// Package pg translates a SELECT subset of the PostgreSQL dialect into
// Milvus query plans — the pgvector compatibility surface: SQL clients
// (psql, JDBC, psycopg) connect through pgserver and read collections as
// tables.
//
// Entry point: Parse(sql) → *Select, then Select.Plan(schema) →
// *translate.Plan. The split exists because a SQL statement names its own
// table (FROM items) while translate.Plan deliberately carries no
// collection name — the store Executor takes the collection as a separate
// argument, and the schema the lowering step needs is resolved per
// collection. So Parse runs first with no cluster access (syntax and
// capability checks), the caller resolves Select.Table's schema
// (Executor.Schema), and Plan lowers schema-aware:
//
//	sel, err := pg.Parse(sql)
//	schema, err := exec.Schema(ctx, sel.Table)
//	plan, err := sel.Plan(schema)
//	exec.Search(ctx, sel.Table, plan)
//
// Phase 2 subset (scalar reads):
//
//   - SELECT * or a plain column list; expressions, functions, aliases and
//     DISTINCT fail fast (computed columns need response shaping not built)
//   - FROM one table with an optional alias; column qualifiers (t.col) are
//     checked against it — joins, subqueries and multi-table FROM fail fast
//   - WHERE with =, <>, !=, <, <=, >, >=, AND/OR/NOT, parentheses, IN,
//     BETWEEN, IS [NOT] NULL, TRUE/FALSE/NULL. Comparisons follow SQL
//     three-valued logic: x = NULL never passes a row, x IN (1, NULL) is
//     exactly x = 1, x NOT IN (…, NULL) passes nothing — IS NULL is how
//     SQL tests for null, and the translations below are exact, not
//     approximations
//   - ORDER BY column [ASC|DESC]; LIMIT and OFFSET in either order, with
//     PG's ROW/ROWS noise words. LIMIT is required: an uncapped SELECT
//     would stream the whole collection through the store's client-side
//     sort path
//
// Fail-fast class: valid SQL beyond the subset returns 0A000
// (feature_not_supported) naming the member — GROUP BY, aggregates, LIKE,
// casts, query parameters. The pgvector distance operators (<-> <=> <#>
// <+>) parse cleanly and are rejected as the not-yet-wired vector path;
// the operator→metric mapping that path will implement:
//
//	<->  L2 distance     → L2      (distances carry over unchanged)
//	<=>  cosine distance → COSINE  (pgvector returns distance 0..2, Milvus
//	                       similarity 1..-1 — the shell converts)
//	<#>  negated dot      → IP      (pgvector emits -(dot); negate on output)
//	<+>  L1 distance     → no Milvus metric; stays rejected
//
// Mapping notes (see docs/design.md):
//
//   - table → collection, column → field. Storage is shared with the ES
//     front, so value coercions match it: numbers keep their kind (keyword
//     fields take the text form), date fields take epoch millis from
//     date strings. PG-native microseconds are a Phase 3 catalog decision —
//     until then both fronts must read one storage encoding.
//   - identifiers keep their written case. Folding to lower case (what PG
//     does to unquoted names) would break the mixed-case fields ES clients
//     created; quote an identifier that collides with a keyword instead.
//   - errors are *translate.Error with SQLSTATE codes as Type: 42601
//     syntax, 22023 invalid value, 0A000 beyond the subset — the wire
//     protocol can render them verbatim.
package pg
