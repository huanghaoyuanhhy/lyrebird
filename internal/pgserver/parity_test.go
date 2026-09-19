package pgserver

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"github.com/huanghaoyuanhhy/lyrebird/internal/milvustest"
	"github.com/huanghaoyuanhhy/lyrebird/internal/store"
)

// pgDSNEnv carries the DSN of the reference Postgres (with the pgvector
// extension) the parity suite replays the same SQL against;
// docker-compose.e2e.yml serves one on 127.0.0.1:15432. The suite skips
// when it is unset.
const pgDSNEnv = "LYREBIRD_TEST_PG_DSN"

// TestPgserverParity replays one SQL statement against a real Postgres
// with pgvector and against the gateway over the wire, comparing column
// order and cell-for-cell row values. The matrix keeps to the shared
// capability set — the gateway's deliberate divergences (LIMIT mandatory,
// the metric bound to the field's index, unsupported operators) are
// asserted by the full-stack suite, not here.
func TestPgserverParity(t *testing.T) {
	cfg, ok := milvustest.FromEnv()
	if !ok {
		t.Skipf("set %s and %s to run the parity suite", milvustest.URIEnv, milvustest.TokenEnv)
	}
	dsn := os.Getenv(pgDSNEnv)
	if dsn == "" {
		t.Skipf("set %s to run the parity suite", pgDSNEnv)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	if err := milvustest.Seed(ctx, cfg); err != nil {
		t.Fatalf("seed fixture: %v", err)
	}
	seedReferencePostgres(t, ctx, dsn)

	exec, err := store.NewMilvusExecutor(ctx, store.MilvusConfig{URI: cfg.URI, Token: cfg.Token})
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	defer exec.Close(ctx)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := New(exec, zap.NewNop())
	go srv.serve(ctx, ln)
	t.Cleanup(func() { ln.Close() })

	ref, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect reference postgres: %v", err)
	}
	defer ref.Close(context.Background())
	gw, err := pgx.Connect(ctx, fmt.Sprintf("postgres://lyrebird:lyrebird@%s/postgres?sslmode=disable", ln.Addr()))
	if err != nil {
		t.Fatalf("connect gateway: %v", err)
	}
	defer gw.Close(context.Background())

	// Vector queries split by fixture metric: the store fixture carries an
	// L2 index (so <-> there), the vec fixture a cosine one (so <=> there)
	// — the gateway binds the metric to the index, reference Postgres
	// answers both on any table.
	cases := []struct{ name, sql string }{
		{"select star rides the schema order",
			fmt.Sprintf("SELECT * FROM %s LIMIT 1", milvustest.Collection)},
		{"where + order + limit",
			fmt.Sprintf("SELECT name FROM %s WHERE price > 10 AND active = true ORDER BY price DESC LIMIT 2", milvustest.Collection)},
		{"in and between",
			fmt.Sprintf("SELECT id FROM %s WHERE name IN ('bravo', 'echo') AND price BETWEEN 15 AND 25 ORDER BY id LIMIT 10", milvustest.Collection)},
		{"equals null matches nothing",
			fmt.Sprintf("SELECT id FROM %s WHERE price = NULL LIMIT 10", milvustest.Collection)},
		{"is null reads the null rows",
			fmt.Sprintf("SELECT id FROM %s WHERE note IS NULL ORDER BY id LIMIT 10", milvustest.Collection)},
		{"l2 distance order",
			fmt.Sprintf("SELECT id FROM %s ORDER BY emb <-> '[0.1, 0.2, 0.3, 0.4]' LIMIT 2", milvustest.Collection)},
		{"l2 distance with filter and offset",
			fmt.Sprintf("SELECT id FROM %s WHERE price > 10 ORDER BY emb <-> '[0.1, 0.2, 0.3, 0.4]' LIMIT 2 OFFSET 1", milvustest.Collection)},
		{"cosine distance order",
			fmt.Sprintf("SELECT id FROM %s ORDER BY emb <=> '[1, 0, 0, 0]' LIMIT 5", milvustest.VectorCollection)},
		{"cosine distance with filter",
			fmt.Sprintf("SELECT id FROM %s WHERE tag = 'odd' ORDER BY emb <=> '[1, 0, 0, 0]' LIMIT 3", milvustest.VectorCollection)},
		{"text fixture as plain columns",
			fmt.Sprintf("SELECT id, body FROM %s ORDER BY id LIMIT 5", milvustest.TextCollection)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			refCols, refRows := parityRows(t, ctx, ref, tc.sql)
			gwCols, gwRows := parityRows(t, ctx, gw, tc.sql)
			if strings.Join(gwCols, ",") != strings.Join(refCols, ",") {
				t.Errorf("columns = %v, want %v", gwCols, refCols)
			}
			if len(gwRows) != len(refRows) {
				t.Fatalf("rows = %d, want %d\nreference: %v\ngateway:   %v", len(gwRows), len(refRows), refRows, gwRows)
			}
			for i := range refRows {
				if strings.Join(gwRows[i], "|") != strings.Join(refRows[i], "|") {
					t.Errorf("row %d = %v, want %v", i, gwRows[i], refRows[i])
				}
			}
		})
	}
}

