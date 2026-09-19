package pgserver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.uber.org/zap"

	"github.com/huanghaoyuanhhy/lyrebird/internal/milvustest"
	"github.com/huanghaoyuanhhy/lyrebird/internal/store"
)

// TestPgserverFullStackE2E drives the real wire surface: a pgx client over
// TCP into the production pgserver, which translates and executes against a
// real MilvusExecutor and the seeded fixture collections. It skips unless
// LYREBIRD_TEST_MILVUS_URI / _TOKEN are set.
func TestPgserverFullStackE2E(t *testing.T) {
	cfg, ok := milvustest.FromEnv()
	if !ok {
		t.Skipf("set %s and %s to run the full-stack e2e suite", milvustest.URIEnv, milvustest.TokenEnv)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	if err := milvustest.Seed(ctx, cfg); err != nil {
		t.Fatalf("seed fixture: %v", err)
	}

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

	for _, mode := range []string{"simple_protocol", "cache_statement"} {
		t.Run(mode, func(t *testing.T) {
			dsn := fmt.Sprintf("postgres://lyrebird:lyrebird@%s/postgres?sslmode=disable&default_query_exec_mode=%s", ln.Addr(), mode)
			conn, err := pgx.Connect(ctx, dsn)
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			defer conn.Close(context.Background())
			table := milvustest.Collection

			t.Run("select star returns every field in schema order", func(t *testing.T) {
				rows, err := conn.Query(ctx, fmt.Sprintf("SELECT * FROM %s LIMIT 1", table))
				if err != nil {
					t.Fatal(err)
				}
				defer rows.Close()
				fields := rows.FieldDescriptions()
				want := []string{"id", "name", "price", "qty", "active", "created_ms", "note", "emb"}
				if len(fields) != len(want) {
					t.Fatalf("columns = %d, want %d (%v)", len(fields), len(want), fields)
				}
				for i := range want {
					if fields[i].Name != want[i] {
						t.Errorf("column %d = %s, want %s", i, fields[i].Name, want[i])
					}
				}
				if !rows.Next() {
					t.Fatalf("no rows: %v", rows.Err())
				}
				vals, err := rows.Values()
				if err != nil {
					t.Fatal(err)
				}
				// first fixture row: id 1, alpha, 10.5 — numbers decode as
				// float64 (float8 columns), vectors ride the json encoding.
				if vals[0] != float64(1) || vals[1] != "alpha" || vals[2] != 10.5 {
					t.Errorf("first row = %v %v %v, want 1 alpha 10.5", vals[0], vals[1], vals[2])
				}
				if err := rows.Err(); err != nil {
					t.Fatalf("rows: %v", err)
				}
			})

			t.Run("where filter and sort", func(t *testing.T) {
				sql := fmt.Sprintf("SELECT name FROM %s WHERE price > 10 AND active = true ORDER BY price DESC LIMIT 2", table)
				rows, err := conn.Query(ctx, sql)
				if err != nil {
					t.Fatal(err)
				}
				defer rows.Close()
				var got []string
				for rows.Next() {
					var name string
					if err := rows.Scan(&name); err != nil {
						t.Fatal(err)
					}
					got = append(got, name)
				}
				if err := rows.Err(); err != nil {
					t.Fatal(err)
				}
				// matches: delta(30), charlie(5 no), alpha(10.5) → active>10: delta, alpha
				want := []string{"delta", "alpha"}
				if len(got) != len(want) {
					t.Fatalf("names = %v, want %v", got, want)
				}
				for i := range want {
					if got[i] != want[i] {
						t.Errorf("row %d = %s, want %s", i, got[i], want[i])
					}
				}
			})

			t.Run("in and between", func(t *testing.T) {
				rows, err := conn.Query(ctx, fmt.Sprintf("SELECT id FROM %s WHERE name IN ('bravo', 'echo') AND price BETWEEN 15 AND 25 ORDER BY id LIMIT 10", table))
				if err != nil {
					t.Fatal(err)
				}
				defer rows.Close()
				var ids []int64
				for rows.Next() {
					var id int64
					if err := rows.Scan(&id); err != nil {
						t.Fatal(err)
					}
					ids = append(ids, id)
				}
				if err := rows.Err(); err != nil {
					t.Fatal(err)
				}
				// bravo 20 and echo 15 land in [15,25]; charlie 5 does not
				if len(ids) != 2 || ids[0] != 2 || ids[1] != 5 {
					t.Errorf("ids = %v, want [2 5]", ids)
				}
			})

			t.Run("vector ordering returns nearest-first", func(t *testing.T) {
				sql := fmt.Sprintf("SELECT id FROM %s ORDER BY emb <-> '[0.1, 0.2, 0.3, 0.4]' LIMIT 2", table)
				ids := queryIDs(t, ctx, conn, sql)
				// the query vector is row 1's; L2 grows strictly with id
				if len(ids) != 2 || ids[0] != 1 || ids[1] != 2 {
					t.Errorf("ids = %v, want [1 2]", ids)
				}
			})

			t.Run("vector ordering with filter and offset", func(t *testing.T) {
				sql := fmt.Sprintf("SELECT id FROM %s WHERE price > 10 ORDER BY emb <-> '[0.1, 0.2, 0.3, 0.4]' LIMIT 2 OFFSET 1", table)
				ids := queryIDs(t, ctx, conn, sql)
				// price > 10 keeps ids 1,2,4,5 → nearest order [1 2 4 5], window [1,3)
				if len(ids) != 2 || ids[0] != 2 || ids[1] != 4 {
					t.Errorf("ids = %v, want [2 4]", ids)
				}
			})

			t.Run("vector errors render natively", func(t *testing.T) {
				// <+> has no Milvus metric: rejected in the translator
				_, err := conn.Exec(ctx, fmt.Sprintf("SELECT id FROM %s ORDER BY emb <+> '[0.1, 0.2, 0.3, 0.4]' LIMIT 2", table))
				var pgErr *pgconn.PgError
				if !errors.As(err, &pgErr) || pgErr.Code != "0A000" {
					t.Errorf("<+> operator: err = %v, want SQLSTATE 0A000", err)
				}
				// a non-numeric vector literal is a value error
				_, err = conn.Exec(ctx, fmt.Sprintf("SELECT id FROM %s ORDER BY emb <-> '[0.1, oops]' LIMIT 2", table))
				if !errors.As(err, &pgErr) || pgErr.Code != "22023" {
					t.Errorf("bad vector literal: err = %v, want SQLSTATE 22023", err)
				}
				// a cosine question against an L2 index must fail loudly at
				// execution, not silently answer with L2 distances (the
				// metric travels with the search; the server rejects it)
				_, err = conn.Exec(ctx, fmt.Sprintf("SELECT id FROM %s ORDER BY emb <=> '[0.1, 0.2, 0.3, 0.4]' LIMIT 2", table))
				if err == nil {
					t.Error("cosine operator on an L2-indexed field should fail")
				} else {
					t.Logf("metric mismatch surfaced as: %v", err)
				}
			})

			t.Run("three valued logic: equals null matches nothing", func(t *testing.T) {
				rows, err := conn.Query(ctx, fmt.Sprintf("SELECT id FROM %s WHERE price = NULL LIMIT 10", table))
				if err != nil {
					t.Fatal(err)
				}
				defer rows.Close()
				for rows.Next() {
					t.Errorf("unexpected row: SQL x = NULL never passes")
				}
				if err := rows.Err(); err != nil {
					t.Fatal(err)
				}
			})

			t.Run("is null reads the nullable column", func(t *testing.T) {
				rows, err := conn.Query(ctx, fmt.Sprintf("SELECT id FROM %s WHERE note IS NULL ORDER BY id LIMIT 10", table))
				if err != nil {
					t.Fatal(err)
				}
				defer rows.Close()
				var ids []int64
				for rows.Next() {
					var id int64
					if err := rows.Scan(&id); err != nil {
						t.Fatal(err)
					}
					ids = append(ids, id)
				}
				if err := rows.Err(); err != nil {
					t.Fatal(err)
				}
				if len(ids) != 2 || ids[0] != 2 || ids[1] != 5 {
					t.Errorf("ids = %v, want [2 5] (the null-note rows)", ids)
				}
			})

			t.Run("errors render native SQLSTATEs", func(t *testing.T) {
				_, err := conn.Exec(ctx, "SELECT * FROM "+table) // no LIMIT
				var pgErr *pgconn.PgError
				if !errors.As(err, &pgErr) || pgErr.Code != "0A000" {
					t.Errorf("no LIMIT: err = %v, want SQLSTATE 0A000", err)
				}

				_, err = conn.Exec(ctx, "SELECT * FROM no_such_table LIMIT 1")
				if !errors.As(err, &pgErr) || pgErr.Code != "42P01" || pgErr.Message != `relation "no_such_table" does not exist` {
					t.Errorf("missing table: err = %v, want native 42P01", err)
				}
			})
		})
	}

	t.Run("cosine fixture answers the <=> operator", func(t *testing.T) {
		dsn := fmt.Sprintf("postgres://lyrebird:lyrebird@%s/postgres?sslmode=disable", ln.Addr())
		conn, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close(context.Background())
		table := milvustest.VectorCollection

		ids := queryIDs(t, ctx, conn, fmt.Sprintf("SELECT id FROM %s ORDER BY emb <=> '[1, 0, 0, 0]' LIMIT 5", table))
		if len(ids) != 5 {
			t.Fatalf("ids = %v, want five rows", ids)
		}
		for i, want := range []int64{1, 2, 3, 4, 5} {
			if ids[i] != want {
				t.Errorf("row %d = %d, want %d (cosine order is strict on this fixture)", i, ids[i], want)
			}
		}

		ids = queryIDs(t, ctx, conn, fmt.Sprintf("SELECT id FROM %s WHERE tag = 'odd' ORDER BY emb <=> '[1, 0, 0, 0]' LIMIT 3", table))
		if len(ids) != 3 || ids[0] != 1 || ids[1] != 3 || ids[2] != 5 {
			t.Errorf("filtered ids = %v, want [1 3 5]", ids)
		}
	})

	t.Run("bm25 text fixture selects as plain columns", func(t *testing.T) {
		dsn := fmt.Sprintf("postgres://lyrebird:lyrebird@%s/postgres?sslmode=disable", ln.Addr())
		conn, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close(context.Background())

		rows, err := conn.Query(ctx, fmt.Sprintf("SELECT id, body FROM %s ORDER BY id LIMIT 5", milvustest.TextCollection))
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var count int
		for rows.Next() {
			var id int64
			var body string
			if err := rows.Scan(&id, &body); err != nil {
				t.Fatal(err)
			}
			count++
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if count != 5 {
			t.Errorf("rows = %d, want 5", count)
		}
	})

	t.Run("catalog introspection over the real cluster", func(t *testing.T) {
		dsn := fmt.Sprintf("postgres://lyrebird:lyrebird@%s/postgres?sslmode=disable", ln.Addr())
		conn, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close(context.Background())

		t.Run("getTables shows every fixture collection", func(t *testing.T) {
			rows, err := conn.Query(ctx,
				`SELECT c.relname, n.nspname FROM pg_catalog.pg_class c `+
					`LEFT JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace `+
					`WHERE c.relnamespace = n.oid AND c.relkind = 'r' AND n.nspname !~ '^pg_' `+
					`ORDER BY c.relname LIMIT 100`)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var names []string
			for rows.Next() {
				var relname, nspname string
				if err := rows.Scan(&relname, &nspname); err != nil {
					t.Fatal(err)
				}
				if nspname != "public" {
					t.Errorf("schema = %q, want public", nspname)
				}
				names = append(names, relname)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			rows.Close()
			joined := strings.Join(names, ",")
			for _, want := range []string{milvustest.Collection, milvustest.TextCollection, milvustest.VectorCollection} {
				if !strings.Contains(joined, want) {
					t.Errorf("tables missing %q: %v", want, names)
				}
			}
		})

		t.Run("getColumns orders the fixture fields", func(t *testing.T) {
			rows, err := conn.Query(ctx,
				`SELECT a.attname, a.attnum FROM pg_catalog.pg_attribute a `+
					`JOIN pg_catalog.pg_class c ON (a.attrelid = c.oid) `+
					`WHERE c.relname = $1 AND a.attnum > 0 ORDER BY a.attnum LIMIT 20`, milvustest.Collection)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var fields []string
			for rows.Next() {
				var name string
				var attnum int32
				if err := rows.Scan(&name, &attnum); err != nil {
					t.Fatal(err)
				}
				if attnum != int32(len(fields)+1) {
					t.Errorf("attnum %d out of order at position %d", attnum, len(fields)+1)
				}
				fields = append(fields, name)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			rows.Close()
			want := []string{"id", "name", "price", "qty", "active", "created_ms", "note", "emb"}
			if strings.Join(fields, ",") != strings.Join(want, ",") {
				t.Errorf("fields = %v, want %v", fields, want)
			}
		})

		t.Run("session surface keeps psql alive", func(t *testing.T) {
			for _, stmt := range []string{
				"SET extra_float_digits = 3",
				"BEGIN", "COMMIT",
				"SELECT 1",
				"SELECT version()",
				"SELECT current_database()",
				"SELECT pg_catalog.set_config('search_path', '', false)",
			} {
				tag, err := conn.Exec(ctx, stmt)
				if err != nil {
					t.Errorf("%q: %v", stmt, err)
					continue
				}
				_ = tag
			}
			var setting string
			if err := conn.QueryRow(ctx, "SHOW search_path").Scan(&setting); err != nil {
				t.Errorf("SHOW search_path: %v", err)
			}
		})
	})
}

// queryIDs runs one single-int-column statement and collects the rows.
func queryIDs(t *testing.T, ctx context.Context, conn *pgx.Conn, sql string) []int64 {
	t.Helper()
	rows, err := conn.Query(ctx, sql)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return ids
}
