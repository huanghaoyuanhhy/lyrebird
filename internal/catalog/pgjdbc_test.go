package catalog

// Fixtures from pgjdbc's PgDatabaseMetaData.java (42.x): the SQL text a
// DataGrip "JDBC metadata" introspection level sends. Each fixture keeps
// the query shape verbatim — clause order, quoted aliases, bind-parameter
// fragments — and asserts what the engine answers for the two-collection
// test provider. When a fixture is a named special case, the test doubles
// as the match-check: if the landmark matcher drifts, the engine's parse
// error surfaces here.

import (
	"context"
	"strings"
	"testing"
)

func execFixture(t *testing.T, sql string, params ...any) [][]any {
	t.Helper()
	st, err := Prepare(sql)
	if err != nil {
		t.Fatalf("prepare: %v\nsql: %s", err, sql)
	}
	rows, err := st.Exec(context.Background(), testProvider(), "default", params)
	if err != nil {
		t.Fatalf("exec: %v\nsql: %s", err, sql)
	}
	return rows
}

func firstCell(t *testing.T, rows [][]any) string {
	t.Helper()
	if len(rows) == 0 {
		return ""
	}
	s, _ := toString(rows[0][0])
	return s
}

func TestPgjdbcGetMaxIndexKeys(t *testing.T) {
	rows := execFixture(t,
		"SELECT setting FROM pg_catalog.pg_settings WHERE name='max_index_keys'")
	if firstCell(t, rows) != "32" {
		t.Errorf("max_index_keys = %v", rows)
	}
}

func TestPgjdbcGetMaxNameLength(t *testing.T) {
	rows := execFixture(t,
		"SELECT t.typlen FROM pg_catalog.pg_type t, pg_catalog.pg_namespace n "+
			"WHERE t.typnamespace=n.oid AND t.typname='name' AND n.nspname='pg_catalog'")
	if firstCell(t, rows) != "64" {
		t.Errorf("typlen(name) = %v, want 64", rows)
	}
}

func TestPgjdbcGetDefaultTransactionIsolation(t *testing.T) {
	rows := execFixture(t,
		"SELECT setting FROM pg_catalog.pg_settings WHERE name='default_transaction_isolation'")
	if firstCell(t, rows) != "read committed" {
		t.Errorf("default isolation = %v", rows)
	}
}

func TestPgjdbcGetCatalogs(t *testing.T) {
	rows := execFixture(t,
		`SELECT datname AS "TABLE_CAT" FROM pg_catalog.pg_database WHERE datallowconn = true ORDER BY datname`)
	var got []string
	for _, r := range rows {
		s, _ := toString(r[0])
		got = append(got, s)
	}
	if strings.Join(got, ",") != "default,second" {
		t.Errorf("catalogs = %v", got)
	}
}

func TestPgjdbcGetSchemas(t *testing.T) {
	sql := `SELECT nspname AS "TABLE_SCHEM", current_database() AS "TABLE_CATALOG" FROM pg_catalog.pg_namespace ` +
		`WHERE nspname <> 'pg_toast' AND (nspname !~ '^pg_temp_' OR nspname = (pg_catalog.current_schemas(true))[1]) ` +
		`AND (nspname !~ '^pg_toast_temp_' OR nspname = replace((pg_catalog.current_schemas(true))[1], 'pg_temp_', 'pg_toast_temp_')) ` +
		`ORDER BY "TABLE_SCHEM"`
	rows := execFixture(t, sql)
	if len(rows) != 3 {
		t.Fatalf("schemas = %d rows, want 3", len(rows))
	}
	// with a schema pattern, the LIKE fragment rides a parameter
	rows = execFixture(t,
		`SELECT nspname AS "TABLE_SCHEM", current_database() AS "TABLE_CATALOG" FROM pg_catalog.pg_namespace `+
			`WHERE nspname <> 'pg_toast' AND (nspname !~ '^pg_temp_' OR nspname = (pg_catalog.current_schemas(true))[1]) `+
			`AND (nspname !~ '^pg_toast_temp_' OR nspname = replace((pg_catalog.current_schemas(true))[1], 'pg_temp_', 'pg_toast_temp_')) `+
			`AND nspname LIKE $1 ORDER BY "TABLE_SCHEM"`, "public")
	if len(rows) != 1 {
		t.Fatalf("filtered schemas = %d rows, want 1", len(rows))
	}
	if s, _ := toString(rows[0][0]); s != "public" {
		t.Errorf("schema = %q, want public", rows[0][0])
	}
}

