package pgserver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.uber.org/zap"

	"github.com/huanghaoyuanhhy/lyrebird/internal/catalog"
	"github.com/huanghaoyuanhhy/lyrebird/internal/store"
	"github.com/huanghaoyuanhhy/lyrebird/internal/translate"
)

// fakeExecutor stands in for the Milvus executor: fixed schema per table and
// one canned result, so the wire layer is exercised without a cluster. It
// also serves the catalog's introspection surface — the catalog projects its
// schemas as the default database's tables, with optional per-database
// overrides for the routing tests.
type fakeExecutor struct {
	schemas map[string]translate.Schema
	// alt holds schemas for databases other than the default; absent
	// databases answer not-found like the real store.
	alt    map[string]map[string]translate.Schema
	result *store.SearchResult
	err    error
}

func (f *fakeExecutor) Schema(ctx context.Context, collection string) (translate.Schema, error) {
	if schema, ok := f.schemas[collection]; ok {
		return schema, nil
	}
	return nil, fmt.Errorf("%w: %s", store.ErrCollectionNotFound, collection)
}

func (f *fakeExecutor) Search(ctx context.Context, collection string, plan *translate.Plan) (*store.SearchResult, error) {
	if f.err != nil {
		return nil, f.err
	}
	// Honor the plan's window the way the real store does: LIMIT 0 counts
	// only, a capped fetch truncates, no cap returns everything fetched.
	hits := f.result.Hits
	switch {
	case plan.Limit == 0:
		hits = nil
	case plan.Limit < len(hits):
		hits = hits[:plan.Limit]
	}
	return &store.SearchResult{Total: f.result.Total, Hits: hits}, nil
}

// Database implements store.Cluster: one fake, any database.
func (f *fakeExecutor) Database(db string) (store.Executor, error) { return f, nil }

// DefaultDatabase implements store.Cluster.
func (f *fakeExecutor) DefaultDatabase() string { return "default" }

// Databases implements store.Cluster.
func (f *fakeExecutor) Databases(ctx context.Context) ([]string, error) {
	return []string{"default"}, nil
}

// ListCollections implements store.Cluster from the fake's schemas.
func (f *fakeExecutor) ListCollections(ctx context.Context, db string) ([]string, error) {
	schemas, ok := f.dbSchemas(db)
	names := make([]string, 0, len(schemas))
	for name := range schemas {
		names = append(names, name)
	}
	if !ok {
		return names, nil // an empty database, not an error
	}
	sort.Strings(names)
	return names, nil
}

// Collection implements store.Cluster: field metadata from the fake schema.
func (f *fakeExecutor) Collection(ctx context.Context, db, name string) (catalog.CollectionMeta, error) {
	schemas, _ := f.dbSchemas(db)
	schema, ok := schemas[name]
	if !ok {
		return catalog.CollectionMeta{}, fmt.Errorf("%w: %s", store.ErrCollectionNotFound, name)
	}
	meta := catalog.CollectionMeta{Name: name}
	for _, field := range schema.Fields() {
		meta.Fields = append(meta.Fields, catalog.FieldMeta{Name: field, Type: schema.FieldType(field)})
	}
	return meta, nil
}

// dbSchemas picks the schema set a database name resolves to.
func (f *fakeExecutor) dbSchemas(db string) (map[string]translate.Schema, bool) {
	switch db {
	case "", "default":
		return f.schemas, true
	default:
		schemas, ok := f.alt[db]
		return schemas, ok
	}
}

// startWireServer boots a Server on 127.0.0.1:0 and returns its address.
func startWireServer(t *testing.T, exec store.Cluster) net.Addr {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := New(exec, zap.NewNop())
	ctx, cancel := context.WithCancel(context.Background())
	go srv.serve(ctx, ln)
	t.Cleanup(func() {
		cancel()
	})
	return ln.Addr()
}

// query opens a pgx connection in the given exec mode and runs one SELECT,
// returning the raw rows. The connection lives until the test ends — closing
// it here would sever the returned rows mid-stream. modes cover both
// protocol flavors clients use: extended (Parse/Bind/Execute — pgx's
// default) and simple.
func query(t *testing.T, addr net.Addr, mode string, sql string) pgx.Rows {
	t.Helper()
	conn := connect(t, addr, mode)
	t.Cleanup(func() { conn.Close(context.Background()) })
	rows, err := conn.Query(context.Background(), sql)
	if err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return rows
}

