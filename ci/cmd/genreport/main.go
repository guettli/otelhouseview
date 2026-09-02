// Command genreport reads the OTel tables the collector's clickhouseexporter
// created (otel_logs, otel_traces, otel_metrics_gauge) and writes a report.json
// in the exact shape ui/src/lib/report.ts defines. The CI pipeline then bakes
// that JSON into the single-file Svelte report.
//
// It fails loudly on any query error rather than emitting a partial report — a
// truncated report would look successful while hiding a broken pipeline.
//
// Usage: genreport -dsn clickhouse://... -out report.json [-assert]
//
// With -assert it also fails (non-zero exit) if any seeded signal came back
// empty — the e2e's proof that data actually round-tripped
// collector → ClickHouse → query (issue #2).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

type report struct {
	GeneratedAt string      `json:"generatedAt"`
	Source      source      `json:"source"`
	Window      window      `json:"window"`
	Summary     summary     `json:"summary"`
	LogVolume   []logBucket `json:"logVolume"`
	LogSeverity []sevCount  `json:"logSeverity"`
	Metrics     []metric    `json:"metrics"`
	Traces      []traceRow  `json:"traces"`
}

type source struct {
	Repo   string `json:"repo"`
	Commit string `json:"commit"`
	RunURL string `json:"runURL"`
}
type window struct {
	From string `json:"from"`
	To   string `json:"to"`
}
type summary struct {
	Logs         uint64 `json:"logs"`
	Spans        uint64 `json:"spans"`
	Traces       uint64 `json:"traces"`
	MetricPoints uint64 `json:"metricPoints"`
	ErrorLogs    uint64 `json:"errorLogs"`
}
type logBucket struct {
	T        string `json:"t"`
	Severity string `json:"severity"`
	Count    uint64 `json:"count"`
}
type sevCount struct {
	Severity string `json:"severity"`
	Count    uint64 `json:"count"`
}
type metric struct {
	Name   string        `json:"name"`
	Unit   string        `json:"unit"`
	Latest float64       `json:"latest"`
	Points []metricPoint `json:"points"`
}
type metricPoint struct {
	T     string  `json:"t"`
	Value float64 `json:"value"`
}
type traceRow struct {
	TraceID    string  `json:"traceId"`
	Service    string  `json:"service"`
	RootSpan   string  `json:"rootSpan"`
	SpanCount  uint64  `json:"spanCount"`
	DurationMs float64 `json:"durationMs"`
	Status     string  `json:"status"`
	StartTime  string  `json:"startTime"`
}

func main() {
	var (
		dsn    = flag.String("dsn", os.Getenv("CLICKHOUSE_DSN"), "ClickHouse DSN (clickhouse://user:pass@host:9000/db)")
		out    = flag.String("out", "report.json", "output path for report.json")
		assert = flag.Bool("assert", false, "fail (non-zero exit) if any seeded signal is empty — the e2e proof that data round-tripped collector→ClickHouse→query")
	)
	flag.Parse()
	if *dsn == "" {
		fatal("no -dsn and CLICKHOUSE_DSN is empty")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	opts, err := clickhouse.ParseDSN(*dsn)
	if err != nil {
		fatal("parse dsn: %v", err)
	}
	conn, err := clickhouse.Open(opts)
	if err != nil {
		fatal("open clickhouse: %v", err)
	}
	defer func() { _ = conn.Close() }()

	rep, err := build(ctx, conn)
	if err != nil {
		fatal("build report: %v", err)
	}

	if *assert {
		if err := assertSeeded(rep); err != nil {
			fatal("assert: %v", err)
		}
	}

	blob, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		fatal("marshal: %v", err)
	}
	if err := os.WriteFile(*out, append(blob, '\n'), 0o644); err != nil {
		fatal("write %s: %v", *out, err)
	}
	fmt.Fprintf(os.Stderr, "genreport: wrote %s (%d logs, %d spans, %d traces, %d metric points)\n",
		*out, rep.Summary.Logs, rep.Summary.Spans, rep.Summary.Traces, rep.Summary.MetricPoints)
}

