package catalog

import (
	"context"
	"strings"
)

// This file is the virtual-table registry: the PostgreSQL catalog tables
// lyrebird answers, their column shapes, and how each one is generated from
// a Provider snapshot. Column OIDs are the real PostgreSQL numbers so
// clients decode rows without a custom type map; cell values stay Go-native
// (string / float64 / bool / []any) and the wire layer coerces per OID.

type colDef struct {
	name string
	oid  uint32
}

type tableDef struct {
	name  string
	cols  []colDef
	build func(ctx context.Context, snap *snapshot) ([][]any, error)
}

// synthetic OID bases for objects the store cannot name: user tables start
// at 16384 in real PostgreSQL, so the catalog follows the convention.
const (
	firstTableOID = 16384
	publicNSOID   = 2200 // the fixed OID real PG gives the public schema
	ownerOID      = 10
)

// tableRegistry holds every catalog table by bare name; lookup ignores the
// schema qualifier (pg_catalog.pg_class and pg_class are the same table).
var tableRegistry = func() map[string]*tableDef {
	reg := make(map[string]*tableDef)
	for i := range catalogTables {
		reg[catalogTables[i].name] = &catalogTables[i]
	}
	return reg
}()

// isCatalogTable reports whether a (possibly schema-qualified) reference
// names a virtual catalog table. The pgserver dispatcher uses it to route
// statements; catalog queries may not mix catalog and collection tables.
func isCatalogTable(schema, name string) bool {
	if schema != "" && schema != "pg_catalog" && schema != "information_schema" {
		return false
	}
	_, ok := tableRegistry[strings.ToLower(name)]
	return ok
}

func col(name string, oid uint32) colDef { return colDef{name: name, oid: oid} }

func (d *tableDef) colIndex(name string) int {
	for i, c := range d.cols {
		if c.name == name {
			return i
		}
	}
	return -1
}

// row wraps one table row; the count is checked against the definition at
// build time by the (+width) helper below.
func row(values ...any) []any { return values }

// snapshotMeta is one collection's described metadata with the synthetic
// OID this snapshot assigned it.
type snapshotMeta struct {
	meta CollectionMeta
	oid  uint64
}

// snapshot is one execution's catalog data: which tables the query touches,
// their generated rows, and the execution's session context (database name,
// bind parameters, SET overlays). ctx rides along because correlated scalar
// subqueries materialize tables mid-evaluation.
type snapshot struct {
	db       string
	provider Provider
	tables   map[string]*tableData
	settings map[string]string // set_config overlays for this execution
	params   []any
	ctx      context.Context

	metas     []snapshotMeta
	metasDone bool
	metasErr  error
}

type tableData struct {
	def  *tableDef
	rows [][]any
}

// table materializes one virtual table, once per execution. A nil build is
// the empty-system-table convention: registered for FROM references, never
// contributing rows.
func (s *snapshot) table(ctx context.Context, def *tableDef) (*tableData, error) {
	if td, ok := s.tables[def.name]; ok {
		return td, nil
	}
	var rows [][]any
	if def.build != nil {
		var err error
		rows, err = def.build(ctx, s)
		if err != nil {
			return nil, err
		}
	}
	td := &tableData{def: def, rows: rows}
	s.tables[def.name] = td
	return td, nil
}

// collectionMetas describes every collection of the current database once
// per snapshot. The OID assignment is snapshot-local: two independent
// ListCollections calls may order collections differently, and pg_class.
// oid must match pg_attribute.attrelid within one answer.
func (s *snapshot) collectionMetas(ctx context.Context) ([]snapshotMeta, error) {
	if s.metasDone {
		return s.metas, s.metasErr
	}
	s.metasDone = true
	names, err := s.provider.ListCollections(ctx, s.db)
	if err != nil {
		s.metasErr = err
		return nil, err
	}
	for i, name := range names {
		coll, err := s.provider.Collection(ctx, s.db, name)
		if err != nil {
			s.metasErr = err
			return nil, err
		}
		s.metas = append(s.metas, snapshotMeta{meta: coll, oid: firstTableOID + uint64(i)})
	}
	return s.metas, nil
}

