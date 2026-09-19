package es

import (
	"bytes"
	"encoding/json"

	"github.com/huanghaoyuanhhy/lyrebird/internal/translate"
)

// DecodeDocument turns an ES index/update document body into one write row
// against the schema: strings, booleans and null lower directly, numbers
// keep their written form (json.Number, so Int64 fields keep full
// precision), a vector field's numeric array parses to fp32s, and any other
// value rides as raw JSON for the JSON fields.
//
// This is shaping, not validation: whether a value fits its field is the
// store's strict-schema call, so a string sent to a number field fails
// there with the shared unknown/type classifications, never coerced here.
func DecodeDocument(body []byte, schema translate.Schema) (translate.WriteRow, error) {
	if schema == nil {
		schema = translate.MapSchema{}
	}
	top, err := decodeBody(body)
	if err != nil {
		return nil, err
	}
	row := make(translate.WriteRow, len(top))
	for name, raw := range top {
		v, err := decodeWriteValue(name, raw, schema)
		if err != nil {
			return nil, err
		}
		row[name] = v
	}
	return row, nil
}

// decodeWriteValue converts one field's raw JSON onto the write value
// vocabulary.
func decodeWriteValue(name string, raw json.RawMessage, schema translate.Schema) (any, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, parseErrf("field [%s] has an empty value", name)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, parseErrf("field [%s] is not valid JSON: %v", name, err)
	}
	switch x := v.(type) {
	case nil:
		return nil, nil
	case string:
		return x, nil
	case bool:
		return x, nil
	case json.Number:
		return x, nil
	case []any:
		// a vector field's array lowers to fp32s here, where the field type
		// is known; every other array rides as raw JSON
		if schema.FieldType(name) != translate.TypeVector {
			return raw, nil
		}
		vec := make([]float32, 0, len(x))
		for i, item := range x {
			n, ok := item.(json.Number)
			if !ok {
				return nil, illegalErrf("failed to parse field [%s] of type [dense_vector]: entry %d is not a number", name, i+1)
			}
			f, err := n.Float64()
			if err != nil {
				return nil, illegalErrf("failed to parse field [%s] of type [dense_vector]: entry %d ([%s]) is not a number", name, i+1, n.String())
			}
			vec = append(vec, float32(f))
		}
		return vec, nil
	default:
		// a vector field only takes the numeric array handled above
		if schema.FieldType(name) == translate.TypeVector {
			return nil, illegalErrf("failed to parse field [%s] of type [dense_vector]: expected an array of numbers", name)
		}
		// objects and anything else ride as the raw bytes they came with
		return raw, nil
	}
}