func connect(t *testing.T, addr net.Addr, mode string) *pgx.Conn {
	t.Helper()
	dsn := fmt.Sprintf("postgres://lyrebird:lyrebird@%s/postgres?sslmode=disable&default_query_exec_mode=%s", addr, mode)
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect (%s): %v", mode, err)
	}
	return conn
}

func testExecutor() *fakeExecutor {
	return &fakeExecutor{
		schemas: map[string]translate.Schema{
			"items": translate.MapSchema{
				"id":    translate.TypeNumber, // Int64 pk
				"name":  translate.TypeKeyword,
				"price": translate.TypeNumber,
				"note":  translate.TypeKeyword, // nullable
			},
		},
		result: &store.SearchResult{
			Total: 2,
			Hits: []store.Hit{
				{ID: "1", Source: map[string]any{"id": int64(1), "name": "alpha", "price": float64(10.5)}},
				{ID: "2", Source: map[string]any{"id": int64(2), "name": "bravo", "price": float64(20), "note": nil}},
			},
		},
	}
}

func TestQueryRoundtrip(t *testing.T) {
	for _, mode := range []string{"cache_statement", "simple_protocol"} {
		t.Run(mode, func(t *testing.T) {
			addr := startWireServer(t, testExecutor())

			t.Run("column list with values", func(t *testing.T) {
				rows := query(t, addr, mode, "SELECT name, price FROM items ORDER BY price LIMIT 2")
				defer rows.Close()

				fields := rows.FieldDescriptions()
				if len(fields) != 2 || fields[0].Name != "name" || fields[1].Name != "price" {
					t.Fatalf("fields = %v", fields)
				}

				type cell struct {
					name  string
					price float64
				}
				var got []cell
				for rows.Next() {
					var c cell
					if err := rows.Scan(&c.name, &c.price); err != nil {
						t.Fatalf("scan: %v", err)
					}
					got = append(got, c)
				}
				if err := rows.Err(); err != nil {
					t.Fatalf("rows: %v", err)
				}
				want := []cell{{"alpha", 10.5}, {"bravo", 20}}
				if len(got) != len(want) {
					t.Fatalf("rows = %v, want %v", got, want)
				}
				for i := range want {
					if got[i] != want[i] {
						t.Errorf("row %d = %v, want %v", i, got[i], want[i])
					}
				}
			})

			t.Run("select star expands schema fields", func(t *testing.T) {
				rows := query(t, addr, mode, "SELECT * FROM items LIMIT 2")
				defer rows.Close()

				fields := rows.FieldDescriptions()
				var names []string
				for _, f := range fields {
					names = append(names, f.Name)
				}
				// MapSchema has no storage order: alphabetical expansion.
				want := []string{"id", "name", "note", "price"}
				if len(names) != len(want) {
					t.Fatalf("columns = %v, want %v", names, want)
				}
				for i := range want {
					if names[i] != want[i] {
						t.Errorf("column %d = %s, want %s", i, names[i], want[i])
					}
				}
			})

			t.Run("null cell scans as nil", func(t *testing.T) {
				rows := query(t, addr, mode, "SELECT note FROM items WHERE id = 2 LIMIT 1")
				defer rows.Close()

				if !rows.Next() {
					t.Fatalf("no rows: %v", rows.Err())
				}
				var note *string
				if err := rows.Scan(&note); err != nil {
					t.Fatalf("scan: %v", err)
				}
				if note != nil {
					t.Errorf("note = %q, want NULL", *note)
				}
			})

			t.Run("limit zero returns no rows", func(t *testing.T) {
				rows := query(t, addr, mode, "SELECT name FROM items LIMIT 0")
				defer rows.Close()

				if rows.Next() {
					t.Errorf("unexpected row")
				}
				if len(rows.FieldDescriptions()) != 1 {
					t.Errorf("field descriptions missing despite zero rows")
				}
			})
		})
	}
}

