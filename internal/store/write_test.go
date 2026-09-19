package store

import (
	"encoding/json"
	"testing"

	"github.com/huanghaoyuanhhy/lyrebird/internal/catalog"
	"github.com/huanghaoyuanhhy/lyrebird/internal/translate"
)

// scalarMeta is the validation test's fixture: every writable storage type
// the strict path knows, plus the shapes it must refuse.
var scalarMeta = catalog.CollectionMeta{
	Name: "items",
	Fields: []catalog.FieldMeta{
		{Name: "id", Type: translate.TypeNumber, PrimaryKey: true, Native: "int64"},
		{Name: "name", Type: translate.TypeKeyword, Native: "string", MaxLength: 4},
		{Name: "price", Type: translate.TypeNumber, Native: "float64"},
		{Name: "qty", Type: translate.TypeNumber, Native: "int32"},
		{Name: "active", Type: translate.TypeBool, Native: "bool"},
		{Name: "note", Type: translate.TypeKeyword, Native: "string", Nullable: true},
		{Name: "emb", Type: translate.TypeVector, Native: "float_vector", Dim: 2},
		{Name: "j", Type: translate.TypeUnknown, Native: "json", Nullable: true},
		{Name: "tags", Type: translate.TypeUnknown, Native: "array", Nullable: true},
	},
	FunctionOutputs: []string{"sparse"},
}

// goodRow satisfies the fixture schema completely.
func goodRow() translate.WriteRow {
	return translate.WriteRow{
		"id":     json.Number("7"),
		"name":   "hi",
		"price":  json.Number("1.5"),
		"qty":    json.Number("3"),
		"active": true,
		"note":   nil,
		"emb":    []float32{0.1, 0.2},
		"j":      json.RawMessage(`{"a":1}`),
	}
}

func TestWriteColumnsAcceptsMatchingRows(t *testing.T) {
	rows := []translate.WriteRow{
		goodRow(),
		// second row: nulls and storage-read encodings mixed in
		{
			"id":     int64(8),
			"name":   "ok",
			"price":  2.5, // read-back encoding
			"qty":    json.Number("4"),
			"active": false,
			// note omitted (nullable)
			"emb": []float32{1, 2},
			"j":   []byte(`"text"`),
		},
	}
	cols, err := writeColumns(scalarMeta, rows)
	if err != nil {
		t.Fatalf("writeColumns: %v", err)
	}
	// one column per writable non-autoID field: id, name, price, qty,
	// active, note, emb, j — tags (array) has no write encoding and no row
	// supplied it, so it rides as an omitted column.
	if len(cols) != 8 {
		t.Fatalf("got %d columns", len(cols))
	}
}

func TestWriteColumnsRejectsMismatchedInput(t *testing.T) {
	cases := []struct {
		name  string
		row   translate.WriteRow
		class WriteErrClass
	}{
		{"unknown field", merge(goodRow(), map[string]any{"nope": "x"}), ClassUnknownField},
		{"function output", merge(goodRow(), map[string]any{"sparse": "x"}), ClassFunctionField},
		{"autoID not set here", goodRow(), ""},
		{"string into int64", merge(goodRow(), map[string]any{"id": "seven"}), ClassType},
		{"fraction into int64", merge(goodRow(), map[string]any{"qty": json.Number("3.5")}), ClassType},
		{"int32 overflow", merge(goodRow(), map[string]any{"qty": json.Number("3000000000")}), ClassRange},
		{"length overflow", merge(goodRow(), map[string]any{"name": "toolong"}), ClassLength},
		{"bool mismatch", merge(goodRow(), map[string]any{"active": "yes"}), ClassType},
		{"number into bool", merge(goodRow(), map[string]any{"active": json.Number("1")}), ClassType},
		{"dim mismatch", merge(goodRow(), map[string]any{"emb": []float32{1}}), ClassDim},
		{"string into vector", merge(goodRow(), map[string]any{"emb": "[1,2]"}), ClassType},
		{"array field", merge(goodRow(), map[string]any{"tags": json.RawMessage(`[1]`)}), ClassUnsupported},
		{"invalid json literal", merge(goodRow(), map[string]any{"j": "{nope}"}), ClassType},
		{"null into required", merge(goodRow(), map[string]any{"name": nil}), ClassNull},
		{"number into string", merge(goodRow(), map[string]any{"name": json.Number("5")}), ClassType},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.class == "" {
				t.Skip("placeholder row")
			}
			_, err := writeColumns(scalarMeta, []translate.WriteRow{tc.row})
			var we *WriteError
			if !asWriteError(err, &we) {
				t.Fatalf("expected WriteError, got %v", err)
			}
			if we.Class != tc.class {
				t.Fatalf("class = %s, want %s (reason: %s)", we.Class, tc.class, we.Reason)
			}
		})
	}
}

func TestWriteColumnsRejectsMissingRequired(t *testing.T) {
	row := goodRow()
	delete(row, "id")
	_, err := writeColumns(scalarMeta, []translate.WriteRow{row})
	var we *WriteError
	if !asWriteError(err, &we) || we.Class != ClassMissing {
		t.Fatalf("expected ClassMissing, got %v", err)
	}

	row = goodRow()
	delete(row, "name")
	_, err = writeColumns(scalarMeta, []translate.WriteRow{row})
	if !asWriteError(err, &we) || we.Class != ClassMissing {
		t.Fatalf("expected ClassMissing for non-nullable name, got %v", err)
	}
}

func TestWriteColumnsAutoIDFieldsAreSkippedAndFilled(t *testing.T) {
	meta := catalog.CollectionMeta{
		Name: "gen",
		Fields: []catalog.FieldMeta{
			{Name: "id", Type: translate.TypeNumber, PrimaryKey: true, AutoID: true, Native: "int64"},
			{Name: "v", Type: translate.TypeKeyword, Native: "string"},
		},
	}
	cols, err := writeColumns(meta, []translate.WriteRow{{"v": "x"}})
	if err != nil {
		t.Fatalf("writeColumns: %v", err)
	}
	if len(cols) != 1 {
		t.Fatalf("expected only the non-autoID column, got %d", len(cols))
	}

	// supplying a value for the auto key is rejected outright
	_, err = writeColumns(meta, []translate.WriteRow{{"id": json.Number("1"), "v": "x"}})
	var we *WriteError
	if !asWriteError(err, &we) || we.Class != ClassAutoID {
		t.Fatalf("expected ClassAutoID, got %v", err)
	}
}

func TestWriteColumnsIntegerTextForms(t *testing.T) {
	cases := []struct {
		text string
		ok   bool
	}{
		{"3", true}, {"3.0", true}, {" 3", false}, {"3.5", false}, {"1e2", true},
	}
	for _, tc := range cases {
		_, err := writeColumns(scalarMeta, []translate.WriteRow{merge(goodRow(), map[string]any{"qty": json.Number(tc.text)})})
		if tc.ok && err != nil {
			t.Fatalf("qty %q: unexpected error %v", tc.text, err)
		}
		if !tc.ok {
			var we *WriteError
			if !asWriteError(err, &we) || we.Class != ClassType {
				t.Fatalf("qty %q: expected ClassType, got %v", tc.text, err)
			}
		}
	}
}

func asWriteError(err error, target **WriteError) bool {
	if we, ok := err.(*WriteError); ok {
		*target = we
		return true
	}
	return false
}

func merge(base translate.WriteRow, overlay map[string]any) translate.WriteRow {
	out := translate.WriteRow{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range overlay {
		out[k] = v
	}
	return out
}
