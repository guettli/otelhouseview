package otelstore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "github.com/ClickHouse/clickhouse-go/v2" // registers the "clickhouse" driver with database/sql
)

// defaultTracesTable / defaultLogsTable are the table names produced by the
// upstream OpenTelemetry Collector clickhouseexporter under its default
// configuration. This package only reads from these tables — writes are owned
// by the Collector.
const (
	defaultTracesTable = "otel_traces"
	defaultLogsTable   = "otel_logs"
)

// ClickHouseStore is a read-only Store implementation that queries the tables
// written by the opentelemetry-collector-contrib clickhouseexporter. It never
// writes: the Collector owns ingestion and schema creation.
type ClickHouseStore struct {
	db          *sql.DB
	tracesTable string
	logsTable   string
}

// Option customises a ClickHouseStore at open time.
type Option func(*ClickHouseStore)

// WithTracesTable overrides the span table name. Only needed when the
// clickhouseexporter is configured away from its default of otel_traces.
func WithTracesTable(name string) Option {
	return func(c *ClickHouseStore) { c.tracesTable = name }
}

// WithLogsTable overrides the log table name. Only needed when the
// clickhouseexporter is configured away from its default of otel_logs.
func WithLogsTable(name string) Option {
	return func(c *ClickHouseStore) { c.logsTable = name }
}

