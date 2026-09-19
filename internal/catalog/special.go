package catalog

// Named special cases: client queries whose shape outgrows the general
// engine — derived tables, window functions, table functions, correlated
// subqueries — run through dedicated row generators. Every case here is a
// pgjdbc DatabaseMetaData method (DataGrip's JDBC introspection level), and
// the test fixtures are those methods' real SQL text, taken verbatim from
// PgDatabaseMetaData.java. A case must match conservatively: a false
// positive silently replaces the general engine's answer.
//
// The runners consume the client's bind parameters positionally, located by
// the filter each one feeds (scanParamSlots): pgjdbc appends "AND col LIKE ?"
// fragments conditionally, so the query text itself says which parameter is
// which.

import (
	"context"
	"regexp"
	"strings"
)

// specialCase recognizes one known client query shape.
type specialCase struct {
	// name identifies the client method this case serves, for logs and tests.
	name string
	// match inspects the raw SQL text; must be conservative.
	match func(query string) bool
	// cols is the output shape.
	cols []Col
	// run generates the rows directly from a provider snapshot.
	run func(ctx context.Context, snap *snapshot, query string, params []any) ([][]any, error)
}

// specialCases is consulted in order; first match wins. The registry sits
// behind a func-typed cols field only where construction needs code; most
// are plain slices built once at init.
var specialCases = []specialCase{
	{name: "pgjdbc.getColumns", match: matchGetColumns, cols: getColumnsCols(), run: runGetColumns},
	{name: "pgjdbc.getPrimaryKeys", match: matchGetPrimaryKeys, cols: primaryKeyCols(), run: runGetPrimaryKeys},
	{name: "pgjdbc.getPrimaryUniqueKeys", match: matchGetPrimaryUniqueKeys, cols: primaryKeyUniqueCols(), run: runGetPrimaryUniqueKeys},
	{name: "pgjdbc.getBestRowIdentifier", match: matchGetBestRowIdentifier, cols: bestRowCols(), run: runGetBestRowIdentifier},
	{name: "pgjdbc.getImportedExportedKeys", match: matchGetImportedExportedKeys, cols: importedKeyCols(), run: runGetImportedExportedKeys},
	{name: "pgjdbc.getTypeInfo", match: matchGetTypeInfo, cols: typeInfoCols(), run: runGetTypeInfo},
	{name: "pgjdbc.getUDTs", match: matchGetUDTs, cols: udtCols(), run: runGetUDTs},
	{name: "pgjdbc.getIndexInfo", match: matchGetIndexInfo, cols: indexInfoCols(), run: runGetIndexInfo},
	{name: "pgjdbc.getFunctionColumns", match: matchGetFunctionColumns, cols: functionColumnsCols(), run: runGetFunctionColumns},
}

// specialResult is a matched case's execution plan: static output columns
// plus a runner bound to the query text that matched.
type specialResult struct {
	cols  []Col
	query string
	run   func(ctx context.Context, snap *snapshot) ([][]any, error)
}

// matchSpecial finds a special case for the query text.
func matchSpecial(query string) (*specialResult, bool) {
	for _, sc := range specialCases {
		if !sc.match(query) {
			continue
		}
		caseCopy := sc
		return &specialResult{
			cols:  caseCopy.cols,
			query: query,
			run: func(ctx context.Context, snap *snapshot) ([][]any, error) {
				return caseCopy.run(ctx, snap, query, snap.params)
			},
		}, true
	}
	return nil, false
}

// ─── matchers: verbatim landmarks from PgDatabaseMetaData.java ─────────────

func matchLandmarks(query string, landmarks ...string) bool {
	for _, l := range landmarks {
		if !strings.Contains(query, l) {
			return false
		}
	}
	return true
}

func matchGetColumns(q string) bool {
	return matchLandmarks(q, "row_number() OVER (PARTITION BY a.attrelid")
}