func TestPgjdbcGetTables(t *testing.T) {
	// the TABLE-types clause with its tableTypeClauses filter, bound patterns
	// and the big relkind → TABLE_TYPE CASE
	sql := `SELECT NULL AS "TABLE_CAT", n.nspname AS "TABLE_SCHEM", c.relname AS "TABLE_NAME", ` +
		`CASE n.nspname ~ '^pg_' OR n.nspname = 'information_schema' WHEN true THEN CASE c.relkind WHEN 'r' THEN 'SYSTEM TABLE' WHEN 'v' THEN 'SYSTEM VIEW' ELSE NULL END ` +
		`WHEN false THEN CASE c.relkind WHEN 'r' THEN 'TABLE' WHEN 'v' THEN 'VIEW' WHEN 'i' THEN 'INDEX' WHEN 'S' THEN 'SEQUENCE' ELSE NULL END ELSE NULL END AS "TABLE_TYPE", ` +
		`d.description AS "REMARKS", '' as "TYPE_CAT", '' as "THEIR_TYPE_CAT", '' as "THEIR_TYPE_SCHEM", '' as "THEIR_TYPE_NAME", '' as "SELF_REFERENCING_COL_NAME", '' as "REF_GENERATION" ` +
		`FROM pg_catalog.pg_namespace n, pg_catalog.pg_class c LEFT JOIN pg_catalog.pg_description d ON (c.oid = d.objoid AND d.objsubid = 0 and d.classoid = 'pg_class'::regclass) ` +
		`WHERE c.relnamespace = n.oid AND n.nspname LIKE $1 AND c.relname LIKE $2 ` +
		`AND ( false OR ( c.relkind = 'r' AND n.nspname !~ '^pg_' AND n.nspname <> 'information_schema' ) ) ` +
		`ORDER BY "TABLE_TYPE","TABLE_SCHEM","TABLE_NAME"`
	rows := execFixture(t, sql, "public", "%")
	if len(rows) != 2 {
		t.Fatalf("tables = %d rows, want 2", len(rows))
	}
	if s, _ := toString(rows[0][3]); s != "TABLE" {
		t.Errorf("table type = %q, want TABLE", rows[0][3])
	}
	if s, _ := toString(rows[0][1]); s != "public" {
		t.Errorf("schema = %q", rows[0][1])
	}
}

