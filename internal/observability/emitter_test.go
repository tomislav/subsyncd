package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestEmitterWritesRequiredFieldsAndFiltersDebug(t *testing.T) {
	var output bytes.Buffer
	events, err := New(&output, Options{Level: "info", Version: "sha-test"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithAttrs(context.Background(), slog.String("job_id", "job-1"), slog.Int64("media_id", 42))
	events.For("worker").Log(ctx, slog.LevelInfo, "job.started", "subtitle job started", slog.Int("attempt", 2))
	events.For("worker").Log(ctx, slog.LevelDebug, "candidate.evaluated", "candidate evaluated")

	records := decodeJSONLines(t, output.String())
	if len(records) != 1 {
		t.Fatalf("records = %d, want 1: %s", len(records), output.String())
	}
	record := records[0]
	wants := map[string]any{
		"level": "info", "msg": "subtitle job started", "event": "job.started",
		"service": "subsyncd", "version": "sha-test", "component": "worker",
		"job_id": "job-1", "media_id": float64(42), "attempt": float64(2),
	}
	for key, want := range wants {
		if got := record[key]; got != want {
			t.Errorf("%s = %#v, want %#v", key, got, want)
		}
	}
	if _, ok := record["time"].(string); !ok {
		t.Errorf("time = %#v, want JSON string", record["time"])
	}
	if strings.Contains(output.String(), "candidate.evaluated") {
		t.Fatal("debug event was emitted at info level")
	}
}

func TestEmitterProtectsCommonFieldsAndUsesLastDynamicValue(t *testing.T) {
	var output bytes.Buffer
	events, err := New(&output, Options{Level: "info", Version: "sha-test"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithAttrs(context.Background(), slog.String("job_id", "first"), slog.String("event", "context.override"))
	events.For("worker").Log(ctx, slog.LevelInfo, "job.started", "started",
		slog.String("job_id", "last"),
		slog.String("service", "override"),
		slog.String("version", "override"),
		slog.String("component", "override"),
		slog.String("event", "override"),
	)
	record := decodeJSONLines(t, output.String())[0]
	for key, want := range map[string]any{
		"service": "subsyncd", "version": "sha-test", "component": "worker", "event": "job.started", "job_id": "last",
	} {
		if got := record[key]; got != want {
			t.Errorf("%s = %#v, want %#v", key, got, want)
		}
	}
}

func TestEmitterCopiesContextAttributes(t *testing.T) {
	var output bytes.Buffer
	events, err := New(&output, Options{Level: "debug", Version: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	attributes := []slog.Attr{slog.String("job_id", "original")}
	ctx := WithAttrs(context.Background(), attributes...)
	attributes[0] = slog.String("job_id", "mutated")
	events.For("worker").Log(ctx, slog.LevelInfo, "job.started", "started")
	record := decodeJSONLines(t, output.String())[0]
	if got := record["job_id"]; got != "original" {
		t.Fatalf("job_id = %#v, want original", got)
	}
}

func TestEmitterRequiresValidLevelComponentAndEvent(t *testing.T) {
	if _, err := New(&bytes.Buffer{}, Options{Level: "trace"}); err == nil {
		t.Fatal("New() error = nil, want invalid-level error")
	}
	var output bytes.Buffer
	events, err := New(&output, Options{Level: "info"})
	if err != nil {
		t.Fatal(err)
	}
	events.Log(context.Background(), slog.LevelInfo, "job.started", "missing component")
	events.For("worker").Log(context.Background(), slog.LevelInfo, "", "missing event")
	if output.Len() != 0 {
		t.Fatalf("invalid event emitted output: %s", output.String())
	}
	events.For("worker").Log(context.Background(), slog.LevelInfo, "job.started", "started")
	if got := decodeJSONLines(t, output.String())[0]["version"]; got != "dev" {
		t.Fatalf("default version = %#v, want dev", got)
	}
}

func TestEmitterKeepsMultilineMessageOnOnePhysicalLine(t *testing.T) {
	var output bytes.Buffer
	events, err := New(&output, Options{Level: "info"})
	if err != nil {
		t.Fatal(err)
	}
	events.For("app").Log(context.Background(), slog.LevelInfo, "service.ready", "ready\nsecond\r\nthird")
	if got := len(strings.Split(strings.TrimSuffix(output.String(), "\n"), "\n")); got != 1 {
		t.Fatalf("physical lines = %d, want 1: %q", got, output.String())
	}
	if got := decodeJSONLines(t, output.String())[0]["msg"]; got != "ready second third" {
		t.Fatalf("sanitized msg = %#v", got)
	}
}

func decodeJSONLines(t *testing.T, output string) []map[string]any {
	t.Helper()
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		return nil
	}
	lines := strings.Split(trimmed, "\n")
	records := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode JSON log %q: %v", line, err)
		}
		records = append(records, record)
	}
	return records
}
