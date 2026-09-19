package pgserver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/huanghaoyuanhhy/lyrebird/internal/translate"
)

// Wire-level catalog tests: session statements, scalar SELECTs and catalog
// introspection over both protocol flavors, plus the startup-database
// routing the Milvus-database mapping rides on.

func TestSessionStatements(t *testing.T) {
	for _, mode := range []string{"cache_statement", "simple_protocol"} {
		t.Run(mode, func(t *testing.T) {
			addr := startWireServer(t, testExecutor())
			conn := connect(t, addr, mode)
			defer conn.Close(context.Background())

			// ack statements must report the tags clients switch on
			cases := []struct {
				sql string
				tag string
			}{
				{"SET extra_float_digits = 3", "SET"},
				{"SET search_path TO public", "SET"},
				{"BEGIN", "BEGIN"},
				{"COMMIT", "COMMIT"},
				{"ROLLBACK", "ROLLBACK"},
				{"DISCARD ALL", "DISCARD ALL"},
				{"RESET ALL", "RESET"},
			}
			for _, tc := range cases {
				tag, err := conn.Exec(context.Background(), tc.sql)
				if err != nil {
					t.Errorf("%q: %v", tc.sql, err)
					continue
				}
				if tag.String() != tc.tag {
					t.Errorf("%q: tag = %q, want %q", tc.sql, tag.String(), tc.tag)
				}
			}

			// SHOW answers from the settings table
			for _, tc := range []struct{ sql, want string }{
				{"SHOW search_path", "public"},
				{"SHOW standard_conforming_strings", "on"},
				{"show time zone", "UTC"},
			} {
				rows, err := conn.Query(context.Background(), tc.sql)
				if err != nil {
					t.Errorf("%q: %v", tc.sql, err)
					continue
				}
				if !rows.Next() {
					t.Errorf("%q: no rows", tc.sql)
					rows.Close()
					continue
				}
				var got string
				if err := rows.Scan(&got); err != nil {
					t.Errorf("%q scan: %v", tc.sql, err)
				}
				rows.Close()
				if got != tc.want {
					t.Errorf("%q = %q, want %q", tc.sql, got, tc.want)
				}
			}

			// unknown setting → 42704, the native answer
			_, err := conn.Exec(context.Background(), "SHOW no_such_setting")
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "42704" {
				t.Errorf("SHOW no_such_setting: err = %v, want 42704", err)
			}
		})
	}
}

func TestScalarSelects(t *testing.T) {
	for _, mode := range []string{"cache_statement", "simple_protocol"} {
		t.Run(mode, func(t *testing.T) {
			addr := startWireServer(t, testExecutor())
			conn := connect(t, addr, mode)
			defer conn.Close(context.Background())

			rows, err := conn.Query(context.Background(), "SELECT 1, version(), current_database()")
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			defer rows.Close()
			if !rows.Next() {
				t.Fatalf("no rows: %v", rows.Err())
			}
			var one float64
			var version, db string
			if err := rows.Scan(&one, &version, &db); err != nil {
				t.Fatalf("scan: %v", err)
			}
			if one != 1 || !strings.Contains(version, "lyrebird") || db != "default" {
				t.Errorf("row = (%v, %q, %q)", one, version, db)
			}
		})
	}
}

func TestCatalogQueriesOverWire(t *testing.T) {
	for _, mode := range []string{"cache_statement", "simple_protocol"} {
		t.Run(mode, func(t *testing.T) {
			addr := startWireServer(t, testExecutor())
			conn := connect(t, addr, mode)
			defer conn.Close(context.Background())

			// pgjdbc getCatalogs shape
			rows, err := conn.Query(context.Background(),
				"SELECT datname AS TABLE_CAT FROM pg_catalog.pg_database WHERE datallowconn = true ORDER BY TABLE_CAT")
			if err != nil {
				t.Fatalf("pg_database: %v", err)
			}
			defer rows.Close()
			var cats []string
			for rows.Next() {
				var cat string
				if err := rows.Scan(&cat); err != nil {
					t.Fatalf("scan: %v", err)
				}
				cats = append(cats, cat)
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("rows: %v", err)
			}
			rows.Close()
			if strings.Join(cats, ",") != "default" {
				t.Errorf("catalogs = %v, want [default]", cats)
			}
		})
	}
}

func TestCatalogBindParameters(t *testing.T) {
	// extended protocol only: pgjdbc's metadata queries always bind their
	// LIKE patterns as parameters
	addr := startWireServer(t, testExecutor())
	conn := connect(t, addr, "cache_statement")
	defer conn.Close(context.Background())

	rows, err := conn.Query(context.Background(),
		"SELECT relname FROM pg_catalog.pg_class WHERE relkind = 'r' AND relname LIKE $1", "%e%")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	rows.Close()
	if strings.Join(names, ",") != "items" {
		t.Errorf("names = %v, want [items]", names)
	}
}

func TestCatalogDatabaseRouting(t *testing.T) {
	// the startup database names the catalog slice: current_database() and
	// the table list follow it, and "postgres" (every PG client's URL
	// default) aliases onto the gateway's configured database
	exec := testExecutor()
	exec.alt = map[string]map[string]translate.Schema{
		"second": {"other": translate.MapSchema{"id": translate.TypeNumber}},
	}
	addr := startWireServer(t, exec)

	t.Run("postgres aliases to default", func(t *testing.T) {
		conn := connect(t, addr, "cache_statement") // DSN names postgres
		defer conn.Close(context.Background())
		var db string
		if err := conn.QueryRow(context.Background(), "SELECT current_database()").Scan(&db); err != nil {
			t.Fatal(err)
		}
		if db != "default" {
			t.Errorf("current_database() = %q, want default", db)
		}
	})

	t.Run("named database routes", func(t *testing.T) {
		dsn := fmt.Sprintf("postgres://l:l@%s/second?sslmode=disable", addr)
		conn, err := pgx.Connect(context.Background(), dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close(context.Background())
		var db string
		if err := conn.QueryRow(context.Background(), "SELECT current_database()").Scan(&db); err != nil {
			t.Fatal(err)
		}
		if db != "second" {
			t.Errorf("current_database() = %q, want second", db)
		}
		rows, err := conn.Query(context.Background(), "SELECT relname FROM pg_class ORDER BY relname")
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatal(err)
			}
			names = append(names, name)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		rows.Close()
		if strings.Join(names, ",") != "other" {
			t.Errorf("tables in second = %v, want [other]", names)
		}
	})
}

func TestCollectionReadsStillRouteToPipeline(t *testing.T) {
	// the catalog layer must not swallow collection reads: a table outside
	// the registry keeps the translate/pg pipeline's native errors
	addr := startWireServer(t, testExecutor())
	conn := connect(t, addr, "cache_statement")
	defer conn.Close(context.Background())

	rows, err := conn.Query(context.Background(), "SELECT name FROM items WHERE id = 1 LIMIT 1")
	if err != nil {
		t.Fatalf("collection query: %v", err)
	}
	if !rows.Next() {
		t.Fatalf("no rows: %v", rows.Err())
	}
	var name string
	if err := rows.Scan(&name); err != nil {
		t.Fatal(err)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	if name != "alpha" {
		t.Errorf("name = %q, want alpha", name)
	}

	_, err = conn.Exec(context.Background(), "SELECT * FROM ghost LIMIT 1")
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42P01" {
		t.Errorf("missing table: err = %v, want 42P01", err)
	}
}
