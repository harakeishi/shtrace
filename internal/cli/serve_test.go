package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/harakeishi/shtrace/internal/storage"
)

// openTestStore sets up a store in a temp dir and migrates it.
func openTestStore(t *testing.T) (*storage.Store, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := storage.Open(filepath.Join(dir, "sessions.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return store, dir
}

// insertTestSession inserts a session and fails the test on error.
func insertTestSession(t *testing.T, store *storage.Store, id string) {
	t.Helper()
	if err := store.InsertSession(context.Background(), storage.Session{
		ID:        id,
		StartedAt: time.Now().UTC(),
		Tags:      map[string]string{},
	}); err != nil {
		t.Fatalf("insert session %q: %v", id, err)
	}
}

// insertTestSpan inserts a span and fails the test on error.
func insertTestSpan(t *testing.T, store *storage.Store, sessID, spanID, cmd string) {
	t.Helper()
	exitCode := 0
	if err := store.InsertSpan(context.Background(), storage.Span{
		ID:        spanID,
		SessionID: sessID,
		Command:   cmd,
		Argv:      []string{cmd},
		Mode:      "pipe",
		StartedAt: time.Now().UTC(),
		EndedAt:   time.Now().UTC().Add(time.Second),
		ExitCode:  &exitCode,
	}); err != nil {
		t.Fatalf("insert span %q: %v", spanID, err)
	}
}

// ---- parseServeArgs ----

func TestParseServeArgs(t *testing.T) {
	cases := []struct {
		args     []string
		wantPort int
		wantErr  bool
	}{
		{nil, defaultServePort, false},
		{[]string{"--port", "8080"}, 8080, false},
		{[]string{"--port=9090"}, 9090, false},
		{[]string{"--port", "1"}, 1, false},
		{[]string{"--port", "65535"}, 65535, false},
		{[]string{"--port", "0"}, 0, true},
		{[]string{"--port", "65536"}, 0, true},
		{[]string{"--port", "99999"}, 0, true},
		{[]string{"--port"}, 0, true},
		{[]string{"--unknown"}, 0, true},
	}
	for _, tc := range cases {
		p, err := parseServeArgs(tc.args)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseServeArgs(%v): want error, got port=%d", tc.args, p)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseServeArgs(%v): unexpected error: %v", tc.args, err)
			continue
		}
		if p != tc.wantPort {
			t.Errorf("parseServeArgs(%v): got port=%d, want %d", tc.args, p, tc.wantPort)
		}
	}
}

// ---- /api/sessions ----

func TestSessionsHandler_OK(t *testing.T) {
	store, _ := openTestStore(t)

	if err := store.InsertSession(context.Background(), storage.Session{
		ID:        "sess-abc",
		StartedAt: time.Now().UTC(),
		Tags:      map[string]string{"pr": "42"},
	}); err != nil {
		t.Fatalf("insert session: %v", err)
	}

	h := makeSessionsHandler(store)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/api/sessions", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	var sessions []apiSession
	if err := json.Unmarshal(rec.Body.Bytes(), &sessions); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("got %d sessions, want 1", len(sessions))
	}
	if sessions[0].ID != "sess-abc" {
		t.Errorf("id=%q, want sess-abc", sessions[0].ID)
	}
	if sessions[0].Tags["pr"] != "42" {
		t.Errorf("tags=%v, want pr=42", sessions[0].Tags)
	}
}

func TestSessionsHandler_Capped(t *testing.T) {
	store, _ := openTestStore(t)
	// Insert 502 sessions (cap is 500; 501 triggers the capped flag).
	for i := 0; i < 502; i++ {
		if err := store.InsertSession(context.Background(), storage.Session{
			ID:        fmt.Sprintf("sess-%04d", i),
			StartedAt: time.Now().UTC(),
			Tags:      map[string]string{},
		}); err != nil {
			t.Fatalf("insert session %d: %v", i, err)
		}
	}

	h := makeSessionsHandler(store)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/api/sessions", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("X-Shtrace-Sessions-Capped"); got != "true" {
		t.Errorf("X-Shtrace-Sessions-Capped=%q, want true", got)
	}
	var sessions []apiSession
	if err := json.Unmarshal(rec.Body.Bytes(), &sessions); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(sessions) != 500 {
		t.Errorf("got %d sessions, want exactly 500", len(sessions))
	}
}

func TestSessionsHandler_MethodNotAllowed(t *testing.T) {
	h := makeSessionsHandler(nil)
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(method, "/api/sessions", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("method %s: status %d, want 405", method, rec.Code)
		}
		if allow := rec.Header().Get("Allow"); allow == "" {
			t.Errorf("method %s: Allow header missing from 405 response", method)
		}
	}
}

// ---- /api/sessions/{id}/spans ----

