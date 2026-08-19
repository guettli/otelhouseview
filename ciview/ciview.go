// Package ciview renders one OTLP trace as a self-contained static HTML page.
//
// It is the trace waterfall view: RenderTrace produces the page for a single
// trace, RenderIndex the listing of recent ones. Both take otelstore types, so
// any Store backs them — ClickHouse in production, MemoryStore in tests.
//
// The output has no backend and no framework: a caller mounts the bytes on
// whatever route it likes (agentloop serves them at /ci/<traceID>).
//
// Each rendered page presents one OTLP trace as a pipeline run:
//
//   - The root span's name, status, start / end timestamps and total duration.
//   - A Gantt-style waterfall of every span, laid out horizontally against
//     the trace's wall-clock window so the reader can see where the run
//     spent its time. Clicking a bar jumps to that span's row below.
//   - A collapsible tree of every child span with its own duration.
//   - The log records that were emitted under each span, hidden behind a
//     "Logs" toggle so the page is readable even on noisy runs.
//
// The trace page ships a small inline JavaScript snippet that wires up
// expand/collapse-all buttons, waterfall-to-row jumps and client-side log
// search / severity filters; no external framework is loaded. The index
// page (RenderIndex) lists the most recent runs.
package ciview

import (
	"bytes"
	_ "embed"
	"fmt"
	"html/template"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/guettli/otelhouseview/otelstore"
)

//go:embed templates/trace.html.tmpl
var traceTmplSrc string

//go:embed templates/index.html.tmpl
var indexTmplSrc string

var (
	traceTmpl = template.Must(template.New("trace").Funcs(funcMap()).Parse(traceTmplSrc))
	indexTmpl = template.Must(template.New("index").Funcs(funcMap()).Parse(indexTmplSrc))
)

// statusLabels maps the OTLP Status_StatusCode enum to a short human label.
// Values come from opentelemetry/proto/trace/v1/Status.StatusCode.
var statusLabels = map[int32]string{
	0: "unset",
	1: "ok",
	2: "error",
}

func funcMap() template.FuncMap {
	return template.FuncMap{
		"fmtTime":       fmtTime,
		"fmtDuration":   fmtDuration,
		"fmtPct":        fmtPct,
		"statusLabel":   statusLabel,
		"statusClass":   statusClass,
		"sortKeys":      sortKeys,
		"severityClass": severityClass,
		"shortSHA":      shortSHA,
	}
}

// shortSHA abbreviates a git object id to the 7 characters humans compare.
// Anything that is not a full-length hex sha is returned untouched — a tag or
// a branch name is already the readable form.
func shortSHA(s string) string {
	if len(s) != 40 {
		return s
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return s
		}
	}
	return s[:7]
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.UTC().Format("2006-01-02 15:04:05 UTC")
}

// fmtPct renders a float percentage in a CSS-friendly form. Values are
// clamped to [0, 100] so a badly-timestamped span cannot escape its lane.
func fmtPct(v float64) string {
	if v < 0 {
		v = 0
	}
	if v > 100 {
		v = 100
	}
	return strconv.FormatFloat(v, 'f', 4, 64)
}

func fmtDuration(d time.Duration) string {
	if d <= 0 {
		return "—"
	}
	if d < time.Millisecond {
		return d.String()
	}
	if d < time.Second {
		return fmt.Sprintf("%d ms", d.Milliseconds())
	}
	return d.Truncate(time.Millisecond).String()
}

func statusLabel(code int32) string {
	if s, ok := statusLabels[code]; ok {
		return s
	}
	return fmt.Sprintf("code %d", code)
}

func statusClass(code int32) string {
	switch code {
	case 1:
		return "ok"
	case 2:
		return "error"
	}
	return "unset"
}