func TestPgjdbcGetColumns(t *testing.T) {
	// landmark-truncated but shape-faithful: the derived table with
	// row_number() OVER, the relkind filter, and the three conditional
	// LIKE parameters in order (schema, table, column)
	sql := `SELECT * FROM ( ` +
		`SELECT n.nspname, c.relname, a.attname, a.atttypid, a.attnum, a.atttypmod, a.attnotnull, a.attisdropped, a.attidentity, a.attgenerated, ` +
		`row_number() OVER (PARTITION BY a.attrelid ORDER BY a.attnum) AS attnum2, ` +
		`format_type(a.atttypid, a.atttypmod) as format_type, ` +
		`a.attnotnull OR (t.typtype = 'd' AND t.typnotnull) as attnotnull2, ` +
		`nullif(ad.adsrc, '') as adsrc ` +
		`FROM pg_catalog.pg_namespace n JOIN pg_catalog.pg_class c ON (c.relnamespace = n.oid) ` +
		`JOIN pg_catalog.pg_attribute a ON (a.attrelid = c.oid) ` +
		`JOIN pg_catalog.pg_type t ON (a.atttypid = t.oid) ` +
		`LEFT JOIN pg_catalog.pg_attrdef ad ON (a.attrelid = ad.adrelid AND a.attnum = ad.adnum) ` +
		`WHERE c.relkind in ('r','p','v','f','m') and a.attnum > 0 AND NOT a.attisdropped ) c ` +
		`WHERE true AND nspname LIKE $1 AND relname LIKE $2 AND attname LIKE $3 ` +
		`ORDER BY nspname,c.relname,attnum`
	rows := execFixture(t, sql, "public", "items", "%")
	if len(rows) != 4 {
		t.Fatalf("columns = %d rows, want 4 (items' fields)", len(rows))
	}

	// column-pattern only: pgjdbc appends attname LIKE ? without the others
	rows = execFixture(t, sql, "", "docs", "body")
	if len(rows) != 1 {
		t.Fatalf("filtered columns = %d rows, want 1", len(rows))
	}
}

func TestPgjdbcGetPrimaryKeys(t *testing.T) {
	// the constraint path: conkey expanded via _pg_expandarray inside the
	// derived table, exact schema/table equality outside
	sql := `SELECT result.TABLE_CAT, result.TABLE_SCHEM, result.TABLE_NAME, result.COLUMN_NAME, result.KEY_SEQ, result.PK_NAME FROM ` +
		`(SELECT NULL AS TABLE_CAT, n.nspname AS TABLE_SCHEM, ct.relname AS TABLE_NAME, a.attname AS COLUMN_NAME, ` +
		`(information_schema._pg_expandarray(con.conkey)).n AS KEY_SEQ, con.conname AS PK_NAME ` +
		`FROM pg_catalog.pg_constraint con ` +
		`JOIN pg_catalog.pg_class ct ON (ct.oid = con.conrelid) ` +
		`JOIN pg_catalog.pg_namespace n ON (ct.relnamespace = n.oid) ` +
		`JOIN pg_catalog.pg_attribute a ON (a.attrelid = ct.oid) ` +
		`WHERE con.contype = 'p' ) result ` +
		`WHERE result.TABLE_SCHEM = $1 AND result.TABLE_NAME = $2 ` +
		`ORDER BY result.table_name, result.pk_name, result.key_seq`
	rows := execFixture(t, sql, "public", "items")
	if len(rows) != 1 {
		t.Fatalf("pk = %d rows, want 1 (items.id)", len(rows))
	}
	if s, _ := toString(rows[0][3]); s != "id" {
		t.Errorf("pk column = %q, want id", rows[0][3])
	}
	if s, _ := toString(rows[0][5]); s != "items_pkey" {
		t.Errorf("pk name = %q, want items_pkey", rows[0][5])
	}
}

func TestPgjdbcGetBestRowIdentifier(t *testing.T) {
	sql := `SELECT a.attname, a.atttypid, NULL, a.atttypmod, a.attnum, NULL, NULL, NULL FROM pg_catalog.pg_class ct ` +
		`JOIN pg_catalog.pg_attribute a ON (ct.oid = a.attrelid) ` +
		`JOIN pg_catalog.pg_namespace n ON (ct.relnamespace = n.oid) ` +
		`JOIN (SELECT i.indexrelid, i.indrelid, i.indisprimary, information_schema._pg_expandarray(i.indkey) AS keys FROM pg_catalog.pg_index i) i ` +
		`ON (a.attnum = (i.keys).x AND a.attrelid = i.indrelid) ` +
		`WHERE true AND n.nspname = $1 AND ct.relname = $2 AND i.indisprimary ORDER BY a.attnum`
	rows := execFixture(t, sql, "public", "items")
	if len(rows) != 1 {
		t.Fatalf("best row = %d rows, want 1 (items.id)", len(rows))
	}
}

