package otelstore

import (
	"context"
	"sort"
	"sync"
)

// MemoryStore keeps every span and log record in process memory. It is the
// test fallback used when no ClickHouse backend is configured.
//
// WriteSpans and WriteLogs are concrete helpers tests use to seed fixture
// data — they are deliberately not on the Store interface, because production
// writes go through the OpenTelemetry Collector and are never this package's
// responsibility.
type MemoryStore struct {
	mu    sync.Mutex
	spans map[string]Span // keyed by SpanID
	logs  []LogRecord
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{spans: make(map[string]Span)}
}

// WriteSpans seeds spans for tests. Spans with the same SpanID overwrite
// earlier rows so a test can iterate on a fixture without rebuilding it.
func (m *MemoryStore) WriteSpans(_ context.Context, spans []Span) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range spans {
		m.spans[s.SpanID] = s
	}
	return nil
}

// WriteLogs seeds log records for tests.
func (m *MemoryStore) WriteLogs(_ context.Context, logs []LogRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.logs = append(m.logs, logs...)
	return nil
}

// GetTrace implements Store.
func (m *MemoryStore) GetTrace(_ context.Context, traceID string) (Trace, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := Trace{TraceID: traceID}
	for _, s := range m.spans {
		if s.TraceID == traceID {
			out.Spans = append(out.Spans, s)
		}
	}
	if len(out.Spans) == 0 {
		return Trace{}, ErrNotFound
	}
	for _, l := range m.logs {
		if l.TraceID == traceID {
			out.Logs = append(out.Logs, l)
		}
	}
	sort.Slice(out.Spans, func(i, j int) bool { return out.Spans[i].StartTime.Before(out.Spans[j].StartTime) })
	sort.Slice(out.Logs, func(i, j int) bool { return out.Logs[i].Time.Before(out.Logs[j].Time) })
	return out, nil
}

// ListTraces implements Store.
func (m *MemoryStore) ListTraces(ctx context.Context, limit int) ([]TraceSummary, error) {
	return m.ListTracesFiltered(ctx, ListOptions{Limit: limit})
}

// ListTracesFiltered implements Store. Iterates every span once per call;
// acceptable because MemoryStore is only used in tests.
func (m *MemoryStore) ListTracesFiltered(_ context.Context, opts ListOptions) ([]TraceSummary, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	byTrace := map[string][]Span{}
	for _, s := range m.spans {
		if !opts.Since.IsZero() && s.StartTime.Before(opts.Since) {
			continue
		}
		byTrace[s.TraceID] = append(byTrace[s.TraceID], s)
	}

	summaries := make([]TraceSummary, 0, len(byTrace))
	for id, spans := range byTrace {
		if !matchesAttrKeys(spans, opts.ResourceAttrKeys) {
			continue
		}
		t := Trace{TraceID: id, Spans: spans}
		root, ok := t.Root()
		if !ok {
			continue
		}
		start, end := root.StartTime, root.EndTime
		for _, s := range spans {
			if s.StartTime.Before(start) {
				start = s.StartTime
			}
			if s.EndTime.After(end) {
				end = s.EndTime
			}
		}
		summaries = append(summaries, TraceSummary{
			TraceID:            id,
			ServiceName:        root.ServiceName,
			Name:               root.Name,
			StartTime:          start,
			EndTime:            end,
			StatusCode:         root.StatusCode,
			ResourceAttributes: root.ResourceAttributes,
		})
	}
	sort.Slice(summaries, func(i, j int) bool { return summaries[i].StartTime.After(summaries[j].StartTime) })

	limit := opts.Limit
	if limit <= 0 {
		limit = 100
	}
	if len(summaries) > limit {
		summaries = summaries[:limit]
	}
	return summaries, nil
}

// matchesAttrKeys reports whether any span carries any of keys as a resource
// attribute. No keys means no filter.
func matchesAttrKeys(spans []Span, keys []string) bool {
	if len(keys) == 0 {
		return true
	}
	for _, s := range spans {
		for _, k := range keys {
			if _, ok := s.ResourceAttributes[k]; ok {
				return true
			}
		}
	}
	return false
}

// Close implements Store.
func (m *MemoryStore) Close() error { return nil }
