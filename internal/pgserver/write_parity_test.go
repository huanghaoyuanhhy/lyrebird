package pgserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"go.uber.org/zap"

	"github.com/huanghaoyuanhhy/lyrebird/internal/esserver"
	"github.com/huanghaoyuanhhy/lyrebird/internal/milvustest"
	"github.com/huanghaoyuanhhy/lyrebird/internal/store"
)

// TestWriteParityMatrix drives the same logical write scenarios through
// both lyrebird entries — the ES index/update/delete APIs and the PG
// INSERT/UPDATE/DELETE statements — against twin Milvus collections of
// one shape (milvustest.WriteShape), then compares what a client of each
// entry observes: the row count the statement reported, and the full
// scalar state (id, name, price, active, note) after every step, read
// back through each entry's own query path.
//
// The matrix stays inside the consistent subset. Known cross-entry
// divergences are asserted nowhere: an ES index put upserts by id while
// a PG INSERT of an existing pk appends (Milvus insert semantics), and
// ES _update answers 404 on a missing document while PG UPDATE answers
// UPDATE 0. Vector values are excluded from the comparison (fp32
// formatting differs across the two encodings; vector round-trips are
// covered by the write e2e suites).
func TestWriteParityMatrix(t *testing.T) {
	cfg, ok := milvustest.FromEnv()
	if !ok {
		t.Skipf("set %s and %s to run the parity suite", milvustest.URIEnv, milvustest.TokenEnv)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cli, err := milvustest.Connect(ctx, cfg)
	if err != nil {
		t.Fatalf("connect for fixture rebuild: %v", err)
	}
	defer cli.Close(ctx)

	exec, err := store.NewMilvusExecutor(ctx, store.MilvusConfig{URI: cfg.URI, Token: cfg.Token})
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	defer exec.Close(ctx)

	esSrv := httptest.NewServer(esserver.New(exec, zap.NewNop()))
	t.Cleanup(esSrv.Close)

	addr := startWireServer(t, exec)
	conn := connect(t, addr, "cache_statement")
	defer conn.Close(context.Background())

	const (
		esColl = "lyrebird_e2e_wp_es"
		pgColl = "lyrebird_e2e_wp_pg"
	)

	esDo := func(t *testing.T, method, path, body string) (int, map[string]any) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, method, esSrv.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var top map[string]any
		_ = json.Unmarshal(raw, &top)
		return resp.StatusCode, top
	}

	pgExec := func(t *testing.T, sql string) pgconn.CommandTag {
		t.Helper()
		tag, err := conn.Exec(ctx, sql)
		if err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
		return tag
	}

	reset := func(t *testing.T) {
		t.Helper()
		for _, name := range []string{esColl, pgColl} {
			if err := milvustest.WriteShape(ctx, cli, name); err != nil {
				t.Fatalf("rebuild fixture %s: %v", name, err)
			}
		}
	}

	// seedDoc writes one document through both entries: ES PUT with the URL
	// id, PG INSERT with an explicit column list. An empty note means NULL.
	seedDoc := func(t *testing.T, id int, name, price string, active bool, note, emb string) {
		t.Helper()
		noteJSON, noteSQL := "null", "NULL"
		if note != "" {
			noteJSON, noteSQL = strconv.Quote(note), "'"+note+"'"
		}
		status, body := esDo(t, "PUT", fmt.Sprintf("/%s/_doc/%d", esColl, id),
			fmt.Sprintf(`{"id": %d, "name": %q, "price": %s, "active": %t, "note": %s, "emb": %s}`,
				id, name, price, active, noteJSON, emb))
		if status != http.StatusCreated {
			t.Fatalf("es seed %d: status=%d body=%v", id, status, body)
		}
		tag := pgExec(t, fmt.Sprintf(
			"INSERT INTO %s (id, name, price, active, note, emb) VALUES (%d, '%s', %s, %t, %s, '%s')",
			pgColl, id, name, price, active, noteSQL, emb))
		if tag.String() != "INSERT 0 1" {
			t.Fatalf("pg seed %d: tag = %q", id, tag.String())
		}
	}

	// parityRow is one compared scalar row in canonical string form.
	type parityRow struct{ id, name, price, active, note string }

	esState := func(t *testing.T) []parityRow {
		t.Helper()
		status, body := esDo(t, "POST", "/"+esColl+"/_search", `{"size":1000,"sort":[{"id":"asc"}]}`)
		if status != http.StatusOK {
			t.Fatalf("es state read: status=%d body=%v", status, body)
		}
		items := body["hits"].(map[string]any)["hits"].([]any)
		out := make([]parityRow, 0, len(items))
		for _, item := range items {
			src := item.(map[string]any)["_source"].(map[string]any)
			out = append(out, parityRow{
				id:     canonWriteCell(src["id"]),
				name:   canonWriteCell(src["name"]),
				price:  canonWriteCell(src["price"]),
				active: canonWriteCell(src["active"]),
				note:   canonWriteCell(src["note"]),
			})
		}
		return out
	}

	pgState := func(t *testing.T) []parityRow {
		t.Helper()
		rows, err := conn.Query(ctx, fmt.Sprintf(
			"SELECT id, name, price, active, note FROM %s ORDER BY id LIMIT 1000", pgColl))
		if err != nil {
			t.Fatalf("pg state read: %v", err)
		}
		defer rows.Close()
		out := []parityRow{}
		for rows.Next() {
			var id int64
			var name string
			var price float64
			var active bool
			var note *string
			if err := rows.Scan(&id, &name, &price, &active, &note); err != nil {
				t.Fatalf("scan: %v", err)
			}
			row := parityRow{
				id:     strconv.FormatInt(id, 10),
				name:   name,
				price:  strconv.FormatFloat(price, 'g', -1, 64),
				active: strconv.FormatBool(active),
				note:   "null",
			}
			if note != nil {
				row.note = *note
			}
			out = append(out, row)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		return out
	}

	renderRows := func(rs []parityRow) string {
		parts := make([]string, len(rs))
		for i, r := range rs {
			parts[i] = fmt.Sprintf("{id:%s name:%s price:%s active:%s note:%s}", r.id, r.name, r.price, r.active, r.note)
		}
		return "[" + strings.Join(parts, " ") + "]"
	}

	// compareState polls until both twins hold the same scalar state —
	// inserts and delete tombstones ride a sub-second visibility window, so
	// the comparison waits instead of assuming. wantCount >= 0 additionally
	// requires exactly that many rows on both sides (guards against "both
	// empty because nothing is visible yet").
	compareState := func(t *testing.T, step string, wantCount int) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			want, got := esState(t), pgState(t)
			if reflect.DeepEqual(want, got) && (wantCount < 0 || len(want) == wantCount) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: state diverged\n  es: %s\n  pg: %s", step, renderRows(want), renderRows(got))
			}
			time.Sleep(200 * time.Millisecond)
		}
	}

	scenarios := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"insert whole row", func(t *testing.T) {
			status, body := esDo(t, "PUT", "/"+esColl+"/_doc/1",
				`{"id": 1, "name": "one", "price": 1.5, "active": true, "note": "n1", "emb": [1, 1]}`)
			if status != http.StatusCreated || body["result"] != "created" {
				t.Fatalf("es insert: status=%d body=%v", status, body)
			}
			if tag := pgExec(t, fmt.Sprintf(
				"INSERT INTO %s (id, name, price, active, note, emb) VALUES (1, 'one', 1.5, true, 'n1', '[1, 1]')", pgColl)); tag.String() != "INSERT 0 1" {
				t.Fatalf("pg insert: tag = %q", tag.String())
			}
			compareState(t, "insert whole row", 1)
		}},

		{"insert with null note", func(t *testing.T) {
			status, _ := esDo(t, "PUT", "/"+esColl+"/_doc/2",
				`{"id": 2, "name": "two", "price": 2, "active": false, "note": null, "emb": [0, 1]}`)
			if status != http.StatusCreated {
				t.Fatalf("es insert: status=%d", status)
			}
			if tag := pgExec(t, fmt.Sprintf(
				"INSERT INTO %s (id, name, price, active, note, emb) VALUES (2, 'two', 2, false, NULL, '[0, 1]')", pgColl)); tag.String() != "INSERT 0 1" {
				t.Fatalf("pg insert: tag = %q", tag.String())
			}
			compareState(t, "insert with null note", 1)
		}},

		{"insert three rows", func(t *testing.T) {
			for id := 1; id <= 3; id++ {
				seedDoc(t, id, fmt.Sprintf("r%d", id), fmt.Sprintf("%d", id), id%2 == 0, "", "[1, 1]")
			}
			compareState(t, "insert three rows", 3)
		}},

		{"update by id keeps other fields", func(t *testing.T) {
			seedDoc(t, 1, "one", "1.5", true, "n1", "[1, 1]")
			seedDoc(t, 2, "two", "2", false, "", "[0, 1]")
			compareState(t, "seed", 2)

			status, body := esDo(t, "POST", "/"+esColl+"/_update/1", `{"doc": {"price": 9}}`)
			if status != http.StatusOK || body["result"] != "updated" {
				t.Fatalf("es update: status=%d body=%v", status, body)
			}
			if tag := pgExec(t, fmt.Sprintf("UPDATE %s SET price = 9 WHERE id = 1", pgColl)); tag.String() != "UPDATE 1" {
				t.Fatalf("pg update: tag = %q", tag.String())
			}
			compareState(t, "update by id", 2)
		}},

		{"delete by id", func(t *testing.T) {
			seedDoc(t, 1, "one", "1.5", true, "", "[1, 1]")
			seedDoc(t, 2, "two", "2", true, "", "[1, 1]")
			seedDoc(t, 3, "three", "3", true, "", "[1, 1]")
			compareState(t, "seed", 3)

			status, body := esDo(t, "DELETE", "/"+esColl+"/_doc/2", "")
			if status != http.StatusOK || body["result"] != "deleted" {
				t.Fatalf("es delete: status=%d body=%v", status, body)
			}
			if tag := pgExec(t, fmt.Sprintf("DELETE FROM %s WHERE id = 2", pgColl)); tag.String() != "DELETE 1" {
				t.Fatalf("pg delete: tag = %q", tag.String())
			}
			compareState(t, "delete by id", 2)
		}},

		{"delete by range", func(t *testing.T) {
			seedDoc(t, 1, "one", "1", true, "", "[1, 1]")
			seedDoc(t, 2, "two", "2", false, "", "[1, 1]")
			seedDoc(t, 3, "three", "3", true, "", "[1, 1]")
			compareState(t, "seed", 3)

			status, body := esDo(t, "POST", "/"+esColl+"/_delete_by_query",
				`{"query":{"range":{"price":{"lt":2.5}}}}`)
			if status != http.StatusOK || body["deleted"] != float64(2) {
				t.Fatalf("es delete_by_query: status=%d body=%v", status, body)
			}
			if tag := pgExec(t, fmt.Sprintf("DELETE FROM %s WHERE price < 2.5", pgColl)); tag.String() != "DELETE 2" {
				t.Fatalf("pg delete: tag = %q", tag.String())
			}
			compareState(t, "delete by range", 1)
		}},

		{"delete by compound condition", func(t *testing.T) {
			seedDoc(t, 1, "one", "1", true, "", "[1, 1]")
			seedDoc(t, 2, "two", "2", false, "", "[1, 1]")
			seedDoc(t, 3, "three", "3", true, "", "[1, 1]")
			seedDoc(t, 4, "four", "2.5", true, "", "[1, 1]")
			compareState(t, "seed", 4)

			status, body := esDo(t, "POST", "/"+esColl+"/_delete_by_query",
				`{"query":{"bool":{"must":[{"term":{"active":true}},{"range":{"price":{"lt":3}}}]}}}`)
			if status != http.StatusOK || body["deleted"] != float64(2) {
				t.Fatalf("es delete_by_query: status=%d body=%v", status, body)
			}
			if tag := pgExec(t, fmt.Sprintf(
				"DELETE FROM %s WHERE active = true AND price < 3", pgColl)); tag.String() != "DELETE 2" {
				t.Fatalf("pg delete: tag = %q", tag.String())
			}
			compareState(t, "delete by compound condition", 2)
		}},

		{"delete nothing", func(t *testing.T) {
			seedDoc(t, 1, "one", "1", true, "", "[1, 1]")
			compareState(t, "seed", 1)

			// Neither side may remove the surviving row. The judged fact is
			// "nothing was deleted": the state assertion below is the strong
			// check, and the reported counts must merely agree with each
			// other — Milvus's delete count for a key that does not exist is
			// exact after a flush (probed: 0) but observed unstable in the
			// growing state (once reported 1 here), so neither absolute
			// value is pinned.
			status, body := esDo(t, "POST", "/"+esColl+"/_delete_by_query",
				`{"query":{"term":{"id":999}}}`)
			if status != http.StatusOK {
				t.Fatalf("es delete_by_query: status=%d body=%v", status, body)
			}
			esDeleted, _ := body["deleted"].(float64)
			tag := pgExec(t, fmt.Sprintf("DELETE FROM %s WHERE id = 999", pgColl))
			pgDeleted, err := strconv.ParseInt(strings.Fields(tag.String())[1], 10, 64)
			if err != nil {
				t.Fatalf("pg delete tag = %q: %v", tag.String(), err)
			}
			if int64(esDeleted) != pgDeleted {
				t.Fatalf("delete-nothing counts disagree: es deleted=%v, pg %q", body["deleted"], tag.String())
			}
			compareState(t, "delete nothing", 1)
		}},

		{"strict rejection leaves state unchanged", func(t *testing.T) {
			seedDoc(t, 1, "one", "1", true, "", "[1, 1]")
			compareState(t, "seed", 1)

			status, body := esDo(t, "PUT", "/"+esColl+"/_doc/9",
				`{"id": 9, "name": "x", "price": 1, "active": true, "emb": [1, 1], "junk": 1}`)
			if status != http.StatusBadRequest || body["error"].(map[string]any)["type"] != "strict_dynamic_mapping_exception" {
				t.Fatalf("es rejection: status=%d body=%v", status, body)
			}
			pgErr := writeExpectError(t, addr, "cache_statement",
				fmt.Sprintf("INSERT INTO %s (id, junk) VALUES (9, 'x')", pgColl))
			if pgErr.Code != "42703" {
				t.Fatalf("pg rejection: code = %s (message: %s)", pgErr.Code, pgErr.Message)
			}
			compareState(t, "after rejections", 1)
		}},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			reset(t)
			sc.run(t)
		})
	}
}

// canonWriteCell renders one _source cell in the canonical form the matrix
// compares: numbers by value across the encodings (PG scans float64 /
// int64, ES hands back json.Number), null as the literal "null".
func canonWriteCell(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		return strconv.FormatBool(x)
	case string:
		return x
	case json.Number:
		return canonWriteNumber(x.String())
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	default:
		return fmt.Sprintf("%v", x)
	}
}

func canonWriteNumber(s string) string {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return s
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}
