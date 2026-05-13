package report

import (
	"bytes"
	"context"
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
