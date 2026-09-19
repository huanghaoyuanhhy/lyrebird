package store

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"

	"github.com/milvus-io/milvus/client/v3/column"
	"github.com/milvus-io/milvus/client/v3/entity"
	"github.com/milvus-io/milvus/client/v3/milvusclient"

	"github.com/huanghaoyuanhhy/lyrebird/internal/catalog"
	"github.com/huanghaoyuanhhy/lyrebird/internal/translate"
)

// WriteResult is a completed write: how many rows the store accepted and —
// when the primary key is auto-generated — the ids it assigned. IDs are
// rendered as strings (the form ES clients see as _id); they are empty
// when the caller supplied the primary keys.
type WriteResult struct {
	Count int64
	IDs   []string
}

// WriteErrClass classifies a strict-schema write rejection. The classes are
// the shared meaning; each protocol shell renders them in its native
// vocabulary (SQLSTATE codes on the pg side, ES exception types on the es
// side). See docs/design.md, write path.
type WriteErrClass string

const (
	// ClassUnknownField: the document names a field the schema does not have
	// (ES strict mapping; dynamic fields are refused).
	ClassUnknownField WriteErrClass = "unknown_field"
	// ClassFunctionField: the field is a schema function's output (e.g. the
	// sparse vector a BM25 function owns) — the store computes it.
	ClassFunctionField WriteErrClass = "function_field"
	// ClassAutoID: a value was supplied for an auto-generated primary key.
	ClassAutoID WriteErrClass = "autoid"
	// ClassMissing: a row omits a non-nullable field.
	ClassMissing WriteErrClass = "missing"
	// ClassNull: an explicit NULL targets a non-nullable field.
	ClassNull WriteErrClass = "null"
	// ClassType: the value's shape does not match the field's type.
	ClassType WriteErrClass = "type"
	// ClassRange: a numeric value is outside the field's type range.
	ClassRange WriteErrClass = "range"
	// ClassLength: a string exceeds the field's max_length.
	ClassLength WriteErrClass = "length"
	// ClassDim: a vector's dimension differs from the field's dim.
	ClassDim WriteErrClass = "dim"
	// ClassUnsupported: the field's storage type has no write encoding yet
	// (arrays, non-fp32 vector families, sparse, timestamptz).
	ClassUnsupported WriteErrClass = "unsupported"
)

// WriteError is one strict-schema rejection: the classification protocol
// shells map, the field it concerns, and a human-readable reason.
type WriteError struct {
	Class  WriteErrClass
	Field  string
	Reason string
}

func (e *WriteError) Error() string { return e.Reason }

func writeErrf(class WriteErrClass, field, format string, args ...any) *WriteError {
	return &WriteError{Class: class, Field: field, Reason: fmt.Sprintf(format, args...)}
}

// Insert writes new rows. Every value is validated against the live
// collection schema first: an input that does not match is rejected before
// anything is sent (the strict write path — no coercion, no dynamic
// fields). Auto-generated primary keys are filled by Milvus and returned in
// WriteResult.IDs.
func (e *MilvusExecutor) Insert(ctx context.Context, collection string, rows []translate.WriteRow) (WriteResult, error) {
	return e.write(ctx, collection, rows, false)
}

// Upsert writes rows by primary key, replacing the stored row whole: fields
// absent from the incoming row keep nothing from the old one. Partial
// modification is a read-modify-write in the callers (ES _update, PG
// UPDATE), which fetch, merge, and upsert here.
func (e *MilvusExecutor) Upsert(ctx context.Context, collection string, rows []translate.WriteRow) (WriteResult, error) {
	return e.write(ctx, collection, rows, true)
}

// write is the shared body of Insert and Upsert: validate strictly, build
// one column per non-autoID field, send.
func (e *MilvusExecutor) write(ctx context.Context, collection string, rows []translate.WriteRow, upsert bool) (WriteResult, error) {
	if len(rows) == 0 {
		return WriteResult{}, nil
	}
	meta, err := e.Describe(ctx, collection)
	if err != nil {
		return WriteResult{}, err
	}
	cols, err := writeColumns(meta, rows)
	if err != nil {
		return WriteResult{}, err
	}

	opt := milvusclient.NewColumnBasedInsertOption(collection, cols...)
	var ids column.Column
	if upsert {
		res, err := e.cli.Upsert(ctx, opt)
		if err != nil {
			return WriteResult{}, fmt.Errorf("upsert into collection %q: %w", collection, err)
		}
		ids = res.IDs
	} else {
		res, err := e.cli.Insert(ctx, opt)
		if err != nil {
			return WriteResult{}, fmt.Errorf("insert into collection %q: %w", collection, err)
		}
		ids = res.IDs
	}
	return WriteResult{Count: int64(len(rows)), IDs: idsToStrings(ids)}, nil
}