// parityRows drains one statement into canonical cells, closing the Rows
// before the connection is reused (the wire suite learned this the hard
// way: unclosed Rows poison the next query on the same connection).
func parityRows(t *testing.T, ctx context.Context, conn *pgx.Conn, sql string) ([]string, [][]string) {
	t.Helper()
	rows, err := conn.Query(ctx, sql)
	if err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	defer rows.Close()
	var cols []string
	for _, fd := range rows.FieldDescriptions() {
		cols = append(cols, fd.Name)
	}
	var out [][]string
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			t.Fatalf("collect %q: %v", sql, err)
		}
		cells := make([]string, len(vals))
		for i, v := range vals {
			cells[i] = canonCell(v)
		}
		out = append(out, cells)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows %q: %v", sql, err)
	}
	return cols, out
}

// canonCell renders one decoded cell so the two backends can be compared
// cell-for-cell: the gateway answers every numeric column in float8 while
// reference Postgres keeps int8/int4 native, and vectors arrive as a json
// array on one side and pgvector's text form on the other — every cell
// meets as a string.
func canonCell(v any) string {
	switch x := v.(type) {
	case nil:
		return "<nil>"
	case bool:
		return strconv.FormatBool(x)
	case string:
		return strings.ReplaceAll(x, " ", "")
	case int8:
		return strconv.FormatInt(int64(x), 10)
	case int16:
		return strconv.FormatInt(int64(x), 10)
	case int32:
		return strconv.FormatInt(int64(x), 10)
	case int64:
		return strconv.FormatInt(x, 10)
	case float32:
		return strconv.FormatFloat(float64(x), 'f', -1, 64)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = canonCell(e)
		}
		return "[" + strings.Join(parts, ",") + "]"
	default:
		return fmt.Sprintf("%v", x)
	}
}

// seedReferencePostgres mirrors the milvus fixtures into reference tables:
// same names, same rows, pgvector columns for the embeddings. Vector
// literals ride inline — an untyped literal resolves to the column type,
// while a text-typed parameter would need a cast pgvector does not ship.
func seedReferencePostgres(t *testing.T, ctx context.Context, dsn string) {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect reference postgres: %v", err)
	}
	defer conn.Close(context.Background())

	for _, stmt := range []string{
		"CREATE EXTENSION IF NOT EXISTS vector",
		"DROP TABLE IF EXISTS " + milvustest.Collection,
		fmt.Sprintf(`CREATE TABLE %s (
			id bigint PRIMARY KEY, name text, price double precision, qty int,
			active boolean, created_ms bigint, note text, emb vector(4))`, milvustest.Collection),
		"DROP TABLE IF EXISTS " + milvustest.TextCollection,
		fmt.Sprintf("CREATE TABLE %s (id bigint PRIMARY KEY, body text)", milvustest.TextCollection),
		"DROP TABLE IF EXISTS " + milvustest.VectorCollection,
		fmt.Sprintf("CREATE TABLE %s (id bigint PRIMARY KEY, tag text, emb vector(4))", milvustest.VectorCollection),
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	// rows mirror milvustest.seedDocs exactly: prices 10.5/20/5/30/15,
	// note null on ids 2 and 5, vectors growing away from [0.1,0.2,0.3,0.4].
	storeRows := []struct {
		id      int64
		name    string
		price   float64
		qty     int32
		active  bool
		created int64
		note    any // nil renders SQL NULL
		emb     string
	}{
		{1, "alpha", 10.5, 1, true, 1700000000000, "alpha note", "[0.1,0.2,0.3,0.4]"},
		{2, "bravo", 20.0, 2, false, 1700000100000, nil, "[0.5,0.6,0.7,0.8]"},
		{3, "charlie", 5.0, 3, true, 1700000200000, "charlie note", "[0.9,1.0,1.1,1.2]"},
		{4, "delta", 30.0, 4, true, 1700000300000, "delta note", "[1.3,1.4,1.5,1.6]"},
		{5, "echo", 15.0, 5, false, 1700000400000, nil, "[1.7,1.8,1.9,2.0]"},
	}
	for _, r := range storeRows {
		if _, err := conn.Exec(ctx,
			fmt.Sprintf("INSERT INTO %s VALUES ($1,$2,$3,$4,$5,$6,$7,'%s')", milvustest.Collection, r.emb),
			r.id, r.name, r.price, r.qty, r.active, r.created, r.note); err != nil {
			t.Fatalf("insert store row %d: %v", r.id, err)
		}
	}

	textRows := []struct {
		id   int64
		body string
	}{
		{1, "the quick brown fox jumps over the lazy dog"},
		{2, "a fast brown dog barks at night"},
		{3, "completely unrelated filler text lives here"},
		{4, "quick quick slow turtle"},
		{5, "foxes and quick rabbits play"},
	}
	for _, r := range textRows {
		if _, err := conn.Exec(ctx,
			fmt.Sprintf("INSERT INTO %s VALUES ($1,$2)", milvustest.TextCollection),
			r.id, r.body); err != nil {
			t.Fatalf("insert text row %d: %v", r.id, err)
		}
	}

	vecRows := []struct {
		id  int64
		tag string
		emb string
	}{
		{1, "odd", "[1.0,0.0,0.0,0.0]"},
		{2, "even", "[0.8,0.2,0.0,0.0]"},
		{3, "odd", "[0.6,0.4,0.0,0.0]"},
		{4, "even", "[0.4,0.6,0.0,0.0]"},
		{5, "odd", "[0.2,0.8,0.0,0.0]"},
	}
	for _, r := range vecRows {
		if _, err := conn.Exec(ctx,
			fmt.Sprintf("INSERT INTO %s VALUES ($1,$2,'%s')", milvustest.VectorCollection, r.emb),
			r.id, r.tag); err != nil {
			t.Fatalf("insert vec row %d: %v", r.id, err)
		}
	}
}