// severityClass maps an OTLP severity text to a short CSS class so the
// template can color-code each log line. Unknown levels render as "default".
func severityClass(sev string) string {
	switch strings.ToLower(strings.TrimSpace(sev)) {
	case "fatal", "fatal2", "fatal3", "fatal4", "error", "error2", "error3", "error4":
		return "error"
	case "warn", "warning", "warn2", "warn3", "warn4":
		return "warn"
	case "info", "info2", "info3", "info4":
		return "info"
	case "debug", "debug2", "debug3", "debug4":
		return "debug"
	case "trace", "trace2", "trace3", "trace4":
		return "trace"
	}
	return "default"
}

func sortKeys(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// SpanNode is the recursive view-model the template renders.
//
// OffsetPct and WidthPct place the span on the waterfall: they are the
// span's start offset and duration expressed as a percentage of the whole
// trace's wall-clock window. Both are 0 when the trace has no duration
// (a still-in-flight trace whose root end time is unknown, or a fixture
// with instant spans) — the template then just draws a hairline bar.
type SpanNode struct {
	Span      otelstore.Span
	Depth     int
	Children  []*SpanNode
	Logs      []otelstore.LogRecord
	OffsetPct float64
	WidthPct  float64
}

// buildTree groups every span under its parent. Orphan spans (parent id set
// but parent absent — happens when the receiver gets the child batch before
// the parent batch) are attached at the root so they still show up.
func buildTree(trace otelstore.Trace) (*SpanNode, []*SpanNode) {
	byID := make(map[string]*SpanNode, len(trace.Spans))
	for i := range trace.Spans {
		s := trace.Spans[i]
		byID[s.SpanID] = &SpanNode{Span: s}
	}

	logsBySpan := make(map[string][]otelstore.LogRecord, len(trace.Logs))
	for _, l := range trace.Logs {
		logsBySpan[l.SpanID] = append(logsBySpan[l.SpanID], l)
	}
	for id, node := range byID {
		node.Logs = logsBySpan[id]
	}

	var root *SpanNode
	var orphans []*SpanNode
	for _, node := range byID {
		if node.Span.IsRoot() {
			root = node
			continue
		}
		parent, ok := byID[node.Span.ParentSpanID]
		if ok {
			parent.Children = append(parent.Children, node)
			continue
		}
		orphans = append(orphans, node)
	}

	walk(root, 0)
	for _, o := range orphans {
		walk(o, 0)
	}
	return root, orphans
}

// walk assigns Depth and sorts every Children slice by StartTime so the
// template renders them in causal order. Logs are sorted the same way.
func walk(n *SpanNode, depth int) {
	if n == nil {
		return
	}
	n.Depth = depth
	sort.Slice(n.Children, func(i, j int) bool {
		return n.Children[i].Span.StartTime.Before(n.Children[j].Span.StartTime)
	})
	sort.Slice(n.Logs, func(i, j int) bool { return n.Logs[i].Time.Before(n.Logs[j].Time) })
	for _, c := range n.Children {
		walk(c, depth+1)
	}
}

// flatten linearises the tree depth-first so the template can render a single
// indented list — html/template's nested ranges are awkward to indent.
func flatten(n *SpanNode) []*SpanNode {
	if n == nil {
		return nil
	}
	out := []*SpanNode{n}
	for _, c := range n.Children {
		out = append(out, flatten(c)...)
	}
	return out
}

// traceView is the value passed to the trace template.
type traceView struct {
	TraceID  string
	Root     *SpanNode
	Flat     []*SpanNode
	Orphans  []*SpanNode
	Duration time.Duration
	HasRoot  bool
}

// RenderTrace produces the standalone HTML page for a single trace. It does
// not write to disk; the caller decides whether to serve it inline or
// snapshot it as a static file.
func RenderTrace(trace otelstore.Trace) ([]byte, error) {
	root, orphans := buildTree(trace)
	view := traceView{TraceID: trace.TraceID, Root: root, Orphans: orphans}
	if root != nil {
		view.HasRoot = true
		view.Duration = root.Span.Duration()
		view.Flat = flatten(root)
	}
	for _, o := range orphans {
		view.Flat = append(view.Flat, flatten(o)...)
	}
	layoutWaterfall(view.Flat)
	var buf bytes.Buffer
	if err := traceTmpl.Execute(&buf, view); err != nil {
		return nil, fmt.Errorf("ciview: render trace: %w", err)
	}
	return buf.Bytes(), nil
}

// layoutWaterfall assigns each node its OffsetPct and WidthPct against the
// wall-clock window spanned by every rendered span. Using the union (not the
// root span's window) keeps orphan and root-less traces laid out correctly:
// a partial trace still lays every visible span against the same shared
// timeline instead of overshooting a shorter root.
func layoutWaterfall(nodes []*SpanNode) {
	if len(nodes) == 0 {
		return
	}
	var start, end time.Time
	for _, n := range nodes {
		if start.IsZero() || n.Span.StartTime.Before(start) {
			start = n.Span.StartTime
		}
		if end.IsZero() || n.Span.EndTime.After(end) {
			end = n.Span.EndTime
		}
	}
	total := end.Sub(start)
	if total <= 0 {
		return
	}
	totalNs := float64(total.Nanoseconds())
	for _, n := range nodes {
		offset := n.Span.StartTime.Sub(start)
		if offset < 0 {
			offset = 0
		}
		width := n.Span.Duration()
		if width < 0 {
			width = 0
		}
		n.OffsetPct = float64(offset.Nanoseconds()) / totalNs * 100
		n.WidthPct = float64(width.Nanoseconds()) / totalNs * 100
		// Ensure zero-duration spans stay visible as a hairline tick.
		if n.WidthPct < 0.2 {
			n.WidthPct = 0.2
		}
		if n.OffsetPct+n.WidthPct > 100 {
			n.WidthPct = 100 - n.OffsetPct
		}
	}
}

// vcsAttrs lists, per column, the resource-attribute keys that can carry a
// run's provenance, most specific first.
//
// The ci.* keys are what a workflow sets deliberately; the dagger.io/* keys
// are what the Dagger CLI stamps on its own. Both are read so a run is
// labelled whether or not its workflow was updated — and the deliberate key
// wins, because the Dagger one is derived from the checkout and on a
// pull_request that is a merge commit nobody can look up.
var vcsAttrs = struct{ Repo, Branch, Commit []string }{
	Repo:   []string{"ci.repo", "dagger.io/vcs.repo.full_name"},
	Branch: []string{"ci.branch", "dagger.io/git.branch"},
	Commit: []string{"ci.commit", "dagger.io/git.ref"},
}

// indexRow is one line of the listing: the store's summary plus the run
// provenance pulled out of its resource attributes.
type indexRow struct {
	otelstore.TraceSummary
	Repo   string
	Branch string
	Commit string
}

// indexData is what the index template renders.
type indexData struct {
	Rows []indexRow
	// ShowVCS keeps the repo / branch / commit columns off the page entirely
	// when nothing in the listing has them, rather than showing three empty
	// columns to a deployment whose producers are not CI pipelines.
	ShowVCS bool
}

// firstAttr returns the value of the first key present in attrs.
func firstAttr(attrs map[string]string, keys []string) string {
	for _, k := range keys {
		if v, ok := attrs[k]; ok && v != "" {
			return v
		}
	}
	return ""
}

// RenderIndex produces the list page shown at /ci/.
func RenderIndex(summaries []otelstore.TraceSummary) ([]byte, error) {
	data := indexData{Rows: make([]indexRow, 0, len(summaries))}
	for _, s := range summaries {
		r := indexRow{
			TraceSummary: s,
			Repo:         firstAttr(s.ResourceAttributes, vcsAttrs.Repo),
			Branch:       firstAttr(s.ResourceAttributes, vcsAttrs.Branch),
			Commit:       firstAttr(s.ResourceAttributes, vcsAttrs.Commit),
		}
		if r.Repo != "" || r.Branch != "" || r.Commit != "" {
			data.ShowVCS = true
		}
		data.Rows = append(data.Rows, r)
	}
	var buf bytes.Buffer
	if err := indexTmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("ciview: render index: %w", err)
	}
	return buf.Bytes(), nil
}