// collectionRows is the shared generator behind pg_class, pg_attribute,
// pg_constraint and pg_index: it describes every collection of the current
// database once and hands each CollectionMeta to the row-shaping callback,
// which may contribute any number of rows (pg_attribute: one per field).
func collectionRows(ctx context.Context, s *snapshot, shape func(coll CollectionMeta, tableOID uint64) ([][]any, error)) ([][]any, error) {
	metas, err := s.collectionMetas(ctx)
	if err != nil {
		return nil, err
	}
	rows := make([][]any, 0, len(metas))
	for _, m := range metas {
		part, err := shape(m.meta, m.oid)
		if err != nil {
			return nil, err
		}
		rows = append(rows, part...)
	}
	return rows, nil
}

// catalogTables is the registry body. Tables whose rows never vary
// (pg_type, pg_settings, pg_am…) build from static data; store-backed ones
// project the Provider snapshot.
var catalogTables = []tableDef{
	{
		name: "pg_database",
		cols: []colDef{
			col("oid", OIDOID), col("datname", OIDName), col("datdba", OIDOID),
			col("encoding", OIDInt4), col("datistemplate", OIDBool),
			col("datallowconn", OIDBool), col("datconnlimit", OIDInt4),
			col("dattablespace", OIDOID), col("datcollate", OIDText), col("datctype", OIDText),
			col("datlocprovider", OIDChar), col("datlocale", OIDText),
			col("daticulocale", OIDText), col("daticurules", OIDText), col("datacl", OIDTextArray),
		},
		build: func(ctx context.Context, s *snapshot) ([][]any, error) {
			dbs, err := s.provider.Databases(ctx)
			if err != nil {
				return nil, err
			}
			rows := make([][]any, 0, len(dbs))
			for i, name := range dbs {
				rows = append(rows, row(
					float64(firstTableOID+uint64(i)), name, float64(ownerOID),
					float64(6), false, true, float64(-1), float64(1663), "C", "C",
					"c", nil, nil, nil, nil))
			}
			return rows, nil
		},
	},
	{
		name: "pg_namespace",
		cols: []colDef{
			col("oid", OIDOID), col("nspname", OIDName), col("nspowner", OIDOID), col("nspacl", OIDTextArray),
		},
		build: func(ctx context.Context, s *snapshot) ([][]any, error) {
			// the schema list real PostgreSQL presents: pgjdbc's
			// getMaxNameLength joins pg_namespace on nspname='pg_catalog',
			// and getTables' table-type CASE special-cases
			// information_schema, so these rows exist even though the
			// catalog tables under them stay empty. Milvus databases
			// surface as PG databases, never as schemas.
			return [][]any{
				{float64(99), "pg_toast", float64(ownerOID), nil},
				{float64(11), "pg_catalog", float64(ownerOID), nil},
				{float64(13003), "information_schema", float64(ownerOID), nil},
				{float64(publicNSOID), "public", float64(ownerOID), nil},
			}, nil
		},
	},
	{
		name: "pg_class",
		cols: []colDef{
			col("oid", OIDOID), col("relname", OIDName), col("relnamespace", OIDOID),
			col("relkind", OIDChar), col("relowner", OIDOID), col("relam", OIDOID),
			col("reltablespace", OIDOID), col("relpages", OIDInt4), col("reltuples", OIDFloat4),
			col("relhasindex", OIDBool), col("relhasrules", OIDBool), col("relhastriggers", OIDBool),
			col("relpersistence", OIDChar), col("relnatts", OIDInt2), col("relrowsecurity", OIDBool),
			col("relchecks", OIDInt2), col("reloftype", OIDOID),
			col("relforcerowsecurity", OIDBool), col("relispartition", OIDBool),
			col("relispopulated", OIDBool), col("relreplident", OIDChar), col("reltoastrelid", OIDOID),
			col("relacl", OIDText),
		},
		build: func(ctx context.Context, s *snapshot) ([][]any, error) {
			return collectionRows(ctx, s, func(coll CollectionMeta, tableOID uint64) ([][]any, error) {
			return [][]any{{
				float64(tableOID), coll.Name, float64(publicNSOID),
				"r", float64(ownerOID), float64(0),
				float64(0), float64(0), float64(0),
				false, false, false,
				"p", float64(len(coll.Fields)), false,
				float64(0), float64(0), false, false,
				true, "d", float64(0),
				nil,
			}}, nil
			})
		},
	},
	{
		name: "pg_attribute",
		cols: []colDef{
			col("attrelid", OIDOID), col("attname", OIDName), col("atttypid", OIDOID),
			col("attlen", OIDInt2), col("attnum", OIDInt2), col("atttypmod", OIDInt4),
			col("attnotnull", OIDBool), col("attisdropped", OIDBool), col("attislocal", OIDBool),
			col("attinhcount", OIDInt4), col("attidentity", OIDChar), col("attgenerated", OIDChar),
			col("attbyval", OIDBool), col("attalign", OIDChar), col("attstorage", OIDChar),
			col("attstattarget", OIDInt4), col("attndims", OIDInt2), col("attcacheoff", OIDInt4),
			col("attacl", OIDText), col("attcollation", OIDOID),
		},
		build: func(ctx context.Context, s *snapshot) ([][]any, error) {
			return collectionRows(ctx, s, func(coll CollectionMeta, tableOID uint64) ([][]any, error) {
				var rows [][]any
				for i, f := range coll.Fields {
					oid := FieldTypeOID(f.Type)
					rows = append(rows, row(
						float64(tableOID), f.Name, float64(oid),
						float64(typLen(oid)), float64(i+1), float64(typMod(f)),
						!f.Nullable, false, true,
						float64(0), "", "",
						typByVal(oid), typAlign(oid), typStorage(oid),
						float64(-1), float64(0), float64(-1),
						nil, float64(collationDefaultOID)))
				}
				return rows, nil
			})
		},
	},
	{
		name: "pg_constraint",
		cols: []colDef{
			col("oid", OIDOID), col("conname", OIDName), col("connamespace", OIDOID),
			col("contype", OIDChar), col("condeferrable", OIDBool), col("condeferred", OIDBool),
			col("convalidated", OIDBool), col("conrelid", OIDOID), col("contypid", OIDOID),
			col("conindid", OIDOID), col("confrelid", OIDOID), col("confupdtype", OIDChar),
			col("confdeltype", OIDChar), col("confmatchtype", OIDChar), col("conislocal", OIDBool),
			col("coninheritable", OIDBool), col("connoinherit", OIDBool),
			col("conkey", OIDInt2Array), col("confkey", OIDInt2Array),
		},
		build: func(ctx context.Context, s *snapshot) ([][]any, error) {
			return collectionRows(ctx, s, func(coll CollectionMeta, tableOID uint64) ([][]any, error) {
				// one PRIMARY KEY row per collection with a declared PK field
				key := []any{}
				for i, f := range coll.Fields {
					if f.PrimaryKey {
						key = append(key, float64(i+1))
					}
				}
				if len(key) == 0 {
					return nil, nil
				}
				conOID := firstTableOID + 100000 + uint64(len(coll.Name)) // stable per execution
				return [][]any{{float64(conOID), coll.Name + "_pkey", float64(publicNSOID),
					"p", false, false,
					true, float64(tableOID), float64(0),
					float64(0), float64(0), "", "", "",
					true, false, true,
					key, nil}}, nil
			})
		},
	},
	{
		name: "pg_index",
		cols: []colDef{
			col("indexrelid", OIDOID), col("indrelid", OIDOID), col("indnatts", OIDInt2),
			col("indnkeyatts", OIDInt2), col("indisunique", OIDBool), col("indisprimary", OIDBool),
			col("indisexclusion", OIDBool), col("indimmediate", OIDBool), col("indisclustered", OIDBool),
			col("indisvalid", OIDBool), col("indcheckxmin", OIDBool), col("indisready", OIDBool),
			col("indislive", OIDBool), col("indisreplident", OIDBool),
			col("indkey", OIDInt2Array), col("indoption", OIDInt2Array),
			col("indexprs", OIDText), col("indpred", OIDText),
		},
		build: func(ctx context.Context, s *snapshot) ([][]any, error) {
			return collectionRows(ctx, s, func(coll CollectionMeta, tableOID uint64) ([][]any, error) {
				key := []any{}
				for i, f := range coll.Fields {
					if f.PrimaryKey {
						key = append(key, float64(i+1))
					}
				}
				if len(key) == 0 {
					return nil, nil
				}
				idxOID := firstTableOID + 200000 + uint64(len(coll.Name))
				return [][]any{{float64(idxOID), float64(tableOID), float64(len(key)),
					float64(len(key)), true, true,
					false, true, false,
					true, false, true,
					true, false,
					key, []any{}, nil, nil}}, nil
			})
		},
	},
	{
		name: "pg_type",
		cols: []colDef{
			col("oid", OIDOID), col("typname", OIDName), col("typnamespace", OIDOID),
			col("typowner", OIDOID), col("typlen", OIDInt2), col("typtype", OIDChar),
			col("typisdefined", OIDBool), col("typdelim", OIDChar), col("typrelid", OIDOID),
			col("typelem", OIDOID), col("typarray", OIDOID), col("typalign", OIDChar),
			col("typstorage", OIDChar), col("typcategory", OIDChar), col("typdefault", OIDText),
			col("typcollation", OIDOID),
		},
		build: func(ctx context.Context, s *snapshot) ([][]any, error) {
			rows := make([][]any, 0, len(sysTypes))
			for _, t := range sysTypes {
				rows = append(rows, row(
					float64(t.oid), t.name, float64(11),
					float64(ownerOID), t.typlen, t.typtype,
					true, ",", float64(0),
					float64(0), float64(t.arrayOf), "i",
					"p", t.category, nil,
					float64(0)))
			}
			return rows, nil
		},
	},
	{
		name: "pg_description",
		cols: []colDef{col("objoid", OIDOID), col("classoid", OIDOID), col("objsubid", OIDInt4), col("description", OIDText)},
		build: func(ctx context.Context, s *snapshot) ([][]any, error) {
			return nil, nil // no descriptions yet
		},
	},
	{
		name: "pg_attrdef",
		cols: []colDef{col("oid", OIDOID), col("adrelid", OIDOID), col("adnum", OIDInt2), col("adbin", OIDText), col("adsrc", OIDText)},
		build: func(ctx context.Context, s *snapshot) ([][]any, error) {
			return nil, nil // no column defaults
		},
	},
	{
		name: "pg_proc",
		cols: []colDef{
			col("oid", OIDOID), col("proname", OIDName), col("pronamespace", OIDOID),
			col("prokind", OIDChar), col("prorettype", OIDOID), col("pronargs", OIDInt2),
			col("proretset", OIDBool), col("proargtypes", OIDOIDVector),
			col("proallargtypes", OIDOIDArray), col("proargmodes", OIDTextArray),
			col("proargnames", OIDTextArray), col("prosrc", OIDText),
		},
		build: func(ctx context.Context, s *snapshot) ([][]any, error) {
			return nil, nil // no stored procedures: procedure-family queries
			// (getProcedures, getFunctions, …) join this table and come back
			// empty, which is the truthful answer
		},
	},
	{
		name: "pg_roles",
		cols: []colDef{
			col("oid", OIDOID), col("rolname", OIDName), col("rolsuper", OIDBool),
			col("rolinherit", OIDBool), col("rolcreaterole", OIDBool), col("rolcreatedb", OIDBool),
			col("rolcanlogin", OIDBool), col("rolreplication", OIDBool), col("rolconnlimit", OIDInt4),
			col("rolpassword", OIDText), col("rolvaliduntil", OIDTsTz), col("rolbypassrls", OIDBool),
			col("rolconfig", OIDTextArray),
		},
		build: func(ctx context.Context, s *snapshot) ([][]any, error) {
			return [][]any{{float64(ownerOID), "lyrebird", true, true, true, true, true, true, float64(-1), nil, nil, true, nil}}, nil
		},
	},
	{
		name: "pg_user",
		cols: []colDef{
			col("usename", OIDName), col("usesysid", OIDOID), col("usecreatedb", OIDBool),
			col("usesuper", OIDBool), col("userepl", OIDBool), col("usebypassrls", OIDBool),
			col("passwd", OIDText), col("valuntil", OIDTsTz), col("useconfig", OIDTextArray),
		},
		build: func(ctx context.Context, s *snapshot) ([][]any, error) {
			return [][]any{{"lyrebird", float64(ownerOID), true, true, true, true, nil, nil, nil}}, nil
		},
	},
	{
		name: "pg_am",
		cols: []colDef{col("oid", OIDOID), col("amname", OIDName), col("amhandler", OIDOID), col("amtype", OIDChar)},
		build: func(ctx context.Context, s *snapshot) ([][]any, error) {
			return [][]any{
				{float64(403), "btree", float64(0), "i"},
				{float64(405), "hash", float64(0), "h"},
				{float64(2745), "gin", float64(0), "i"},
			}, nil
		},
	},
	{
		name: "schemata",
		cols: []colDef{
			col("catalog_name", OIDText), col("schema_name", OIDName), col("schema_owner", OIDName),
		},
		build: func(ctx context.Context, s *snapshot) ([][]any, error) {
			return [][]any{{s.db, "public", "lyrebird"}}, nil
		},
	},
	{
		name: "tables",
		cols: []colDef{
			col("table_catalog", OIDText), col("table_schema", OIDName),
			col("table_name", OIDName), col("table_type", OIDText),
			col("is_insertable_into", OIDText), col("commit_action", OIDText),
		},
		build: func(ctx context.Context, s *snapshot) ([][]any, error) {
			names, err := s.provider.ListCollections(ctx, s.db)
			if err != nil {
				return nil, err
			}
			rows := make([][]any, 0, len(names))
			for _, name := range names {
				rows = append(rows, row( s.db, "public", name, "BASE TABLE", "YES", nil))
			}
			return rows, nil
		},
	},
	{
		name: "columns",
		cols: []colDef{
			col("table_catalog", OIDText), col("table_schema", OIDName),
			col("table_name", OIDName), col("column_name", OIDName),
			col("ordinal_position", OIDInt4), col("column_default", OIDText),
			col("is_nullable", OIDText), col("data_type", OIDText),
			col("character_maximum_length", OIDInt4), col("numeric_precision", OIDInt4),
			col("udt_name", OIDName),
		},
		build: func(ctx context.Context, s *snapshot) ([][]any, error) {
			names, err := s.provider.ListCollections(ctx, s.db)
			if err != nil {
				return nil, err
			}
			var rows [][]any
			for _, name := range names {
				coll, err := s.provider.Collection(ctx, s.db, name)
				if err != nil {
					return nil, err
				}
				for i, f := range coll.Fields {
					oid := FieldTypeOID(f.Type)
					maxLen := any(nil)
					if f.MaxLength > 0 {
						maxLen = float64(f.MaxLength)
					}
					rows = append(rows, row(
						s.db, "public", name, f.Name,
						float64(i+1), nil,
						boolText(f.Nullable), dataTypeName(oid), maxLen, nil,
						typName(oid)))
				}
			}
			return rows, nil
		},
	},
}

