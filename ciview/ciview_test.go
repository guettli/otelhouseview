package ciview

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/guettli/otelhouseview/otelstore"
)

// updateGolden regenerates the testdata/*.html files. Run with
// `go test ./internal/ciview -update` after intentional template changes.
var updateGolden = flag.Bool("update", false, "regenerate ciview golden files")

func sampleTrace() otelstore.Trace {
	t0 := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	return otelstore.Trace{
		TraceID: "00112233445566778899aabbccddeeff",
		Spans: []otelstore.Span{
			{
				TraceID:     "00112233445566778899aabbccddeeff",
				SpanID:      "0011223344556677",
				Name:        "build",
				ServiceName: "dagger",
				StartTime:   t0,
				EndTime:     t0.Add(2 * time.Second),
				StatusCode:  1,
				Attributes:  map[string]string{"repo": "guettli/agentloop"},
			},
			{
				TraceID:      "00112233445566778899aabbccddeeff",
				SpanID:       "8899aabbccddeeff",
				ParentSpanID: "0011223344556677",
				Name:         "go test",
				ServiceName:  "dagger",
				StartTime:    t0.Add(100 * time.Millisecond),
				EndTime:      t0.Add(1900 * time.Millisecond),
				StatusCode:   1,
			},
		},
		Logs: []otelstore.LogRecord{{
			TraceID:      "00112233445566778899aabbccddeeff",
			SpanID:       "8899aabbccddeeff",
			Time:         t0.Add(500 * time.Millisecond),
			SeverityText: "INFO",
			Body:         "PASS: TestSomething",
		}},
	}
}

func TestRenderTraceGolden(t *testing.T) {
	got, err := RenderTrace(sampleTrace())
	if err != nil {
		t.Fatal(err)
	}
	checkGolden(t, "trace.html", got)
}

func TestRenderIndexGolden(t *testing.T) {
	t0 := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	got, err := RenderIndex([]otelstore.TraceSummary{{
		TraceID:     "00112233445566778899aabbccddeeff",
		ServiceName: "dagger-cli",
		Name:        "check --source=.",
		StartTime:   t0,
		EndTime:     t0.Add(90 * time.Second),
		StatusCode:  1,
		ResourceAttributes: map[string]string{
			"ci.repo":   "guettli/agentloop",
			"ci.branch": "main",
			"ci.commit": "97b256d5ea92c7ebf68ebadcab2a532276a3bf52",
		},
	}, {
		// Dagger's own keys, for a producer whose workflow sets no ci.*.
		TraceID:     "ffeeddccbbaa99887766554433221100",
		ServiceName: "dagger-cli",
		Name:        "check",
		StartTime:   t0.Add(-time.Minute),
		EndTime:     t0.Add(-time.Second),
		StatusCode:  2,
		ResourceAttributes: map[string]string{
			"dagger.io/vcs.repo.full_name": "guettli/sharedinbox",
			"dagger.io/git.branch":         "main",
			"dagger.io/git.ref":            "2daef76413cf43e9c753a2a7fbeaf9391c9f3f9d",
		},
	}, {
		// No provenance at all — the row still renders, the cells are blank.
		TraceID:     "aaaabbbbccccddddeeeeffff00001111",
		ServiceName: "agentloop",
		Name:        "tick",
		StartTime:   t0.Add(-2 * time.Minute),
		EndTime:     t0.Add(-119 * time.Second),
		StatusCode:  1,
	}})
	if err != nil {
		t.Fatal(err)
	}
	checkGolden(t, "index.html", got)
}

