package main

import (
	"strings"
	"testing"
)

func TestRound2(t *testing.T) {
	cases := map[float64]float64{
		0.615:  0.62,
		0.614:  0.61,
		1.0:    1.0,
		-0.615: -0.62,
		142.44: 142.44,
	}
	for in, want := range cases {
		if got := round2(in); got != want {
			t.Errorf("round2(%v) = %v, want %v", in, got, want)
		}
	}
}

func TestEnvOr(t *testing.T) {
	t.Setenv("OTELHOUSEVIEW_TEST_KEY", "")
	if got := envOr("OTELHOUSEVIEW_TEST_KEY", "fallback"); got != "fallback" {
		t.Errorf("empty env should fall back, got %q", got)
	}
	t.Setenv("OTELHOUSEVIEW_TEST_KEY", "value")
	if got := envOr("OTELHOUSEVIEW_TEST_KEY", "fallback"); got != "value" {
		t.Errorf("set env should win, got %q", got)
	}
}

func fullReport() *report {
	return &report{
		Summary:   summary{Logs: 10, Spans: 6, Traces: 2, MetricPoints: 12, ErrorLogs: 1},
		LogVolume: []logBucket{{T: "t", Severity: "INFO", Count: 10}},
		Metrics:   []metric{{Name: "otelhouseview_x", Points: []metricPoint{{T: "t", Value: 1}}}},
		Traces:    []traceRow{{TraceID: "abc", Service: "s"}},
	}
}

func TestAssertSeededPassesOnFullReport(t *testing.T) {
	if err := assertSeeded(fullReport()); err != nil {
		t.Fatalf("full report should pass, got %v", err)
	}
}

func TestAssertSeededFailsOnEmptySignals(t *testing.T) {
	cases := map[string]func(*report){
		"no logs":         func(r *report) { r.Summary.Logs = 0 },
		"no spans":        func(r *report) { r.Summary.Spans = 0 },
		"no traces":       func(r *report) { r.Summary.Traces = 0 },
		"no metricPoints": func(r *report) { r.Summary.MetricPoints = 0 },
		"no metrics[]":    func(r *report) { r.Metrics = nil },
		"no traces[]":     func(r *report) { r.Traces = nil },
		"no logVolume[]":  func(r *report) { r.LogVolume = nil },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := fullReport()
			mutate(r)
			if err := assertSeeded(r); err == nil {
				t.Fatalf("%s: expected an assertion failure, got nil", name)
			}
		})
	}
}

// TestAssertSeededReportsAllEmptyAtOnce: a completely empty report should name
// every missing signal in one error, not just the first.
func TestAssertSeededReportsAllEmptyAtOnce(t *testing.T) {
	err := assertSeeded(&report{})
	if err == nil {
		t.Fatal("empty report must fail")
	}
	for _, want := range []string{"logs", "spans", "traces", "metricPoints", "metrics[]", "traces[]", "logVolume[]"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing mention of %q", err.Error(), want)
		}
	}
}