// TestSQLErrorsNative asserts the fail-fast classes surface as PG errors
// with the translator's SQLSTATE codes.
func TestSQLErrorsNative(t *testing.T) {
	cases := []struct {
		sql  string
		code string
	}{
		{"SELECT name FROM items", "0A000"},        // LIMIT required
		{"SELECT FROM items LIMIT 1", "42601"},     // syntax
		{"DELETE FROM items", "0A000"},             // read-only subset
		{"SELECT * FROM no_such LIMIT 1", "42P01"}, /* absent table */
	}
	for _, mode := range []string{"cache_statement", "simple_protocol"} {
		t.Run(mode, func(t *testing.T) {
			addr := startWireServer(t, testExecutor())
			conn := connect(t, addr, mode)
			defer conn.Close(context.Background())

			for _, tc := range cases {
				rows, err := conn.Query(context.Background(), tc.sql)
				if err == nil {
					for rows.Next() {
					}
					err = rows.Err()
				}
				var pgErr *pgconn.PgError
				if !errors.As(err, &pgErr) {
					t.Errorf("%q: got %v, want a PgError", tc.sql, err)
					continue
				}
				if pgErr.Code != tc.code {
					t.Errorf("%q: SQLSTATE = %s (%s), want %s", tc.sql, pgErr.Code, pgErr.Message, tc.code)
				}
			}
		})
	}
}

// TestMissingTableMessage asserts the native wording for an absent relation.
func TestMissingTableMessage(t *testing.T) {
	addr := startWireServer(t, testExecutor())
	conn := connect(t, addr, "simple_protocol")
	defer conn.Close(context.Background())

	_, err := conn.Exec(context.Background(), "SELECT * FROM ghost LIMIT 1")
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Message != `relation "ghost" does not exist` {
		t.Errorf("err = %v, want native relation-does-not-exist message", err)
	}
}

// TestJSONColumnRoundtrip encodes an unknown-typed field (SDK JSON bytes)
// through the json OID.
func TestJSONColumnRoundtrip(t *testing.T) {
	exec := &fakeExecutor{
		schemas: map[string]translate.Schema{
			"docs": translate.MapSchema{"id": translate.TypeNumber, "meta": translate.TypeUnknown},
		},
		result: &store.SearchResult{
			Total: 1,
			Hits: []store.Hit{
				{ID: "1", Source: map[string]any{"id": int64(1), "meta": []byte(`{"k":"v"}`)}},
			},
		},
	}
	addr := startWireServer(t, exec)

	rows := query(t, addr, "cache_statement", "SELECT meta FROM docs LIMIT 1")
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("no rows: %v", rows.Err())
	}
	var meta string
	if err := rows.Scan(&meta); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if meta != `{"k":"v"}` {
		t.Errorf("meta = %q", meta)
	}
}

// TestContextCancellationSurfacesAsQueryCanceled checks the ctx path (the
// store fails on a closed context rather than at the wire layer).
func TestContextCancellationSurfacesAsQueryCanceled(t *testing.T) {
	exec := &fakeExecutor{
		schemas: map[string]translate.Schema{"items": translate.MapSchema{"name": translate.TypeKeyword}},
		err:     context.DeadlineExceeded,
	}
	addr := startWireServer(t, exec)
	conn := connect(t, addr, "simple_protocol")
	defer conn.Close(context.Background())

	_, err := conn.Exec(context.Background(), "SELECT name FROM items LIMIT 1")
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "57014" {
		t.Errorf("err = %v, want SQLSTATE 57014", err)
	}
}

// TestConcurrentConnections guards the per-connection state psql-wire keeps:
// two clients querying at once must not interfere.
func TestConcurrentConnections(t *testing.T) {
	addr := startWireServer(t, testExecutor())

	done := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			conn, err := pgx.Connect(context.Background(), fmt.Sprintf("postgres://l:l@%s/postgres?sslmode=disable", addr))
			if err != nil {
				done <- err
				return
			}
			defer conn.Close(context.Background())
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			rows, err := conn.Query(ctx, "SELECT name FROM items LIMIT 1")
			if err != nil {
				done <- err
				return
			}
			rows.Close()
			done <- rows.Err()
		}()
	}
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Errorf("concurrent client %d: %v", i, err)
		}
	}
}