func TestPgjdbcGetIndexInfo(t *testing.T) {
	// the v8.3+ getIndexInfo: derived table over pg_class/pg_namespace/
	// pg_index/pg_am with pg_get_indexdef and _pg_expandarray
	sql := `SELECT tmp.TABLE_CAT, tmp.TABLE_SCHEM, tmp.TABLE_NAME, NOT tmp.indisunique AS NON_UNIQUE, tmp.INDEX_QUALIFIER, tmp.INDEX_NAME, ` +
		`CASE WHEN tmp.indisclustered THEN 1 WHEN tmp.type = 'hash' THEN 2 ELSE 3 END AS TYPE, ` +
		`tmp.ORDINAL_POSITION, tmp.COLUMN_NAME, tmp.ASC_OR_DESC, tmp.CARDINALITY, tmp.PAGES, tmp.FILTER_CONDITION, tmp.REMARKS ` +
		`FROM (SELECT NULL AS TABLE_CAT, n.nspname AS TABLE_SCHEM, ct.relname AS TABLE_NAME, i.indisunique, i.indisclustered, ` +
		`NULL AS INDEX_QUALIFIER, ci.relname AS INDEX_NAME, trim(both '<>' from pg_get_indexdef(i.indexrelid, tmp.indkey, tmp.ORDINAL_POSITION, true)) AS COLUMN_NAME, ` +
		`CASE WHEN i.indoption[tmp.ORDINAL_POSITION - 1] & 1::smallint = 1 THEN 'D' ELSE 'A' END AS ASC_OR_DESC, ` +
		`ct.reltuples AS CARDINALITY, ct.relpages AS PAGES, pg_get_indexdef(i.indexrelid, tmp.ORDINAL_POSITION, false) AS FILTER_CONDITION, ` +
		`trim(both '<>' from pg_get_indexdef(i.indexrelid, tmp.indkey, tmp.ORDINAL_POSITION, true)) AS REMARKS ` +
		`FROM pg_catalog.pg_class ct JOIN pg_catalog.pg_namespace n ON (ct.relnamespace = n.oid) ` +
		`JOIN pg_catalog.pg_index i ON (ct.oid = i.indrelid) ` +
		`JOIN pg_catalog.pg_class ci ON (ci.oid = i.indexrelid) ` +
		`JOIN pg_catalog.pg_am am ON (ci.relam = am.oid) ` +
		`JOIN information_schema._pg_expandarray(i.indkey) AS keys ON (true), ` +
		`LATERAL (SELECT keys.n AS ORDINAL_POSITION) tmp ) tmp ` +
		`WHERE true AND tmp.TABLE_SCHEM = $1 AND tmp.TABLE_NAME = $2 AND NOT i.indisunique ORDER BY tmp.TABLE_NAME, tmp.INDEX_NAME, tmp.ORDINAL_POSITION`
	rows := execFixture(t, sql, "public", "items")
	if len(rows) != 1 {
		t.Fatalf("indexes = %d rows, want 1 (items_pkey)", len(rows))
	}
}