// settings is the SHOW / current_setting / pg_settings body: the session
// surface a PG client expects, broadcast or answered statically.
var settings = []struct {
	name     string
	value    string
	category string
	vartype  string
}{
	{"server_version", VersionString, "Preset Options", "string"},
	{"server_version_num", "160000", "Preset Options", "integer"},
	{"server_encoding", "UTF8", "Client Connection Defaults", "string"},
	{"client_encoding", "UTF8", "Client Connection Defaults", "string"},
	{"standard_conforming_strings", "on", "Version and Platform Compatibility", "bool"},
	{"integer_datetimes", "on", "Preset Options", "bool"},
	{"DateStyle", "ISO, MDY", "Client Connection Defaults", "string"},
	{"TimeZone", "UTC", "Client Connection Defaults", "string"},
	{"IntervalStyle", "postgres", "Client Connection Defaults", "string"},
	{"search_path", "public", "Client Connection Defaults", "string"},
	{"transaction_isolation", "read committed", "Client Connection Defaults", "enum"},
	{"transaction_read_only", "off", "Client Connection Defaults", "bool"},
	{"default_transaction_isolation", "read committed", "Client Connection Defaults", "enum"},
	{"default_transaction_read_only", "off", "Client Connection Defaults", "bool"},
	{"max_index_keys", "32", "Preset Options", "integer"},
	{"max_identifier_length", "63", "Preset Options", "integer"},
	{"application_name", "", "Reporting and Logging", "string"},
	{"is_superuser", "on", "Preset Options", "bool"},
	{"session_authorization", "lyrebird", "Client Connection Defaults", "string"},
	{"in_hot_standby", "off", "Preset Options", "bool"},
}

