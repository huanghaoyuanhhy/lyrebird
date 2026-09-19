package pgserver

import (
	"context"
	"fmt"
	"time"

	"github.com/jeroenrinzema/psql-wire"
	"github.com/jeroenrinzema/psql-wire/codes"
	psqlerr "github.com/jeroenrinzema/psql-wire/errors"
	"go.uber.org/zap"

	"github.com/huanghaoyuanhhy/lyrebird/internal/catalog"
)

// catalogStatement wraps a prepared catalog query as a wire statement. The
// output shape is static (derived from the virtual-table definitions at
// prepare time), so RowDescription goes out without touching the store; the
// provider is only consulted inside the statement body, once per execution.
func (s *Server) catalogStatement(st *catalog.Statement) (wire.PreparedStatements, error) {
	cols := st.Columns()
	columns := make(wire.Columns, len(cols))
	oids := make([]uint32, len(cols))
	for i, c := range cols {
		oids[i] = c.Oid
		columns[i] = wire.Column{Name: c.Name, Oid: c.Oid}
	}
	return wire.Prepared(wire.NewStatement(s.execCatalog(st, oids), wire.WithColumns(columns), wire.WithParameters(make([]uint32, st.ParamCount())))), nil
}

// execCatalog runs the statement: bind parameters arrive as text, the
// connection's database names the catalog slice to project, and rows are
// coerced per column OID before hitting the wire.
func (s *Server) execCatalog(st *catalog.Statement, oids []uint32) wire.PreparedStatementFn {
	return func(ctx context.Context, writer wire.DataWriter, parameters []wire.Parameter) (err error) {
		// psql-wire does not recover per connection: a panic here would take
		// the whole gateway down.
		defer func() {
			if p := recover(); p != nil {
				err = psqlerr.WithCode(fmt.Errorf("internal error while executing the statement: %v", p), codes.Internal)
			}
		}()

		params := make([]any, len(parameters))
		for i, p := range parameters {
			if p.Value() == nil {
				continue // unbound parameter → SQL NULL
			}
			params[i] = string(p.Value())
		}

		start := time.Now()
		db := s.databaseName(ctx)
		rows, err := st.Exec(ctx, s.cluster, db, params)
		if err != nil {
			s.zapLogger().Warn("catalog query failed",
				zap.String("database", db), zap.Error(err))
			return wireError(err)
		}
		for _, row := range rows {
			cells := make([]any, len(row))
			for i, v := range row {
				cells[i] = catalog.Coerce(oids[i], v)
			}
			if err := writer.Row(cells); err != nil {
				return err
			}
		}

		s.zapLogger().Info("catalog query",
			zap.String("database", db),
			zap.Uint32("returned", writer.Written()),
			zap.Duration("took", time.Since(start)))
		return writer.Complete(fmt.Sprintf("SELECT %d", writer.Written()))
	}
}