func TestPgjdbcGetImportedExportedKeys(t *testing.T) {
	sql := `SELECT NULL AS PKTABLE_CAT, pkn.nspname AS PKTABLE_SCHEM, pkc.relname AS PKTABLE_NAME, pka.attname AS PKCOLUMN_NAME, ` +
		`NULL AS FKTABLE_CAT, fkn.nspname AS FKTABLE_SCHEM, fkc.relname AS FKTABLE_NAME, fka.attname AS FKCOLUMN_NAME, ` +
		`pos.n AS KEY_SEQ, CASE WHEN con.confupdtype = 'c' THEN 0 WHEN con.confupdtype = 'n' THEN 2 WHEN con.confupdtype = 'd' THEN 4 WHEN con.confupdtype = 'r' THEN 1 ELSE 3 END AS UPDATE_RULE, ` +
		`CASE WHEN con.confdeltype = 'c' THEN 0 WHEN con.confdeltype = 'n' THEN 2 WHEN con.confdeltype = 'd' THEN 4 WHEN con.confdeltype = 'r' THEN 1 ELSE 3 END AS DELETE_RULE, ` +
		`con.conname AS FK_NAME, pkic.relname AS PK_NAME, ` +
		`CASE WHEN con.condeferrable AND con.condeferred THEN 5 WHEN con.condeferrable THEN 6 ELSE 7 END AS DEFERRABILITY ` +
		`FROM pg_catalog.pg_namespace pkn, pg_catalog.pg_class pkc, pg_catalog.pg_attribute pka, ` +
		`pg_catalog.pg_namespace fkn, pg_catalog.pg_class fkc, pg_catalog.pg_attribute fka, ` +
		`pg_catalog.pg_constraint con, pg_catalog.generate_series(1, 32) pos(n) ` +
		`WHERE con.contype = 'f' AND pos.n <= con.conkey cardinality ` +
		`AND pkn.oid = pkc.relnamespace AND fkn.oid = fkc.relnamespace`
	// shape is deliberately approximate — the matcher keys on generate_series
	// + contype='f', and the answer is always empty
	rows := execFixture(t, sql)
	if len(rows) != 0 {
		t.Fatalf("imported keys = %d rows, want 0", len(rows))
	}
}

func TestPgjdbcGetTypeInfo(t *testing.T) {
	sql := `SELECT t.typname,t.oid FROM pg_catalog.pg_type t JOIN pg_catalog.pg_namespace n ON (t.typnamespace = n.oid) ` +
		`WHERE n.nspname != 'pg_toast' AND (t.typrelid = 0 OR (SELECT c.relkind = 'c' FROM pg_catalog.pg_class c WHERE c.oid = t.typrelid)) ` +
		`AND (t.typelem = 0 OR (SELECT c.relkind = 'c' FROM pg_catalog.pg_class c WHERE c.oid = t.typelem))`
	rows := execFixture(t, sql)
	if len(rows) == 0 {
		t.Fatal("type info = 0 rows")
	}
	// engine-level sanity only: rows must name the catalog's base types
	var names []string
	for _, r := range rows {
		s, _ := toString(r[0])
		names = append(names, s)
	}
	if !strings.Contains(strings.Join(names, ","), "float8") {
		t.Errorf("type info missing float8: %v", names)
	}
}

func TestPgjdbcGetUDTs(t *testing.T) {
	sql := `SELECT current_database() as "TYPE_CAT", n.nspname as "TYPE_SCHEM", t.typname as "TYPE_NAME", NULL AS "CLASS_NAME", ` +
		`CASE WHEN t.typtype='c' THEN 2002 WHEN t.typtype='d' THEN 2001 ELSE 0 END AS "DATA_TYPE", ` +
		`pg_catalog.obj_description(t.oid, 'pg_type') as "REMARKS", NULL AS "BASE_TYPE" ` +
		`FROM pg_catalog.pg_type t, pg_catalog.pg_namespace n where t.typnamespace = n.oid and n.nspname != 'pg_catalog' ` +
		`and ((relkind = 'r' and relhasoids) or (t.typtype IN ('c','d') and t.typrelid = 0)) `
	rows := execFixture(t, sql)
	if len(rows) != 0 {
		t.Fatalf("udts = %d rows, want 0", len(rows))
	}
}

