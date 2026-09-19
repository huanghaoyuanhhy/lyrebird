package pgserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/jeroenrinzema/psql-wire"
	"github.com/jeroenrinzema/psql-wire/codes"
	psqlerr "github.com/jeroenrinzema/psql-wire/errors"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/huanghaoyuanhhy/lyrebird/internal/catalog"
	"github.com/huanghaoyuanhhy/lyrebird/internal/store"
	"github.com/huanghaoyuanhhy/lyrebird/internal/translate/pg"
)

// Server serves the PostgreSQL wire protocol over TCP. Connections are
// unauthenticated (trust): the gateway fronts a read-only scalar slice of
// the cluster, and per-user auth is a later concern (docs/design.md).
type Server struct {
	cluster store.Cluster
	logger  *zap.Logger
}

// New constructs the PG wire server around a cluster (the real
// MilvusExecutor, or the LogExecutor for development without a cluster).
// A nil logger falls back to the global zap logger.
func New(cluster store.Cluster, logger *zap.Logger) *Server {
	return &Server{cluster: cluster, logger: logger}
}

func (s *Server) zapLogger() *zap.Logger {
	if s.logger == nil {
		return zap.L()
	}
	return s.logger
}

// ListenAndServe binds addr and serves PG wire connections until ctx is
// canceled, then drains in-flight queries within the wire server's shutdown
// timeout.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	return s.serve(ctx, ln)
}

// serve runs the wire server on an existing listener; split out so tests
// can bind port 0.
func (s *Server) serve(ctx context.Context, ln net.Listener) error {
	logger := s.zapLogger()
	logger.Info("PostgreSQL-compatible entry point listening", zap.String("addr", ln.Addr().String()))

	srv, err := wire.NewServer(s.parse, wire.Logger(slog.New(&zapSlog{logger: logger})), wire.GlobalParameters(serverParameters))
	if err != nil {
		return fmt.Errorf("build wire server: %w", err)
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		// Shutdown closes the listener and waits for in-flight queries;
		// Serve's own return (accept on a closed listener) is dropped.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("drain pg connections: %w", err)
		}
		return nil
	}
}

// serverParameters is the ParameterStatus broadcast every client session
// starts with. psql-wire sends none by default, but real clients gate
// behavior on them: pgx refuses simple-protocol queries unless
// standard_conforming_strings is on, and psql renders the version string.
// The version matches catalog.VersionString, so version() and the startup
// packet agree.
var serverParameters = wire.Parameters{
	wire.ParamServerVersion:       catalog.VersionString,
	wire.ParamServerEncoding:      "UTF8",
	wire.ParamClientEncoding:      "UTF8",
	"standard_conforming_strings": "on",
	"DateStyle":                   "ISO, MDY",
	"integer_datetimes":           "on",
	"TimeZone":                    "UTC",
}

// parse is the psql-wire entry point: one SQL statement in, one prepared
// statement out. The three families dispatch here so the wire layer can
// describe result columns before executing (RowDescription precedes the
// rows it describes); the store is only hit inside statement bodies.
func (s *Server) parse(ctx context.Context, query string) (wire.PreparedStatements, error) {
	logger := s.zapLogger()

	// 1. session statements: SET / SHOW / RESET / DISCARD / transactions.
	// These never touch the store, so they are classified by their first
	// word before any parsing.
	if words, err := catalog.Words(query); err == nil && len(words) > 0 {
		if isSessionStatement(strings.ToUpper(words[0])) {
			return s.sessionStatement(words)
		}
	}

	// 2. catalog introspection and scalar SELECTs (SELECT 1, version(), …).
	// Prepare only succeeds when every FROM reference is a virtual catalog
	// table, so collection reads never land here.
	if catalog.Prepareable(query) {
		st, err := catalog.Prepare(query)
		if err != nil {
			// a failed parse is the catalog engine's problem only when the
			// query actually references catalog tables; otherwise it belongs
			// to the collection pipeline below
			if catalog.ReferencesCatalogTables(query) {
				logger.Warn("pg query rejected", zap.String("stage", "catalog parse"), zap.Error(err))
				return nil, wireError(err)
			}
		} else {
			return s.catalogStatement(st)
		}
	}

	// 3. collection reads: the translate/pg pipeline, against the
	// connection's database view.
	sel, err := pg.Parse(query)
	if err != nil {
		logger.Warn("pg query rejected", zap.String("stage", "parse"), zap.Error(err))
		return nil, wireError(err)
	}

	exec, err := s.databaseView(ctx)
	if err != nil {
		return nil, wireError(err)
	}

	schema, err := exec.Schema(ctx, sel.Table)
	if err != nil {
		if errors.Is(err, store.ErrCollectionNotFound) {
			logger.Info("query on missing table", zap.String("table", sel.Table))
			return nil, psqlerr.WithCode(fmt.Errorf("relation %q does not exist", sel.Table), codes.UndefinedTable)
		}
		logger.Error("schema lookup failed", zap.String("table", sel.Table), zap.Error(err))
		return nil, wireError(err)
	}

	plan, err := sel.Plan(schema)
	if err != nil {
		logger.Warn("pg query rejected", zap.String("stage", "plan"), zap.Error(err))
		return nil, wireError(err)
	}

	columns, types := resultColumns(sel, schema)
	return wire.Prepared(wire.NewStatement(s.search(exec, sel, plan, types), wire.WithColumns(columns))), nil
}

// databaseView resolves the connection's database: the startup parameter,
// with the two spellings every PG client uses ("", the empty default, and
// "postgres", the URL default) aliased onto the gateway's configured
// database. Any other name must be a real Milvus database.
func (s *Server) databaseView(ctx context.Context) (store.Executor, error) {
	switch db := wire.ClientParameters(ctx)["database"]; db {
	case "", "postgres":
		return s.cluster.Database("")
	default:
		return s.cluster.Database(db)
	}
}

// databaseName is databaseView's twin for the catalog engine, which wants
// the concrete database name (current_database()'s answer) rather than a
// read view.
func (s *Server) databaseName(ctx context.Context) string {
	switch db := wire.ClientParameters(ctx)["database"]; db {
	case "", "postgres":
		return s.cluster.DefaultDatabase()
	default:
		return db
	}
}

// zapSlog adapts a *zap.Logger to the slog interface psql-wire logs through,
// so protocol-level entries land in the same log pipeline as lyrebird's own.
type zapSlog struct {
	logger *zap.Logger
}

func (h zapSlog) Enabled(_ context.Context, level slog.Level) bool {
	return h.logger.Core().Enabled(zapLevel(level))
}

func (h zapSlog) Handle(_ context.Context, r slog.Record) error {
	log := h.logger.Debug
	switch {
	case r.Level >= slog.LevelError:
		log = h.logger.Error
	case r.Level >= slog.LevelWarn:
		log = h.logger.Warn
	case r.Level >= slog.LevelInfo:
		log = h.logger.Info
	}
	fields := make([]zap.Field, 0, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool {
		fields = append(fields, zap.Any(a.Key, a.Value.Any()))
		return true
	})
	log(r.Message, fields...)
	return nil
}

func (h zapSlog) WithAttrs(attrs []slog.Attr) slog.Handler { return h }

func (h zapSlog) WithGroup(name string) slog.Handler { return h }

func zapLevel(level slog.Level) zapcore.Level {
	switch {
	case level >= slog.LevelError:
		return zapcore.ErrorLevel
	case level >= slog.LevelWarn:
		return zapcore.WarnLevel
	case level >= slog.LevelInfo:
		return zapcore.InfoLevel
	default:
		return zapcore.DebugLevel
	}
}
