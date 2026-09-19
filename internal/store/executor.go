package store

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"github.com/huanghaoyuanhhy/lyrebird/internal/catalog"
	"github.com/huanghaoyuanhhy/lyrebird/internal/translate"
)

// Executor executes translated plans against the backing store. It is the
// only layer that talks to Milvus; everything above it speaks Plan.
// collection is the Milvus-side name — contract vocabulary follows the
// target (docs/design.md) — and the protocol shells translate their native
// names (ES index, PG table) before calling in.
type Executor interface {
	Search(ctx context.Context, collection string, plan *translate.Plan) (*SearchResult, error)

	// Schema answers field-type questions for the collection, as seen by the
	// backing store. Translators call it before parsing so type-sensitive
	// choices (match → exact compare vs token match) follow the real fields;
	// absent fields read as translate.TypeUnknown.
	Schema(ctx context.Context, collection string) (translate.Schema, error)

	// Describe answers the fine-grained collection metadata (storage types,
	// nullability, dims, primary key, auto-generation) the strict write path
	// validates against. Reads lean on Schema's coarse vocabulary; writes
	// need the detail.
	Describe(ctx context.Context, collection string) (catalog.CollectionMeta, error)

	// Insert writes new rows, rejecting any input that does not match the
	// live collection schema (the strict write path — docs/design.md).
	Insert(ctx context.Context, collection string, rows []translate.WriteRow) (WriteResult, error)

	// Upsert writes rows by primary key, replacing each stored row whole.
	Upsert(ctx context.Context, collection string, rows []translate.WriteRow) (WriteResult, error)
}

// Hit is one matched entity: its primary key and the fields fetched for it.
// Source holds values in storage encoding; protocol shells shape them into
// their native row/document form.
type Hit struct {
	ID     string
	Source map[string]any
}

// SearchResult is the executor's entire vocabulary for "what came back":
// shells derive totals, documents and response envelopes from it.
type SearchResult struct {
	Total int64
	Hits  []Hit
}

// LogExecutor is the development stand-in for the Milvus executor: it logs
// the call — collection, rendered filter, paging — and returns an empty
// success. Entry points wire against it so the full parse → translate →
// execute path runs before the real store lands.
type LogExecutor struct {
	// Logger receives one structured entry per search; nil uses the global
	// zap.L() (wired by zap.ReplaceGlobals in main).
	Logger *zap.Logger
}

// Database implements Cluster: the stand-in has no databases to separate.
func (e *LogExecutor) Database(db string) (Executor, error) { return e, nil }

// DefaultDatabase implements Cluster.
func (e *LogExecutor) DefaultDatabase() string { return "default" }

// Databases implements Cluster.
func (e *LogExecutor) Databases(ctx context.Context) ([]string, error) {
	return []string{"default"}, nil
}

// ListCollections implements Cluster: the stand-in knows no collections.
func (e *LogExecutor) ListCollections(ctx context.Context, db string) ([]string, error) {
	return nil, nil
}

// Collection implements Cluster: nothing exists, so describe misses.
func (e *LogExecutor) Collection(ctx context.Context, db, name string) (catalog.CollectionMeta, error) {
	return catalog.CollectionMeta{}, fmt.Errorf("%w: %s", ErrCollectionNotFound, name)
}

// CollectionStats implements Cluster: the stand-in has no entities.
func (e *LogExecutor) CollectionStats(ctx context.Context, db, name string) (int64, error) {
	return 0, nil
}

// Schema implements Executor: the stand-in knows no fields, so every lookup
// reads as unknown — the same answer as a real collection with no fields.
func (e *LogExecutor) Schema(ctx context.Context, collection string) (translate.Schema, error) {
	return translate.MapSchema{}, nil
}

// Describe implements Executor: nothing exists, so every describe misses.
func (e *LogExecutor) Describe(ctx context.Context, collection string) (catalog.CollectionMeta, error) {
	return catalog.CollectionMeta{}, fmt.Errorf("%w: %s", ErrCollectionNotFound, collection)
}

// Insert implements Executor: the stand-in writes nowhere, logging the rows.
func (e *LogExecutor) Insert(ctx context.Context, collection string, rows []translate.WriteRow) (WriteResult, error) {
	logger := e.Logger
	if logger == nil {
		logger = zap.L()
	}
	logger.Info("store: insert (log executor)",
		zap.String("collection", collection), zap.Int("rows", len(rows)))
	return WriteResult{Count: int64(len(rows))}, nil
}

// Upsert implements Executor: the stand-in writes nowhere, logging the rows.
func (e *LogExecutor) Upsert(ctx context.Context, collection string, rows []translate.WriteRow) (WriteResult, error) {
	logger := e.Logger
	if logger == nil {
		logger = zap.L()
	}
	logger.Info("store: upsert (log executor)",
		zap.String("collection", collection), zap.Int("rows", len(rows)))
	return WriteResult{Count: int64(len(rows))}, nil
}

// Search implements Executor. Render errors are returned, not swallowed:
// Render is where field names get validated, so an invalid plan fails here
// exactly where the Milvus executor would fail later.
func (e *LogExecutor) Search(ctx context.Context, collection string, plan *translate.Plan) (*SearchResult, error) {
	logger := e.Logger
	if logger == nil {
		logger = zap.L()
	}

	expr := "(no filter)"
	switch {
	case plan.NoMatch:
		expr = "(no match: guaranteed empty)"
	case plan.Expr != nil:
		rendered, err := translate.Render(plan.Expr)
		if err != nil {
			return nil, fmt.Errorf("render plan for collection %q: %w", collection, err)
		}
		expr = rendered
	}

	logger.Info("store: search (log executor)",
		zap.String("collection", collection),
		zap.String("expr", expr),
		zap.Int("offset", plan.Offset),
		zap.Int("limit", plan.Limit),
		zap.Any("sort", plan.Sort),
		zap.Any("source", plan.Source),
		zap.Any("search", plan.Search),
		zap.Bool("no_match", plan.NoMatch),
	)

	return &SearchResult{}, nil
}
