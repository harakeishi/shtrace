package storage_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/harakeishi/shtrace/internal/storage"
)

func writeTempLog(t *testing.T, dir, sessionID, spanID string, chunks []storage.Chunk) string {
	t.Helper()
	logPath := storage.OutputPath(dir, sessionID, spanID)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	f, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("create log: %v", err)
	}
	for _, c := range chunks {
		b, _ := json.Marshal(c)
		_, _ = f.Write(b)
		_, _ = f.Write([]byte("\n"))
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close log: %v", err)
	}
	return logPath
}

func spanIDs(results []storage.SearchResult) []string {
	ids := make([]string, len(results))
	for i, r := range results {
		ids[i] = r.SpanID
	}
	return ids
}

func TestFTS_InsertAndSearch(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	fts, err := storage.OpenFTS(filepath.Join(dir, "outputs.idx"))
	if err != nil {
		t.Fatalf("OpenFTS: %v", err)
	}
	defer func() { _ = fts.Close() }()
	if err := fts.MigrateFTS(ctx); err != nil {
		t.Fatalf("MigrateFTS: %v", err)
	}

	// Write a fake JSONL log and index it.
	chunks := []storage.Chunk{
		{TS: "2026-01-01T00:00:00Z", Stream: "stdout", Data: "hello world from the build\n"},
		{TS: "2026-01-01T00:00:01Z", Stream: "stderr", Data: "error: compilation failed\n"},
	}
	logPath := writeTempLog(t, dir, "sess1", "span1", chunks)

	if err := fts.IndexSpan(ctx, "span1", "sess1", logPath); err != nil {
		t.Fatalf("IndexSpan: %v", err)
	}

	// Search for a term that appears in stdout.
	results, err := fts.Search(ctx, "build", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected at least one result, got none")
	}
	if results[0].SpanID != "span1" || results[0].SessionID != "sess1" {
		t.Errorf("unexpected result: %+v", results[0])
	}
	if !strings.Contains(results[0].Snippet, "build") {
		t.Errorf("snippet does not contain 'build': %q", results[0].Snippet)
	}

	// Search for a term in stderr.
	results, err = fts.Search(ctx, "compilation", 10)
	if err != nil {
		t.Fatalf("Search stderr term: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected stderr content to be indexed, got no results")
	}

	// Search for a term that does not exist.
	results, err = fts.Search(ctx, "xyzzy_nonexistent", 10)
	if err != nil {
		t.Fatalf("Search missing term: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected no results for missing term, got %d", len(results))
	}
}

func TestFTS_MultipleSpans(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	fts, err := storage.OpenFTS(filepath.Join(dir, "outputs.idx"))
	if err != nil {
		t.Fatalf("OpenFTS: %v", err)
	}
	defer func() { _ = fts.Close() }()
	if err := fts.MigrateFTS(ctx); err != nil {
		t.Fatalf("MigrateFTS: %v", err)
	}

	spans := []struct {
		spanID  string
		content string
	}{
		{"span-a", "pytest passed 42 tests"},
		{"span-b", "go test FAIL: TestFoo"},
		{"span-c", "npm run build succeeded"},
	}
	for _, sp := range spans {
		logPath := writeTempLog(t, dir, "sessX", sp.spanID, []storage.Chunk{
			{TS: "2026-01-01T00:00:00Z", Stream: "stdout", Data: sp.content},
		})
		if err := fts.IndexSpan(ctx, sp.spanID, "sessX", logPath); err != nil {
			t.Fatalf("IndexSpan %s: %v", sp.spanID, err)
		}
	}

	// Verify that searching "pytest" returns exactly span-a and not the others.
	results, err := fts.Search(ctx, "pytest", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result for 'pytest', got %d (span IDs: %v)", len(results), spanIDs(results))
	}
	if results[0].SpanID != "span-a" {
		t.Errorf("expected span-a, got %q", results[0].SpanID)
	}

	// Verify span-b and span-c are not returned for "pytest".
	for _, r := range results {
		if r.SpanID == "span-b" || r.SpanID == "span-c" {
			t.Errorf("unexpected span in results: %q", r.SpanID)
		}
	}
}