func TestSpansHandler_OK(t *testing.T) {
	store, _ := openTestStore(t)

	insertTestSession(t, store, "sess-1")
	exitCode := 0
	if err := store.InsertSpan(context.Background(), storage.Span{
		ID:        "span-1",
		SessionID: "sess-1",
		Command:   "echo",
		Argv:      []string{"echo", "hi"},
		Mode:      "pipe",
		StartedAt: time.Now().UTC(),
		EndedAt:   time.Now().UTC().Add(time.Second),
		ExitCode:  &exitCode,
	}); err != nil {
		t.Fatalf("insert span: %v", err)
	}

	h := makeSpansHandler(store)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/api/sessions/sess-1/spans", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	var spans []apiSpan
	if err := json.Unmarshal(rec.Body.Bytes(), &spans); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	if spans[0].Command != "echo" {
		t.Errorf("command=%q, want echo", spans[0].Command)
	}
	if spans[0].ExitCode == nil || *spans[0].ExitCode != 0 {
		t.Errorf("exit_code=%v, want 0", spans[0].ExitCode)
	}
}

func TestSpansHandler_BadPath(t *testing.T) {
	store, _ := openTestStore(t)
	h := makeSpansHandler(store)

	cases := []struct {
		path     string
		wantCode int
	}{
		{"/api/sessions//spans", http.StatusBadRequest},
		{"/api/sessions/a/b/spans", http.StatusBadRequest},
		{"/api/sessions/foo", http.StatusNotFound},         // missing /spans suffix
		{"/api/sessions/foo/spans/extra", http.StatusNotFound}, // extra segment
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if rec.Code != tc.wantCode {
			t.Errorf("path %q: status %d, want %d", tc.path, rec.Code, tc.wantCode)
		}
	}
}

func TestSpansHandler_DotDotSessionID(t *testing.T) {
	store, _ := openTestStore(t)
	h := makeSpansHandler(store)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/api/sessions/../spans", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status %d, want 400 for .. in session ID", rec.Code)
	}
}

func TestSpansHandler_SessionNotFound(t *testing.T) {
	store, _ := openTestStore(t)
	h := makeSpansHandler(store)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/api/sessions/nonexistent-session/spans", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status %d, want 404 for unknown session ID", rec.Code)
	}
}

func TestSpansHandler_MethodNotAllowed(t *testing.T) {
	h := makeSpansHandler(nil)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, "/api/sessions/x/spans", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow == "" {
		t.Error("Allow header missing from 405 response")
	}
}

// ---- /api/search ----

func TestSearchHandler_NoFTS_WithQuery(t *testing.T) {
	h := makeSearchHandler(nil)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/api/search?q=hello", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", rec.Code)
	}
}

func TestSearchHandler_NoFTS_EmptyQuery(t *testing.T) {
	// fts==nil check runs before q check, so empty query also returns 503.
	h := makeSearchHandler(nil)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/api/search", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503 (fts nil takes precedence over empty query)", rec.Code)
	}
}

func TestSearchHandler_MethodNotAllowed(t *testing.T) {
	h := makeSearchHandler(nil)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, "/api/search", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow == "" {
		t.Error("Allow header missing from 405 response")
	}
}

// ---- / (UI) ----

func TestUIHandler_OK(t *testing.T) {
	h := makeUIHandler()
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type=%q, want text/html", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<title>shtrace</title>") {
		t.Error("response does not contain expected title")
	}
	if !strings.Contains(body, "loadSessions") {
		t.Error("response does not contain JS entry point")
	}
	const wantCSP = "default-src 'self'; style-src 'unsafe-inline'; script-src 'unsafe-inline'"
	if got := rec.Header().Get("Content-Security-Policy"); got != wantCSP {
		t.Errorf("Content-Security-Policy=%q, want %q", got, wantCSP)
	}
}

func TestUIHandler_NotFound(t *testing.T) {
	h := makeUIHandler()
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/unknown", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", rec.Code)
	}
}

func TestUIHandler_MethodNotAllowed(t *testing.T) {
	h := makeUIHandler()
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(method, "/", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("method %s: status %d, want 405", method, rec.Code)
		}
		if allow := rec.Header().Get("Allow"); allow == "" {
			t.Errorf("method %s: Allow header missing from 405 response", method)
		}
	}
}

// ---- /api/output/{sessID}/{spanID} ----

func TestOutputHandler_SpanNotFound(t *testing.T) {
	store, dataDir := openTestStore(t)
	insertTestSession(t, store, "s1")

	h := makeOutputHandler(store, dataDir)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/api/output/s1/nonexistent-span", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", rec.Code)
	}
}

func TestOutputHandler_BadPath(t *testing.T) {
	store, dataDir := openTestStore(t)
	h := makeOutputHandler(store, dataDir)

	for _, path := range []string{"/api/output/", "/api/output/onlyone"} {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("path %q: status %d, want 400", path, rec.Code)
		}
	}
}