func TestPgjdbcGetProcedures(t *testing.T) {
	// general engine over the empty pg_proc: verbatim v11+ text
	sql := `SELECT current_database() AS "PROCEDURE_CAT", n.nspname AS "PROCEDURE_SCHEM", p.proname AS "PROCEDURE_NAME", ` +
		`NULL, NULL, NULL, d.description AS "REMARKS", 1 AS "PROCEDURE_TYPE", p.proname || '_' || p.oid AS "SPECIFIC_NAME" ` +
		`FROM pg_catalog.pg_namespace n, pg_catalog.pg_proc p ` +
		`LEFT JOIN pg_catalog.pg_description d ON (p.oid=d.objoid) ` +
		`LEFT JOIN pg_catalog.pg_class c ON (d.classoid=c.oid AND c.relname='pg_proc') ` +
		`LEFT JOIN pg_catalog.pg_namespace pn ON (c.relnamespace=pn.oid AND pn.nspname='pg_catalog') ` +
		`WHERE p.pronamespace=n.oid AND p.prokind='p' ` +
		`ORDER BY "PROCEDURE_SCHEM", "PROCEDURE_NAME", p.oid::text`
	rows := execFixture(t, sql)
	if len(rows) != 0 {
		t.Fatalf("procedures = %d rows, want 0", len(rows))
	}
}

func TestPgjdbcGetFunctions(t *testing.T) {
	sql := `SELECT current_database() AS "FUNCTION_CAT", n.nspname AS "FUNCTION_SCHEM", p.proname AS "FUNCTION_NAME", ` +
		`d.description AS "REMARKS", ` +
		`CASE WHEN (format_type(p.prorettype, null) = 'unknown') THEN 0 WHEN (substring(pg_get_function_result(p.oid) from 0 for 6) = 'TABLE') OR (substring(pg_get_function_result(p.oid) from 0 for 6) = 'SETOF') THEN 2 ELSE 1 END AS "FUNCTION_TYPE", ` +
		`p.proname || '_' || p.oid AS "SPECIFIC_NAME" ` +
		`FROM pg_catalog.pg_proc p INNER JOIN pg_catalog.pg_namespace n ON (p.pronamespace = n.oid) ` +
		`LEFT JOIN pg_catalog.pg_description d ON (p.oid=d.objoid) ` +
		`LEFT JOIN pg_catalog.pg_class c ON (d.classoid=c.oid AND c.relname='pg_proc') ` +
		`LEFT JOIN pg_catalog.pg_namespace pn ON (c.relnamespace=pn.oid AND pn.nspname='pg_catalog') ` +
		`WHERE true ORDER BY "FUNCTION_SCHEM", "FUNCTION_NAME", p.oid::text`
	rows := execFixture(t, sql)
	if len(rows) != 0 {
		t.Fatalf("functions = %d rows, want 0", len(rows))
	}
}

func TestPgjdbcGetProcedureColumns(t *testing.T) {
	sql := `SELECT current_database() AS current_database, n.nspname,p.proname,p.prorettype,p.proargtypes, ` +
		`t.typtype,t.typrelid, p.proargnames, p.proargmodes, p.proallargtypes, p.oid ` +
		`FROM pg_catalog.pg_proc p, pg_catalog.pg_namespace n, pg_catalog.pg_type t ` +
		`WHERE p.pronamespace=n.oid AND p.prorettype=t.oid ` +
		`ORDER BY n.nspname, p.proname, p.oid::text`
	rows := execFixture(t, sql)
	if len(rows) != 0 {
		t.Fatalf("procedure columns = %d rows, want 0", len(rows))
	}
}

func TestPgjdbcGetTablePrivileges(t *testing.T) {
	sql := `SELECT current_database() AS current_database, n.nspname, c.relname, r.rolname, c.relacl ` +
		`FROM pg_catalog.pg_namespace n, pg_catalog.pg_class c, pg_catalog.pg_roles r ` +
		`WHERE c.relnamespace = n.oid AND c.relowner = r.oid ` +
		`AND n.nspname LIKE $1 AND c.relname LIKE $2 ` +
		`ORDER BY n.nspname, c.relname, r.rolname`
	rows := execFixture(t, sql, "public", "%")
	if len(rows) != 2 {
		t.Fatalf("table privileges = %d rows, want 2 (one per collection)", len(rows))
	}
	if s, _ := toString(rows[0][3]); s != "lyrebird" {
		t.Errorf("grantee = %q, want lyrebird", rows[0][3])
	}
}
