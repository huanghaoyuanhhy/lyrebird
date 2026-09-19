package translate

// WriteRow is one row bound for the store's write path: field name → value.
// It is the protocol-neutral insert/update payload, the write twin of Plan —
// both frontends (es, pg) produce it, store consumes it.
//
// The value vocabulary is deliberately narrow and lossless; the store's
// strict schema validation (docs/design.md, write path) accepts exactly:
//
//	string           — text/keyword values
//	json.Number      — numeric literals as written (no float round-trip, so
//	                   Int64 fields keep full 64-bit precision). Stored values
//	                   read back as int64/float64 are accepted too, so a
//	                   read-modify-write can upsert what it fetched.
//	bool             — boolean values
//	nil              — an explicit NULL (only legal on nullable fields)
//	[]float32        — an fp32 vector (the one vector family written today)
//	json.RawMessage  — a JSON field's value, bytes preserved as sent
//	[]byte           — the same, in the storage encoding reads return
//
// Anything else is a programmer error and fails validation; any protocol
// value that does not fit the field's schema type fails it too — the write
// path is strict, mismatching input is rejected, never coerced.
type WriteRow map[string]any
