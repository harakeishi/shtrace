package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/harakeishi/shtrace/internal/storage"
)

func TestComputeEdits_NoChange(t *testing.T) {
	lines := []string{"a", "b", "c"}
	ops := computeEdits(lines, lines)
	for _, op := range ops {
		if op.kind != editEqual {
			t.Fatalf("expected all equal ops, got kind=%d line=%q", op.kind, op.line)
		}
	}
}

func TestComputeEdits_AllInserted(t *testing.T) {
	ops := computeEdits(nil, []string{"x", "y"})
	for _, op := range ops {
		if op.kind != editInsert {
			t.Fatalf("expected all insert ops, got kind=%d", op.kind)
		}
	}
}

func TestComputeEdits_AllDeleted(t *testing.T) {
	ops := computeEdits([]string{"x", "y"}, nil)
	for _, op := range ops {
		if op.kind != editDelete {
			t.Fatalf("expected all delete ops, got kind=%d", op.kind)
		}
	}
}

func TestComputeEdits_OneLineChanged(t *testing.T) {
	a := []string{"line1", "old", "line3"}
	b := []string{"line1", "new", "line3"}
	ops := computeEdits(a, b)

	var del, ins, eq int
	for _, op := range ops {
		switch op.kind {
		case editDelete:
			del++
			if op.line != "old" {
				t.Fatalf("deleted unexpected line %q", op.line)
			}
		case editInsert:
			ins++
			if op.line != "new" {
				t.Fatalf("inserted unexpected line %q", op.line)
			}
		case editEqual:
			eq++
		}
	}
	if del != 1 || ins != 1 || eq != 2 {
		t.Fatalf("del=%d ins=%d eq=%d, want 1 1 2", del, ins, eq)
	}
}

func TestRenderUnifiedDiff_NoDiff(t *testing.T) {
	lines := []string{"a", "b"}
	ops := computeEdits(lines, lines)
	if got := renderUnifiedDiff(ops); got != "" {
		t.Fatalf("expected empty diff for identical inputs, got %q", got)
	}
}

func TestRenderUnifiedDiff_HasHunkHeader(t *testing.T) {
	a := []string{"a", "b", "old", "d", "e"}
	b := []string{"a", "b", "new", "d", "e"}
	ops := computeEdits(a, b)
	diff := renderUnifiedDiff(ops)
	if !strings.Contains(diff, "@@") {
		t.Fatalf("expected hunk header in diff, got:\n%s", diff)
	}
	if !strings.Contains(diff, "-old") {
		t.Fatalf("expected -old in diff, got:\n%s", diff)
	}
	if !strings.Contains(diff, "+new") {
		t.Fatalf("expected +new in diff, got:\n%s", diff)
	}
}

func TestSplitToLines_CapsAtMax(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < maxDiffLines+10; i++ {
		sb.WriteString("line\n")
	}
	lines := splitToLines(sb.String())
	if len(lines) != maxDiffLines {
		t.Fatalf("expected %d lines, got %d", maxDiffLines, len(lines))
	}
}

func TestMatchSpans_BasicPairing(t *testing.T) {
	a := makeSpan("s1", "sp1", []string{"go", "test", "./..."})
	b := makeSpan("s2", "sp2", []string{"go", "test", "./..."})
	c := makeSpan("s2", "sp3", []string{"go", "build", "./..."})

	pairs, unmatchedA, unmatchedB := matchSpans([]storage.Span{a}, []storage.Span{b, c})
	if len(pairs) != 1 {
		t.Fatalf("expected 1 pair, got %d", len(pairs))
	}
	if pairs[0].a.ID != "sp1" || pairs[0].b.ID != "sp2" {
		t.Fatalf("unexpected pair: a=%s b=%s", pairs[0].a.ID, pairs[0].b.ID)
	}
	if len(unmatchedA) != 0 {
		t.Fatalf("expected 0 unmatchedA, got %d", len(unmatchedA))
	}
	if len(unmatchedB) != 1 || unmatchedB[0].ID != "sp3" {
		t.Fatalf("expected sp3 in unmatchedB, got %v", unmatchedB)
	}
}

