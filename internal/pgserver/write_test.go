package pgserver

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/huanghaoyuanhhy/lyrebird/internal/store"
)

// writeExec runs one write statement and returns the command tag; INSERT and
// UPDATE answer with no rows, only the tag.
func writeExec(t *testing.T, addr net.Addr, mode, sql string) pgconn.CommandTag {
	t.Helper()
	conn := connect(t, addr, mode)
	t.Cleanup(func() { conn.Close(context.Background()) })
	tag, err := conn.Exec(context.Background(), sql)
	if err != nil {
		t.Fatalf("exec %q (%s): %v", sql, mode, err)
	}
	return tag
}

// writeExpectError runs one write statement expected to fail, returning the
// PG error with its SQLSTATE.
func writeExpectError(t *testing.T, addr net.Addr, mode, sql string) *pgconn.PgError {
	t.Helper()
	conn := connect(t, addr, mode)
	t.Cleanup(func() { conn.Close(context.Background()) })
	_, err := conn.Exec(context.Background(), sql)
	if err == nil {
		t.Fatalf("exec %q (%s): expected failure", sql, mode)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("exec %q (%s): expected a PG error, got %v", sql, mode, err)
	}
	return pgErr
}

func TestInsertRoundtrip(t *testing.T) {
	for _, mode := range []string{"cache_statement", "simple_protocol"} {
		t.Run(mode, func(t *testing.T) {
			exec := testExecutor()
			addr := startWireServer(t, exec)

			tag := writeExec(t, addr, mode, "INSERT INTO items (id, name, price, note) VALUES (7, 'hi', 1.5, NULL)")
			if tag.String() != "INSERT 0 1" {
				t.Fatalf("tag = %q", tag.String())
			}
			if len(exec.writeCalls) != 1 || exec.writeCalls[0].Upsert {
				t.Fatalf("calls = %#v", exec.writeCalls)
			}
			row := exec.writeCalls[0].Rows[0]
			if row["id"] != json.Number("7") { // numbers keep their written form
				t.Fatalf("id = %#v", row["id"])
			}
			if v, ok := row["note"]; !ok || v != nil {
				t.Fatalf("note = %#v, want nil", v)
			}

			tag = writeExec(t, addr, mode, "INSERT INTO items (id, name) VALUES (8, 'yo'), (9, 'no')")
			if tag.String() != "INSERT 0 2" {
				t.Fatalf("multi-row tag = %q", tag.String())
			}
			if len(exec.writeCalls) != 2 || len(exec.writeCalls[1].Rows) != 2 {
				t.Fatalf("calls = %#v", exec.writeCalls)
			}
		})
	}
}

func TestUpdateReadModifyWrite(t *testing.T) {
	for _, mode := range []string{"cache_statement", "simple_protocol"} {
		t.Run(mode, func(t *testing.T) {
			exec := testExecutor()
			addr := startWireServer(t, exec)

			tag := writeExec(t, addr, mode, "UPDATE items SET price = 99, note = 'z' WHERE name <> 'none'")
			if tag.String() != "UPDATE 2" {
				t.Fatalf("tag = %q", tag.String())
			}
			if len(exec.writeCalls) != 1 || !exec.writeCalls[0].Upsert {
				t.Fatalf("calls = %#v", exec.writeCalls)
			}
			// each fetched row comes back whole, with the SET fields applied
			rows := exec.writeCalls[0].Rows
			if len(rows) != 2 {
				t.Fatalf("rows = %d", len(rows))
			}
			first := rows[0]
			if first["price"] != json.Number("99") || first["note"] != "z" {
				t.Fatalf("set fields = %#v %#v", first["price"], first["note"])
			}
			if first["name"] != "alpha" || first["id"] != int64(1) {
				t.Fatalf("kept fields = %#v %#v", first["name"], first["id"])
			}
			// the second row keeps its own values (note was null, stays null)
			if rows[1]["name"] != "bravo" {
				t.Fatalf("second row = %#v", rows[1])
			}
		})
	}
}

func TestWriteErrorsNative(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		code string
	}{
		{"update without where", "UPDATE items SET price = 1", "0A000"},
		{"insert syntax", "INSERT INTO items (id) VALUES", "42601"},
		{"set expression", "UPDATE items SET price = price + 1 WHERE id = 1", "0A000"},
		{"insert select", "INSERT INTO items SELECT * FROM items", "0A000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			addr := startWireServer(t, testExecutor())
			pgErr := writeExpectError(t, addr, "simple_protocol", tc.sql)
			if pgErr.Code != tc.code {
				t.Fatalf("code = %s, want %s (message: %s)", pgErr.Code, tc.code, pgErr.Message)
			}
		})
	}
}

func TestInsertStrictSchemaErrorMapped(t *testing.T) {
	exec := testExecutor()
	exec.writeErr = &store.WriteError{
		Class:  store.ClassUnknownField,
		Field:  "id",
		Reason: "field [id] is not in the schema",
	}
	addr := startWireServer(t, exec)
	pgErr := writeExpectError(t, addr, "simple_protocol", "INSERT INTO items (id) VALUES (1)")
	if pgErr.Code != "42703" {
		t.Fatalf("code = %s, want 42703", pgErr.Code)
	}

	exec = testExecutor()
	exec.writeErr = &store.WriteError{Class: store.ClassType, Reason: "type mismatch"}
	addr = startWireServer(t, exec)
	pgErr = writeExpectError(t, addr, "simple_protocol", "INSERT INTO items (id) VALUES (1)")
	if pgErr.Code != "42804" {
		t.Fatalf("code = %s, want 42804", pgErr.Code)
	}
}
