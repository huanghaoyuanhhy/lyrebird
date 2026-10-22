package pgserver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/huanghaoyuanhhy/lyrebird/internal/milvustest"
	"github.com/huanghaoyuanhhy/lyrebird/internal/store"
)

// TestPgserverDDLE2E pins the DDL boundary of the SQL surface. Collections
// come from the control plane; the SQL lane moves rows (SELECT/INSERT/
// UPDATE), so every schema statement stops at the translator with SQLSTATE
// 0A000 (feature_not_supported). The cases matter as much for what they
// leave behind as for the error: DDL naming a live collection must neither
// drop it nor empty it, and a refused CREATE must not leave a half-created
// relation in the cluster.
//
// When LYREBIRD_TEST_PG_DSN names a reference Postgres, the matrix's
// statement shapes run there first (in a throwaway probe table), so the
// cases are known to be real DDL and not a typo the gateway would refuse
// for the wrong reason. Destructive cases (DROP/TRUNCATE of a fixture) are
// not replayed there — the reference keeps its mirror tables for the parity
// suite.
//
// When Phase 3b's CREATE TABLE → CreateCollection lands, these cases flip
// from refusal to a create/insert/select/drop round-trip.
func TestPgserverDDLE2E(t *testing.T) {
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

	probeReferenceDDLSyntax(t, ctx)

	addr := startWireServer(t, exec)
	table := milvustest.WriteCollection

	for _, mode := range []string{"simple_protocol", "cache_statement"} {
		t.Run(mode, func(t *testing.T) {
			conn := connect(t, addr, mode)
			defer conn.Close(context.Background())

			// a row the refused TRUNCATE must leave alone; ids differ per
			// mode so the two runs cannot see each other's marker
			marker := int64(9001)
			if mode == "cache_statement" {
				marker = 9002
			}
			if _, err := conn.Exec(ctx, fmt.Sprintf(
				"INSERT INTO %s (id, name, price, active, emb) VALUES (%d, 'ddl-survivor', 1.5, true, '[0.5, 0.5]')", table, marker)); err != nil {
				t.Fatalf("insert marker: %v", err)
			}
			var name string
			waitVisible(t, ctx, conn, fmt.Sprintf("SELECT name FROM %s WHERE id = %d LIMIT 1", table, marker), &name)

			for _, tc := range ddlCases(table, marker) {
				t.Run(tc.name, func(t *testing.T) {
					_, err := conn.Exec(ctx, tc.sql)
					var pgErr *pgconn.PgError
					if !errors.As(err, &pgErr) {
						t.Fatalf("%s: err = %v, want a PG error", tc.sql, err)
					}
					if pgErr.Code != "0A000" {
						t.Fatalf("%s: SQLSTATE = %s, want 0A000 (message: %s)", tc.sql, pgErr.Code, pgErr.Message)
					}
					if !strings.Contains(strings.ToUpper(pgErr.Message), tc.verb) {
						t.Errorf("%s: message = %q, want it to name [%s]", tc.sql, pgErr.Message, tc.verb)
					}
					tc.check(t, ctx, conn)
				})
			}
		})
	}
}

// ddlCase is one refused schema statement plus the probe that shows the
// cluster is unchanged. marker is the row the TRUNCATE case must spare.
type ddlCase struct {
	name  string
	sql   string
	verb  string // statement head the error message must name
	check func(t *testing.T, ctx context.Context, conn *pgx.Conn)
}

