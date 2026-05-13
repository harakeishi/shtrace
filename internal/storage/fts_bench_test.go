package storage_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/harakeishi/shtrace/internal/storage"
)

// openMigratedFTS opens a fresh FTS index under tb.TempDir() and migrates it.
func openMigratedFTS(tb testing.TB) (*storage.FTSStore, string) {
	tb.Helper()
	dir := tb.TempDir()
	fts, err := storage.OpenFTS(filepath.Join(dir, "outputs.idx"))
	if err != nil {
		tb.Fatalf("OpenFTS: %v", err)
	}
	if err := fts.MigrateFTS(context.Background()); err != nil {
		_ = fts.Close()
		tb.Fatalf("MigrateFTS: %v", err)
	}
	tb.Cleanup(func() { _ = fts.Close() })
	return fts, dir
}

// makeBenchChunks builds a realistic-looking JSONL chunk slice of the given
// line count. The matchTerm is embedded only when matches is true so that
// search benchmarks can control selectivity rather than always matching every
// document.
func makeBenchChunks(lines int, matches bool, matchTerm string) []storage.Chunk {
	out := make([]storage.Chunk, lines)
	verb := "skipped"
	if matches {
		verb = matchTerm
	}
	for i := 0; i < lines; i++ {
		out[i] = storage.Chunk{
			TS:     "2026-01-01T00:00:00Z",
			Stream: "stdout",
			Data:   fmt.Sprintf("build step %d %s module fooBar without warnings\n", i, verb),
		}
	}
	return out
}

// BenchmarkFTSIndexSpan measures the steady-state cost of indexing a fresh
// span's JSONL log. Each iteration uses a unique span_id so the bench
// exercises the INSERT-into-span_contents + trigger-INSERT-into-outputs_fts
// path rather than the slower delete-then-insert upsert path. 100 lines/span
// approximates a chatty test command.
func BenchmarkFTSIndexSpan(b *testing.B) {
	fts, dir := openMigratedFTS(b)
	ctx := context.Background()
	chunks := makeBenchChunks(100, true, "compiled")

	// Pre-write a single log file; the IndexSpan path reads it once per call
	// and the file contents are loop-invariant.
	logPath := writeTempLog(b, dir, "sess-bench", "span-template", chunks)

	i := 0
	for b.Loop() {
		spanID := "span-" + strconv.Itoa(i)
		if err := fts.IndexSpan(ctx, spanID, "sess-bench", logPath); err != nil {
			b.Fatalf("IndexSpan: %v", err)
		}
		i++
	}
}

// BenchmarkFTSSearch measures the read path behind `search_commands`
// (MCP Phase 2). Selectivity is meaningful: only ~20 % of seeded spans contain
// the probe term, so bm25 rank ordering is non-trivial. The bench asserts the
// exact expected hit count so a regression that returns fewer hits fails
// loudly instead of silently shrinking the result set.
func BenchmarkFTSSearch(b *testing.B) {
	fts, dir := openMigratedFTS(b)
	ctx := context.Background()

	const (
		spans     = 100
		matching  = 20 // ~20 % selectivity
		linesEach = 50
		probeTerm = "compiled"
		searchCap = 20
	)
	for i := 0; i < spans; i++ {
		spanID := "span-" + strconv.Itoa(i)
		chunks := makeBenchChunks(linesEach, i < matching, probeTerm)
		logPath := writeTempLog(b, dir, "sess-bench", spanID, chunks)
		if err := fts.IndexSpan(ctx, spanID, "sess-bench", logPath); err != nil {
			b.Fatalf("seed IndexSpan: %v", err)
		}
	}

	wantHits := matching
	if wantHits > searchCap {
		wantHits = searchCap
	}
	for b.Loop() {
		out, err := fts.Search(ctx, probeTerm, searchCap)
		if err != nil {
			b.Fatalf("Search: %v", err)
		}
		if len(out) != wantHits {
			b.Fatalf("Search returned %d hits, want %d", len(out), wantHits)
		}
	}
}
