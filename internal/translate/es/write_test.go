package es

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/huanghaoyuanhhy/lyrebird/internal/translate"
)

var docSchema = translate.MapSchema{
	"id": translate.TypeNumber, "name": translate.TypeKeyword,
	"price": translate.TypeNumber, "emb": translate.TypeVector,
	"j": translate.TypeUnknown,
}

func decode(t *testing.T, body string) translate.WriteRow {
	t.Helper()
	row, err := DecodeDocument([]byte(body), docSchema)
	if err != nil {
		t.Fatalf("DecodeDocument(%s): %v", body, err)
	}
	return row
}

func TestDecodeDocumentShapes(t *testing.T) {
	row := decode(t, `{
		"id": 9007199254740993,
		"name": "hi",
		"price": 1.5,
		"active": true,
		"note": null,
		"emb": [0.1, 0.2, 0.3],
		"j": {"a": [1, 2], "b": "x"},
		"tags": [1, 2]
	}`)

	if n, ok := row["id"].(json.Number); !ok || n.String() != "9007199254740993" {
		t.Fatalf("id = %#v, want exact json.Number beyond float53", row["id"])
	}
	if row["name"] != "hi" || row["active"] != true {
		t.Fatalf("scalars = %#v %#v", row["name"], row["active"])
	}
	if v, ok := row["note"]; !ok || v != nil {
		t.Fatalf("note = %#v, want present nil", v)
	}
	vec, ok := row["emb"].([]float32)
	if !ok || len(vec) != 3 || vec[2] != 0.3 {
		t.Fatalf("emb = %#v, want []float32{0.1 0.2 0.3}", row["emb"])
	}
	if _, ok := row["j"].(json.RawMessage); !ok {
		t.Fatalf("j = %#v, want raw JSON", row["j"])
	}
	if _, ok := row["tags"].(json.RawMessage); !ok {
		t.Fatalf("tags = %#v, want raw JSON (not a vector field)", row["tags"])
	}
}

func TestDecodeDocumentErrors(t *testing.T) {
	cases := []struct{ body, wantType string }{
		{`{bad`, "parsing_exception"},
		{`[1,2]`, "parsing_exception"}, // top level must be an object
		{`{"emb": [1, "x"]}`, "illegal_argument_exception"},
		{`{"emb": {"a": 1}}`, "illegal_argument_exception"},
	}
	for _, tc := range cases {
		_, err := DecodeDocument([]byte(tc.body), docSchema)
		if err == nil {
			t.Fatalf("%q: expected error", tc.body)
		}
		if !strings.Contains(err.Error(), tc.wantType) {
			t.Fatalf("%q: error %q, want %s", tc.body, err.Error(), tc.wantType)
		}
	}
}