// Describe implements Executor: the collection's catalog view, the strict
// write path's schema source. It answers with the same metadata the
// introspection surface serves, so a write is checked against exactly what
// clients are told the schema is.
func (e *MilvusExecutor) Describe(ctx context.Context, collection string) (catalog.CollectionMeta, error) {
	coll, err := e.describe(ctx, collection)
	if err != nil {
		return catalog.CollectionMeta{}, err
	}
	return catalogMeta(coll), nil
}

// ValidateWriteRows checks rows against the collection metadata without
// touching the store: the strict-schema rejection the real executor makes
// before anything is sent. Test doubles and future callers that only need
// the verdict (not the built columns) share it, so a fake executor rejects
// exactly what the real one would.
func ValidateWriteRows(meta catalog.CollectionMeta, rows []translate.WriteRow) error {
	_, _, err := validateWriteCells(meta, rows)
	return err
}

// writeColumns validates rows and builds the SDK columns: strict schema
// checking lives here, in the one layer that knows both the storage types
// and the write value vocabulary.
func writeColumns(meta catalog.CollectionMeta, rows []translate.WriteRow) ([]column.Column, error) {
	cells, present, err := validateWriteCells(meta, rows)
	if err != nil {
		return nil, err
	}

	cols := make([]column.Column, 0, len(meta.Fields))
	for i, f := range meta.Fields {
		if f.AutoID {
			continue // Milvus fills it; the column is not sent at all
		}
		if !writableNative(f.Native) {
			// No row can have supplied this type (writeValue would have
			// rejected it); a nullable unwritable field rides as an omitted
			// column instead of blocking the write.
			continue
		}
		col, err := columnOf(f, cells[i], present[i])
		if err != nil {
			return nil, err
		}
		cols = append(cols, col)
	}
	return cols, nil
}

// validateWriteCells runs the strict-schema checks and returns the
// normalized cells: cells[i][j] is row j's value for field i, present marks
// the rows that supplied each field.
func validateWriteCells(meta catalog.CollectionMeta, rows []translate.WriteRow) ([][]any, [][]bool, error) {
	byName := make(map[string]int, len(meta.Fields))
	for i, f := range meta.Fields {
		byName[f.Name] = i
	}
	funcOutput := make(map[string]bool, len(meta.FunctionOutputs))
	for _, name := range meta.FunctionOutputs {
		funcOutput[name] = true
	}

	// Per-field cell lists in schema order: cells[i][j] is row j's value for
	// field i. Every field gets a full-length list — a row omitting a
	// nullable field contributes a null cell — so all columns line up with
	// the row order and no per-row column surgery is needed.
	cells := make([][]any, len(meta.Fields))
	present := make([][]bool, len(meta.Fields))
	for i := range cells {
		cells[i] = make([]any, len(rows))
		present[i] = make([]bool, len(rows))
	}

	for r, row := range rows {
		for name, v := range row {
			i, known := byName[name]
			switch {
			case funcOutput[name]:
				return nil, nil, writeErrf(ClassFunctionField, name,
					"field [%s] is computed by a schema function and cannot be written", name)
			case !known:
				return nil, nil, writeErrf(ClassUnknownField, name,
					"field [%s] is not in the schema of %q (strict mapping: unknown fields are rejected)", name, meta.Name)
			}
			f := meta.Fields[i]
			if f.AutoID {
				return nil, nil, writeErrf(ClassAutoID, name,
					"field [%s] is auto-generated; writers must not supply it", name)
			}
			cell, err := writeValue(f, v)
			if err != nil {
				return nil, nil, err
			}
			cells[i][r], present[i][r] = cell, true
		}
	}

	// A row that omits a field leaves the cell unset: legal on nullable
	// fields, a strict-schema violation otherwise.
	for i, f := range meta.Fields {
		if f.AutoID {
			continue
		}
		for r, ok := range present[i] {
			if !ok && !f.Nullable {
				return nil, nil, writeErrf(ClassMissing, f.Name,
					"field [%s] is not nullable and row %d does not supply it", f.Name, r+1)
			}
		}
	}
	return cells, present, nil
}

