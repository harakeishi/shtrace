package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"
	"strings"

	"github.com/harakeishi/shtrace/internal/mcp"
	"github.com/harakeishi/shtrace/internal/storage"
)

const maxDiffLines = 500
const diffContextLines = 3

// runDiff compares two sessions: test-run summary and span-level output diff.
//
// Usage:
//
//	shtrace diff <session_a> <session_b>
//	shtrace diff <session_a> <session_b> --json
func runDiff(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	jsonOut := false
	var ids []string
	for _, a := range args {
		if a == "--json" {
			jsonOut = true
		} else {
			ids = append(ids, a)
		}
	}
	if len(ids) != 2 {
		_, _ = fmt.Fprintln(stderr, "usage: shtrace diff <session_a> <session_b> [--json]")
		return 2
	}
	sessionA, sessionB := ids[0], ids[1]

	env := envMap()
	dataDir, err := storage.ResolveDataDir(env, runtime.GOOS)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: %v\n", err)
		return 1
	}
	store, err := storage.Open(dataDir + "/sessions.db")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: open store: %v\n", err)
		return 1
	}
	defer func() { _ = store.Close() }()
	if err := store.Migrate(ctx); err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: migrate: %v\n", err)
		return 1
	}

	warn := func(e error) { _, _ = fmt.Fprintf(stderr, "shtrace: warning: %v\n", e) }
	spansA, err := store.SpansForSession(ctx, sessionA, warn)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: spans for %s: %v\n", sessionA, err)
		return 1
	}
	spansB, err := store.SpansForSession(ctx, sessionB, warn)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: spans for %s: %v\n", sessionB, err)
		return 1
	}

	runsA := mcp.DetectTestRuns(spansA, dataDir)
	runsB := mcp.DetectTestRuns(spansB, dataDir)
	pairs, unmatchedA, unmatchedB := matchSpans(spansA, spansB)

	if jsonOut {
		return emitDiffJSON(stdout, stderr, sessionA, sessionB, runsA, runsB, pairs, unmatchedA, unmatchedB, dataDir)
	}
	return emitDiffText(stdout, sessionA, sessionB, runsA, runsB, pairs, unmatchedA, unmatchedB, dataDir)
}

// --- span matching -------------------------------------------------------

type spanPair struct{ a, b storage.Span }

// matchSpans pairs spans between two sessions by normalised command key.
// First-match wins when multiple spans share the same key.
func matchSpans(spansA, spansB []storage.Span) (pairs []spanPair, unmatchedA, unmatchedB []storage.Span) {
	byKey := make(map[string][]storage.Span, len(spansB))
	for _, sp := range spansB {
		k := diffSpanKey(sp)
		byKey[k] = append(byKey[k], sp)
	}
	usedB := make(map[string]bool, len(spansB))

	for _, spA := range spansA {
		k := diffSpanKey(spA)
		matched := false
		for _, spB := range byKey[k] {
			if !usedB[spB.ID] {
				usedB[spB.ID] = true
				pairs = append(pairs, spanPair{spA, spB})
				matched = true
				break
			}
		}
		if !matched {
			unmatchedA = append(unmatchedA, spA)
		}
	}
	for _, sp := range spansB {
		if !usedB[sp.ID] {
			unmatchedB = append(unmatchedB, sp)
		}
	}
	return
}

// diffSpanKey returns the first two argv tokens as the matching key so that
// "go test" and "go build" are not conflated. Falls back to one token or
// Command when argv is short or empty.
func diffSpanKey(sp storage.Span) string {
	switch len(sp.Argv) {
	case 0:
		return sp.Command
	case 1:
		return sp.Argv[0]
	default:
		return sp.Argv[0] + " " + sp.Argv[1]
	}
}

// --- text output ---------------------------------------------------------