func ddlCases(table string, marker int64) []ddlCase {
	return []ddlCase{
		{
			name: "create table leaves no table",
			sql:  "CREATE TABLE lyrebird_e2e_ddl (id bigint PRIMARY KEY, name text)",
			verb: "CREATE",
			check: func(t *testing.T, ctx context.Context, conn *pgx.Conn) {
				_, err := conn.Exec(ctx, "SELECT id FROM lyrebird_e2e_ddl LIMIT 1")
				expectSQLSTATE(t, err, "42P01")
			},
		},
		{
			name: "create view leaves no relation",
			sql:  fmt.Sprintf("CREATE VIEW lyrebird_e2e_ddl_view AS SELECT id FROM %s", table),
			verb: "CREATE",
			check: func(t *testing.T, ctx context.Context, conn *pgx.Conn) {
				_, err := conn.Exec(ctx, "SELECT id FROM lyrebird_e2e_ddl_view LIMIT 1")
				expectSQLSTATE(t, err, "42P01")
			},
		},
		{
			name: "alter table adds no column",
			sql:  fmt.Sprintf("ALTER TABLE %s ADD COLUMN extra text", table),
			verb: "ALTER",
			check: func(t *testing.T, ctx context.Context, conn *pgx.Conn) {
				rows, err := conn.Query(ctx, fmt.Sprintf("SELECT * FROM %s LIMIT 1", table))
				if err != nil {
					t.Fatalf("select after refused ALTER: %v", err)
				}
				defer rows.Close()
				for _, fd := range rows.FieldDescriptions() {
					if fd.Name == "extra" {
						t.Error("refused ALTER added the column anyway")
					}
				}
			},
		},
		{
			name: "drop table keeps the live collection",
			sql:  fmt.Sprintf("DROP TABLE %s", table),
			verb: "DROP",
			check: func(t *testing.T, ctx context.Context, conn *pgx.Conn) {
				rows, err := conn.Query(ctx, fmt.Sprintf("SELECT id FROM %s LIMIT 1", table))
				if err != nil {
					t.Fatalf("refused DROP removed the table: %v", err)
				}
				rows.Close()
			},
		},
		{
			name: "truncate keeps the rows",
			sql:  fmt.Sprintf("TRUNCATE %s", table),
			verb: "TRUNCATE",
			check: func(t *testing.T, ctx context.Context, conn *pgx.Conn) {
				var name string
				if err := conn.QueryRow(ctx,
					fmt.Sprintf("SELECT name FROM %s WHERE id = %d LIMIT 1", table, marker)).Scan(&name); err != nil {
					t.Fatalf("refused TRUNCATE lost the marker row: %v", err)
				}
				if name != "ddl-survivor" {
					t.Errorf("marker name = %q, want ddl-survivor", name)
				}
			},
		},
		{
			name: "create index is refused",
			sql:  fmt.Sprintf("CREATE INDEX %s_name ON %s (name)", table, table),
			verb: "CREATE",
			check: func(t *testing.T, ctx context.Context, conn *pgx.Conn) {
				// indexes are not projected into the catalog, so there is
				// nothing to observe: the refusal is the whole contract
			},
		},
	}
}

// expectSQLSTATE asserts the error is a PG error with the given SQLSTATE.
func expectSQLSTATE(t *testing.T, err error, code string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("err = %v, want a PG error with SQLSTATE %s", err, code)
	}
	if pgErr.Code != code {
		t.Fatalf("SQLSTATE = %s, want %s (message: %s)", pgErr.Code, code, pgErr.Message)
	}
}

// probeReferenceDDLSyntax validates the matrix's statement shapes against
// the reference Postgres: create/alter/index/view run on a throwaway table
// that is dropped again. It also covers DROP TABLE there — on the probe,
// never on a fixture.
func probeReferenceDDLSyntax(t *testing.T, ctx context.Context) {
	t.Helper()
	dsn := os.Getenv(pgDSNEnv)
	if dsn == "" {
		t.Logf("%s unset: skipping the reference-syntax probe", pgDSNEnv)
		return
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect reference postgres: %v", err)
	}
	defer conn.Close(context.Background())

	for _, stmt := range []string{
		"DROP VIEW IF EXISTS lyrebird_e2e_ddl_view",
		"DROP TABLE IF EXISTS lyrebird_e2e_ddl_probe",
		"CREATE TABLE lyrebird_e2e_ddl_probe (id bigint PRIMARY KEY, name text)",
		"ALTER TABLE lyrebird_e2e_ddl_probe ADD COLUMN extra text",
		"CREATE INDEX lyrebird_e2e_ddl_probe_name ON lyrebird_e2e_ddl_probe (name)",
		"CREATE VIEW lyrebird_e2e_ddl_view AS SELECT id FROM lyrebird_e2e_ddl_probe",
		"DROP VIEW lyrebird_e2e_ddl_view",
		"DROP TABLE lyrebird_e2e_ddl_probe",
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("reference postgres: %s: %v", stmt, err)
		}
	}
}