// writableNative reports whether a storage type has a write encoding. The
// set mirrors writeValue's accepting branches.
func writableNative(native string) bool {
	switch native {
	case "string", "int8", "int16", "int32", "int64",
		"float32", "float64", "bool", "json", "float_vector":
		return true
	default:
		return false
	}
}

// writeValue validates one cell against its field's storage type, returning
// the normalized Go value the column builder consumes. Acceptance follows
// the write value vocabulary (translate.WriteRow) widened only by the
// storage encodings reads return, so a read-modify-write can upsert exactly
// what it fetched.
func writeValue(f catalog.FieldMeta, v any) (any, error) {
	if v == nil {
		if !f.Nullable {
			return nil, writeErrf(ClassNull, f.Name,
				"field [%s] is not nullable; NULL is rejected", f.Name)
		}
		return nil, nil
	}
	switch f.Native {
	case "string":
		s, ok := v.(string)
		if !ok {
			return nil, typeErrf(f, v, "a string")
		}
		if f.MaxLength > 0 && len(s) > f.MaxLength {
			return nil, writeErrf(ClassLength, f.Name,
				"string of %d bytes exceeds field [%s] max_length %d", len(s), f.Name, f.MaxLength)
		}
		return s, nil

	case "int8", "int16", "int32", "int64":
		n, ok := toInt64(v)
		if !ok {
			return nil, typeErrf(f, v, "an integer")
		}
		lo, hi := intBoundaries(f.Native)
		if n < lo || n > hi {
			return nil, writeErrf(ClassRange, f.Name,
				"value %d is out of range for field [%s] of type %s", n, f.Name, f.Native)
		}
		switch f.Native {
		case "int8":
			return int8(n), nil
		case "int16":
			return int16(n), nil
		case "int32":
			return int32(n), nil
		default:
			return n, nil
		}

	case "float32", "float64":
		fl, ok := toFloat64(v)
		if !ok {
			return nil, typeErrf(f, v, "a number")
		}
		if math.IsNaN(fl) || math.IsInf(fl, 0) {
			return nil, writeErrf(ClassRange, f.Name,
				"NaN/Inf is not a writable value for field [%s]", f.Name)
		}
		if f.Native == "float32" && (fl > math.MaxFloat32 || fl < -math.MaxFloat32) {
			return nil, writeErrf(ClassRange, f.Name,
				"value %v is out of range for field [%s] of type float32", v, f.Name)
		}
		if f.Native == "float32" {
			return float32(fl), nil
		}
		return fl, nil

	case "bool":
		b, ok := v.(bool)
		if !ok {
			return nil, typeErrf(f, v, "a boolean")
		}
		return b, nil

	case "json":
		// JSON fields accept any JSON value. The two byte forms come from
		// the wire (raw bytes preserved) and storage reads; a string is the
		// one text encoding SQL has for a JSON value, and it must parse.
		switch x := v.(type) {
		case json.RawMessage:
			return []byte(x), nil
		case []byte:
			return x, nil
		case string:
			// SQL's only JSON encoding is a text literal; it must parse.
			if !isValidJSON(x) {
				return nil, typeErrf(f, v, "valid JSON")
			}
			return []byte(x), nil
		default:
			return nil, typeErrf(f, v, "a JSON value")
		}

	case "float_vector":
		// reads return the SDK's named slice type; normalize it so callers
		// can hand back exactly what they fetched
		if fv, isFv := v.(entity.FloatVector); isFv {
			v = []float32(fv)
		}
		vec, ok := v.([]float32)
		if !ok {
			return nil, typeErrf(f, v, "an array of numbers")
		}
		if f.Dim > 0 && len(vec) != f.Dim {
			return nil, writeErrf(ClassDim, f.Name,
				"vector for field [%s] has %d dimensions, the schema requires %d", f.Name, len(vec), f.Dim)
		}
		for _, x := range vec {
			if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
				return nil, writeErrf(ClassRange, f.Name,
					"vector for field [%s] contains NaN/Inf", f.Name)
			}
		}
		return vec, nil

	default:
		return nil, writeErrf(ClassUnsupported, f.Name,
			"field [%s] of storage type %s has no write encoding yet", f.Name, f.Native)
	}
}