func emitDiffText(w io.Writer, sessionA, sessionB string, runsA, runsB []mcp.TestRun, pairs []spanPair, unmatchedA, unmatchedB []storage.Span, dataDir string) int {
	_, _ = fmt.Fprintf(w, "session A: %s\n", sessionA)
	_, _ = fmt.Fprintf(w, "session B: %s\n", sessionB)
	_, _ = fmt.Fprintln(w)

	// Test-run summary
	entries := compareTestRuns(runsA, runsB)
	if len(entries) > 0 {
		_, _ = fmt.Fprintln(w, "=== test run comparison ===")
		for _, e := range entries {
			mark := " "
			if e.change != "unchanged" {
				mark = "!"
			}
			_, _ = fmt.Fprintf(w, "%s [%s] %s  A:%s → B:%s\n", mark, e.framework, e.command, e.statusA, e.statusB)
		}
		_, _ = fmt.Fprintln(w)
	}

	// Span-level output diff
	_, _ = fmt.Fprintln(w, "=== span output diff ===")
	hasDiff := false
	for _, p := range pairs {
		diff := spanDiffText(dataDir, p)
		if diff == "" {
			continue
		}
		hasDiff = true
		argv := strings.Join(p.a.Argv, " ")
		if argv == "" {
			argv = p.a.Command
		}
		_, _ = fmt.Fprintf(w, "--- a/%s  exit=%s\n", argv, exitCodeStr(p.a.ExitCode))
		_, _ = fmt.Fprintf(w, "+++ b/%s  exit=%s\n", argv, exitCodeStr(p.b.ExitCode))
		_, _ = fmt.Fprint(w, diff)
		_, _ = fmt.Fprintln(w)
	}

	for _, sp := range unmatchedA {
		argv := strings.Join(sp.Argv, " ")
		if argv == "" {
			argv = sp.Command
		}
		hasDiff = true
		_, _ = fmt.Fprintf(w, "removed: %s  exit=%s\n", argv, exitCodeStr(sp.ExitCode))
	}
	for _, sp := range unmatchedB {
		argv := strings.Join(sp.Argv, " ")
		if argv == "" {
			argv = sp.Command
		}
		hasDiff = true
		_, _ = fmt.Fprintf(w, "added:   %s  exit=%s\n", argv, exitCodeStr(sp.ExitCode))
	}

	if !hasDiff {
		_, _ = fmt.Fprintln(w, "no differences")
	}
	return 0
}

// --- JSON output ---------------------------------------------------------

type diffJSONOutput struct {
	SessionA    string           `json:"session_a"`
	SessionB    string           `json:"session_b"`
	TestRuns    []testRunDiff    `json:"test_runs"`
	SpanOutputs []spanOutputDiff `json:"span_outputs"`
}

type testRunDiff struct {
	Framework string `json:"framework"`
	Command   string `json:"command"`
	StatusA   string `json:"status_a"`
	StatusB   string `json:"status_b"`
	Change    string `json:"change"`
}

type spanOutputDiff struct {
	Command  string `json:"command"`
	ExitA    *int   `json:"exit_a"`
	ExitB    *int   `json:"exit_b"`
	Diff     string `json:"diff"`
	OnlyIn   string `json:"only_in,omitempty"` // "a" or "b" for unmatched spans
}

func emitDiffJSON(w, stderr io.Writer, sessionA, sessionB string, runsA, runsB []mcp.TestRun, pairs []spanPair, unmatchedA, unmatchedB []storage.Span, dataDir string) int {
	out := diffJSONOutput{
		SessionA: sessionA,
		SessionB: sessionB,
	}

	for _, e := range compareTestRuns(runsA, runsB) {
		out.TestRuns = append(out.TestRuns, testRunDiff{
			Framework: e.framework,
			Command:   e.command,
			StatusA:   e.statusA,
			StatusB:   e.statusB,
			Change:    e.change,
		})
	}
	if out.TestRuns == nil {
		out.TestRuns = []testRunDiff{}
	}

	for _, p := range pairs {
		argv := strings.Join(p.a.Argv, " ")
		if argv == "" {
			argv = p.a.Command
		}
		out.SpanOutputs = append(out.SpanOutputs, spanOutputDiff{
			Command: argv,
			ExitA:   p.a.ExitCode,
			ExitB:   p.b.ExitCode,
			Diff:    spanDiffText(dataDir, p),
		})
	}
	for _, sp := range unmatchedA {
		argv := strings.Join(sp.Argv, " ")
		if argv == "" {
			argv = sp.Command
		}
		out.SpanOutputs = append(out.SpanOutputs, spanOutputDiff{Command: argv, ExitA: sp.ExitCode, OnlyIn: "a"})
	}
	for _, sp := range unmatchedB {
		argv := strings.Join(sp.Argv, " ")
		if argv == "" {
			argv = sp.Command
		}
		out.SpanOutputs = append(out.SpanOutputs, spanOutputDiff{Command: argv, ExitB: sp.ExitCode, OnlyIn: "b"})
	}
	if out.SpanOutputs == nil {
		out.SpanOutputs = []spanOutputDiff{}
	}

	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: marshal json: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintln(w, string(b))
	return 0
}

