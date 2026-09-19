package pgserver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.uber.org/zap"

	"github.com/huanghaoyuanhhy/lyrebird/internal/milvustest"
	"github.com/huanghaoyuanhhy/lyrebird/internal/store"
)

// waitVisible scans one row, retrying until it appears: Milvus's
// growing-segment visibility window after an insert is sub-second in
// practice but not a guarantee, so the read-backs poll instead of assuming.
func waitVisible(t *testing.T, ctx context.Context, conn *pgx.Conn, sql string, dest ...any) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := conn.QueryRow(ctx, sql).Scan(dest...)
		if err == nil {
			return
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("query %q: %v", sql, err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("row never became visible for %q", sql)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestPgserverWriteE2E drives INSERT/UPDATE over the real wire: rows land
// in Milvus, reads see them, strict rejections come back with their
// SQLSTATE codes.
func TestPgserverWriteE2E(t *testing.T) {
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

	dsn := fmt.Sprintf("postgres://lyrebird:lyrebird@%s/postgres?sslmode=disable", ln.Addr())
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(context.Background())

	table := milvustest.WriteCollection

	t.Run("insert and read back", func(t *testing.T) {
		tag, err := conn.Exec(ctx,
			fmt.Sprintf("INSERT INTO %s (id, name, price, active, note, emb) VALUES (11, 'eleven', 1.5, true, 'n11', '[0.5, 0.5]'), (12, 'twelve', 2.5, false, NULL, '[0, 1]')", table))
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
		if tag.String() != "INSERT 0 2" {
			t.Fatalf("tag = %q", tag.String())
		}

		var name string
		var price float64
		var note *string
		waitVisible(t, ctx, conn,
			fmt.Sprintf("SELECT name, price, note FROM %s WHERE id = 11 LIMIT 1", table),
			&name, &price, &note)
		if name != "eleven" || price != 1.5 || note == nil || *note != "n11" {
			t.Fatalf("row = %q %v %v", name, price, note)
		}
	})

	t.Run("update rewrites matched rows", func(t *testing.T) {
		tag, err := conn.Exec(ctx,
			fmt.Sprintf("UPDATE %s SET price = 9.5, note = 'bumped' WHERE name <> 'nobody'", table))
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		if tag.String() != "UPDATE 2" {
			t.Fatalf("tag = %q, want UPDATE 2", tag.String())
		}

		var price float64
		waitVisible(t, ctx, conn,
			fmt.Sprintf("SELECT price FROM %s WHERE id = 12 LIMIT 1", table), &price)
		if price != 9.5 {
			t.Fatalf("price = %v, want 9.5", price)
		}
	})

	t.Run("insert vector literal and query by distance", func(t *testing.T) {
		tag, err := conn.Exec(ctx,
			fmt.Sprintf("INSERT INTO %s (id, name, price, active, emb) VALUES (13, 'thirteen', 3, true, '[1, 0]')", table))
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
		if tag.String() != "INSERT 0 1" {
			t.Fatalf("tag = %q", tag.String())
		}

		// the ANN path needs the new row visible to the vector index; poll
		// the same way the scalar read-backs do
		deadline := time.Now().Add(10 * time.Second)
		var name string
		for {
			// <-> (L2) matches the fixture's index metric
			rows, err := conn.Query(ctx,
				fmt.Sprintf("SELECT name FROM %s ORDER BY emb <-> '[1, 0]' LIMIT 1", table))
			if err != nil {
				t.Fatalf("distance query: %v", err)
			}
			if !rows.Next() {
				rows.Close()
				if time.Now().After(deadline) {
					t.Fatal("nearest row never became visible to the ANN search")
				}
				time.Sleep(200 * time.Millisecond)
				continue
			}
			if err := rows.Scan(&name); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			rows.Close()
			break
		}
		if name != "thirteen" {
			t.Fatalf("nearest = %q, want thirteen", name)
		}
	})

	t.Run("strict rejections with SQLSTATE", func(t *testing.T) {
		cases := []struct {
			name string
			sql  string
			code string
		}{
			{"unknown column", fmt.Sprintf("INSERT INTO %s (id, junk) VALUES (1, 'x')", table), "42703"},
			{"type mismatch", fmt.Sprintf("INSERT INTO %s (id, name, price, active, emb) VALUES (1, 'x', 'cheap', true, '[1, 1]')", table), "42804"},
			{"dim mismatch", fmt.Sprintf("INSERT INTO %s (id, name, price, active, emb) VALUES (1, 'x', 1, true, '[1, 2, 3]')", table), "22023"},
			{"length overflow", fmt.Sprintf("INSERT INTO %s (id, name, price, active, emb) VALUES (1, 'a-name-way-too-long-for-the-column', 1, true, '[1, 1]')", table), "22001"},
			{"update without where", fmt.Sprintf("UPDATE %s SET price = 1", table), "0A000"},
			{"update from", fmt.Sprintf("UPDATE %s SET price = 1 FROM other WHERE id = 1", table), "0A000"},
			{"on conflict", fmt.Sprintf("INSERT INTO %s (id) VALUES (1) ON CONFLICT DO NOTHING", table), "0A000"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				_, err := conn.Exec(ctx, tc.sql)
				var pgErr *pgconn.PgError
				if !errors.As(err, &pgErr) {
					t.Fatalf("err = %v, want a PG error", err)
				}
				if pgErr.Code != tc.code {
					t.Fatalf("code = %s, want %s (message: %s)", pgErr.Code, tc.code, pgErr.Message)
				}
			})
		}
	})
}
