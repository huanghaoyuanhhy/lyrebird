package store

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"lyrebird/internal/translate"
)

func newTestExecutor(buf *bytes.Buffer) *LogExecutor {
	cfg := zap.NewProductionEncoderConfig()
	core := zapcore.NewCore(zapcore.NewJSONEncoder(cfg), zapcore.AddSync(buf), zap.DebugLevel)
	return &LogExecutor{Logger: zap.New(core)}
}

func TestLogExecutorReturnsEmptySuccess(t *testing.T) {
	var buf bytes.Buffer
	e := newTestExecutor(&buf)

	plan := &translate.Plan{
		Expr: translate.Compare{
			Op:    translate.Eq,
			Field: "status",
			Value: translate.StringValue("ok"),
		},
		Limit:  10,
		Source: translate.SourceFilter{FetchSource: true},
	}
	got, err := e.Search(context.Background(), "goods", plan)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if got.Total != 0 || len(got.Hits) != 0 {
		t.Fatalf("Search() = %+v, want empty success", got)
	}

	out := buf.String()
	for _, want := range []string{`"collection":"goods"`, `status ==`, `"limit":10`} {
		if !strings.Contains(out, want) {
			t.Errorf("log output missing %q:\n%s", want, out)
		}
	}
}

func TestLogExecutorNoMatchShortCircuit(t *testing.T) {
	var buf bytes.Buffer
	e := newTestExecutor(&buf)

	plan := &translate.Plan{NoMatch: true, Limit: 10}
	got, err := e.Search(context.Background(), "goods", plan)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if got.Total != 0 || len(got.Hits) != 0 {
		t.Fatalf("Search() = %+v, want empty success", got)
	}
	if !strings.Contains(buf.String(), `"no_match":true`) {
		t.Errorf("log output missing no_match=true:\n%s", buf.String())
	}
}

func TestLogExecutorRenderError(t *testing.T) {
	var buf bytes.Buffer
	e := newTestExecutor(&buf)

	// A field name Render rejects — Translate lets it through, Render is the
	// validation point, so the executor must surface the error.
	plan := &translate.Plan{
		Expr: translate.Compare{Op: translate.Eq, Field: "bad field", Value: translate.IntValue(1)},
	}
	if _, err := e.Search(context.Background(), "goods", plan); err == nil {
		t.Fatal("Search() expected render error for invalid field name")
	} else if !strings.Contains(err.Error(), "invalid field name") {
		t.Fatalf("Search() error = %v, want invalid-field-name error", err)
	}
}
