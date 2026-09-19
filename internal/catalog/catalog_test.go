package catalog

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/huanghaoyuanhhy/lyrebird/internal/translate"
)

// fakeProvider serves two collections in two databases, mirroring the real
// provider's shape without Milvus.
type fakeProvider struct {
	databases   []string
	collections map[string][]CollectionMeta
}

func (p *fakeProvider) Databases(ctx context.Context) ([]string, error) {
	return p.databases, nil
}

func (p *fakeProvider) ListCollections(ctx context.Context, db string) ([]string, error) {
	var names []string
	for _, c := range p.collections[db] {
		names = append(names, c.Name)
	}
	sort.Strings(names)
	return names, nil
}

func (p *fakeProvider) Collection(ctx context.Context, db, name string) (CollectionMeta, error) {
	for _, c := range p.collections[db] {
		if c.Name == name {
			return c, nil
		}
	}
	return CollectionMeta{}, fmt.Errorf("collection not found: %s", name)
}

func testProvider() *fakeProvider {
	return &fakeProvider{
		databases: []string{"default", "second"},
		collections: map[string][]CollectionMeta{
			"default": {
				{Name: "items", Fields: []FieldMeta{
					{Name: "id", Type: translate.TypeNumber, PrimaryKey: true},
					{Name: "name", Type: translate.TypeKeyword, MaxLength: 128},
					{Name: "price", Type: translate.TypeNumber, Nullable: true},
					{Name: "emb", Type: translate.TypeVector, Dim: 4},
				}},
				{Name: "docs", Fields: []FieldMeta{
					{Name: "id", Type: translate.TypeKeyword, PrimaryKey: true},
					{Name: "body", Type: translate.TypeText},
				}},
			},
			"second": {
				{Name: "other", Fields: []FieldMeta{
					{Name: "id", Type: translate.TypeNumber, PrimaryKey: true},
				}},
			},
		},
	}
}

// runQuery prepares and executes against the test provider, returning cell
// values raw for comparison.
func runQuery(t *testing.T, sql string, params ...any) [][]any {
	t.Helper()
	st, err := Prepare(sql)
	if err != nil {
		t.Fatalf("prepare %q: %v", sql, err)
	}
	rows, err := st.Exec(context.Background(), testProvider(), "default", params)
	if err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
	return rows
}

func TestScalarSelects(t *testing.T) {
	cases := []struct {
		sql  string
		want string
	}{
		{"SELECT 1", "1"},
		{"SELECT version()", VersionString},
		{"SELECT current_database()", "default"},
		{"SELECT current_user", "lyrebird"},
		{"SELECT current_schema()", "public"},
		{"SELECT pg_catalog.set_config('search_path', '', false)", ""},
	}
	for _, tc := range cases {
		rows := runQuery(t, tc.sql)
		if len(rows) != 1 {
			t.Fatalf("%q: %d rows, want 1", tc.sql, len(rows))
		}
		got := ""
		if rows[0][0] != nil {
			got, _ = toString(rows[0][0])
		}
		if got != tc.want {
			t.Errorf("%q = %q, want %q", tc.sql, got, tc.want)
		}
	}
}

func TestPgDatabase(t *testing.T) {
	rows := runQuery(t, "SELECT datname FROM pg_database WHERE datallowconn = true ORDER BY datname")
	var got []string
	for _, r := range rows {
		s, _ := toString(r[0])
		got = append(got, s)
	}
	if strings.Join(got, ",") != "default,second" {
		t.Errorf("databases = %v", got)
	}
}

func TestPgSettingsMaxIndexKeys(t *testing.T) {
	// pgjdbc getMaxIndexKeys: exactly this shape
	rows := runQuery(t, "SELECT setting FROM pg_catalog.pg_settings WHERE name='max_index_keys'")
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if s, _ := toString(rows[0][0]); s != "32" {
		t.Errorf("setting = %q, want 32", rows[0][0])
	}
}

func TestCurrentSchemasSubscript(t *testing.T) {
	// pgjdbc getSchemas compares pg_temp names against current_schemas(true)[1]
	rows := runQuery(t, "SELECT current_schemas(true)[1]")
	if s, _ := toString(rows[0][0]); s != "pg_catalog" {
		t.Errorf("current_schemas(true)[1] = %q", rows[0][0])
	}
}

func TestPgNamespaceFiltered(t *testing.T) {
	// pgjdbc getSchemas shape: single table, regex + OR filters, quoted alias
	sql := `SELECT nspname AS "TABLE_SCHEM", current_database() AS "TABLE_CATALOG" FROM pg_catalog.pg_namespace ` +
		`WHERE nspname <> 'pg_toast' AND (nspname !~ '^pg_temp_' OR nspname = current_schemas(true)[1]) ` +
		`AND nspname <> 'pg_toast_temp_1' AND (nspname !~ '^pg_toast_temp_' OR nspname = current_schemas(true)[1]) ` +
		`ORDER BY "TABLE_SCHEM"`
	rows := runQuery(t, sql)
	if len(rows) != 1 {
		t.Fatalf("schemas = %d rows, want exactly public", len(rows))
	}
	if s, _ := toString(rows[0][0]); s != "public" {
		t.Errorf("schema = %q, want public", rows[0][0])
	}
}

