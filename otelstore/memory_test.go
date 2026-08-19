package otelstore

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMemoryStoreWriteReadTrace(t *testing.T) {
	m := NewMemoryStore()
	t0 := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	root := Span{
		TraceID:     "trace1",
		SpanID:      "root",
		Name:        "build",
		StartTime:   t0,
		EndTime:     t0.Add(2 * time.Second),
		ServiceName: "dagger",
		StatusCode:  1,
	}
	child := Span{
		TraceID:      "trace1",
		SpanID:       "child",
		ParentSpanID: "root",
		Name:         "go test",
		StartTime:    t0.Add(100 * time.Millisecond),
		EndTime:      t0.Add(1 * time.Second),
		ServiceName:  "dagger",
	}
	if err := m.WriteSpans(context.Background(), []Span{child, root}); err != nil {
		t.Fatalf("WriteSpans: %v", err)
	}
	if err := m.WriteLogs(context.Background(), []LogRecord{{
		TraceID:      "trace1",
		SpanID:       "child",
		Time:         t0.Add(200 * time.Millisecond),
		SeverityText: "INFO",
		Body:         "hello",
	}}); err != nil {
		t.Fatalf("WriteLogs: %v", err)
	}
	got, err := m.GetTrace(context.Background(), "trace1")
	if err != nil {
		t.Fatalf("GetTrace: %v", err)
	}
	if len(got.Spans) != 2 {
		t.Fatalf("want 2 spans, got %d", len(got.Spans))
	}
	if !got.Spans[0].StartTime.Equal(root.StartTime) {
		t.Errorf("spans not sorted: first start %v, want %v", got.Spans[0].StartTime, root.StartTime)
	}
	r, ok := got.Root()
	if !ok || r.SpanID != "root" {
		t.Errorf("Root() = (%+v, %v), want root span", r, ok)
	}
	if len(got.Logs) != 1 || got.Logs[0].Body != "hello" {
		t.Errorf("logs not persisted, got %+v", got.Logs)
	}
}

func TestMemoryStoreUpsertSpan(t *testing.T) {
	m := NewMemoryStore()
	s := Span{TraceID: "t", SpanID: "s", Name: "v1", StartTime: time.Now()}
	if err := m.WriteSpans(context.Background(), []Span{s}); err != nil {
		t.Fatal(err)
	}
	s.Name = "v2"
	if err := m.WriteSpans(context.Background(), []Span{s}); err != nil {
		t.Fatal(err)
	}
	got, err := m.GetTrace(context.Background(), "t")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Spans) != 1 || got.Spans[0].Name != "v2" {
		t.Errorf("upsert failed: %+v", got.Spans)
	}
}

// A trace whose parent context was minted outside the store — the shape a CI
// workflow produces when it generates its own TRACEPARENT — has no span with
// an empty parent. It must still be listed: it used to be dropped, so those
// runs were fetchable by id and invisible in the index.
func TestMemoryStoreListTracesIncludesExternallyParentedTraces(t *testing.T) {
	m := NewMemoryStore()
	t0 := time.Now()
	// Trace 'a': every span names a parent that was never exported.
	if err := m.WriteSpans(context.Background(), []Span{{
		TraceID: "a", SpanID: "c", ParentSpanID: "missing",
		Name: "pipeline", ServiceName: "dagger-cli",
		StartTime: t0, EndTime: t0.Add(time.Second),
	}, {
		TraceID: "a", SpanID: "c2", ParentSpanID: "c",
		Name: "step", StartTime: t0.Add(time.Second), EndTime: t0.Add(5 * time.Second),
	}}); err != nil {
		t.Fatal(err)
	}
	// Trace 'b' has a conventional root.
	if err := m.WriteSpans(context.Background(), []Span{{
		TraceID: "b", SpanID: "r",
		Name: "root", StartTime: t0.Add(2 * time.Second), EndTime: t0.Add(3 * time.Second),
	}}); err != nil {
		t.Fatal(err)
	}
	got, err := m.ListTraces(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("ListTraces = %+v, want both traces", got)
	}
	// Newest first: 'b' starts at t0+2s, 'a' at t0.
	if got[0].TraceID != "b" || got[1].TraceID != "a" {
		t.Fatalf("ListTraces order = %s,%s; want b,a", got[0].TraceID, got[1].TraceID)
	}
	// The earliest span stands in for the missing root...
	if got[1].Name != "pipeline" || got[1].ServiceName != "dagger-cli" {
		t.Errorf("effective root = %q/%q, want pipeline/dagger-cli", got[1].Name, got[1].ServiceName)
	}
	// ...but the duration is the whole trace's wall clock, not that span's.
	if d := got[1].EndTime.Sub(got[1].StartTime); d != 5*time.Second {
		t.Errorf("duration = %s, want 5s (first span start to last span end)", d)
	}
}

func TestMemoryStoreListTracesFiltersByResourceAttrKey(t *testing.T) {
	m := NewMemoryStore()
	t0 := time.Now()
	if err := m.WriteSpans(context.Background(), []Span{{
		TraceID: "ci", SpanID: "r", Name: "check", StartTime: t0, EndTime: t0.Add(time.Second),
		ResourceAttributes: map[string]string{"ci.commit": "deadbeef"},
	}, {
		// Same trace, a producer that knows nothing about CI. The trace still
		// matches: membership is per trace, not per span.
		TraceID: "ci", SpanID: "e", ParentSpanID: "r", Name: "exec",
		StartTime: t0, EndTime: t0.Add(time.Second),
	}, {
		TraceID: "daemon", SpanID: "r2", Name: "tick",
		StartTime: t0.Add(time.Second), EndTime: t0.Add(2 * time.Second),
	}}); err != nil {
		t.Fatal(err)
	}
	got, err := m.ListTracesFiltered(context.Background(), ListOptions{ResourceAttrKeys: []string{"ci.commit"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].TraceID != "ci" {
		t.Fatalf("filtered listing = %+v, want only [ci]", got)
	}
	if got[0].ResourceAttributes["ci.commit"] != "deadbeef" {
		t.Errorf("summary lost the root resource attributes: %+v", got[0].ResourceAttributes)
	}
}

func TestMemoryStoreListTracesSince(t *testing.T) {
	m := NewMemoryStore()
	t0 := time.Now()
	if err := m.WriteSpans(context.Background(), []Span{{
		TraceID: "old", SpanID: "r", Name: "old", StartTime: t0, EndTime: t0.Add(time.Second),
	}, {
		TraceID: "new", SpanID: "r2", Name: "new",
		StartTime: t0.Add(time.Hour), EndTime: t0.Add(time.Hour + time.Second),
	}}); err != nil {
		t.Fatal(err)
	}
	got, err := m.ListTracesFiltered(context.Background(), ListOptions{Since: t0.Add(30 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].TraceID != "new" {
		t.Fatalf("listing = %+v, want only [new]", got)
	}
}

func TestMemoryStoreGetTraceNotFound(t *testing.T) {
	m := NewMemoryStore()
	if _, err := m.GetTrace(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}
