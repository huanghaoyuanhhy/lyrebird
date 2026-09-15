// Package pgserver implements the PostgreSQL wire protocol entry point:
// psql, JDBC and psycopg connect directly, SQL is translated by translate/pg
// and executed against Milvus via store.
//
// The protocol itself is delegated to jeroenrinzema/psql-wire (docs/design.md):
// it covers the handshake, SSL negotiation and the simple + extended query
// cycles. This package owns everything above it — the per-statement pipeline
//
//	pg.Parse → Executor.Schema → Select.Plan → (statement) → Executor.Search
//
// and the SQLSTATE error rendering (translate.Error.Type carries the code
// verbatim, so a classified failure becomes a native PG error with no
// mapping beyond wireError).
//
// Two wire-protocol facts shape the code:
//
//   - RowDescription precedes the rows it describes, so SELECT * must expand
//     to column names before execution (Select.Columns + Schema.Fields) —
//     the store's answer arrives too late to describe it.
//   - Cells travel in text format under a declared type OID; cellValue
//     narrows stored values to the Go type that OID's codec encodes
//     (float8 for every number, text for strings, json for unknown fields).
package pgserver
