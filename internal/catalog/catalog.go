// Package catalog implements the introspection surface of the PostgreSQL
// entry point: the virtual pg_catalog / information_schema tables that GUI
// clients (DataGrip, psql's \d family) and JDBC metadata calls read to draw
// the database → schema → table → column tree.
//
// The catalog is generated live from the backing store: a Provider (the
// Milvus executor) supplies the collection list and per-collection field
// metadata, and the virtual tables project it onto PostgreSQL vocabulary
// (database = Milvus database, table = collection, column = field).
//
// Clients reach it through ordinary SELECT statements against catalog tables,
// evaluated by a small dedicated engine (this package). That engine accepts
// the shapes catalog queries actually use — joins, CASE, LIKE/regex filters,
// ORDER BY over output aliases, bind parameters, a function whitelist — but
// deliberately nothing else: data reads stay on the translate/pg pipeline,
// and anything the engine cannot parse never reaches it (the pgserver
// dispatcher only routes statements whose FROM references catalog tables).
//
// No LIMIT is enforced here: catalog row sets are the cluster's own metadata,
// small by construction, unlike the mandatory-LIMIT data read path.
package catalog

import (
	"context"

	"github.com/huanghaoyuanhhy/lyrebird/internal/translate"
)

// FieldMeta is one collection field as the catalog sees it: the translate
// type vocabulary plus the flag-level facts catalog tables carry.
type FieldMeta struct {
	Name       string
	Type       translate.FieldType
	PrimaryKey bool
	Nullable   bool
	// Dim is the vector dimension (TypeParams["dim"]); 0 on non-vector fields.
	Dim int
	// MaxLength is a VarChar field's max_length type param; 0 when unset.
	// It drives pg_attribute's atttypmod (format_type renders varchar(n)).
	MaxLength int
}

// CollectionMeta is one collection's catalog view: the name plus its fields
// in storage order (the order pg_attribute.attnum reports).
type CollectionMeta struct {
	Name   string
	Fields []FieldMeta
}

// Provider supplies the live metadata the virtual tables project. The
// Milvus executor implements it; db selects the Milvus database a PG
// connection is attached to ("" means the gateway's default database).
// All Milvus speaking stays behind this interface — catalog code never
// imports the SDK.
type Provider interface {
	// Databases lists every Milvus database visible to the gateway (the rows
	// of pg_database).
	Databases(ctx context.Context) ([]string, error)

	// ListCollections lists the collections of one database (pg_class rows).
	ListCollections(ctx context.Context, db string) ([]string, error)

	// Collection describes one collection of one database.
	Collection(ctx context.Context, db, name string) (CollectionMeta, error)
}
