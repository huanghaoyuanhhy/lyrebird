package catalog

// PostgreSQL type OIDs the catalog speaks. These are the real PG numbers
// (catalog.typeinfo hands them to RowDescription and to pg_attribute), so
// clients decode cells without a custom type map.
const (
	OIDBool    uint32 = 16
	OIDBytea   uint32 = 17
	OIDChar    uint32 = 18
	OIDName    uint32 = 19
	OIDInt8    uint32 = 20
	OIDInt2    uint32 = 21
	OIDInt4    uint32 = 23
	OIDText    uint32 = 25
	OIDOID     uint32 = 26
	OIDJSON    uint32 = 114
	OIDFloat4  uint32 = 700
	OIDFloat8  uint32 = 701
	OIDBpChar  uint32 = 1042
	OIDVarChar uint32 = 1043
	OIDDate    uint32 = 1082
	OIDTime    uint32 = 1083
	OIDTs      uint32 = 1114
	OIDTsTz    uint32 = 1184
	OIDNumeric uint32 = 1700
	OIDUUID    uint32 = 2950
	OIDJSONB   uint32 = 3802

	// OIDOIDVector is pg_proc.proargtypes' pseudo-type.
	OIDOIDVector uint32 = 30

	// Array twins the catalog itself uses (conkey/confkey, indkey).
	OIDInt2Array  uint32 = 1005
	OIDInt4Array  uint32 = 1007
	OIDTextArray  uint32 = 1009
	OIDInt8Array  uint32 = 1016
	OIDBoolArray  uint32 = 1000
	OIDOIDArray   uint32 = 1028
	OIDFloatArray uint32 = 1022
)

// sysType is one row of the virtual pg_type table: the types catalog queries
// and RowDescription can name, with the columns pgjdbc's metadata reads.
// typlen -1 marks varlena storage, 0 the pseudo-types lyrebird names.
type sysType struct {
	oid      uint32
	name     string
	typlen   float64
	typtype  string // 'b' base, 'c' composite, 'a' autovacuum? — PG: b/e/c/p/s
	category string
	arrayOf  uint32 // typarray: 0 = none
}

// sysTypes is the static pg_type body. The set covers every type the
// catalog names plus the common array twins; anything else resolves to
// text-like rendering rather than a wrong claim.
var sysTypes = []sysType{
	{OIDBool, "bool", 1, "b", "B", OIDBoolArray},
	{OIDBytea, "bytea", -1, "b", "U", 0},
	{OIDChar, "char", 1, "b", "S", 0},
	{OIDName, "name", 64, "b", "S", 0},
	{OIDInt8, "int8", 8, "b", "N", OIDInt8Array},
	{OIDInt2, "int2", 2, "b", "N", OIDInt2Array},
	{OIDInt4, "int4", 4, "b", "N", OIDInt4Array},
	{OIDText, "text", -1, "b", "S", 0},
	{OIDOID, "oid", 4, "b", "N", OIDOIDArray},
	{OIDJSON, "json", -1, "b", "U", 0},
	{OIDFloat4, "float4", 4, "b", "N", OIDFloatArray},
	{OIDFloat8, "float8", 8, "b", "N", 0},
	{OIDBpChar, "bpchar", -1, "b", "S", 0},
	{OIDVarChar, "varchar", -1, "b", "S", 0},
	{OIDDate, "date", 4, "b", "D", 0},
	{OIDTime, "time", 8, "b", "D", 0},
	{OIDTs, "timestamp", 8, "b", "D", 0},
	{OIDTsTz, "timestamptz", 8, "b", "D", 0},
	{OIDNumeric, "numeric", -1, "b", "N", 0},
	{OIDUUID, "uuid", 16, "b", "U", 0},
	{OIDJSONB, "jsonb", -1, "b", "U", 0},
	{OIDOIDVector, "oidvector", -1, "b", "P", 0},
	// array twins (typlen -1, typtype 'b', category 'A')
	{OIDBoolArray, "_bool", -1, "b", "A", 0},
	{OIDInt2Array, "_int2", -1, "b", "A", 0},
	{OIDInt4Array, "_int4", -1, "b", "A", 0},
	{OIDTextArray, "_text", -1, "b", "A", 0},
	{OIDInt8Array, "_int8", -1, "b", "A", 0},
	{OIDOIDArray, "_oid", -1, "b", "A", 0},
	{OIDFloatArray, "_float4", -1, "b", "A", 0},
}

// oidByType indexes sysTypes by OID; built once at init.
var oidByType = func() map[uint32]sysType {
	m := make(map[uint32]sysType, len(sysTypes))
	for _, t := range sysTypes {
		m[t.oid] = t
	}
	return m
}()

func typName(oid uint32) string {
	if t, ok := oidByType[oid]; ok {
		return t.name
	}
	return "???"
}
