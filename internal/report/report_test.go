package report

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/harakeishi/shtrace/internal/storage"
)

// fakeSource is an in-memory Source so tests don't need a real SQLite db.
type fakeSource struct {
	sessions []storage.Session
	spans    map[string][]storage.Span
	// corruptIDs, if non-nil, returns ErrSessionCorrupt for those ids so
	// the corrupt-row path can be exercised without injecting bad data.
	corruptIDs map[string]bool
	// spansErr, if non-nil, replaces the SpansForSession return so tests
	// can exercise the render-failure path.
	spansErr error
}

func (f *fakeSource) GetSession(_ context.Context, id string) (storage.Session, error) {
	if f.corruptIDs[id] {
		return storage.Session{}, fmt.Errorf("%w: synthetic", storage.ErrSessionCorrupt)
	}
	for _, s := range f.sessions {
		if s.ID == id {
			return s, nil
		}
	}
	return storage.Session{}, storage.ErrSessionNotFound
}

func (f *fakeSource) ListSessions(_ context.Context, limit int, _ func(error)) ([]storage.Session, error) {
	if limit <= 0 || limit > len(f.sessions) {
		limit = len(f.sessions)
	}
	out := make([]storage.Session, limit)
	copy(out, f.sessions[:limit])
	return out, nil
}

func (f *fakeSource) SpansForSession(_ context.Context, id string, _ func(error)) ([]storage.Span, error) {
	if f.spansErr != nil {
		return nil, f.spansErr
	}
	return f.spans[id], nil
}

// writeJSONL writes a minimal log file at outputs/<session>/<span>.log so
// readChunks can find it during Render.
func writeJSONL(t *testing.T, dataDir, sessionID, spanID string, lines []string) {
	t.Helper()
	dir := filepath.Join(dataDir, "outputs", sessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir outputs: %v", err)
	}
	path := filepath.Join(dir, spanID+".log")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}
}

func intPtr(v int) *int { return &v }