// --- test run comparison -------------------------------------------------

type testRunEntry struct {
	framework, command, statusA, statusB, change string
}

func compareTestRuns(runsA, runsB []mcp.TestRun) []testRunEntry {
	type key struct{ framework, command string }
	mapA := make(map[key]mcp.TestRun, len(runsA))
	for _, r := range runsA {
		mapA[key{r.Framework, r.Command}] = r
	}
	mapB := make(map[key]mcp.TestRun, len(runsB))
	for _, r := range runsB {
		mapB[key{r.Framework, r.Command}] = r
	}

	seen := make(map[key]bool)
	var out []testRunEntry
	for k, rA := range mapA {
		seen[k] = true
		sa := testStatus(rA)
		if rB, ok := mapB[k]; ok {
			sb := testStatus(rB)
			out = append(out, testRunEntry{k.framework, k.command, sa, sb, changeLabel(sa, sb)})
		} else {
			out = append(out, testRunEntry{k.framework, k.command, sa, "absent", "removed"})
		}
	}
	for k, rB := range mapB {
		if seen[k] {
			continue
		}
		sb := testStatus(rB)
		out = append(out, testRunEntry{k.framework, k.command, "absent", sb, "added"})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].framework != out[j].framework {
			return out[i].framework < out[j].framework
		}
		return out[i].command < out[j].command
	})
	return out
}

func testStatus(r mcp.TestRun) string {
	if r.ExitCode != nil {
		if *r.ExitCode == 0 {
			return "pass"
		}
		return "fail"
	}
	if r.Failed != nil && *r.Failed > 0 {
		return "fail"
	}
	if r.Failed != nil || r.Passed != nil {
		return "pass"
	}
	return "unknown"
}

func changeLabel(a, b string) string {
	if a == b {
		return "unchanged"
	}
	return a + "→" + b
}

// --- unified diff --------------------------------------------------------

// spanDiffText returns the unified diff of the text outputs of two spans.
// Returns "" when outputs are identical or cannot be read.
func spanDiffText(dataDir string, p spanPair) string {
	textA := readSpanText(dataDir, p.a.SessionID, p.a.ID)
	textB := readSpanText(dataDir, p.b.SessionID, p.b.ID)
	linesA := splitToLines(textA)
	linesB := splitToLines(textB)
	ops := computeEdits(linesA, linesB)
	return renderUnifiedDiff(ops)
}

// readSpanText concatenates all chunk data from a span's JSONL log.
func readSpanText(dataDir, sessionID, spanID string) string {
	p := storage.OutputPath(dataDir, sessionID, spanID)
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()

	var sb strings.Builder
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		var c storage.Chunk
		if json.Unmarshal(sc.Bytes(), &c) == nil {
			sb.WriteString(c.Data)
		}
	}
	// sc.Err() is non-nil when a line exceeds the 1 MiB buffer limit or an I/O
	// error occurs. Return whatever was collected (best-effort); the diff output
	// will be partial but still useful, and the caller has no warn hook available.
	_ = sc.Err()
	return sb.String()
}

