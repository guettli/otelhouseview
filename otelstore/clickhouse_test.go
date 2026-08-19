package otelstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

// TestClickHouseStoreReadsContribSchema runs against a real ClickHouse cluster
// when CLICKHOUSE_TEST_DSN is set. The test:
//
//   - creates the contrib exporter's otel_traces / otel_logs tables (subset of
//     columns the daemon actually reads),
//   - inserts a root span + child span + log row,
//   - exercises GetTrace and ListTraces against ClickHouseStore.
//
// The test is skipped in CI by default and opt-in by an operator standing up
// a local ClickHouse with `docker run -p 9000:9000 clickhouse/clickhouse-server`.
func TestClickHouseStoreReadsContribSchema(t *testing.T) {
	dsn := os.Getenv("CLICKHOUSE_TEST_DSN")
	if dsn == "" {
		t.Skip("CLICKHOUSE_TEST_DSN not set; skipping ClickHouse integration test")
	}
	ctx := context.Background()
	store, err := OpenClickHouse(ctx, dsn)
	if err != nil {
		t.Fatalf("OpenClickHouse: %v", err)
	}
	defer func() { _ = store.Close() }()

	// Fresh tables per run keep the assertions deterministic. Using IF EXISTS
	// + temporary suffix would be safer for a shared cluster; here we assume
	// the caller's DSN points at a throwaway database.
	tracesDDL := fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS %s (
    Timestamp DateTime64(9),
    TraceId String,
    SpanId String,
    ParentSpanId String,
    SpanName LowCardinality(String),
    SpanKind LowCardinality(String),
    ServiceName LowCardinality(String),
    ResourceAttributes Map(LowCardinality(String), String),
    SpanAttributes Map(LowCardinality(String), String),
    Duration Int64,
    StatusCode LowCardinality(String),
    StatusMessage String
) ENGINE = MergeTree() ORDER BY (TraceId, Timestamp)`, store.tracesTable)
	logsDDL := fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS %s (
    Timestamp DateTime64(9),
    TraceId String,
    SpanId String,
    SeverityText LowCardinality(String),
    SeverityNumber UInt8,
    ServiceName LowCardinality(String),
    Body String,
    ResourceAttributes Map(LowCardinality(String), String),
    LogAttributes Map(LowCardinality(String), String)
) ENGINE = MergeTree() ORDER BY (TraceId, Timestamp)`, store.logsTable)
	for _, stmt := range []string{
		fmt.Sprintf("TRUNCATE TABLE IF EXISTS %s", store.tracesTable),
		fmt.Sprintf("TRUNCATE TABLE IF EXISTS %s", store.logsTable),
		tracesDDL,
		logsDDL,
	} {
		if _, err := store.db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("DDL %q: %v", stmt, err)
		}
	}

	t0 := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	traceID := "00112233445566778899aabbccddeeff"

	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}
	mustExec(fmt.Sprintf(`
INSERT INTO %s
    (Timestamp, TraceId, SpanId, ParentSpanId, SpanName, SpanKind, ServiceName,
     ResourceAttributes, SpanAttributes, Duration, StatusCode, StatusMessage)
VALUES (?, ?, ?, '', ?, 'Server', ?, map('service.name','dagger'),
        map('repo','guettli/agentloop'), ?, 'Ok', '')`, store.tracesTable),
		t0, traceID, "0011223344556677", "build", "dagger",
		int64(2*time.Second),
	)
	mustExec(fmt.Sprintf(`
INSERT INTO %s
    (Timestamp, TraceId, SpanId, ParentSpanId, SpanName, SpanKind, ServiceName,
     ResourceAttributes, SpanAttributes, Duration, StatusCode, StatusMessage)
VALUES (?, ?, ?, ?, ?, 'Internal', ?, map('service.name','dagger'),
        map(), ?, 'Ok', '')`, store.tracesTable),
		t0.Add(100*time.Millisecond), traceID, "8899aabbccddeeff", "0011223344556677", "go test", "dagger",
		int64(1800*time.Millisecond),
	)
	mustExec(fmt.Sprintf(`
INSERT INTO %s
    (Timestamp, TraceId, SpanId, SeverityText, SeverityNumber, ServiceName, Body,
     ResourceAttributes, LogAttributes)
VALUES (?, ?, ?, 'INFO', 9, 'dagger', 'PASS: TestSomething',
        map('service.name','dagger'), map())`, store.logsTable),
		t0.Add(500*time.Millisecond), traceID, "8899aabbccddeeff",
	)

	got, err := store.GetTrace(ctx, traceID)
	if err != nil {
		t.Fatalf("GetTrace: %v", err)
	}
	if len(got.Spans) != 2 {
		t.Fatalf("want 2 spans, got %d (%+v)", len(got.Spans), got.Spans)
	}
	root, ok := got.Root()
	if !ok || root.SpanID != "0011223344556677" {
		t.Errorf("Root() = (%+v, %v); want root span", root, ok)
	}
	if root.Kind != 2 {
		t.Errorf("root span kind = %d, want 2 (Server)", root.Kind)
	}
	if root.StatusCode != 1 {
		t.Errorf("root span status = %d, want 1 (Ok)", root.StatusCode)
	}
	if root.ServiceName != "dagger" {
		t.Errorf("root service name = %q, want dagger", root.ServiceName)
	}
	if root.Duration() != 2*time.Second {
		t.Errorf("root duration = %s, want 2s", root.Duration())
	}
	if root.Attributes["repo"] != "guettli/agentloop" {
		t.Errorf("root attributes = %+v, want repo=guettli/agentloop", root.Attributes)
	}
	if len(got.Logs) != 1 || got.Logs[0].Body != "PASS: TestSomething" {
		t.Errorf("logs not read back: %+v", got.Logs)
	}

	// A second trace in the shape a CI workflow produces: every span names a
	// parent that was minted outside the store (an injected TRACEPARENT), so
	// no span here has an empty ParentSpanId.
	ciTraceID := "aabbccddeeff00112233445566778899"
	mustExec(fmt.Sprintf(`
INSERT INTO %s
    (Timestamp, TraceId, SpanId, ParentSpanId, SpanName, SpanKind, ServiceName,
     ResourceAttributes, SpanAttributes, Duration, StatusCode, StatusMessage)
VALUES (?, ?, ?, ?, ?, 'Internal', ?,
        map('service.name','dagger-cli','ci.commit','deadbeef','ci.repo','guettli/agentloop'),
        map(), ?, 'Ok', '')`, store.tracesTable),
		t0.Add(time.Minute), ciTraceID, "1111111111111111", "9999999999999999",
		"check --source=.", "dagger-cli", int64(30*time.Second),
	)
	// ...and a span from a second producer in the same trace, whose own
	// resource knows nothing about CI. The trace must still match a ci.commit
	// filter: membership is per trace, not per span.
	mustExec(fmt.Sprintf(`
INSERT INTO %s
    (Timestamp, TraceId, SpanId, ParentSpanId, SpanName, SpanKind, ServiceName,
     ResourceAttributes, SpanAttributes, Duration, StatusCode, StatusMessage)
VALUES (?, ?, ?, ?, ?, 'Internal', ?, map('service.name','dagger-engine'),
        map(), ?, 'Ok', '')`, store.tracesTable),
		t0.Add(70*time.Second), ciTraceID, "2222222222222222", "1111111111111111",
		"exec", "dagger-engine", int64(50*time.Second),
	)

	summaries, err := store.ListTraces(ctx, 10)
	if err != nil {
		t.Fatalf("ListTraces: %v", err)
	}
	if len(summaries) != 2 {
		t.Fatalf("ListTraces = %+v, want both traces", summaries)
	}
	// Newest first, and the externally-parented trace is listed at all —
	// selecting on ParentSpanId = '' used to drop it.
	if summaries[0].TraceID != ciTraceID || summaries[1].TraceID != traceID {
		t.Fatalf("ListTraces order = %s,%s; want %s,%s",
			summaries[0].TraceID, summaries[1].TraceID, ciTraceID, traceID)
	}
	if summaries[1].Name != "build" {
		t.Errorf("summary name = %q, want build", summaries[1].Name)
	}
	// The earliest span stands in for the missing root...
	if summaries[0].Name != "check --source=." || summaries[0].ServiceName != "dagger-cli" {
		t.Errorf("effective root = %q/%q, want check --source=./dagger-cli",
			summaries[0].Name, summaries[0].ServiceName)
	}
	// ...while the duration spans the whole trace: the CLI span starts at
	// t0+60s and the engine span ends at t0+120s.
	if d := summaries[0].EndTime.Sub(summaries[0].StartTime); d != 60*time.Second {
		t.Errorf("CI trace duration = %s, want 60s", d)
	}
	if summaries[0].ResourceAttributes["ci.commit"] != "deadbeef" {
		t.Errorf("summary resource attributes = %+v, want ci.commit=deadbeef",
			summaries[0].ResourceAttributes)
	}

	filtered, err := store.ListTracesFiltered(ctx, ListOptions{ResourceAttrKeys: []string{"ci.commit"}})
	if err != nil {
		t.Fatalf("ListTracesFiltered: %v", err)
	}
	if len(filtered) != 1 || filtered[0].TraceID != ciTraceID {
		t.Fatalf("filtered listing = %+v, want only %s", filtered, ciTraceID)
	}

	since, err := store.ListTracesFiltered(ctx, ListOptions{Since: t0.Add(30 * time.Second)})
	if err != nil {
		t.Fatalf("ListTracesFiltered(Since): %v", err)
	}
	if len(since) != 1 || since[0].TraceID != ciTraceID {
		t.Fatalf("Since listing = %+v, want only %s", since, ciTraceID)
	}

	if _, err := store.GetTrace(ctx, "missingtrace"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing trace error = %v, want ErrNotFound", err)
	}
}

// TestDecodeSpanKindStatusCode pins the string-to-enum mapping the
// ClickHouseStore relies on. It runs without any database.
func TestDecodeSpanKindStatusCode(t *testing.T) {
	kindCases := map[string]int32{
		"Internal":               1,
		"Server":                 2,
		"Client":                 3,
		"Producer":               4,
		"Consumer":               5,
		"Unspecified":            0,
		"SPAN_KIND_SERVER":       2,
		"SPAN_KIND_CLIENT":       3,
		"":                       0,
		"something-unrecognised": 0,
	}
	for in, want := range kindCases {
		if got := decodeSpanKind(in); got != want {
			t.Errorf("decodeSpanKind(%q) = %d, want %d", in, got, want)
		}
	}
	statusCases := map[string]int32{
		"Ok":                 1,
		"Error":              2,
		"Unset":              0,
		"STATUS_CODE_OK":     1,
		"STATUS_CODE_ERROR":  2,
		"":                   0,
		"weird-future-value": 0,
	}
	for in, want := range statusCases {
		if got := decodeStatusCode(in); got != want {
			t.Errorf("decodeStatusCode(%q) = %d, want %d", in, got, want)
		}
	}
}