func TestRender_IncludesSessionMetadata(t *testing.T) {
	dataDir := t.TempDir()
	started := time.Date(2026, 5, 13, 10, 0, 0, 0, time.UTC)
	ended := started.Add(5 * time.Second)
	src := &fakeSource{
		sessions: []storage.Session{{
			ID:        "sess-1",
			StartedAt: started,
			EndedAt:   &ended,
			Tags:      map[string]string{"env": "ci"},
		}},
		spans: map[string][]storage.Span{
			"sess-1": {{
				ID:        "span-a",
				SessionID: "sess-1",
				Command:   "echo",
				Argv:      []string{"echo", "hi"},
				Mode:      "pipe",
				StartedAt: started,
				EndedAt:   started.Add(50 * time.Millisecond),
				ExitCode:  intPtr(0),
			}},
		},
	}
	writeJSONL(t, dataDir, "sess-1", "span-a",
		[]string{`{"ts":"2026-05-13T10:00:00Z","stream":"stdout","data":"hi\n"}`})

	var buf bytes.Buffer
	got, err := Render(context.Background(), src, &buf, Options{
		SessionID: "sess-1",
		DataDir:   dataDir,
	})
	if err != nil {
		t.Fatalf("Render error: %v", err)
	}
	if got != "sess-1" {
		t.Fatalf("Render returned session id %q, want sess-1", got)
	}
	out := buf.String()
	for _, want := range []string{
		"sess-1", "echo", "exit 0", "mode=pipe",
		"hi\n", "env=ci",
		`<!DOCTYPE html>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered HTML missing %q\n--- output ---\n%s", want, out)
		}
	}
}

func TestRender_LatestPicksMostRecent(t *testing.T) {
	dataDir := t.TempDir()
	t0 := time.Date(2026, 5, 13, 10, 0, 0, 0, time.UTC)
	// fakeSource.ListSessions returns its slice in order — the contract is
	// "newest-first", so put newer first.
	src := &fakeSource{
		sessions: []storage.Session{
			{ID: "newer", StartedAt: t0.Add(time.Hour)},
			{ID: "older", StartedAt: t0},
		},
		spans: map[string][]storage.Span{
			"newer": {{ID: "s1", SessionID: "newer", Command: "newcmd", Mode: "pipe", StartedAt: t0.Add(time.Hour), EndedAt: t0.Add(time.Hour + time.Second), ExitCode: intPtr(0)}},
			"older": {{ID: "s2", SessionID: "older", Command: "oldcmd", Mode: "pipe", StartedAt: t0, EndedAt: t0.Add(time.Second), ExitCode: intPtr(0)}},
		},
	}
	var buf bytes.Buffer
	got, err := Render(context.Background(), src, &buf, Options{Latest: true, DataDir: dataDir})
	if err != nil {
		t.Fatalf("Render error: %v", err)
	}
	if got != "newer" {
		t.Fatalf("Render with Latest selected %q, want %q", got, "newer")
	}
	if strings.Contains(buf.String(), "oldcmd") {
		t.Errorf("Render output should not include the older session's command")
	}
}

// TestRender_EscapesHTMLInRecordedOutput is the security-critical case: a
// command can have printed `<script>alert(1)</script>` and that text is sitting
// verbatim in the JSONL log. The HTML report must NOT render it as a live
// element. html/template handles this automatically — this test guards against
// a future refactor that swaps to text/template.
func TestRender_EscapesHTMLInRecordedOutput(t *testing.T) {
	dataDir := t.TempDir()
	now := time.Now().UTC()
	src := &fakeSource{
		sessions: []storage.Session{{ID: "xss", StartedAt: now}},
		spans: map[string][]storage.Span{
			"xss": {{ID: "sp", SessionID: "xss", Command: "sh", Mode: "pipe", StartedAt: now, EndedAt: now, ExitCode: intPtr(0)}},
		},
	}
	writeJSONL(t, dataDir, "xss", "sp",
		[]string{`{"ts":"2026-05-13T10:00:00Z","stream":"stdout","data":"<script>alert(1)</script>"}`})

	var buf bytes.Buffer
	if _, err := Render(context.Background(), src, &buf, Options{SessionID: "xss", DataDir: dataDir}); err != nil {
		t.Fatalf("Render: %v", err)
	}
	out := buf.String()
	// Raw <script> from the recorded stream must not appear in the rendered
	// HTML — only its escaped form.
	if strings.Contains(out, "<script>alert(1)</script>") {
		t.Fatalf("rendered HTML contains unescaped <script> tag from recorded data — XSS regression")
	}
	if !strings.Contains(out, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Errorf("expected escaped form in output, got:\n%s", out)
	}
}

func TestRender_AssignsStderrClass(t *testing.T) {
	dataDir := t.TempDir()
	now := time.Now().UTC()
	src := &fakeSource{
		sessions: []storage.Session{{ID: "s", StartedAt: now}},
		spans: map[string][]storage.Span{
			"s": {{ID: "sp", SessionID: "s", Command: "sh", Mode: "pipe", StartedAt: now, EndedAt: now, ExitCode: intPtr(0)}},
		},
	}
	writeJSONL(t, dataDir, "s", "sp", []string{
		`{"ts":"2026-05-13T10:00:00Z","stream":"stdout","data":"out-line\n"}`,
		`{"ts":"2026-05-13T10:00:01Z","stream":"stderr","data":"err-line\n"}`,
	})

	var buf bytes.Buffer
	if _, err := Render(context.Background(), src, &buf, Options{SessionID: "s", DataDir: dataDir}); err != nil {
		t.Fatalf("Render: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `class="stream-stderr">err-line`) {
		t.Errorf("stderr chunk should carry stream-stderr class, got:\n%s", out)
	}
	if !strings.Contains(out, `class="stream-stdout">out-line`) {
		t.Errorf("stdout chunk should carry stream-stdout class, got:\n%s", out)
	}
}

func TestRender_ReportsCorruptLines(t *testing.T) {
	dataDir := t.TempDir()
	now := time.Now().UTC()
	src := &fakeSource{
		sessions: []storage.Session{{ID: "c", StartedAt: now}},
		spans: map[string][]storage.Span{
			"c": {{ID: "sp", SessionID: "c", Command: "sh", Mode: "pipe", StartedAt: now, EndedAt: now, ExitCode: intPtr(0)}},
		},
	}
	writeJSONL(t, dataDir, "c", "sp", []string{
		`{"ts":"2026-05-13T10:00:00Z","stream":"stdout","data":"ok\n"}`,
		`{not-json`,
	})

	var buf bytes.Buffer
	if _, err := Render(context.Background(), src, &buf, Options{SessionID: "c", DataDir: dataDir}); err != nil {
		t.Fatalf("Render: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "corrupt line") {
		t.Errorf("expected corrupt-line note in rendered HTML, got:\n%s", out)
	}
}

func TestRender_FailExitCodeMarked(t *testing.T) {
	dataDir := t.TempDir()
	now := time.Now().UTC()
	src := &fakeSource{
		sessions: []storage.Session{{ID: "f", StartedAt: now}},
		spans: map[string][]storage.Span{
			"f": {{ID: "sp", SessionID: "f", Command: "sh", Mode: "pipe", StartedAt: now, EndedAt: now, ExitCode: intPtr(3)}},
		},
	}
	var buf bytes.Buffer
	if _, err := Render(context.Background(), src, &buf, Options{SessionID: "f", DataDir: dataDir}); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(buf.String(), `class="exit fail"`) {
		t.Errorf("non-zero exit should get the fail class, got:\n%s", buf.String())
	}
}

func TestRender_MissingSessionErrors(t *testing.T) {
	src := &fakeSource{}
	var buf bytes.Buffer
	_, err := Render(context.Background(), src, &buf, Options{SessionID: "nope", DataDir: t.TempDir()})
	if err == nil {
		t.Fatalf("expected error for missing session, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' in error, got %q", err.Error())
	}
}

func TestRender_RequiresSessionIDOrLatest(t *testing.T) {
	src := &fakeSource{}
	var buf bytes.Buffer
	_, err := Render(context.Background(), src, &buf, Options{DataDir: t.TempDir()})
	if err == nil {
		t.Fatalf("expected error when neither SessionID nor Latest is set")
	}
}

// TestRender_SurfacesCorruptSessionDistinctly ensures a corrupt session row
// produces a "corrupt" error rather than the ambiguous "not found" — the
// reviewer flagged that conflation as a data-loss masking risk.
func TestRender_SurfacesCorruptSessionDistinctly(t *testing.T) {
	src := &fakeSource{corruptIDs: map[string]bool{"bad": true}}
	var buf bytes.Buffer
	_, err := Render(context.Background(), src, &buf, Options{SessionID: "bad", DataDir: t.TempDir()})
	if err == nil {
		t.Fatalf("expected error for corrupt session, got nil")
	}
	if !errors.Is(err, storage.ErrSessionCorrupt) {
		t.Errorf("err = %v, want one wrapping ErrSessionCorrupt", err)
	}
	if strings.Contains(err.Error(), "not found") {
		t.Errorf("err message should not say 'not found' for a corrupt row: %q", err.Error())
	}
}

// TestSortTags exercises the determinism guarantee directly. Within a
// single Go test binary, map iteration order is randomised once at process
// start and reused for every subsequent iteration, so a high-level "render
// twice and compare" test cannot actually detect a missing sort. This test
// asserts the explicit key order instead.
func TestSortTags(t *testing.T) {
	in := map[string]string{"z": "1", "a": "2", "m": "3", "b": "4"}
	got := sortTags(in)
	wantKeys := []string{"a", "b", "m", "z"}
	if len(got) != len(wantKeys) {
		t.Fatalf("len = %d, want %d", len(got), len(wantKeys))
	}
	for i, kv := range got {
		if kv.K != wantKeys[i] {
			t.Errorf("got[%d].K = %q, want %q", i, kv.K, wantKeys[i])
		}
		if kv.V != in[kv.K] {
			t.Errorf("got[%d].V = %q, want %q", i, kv.V, in[kv.K])
		}
	}
	// Empty map → empty slice (not nil checks specifically, just length).
	if got := sortTags(nil); len(got) != 0 {
		t.Errorf("sortTags(nil) returned %v, want empty", got)
	}
}

// TestRender_TagsAreDeterministic verifies that two renders of the same
// session produce byte-identical tag ordering, so CI artifact diffing
// stays meaningful. Map iteration order is randomised per process, so this
// test would flake without an explicit sort.
func TestRender_TagsAreDeterministic(t *testing.T) {
	dataDir := t.TempDir()
	now := time.Now().UTC()
	src := &fakeSource{
		sessions: []storage.Session{{
			ID:        "tags",
			StartedAt: now,
			Tags:      map[string]string{"z": "1", "a": "2", "m": "3", "b": "4"},
		}},
		spans: map[string][]storage.Span{
			"tags": {{ID: "sp", SessionID: "tags", Command: "sh", Mode: "pipe", StartedAt: now, EndedAt: now, ExitCode: intPtr(0)}},
		},
	}
	var first bytes.Buffer
	if _, err := Render(context.Background(), src, &first, Options{SessionID: "tags", DataDir: dataDir}); err != nil {
		t.Fatalf("Render 1: %v", err)
	}
	for i := 0; i < 10; i++ {
		var next bytes.Buffer
		if _, err := Render(context.Background(), src, &next, Options{SessionID: "tags", DataDir: dataDir}); err != nil {
			t.Fatalf("Render %d: %v", i+2, err)
		}
		if next.String() != first.String() {
			t.Fatalf("render %d differs from render 1 — non-deterministic output", i+2)
		}
	}
}

// TestRender_StripsC0ControlBytes verifies that ANSI escape sequences and
// other control bytes are removed from rendered data, so the report stays
// readable in a browser (full ANSI-to-HTML rendering is Phase 4 #15).
func TestRender_StripsC0ControlBytes(t *testing.T) {
	dataDir := t.TempDir()
	now := time.Now().UTC()
	src := &fakeSource{
		sessions: []storage.Session{{ID: "ansi", StartedAt: now}},
		spans: map[string][]storage.Span{
			"ansi": {{ID: "sp", SessionID: "ansi", Command: "sh", Mode: "pipe", StartedAt: now, EndedAt: now, ExitCode: intPtr(0)}},
		},
	}
	// On disk the runner emits json.Marshal output, which encodes control
	// bytes via \uXXXX escapes — never as raw bytes (raw control bytes
	// would make the line invalid JSON). So the fixture matches that
	// shape: \u001b for ESC, \u0000 for NUL, plus literal \t / \n.
	line := `{"ts":"2026-05-13T10:00:00Z","stream":"stdout","data":"\u001b[31mred\u001b[0m\tafter\u0000nul\nnewline"}`
	writeJSONL(t, dataDir, "ansi", "sp", []string{line})
	var buf bytes.Buffer
	if _, err := Render(context.Background(), src, &buf, Options{SessionID: "ansi", DataDir: dataDir}); err != nil {
		t.Fatalf("Render: %v", err)
	}
	out := buf.String()
	// \x1b should be stripped — the surrounding "[31m...red...[0m" survives
	// as readable text noise; \x00 should be gone; \t and \n preserved.
	if strings.ContainsRune(out, 0x1b) {
		t.Errorf("ESC byte should have been stripped from output")
	}
	if strings.ContainsRune(out, 0x00) {
		t.Errorf("NUL byte should have been stripped from output")
	}
	if !strings.Contains(out, "[31mred[0m\tafter") {
		t.Errorf("expected surrounding text after C0 stripping, got:\n%s", out)
	}
	// After stripping NUL between "after" and "nul", the two tokens are
	// adjacent. A surviving \n must separate "nul" from "newline".
	if !strings.Contains(out, "afternul\nnewline") {
		t.Errorf("expected NUL dropped (afternul) and \\n preserved (nul\\nnewline), got:\n%q", out)
	}
}

func TestSanitizeForHTML_PassthroughCommonChars(t *testing.T) {
	cases := []struct{ in, want string }{
		{"hello", "hello"},
		{"line1\nline2", "line1\nline2"},
		{"tabs\tand\rnewlines\n", "tabs\tand\rnewlines\n"},
		{"日本語", "日本語"}, // UTF-8 multi-byte stays intact
		{"esc:\x1b[31m", "esc:[31m"},
		{"nul:\x00", "nul:"},
		{"del:\x7f", "del:"},
		{"", ""},
	}
	for _, c := range cases {
		if got := sanitizeForHTML(c.in); got != c.want {
			t.Errorf("sanitizeForHTML(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