var settingsByName = func() map[string]string {
	m := make(map[string]string, len(settings))
	for _, s := range settings {
		m[strings.ToLower(s.name)] = s.value
	}
	return m
}()

// setting resolves one SHOW / current_setting name.
func setting(name string) (string, bool) {
	v, ok := settingsByName[name]
	return v, ok
}

// Setting is the exported SHOW lookup (case-insensitive, per PG semantics).
func Setting(name string) (string, bool) {
	v, ok := settingsByName[strings.ToLower(name)]
	return v, ok
}

// SettingInfo is one row of the SHOW ALL surface.
type SettingInfo struct {
	Name        string
	Value       string
	Description string
}

// AllSettings lists the settings in catalog order.
func AllSettings() []SettingInfo {
	out := make([]SettingInfo, 0, len(settings))
	for _, s := range settings {
		out = append(out, SettingInfo{Name: s.name, Value: s.value, Description: "lyrebird gateway setting"})
	}
	return out
}

func settingIndex(name string) int {
	for i, s := range settings {
		if s.name == name {
			return i
		}
	}
	return -1
}

// VersionString is the product string version() reports and the wire
// ParameterStatus broadcasts — one source so clients see the same answer.
const VersionString = "PostgreSQL 16.0 (lyrebird)"

// typLen mirrors pg_type.typlen for the catalog's named types.
func typLen(oid uint32) float64 {
	if t, ok := oidByType[oid]; ok {
		return t.typlen
	}
	return -1
}