func typeErrf(f catalog.FieldMeta, v any, want string) *WriteError {
	return writeErrf(ClassType, f.Name,
		"invalid value %s for field [%s] of type %s: expected %s", renderValue(v), f.Name, f.Native, want)
}

func renderValue(v any) string {
	switch x := v.(type) {
	case string:
		return strconv.Quote(x)
	case json.Number:
		return x.String()
	case []float32:
		return "a vector"
	case entity.FloatVector:
		return "a vector"
	default:
		return fmt.Sprintf("%v", x)
	}
}

// toInt64 reads an integer from the write vocabulary: a json.Number whose
// text is an integer (the fast path), any Go integer, or a float64 that is
// mathematically integral (a value read back from a float column). "3.0"
// passes — it is an integer spelled with a decimal point — while "3.5" and
// out-of-range texts fail, keeping Int64 fields free of silent truncation.
func toInt64(v any) (int64, bool) {
	switch x := v.(type) {
	case json.Number:
		if n, err := x.Int64(); err == nil {
			return n, true
		}
		f, err := x.Float64()
		if err != nil || math.IsInf(f, 0) || math.Trunc(f) != f || math.Abs(f) >= 1<<53 {
			return 0, false
		}
		return int64(f), true
	case int64:
		return x, true
	case int:
		return int64(x), true
	case int32:
		return int64(x), true
	case float64:
		if math.Trunc(x) != x || math.Abs(x) >= 1<<53 {
			return 0, false
		}
		return int64(x), true
	default:
		return 0, false
	}
}

// toFloat64 reads a float from the write vocabulary: a json.Number or any
// Go numeric.
func toFloat64(v any) (float64, bool) {
	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int64:
		return float64(x), true
	case int:
		return float64(x), true
	default:
		return 0, false
	}
}

// intBoundaries returns the inclusive range of an int storage type name.
func intBoundaries(native string) (int64, int64) {
	switch native {
	case "int8":
		return math.MinInt8, math.MaxInt8
	case "int16":
		return math.MinInt16, math.MaxInt16
	case "int32":
		return math.MinInt32, math.MaxInt32
	default:
		return math.MinInt64, math.MaxInt64
	}
}

func isValidJSON(s string) bool {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return false
	}
	// trailing garbage ("{} {}") is not a JSON value
	if _, err := dec.Token(); err != io.EOF {
		return false
	}
	return true
}

// columnOf builds one SDK column from validated cells. Every column goes
// through the nullable constructor so null cells (rows that omitted the
// field, or explicit NULLs) carry an explicit validity mask instead of a
// silent zero value.
func columnOf(f catalog.FieldMeta, cells []any, present []bool) (column.Column, error) {
	if !f.Nullable {
		// A non-nullable field always has a value for every row (the
		// validation guarantees it), and the server rejects a validity
		// mask on a non-nullable column — build it plain.
		return plainColumnOf(f, cells)
	}
	// The SDK's nullable columns are compact: values holds only the
	// non-null cells, valid marks which rows they belong to. An omitted
	// field and an explicit NULL are both invalid cells.
	valid := make([]bool, len(present))
	for i, c := range cells {
		valid[i] = present[i] && c != nil
	}
	switch f.Native {
	case "string":
		vals := compact(cells, func(c any) string { return c.(string) })
		return column.NewNullableColumnVarChar(f.Name, vals, valid)
	case "int8":
		vals := compact(cells, func(c any) int8 { return c.(int8) })
		return column.NewNullableColumnInt8(f.Name, vals, valid)
	case "int16":
		vals := compact(cells, func(c any) int16 { return c.(int16) })
		return column.NewNullableColumnInt16(f.Name, vals, valid)
	case "int32":
		vals := compact(cells, func(c any) int32 { return c.(int32) })
		return column.NewNullableColumnInt32(f.Name, vals, valid)
	case "int64":
		vals := compact(cells, func(c any) int64 { return c.(int64) })
		return column.NewNullableColumnInt64(f.Name, vals, valid)
	case "float32":
		vals := compact(cells, func(c any) float32 { return c.(float32) })
		return column.NewNullableColumnFloat(f.Name, vals, valid)
	case "float64":
		vals := compact(cells, func(c any) float64 { return c.(float64) })
		return column.NewNullableColumnDouble(f.Name, vals, valid)
	case "bool":
		vals := compact(cells, func(c any) bool { return c.(bool) })
		return column.NewNullableColumnBool(f.Name, vals, valid)
	case "json":
		vals := compact(cells, func(c any) []byte { return c.([]byte) })
		return column.NewNullableColumnJSONBytes(f.Name, vals, valid)
	case "float_vector":
		vals := compact(cells, func(c any) []float32 { return c.([]float32) })
		dim := f.Dim
		if dim == 0 && len(vals) > 0 && vals[0] != nil {
			dim = len(vals[0])
		}
		return column.NewNullableColumnFloatVector(f.Name, dim, vals, valid)
	default:
		// unreachable: writeValue already rejected unwritable types
		return nil, writeErrf(ClassUnsupported, f.Name,
			"field [%s] of storage type %s has no write encoding yet", f.Name, f.Native)
	}
}