// splitToLines splits text into lines and caps at maxDiffLines (tail).
func splitToLines(text string) []string {
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > maxDiffLines {
		lines = lines[len(lines)-maxDiffLines:]
	}
	return lines
}

type editKind uint8

const (
	editEqual  editKind = iota
	editDelete          // line present in A, absent in B
	editInsert          // line present in B, absent in A
)

type editOp struct {
	kind editKind
	line string
}

// computeEdits returns the edit script (LCS-based) transforming a into b.
func computeEdits(a, b []string) []editOp {
	m, n := len(a), len(b)

	// Build LCS table
	dp := make([][]int, m+1)
	for i := range dp {
		dp[i] = make([]int, n+1)
	}
	for i := 1; i <= m; i++ {
		for j := 1; j <= n; j++ {
			if a[i-1] == b[j-1] {
				dp[i][j] = dp[i-1][j-1] + 1
			} else if dp[i-1][j] >= dp[i][j-1] {
				dp[i][j] = dp[i-1][j]
			} else {
				dp[i][j] = dp[i][j-1]
			}
		}
	}

	// Traceback
	ops := make([]editOp, 0, m+n)
	i, j := m, n
	for i > 0 || j > 0 {
		switch {
		case i > 0 && j > 0 && a[i-1] == b[j-1]:
			ops = append(ops, editOp{editEqual, a[i-1]})
			i--
			j--
		case j > 0 && (i == 0 || dp[i][j-1] >= dp[i-1][j]):
			ops = append(ops, editOp{editInsert, b[j-1]})
			j--
		default:
			ops = append(ops, editOp{editDelete, a[i-1]})
			i--
		}
	}

	// Reverse (built backwards during traceback)
	for lo, hi := 0, len(ops)-1; lo < hi; lo, hi = lo+1, hi-1 {
		ops[lo], ops[hi] = ops[hi], ops[lo]
	}
	return ops
}

// renderUnifiedDiff formats the edit script as a unified diff body (no --- +++ header).
// Returns "" when there are no changes.
func renderUnifiedDiff(ops []editOp) string {
	hasDiff := false
	for _, op := range ops {
		if op.kind != editEqual {
			hasDiff = true
			break
		}
	}
	if !hasDiff {
		return ""
	}

	// Mark which op indices should appear in a hunk (changed ± context)
	inHunk := make([]bool, len(ops))
	for i, op := range ops {
		if op.kind == editEqual {
			continue
		}
		lo := max(i-diffContextLines, 0)
		hi := min(i+diffContextLines+1, len(ops))
		for j := lo; j < hi; j++ {
			inHunk[j] = true
		}
	}

	var sb strings.Builder
	i := 0
	for i < len(ops) {
		if !inHunk[i] {
			i++
			continue
		}

		// Find end of contiguous hunk region
		end := i
		for end < len(ops) && inHunk[end] {
			end++
		}

		// Count line numbers up to hunk start
		aLine, bLine := 1, 1
		for j := 0; j < i; j++ {
			if ops[j].kind != editInsert {
				aLine++
			}
			if ops[j].kind != editDelete {
				bLine++
			}
		}

		aCount, bCount := 0, 0
		for j := i; j < end; j++ {
			if ops[j].kind != editInsert {
				aCount++
			}
			if ops[j].kind != editDelete {
				bCount++
			}
		}

		_, _ = fmt.Fprintf(&sb, "@@ -%d,%d +%d,%d @@\n", aLine, aCount, bLine, bCount)
		for j := i; j < end; j++ {
			switch ops[j].kind {
			case editEqual:
				_, _ = fmt.Fprintf(&sb, " %s\n", ops[j].line)
			case editDelete:
				_, _ = fmt.Fprintf(&sb, "-%s\n", ops[j].line)
			case editInsert:
				_, _ = fmt.Fprintf(&sb, "+%s\n", ops[j].line)
			}
		}
		i = end
	}
	return sb.String()
}

func exitCodeStr(code *int) string {
	if code == nil {
		return "?"
	}
	return fmt.Sprintf("%d", *code)
}