func matchGetPrimaryKeys(q string) bool {
	// the constraint path: conkey expanded via information_schema._pg_expandarray
	return matchLandmarks(q, "_pg_expandarray", "con.contype = 'p'")
}

func matchGetPrimaryUniqueKeys(q string) bool {
	// the index path that also accepts plain unique indexes
	return matchLandmarks(q, "_pg_expandarray", "indpred IS NULL", "indexprs IS NULL")
}

func matchGetBestRowIdentifier(q string) bool {
	return matchLandmarks(q, "_pg_expandarray", "i.indisprimary") &&
		!strings.Contains(q, "con.contype = 'p'") &&
		!strings.Contains(q, "indpred IS NULL")
}

func matchGetImportedExportedKeys(q string) bool {
	return matchLandmarks(q, "generate_series", "con.contype = 'f' ")
}

func matchGetTypeInfo(q string) bool {
	return matchLandmarks(q, "SELECT t.typname,t.oid FROM pg_catalog.pg_type")
}

func matchGetUDTs(q string) bool {
	return matchLandmarks(q, "typtype IN ('c','d')")
}

func matchGetIndexInfo(q string) bool {
	return matchLandmarks(q, "pg_get_indexdef", "_pg_expandarray", "indoption")
}

func matchGetFunctionColumns(q string) bool {
	// its SELECT wraps everything in a derived table the engine cannot parse;
	// lyrebird exposes no functions, so the answer is always empty
	return matchLandmarks(q, `AS "FUNCTION_CAT"`, "ORDER BY FUNCTION_SCHEM")
}

// ─── bind-parameter slots ───────────────────────────────────────────────────