// OpenClickHouse dials dsn and returns a ClickHouseStore. The DSN is the
// standard clickhouse-go connection string, e.g.
// "clickhouse://user:pass@host:9000/otel?dial_timeout=10s".
//
// The DSN's identity is the tenancy boundary — see the package doc. Pass a
// DSN already scoped to one tenant's read-only user.
func OpenClickHouse(ctx context.Context, dsn string, opts ...Option) (*ClickHouseStore, error) {
	db, err := sql.Open("clickhouse", dsn)
	if err != nil {
		return nil, fmt.Errorf("otelstore: open clickhouse: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("otelstore: ping clickhouse: %w", err)
	}
	c := &ClickHouseStore{
		db:          db,
		tracesTable: defaultTracesTable,
		logsTable:   defaultLogsTable,
	}
	for _, o := range opts {
		o(c)
	}
	return c, nil
}

// DB exposes the underlying handle so callers can run their own read-only
// queries against the same connection — e.g. aggregating their own metrics out
// of otel_metrics_sum — without this package having to know about them. The
// returned handle is owned by the store; do not close it.
func (c *ClickHouseStore) DB() *sql.DB { return c.db }

// Close implements Store.
func (c *ClickHouseStore) Close() error {
	if c == nil || c.db == nil {
		return nil
	}
	return c.db.Close()
}

// GetTrace implements Store. It hits the spans table once and the logs table
// once; an empty result on the spans table is reported as ErrNotFound so the
// viewer can render a clean 404.
func (c *ClickHouseStore) GetTrace(ctx context.Context, traceID string) (Trace, error) {
	out := Trace{TraceID: traceID}

	spanQuery := fmt.Sprintf(`
SELECT TraceId, SpanId, ParentSpanId, SpanName, SpanKind,
       Timestamp, Duration,
       StatusCode, StatusMessage,
       SpanAttributes, ResourceAttributes, ServiceName
FROM %s
WHERE TraceId = ?
ORDER BY Timestamp`, c.tracesTable)
	rows, err := c.db.QueryContext(ctx, spanQuery, traceID)
	if err != nil {
		return Trace{}, fmt.Errorf("otelstore: query spans: %w", err)
	}
	for rows.Next() {
		var (
			s              Span
			kindStr, codeS string
			ts             time.Time
			durationNs     int64
			attrs, resAttr map[string]string
		)
		if err := rows.Scan(
			&s.TraceID, &s.SpanID, &s.ParentSpanID, &s.Name, &kindStr,
			&ts, &durationNs,
			&codeS, &s.StatusMessage,
			&attrs, &resAttr, &s.ServiceName,
		); err != nil {
			_ = rows.Close()
			return Trace{}, fmt.Errorf("otelstore: scan span: %w", err)
		}
		s.Kind = decodeSpanKind(kindStr)
		s.StartTime = ts.UTC()
		s.EndTime = ts.Add(time.Duration(durationNs)).UTC()
		s.StatusCode = decodeStatusCode(codeS)
		s.Attributes = attrs
		s.ResourceAttributes = resAttr
		out.Spans = append(out.Spans, s)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return Trace{}, fmt.Errorf("otelstore: iter spans: %w", err)
	}
	_ = rows.Close()

	if len(out.Spans) == 0 {
		return Trace{}, ErrNotFound
	}

	logQuery := fmt.Sprintf(`
SELECT TraceId, SpanId, Timestamp,
       SeverityNumber, SeverityText, Body,
       LogAttributes, ResourceAttributes, ServiceName
FROM %s
WHERE TraceId = ?
ORDER BY Timestamp`, c.logsTable)
	lrows, err := c.db.QueryContext(ctx, logQuery, traceID)
	if err != nil {
		return Trace{}, fmt.Errorf("otelstore: query logs: %w", err)
	}
	defer func() { _ = lrows.Close() }()
	for lrows.Next() {
		var (
			l              LogRecord
			ts             time.Time
			sevNum         uint8
			attrs, resAttr map[string]string
		)
		if err := lrows.Scan(
			&l.TraceID, &l.SpanID, &ts,
			&sevNum, &l.SeverityText, &l.Body,
			&attrs, &resAttr, &l.ServiceName,
		); err != nil {
			return Trace{}, fmt.Errorf("otelstore: scan log: %w", err)
		}
		l.Time = ts.UTC()
		l.SeverityNumber = int32(sevNum)
		l.Attributes = attrs
		l.ResourceAttributes = resAttr
		out.Logs = append(out.Logs, l)
	}
	if err := lrows.Err(); err != nil {
		return Trace{}, fmt.Errorf("otelstore: iter logs: %w", err)
	}
	return out, nil
}

// ListTraces implements Store.
func (c *ClickHouseStore) ListTraces(ctx context.Context, limit int) ([]TraceSummary, error) {
	return c.ListTracesFiltered(ctx, ListOptions{Limit: limit})
}

// ListTracesFiltered implements Store.
//
// The listing is built by aggregating each trace's spans rather than by
// selecting the row whose ParentSpanId is empty, which is what this used to
// do. That earlier query silently lost every trace whose parent context was
// minted outside the store — an externally-supplied TRACEPARENT leaves no
// span with an empty parent, so such a trace was fetchable by id but never
// listed. Aggregating also gives the run's true wall clock (first span start
// to last span end) instead of the root span's own duration.
//
// This is a full scan of the matching rows. Bound it with ListOptions.Since;
// a materialised view is the upgrade path once the scan stops being cheap.
func (c *ClickHouseStore) ListTracesFiltered(ctx context.Context, opts ListOptions) ([]TraceSummary, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = 100
	}

	var (
		where []string
		args  []any
	)
	if !opts.Since.IsZero() {
		where = append(where, "Timestamp >= ?")
		args = append(args, opts.Since)
	}
	if len(opts.ResourceAttrKeys) > 0 {
		// Membership is resolved per trace, not per span: the attributes that
		// identify a run sit on the producer's own spans, and a trace mixes
		// producers (a Dagger run carries both CLI and engine spans, and only
		// the CLI's resource knows it is CI). Matching per span would return
		// half a trace.
		sub := fmt.Sprintf(
			"TraceId IN (SELECT DISTINCT TraceId FROM %s WHERE hasAny(mapKeys(ResourceAttributes), ?)%s)",
			c.tracesTable, sinceClause(opts.Since),
		)
		where = append(where, sub)
		args = append(args, opts.ResourceAttrKeys)
		if !opts.Since.IsZero() {
			args = append(args, opts.Since)
		}
	}
	clause := ""
	if len(where) > 0 {
		clause = "WHERE " + strings.Join(where, " AND ") + "\n"
	}

	// The sort key picks the effective root, applying exactly the rule
	// Trace.Root applies in Go: a parentless span wins over a parented one
	// (the tuple's first element is 0 for a root, 1 otherwise), and among
	// equals the earliest starts. Sorting on Timestamp alone would be
	// ambiguous whenever two spans share a start instant, and would pick a
	// child over its own root under clock skew.
	const rootKey = "(ParentSpanId != '', Timestamp)"
	q := fmt.Sprintf(`
SELECT TraceId,
       argMin(ServiceName, %[1]s)        AS RootService,
       argMin(SpanName, %[1]s)           AS RootName,
       argMin(StatusCode, %[1]s)         AS RootStatus,
       argMin(ResourceAttributes, %[1]s) AS RootResource,
       min(Timestamp)                    AS Started,
       max(Timestamp + toIntervalNanosecond(Duration)) AS Ended
FROM %[2]s
%[3]sGROUP BY TraceId
ORDER BY Started DESC
LIMIT ?`, rootKey, c.tracesTable, clause)
	args = append(args, limit)

	rows, err := c.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("otelstore: list traces: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []TraceSummary
	for rows.Next() {
		var (
			t             TraceSummary
			codeS         string
			started, ends time.Time
		)
		if err := rows.Scan(
			&t.TraceID, &t.ServiceName, &t.Name, &codeS, &t.ResourceAttributes,
			&started, &ends,
		); err != nil {
			return nil, fmt.Errorf("otelstore: scan trace summary: %w", err)
		}
		t.StartTime = started.UTC()
		t.EndTime = ends.UTC()
		t.StatusCode = decodeStatusCode(codeS)
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("otelstore: iter trace summaries: %w", err)
	}
	return out, nil
}

// sinceClause returns the WHERE fragment the trace-membership subquery needs
// so it scans the same window as the outer query instead of the whole table.
func sinceClause(since time.Time) string {
	if since.IsZero() {
		return ""
	}
	return " AND Timestamp >= ?"
}

// decodeSpanKind translates the contrib exporter's textual SpanKind back into
// the OTLP enum value the viewer's templates expect. Unknown values fall back
// to 0 (SPAN_KIND_UNSPECIFIED) rather than failing the whole render.
func decodeSpanKind(s string) int32 {
	switch s {
	case "Internal", "SPAN_KIND_INTERNAL":
		return 1
	case "Server", "SPAN_KIND_SERVER":
		return 2
	case "Client", "SPAN_KIND_CLIENT":
		return 3
	case "Producer", "SPAN_KIND_PRODUCER":
		return 4
	case "Consumer", "SPAN_KIND_CONSUMER":
		return 5
	}
	return 0
}

// decodeStatusCode is the inverse mapping for Status.Code. Both the short and
// the full enum name are accepted because the contrib exporter has used both
// over its history.
func decodeStatusCode(s string) int32 {
	switch s {
	case "Ok", "STATUS_CODE_OK":
		return 1
	case "Error", "STATUS_CODE_ERROR":
		return 2
	}
	return 0
}

// Compile-time check that ClickHouseStore satisfies Store. The blank assignment
// keeps the assertion close to the implementation so a future interface change
// fails here rather than in a distant caller.
var _ Store = (*ClickHouseStore)(nil)
