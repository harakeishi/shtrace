package storage

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// openMigrated opens a fresh SQLite store under b.TempDir() and migrates it.
// All store benchmarks share this helper so the open/migrate cost stays out
// of the timed region. Open() applies journal_mode=WAL and the default
// synchronous=NORMAL; SLI ceilings in the README assume those settings.
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
// front and loop-invariant fields (argv, started/ended timestamps, the
// exit-code pointer) are hoisted outside the timed region so the bench
// isolates the SQLite write cost.
func BenchmarkInsertSpan(b *testing.B) {
	s := openMigrated(b)
	ctx := context.Background()
	started := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	ended := started.Add(time.Second)
	exit := 0

	if err := s.InsertSession(ctx, Session{ID: "sess-bench", StartedAt: started}); err != nil {
		b.Fatalf("InsertSession: %v", err)
	}
	argv := []string{"pytest", "tests/"}

	i := 0
	for b.Loop() {
		err := s.InsertSpan(ctx, Span{
			ID:        "span-" + strconv.Itoa(i),
			SessionID: "sess-bench",
			Command:   "pytest",
			Argv:      argv,
			Cwd:       "/repo",
			Mode:      "pipe",
			StartedAt: started,
			EndedAt:   ended,
			ExitCode:  &exit,
		})
		if err != nil {
			b.Fatalf("InsertSpan: %v", err)
		}
		i++
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
	const wantRows = 50 // ListSessions default + explicit limit below
	for i := 0; i < populate; i++ {
		if err := s.InsertSession(ctx, Session{
			ID:        "sess-" + strconv.Itoa(i),
			StartedAt: base.Add(time.Duration(i) * time.Second),
		}); err != nil {
			b.Fatalf("seed InsertSession: %v", err)
		}
	}

	for b.Loop() {
		out, err := s.ListSessions(ctx, wantRows, nil)
		if err != nil {
			b.Fatalf("ListSessions: %v", err)
		}
		if len(out) != wantRows {
			b.Fatalf("ListSessions returned %d rows, want %d", len(out), wantRows)
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
	exit := 0
	argv := []string{"pytest", "tests/"}

	if err := s.InsertSession(ctx, Session{ID: "sess-bench", StartedAt: base}); err != nil {
		b.Fatalf("InsertSession: %v", err)
	}
	const populate = 100
	for i := 0; i < populate; i++ {
		err := s.InsertSpan(ctx, Span{
			ID:        "span-" + strconv.Itoa(i),
			SessionID: "sess-bench",
			Command:   "pytest",
			Argv:      argv,
			Cwd:       "/repo",
			Mode:      "pipe",
			StartedAt: base.Add(time.Duration(i) * time.Second),
			EndedAt:   base.Add(time.Duration(i+1) * time.Second),
			ExitCode:  &exit,
		})
		if err != nil {
			b.Fatalf("seed InsertSpan: %v", err)
		}
	}

	for b.Loop() {
		out, err := s.SpansForSession(ctx, "sess-bench", nil)
		if err != nil {
			b.Fatalf("SpansForSession: %v", err)
		}
		if len(out) != populate {
			b.Fatalf("SpansForSession returned %d rows, want %d", len(out), populate)
		}
	}
}
