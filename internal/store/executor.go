package store

import (
	"context"
	"fmt"
	"log/slog"

	"lyrebird/internal/translate"
)

// Executor executes translated plans against the backing store. It is the
// only layer that talks to Milvus; everything above it speaks Plan.
// collection is the Milvus-side name — contract vocabulary follows the
// target (docs/design.md) — and the protocol shells translate their native
// names (ES index, PG table) before calling in.
type Executor interface {
	Search(ctx context.Context, collection string, plan *translate.Plan) (*SearchResult, error)
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
	// Logger receives one structured line per search; nil uses slog.Default().
	Logger *slog.Logger
}

// Search implements Executor. Render errors are returned, not swallowed:
// Render is where field names get validated, so an invalid plan fails here
// exactly where the Milvus executor would fail later.
func (e *LogExecutor) Search(ctx context.Context, collection string, plan *translate.Plan) (*SearchResult, error) {
	logger := e.Logger
	if logger == nil {
		logger = slog.Default()
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

	logger.InfoContext(ctx, "store: search (log executor)",
		"collection", collection,
		"expr", expr,
		"offset", plan.Offset,
		"limit", plan.Limit,
		"sort", plan.Sort,
		"source", plan.Source,
		"no_match", plan.NoMatch,
	)

	return &SearchResult{}, nil
}
