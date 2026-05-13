package storage_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/harakeishi/shtrace/internal/storage"
)

// writeBenchLog mirrors writeTempLog (fts_test.go) but takes *testing.B so the
// benchmark helpers don't need to fabricate a *testing.T.
func writeBenchLog(b *testing.B, dir, sessionID, spanID string, chunks []storage.Chunk) string {
	b.Helper()
	logPath := storage.OutputPath(dir, sessionID, spanID)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		b.Fatalf("mkdir: %v", err)
	}
	f, err := os.Create(logPath)
	if err != nil {
		b.Fatalf("create log: %v", err)
	}
	defer func() { _ = f.Close() }()
	for _, c := range chunks {
		line, err := json.Marshal(c)
		if err != nil {
			b.Fatalf("marshal chunk: %v", err)
		}
		if _, err := f.Write(append(line, '\n')); err != nil {
			b.Fatalf("write chunk: %v", err)
		}
	}
	return logPath
}

// openMigratedFTS opens a fresh FTS index under b.TempDir() and migrates it.
func openMigratedFTS(b *testing.B) (*storage.FTSStore, string) {
	b.Helper()
	dir := b.TempDir()
	fts, err := storage.OpenFTS(filepath.Join(dir, "outputs.idx"))
	if err != nil {
		b.Fatalf("OpenFTS: %v", err)
	}
	if err := fts.MigrateFTS(context.Background()); err != nil {
		_ = fts.Close()
		b.Fatalf("MigrateFTS: %v", err)
	}
	b.Cleanup(func() { _ = fts.Close() })
	return fts, dir
}

// makeChunks builds a realistic-looking JSONL chunk slice of the given line
// count. Each line is short and printable so the timed cost is dominated by
// the FTS tokeniser rather than by file I/O.
func makeChunks(lines int) []storage.Chunk {
	out := make([]storage.Chunk, lines)
	for i := 0; i < lines; i++ {
		out[i] = storage.Chunk{
			TS:     "2026-01-01T00:00:00Z",
			Stream: "stdout",
			Data:   fmt.Sprintf("build step %d compiled module fooBar without warnings\n", i),
		}
	}
	return out
}

// BenchmarkFTSIndexSpan measures the cost of indexing one span's JSONL log
// into the FTS index. 100 lines/span approximates a chatty test command.
func BenchmarkFTSIndexSpan(b *testing.B) {
	fts, dir := openMigratedFTS(b)
	ctx := context.Background()
	chunks := makeChunks(100)

	// Pre-write the log file once; each iteration re-indexes the same path
	// (the upsert in IndexSpan handles the duplicate span_id).
	logPath := writeBenchLog(b, dir, "sess-bench", "span-bench", chunks)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := fts.IndexSpan(ctx, "span-bench", "sess-bench", logPath); err != nil {
			b.Fatalf("IndexSpan: %v", err)
		}
	}
}

// BenchmarkFTSSearch measures the read path that backs `search_commands`
// (MCP Phase 2). The index is pre-populated with 100 spans of 50 lines each
// — small enough to stay fast, large enough that rank ordering matters.
func BenchmarkFTSSearch(b *testing.B) {
	fts, dir := openMigratedFTS(b)
	ctx := context.Background()

	const spans = 100
	chunks := makeChunks(50)
	for i := 0; i < spans; i++ {
		spanID := fmt.Sprintf("span-%04d", i)
		logPath := writeBenchLog(b, dir, "sess-bench", spanID, chunks)
		if err := fts.IndexSpan(ctx, spanID, "sess-bench", logPath); err != nil {
			b.Fatalf("seed IndexSpan: %v", err)
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out, err := fts.Search(ctx, "compiled", 20)
		if err != nil {
			b.Fatalf("Search: %v", err)
		}
		if len(out) == 0 {
			b.Fatalf("Search returned 0 hits, want >0")
		}
	}
}
