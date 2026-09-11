// Package pg will translate SQL (a SELECT subset) into Milvus query/search.
// The goal is a well-defined subset of the PG dialect; queries beyond the
// supported capability set return structured errors rather than best-effort
// emulation.
package pg