func TestOutputHandler_ReadsLog(t *testing.T) {
	store, dataDir := openTestStore(t)

	insertTestSession(t, store, "sess-out")
	insertTestSpan(t, store, "sess-out", "span-out", "echo")

	logPath := storage.OutputPath(dataDir, "sess-out", "span-out")
	if err := os.MkdirAll(parentDir(logPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	chunk := `{"stream":"stdout","data":"hello world\n"}` + "\n"
	if err := os.WriteFile(logPath, []byte(chunk), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	h := makeOutputHandler(store, dataDir)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/api/output/sess-out/span-out", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	body, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(body), "hello world") {
		t.Errorf("body=%q does not contain expected output", body)
	}
}

func TestOutputHandler_CorruptLines(t *testing.T) {
	store, dataDir := openTestStore(t)

	insertTestSession(t, store, "sess-c")
	insertTestSpan(t, store, "sess-c", "span-c", "echo")

	logPath := storage.OutputPath(dataDir, "sess-c", "span-c")
	if err := os.MkdirAll(parentDir(logPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// One valid line, one corrupt line.
	content := `{"stream":"stdout","data":"ok\n"}` + "\n" + `not json` + "\n"
	if err := os.WriteFile(logPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	h := makeOutputHandler(store, dataDir)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/api/output/sess-c/span-c", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	if rec.Header().Get("X-Shtrace-Corrupt-Lines") != "1" {
		t.Errorf("X-Shtrace-Corrupt-Lines=%q, want 1", rec.Header().Get("X-Shtrace-Corrupt-Lines"))
	}
}

func TestOutputHandler_MethodNotAllowed(t *testing.T) {
	store, dataDir := openTestStore(t)
	h := makeOutputHandler(store, dataDir)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, "/api/output/s/sp", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow == "" {
		t.Error("Allow header missing from 405 response")
	}
}

func TestOutputHandler_TruncatedHeader(t *testing.T) {
	store, dataDir := openTestStore(t)
	insertTestSession(t, store, "sess-trunc")
	insertTestSpan(t, store, "sess-trunc", "span-trunc", "cat")

	logPath := storage.OutputPath(dataDir, "sess-trunc", "span-trunc")
	if err := os.MkdirAll(parentDir(logPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Write a log that fits well within the limit to confirm header is absent.
	chunk := `{"stream":"stdout","data":"tiny\n"}` + "\n"
	if err := os.WriteFile(logPath, []byte(chunk), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	h := makeOutputHandler(store, dataDir)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/api/output/sess-trunc/span-trunc", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("X-Shtrace-Truncated"); got != "" {
		t.Errorf("X-Shtrace-Truncated=%q, want empty for small log", got)
	}
}

func TestOutputHandler_PathTraversal(t *testing.T) {
	store, dataDir := openTestStore(t)
	insertTestSession(t, store, "sess-t")

	h := makeOutputHandler(store, dataDir)

	// IDs containing path traversal sequences must not be served.
	// Paths with "/" in sessionID/spanID segments return 400; others 404.
	for _, path := range []string{
		"/api/output/sess-t/../etc/passwd",
		"/api/output/../sess-t/span-id",
	} {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code == http.StatusOK {
			t.Errorf("path %q: got 200, expected non-200 (path traversal must be rejected)", path)
		}
	}
}

func TestOutputHandler_SpanIDWithSlash(t *testing.T) {
	store, dataDir := openTestStore(t)
	insertTestSession(t, store, "sess-sl")

	h := makeOutputHandler(store, dataDir)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/api/output/sess-sl/span-a/extra", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status %d, want 400 for spanID containing slash", rec.Code)
	}
}

func TestOutputHandler_DotDotInIDs(t *testing.T) {
	store, dataDir := openTestStore(t)
	h := makeOutputHandler(store, dataDir)

	// ".." in sessionID or spanID must be rejected before any DB lookup.
	for _, path := range []string{
		"/api/output/../evil/span-id",
		"/api/output/sess-id/..evil",
		"/api/output/sess..id/span-id",
	} {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code == http.StatusOK {
			t.Errorf("path %q: got 200, want non-200 (.. should be rejected)", path)
		}
	}
}

func TestOutputHandler_CrossSessionSpan(t *testing.T) {
	store, dataDir := openTestStore(t)

	// span-x belongs to sess-A, not sess-B.
	insertTestSession(t, store, "sess-A")
	insertTestSession(t, store, "sess-B")
	insertTestSpan(t, store, "sess-A", "span-x", "echo")

	h := makeOutputHandler(store, dataDir)
	rec := httptest.NewRecorder()
	// Requesting sess-B / span-x must return 404, not serve sess-A's output.
	h(rec, httptest.NewRequest(http.MethodGet, "/api/output/sess-B/span-x", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status %d, want 404 for span belonging to a different session", rec.Code)
	}
}
