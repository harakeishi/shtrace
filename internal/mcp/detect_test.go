package mcp

import (
	"testing"

	"github.com/harakeishi/shtrace/internal/storage"
)

func ptr(n int) *int { return &n }

func TestDetectTestRuns_GoTest(t *testing.T) {
	det := detectors[4] // go test detector

	sp := storage.Span{
		ID:        "span1",
		SessionID: "sess1",
		Command:   "go",
		Argv:      []string{"go", "test", "./..."},
	}
	lines := []string{
		"--- PASS: TestFoo (0.00s)",
		"--- PASS: TestBar (0.01s)",
		"--- FAIL: TestBaz (0.02s)",
		"ok  \tgithub.com/foo/bar\t0.05s",
	}
	run := det.extract(lines, sp)

	if run.Framework != "go test" {
		t.Errorf("framework: got %q, want %q", run.Framework, "go test")
	}
	if run.Passed == nil || *run.Passed != 2 {
		t.Errorf("passed: got %v, want 2", run.Passed)
	}
	if run.Failed == nil || *run.Failed != 1 {
		t.Errorf("failed: got %v, want 1", run.Failed)
	}
	if run.Total == nil || *run.Total != 3 {
		t.Errorf("total: got %v, want 3", run.Total)
	}
}

func TestDetectTestRuns_GoTestNoTestArg(t *testing.T) {
	det := detectors[4] // go test detector

	sp := storage.Span{
		ID:        "span1",
		SessionID: "sess1",
		Command:   "go",
		Argv:      []string{"go", "build", "./..."},
	}
	run := det.extract(nil, sp)
	// Should signal no match (empty SpanID)
	if run.SpanID != "" {
		t.Errorf("expected empty SpanID for non-test go command, got %q", run.SpanID)
	}
}

func TestDetectTestRuns_Pytest(t *testing.T) {
	det := detectors[0] // pytest detector

	sp := storage.Span{
		ID:        "span2",
		SessionID: "sess1",
		Command:   "pytest",
		Argv:      []string{"pytest", "tests/"},
	}
	lines := []string{
		"collected 10 items",
		"",
		"tests/test_foo.py ...F......",
		"",
		"3 passed, 1 failed in 0.42s",
	}
	run := det.extract(lines, sp)

	if run.Framework != "pytest" {
		t.Errorf("framework: got %q", run.Framework)
	}
	if run.Passed == nil || *run.Passed != 3 {
		t.Errorf("passed: got %v, want 3", run.Passed)
	}
	if run.Failed == nil || *run.Failed != 1 {
		t.Errorf("failed: got %v, want 1", run.Failed)
	}
}

func TestDetectTestRuns_Rspec(t *testing.T) {
	det := detectors[5] // rspec detector

	sp := storage.Span{
		ID:        "span3",
		SessionID: "sess1",
		Command:   "rspec",
		Argv:      []string{"rspec", "spec/"},
	}
	lines := []string{
		"Finished in 0.05 seconds",
		"5 examples, 2 failures",
	}
	run := det.extract(lines, sp)

	if run.Framework != "rspec" {
		t.Errorf("framework: got %q", run.Framework)
	}
	if run.Total == nil || *run.Total != 5 {
		t.Errorf("total: got %v, want 5", run.Total)
	}
	if run.Failed == nil || *run.Failed != 2 {
		t.Errorf("failed: got %v, want 2", run.Failed)
	}
	if run.Passed == nil || *run.Passed != 3 {
		t.Errorf("passed: got %v, want 3 (5-2)", run.Passed)
	}
}

func TestRunStatus(t *testing.T) {
	cases := []struct {
		run  TestRun
		want string
	}{
		{TestRun{ExitCode: ptr(0)}, "pass"},
		{TestRun{ExitCode: ptr(1)}, "fail"},
		{TestRun{Failed: ptr(0), Passed: ptr(5)}, "pass"},
		{TestRun{Failed: ptr(2)}, "fail"},
		{TestRun{}, "unknown"},
	}
	for _, tc := range cases {
		got := runStatus(tc.run)
		if got != tc.want {
			t.Errorf("runStatus(%+v) = %q, want %q", tc.run, got, tc.want)
		}
	}
}

func TestChangeLabel(t *testing.T) {
	cases := []struct{ a, b, want string }{
		{"pass", "pass", "unchanged"},
		{"fail", "pass", "fail→pass"},
		{"pass", "fail", "pass→fail"},
	}
	for _, tc := range cases {
		got := changeLabel(tc.a, tc.b)
		if got != tc.want {
			t.Errorf("changeLabel(%q,%q) = %q, want %q", tc.a, tc.b, got, tc.want)
		}
	}
}
