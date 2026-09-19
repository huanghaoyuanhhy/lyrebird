package catalog

import (
	"github.com/huanghaoyuanhhy/lyrebird/internal/translate"
)

// FieldTypeOID maps a translate field type onto the PostgreSQL type OID the
// wire describes it with. This is the one numeric-vocabulary table: the
// catalog's pg_attribute rows and pgserver's RowDescription both read it, so
// a column never describes itself as one type and delivers another. The
// numeric family is float8 and the JSON/array/vector family is json — the
// settled pg-wire decision (docs/design.md) — and dates stay text (epoch
// millis) until a later decision re-types them.
func FieldTypeOID(t translate.FieldType) uint32 {
	switch t {
	case translate.TypeNumber:
		return OIDFloat8
	case translate.TypeBool:
		return OIDBool
	case translate.TypeKeyword, translate.TypeText, translate.TypeDate:
		return OIDText
	default:
		return OIDJSON
	}
}

// FieldTypeName is the type name catalog queries report for a field, the
// human-readable twin of FieldTypeOID's OID choice.
func FieldTypeName(t translate.FieldType) string {
	return typName(FieldTypeOID(t))
}