// compact projects the non-null cells, in row order, into a typed slice —
// the nullable columns' compressed value form.
func compact[T any](cells []any, cast func(any) T) []T {
	vals := make([]T, 0, len(cells))
	for _, c := range cells {
		if c != nil {
			vals = append(vals, cast(c))
		}
	}
	return vals
}

// plainColumnOf builds a non-nullable column: every cell present, no
// validity mask.
func plainColumnOf(f catalog.FieldMeta, cells []any) (column.Column, error) {
	switch f.Native {
	case "string":
		return column.NewColumnVarChar(f.Name, compact(cells, func(c any) string { return c.(string) })), nil
	case "int8":
		return column.NewColumnInt8(f.Name, compact(cells, func(c any) int8 { return c.(int8) })), nil
	case "int16":
		return column.NewColumnInt16(f.Name, compact(cells, func(c any) int16 { return c.(int16) })), nil
	case "int32":
		return column.NewColumnInt32(f.Name, compact(cells, func(c any) int32 { return c.(int32) })), nil
	case "int64":
		return column.NewColumnInt64(f.Name, compact(cells, func(c any) int64 { return c.(int64) })), nil
	case "float32":
		return column.NewColumnFloat(f.Name, compact(cells, func(c any) float32 { return c.(float32) })), nil
	case "float64":
		return column.NewColumnDouble(f.Name, compact(cells, func(c any) float64 { return c.(float64) })), nil
	case "bool":
		return column.NewColumnBool(f.Name, compact(cells, func(c any) bool { return c.(bool) })), nil
	case "json":
		return column.NewColumnJSONBytes(f.Name, compact(cells, func(c any) []byte { return c.([]byte) })), nil
	case "float_vector":
		vals := compact(cells, func(c any) []float32 { return c.([]float32) })
		dim := f.Dim
		if dim == 0 && len(vals) > 0 && vals[0] != nil {
			dim = len(vals[0])
		}
		return column.NewColumnFloatVector(f.Name, dim, vals), nil
	default:
		// unreachable: writeValue already rejected unwritable types
		return nil, writeErrf(ClassUnsupported, f.Name,
			"field [%s] of storage type %s has no write encoding yet", f.Name, f.Native)
	}
}

// idsToStrings renders the store-assigned primary keys as the strings
// clients see (the same rendering hits use for _id); a nil column (caller
// supplied every pk) returns nil.
func idsToStrings(ids column.Column) []string {
	if ids == nil {
		return nil
	}
	out := make([]string, ids.Len())
	for i := range out {
		v, err := ids.Get(i)
		if err != nil {
			continue
		}
		switch x := v.(type) {
		case string:
			out[i] = x
		case int64:
			out[i] = strconv.FormatInt(x, 10)
		default:
			out[i] = fmt.Sprintf("%v", x)
		}
	}
	return out
}