// typMod renders a field's type modifier: varchar carries max_length+4
// (VARHDRSZ, the real PG convention format_type subtracts back); everything
// else is unmodified (-1).
func typMod(f FieldMeta) float64 {
	if f.MaxLength > 0 && FieldTypeOID(f.Type) == OIDVarChar {
		return float64(f.MaxLength + 4)
	}
	return -1
}

func typByVal(oid uint32) bool {
	return typLen(oid) > 0 && typLen(oid) <= 8
}

func typAlign(oid uint32) string {
	if typLen(oid) == 8 {
		return "d"
	}
	return "i"
}

func typStorage(oid uint32) string {
	if typLen(oid) < 0 {
		return "x" // extended: compress + store out of line
	}
	return "p"
}

func dataTypeName(oid uint32) string {
	switch oid {
	case OIDVarChar:
		return "character varying"
	case OIDText:
		return "text"
	case OIDFloat8:
		return "double precision"
	case OIDBool:
		return "boolean"
	case OIDJSON:
		return "json"
	}
	return typName(oid)
}

func boolText(b bool) string {
	if b {
		return "YES"
	}
	return "NO"
}

// settingsRows is the pg_settings body; overlays apply per execution.
func settingsRows(s *snapshot) [][]any {
	rows := make([][]any, 0, len(settings))
	for _, st := range settings {
		value := st.value
		if overlay, ok := s.settings[st.name]; ok {
			value = overlay
		}
		rows = append(rows, []any{
			st.name, value, nil, st.category, "lyrebird gateway setting", "user",
			st.vartype, "default", nil, nil, value, value, false,
		})
	}
	return rows
}