// With no producer carrying provenance, the repo / branch / commit columns are
// not rendered at all — an otelhouseview deployment whose traces are not CI
// pipelines should not get three permanently empty columns.
func TestRenderIndexOmitsVCSColumnsWhenAbsent(t *testing.T) {
	t0 := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	got, err := RenderIndex([]otelstore.TraceSummary{{
		TraceID:     "00112233445566778899aabbccddeeff",
		ServiceName: "agentloop",
		Name:        "tick",
		StartTime:   t0,
		EndTime:     t0.Add(2 * time.Second),
		StatusCode:  1,
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, col := range []string{"<th>repo</th>", "<th>branch</th>", "<th>commit</th>"} {
		if strings.Contains(string(got), col) {
			t.Errorf("index rendered %s with no provenance in the data", col)
		}
	}
}

func TestShortSHA(t *testing.T) {
	for in, want := range map[string]string{
		"97b256d5ea92c7ebf68ebadcab2a532276a3bf52": "97b256d",
		"main":   "main",
		"v1.2.3": "v1.2.3",
		"":       "",
		"zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz": "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz",
	} {
		if got := shortSHA(in); got != want {
			t.Errorf("shortSHA(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRenderTraceEmptyIndex(t *testing.T) {
	got, err := RenderIndex(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "No runs yet") {
		t.Errorf("expected 'No runs yet' in empty index, got: %s", string(got))
	}
}

func TestRenderTraceWithOrphan(t *testing.T) {
	tr := sampleTrace()
	// Add an orphan child whose parent isn't in the batch yet.
	tr.Spans = append(tr.Spans, otelstore.Span{
		TraceID:      tr.TraceID,
		SpanID:       "ffffffffffffffff",
		ParentSpanID: "deadbeefdeadbeef",
		Name:         "orphan-step",
		StartTime:    tr.Spans[0].StartTime,
		EndTime:      tr.Spans[0].EndTime,
	})
	got, err := RenderTrace(tr)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "orphan-step") {
		t.Errorf("orphan span missing from output")
	}
}

func TestRenderTraceHasWaterfall(t *testing.T) {
	got, err := RenderTrace(sampleTrace())
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	wants := []string{
		`section class="waterfall"`,
		`class="wf-bar ok"`,
		`data-span-id="0011223344556677"`,
		`data-span-id="8899aabbccddeeff"`,
		// Root span covers the whole timeline.
		`style="left: 0.0000%; width: 100.0000%;"`,
		// Child span 'go test' starts 100ms in and lasts 1.8s of a 2s trace.
		`style="left: 5.0000%; width: 90.0000%;"`,
		`id="span-0011223344556677"`,
		`id="span-8899aabbccddeeff"`,
		`href="#span-0011223344556677"`,
	}
	for _, want := range wants {
		if !strings.Contains(s, want) {
			t.Errorf("rendered output missing %q", want)
		}
	}
}

func TestLayoutWaterfall(t *testing.T) {
	t0 := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	nodes := []*SpanNode{
		{Span: otelstore.Span{StartTime: t0, EndTime: t0.Add(4 * time.Second)}},
		{Span: otelstore.Span{StartTime: t0.Add(time.Second), EndTime: t0.Add(3 * time.Second)}},
		// Instant span — should still get a minimum-visible width.
		{Span: otelstore.Span{StartTime: t0.Add(2 * time.Second), EndTime: t0.Add(2 * time.Second)}},
	}
	layoutWaterfall(nodes)
	if nodes[0].OffsetPct != 0 || nodes[0].WidthPct != 100 {
		t.Errorf("root layout = %.2f / %.2f, want 0 / 100", nodes[0].OffsetPct, nodes[0].WidthPct)
	}
	if nodes[1].OffsetPct != 25 || nodes[1].WidthPct != 50 {
		t.Errorf("child layout = %.2f / %.2f, want 25 / 50", nodes[1].OffsetPct, nodes[1].WidthPct)
	}
	if nodes[2].WidthPct < 0.2 {
		t.Errorf("instant span got width %.4f, want >= hairline 0.2", nodes[2].WidthPct)
	}
}

func TestLayoutWaterfallZeroTotal(t *testing.T) {
	t0 := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	nodes := []*SpanNode{{Span: otelstore.Span{StartTime: t0, EndTime: t0}}}
	layoutWaterfall(nodes)
	if nodes[0].OffsetPct != 0 || nodes[0].WidthPct != 0 {
		t.Errorf("zero-total layout = %.2f / %.2f, want 0 / 0", nodes[0].OffsetPct, nodes[0].WidthPct)
	}
}

func TestRenderTraceHasControlsAndSeverity(t *testing.T) {
	tr := sampleTrace()
	tr.Logs = append(tr.Logs,
		otelstore.LogRecord{
			TraceID:      tr.TraceID,
			SpanID:       tr.Spans[1].SpanID,
			Time:         tr.Spans[1].StartTime.Add(600 * time.Millisecond),
			SeverityText: "WARN",
			Body:         "deprecated flag --foo",
		},
		otelstore.LogRecord{
			TraceID:      tr.TraceID,
			SpanID:       tr.Spans[1].SpanID,
			Time:         tr.Spans[1].StartTime.Add(700 * time.Millisecond),
			SeverityText: "ERROR",
			Body:         "panic: runtime error",
		},
	)
	got, err := RenderTrace(tr)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	wants := []string{
		`id="expand-all"`,
		`id="collapse-all"`,
		`id="log-search"`,
		`id="severity-filter"`,
		`class="log-line sev-info"`,
		`class="log-line sev-warn"`,
		`class="log-line sev-error"`,
		`data-severity="error"`,
	}
	for _, want := range wants {
		if !strings.Contains(s, want) {
			t.Errorf("rendered output missing %q", want)
		}
	}
}

func TestSeverityClass(t *testing.T) {
	cases := map[string]string{
		"ERROR":   "error",
		"error":   "error",
		"Fatal":   "error",
		"WARNING": "warn",
		"warn":    "warn",
		"INFO":    "info",
		"DEBUG":   "debug",
		"TRACE":   "trace",
		"":        "default",
		"weird":   "default",
	}
	for in, want := range cases {
		if got := severityClass(in); got != want {
			t.Errorf("severityClass(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRenderTraceWithoutRoot(t *testing.T) {
	t0 := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	tr := otelstore.Trace{
		TraceID: "abc",
		Spans: []otelstore.Span{{
			TraceID:      "abc",
			SpanID:       "child",
			ParentSpanID: "missing",
			Name:         "only-child",
			StartTime:    t0,
			EndTime:      t0.Add(time.Second),
		}},
	}
	got, err := RenderTrace(tr)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if !strings.Contains(s, "root span not yet exported") {
		t.Errorf("expected partial-trace notice, got: %s", s)
	}
	if !strings.Contains(s, "only-child") {
		t.Errorf("orphan child not rendered")
	}
}

func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (run with -update to regenerate)", path, err)
	}
	if string(got) != string(want) {
		t.Errorf("rendered output differs from %s; run with -update to refresh", path)
	}
}
