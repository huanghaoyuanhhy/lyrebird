package pgserver

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jeroenrinzema/psql-wire"
	"github.com/jeroenrinzema/psql-wire/codes"
	psqlerr "github.com/jeroenrinzema/psql-wire/errors"
	"go.uber.org/zap"

	"github.com/huanghaoyuanhhy/lyrebird/internal/store"
	"github.com/huanghaoyuanhhy/lyrebird/internal/translate"
	"github.com/huanghaoyuanhhy/lyrebird/internal/translate/pg"
)

// updateScanLimit caps how many rows one UPDATE may rewrite. The
// read-modify-write streams every match through the same window the read
// path sorts in, so a write cannot exceed what a SELECT could fetch.
const updateScanLimit = 10000

// writeWireError renders a write-path failure as a PG wire error: a strict
// schema rejection carries its SQLSTATE (the class → code table below) so
// psql and pgx report it like any PG write failure; everything else falls
// back to the shared mapping.
func writeWireError(err error) error {
	var we *store.WriteError
	if errors.As(err, &we) {
		return psqlerr.WithCode(errors.New(we.Reason), codes.Code(sqlstateOf(we.Class)))
	}
	return wireError(err)
}

// sqlstateOf picks the SQLSTATE a write rejection class reports as.
func sqlstateOf(c store.WriteErrClass) string {
	switch c {
	case store.ClassUnknownField:
		return "42703" // undefined_column
	case store.ClassMissing, store.ClassNull:
		return "23502" // not_null_violation
	case store.ClassType:
		return "42804" // datatype_mismatch
	case store.ClassRange:
		return "22003" // numeric_value_out_of_range
	case store.ClassLength:
		return "22001" // string_data_right_truncation
	case store.ClassDim:
		return "22023" // invalid_parameter_value
	case store.ClassFunctionField, store.ClassAutoID:
		return "428C9" // invalid_generated_column_reference
	default:
		return "0A000" // feature_not_supported
	}
}

// writeStatement is the INSERT statement body: lower the rows against the
// connection's schema view, then insert. A strict-schema rejection aborts
// the statement before anything is sent to the store.
func (s *Server) writeInsertStatement(exec store.Executor, ins *pg.Insert) wire.PreparedStatementFn {
	return func(ctx context.Context, writer wire.DataWriter, parameters []wire.Parameter) (err error) {
		defer func() {
			if p := recover(); p != nil {
				err = psqlerr.WithCode(fmt.Errorf("internal error while executing the statement: %v", p), codes.Internal)
			}
		}()
		if len(parameters) > 0 {
			return psqlerr.WithCode(errors.New("bind parameters are beyond the lyrebird write path: inline the literals"), codes.FeatureNotSupported)
		}

		start := time.Now()
		schema, err := exec.Schema(ctx, ins.Table)
		if err != nil {
			if errors.Is(err, store.ErrCollectionNotFound) {
				return psqlerr.WithCode(fmt.Errorf("relation %q does not exist", ins.Table), codes.UndefinedTable)
			}
			return wireError(err)
		}
		rows, err := ins.Rows(schema)
		if err != nil {
			return wireError(err)
		}
		result, err := exec.Insert(ctx, ins.Table, rows)
		if err != nil {
			s.zapLogger().Error("insert execution failed", zap.String("table", ins.Table), zap.Error(err))
			return writeWireError(err)
		}

		s.zapLogger().Info("pg insert",
			zap.String("table", ins.Table),
			zap.Int64("inserted", result.Count),
			zap.Duration("took", time.Since(start)))
		// PG's command tag for inserts: INSERT <oid> <rows>, oid always 0
		return writer.Complete(fmt.Sprintf("INSERT 0 %d", result.Count))
	}
}

// writeUpdateStatement is the UPDATE statement body: resolve the SET
// template, fetch every row the WHERE matches (whole rows — an upsert
// replaces them whole), apply the template, and upsert each one back.
func (s *Server) writeUpdateStatement(exec store.Executor, upd *pg.Update) wire.PreparedStatementFn {
	return func(ctx context.Context, writer wire.DataWriter, parameters []wire.Parameter) (err error) {
		defer func() {
			if p := recover(); p != nil {
				err = psqlerr.WithCode(fmt.Errorf("internal error while executing the statement: %v", p), codes.Internal)
			}
		}()
		if len(parameters) > 0 {
			return psqlerr.WithCode(errors.New("bind parameters are beyond the lyrebird write path: inline the literals"), codes.FeatureNotSupported)
		}

		start := time.Now()
		schema, err := exec.Schema(ctx, upd.Table)
		if err != nil {
			if errors.Is(err, store.ErrCollectionNotFound) {
				return psqlerr.WithCode(fmt.Errorf("relation %q does not exist", upd.Table), codes.UndefinedTable)
			}
			return wireError(err)
		}
		set, plan, err := upd.Lower(schema)
		if err != nil {
			return wireError(err)
		}
		plan.Limit = updateScanLimit

		result, err := exec.Search(ctx, upd.Table, plan)
		if err != nil {
			s.zapLogger().Error("update scan failed", zap.String("table", upd.Table), zap.Error(err))
			return wireError(err)
		}

		rows := make([]translate.WriteRow, 0, len(result.Hits))
		for _, hit := range result.Hits {
			row := translate.WriteRow{}
			for k, v := range hit.Source {
				row[k] = v
			}
			for k, v := range set {
				row[k] = v
			}
			rows = append(rows, row)
		}
		writeResult, err := exec.Upsert(ctx, upd.Table, rows)
		if err != nil {
			s.zapLogger().Error("update execution failed", zap.String("table", upd.Table), zap.Error(err))
			return writeWireError(err)
		}

		s.zapLogger().Info("pg update",
			zap.String("table", upd.Table),
			zap.Int64("scanned", result.Total),
			zap.Int64("updated", writeResult.Count),
			zap.Duration("took", time.Since(start)))
		return writer.Complete(fmt.Sprintf("UPDATE %d", writeResult.Count))
	}
}