func TestPgClassJoinNamespace(t *testing.T) {
	// the pgjdbc getTables core: join + relkind filter + pattern parameter
	sql := `SELECT c.relname, n.nspname, c.relkind FROM pg_catalog.pg_class c ` +
		`LEFT JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace ` +
		`WHERE c.relnamespace = n.oid AND c.relkind = 'r' AND n.nspname !~ '^pg_' ` +
		`AND c.relname LIKE $1 ORDER BY c.relname`
	rows := runQuery(t, sql, "%o%")
	var got []string
	for _, r := range rows {
		s, _ := toString(r[0])
		got = append(got, s)
	}
	if strings.Join(got, ",") != "docs" {
		t.Errorf("tables matching %%o%% = %v, want [docs]", got)
	}
}

func TestPgAttributeRows(t *testing.T) {
	rows := runQuery(t, `SELECT a.attname, a.attnum FROM pg_attribute a WHERE a.attname = 'price'`)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if s, _ := toString(rows[0][0]); s != "price" {
		t.Errorf("attname = %q", rows[0][0])
	}
}

func TestFormatType(t *testing.T) {
	// all string fields ride the text OID (the settled pg-wire decision), so
	// format_type answers "text" for them; numbers answer float8
	rows := runQuery(t, "SELECT format_type(a.atttypid, a.atttypmod) FROM pg_attribute a WHERE a.attname = 'name'")
	if s, _ := toString(rows[0][0]); s != "text" {
		t.Errorf("format_type(keyword) = %q, want text", rows[0][0])
	}
	rows = runQuery(t, "SELECT format_type(a.atttypid, a.atttypmod) FROM pg_attribute a WHERE a.attname = 'price'")
	if s, _ := toString(rows[0][0]); s != "float8" {
		t.Errorf("format_type(float8) = %q", rows[0][0])
	}
}

func TestCaseRelkindMapping(t *testing.T) {
	// the pgjdbc getTables TABLE_TYPE CASE, exercised over the real columns
	sql := `SELECT CASE WHEN c.relkind = 'r' THEN 'TABLE' WHEN c.relkind = 'v' THEN 'VIEW' ELSE 'OTHER' END ` +
		`FROM pg_class c ORDER BY 1`
	rows := runQuery(t, sql)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 collections", len(rows))
	}
	if s, _ := toString(rows[0][0]); s != "TABLE" {
		t.Errorf("table type = %q, want TABLE", rows[0][0])
	}
}

func TestOrderByOutputAliasAndLimitOffset(t *testing.T) {
	rows := runQuery(t, "SELECT relname AS n FROM pg_class ORDER BY n DESC LIMIT 1 OFFSET 1")
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if s, _ := toString(rows[0][0]); s != "docs" {
		t.Errorf("second-to-last = %q, want docs", rows[0][0])
	}
}

func TestDistinct(t *testing.T) {
	rows := runQuery(t, "SELECT DISTINCT relkind FROM pg_class")
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 (all relkind 'r')", len(rows))
	}
}

func TestUnknownTableFails(t *testing.T) {
	if _, err := Prepare("SELECT * FROM no_such"); err == nil {
		t.Fatal("expected 42P01 for unknown table")
	} else if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("err = %v, want relation-does-not-exist", err)
	}
}

func TestMixedCatalogAndCollectionFails(t *testing.T) {
	// a collection table is not in the registry, so prepare rejects the mix
	if _, err := Prepare("SELECT * FROM pg_class, items"); err == nil {
		t.Fatal("expected failure mixing catalog and collection tables")
	}
}

func TestRightJoinRejected(t *testing.T) {
	if _, err := Prepare("SELECT * FROM pg_class c RIGHT JOIN pg_namespace n ON n.oid = c.relnamespace"); err == nil {
		t.Fatal("expected RIGHT JOIN rejection")
	}
}

func TestNonWhitelistedFunctionFails(t *testing.T) {
	if _, err := Prepare("SELECT pg_sleep(1)"); err == nil {
		t.Fatal("expected 42883 for non-whitelisted function")
	}
}

func TestParamRefs(t *testing.T) {
	rows := runQuery(t, "SELECT relname FROM pg_class WHERE relname = $1", "items")
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if s, _ := toString(rows[0][0]); s != "items" {
		t.Errorf("relname = %q", rows[0][0])
	}
}

func TestDatabaseViewIsolation(t *testing.T) {
	// catalog data follows the database argument, not a global
	st, err := Prepare("SELECT relname FROM pg_class ORDER BY relname")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := st.Exec(context.Background(), testProvider(), "second", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if s, _ := toString(rows[0][0]); s != "other" {
		t.Errorf("relname in second db = %q, want other", rows[0][0])
	}
}

func TestInformationSchema(t *testing.T) {
	rows := runQuery(t, `SELECT table_name FROM information_schema.tables WHERE table_schema = 'public' ORDER BY table_name`)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	rows = runQuery(t, `SELECT column_name, data_type FROM information_schema.columns `+
		`WHERE table_name = 'items' ORDER BY ordinal_position`)
	if len(rows) != 4 {
		t.Fatalf("columns = %d rows, want 4", len(rows))
	}
	if s, _ := toString(rows[3][0]); s != "emb" {
		t.Errorf("last column = %q, want emb (storage order)", rows[3][0])
	}
	if s, _ := toString(rows[1][1]); s != "text" {
		t.Errorf("data_type = %q, want text", rows[1][1])
	}
}

func TestCoerceArray(t *testing.T) {
	got := Coerce(OIDInt2Array, []any{float64(1), float64(2)})
	if ints, ok := got.([]int16); !ok || len(ints) != 2 || ints[0] != 1 || ints[1] != 2 {
		t.Errorf("coerce conkey = %#v", got)
	}
}