func TestFTS_ReindexAll(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	// Set up a metadata store with one session and one span.
	metaStore, err := storage.Open(filepath.Join(dir, "sessions.db"))
	if err != nil {
		t.Fatalf("Open store: %v", err)
	}
	defer func() { _ = metaStore.Close() }()
	if err := metaStore.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	_ = metaStore.InsertSession(ctx, storage.Session{ID: "s1"})
	exitCode := 0
	_ = metaStore.InsertSpan(ctx, storage.Span{
		ID: "sp1", SessionID: "s1", Command: "echo", Mode: "pipe", ExitCode: &exitCode,
	})

	fts, err := storage.OpenFTS(filepath.Join(dir, "outputs.idx"))
	if err != nil {
		t.Fatalf("OpenFTS: %v", err)
	}
	defer func() { _ = fts.Close() }()
	if err := fts.MigrateFTS(ctx); err != nil {
		t.Fatalf("MigrateFTS: %v", err)
	}

	// Pre-index stale content so we can verify ReindexAll replaces it.
	staleLog := writeTempLog(t, dir, "s1", "sp1", []storage.Chunk{
		{TS: "2026-01-01T00:00:00Z", Stream: "stdout", Data: "stale old content"},
	})
	if err := fts.IndexSpan(ctx, "sp1", "s1", staleLog); err != nil {
		t.Fatalf("IndexSpan stale: %v", err)
	}
	// Overwrite the log file with fresh content (simulates a corrupt/outdated index).
	writeTempLog(t, dir, "s1", "sp1", []storage.Chunk{
		{TS: "2026-01-01T00:00:00Z", Stream: "stdout", Data: "reindex target content"},
	})

	if err := storage.ReindexAll(ctx, fts, metaStore, dir); err != nil {
		t.Fatalf("ReindexAll: %v", err)
	}

	// Fresh content must be findable exactly once (no duplicate entries).
	results, err := fts.Search(ctx, "reindex", 10)
	if err != nil {
		t.Fatalf("Search after reindex: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected result after ReindexAll, got none")
	}
	if len(results) != 1 {
		t.Errorf("expected exactly 1 result after ReindexAll (duplicate entries?), got %d (span IDs: %v)", len(results), spanIDs(results))
	}
	if results[0].SpanID != "sp1" {
		t.Errorf("expected sp1, got %s", results[0].SpanID)
	}

	// Stale content must no longer be findable.
	stale, err := fts.Search(ctx, "stale", 10)
	if err != nil {
		t.Fatalf("Search for stale content: %v", err)
	}
	if len(stale) != 0 {
		t.Errorf("expected stale content to be gone after ReindexAll, got %d results", len(stale))
	}
}

// TestFTS_ReindexAll_ManySessionsIndexesAll verifies that ReindexAll processes
// more than 50 sessions — the default cap that ListSessions applies when limit
// is 0 or negative. Regression test for the math.MaxInt32 fix.
func TestFTS_ReindexAll_ManySessionsIndexesAll(t *testing.T) {
	const total = 55 // intentionally > the former 50-session default cap
	dir := t.TempDir()
	ctx := context.Background()

	metaStore, err := storage.Open(filepath.Join(dir, "sessions.db"))
	if err != nil {
		t.Fatalf("Open store: %v", err)
	}
	defer func() { _ = metaStore.Close() }()
	if err := metaStore.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	exitCode := 0
	for i := 0; i < total; i++ {
		sessID := fmt.Sprintf("sess%03d", i)
		spanID := fmt.Sprintf("span%03d", i)
		_ = metaStore.InsertSession(ctx, storage.Session{ID: sessID})
		_ = metaStore.InsertSpan(ctx, storage.Span{
			ID: spanID, SessionID: sessID, Command: "echo", Mode: "pipe", ExitCode: &exitCode,
		})
		writeTempLog(t, dir, sessID, spanID, []storage.Chunk{
			{TS: "2026-01-01T00:00:00Z", Stream: "stdout", Data: fmt.Sprintf("uniquetoken%03d output", i)},
		})
	}

	fts, err := storage.OpenFTS(filepath.Join(dir, "outputs.idx"))
	if err != nil {
		t.Fatalf("OpenFTS: %v", err)
	}
	defer func() { _ = fts.Close() }()
	if err := fts.MigrateFTS(ctx); err != nil {
		t.Fatalf("MigrateFTS: %v", err)
	}

	if err := storage.ReindexAll(ctx, fts, metaStore, dir); err != nil {
		t.Fatalf("ReindexAll: %v", err)
	}

	// Every session's span must be indexed. Check a sample at the boundaries.
	for _, i := range []int{0, 49, 50, total - 1} {
		token := fmt.Sprintf("uniquetoken%03d", i)
		results, err := fts.Search(ctx, token, 10)
		if err != nil {
			t.Fatalf("Search %q: %v", token, err)
		}
		if len(results) == 0 {
			t.Errorf("session %d (token %q) not indexed after ReindexAll", i, token)
		}
	}
}

func TestFTS_IdempotentIndex(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	fts, err := storage.OpenFTS(filepath.Join(dir, "outputs.idx"))
	if err != nil {
		t.Fatalf("OpenFTS: %v", err)
	}
	defer func() { _ = fts.Close() }()
	if err := fts.MigrateFTS(ctx); err != nil {
		t.Fatalf("MigrateFTS: %v", err)
	}

	logPath := writeTempLog(t, dir, "s1", "sp1", []storage.Chunk{
		{TS: "2026-01-01T00:00:00Z", Stream: "stdout", Data: "unique content here"},
	})

	// Index the same span twice — should not produce duplicate results.
	for i := 0; i < 2; i++ {
		if err := fts.IndexSpan(ctx, "sp1", "s1", logPath); err != nil {
			t.Fatalf("IndexSpan (iteration %d): %v", i, err)
		}
	}

	results, err := fts.Search(ctx, "unique", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Errorf("expected exactly 1 result after double-index, got %d (span IDs: %v)", len(results), spanIDs(results))
	}
	// Verify the snippet contains the correct content (confirms rowid alignment).
	if len(results) == 1 && !strings.Contains(results[0].Snippet, "unique") {
		t.Errorf("snippet after re-index does not contain 'unique': %q", results[0].Snippet)
	}
}
