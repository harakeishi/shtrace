package mcp

import (
	"testing"

	"github.com/harakeishi/shtrace/internal/storage"
)

func ptr(n int) *int { return &n }

// findDetector returns the detector with the given framework name.
// Fails the test immediately if not found, so callers don't need to guard
// against a nil/zero value — the test index never silently shifts.
func findDetector(t *testing.T, name string) frameworkDetector {
	t.Helper()
	for _, d := range detectors {
		if d.name == name {
			return d
		}
	}
	t.Fatalf("detector %q not found in detectors slice", name)
	return frameworkDetector{}
}

func TestDetectTestRuns_GoTest(t *testing.T) {
	det := findDetector(t, "go test")

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
	det := findDetector(t, "go test")

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
	det := findDetector(t, "pytest")

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

func TestDetectTestRuns_PytestWithSkipped(t *testing.T) {
	det := findDetector(t, "pytest")

	sp := storage.Span{
		ID:        "span3",
		SessionID: "sess1",
		Command:   "pytest",
		Argv:      []string{"pytest", "tests/"},
	}
	lines := []string{
		"5 passed, 2 skipped in 0.10s",
	}
	run := det.extract(lines, sp)

	if run.Passed == nil || *run.Passed != 5 {
		t.Errorf("passed: got %v, want 5", run.Passed)
	}
	if run.Skipped == nil || *run.Skipped != 2 {
		t.Errorf("skipped: got %v, want 2", run.Skipped)
	}
}

func TestDetectTestRuns_PytestFailFirst(t *testing.T) {
	// pytest outputs "N failed, N passed in Zs" when failures exist.
	det := findDetector(t, "pytest")

	sp := storage.Span{
		ID:        "span5",
		SessionID: "sess1",
		Command:   "pytest",
		Argv:      []string{"pytest", "tests/"},
	}
	lines := []string{
		"1 failed, 3 passed in 0.50s",
	}
	run := det.extract(lines, sp)

	if run.Passed == nil || *run.Passed != 3 {
		t.Errorf("passed: got %v, want 3", run.Passed)
	}
	if run.Failed == nil || *run.Failed != 1 {
		t.Errorf("failed: got %v, want 1", run.Failed)
	}
}

func TestDetectTestRuns_Jest(t *testing.T) {
	det := findDetector(t, "jest")

	sp := storage.Span{
		ID:        "span-jest",
		SessionID: "sess1",
		Command:   "jest",
		Argv:      []string{"jest"},
	}
	lines := []string{
		"Tests: 1 failed, 2 skipped, 5 passed, 8 total",
	}
	run := det.extract(lines, sp)

	if run.Framework != "jest" {
		t.Errorf("framework: got %q", run.Framework)
	}
	if run.Passed == nil || *run.Passed != 5 {
		t.Errorf("passed: got %v, want 5", run.Passed)
	}
	if run.Failed == nil || *run.Failed != 1 {
		t.Errorf("failed: got %v, want 1", run.Failed)
	}
	if run.Skipped == nil || *run.Skipped != 2 {
		t.Errorf("skipped: got %v, want 2", run.Skipped)
	}
	if run.Total == nil || *run.Total != 8 {
		t.Errorf("total: got %v, want 8", run.Total)
	}
}

func TestDetectTestRuns_JestWithTodo(t *testing.T) {
	// Jest v29+ includes a "todo" count in the summary line.
	det := findDetector(t, "jest")

	sp := storage.Span{
		ID:        "span-jest-todo",
		SessionID: "sess1",
		Command:   "jest",
		Argv:      []string{"jest"},
	}
	lines := []string{
		"Tests: 3 todo, 5 passed, 8 total",
	}
	run := det.extract(lines, sp)

	if run.Passed == nil || *run.Passed != 5 {
		t.Errorf("passed: got %v, want 5", run.Passed)
	}
	if run.Total == nil || *run.Total != 8 {
		t.Errorf("total: got %v, want 8", run.Total)
	}
}

func TestDetectTestRuns_Vitest(t *testing.T) {
	det := findDetector(t, "vitest")

	sp := storage.Span{
		ID:        "span-vitest",
		SessionID: "sess1",
		Command:   "vitest",
		Argv:      []string{"vitest", "run"},
	}
	lines := []string{
		"Tests  4 passed | 1 failed (5)",
	}
	run := det.extract(lines, sp)

	if run.Framework != "vitest" {
		t.Errorf("framework: got %q", run.Framework)
	}
	if run.Passed == nil || *run.Passed != 4 {
		t.Errorf("passed: got %v, want 4", run.Passed)
	}
	if run.Failed == nil || *run.Failed != 1 {
		t.Errorf("failed: got %v, want 1", run.Failed)
	}
	if run.Total == nil || *run.Total != 5 {
		t.Errorf("total: got %v, want 5", run.Total)
	}
}

func TestDetectTestRuns_PHPUnitOK(t *testing.T) {
	det := findDetector(t, "phpunit")

	sp := storage.Span{
		ID:        "span-phpunit-ok",
		SessionID: "sess1",
		Command:   "phpunit",
		Argv:      []string{"phpunit", "--testdox"},
	}
	lines := []string{
		"OK (7 tests, 14 assertions)",
	}
	run := det.extract(lines, sp)

	if run.Framework != "phpunit" {
		t.Errorf("framework: got %q", run.Framework)
	}
	if run.Total == nil || *run.Total != 7 {
		t.Errorf("total: got %v, want 7", run.Total)
	}
	if run.Passed == nil || *run.Passed != 7 {
		t.Errorf("passed: got %v, want 7", run.Passed)
	}
}

func TestDetectTestRuns_PHPUnitFail(t *testing.T) {
	det := findDetector(t, "phpunit")

	sp := storage.Span{
		ID:        "span-phpunit-fail",
		SessionID: "sess1",
		Command:   "phpunit",
		Argv:      []string{"phpunit"},
	}
	lines := []string{
		"Tests: 5, Assertions: 10, Failures: 2.",
	}
	run := det.extract(lines, sp)

	if run.Total == nil || *run.Total != 5 {
		t.Errorf("total: got %v, want 5", run.Total)
	}
	if run.Failed == nil || *run.Failed != 2 {
		t.Errorf("failed: got %v, want 2", run.Failed)
	}
	if run.Passed == nil || *run.Passed != 3 {
		t.Errorf("passed: got %v, want 3", run.Passed)
	}
}

func TestDetectTestRuns_NegativePassedClamped(t *testing.T) {
	// Guard: passed = total - failed must not go negative on corrupt output.
	det := findDetector(t, "rspec")

	sp := storage.Span{
		ID:        "span-rspec-corrupt",
		SessionID: "sess1",
		Command:   "rspec",
		Argv:      []string{"rspec"},
	}
	lines := []string{
		"0 examples, 5 failures", // corrupt: more failures than examples
	}
	run := det.extract(lines, sp)

	if run.Passed != nil && *run.Passed < 0 {
		t.Errorf("passed must not be negative, got %d", *run.Passed)
	}
}

func TestDetectTestRuns_Rspec(t *testing.T) {
	det := findDetector(t, "rspec")

	sp := storage.Span{
		ID:        "span4",
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
		{TestRun{Failed: ptr(0)}, "pass"}, // Failed=0, Passed=nil → still "pass"
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

func TestNormaliseCommand(t *testing.T) {
	cases := []struct {
		framework, cmd, want string
	}{
		{"go test", "go test ./...", "go test"},
		{"go test", "go test ./pkg/a", "go test"},
		{"pytest", "pytest tests/", "pytest"},
		{"rspec", "rspec spec/", "rspec"},
		{"go test", "go", "go"},        // single token fallback
		{"pytest", "", ""},             // empty command
	}
	for _, tc := range cases {
		got := normaliseCommand(tc.framework, tc.cmd)
		if got != tc.want {
			t.Errorf("normaliseCommand(%q,%q) = %q, want %q", tc.framework, tc.cmd, got, tc.want)
		}
	}
}

func TestSafeOutputPath_TraversalRejected(t *testing.T) {
	_, err := safeOutputPath("/data", "../../etc", "id")
	if err == nil {
		t.Error("expected error for path traversal, got nil")
	}
}

func TestSafeOutputPath_ValidPath(t *testing.T) {
	p, err := safeOutputPath("/data", "sess123", "span456")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if p == "" {
		t.Error("expected non-empty path")
	}
}