func build(ctx context.Context, conn driver.Conn) (*report, error) {
	rep := &report{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Source: source{
			Repo:   envOr("OTELHOUSEVIEW_REPO", "guettli/otelhouseview"),
			Commit: envOr("OTELHOUSEVIEW_COMMIT", "0000000000000000000000000000000000000000"),
			RunURL: envOr("OTELHOUSEVIEW_RUN_URL", "https://github.com/guettli/otelhouseview"),
		},
		LogVolume:   []logBucket{},
		LogSeverity: []sevCount{},
		Metrics:     []metric{},
		Traces:      []traceRow{},
	}

	// Summary counts.
	if err := conn.QueryRow(ctx, `SELECT count() FROM otel_logs`).Scan(&rep.Summary.Logs); err != nil {
		return nil, fmt.Errorf("count logs: %w", err)
	}
	if err := conn.QueryRow(ctx, `SELECT count() FROM otel_logs WHERE SeverityText = 'ERROR'`).Scan(&rep.Summary.ErrorLogs); err != nil {
		return nil, fmt.Errorf("count error logs: %w", err)
	}
	if err := conn.QueryRow(ctx, `SELECT count() FROM otel_traces`).Scan(&rep.Summary.Spans); err != nil {
		return nil, fmt.Errorf("count spans: %w", err)
	}
	if err := conn.QueryRow(ctx, `SELECT uniqExact(TraceId) FROM otel_traces`).Scan(&rep.Summary.Traces); err != nil {
		return nil, fmt.Errorf("count traces: %w", err)
	}
	if err := conn.QueryRow(ctx, `SELECT count() FROM otel_metrics_gauge WHERE MetricName LIKE 'otelhouseview%'`).Scan(&rep.Summary.MetricPoints); err != nil {
		return nil, fmt.Errorf("count metric points: %w", err)
	}

	// Window from the log timestamps.
	var from, to time.Time
	if err := conn.QueryRow(ctx, `SELECT min(Timestamp), max(Timestamp) FROM otel_logs`).Scan(&from, &to); err != nil {
		return nil, fmt.Errorf("log window: %w", err)
	}
	rep.Window = window{From: from.UTC().Format(time.RFC3339), To: to.UTC().Format(time.RFC3339)}

	// Log volume, bucketed per minute by severity.
	if err := scan(ctx, conn, `
		SELECT toStartOfInterval(Timestamp, INTERVAL 60 second) AS t, SeverityText AS sev, count() AS c
		FROM otel_logs GROUP BY t, sev ORDER BY t, sev`,
		func(rows driver.Rows) error {
			var t time.Time
			var sev string
			var c uint64
			if err := rows.Scan(&t, &sev, &c); err != nil {
				return err
			}
			if sev == "" {
				sev = "UNSET"
			}
			rep.LogVolume = append(rep.LogVolume, logBucket{T: t.UTC().Format(time.RFC3339), Severity: sev, Count: c})
			return nil
		}); err != nil {
		return nil, fmt.Errorf("log volume: %w", err)
	}

	// Severity totals.
	if err := scan(ctx, conn, `
		SELECT SeverityText AS sev, count() AS c FROM otel_logs GROUP BY sev ORDER BY c DESC`,
		func(rows driver.Rows) error {
			var sev string
			var c uint64
			if err := rows.Scan(&sev, &c); err != nil {
				return err
			}
			if sev == "" {
				sev = "UNSET"
			}
			rep.LogSeverity = append(rep.LogSeverity, sevCount{Severity: sev, Count: c})
			return nil
		}); err != nil {
		return nil, fmt.Errorf("log severity: %w", err)
	}

	// Metric series: gauge points averaged per second bucket, per metric name.
	byMetric := map[string]*metric{}
	order := []string{}
	if err := scan(ctx, conn, `
		SELECT MetricName AS name, toStartOfInterval(TimeUnix, INTERVAL 1 second) AS t, avg(Value) AS v
		FROM otel_metrics_gauge WHERE MetricName LIKE 'otelhouseview%'
		GROUP BY name, t ORDER BY name, t`,
		func(rows driver.Rows) error {
			var name string
			var t time.Time
			var v float64
			if err := rows.Scan(&name, &t, &v); err != nil {
				return err
			}
			m, ok := byMetric[name]
			if !ok {
				m = &metric{Name: name, Unit: "1", Points: []metricPoint{}}
				byMetric[name] = m
				order = append(order, name)
			}
			m.Points = append(m.Points, metricPoint{T: t.UTC().Format(time.RFC3339), Value: round2(v)})
			m.Latest = round2(v)
			return nil
		}); err != nil {
		return nil, fmt.Errorf("metrics: %w", err)
	}
	for _, name := range order {
		rep.Metrics = append(rep.Metrics, *byMetric[name])
	}

	// Recent traces, newest first.
	if err := scan(ctx, conn, `
		SELECT
			TraceId,
			any(ServiceName) AS svc,
			min(Timestamp) AS start,
			max(toUnixTimestamp64Nano(Timestamp) + toInt64(Duration)) AS endNano,
			count() AS spans,
			anyIf(SpanName, ParentSpanId = '' OR ParentSpanId = '0000000000000000') AS rootSpan,
			anyIf(StatusCode, ParentSpanId = '' OR ParentSpanId = '0000000000000000') AS status
		FROM otel_traces
		GROUP BY TraceId
		ORDER BY start DESC
		LIMIT 15`,
		func(rows driver.Rows) error {
			var (
				tr      traceRow
				start   time.Time
				endNano int64
			)
			if err := rows.Scan(&tr.TraceID, &tr.Service, &start, &endNano, &tr.SpanCount, &tr.RootSpan, &tr.Status); err != nil {
				return err
			}
			durNs := endNano - start.UnixNano()
			if durNs < 0 {
				durNs = 0
			}
			tr.DurationMs = round2(float64(durNs) / 1e6)
			tr.StartTime = start.UTC().Format(time.RFC3339)
			if tr.Status == "" {
				tr.Status = "STATUS_CODE_UNSET"
			}
			rep.Traces = append(rep.Traces, tr)
			return nil
		}); err != nil {
		return nil, fmt.Errorf("traces: %w", err)
	}

	if rep.Summary.Logs == 0 && rep.Summary.Spans == 0 {
		return nil, fmt.Errorf("no telemetry found in ClickHouse — collector/emit pipeline produced nothing")
	}
	return rep, nil
}

