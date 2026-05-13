package storage_test

import (
	"context"
	"encoding/json"
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
	defer func() { _ = f.Close() }()
	for _, c := range chunks {
		b, _ := json.Marshal(c)
		_, _ = f.Write(b)
		_, _ = f.Write([]byte("\n"))
	}
	return logPath
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

	results, err := fts.Search(ctx, "pytest", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 || results[0].SpanID != "span-a" {
		t.Errorf("expected span-a, got %+v", results)
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

	// Write the log file.
	writeTempLog(t, dir, "s1", "sp1", []storage.Chunk{
		{TS: "2026-01-01T00:00:00Z", Stream: "stdout", Data: "reindex target content"},
	})

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

	results, err := fts.Search(ctx, "reindex", 10)
	if err != nil {
		t.Fatalf("Search after reindex: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected result after ReindexAll, got none")
	}
	if results[0].SpanID != "sp1" {
		t.Errorf("expected sp1, got %s", results[0].SpanID)
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
		t.Errorf("expected exactly 1 result after double-index, got %d", len(results))
	}
}