// pgSettingsDef is registered separately because its rows come from the
// settings list rather than a Provider snapshot.
var pgSettingsDef = tableDef{
	name: "pg_settings",
	cols: []colDef{
		col("name", OIDText), col("setting", OIDText), col("unit", OIDText),
		col("category", OIDText), col("short_desc", OIDText), col("context", OIDText),
		col("vartype", OIDText), col("source", OIDText), col("min_val", OIDText),
		col("max_val", OIDText), col("boot_val", OIDText), col("reset_val", OIDText),
		col("pending_restart", OIDBool),
	},
	build: func(ctx context.Context, s *snapshot) ([][]any, error) {
		return settingsRows(s), nil
	},
}

// collationDefaultOID is the synthetic OID of the one collation row.
const collationDefaultOID = 100

var pgCollationDef = tableDef{
	name: "pg_collation",
	cols: []colDef{
		col("oid", OIDOID), col("collname", OIDName), col("collnamespace", OIDOID),
		col("collprovider", OIDChar), col("collisdeterministic", OIDBool),
	},
	build: func(ctx context.Context, s *snapshot) ([][]any, error) {
		return [][]any{{
			float64(collationDefaultOID), "default", float64(11),
			"d", true,
		}}, nil
	},
}

// emptySystemTables are the catalog tables lyrebird exposes as always
// empty: the objects they describe (triggers, rules, inheritance, extended
// statistics, publications, foreign tables) do not exist in the gateway's
// model, but psql's \d family joins them on every describe.
var emptySystemTables = []tableDef{
	{name: "pg_trigger", cols: []colDef{col("oid", OIDOID), col("tgname", OIDName), col("tgrelid", OIDOID), col("tgfoid", OIDOID), col("tgtype", OIDInt2), col("tgenabled", OIDChar), col("tgparentid", OIDOID), col("tgattr", OIDInt2Array), col("tgargs", OIDBytea), col("tgqual", OIDText), col("tgisinternal", OIDBool)}},
	{name: "pg_rewrite", cols: []colDef{col("oid", OIDOID), col("rulename", OIDName), col("ev_class", OIDOID), col("ev_type", OIDChar), col("ev_enabled", OIDChar), col("is_instead", OIDBool), col("ev_action", OIDText)}},
	{name: "pg_inherits", cols: []colDef{col("inhrelid", OIDOID), col("inhparent", OIDOID), col("inhseqno", OIDInt4), col("inhdetachpending", OIDBool)}},
	{name: "pg_partitioned_table", cols: []colDef{col("partrelid", OIDOID), col("partstrat", OIDChar), col("partnatts", OIDInt2), col("partdefid", OIDOID), col("partattrs", OIDInt2Array), col("partclass", OIDOIDArray)}},
	{name: "pg_depend", cols: []colDef{col("classid", OIDOID), col("objid", OIDOID), col("objsubid", OIDInt4), col("refclassid", OIDOID), col("refobjid", OIDOID), col("refobjsubid", OIDInt4), col("deptype", OIDChar)}},
	{name: "pg_statistic_ext", cols: []colDef{col("oid", OIDOID), col("stxrelid", OIDOID), col("stxname", OIDName), col("stxnamespace", OIDOID), col("stxowner", OIDOID), col("stxstattarget", OIDInt4), col("stxkeys", OIDInt2Array), col("stxkind", OIDTextArray)}},
	{name: "pg_publication", cols: []colDef{col("oid", OIDOID), col("pubname", OIDName), col("pubowner", OIDOID), col("puballtables", OIDBool), col("pubinsert", OIDBool), col("pubupdate", OIDBool), col("pubdelete", OIDBool), col("pubtruncate", OIDBool), col("pubviaroot", OIDBool)}},
	{name: "pg_publication_rel", cols: []colDef{col("oid", OIDOID), col("prpubid", OIDOID), col("prrelid", OIDOID), col("prqual", OIDText), col("prattrs", OIDInt2Array)}},
	{name: "pg_publication_namespace", cols: []colDef{col("oid", OIDOID), col("pnpubid", OIDOID), col("pnnspid", OIDOID)}},
	{name: "pg_foreign_table", cols: []colDef{col("ftrelid", OIDOID), col("ftserver", OIDOID), col("ftoptions", OIDTextArray)}},
	{name: "pg_foreign_server", cols: []colDef{col("oid", OIDOID), col("srvname", OIDName), col("srvowner", OIDOID), col("srvfdw", OIDOID), col("srvoptions", OIDTextArray)}},
	{name: "pg_foreign_data_wrapper", cols: []colDef{col("oid", OIDOID), col("fdwname", OIDName), col("fdwowner", OIDOID), col("fdwoptions", OIDTextArray)}},
	{name: "pg_user_mapping", cols: []colDef{col("oid", OIDOID), col("umuser", OIDOID), col("umdb", OIDOID), col("umoptions", OIDTextArray)}},
	{name: "pg_extension", cols: []colDef{col("oid", OIDOID), col("extname", OIDName), col("extnamespace", OIDOID), col("extowner", OIDOID), col("extversion", OIDText)}},
	{name: "pg_event_trigger", cols: []colDef{col("oid", OIDOID), col("evtname", OIDName), col("evtevent", OIDName), col("evtowner", OIDOID), col("evtenabled", OIDChar)}},
	{name: "pg_subscription", cols: []colDef{col("oid", OIDOID), col("subname", OIDName), col("subowner", OIDOID), col("subenabled", OIDBool), col("subconninfo", OIDText)}},
	{name: "pg_shdepend", cols: []colDef{col("dbid", OIDOID), col("classid", OIDOID), col("objid", OIDOID), col("refclassid", OIDOID), col("refobjid", OIDOID), col("deptype", OIDChar)}},
	{name: "pg_seclabel", cols: []colDef{col("objoid", OIDOID), col("classoid", OIDOID), col("objsubid", OIDInt4), col("provider", OIDText), col("label", OIDText)}},
}

func init() {
	for i := range emptySystemTables {
		d := &emptySystemTables[i]
		tableRegistry[d.name] = d
	}
}

var pgPolicyDef = tableDef{
	name: "pg_policy",
	cols: []colDef{
		col("oid", OIDOID), col("polname", OIDName), col("polrelid", OIDOID),
		col("polcmd", OIDChar), col("polpermissive", OIDBool),
		col("polroles", OIDOIDArray), col("polqual", OIDText), col("polwithcheck", OIDText),
	},
	build: func(ctx context.Context, s *snapshot) ([][]any, error) {
		return nil, nil // no row-level security policies
	},
}

func init() {
	tableRegistry[pgSettingsDef.name] = &pgSettingsDef
	tableRegistry[pgCollationDef.name] = &pgCollationDef
	tableRegistry[pgPolicyDef.name] = &pgPolicyDef
}