func scan(ctx context.Context, conn driver.Conn, query string, fn func(driver.Rows) error) error {
	rows, err := conn.Query(ctx, query)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if err := fn(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

func round2(v float64) float64 {
	return float64(int64(v*100+0.5*sign(v))) / 100
}
func sign(v float64) float64 {
	if v < 0 {
		return -1
	}
	return 1
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// assertSeeded checks that every signal the e2e harness seeds actually
// round-tripped OTLP → collector → ClickHouse → query. Without it the pipeline
// would happily render an empty report if the collector never flushed or a
// starter query silently returned nothing — the failure mode issue #2 exists to
// catch. Reports every empty signal at once so a flush/schema regression is
// diagnosed in one run rather than whack-a-mole.
func assertSeeded(rep *report) error {
	var empty []string
	if rep.Summary.Logs == 0 {
		empty = append(empty, "logs")
	}
	if rep.Summary.Spans == 0 {
		empty = append(empty, "spans")
	}
	if rep.Summary.Traces == 0 {
		empty = append(empty, "traces")
	}
	if rep.Summary.MetricPoints == 0 {
		empty = append(empty, "metricPoints")
	}
	// The summary counts prove ingestion; the derived slices prove the starter
	// queries that back the report's charts return rows too.
	if len(rep.Metrics) == 0 {
		empty = append(empty, "metrics[] (metric starter query)")
	}
	if len(rep.Traces) == 0 {
		empty = append(empty, "traces[] (trace starter query)")
	}
	if len(rep.LogVolume) == 0 {
		empty = append(empty, "logVolume[] (log starter query)")
	}
	if len(empty) > 0 {
		return fmt.Errorf("seeded signals came back empty: %s — collector flush or a starter query is broken", strings.Join(empty, ", "))
	}
	return nil
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "genreport: "+format+"\n", args...)
	os.Exit(1)
}
