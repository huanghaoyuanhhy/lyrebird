package catalog

// Coerce shapes one evaluated cell for the wire so its Go type matches what
// the column OID's pgtype codec encodes. The engine's value model is
// deliberately narrow (string / float64 / bool / []any / nil); this is the
// single place that widens it per column.
func Coerce(oid uint32, v any) any {
	if v == nil {
		return nil
	}
	switch oid {
	case OIDBool:
		b, ok := v.(bool)
		if ok {
			return b
		}
		return nil
	case OIDInt2:
		if f, ok := toFloat(v); ok {
			return int16(f)
		}
	case OIDInt4:
		if f, ok := toFloat(v); ok {
			return int32(f)
		}
	case OIDInt8:
		if f, ok := toFloat(v); ok {
			return int64(f)
		}
	case OIDOID:
		if f, ok := toFloat(v); ok {
			return uint32(f)
		}
	case OIDFloat4, OIDFloat8:
		if f, ok := toFloat(v); ok {
			return f
		}
	case OIDInt2Array:
		if arr, ok := v.([]any); ok {
			return coerceInts[int16](arr)
		}
	case OIDInt4Array:
		if arr, ok := v.([]any); ok {
			return coerceInts[int32](arr)
		}
	case OIDInt8Array:
		if arr, ok := v.([]any); ok {
			return coerceInts[int64](arr)
		}
	case OIDBoolArray:
		if arr, ok := v.([]any); ok {
			out := make([]bool, 0, len(arr))
			for _, e := range arr {
				b, _ := e.(bool)
				out = append(out, b)
			}
			return out
		}
	default:
		// the text family (name/char/varchar/text/json/jsonb/timestamptz…)
		// and unknown OIDs render as text
		s, _ := toString(v)
		return s
	}
	s, _ := toString(v)
	return s
}

func coerceInts[T int16 | int32 | int64](arr []any) []T {
	out := make([]T, 0, len(arr))
	for _, e := range arr {
		f, ok := toFloat(e)
		if !ok {
			continue
		}
		out = append(out, T(f))
	}
	return out
}
