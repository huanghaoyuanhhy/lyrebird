package store

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/milvus-io/milvus/client/v3/entity"
	"github.com/milvus-io/milvus/client/v3/milvusclient"

	"github.com/huanghaoyuanhhy/lyrebird/internal/catalog"
)

// Cluster is the whole cluster as the protocol shells consume it: the
// default-database read path, per-database read views, and the introspection
// data the catalog projects (Milvus database → PG database, collection →
// table, field → column). MilvusExecutor implements it; LogExecutor stands
// in for development and tests.
type Cluster interface {
	Executor

	// Database binds a read path to one Milvus database; "" selects the
	// default. PG connections carry their database in the startup message,
	// so per-database views — not per-call flags — are the seam.
	Database(db string) (Executor, error)

	// DefaultDatabase is the configured base database name.
	DefaultDatabase() string

	// The catalog.Provider surface, listed here so the shells can take one
	// interface. db semantics match the catalog package: "" = default.
	Databases(ctx context.Context) ([]string, error)
	ListCollections(ctx context.Context, db string) ([]string, error)
	Collection(ctx context.Context, db, name string) (catalog.CollectionMeta, error)
}

var _ Cluster = (*MilvusExecutor)(nil)

// Database implements Cluster: same engine, one pooled Milvus client per
// database. The SDK's UseDatabase mutates a shared client, so concurrent
// connections reading different databases need their own clients — pooled
// here, dialed with DBName set at construction.
func (e *MilvusExecutor) Database(db string) (Executor, error) {
	if db == "" || db == e.db {
		return e, nil
	}
	cli, err := e.clientFor(context.Background(), db)
	if err != nil {
		return nil, err
	}
	return &MilvusExecutor{cfg: e.cfg, cli: cli, db: db, pool: e.pool}, nil
}

// DefaultDatabase implements Cluster.
func (e *MilvusExecutor) DefaultDatabase() string { return e.db }

// clientFor returns the pooled client for one database, dialing it on
// first use.
func (e *MilvusExecutor) clientFor(ctx context.Context, db string) (*milvusclient.Client, error) {
	if db == "" || db == e.db {
		return e.cli, nil
	}
	e.poolMu.Lock()
	defer e.poolMu.Unlock()
	if cli, ok := e.pool[db]; ok {
		return cli, nil
	}
	cfg := e.cfg
	cli, err := milvusclient.New(ctx, &milvusclient.ClientConfig{
		Address:       cfg.URI,
		APIKey:        cfg.Token,
		EnableTLSAuth: strings.HasPrefix(cfg.URI, "https://"),
		DBName:        db,
	})
	if err != nil {
		return nil, fmt.Errorf("connect to milvus database %q: %w", db, err)
	}
	e.pool[db] = cli
	return cli, nil
}

// Databases implements Cluster: every Milvus database visible to the
// gateway — the rows of pg_database.
func (e *MilvusExecutor) Databases(ctx context.Context) ([]string, error) {
	names, err := e.cli.ListDatabase(ctx, milvusclient.NewListDatabaseOption())
	if err != nil {
		return nil, fmt.Errorf("list databases: %w", err)
	}
	return names, nil
}

// ListCollections implements Cluster: the collections of one database.
func (e *MilvusExecutor) ListCollections(ctx context.Context, db string) ([]string, error) {
	cli, err := e.clientFor(ctx, db)
	if err != nil {
		return nil, err
	}
	names, err := cli.ListCollections(ctx, milvusclient.NewListCollectionOption())
	if err != nil {
		return nil, fmt.Errorf("list collections in database %q: %w", db, err)
	}
	return names, nil
}

// Collection implements Cluster: one collection's field metadata in storage
// order, with function outputs (BM25 sparse fields) excluded — they are not
// part of any readable projection, matching the Schema implementation.
func (e *MilvusExecutor) Collection(ctx context.Context, db, name string) (catalog.CollectionMeta, error) {
	cli, err := e.clientFor(ctx, db)
	if err != nil {
		return catalog.CollectionMeta{}, err
	}
	coll, err := cli.DescribeCollection(ctx, milvusclient.NewDescribeCollectionOption(name))
	if err != nil {
		if strings.Contains(err.Error(), "can't find collection") || strings.Contains(err.Error(), "not found") {
			return catalog.CollectionMeta{}, fmt.Errorf("%w: %s", ErrCollectionNotFound, name)
		}
		return catalog.CollectionMeta{}, fmt.Errorf("describe collection %q: %w", name, err)
	}
	return catalogMeta(coll), nil
}

// catalogMeta projects a described collection onto the catalog vocabulary.
func catalogMeta(coll *entity.Collection) catalog.CollectionMeta {
	meta := catalog.CollectionMeta{Name: coll.Schema.CollectionName}
	funcOutputs := functionOutputFields(coll.Schema)
	for _, f := range coll.Schema.Fields {
		if isFunctionOutput(f.Name, funcOutputs) {
			continue
		}
		meta.Fields = append(meta.Fields, catalog.FieldMeta{
			Name:       f.Name,
			Type:       translateFieldType(f),
			PrimaryKey: f.PrimaryKey,
			Nullable:   f.Nullable,
			Dim:        typeParamInt(f.TypeParams, "dim"),
			MaxLength:  typeParamInt(f.TypeParams, "max_length"),
		})
	}
	return meta
}

func typeParamInt(params map[string]string, key string) int {
	n, _ := strconv.Atoi(params[key])
	return n
}
