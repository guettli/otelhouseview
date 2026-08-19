// Package otelstore is a read-only, typed client for OpenTelemetry traces and
// logs stored in ClickHouse by the upstream opentelemetry-collector-contrib
// clickhouseexporter (tables otel_traces and otel_logs, stock schema).
//
// The package exposes one interface — Store — and two implementations:
//
//   - ClickHouseStore reads the exporter's tables. Writes are owned by the
//     Collector; this package never issues DDL or INSERT.
//   - MemoryStore holds everything in memory. It is the test fallback and
//     keeps WriteSpans / WriteLogs as concrete helpers tests use to seed
//     fixture data — those helpers are deliberately not on the interface.
//
// # Tenancy
//
// This package is deliberately tenant-blind. In a multi-tenant deployment the
// isolation boundary is the ClickHouse identity in the DSN: the otelhouse
// gateway stamps ResourceAttributes['tenant'] on every record at write time,
// and reads are constrained by a ClickHouse row policy bound to a per-tenant
// read-only user. Callers therefore pass a DSN that is already scoped to one
// tenant, and no query here adds a tenant predicate. Do not add one: a filter
// in Go would look like a security boundary without being one.
package otelstore

import (
	"context"
	"errors"
	"time"
)

// Span is one OpenTelemetry span, flattened so each row carries enough resource
// context to be rendered on its own.
type Span struct {
	TraceID            string
	SpanID             string
	ParentSpanID       string
	Name               string
	Kind               int32
	StartTime          time.Time
	EndTime            time.Time
	StatusCode         int32
	StatusMessage      string
	Attributes         map[string]string
	ResourceAttributes map[string]string
	ServiceName        string
}

// IsRoot reports whether s is the root of its trace. The OTLP convention is
// that a root span has an empty (or all-zero) parent span id.
func (s Span) IsRoot() bool { return s.ParentSpanID == "" }

// Duration returns the wall-clock duration covered by the span.
func (s Span) Duration() time.Duration { return s.EndTime.Sub(s.StartTime) }

// LogRecord is one OpenTelemetry log record, flattened the same way Span is.
//
// SpanID may be empty (record not associated with a span); TraceID is also
// optional, but the viewer only renders logs that carry a trace id so the
// page always has somewhere to attach them.
type LogRecord struct {
	TraceID            string
	SpanID             string
	Time               time.Time
	SeverityNumber     int32
	SeverityText       string
	Body               string
	Attributes         map[string]string
	ResourceAttributes map[string]string
	ServiceName        string
}

// Trace is the materialised view the HTML viewer renders: every span plus
// every log record that share the trace id.
type Trace struct {
	TraceID string
	Spans   []Span
	Logs    []LogRecord
}

// Root returns the trace's effective root span, or false if the trace is
// empty.
//
// A span with no parent is the root. When there is none, the earliest span
// stands in — that is not a guess about missing data, it is the normal shape
// of a trace whose parent context was minted outside the store. A CI workflow
// that generates its own TRACEPARENT and hands it to the pipeline is exactly
// that case: every exported span names a parent that was never itself
// exported, so no span has an empty parent and the trace would otherwise look
// rootless forever.
// Ties are broken the same way the ClickHouse listing breaks them: a
// parentless span wins over a parented one, and among equals the earliest
// starts. Callers must not depend on slice order.
func (t Trace) Root() (Span, bool) {
	best := -1
	for i, s := range t.Spans {
		if best < 0 {
			best = i
			continue
		}
		b := t.Spans[best]
		if s.IsRoot() != b.IsRoot() {
			if s.IsRoot() {
				best = i
			}
			continue
		}
		if s.StartTime.Before(b.StartTime) {
			best = i
		}
	}
	if best < 0 {
		return Span{}, false
	}
	return t.Spans[best], true
}

// TraceSummary is the per-trace row rendered on the viewer index page.
//
// The identity fields come from the trace's *effective* root — the span with
// no parent, or, when the real parent is not in the store, the earliest span
// (see Trace.Root). StartTime / EndTime span the whole trace, not just that
// one span, so the duration is the run's wall clock.
type TraceSummary struct {
	TraceID     string
	ServiceName string
	Name        string
	StartTime   time.Time
	EndTime     time.Time
	StatusCode  int32

	// ResourceAttributes are the effective root span's resource attributes.
	// They are what carries a run's provenance — for a CI trace, the repo,
	// branch and commit a renderer wants in the listing — so the caller does
	// not have to fetch the whole trace to label a row.
	ResourceAttributes map[string]string
}

// ListOptions constrains a trace listing. The zero value lists the most
// recent traces of every kind at the default limit.
type ListOptions struct {
	// Limit caps how many traces are returned. Zero or negative means the
	// default of 100.
	Limit int

	// ResourceAttrKeys keeps only traces carrying at least one of these
	// resource-attribute keys on at least one of their spans. Empty means no
	// filter.
	//
	// This is key *presence*, not a value match, on purpose: it is what
	// separates kinds of producer ("this trace came from a CI pipeline")
	// without the caller having to enumerate values. Matching a value is the
	// caller's job once it has the rows.
	ResourceAttrKeys []string

	// Since drops spans older than this instant before traces are assembled.
	// The zero value means no lower bound. Set it: the query is a scan, and
	// the bound is the difference between reading a week and reading the
	// whole retention window.
	Since time.Time
}

// Store is the read interface over a trace store. Both MemoryStore and
// ClickHouseStore implement it. Writes are out of scope: the Collector lands
// data into ClickHouse directly, and MemoryStore exposes its seed helpers as
// concrete methods so they stay out of the interface.
type Store interface {
	// GetTrace returns every span and log row sharing traceID. The slices
	// are sorted by start / time so the viewer renders them in causal order.
	GetTrace(ctx context.Context, traceID string) (Trace, error)

	// ListTraces returns the most recent traces, newest first. It is
	// ListTracesFiltered with nothing but a limit set.
	ListTraces(ctx context.Context, limit int) ([]TraceSummary, error)

	// ListTracesFiltered returns the most recent traces matching opts,
	// newest first by trace start time.
	ListTracesFiltered(ctx context.Context, opts ListOptions) ([]TraceSummary, error)

	// Close releases any underlying resources. Safe to call multiple times.
	Close() error
}

// ErrNotFound is returned by GetTrace when the trace id is unknown.
var ErrNotFound = errors.New("otelstore: trace not found")
