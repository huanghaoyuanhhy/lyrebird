// Package pgserver implements the PostgreSQL wire protocol entry point:
// psql, JDBC and psycopg connect directly; SQL is routed per statement and
// executed against Milvus via store.
//
// The protocol itself is delegated to jeroenrinzema/psql-wire (docs/design.md):
// it covers the handshake, SSL negotiation and the simple + extended query
// cycles. This package owns everything above it — the per-statement dispatch
//
//	session statements  → ack handlers (session.go): SET / SHOW / RESET /
//	                        DISCARD / BEGIN / COMMIT / ROLLBACK, the surface
//	                        pgjdbc configures every connection through
//	catalog SELECTs     → internal/catalog engine (catalog.go): the virtual
//	                        pg_catalog / information_schema tables GUI
//	                        clients introspect, evaluated against a live
//	                        Provider snapshot
//	collection SELECTs  → the read pipeline (query.go):
//	                        pg.Parse → Executor.Schema → Select.Plan →
//	                        (statement) → Executor.Search
//
// plus the SQLSTATE error rendering (translate.Error.Type carries the code
// verbatim, so a classified failure becomes a native PG error with no
// mapping beyond wireError).
//
// Wire-protocol facts that shape the code:
//
//   - RowDescription precedes the rows it describes, so both SELECT * over a
//     collection and every catalog query must derive their output columns
//     before execution — the store's answer arrives too late to describe it.
//   - Cells travel in text format under a declared type OID; cellValue (data
//     path) and catalog.Coerce (catalog path) narrow values to the Go type
//     that OID's codec encodes (float8 for every number, text for strings,
//     json for unknown fields).
//   - The startup message names the database the connection attaches to
//     (wire.ClientParameters); "postgres" and the empty default alias onto
//     the gateway's configured Milvus database, any other name must be a
//     real one — collections and catalog data both follow it.
package pgserver
