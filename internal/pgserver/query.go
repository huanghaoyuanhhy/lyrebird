package pgserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jeroenrinzema/psql-wire"
	"github.com/jeroenrinzema/psql-wire/codes"
	psqlerr "github.com/jeroenrinzema/psql-wire/errors"
	"go.uber.org/zap"

	"github.com/huanghaoyuanhhy/lyrebird/internal/catalog"
	"github.com/huanghaoyuanhhy/lyrebird/internal/store"
	"github.com/huanghaoyuanhhy/lyrebird/internal/translate"
	"github.com/huanghaoyuanhhy/lyrebird/internal/translate/pg"
)

// resultColumns resolves the statement's result shape: wire columns (name +
// PG type OID) paired with the translate type each cell is encoded by. The
// two slices stay aligned — Column i describes the cell that cellValue
// produces for types[i].
func resultColumns(sel *pg.Select, schema translate.Schema) (wire.Columns, []translate.FieldType) {
	names := sel.Columns(schema)
	columns := make(wire.Columns, len(names))
	types := make([]translate.FieldType, len(names))
	for i, name := range names {
		t := schema.FieldType(name)
		types[i] = t
		columns[i] = wire.Column{Name: name, Oid: typeOID(t)}
	}
	return columns, types
}

// typeOID maps a field's translate type onto the PostgreSQL type OID the
// wire describes cells with. The table lives in the catalog package so the
// catalog's pg_attribute rows and these RowDescriptions agree by
// construction: numbers render as float8 (the one numeric vocabulary that
// covers Int and Float fields alike), the text family as text, and unknown
// fields (JSON, arrays, vectors) as json.
func typeOID(t translate.FieldType) uint32 {
	return catalog.FieldTypeOID(t)
}

// search is the statement body: run the plan against the store and stream
// the hits as data rows, then report the row count the way PG does. exec is
// the connection's database view, resolved at parse time.
func (s *Server) search(exec store.Executor, sel *pg.Select, plan *translate.Plan, types []translate.FieldType) wire.PreparedStatementFn {
	return func(ctx context.Context, writer wire.DataWriter, parameters []wire.Parameter) (err error) {
		// psql-wire does not recover per connection: a panic here would take
		// the whole gateway down.
		defer func() {
			if p := recover(); p != nil {
				err = psqlerr.WithCode(fmt.Errorf("internal error while executing the statement: %v", p), codes.Internal)
			}
		}()

		if len(parameters) > 0 {
			return psqlerr.WithCode(errors.New("bind parameters are beyond the lyrebird SELECT subset"), codes.FeatureNotSupported)
		}

		start := time.Now()
		result, err := exec.Search(ctx, sel.Table, plan)
		if err != nil {
			s.zapLogger().Error("query execution failed", zap.String("table", sel.Table), zap.Error(err))
			return wireError(err)
		}

		columns := writer.Columns()
		for _, hit := range result.Hits {
			row := make([]any, len(types))
			for i := range types {
				row[i] = cellValue(types[i], hit.Source[columns[i].Name])
			}
			if err := writer.Row(row); err != nil {
				return err
			}
		}

		s.zapLogger().Info("pg query",
			zap.String("table", sel.Table),
			zap.Int64("total", result.Total),
			zap.Uint32("returned", writer.Written()),
			zap.Duration("took", time.Since(start)))
		return writer.Complete(fmt.Sprintf("SELECT %d", writer.Written()))
	}
}

// cellValue shapes one stored cell for the wire: a Go type matching the
// column's OID (pgtype encodes from those), nil for absent/null cells.
// Numbers narrow to float64 — the float8 OID every numeric column declares.
func cellValue(t translate.FieldType, v any) any {
	if v == nil {
		return nil
	}
	switch t {
	case translate.TypeNumber:
		switch x := v.(type) {
		case float64:
			return x
		case float32:
			return float64(x)
		case int64:
			return float64(x)
		case int32:
			return float64(x)
		case int16:
			return float64(x)
		case int8:
			return float64(x)
		}
	case translate.TypeBool:
		if b, ok := v.(bool); ok {
			return b
		}
	case translate.TypeKeyword, translate.TypeText, translate.TypeDate:
		if str, ok := v.(string); ok {
			return str
		}
		return fmt.Sprintf("%v", v)
	default:
		// Unknown fields (JSON, arrays, vectors) ride the json OID as text.
		// SDK JSON cells arrive as raw JSON bytes — pass them through.
		switch x := v.(type) {
		case []byte:
			return string(x)
		case string:
			return x
		}
		if encoded, err := json.Marshal(v); err == nil {
			return string(encoded)
		}
	}
	return fmt.Sprintf("%v", v)
}
