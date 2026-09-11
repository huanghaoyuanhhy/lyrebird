// Package translate is the protocol-agnostic translation layer: it maps
// (SQL | ES Query DSL, schema) to Milvus operations. Must be pure functions,
// no IO, covered by golden-file unit tests. Subpackages:
//
//   - pg: SQL → Milvus query/search
//   - es: ES Query DSL → Milvus query/search
package translate