func TestRunDiff_MissingArgs(t *testing.T) {
	_, stderr, exit, _ := runHarness(t, "shtrace", "diff")
	if exit != 2 {
		t.Fatalf("exit = %d, want 2", exit)
	}
	if !strings.Contains(stderr, "usage:") {
		t.Fatalf("expected usage in stderr, got %q", stderr)
	}
}

func TestRunDiff_TwoSessions(t *testing.T) {
	// Both sessions must share the same data dir. runHarness creates a fresh
	// temp dir on each call, so we drive Run directly here.
	dataDir := t.TempDir()
	t.Setenv("SHTRACE_DATA_DIR", dataDir)
	t.Setenv("SHTRACE_SESSION_ID", "")
	t.Setenv("SHTRACE_PARENT_SPAN_ID", "")
	t.Setenv("SHTRACE_TAGS", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GITHUB_ACTIONS", "")
	t.Setenv("GITHUB_WORKSPACE", "")

	run := func(args ...string) (string, string, int) {
		var so, se bytes.Buffer
		exit := Run(context.Background(), args, &so, &se)
		return so.String(), se.String(), exit
	}

	if _, _, exit := run("shtrace", "--", "sh", "-c", "printf hello"); exit != 0 {
		t.Fatalf("session A exit=%d", exit)
	}
	if _, _, exit := run("shtrace", "--", "sh", "-c", "printf world"); exit != 0 {
		t.Fatalf("session B exit=%d", exit)
	}

	lsOut, _, exit := run("shtrace", "ls", "--json")
	if exit != 0 {
		t.Fatalf("ls exit=%d", exit)
	}
	type lsEntry struct {
		ID string `json:"id"`
	}
	var sessions []lsEntry
	if err := json.Unmarshal([]byte(strings.TrimSpace(lsOut)), &sessions); err != nil || len(sessions) < 2 {
		t.Fatalf("expected ≥2 sessions, got %q err=%v", lsOut, err)
	}
	idA := sessions[len(sessions)-2].ID
	idB := sessions[len(sessions)-1].ID

	// Text diff: exit 0 and shows the span diff section
	diffOut, _, exit := run("shtrace", "diff", idA, idB)
	if exit != 0 {
		t.Fatalf("diff exit=%d", exit)
	}
	if !strings.Contains(diffOut, "=== span output diff ===") {
		t.Fatalf("expected span diff section, got:\n%s", diffOut)
	}

	// JSON diff: exit 0 and produces valid JSON with correct session IDs
	jsonOut, _, exit := run("shtrace", "diff", "--json", idA, idB)
	if exit != 0 {
		t.Fatalf("diff --json exit=%d", exit)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(jsonOut)), &parsed); err != nil {
		t.Fatalf("diff --json not valid JSON: %v\n%s", err, jsonOut)
	}
	if parsed["session_a"] != idA || parsed["session_b"] != idB {
		t.Fatalf("session IDs mismatch: %v", parsed)
	}
}

func makeSpan(sessionID, spanID string, argv []string) storage.Span {
	return storage.Span{
		ID:        spanID,
		SessionID: sessionID,
		Command:   argv[0],
		Argv:      argv,
	}
}

func TestCompareTestRuns_Unchanged(t *testing.T) {
	entries := compareTestRuns(nil, nil)
	if len(entries) != 0 {
		t.Fatalf("expected 0 entries for nil inputs, got %d", len(entries))
	}
}

func TestChangeLabel(t *testing.T) {
	if changeLabel("pass", "pass") != "unchanged" {
		t.Fatal("same status should be unchanged")
	}
	if changeLabel("pass", "fail") != "pass→fail" {
		t.Fatal("changed status should show transition")
	}
}
