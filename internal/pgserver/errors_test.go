package pgserver

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestCatalogErrorsNative asserts catalog-layer failures reach the client
// with their own SQLSTATE (parse-time syntax, undefined column, function
// arity, undefined setting) instead of collapsing into XX000.
func TestCatalogErrorsNative(t *testing.T) {
	cases := []struct {
		sql  string
		code string
	}{
		{"SELECT FROM pg_class", "42601"},                      // syntax, catalog parse
		{"SELECT nosuchcol FROM pg_class", "42703"},            // undefined column, catalog exec
		{"SELECT current_setting()", "42601"},                  // function arity, catalog exec
		{"SELECT current_setting('no_such_setting')", "42704"}, // undefined object, catalog exec
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
