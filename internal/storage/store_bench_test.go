package storage

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// openMigrated opens a fresh SQLite store under b.TempDir() and migrates it.
// All store benchmarks share this helper so the open/migrate cost stays out of
// the timed region.
func openMigrated(b *testing.B) *Store {
	b.Helper()
	s, err := Open(filepath.Join(b.TempDir(), "sessions.db"))
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		_ = s.Close()
		b.Fatalf("Migrate: %v", err)
	}
	b.Cleanup(func() { _ = s.Close() })
	return s
}

// BenchmarkInsertSpan measures the single-row write path that every wrapped
// command exercises on completion. The owning session is inserted once up
// front so the timed region is purely the span upsert.
func BenchmarkInsertSpan(b *testing.B) {
	s := openMigrated(b)
	ctx := context.Background()
	started := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)

	if err := s.InsertSession(ctx, Session{ID: "sess-bench", StartedAt: started}); err != nil {
		b.Fatalf("InsertSession: %v", err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		err := s.InsertSpan(ctx, Span{
			ID:        fmt.Sprintf("span-%d", i),
			SessionID: "sess-bench",
			Command:   "pytest",
			Argv:      []string{"pytest", "tests/"},
			Cwd:       "/repo",
			Mode:      "pipe",
			StartedAt: started,
			EndedAt:   started.Add(time.Second),
			ExitCode:  ptrInt(0),
		})
		if err != nil {
			b.Fatalf("InsertSpan: %v", err)
		}
	}
}

// BenchmarkListSessions measures the read path behind `shtrace ls`. The store
// is pre-populated with 1 000 sessions so each iteration runs the same query
// against a non-trivial table.
func BenchmarkListSessions(b *testing.B) {
	s := openMigrated(b)
	ctx := context.Background()
	base := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)

	const populate = 1000
	for i := 0; i < populate; i++ {
		if err := s.InsertSession(ctx, Session{
			ID:        fmt.Sprintf("sess-%04d", i),
			StartedAt: base.Add(time.Duration(i) * time.Second),
		}); err != nil {
			b.Fatalf("seed InsertSession: %v", err)
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out, err := s.ListSessions(ctx, 50, nil)
		if err != nil {
			b.Fatalf("ListSessions: %v", err)
		}
		if len(out) != 50 {
			b.Fatalf("ListSessions returned %d rows, want 50", len(out))
		}
	}
}

// BenchmarkSpansForSession measures the read path behind `shtrace show`. The
// target session holds 100 spans, mirroring a chatty test suite, so each
// iteration must parse a realistic batch of rows.
func BenchmarkSpansForSession(b *testing.B) {
	s := openMigrated(b)
	ctx := context.Background()
	base := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)

	if err := s.InsertSession(ctx, Session{ID: "sess-bench", StartedAt: base}); err != nil {
		b.Fatalf("InsertSession: %v", err)
	}
	const populate = 100
	for i := 0; i < populate; i++ {
		err := s.InsertSpan(ctx, Span{
			ID:        fmt.Sprintf("span-%04d", i),
			SessionID: "sess-bench",
			Command:   "pytest",
			Argv:      []string{"pytest", "tests/"},
			Cwd:       "/repo",
			Mode:      "pipe",
			StartedAt: base.Add(time.Duration(i) * time.Second),
			EndedAt:   base.Add(time.Duration(i+1) * time.Second),
			ExitCode:  ptrInt(0),
		})
		if err != nil {
			b.Fatalf("seed InsertSpan: %v", err)
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out, err := s.SpansForSession(ctx, "sess-bench", nil)
		if err != nil {
			b.Fatalf("SpansForSession: %v", err)
		}
		if len(out) != populate {
			b.Fatalf("SpansForSession returned %d rows, want %d", len(out), populate)
		}
	}
}