var paramSlotRe = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)?)\s+(LIKE|=)\s*(\?|\$\d+)`)

// paramSlot is one client parameter position, identified by the column it
// filters and whether it is a pattern (LIKE) or an exact match.
type paramSlot struct {
	column string // the filter's column, lower-cased
	like   bool
}

// scanParamSlots lists the query's parameters in order with the filter each
// feeds. pgjdbc authors queries with JDBC ? markers, which arrive on the
// wire as $1..$n after the driver's substitution — both spellings are
// recognized here ("n.nspname LIKE $1" → column nspname, like).
func scanParamSlots(query string) []paramSlot {
	matches := paramSlotRe.FindAllStringSubmatch(query, -1)
	slots := make([]paramSlot, 0, len(matches))
	for _, m := range matches {
		col := m[1]
		if i := strings.LastIndex(col, "."); i >= 0 {
			col = col[i+1:]
		}
		slots = append(slots, paramSlot{column: strings.ToLower(col), like: m[2] == "LIKE"})
	}
	return slots
}

// slotFilter answers the bound value that filters the named column ("" = no
// filter — pgjdbc omits the whole fragment when the argument is null).
// column is the canonical virtual-table name; aliases match the
// client-facing names pgjdbc selects under (relname also arrives as
// TABLE_NAME via derived-table aliases).
func slotFilter(slots []paramSlot, params []any, aliases ...string) string {
	wanted := make(map[string]bool, len(aliases))
	for _, a := range aliases {
		wanted[strings.ToLower(a)] = true
	}
	for i, s := range slots {
		if !wanted[s.column] || i >= len(params) {
			continue
		}
		v, _ := params[i].(string)
		return v
	}
	return ""
}

// likeFilter applies a client pattern to a name: an empty pattern matches
// everything, otherwise SQL LIKE semantics.
func likeFilter(pattern, name string) bool {
	if pattern == "" {
		return true
	}
	ok, err := likeMatch(name, pattern, false)
	return err == nil && ok
}

// ─── shared row vocabulary ──────────────────────────────────────────────────

// jdbcType maps a catalog type OID onto the java.sql.Types code pgjdbc's
// TypeInfoCache reports (getSQLType), so DATA_TYPE columns answer what the
// driver would have answered against real PostgreSQL.
func jdbcType(oid uint32) float64 {
	switch oid {
	case OIDBool:
		return -7 // Types.BIT
	case OIDBytea:
		return -2 // Types.BINARY
	case OIDChar, OIDBpChar:
		return 1 // Types.CHAR
	case OIDName, OIDText, OIDVarChar:
		return 12 // Types.VARCHAR
	case OIDInt8, OIDOID:
		return -5 // Types.BIGINT
	case OIDInt2:
		return 5 // Types.SMALLINT
	case OIDInt4:
		return 4 // Types.INTEGER
	case OIDFloat4:
		return 7 // Types.REAL
	case OIDFloat8:
		return 8 // Types.DOUBLE
	case OIDNumeric:
		return 2 // Types.NUMERIC
	case OIDDate:
		return 91 // Types.DATE
	case OIDTime:
		return 92 // Types.TIME
	case OIDTs, OIDTsTz:
		return 93 // Types.TIMESTAMP
	case OIDInt2Array, OIDInt4Array, OIDInt8Array, OIDTextArray, OIDOIDArray:
		return 2003 // Types.ARRAY
	default:
		return 1111 // Types.OTHER (json, jsonb, uuid, …)
	}
}

// jdbcPrecision is the COLUMN_SIZE the TypeInfoCache reports per type.
func jdbcPrecision(oid uint32, f FieldMeta) float64 {
	switch oid {
	case OIDVarChar:
		if f.MaxLength > 0 {
			return float64(f.MaxLength)
		}
		return 2147483647
	case OIDFloat8:
		return 53
	case OIDFloat4:
		return 24
	case OIDBool, OIDChar, OIDBpChar:
		return 1
	case OIDInt8, OIDOID:
		return 19
	case OIDInt4:
		return 10
	case OIDInt2:
		return 5
	default:
		return 2147483647
	}
}

func textCol(name string) Col { return Col{Name: name, Oid: OIDText} }

func intCol(name string, oid uint32) Col { return Col{Name: name, Oid: oid} }

func nullableCode(nullable bool) float64 {
	if nullable {
		return 1 // DatabaseMetaData.columnNullable
	}
	return 0 // columnNoNulls
}

func boolYesNo(b bool) string {
	if b {
		return "YES"
	}
	return "NO"
}

// catalogMetas describes every collection of the snapshot's database, with
// its synthetic pg_class OID (the same convention the virtual tables use).
func catalogMetas(ctx context.Context, snap *snapshot) ([]CollectionMeta, error) {
	names, err := snap.provider.ListCollections(ctx, snap.db)
	if err != nil {
		return nil, err
	}
	metas := make([]CollectionMeta, 0, len(names))
	for _, name := range names {
		coll, err := snap.provider.Collection(ctx, snap.db, name)
		if err != nil {
			return nil, err
		}
		metas = append(metas, coll)
	}
	return metas, nil
}

// pkKeyFields returns the primary-key fields of one collection in key order.
func pkKeyFields(coll CollectionMeta) []FieldMeta {
	var keys []FieldMeta
	for _, f := range coll.Fields {
		if f.PrimaryKey {
			keys = append(keys, f)
		}
	}
	return keys
}

// ─── getColumns ─────────────────────────────────────────────────────────────

func getColumnsCols() []Col {
	return []Col{
		textCol("TABLE_CAT"), textCol("TABLE_SCHEM"), textCol("TABLE_NAME"), textCol("COLUMN_NAME"),
		intCol("DATA_TYPE", OIDInt2), textCol("TYPE_NAME"), intCol("COLUMN_SIZE", OIDInt4),
		textCol("BUFFER_LENGTH"), intCol("DECIMAL_DIGITS", OIDInt4), intCol("NUM_PREC_RADIX", OIDInt4),
		intCol("NULLABLE", OIDInt4), textCol("REMARKS"), textCol("COLUMN_DEF"),
		intCol("SQL_DATA_TYPE", OIDInt4), intCol("SQL_DATETIME_SUB", OIDInt4),
		intCol("CHAR_OCTET_LENGTH", OIDInt4), intCol("ORDINAL_POSITION", OIDInt4), textCol("IS_NULLABLE"),
		textCol("SCOPE_CATALOG"), textCol("SCOPE_SCHEMA"), textCol("SCOPE_TABLE"),
		intCol("SOURCE_DATA_TYPE", OIDInt2), textCol("IS_AUTOINCREMENT"), textCol("IS_GENERATEDCOLUMN"),
	}
}

func runGetColumns(ctx context.Context, snap *snapshot, query string, params []any) ([][]any, error) {
	slots := scanParamSlots(query)
	schemaFilter := slotFilter(slots, params, "nspname", "table_schem")
	tableFilter := slotFilter(slots, params, "relname", "table_name")
	columnFilter := slotFilter(slots, params, "attname", "column_name")

	metas, err := catalogMetas(ctx, snap)
	if err != nil {
		return nil, err
	}
	var rows [][]any
	for _, coll := range metas {
		if !likeFilter(schemaFilter, "public") || !likeFilter(tableFilter, coll.Name) {
			continue
		}
		for i, f := range coll.Fields {
			if !likeFilter(columnFilter, f.Name) {
				continue
			}
			oid := FieldTypeOID(f.Type)
			size := jdbcPrecision(oid, f)
			rows = append(rows, []any{
				snap.db, "public", coll.Name, f.Name,
				jdbcType(oid), typName(oid), size,
				nil, float64(0), float64(10),
				nullableCode(f.Nullable), nil, nil,
				nil, nil,
				size, float64(i + 1), boolYesNo(f.Nullable),
				nil, nil, nil,
				nil, "NO", "NO",
			})
		}
	}
	return rows, nil
}

// ─── primary-key family ─────────────────────────────────────────────────────

func primaryKeyCols() []Col {
	return []Col{
		textCol("TABLE_CAT"), textCol("TABLE_SCHEM"), textCol("TABLE_NAME"),
		textCol("COLUMN_NAME"), intCol("KEY_SEQ", OIDInt4), textCol("PK_NAME"),
	}
}

func primaryKeyUniqueCols() []Col {
	return []Col{
		textCol("TABLE_CAT"), textCol("TABLE_SCHEM"), textCol("TABLE_NAME"),
		textCol("COLUMN_NAME"), intCol("KEY_SEQ", OIDInt4), textCol("PK_NAME"),
		Col{Name: "IS_NOT_NULL", Oid: OIDBool},
	}
}

func bestRowCols() []Col {
	return []Col{
		intCol("SCOPE", OIDInt2), textCol("COLUMN_NAME"), intCol("DATA_TYPE", OIDInt2),
		textCol("TYPE_NAME"), intCol("COLUMN_SIZE", OIDInt4), intCol("BUFFER_LENGTH", OIDInt4),
		intCol("DECIMAL_DIGITS", OIDInt2), intCol("PSEUDO_COLUMN", OIDInt2),
	}
}

func runGetPrimaryKeys(ctx context.Context, snap *snapshot, query string, params []any) ([][]any, error) {
	return primaryKeyRows(ctx, snap, query, params, false)
}

func runGetPrimaryUniqueKeys(ctx context.Context, snap *snapshot, query string, params []any) ([][]any, error) {
	return primaryKeyRows(ctx, snap, query, params, true)
}

// primaryKeyRows serves getPrimaryKeys and getPrimaryUniqueKeys: both walk
// each collection's primary-key fields; the unique variant adds IS_NOT_NULL.
func primaryKeyRows(ctx context.Context, snap *snapshot, query string, params []any, withNotNull bool) ([][]any, error) {
	slots := scanParamSlots(query)
	schemaFilter := slotFilter(slots, params, "nspname", "table_schem")
	tableFilter := slotFilter(slots, params, "relname", "table_name")

	metas, err := catalogMetas(ctx, snap)
	if err != nil {
		return nil, err
	}
	var rows [][]any
	for _, coll := range metas {
		if !likeFilter(schemaFilter, "public") || !likeFilter(tableFilter, coll.Name) {
			continue
		}
		keys := pkKeyFields(coll)
		if len(keys) == 0 {
			continue
		}
		for i, f := range keys {
			row := []any{
				snap.db, "public", coll.Name, f.Name,
				float64(i + 1), coll.Name + "_pkey",
			}
			if withNotNull {
				row = append(row, !f.Nullable)
			}
			rows = append(rows, row)
		}
	}
	return rows, nil
}

func runGetBestRowIdentifier(ctx context.Context, snap *snapshot, query string, params []any) ([][]any, error) {
	slots := scanParamSlots(query)
	tableFilter := slotFilter(slots, params, "relname", "table_name")

	metas, err := catalogMetas(ctx, snap)
	if err != nil {
		return nil, err
	}
	var rows [][]any
	for _, coll := range metas {
		if !likeFilter(tableFilter, coll.Name) {
			continue
		}
		for _, f := range pkKeyFields(coll) {
			oid := FieldTypeOID(f.Type)
			rows = append(rows, []any{
				float64(2), // bestRowSession: the scope rides the Java call, not the SQL
				f.Name, jdbcType(oid), typName(oid),
				jdbcPrecision(oid, f), nil, float64(0),
				float64(1), // bestRowNotPseudo
			})
		}
	}
	return rows, nil
}

// ─── imported/exported keys: no foreign keys exist ──────────────────────────

func importedKeyCols() []Col {
	return []Col{
		textCol("PKTABLE_CAT"), textCol("PKTABLE_SCHEM"), textCol("PKTABLE_NAME"), textCol("PKCOLUMN_NAME"),
		textCol("FKTABLE_CAT"), textCol("FKTABLE_SCHEM"), textCol("FKTABLE_NAME"), textCol("FKCOLUMN_NAME"),
		intCol("KEY_SEQ", OIDInt2), intCol("UPDATE_RULE", OIDInt2), intCol("DELETE_RULE", OIDInt2),
		textCol("FK_NAME"), textCol("PK_NAME"), intCol("DEFERRABILITY", OIDInt2),
	}
}

func runGetImportedExportedKeys(ctx context.Context, snap *snapshot, query string, params []any) ([][]any, error) {
	return nil, nil // con.contype = 'f' never matches: Milvus has no foreign keys
}

// ─── type info ──────────────────────────────────────────────────────────────

func typeInfoCols() []Col {
	return []Col{
		textCol("TYPE_NAME"), intCol("DATA_TYPE", OIDInt2), intCol("PRECISION", OIDInt4),
		textCol("LITERAL_PREFIX"), textCol("LITERAL_SUFFIX"), textCol("CREATE_PARAMS"),
		intCol("NULLABLE", OIDInt2), Col{Name: "CASE_SENSITIVE", Oid: OIDBool},
		intCol("SEARCHABLE", OIDInt2), Col{Name: "UNSIGNED_ATTRIBUTE", Oid: OIDBool},
		Col{Name: "FIXED_PREC_SCALE", Oid: OIDBool}, Col{Name: "AUTO_INCREMENT", Oid: OIDBool},
		textCol("LOCAL_TYPE_NAME"), intCol("MINIMUM_SCALE", OIDInt2), intCol("MAXIMUM_SCALE", OIDInt2),
		intCol("SQL_DATA_TYPE", OIDInt4), intCol("SQL_DATETIME_SUB", OIDInt4), intCol("NUM_PREC_RADIX", OIDInt4),
	}
}

func runGetTypeInfo(ctx context.Context, snap *snapshot, query string, params []any) ([][]any, error) {
	var rows [][]any
	for _, t := range sysTypes {
		if strings.HasPrefix(t.name, "_") {
			continue // array twins are not separately reported
		}
		quoted := t.category == "S" // string family: literal prefix/suffix '
		var prefix any
		if quoted {
			prefix = "'"
		}
		rows = append(rows, []any{
			t.name, jdbcType(t.oid), float64(2147483647),
			prefix, prefix, nil,
			float64(1), t.category == "S", // typeNullable; case-sensitive strings
			float64(3), false, // typeSearchable
			false, false,
			nil, float64(0), numericMaxScale(t.oid),
			nil, nil, float64(10),
		})
	}
	return rows, nil
}

func numericMaxScale(oid uint32) float64 {
	if oid == OIDNumeric {
		return 1000
	}
	return 0
}

// ─── UDTs: no composite or domain types exist ───────────────────────────────

func udtCols() []Col {
	return []Col{
		textCol("TYPE_CAT"), textCol("TYPE_SCHEM"), textCol("TYPE_NAME"), textCol("CLASS_NAME"),
		intCol("DATA_TYPE", OIDInt4), textCol("REMARKS"), intCol("BASE_TYPE", OIDInt2),
	}
}

func runGetUDTs(ctx context.Context, snap *snapshot, query string, params []any) ([][]any, error) {
	return nil, nil // every catalog type is a base type ('b')
}

// ─── function columns: no functions exist ───────────────────────────────────

func functionColumnsCols() []Col {
	return []Col{
		textCol("FUNCTION_CAT"), textCol("FUNCTION_SCHEM"), textCol("FUNCTION_NAME"),
		textCol("COLUMN_NAME"), intCol("COLUMN_TYPE", OIDInt2), intCol("DATA_TYPE", OIDInt2),
		textCol("TYPE_NAME"), intCol("PRECISION", OIDInt4), intCol("LENGTH", OIDInt4),
		intCol("SCALE", OIDInt2), intCol("RADIX", OIDInt2), intCol("NULLABLE", OIDInt2),
		textCol("REMARKS"), textCol("COLUMN_DEF"), intCol("SQL_DATA_TYPE", OIDInt4),
		intCol("SQL_DATETIME_SUB", OIDInt4), intCol("CHAR_OCTET_LENGTH", OIDInt4),
		intCol("ORDINAL_POSITION", OIDInt4), textCol("IS_NULLABLE"), textCol("SPECIFIC_NAME"),
	}
}

func runGetFunctionColumns(ctx context.Context, snap *snapshot, query string, params []any) ([][]any, error) {
	return nil, nil // pg_proc is empty: no functions to describe
}

// ─── index info: the one index per collection is its primary key ────────────

func indexInfoCols() []Col {
	return []Col{
		textCol("TABLE_CAT"), textCol("TABLE_SCHEM"), textCol("TABLE_NAME"),
		Col{Name: "NON_UNIQUE", Oid: OIDBool}, textCol("INDEX_QUALIFIER"), textCol("INDEX_NAME"),
		intCol("TYPE", OIDInt2), intCol("ORDINAL_POSITION", OIDInt2), textCol("COLUMN_NAME"),
		textCol("ASC_OR_DESC"), intCol("CARDINALITY", OIDInt8), intCol("PAGES", OIDInt8),
		textCol("FILTER_CONDITION"), textCol("REMARKS"),
	}
}

func runGetIndexInfo(ctx context.Context, snap *snapshot, query string, params []any) ([][]any, error) {
	slots := scanParamSlots(query)
	tableFilter := slotFilter(slots, params, "relname", "table_name")

	metas, err := catalogMetas(ctx, snap)
	if err != nil {
		return nil, err
	}
	var rows [][]any
	for _, coll := range metas {
		if !likeFilter(tableFilter, coll.Name) {
			continue
		}
		// the PK index is unique, so the client's unique-only filter changes
		// nothing: there are no other indexes to report
		for i, f := range pkKeyFields(coll) {
			rows = append(rows, []any{
				snap.db, "public", coll.Name,
				false, nil, coll.Name + "_pkey",
				float64(3), float64(i + 1), f.Name, // tableIndexOther
				"A", float64(0), float64(0),
				nil, nil,
			})
		}
	}
	return rows, nil
}
