package storage

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestStore_RecordsSessionAndSpan(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "sessions.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	started := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	if err := s.InsertSession(context.Background(), Session{
		ID:        "sess-1",
		StartedAt: started,
		Tags:      map[string]string{"pr_number": "42"},
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}

	if err := s.InsertSpan(context.Background(), Span{
		ID:           "span-1",
		SessionID:    "sess-1",
		ParentSpanID: "",
		Command:      "pytest",
		Argv:         []string{"pytest", "tests/"},
		Cwd:          "/repo",
		Mode:         "pipe",
		StartedAt:    started,
		EndedAt:      started.Add(2 * time.Second),
		ExitCode:     ptrInt(0),
	}); err != nil {
		t.Fatalf("InsertSpan: %v", err)
	}

	sessions, err := s.ListSessions(context.Background(), 10, nil)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("ListSessions returned %d sessions, want 1", len(sessions))
	}
	if sessions[0].ID != "sess-1" {
		t.Fatalf("session id = %q, want sess-1", sessions[0].ID)
	}
	if sessions[0].Tags["pr_number"] != "42" {
		t.Fatalf("session tags = %v, want pr_number=42", sessions[0].Tags)
	}

	spans, err := s.SpansForSession(context.Background(), "sess-1", nil)
	if err != nil {
		t.Fatalf("SpansForSession: %v", err)
	}
	if len(spans) != 1 {
		t.Fatalf("SpansForSession returned %d spans, want 1", len(spans))
	}
	if spans[0].Argv[0] != "pytest" {
		t.Fatalf("span argv = %v, want pytest first", spans[0].Argv)
	}
	if spans[0].ExitCode == nil || *spans[0].ExitCode != 0 {
		t.Fatalf("span exit code = %v, want 0", spans[0].ExitCode)
	}
}

// TestStore_EnsureSession_NoopWhenExists guards the nested-shtrace contract:
// a child invocation that finds an existing session row must not clobber the
// parent's started_at or already-set ended_at.
func TestStore_EnsureSession_NoopWhenExists(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}

	parentStarted := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	parentEnded := parentStarted.Add(5 * time.Minute)
	if err := s.InsertSession(context.Background(), Session{
		ID:        "sess-1",
		StartedAt: parentStarted,
		EndedAt:   &parentEnded,
		Tags:      map[string]string{"pr_number": "42"},
	}); err != nil {
		t.Fatal(err)
	}

	// Child calls EnsureSession with a different start time and no end.
	childStarted := parentStarted.Add(2 * time.Minute)
	if err := s.EnsureSession(context.Background(), Session{
		ID:        "sess-1",
		StartedAt: childStarted,
		Tags:      map[string]string{}, // child env had no tags
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListSessions(context.Background(), 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("ListSessions = %d, want 1", len(got))
	}
	if !got[0].StartedAt.Equal(parentStarted) {
		t.Fatalf("started_at = %v, want preserved parent value %v", got[0].StartedAt, parentStarted)
	}
	if got[0].EndedAt == nil || !got[0].EndedAt.Equal(parentEnded) {
		t.Fatalf("ended_at = %v, want preserved parent value %v", got[0].EndedAt, parentEnded)
	}
	if got[0].Tags["pr_number"] != "42" {
		t.Fatalf("tags clobbered: %v", got[0].Tags)
	}
}

// TestStore_EnsureSession_InsertsWhenMissing makes sure the child path
// actually creates the session row when the parent wrote to a different DB
// (e.g., a different SHTRACE_DATA_DIR). Without this, the subsequent
// InsertSpan would fail with a FOREIGN KEY violation.
func TestStore_EnsureSession_InsertsWhenMissing(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}

	started := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	if err := s.EnsureSession(context.Background(), Session{
		ID:        "child-only",
		StartedAt: started,
		Tags:      map[string]string{"agent_name": "claude"},
	}); err != nil {
		t.Fatal(err)
	}

	// Subsequent InsertSpan must succeed (no FK violation).
	if err := s.InsertSpan(context.Background(), Span{
		ID:        "span-1",
		SessionID: "child-only",
		Command:   "go",
		Argv:      []string{"go", "test"},
		Mode:      "pipe",
		StartedAt: started,
		EndedAt:   started.Add(time.Second),
	}); err != nil {
		t.Fatalf("InsertSpan after EnsureSession: %v", err)
	}
}

// TestStore_InsertSession_PreservesEndedAtOnReinsert covers the case where
// a child invocation writes to the same DB after the parent has closed the
// session: the upsert must not zero out the previously-set ended_at.
func TestStore_InsertSession_PreservesEndedAtOnReinsert(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}

	started := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	ended := started.Add(time.Minute)
	if err := s.InsertSession(context.Background(), Session{
		ID:        "s",
		StartedAt: started,
		EndedAt:   &ended,
		Tags:      map[string]string{"k": "v"},
	}); err != nil {
		t.Fatal(err)
	}

	// A second insert with nil ended_at (simulating a child kicking off
	// while the row is closed) must preserve the original ended_at.
	if err := s.InsertSession(context.Background(), Session{
		ID:        "s",
		StartedAt: started,
		Tags:      map[string]string{"k": "v"},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListSessions(context.Background(), 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].EndedAt == nil || !got[0].EndedAt.Equal(ended) {
		t.Fatalf("ended_at = %v, want preserved value %v", got[0].EndedAt, ended)
	}
}

func TestStore_ListSessions_OrderedByStartedDesc(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "sessions.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	t1 := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Hour)

	if err := s.InsertSession(context.Background(), Session{ID: "older", StartedAt: t1}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertSession(context.Background(), Session{ID: "newer", StartedAt: t2}); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListSessions(context.Background(), 10, nil)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(got) != 2 || got[0].ID != "newer" || got[1].ID != "older" {
		t.Fatalf("ListSessions order = %+v, want newest first", got)
	}
}

// TestStore_ListSessions_SkipsCorruptRowAndReportsViaWarn: one bad row must
// not take the whole listing offline (Round-2 finding), but the failure must
// still be observable via the warn callback so it isn't silently dropped.
func TestStore_ListSessions_SkipsCorruptRowAndReportsViaWarn(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}

	good := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	if err := s.InsertSession(context.Background(), Session{ID: "good", StartedAt: good}); err != nil {
		t.Fatal(err)
	}
	// Inject a corrupt row directly.
	if _, err := s.db.ExecContext(context.Background(),
		`INSERT INTO sessions(id, started_at, tags_json) VALUES(?, ?, ?)`,
		"bad", "not-a-timestamp", "{}"); err != nil {
		t.Fatal(err)
	}

	var warned []error
	got, err := s.ListSessions(context.Background(), 10, func(e error) { warned = append(warned, e) })
	if err != nil {
		t.Fatalf("ListSessions: %v (should be lenient on per-row errors)", err)
	}
	if len(got) != 1 || got[0].ID != "good" {
		t.Fatalf("ListSessions = %+v, want only the good row", got)
	}
	if len(warned) == 0 {
		t.Fatalf("warn callback was never invoked for corrupt row")
	}
}

// TestStore_SpansForSession_SkipsCorruptRowAndReportsViaWarn: same lenient
// behaviour for spans so `shtrace show` can still render the healthy spans
// even when one is corrupt.
func TestStore_SpansForSession_SkipsCorruptRowAndReportsViaWarn(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}

	started := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	if err := s.InsertSession(context.Background(), Session{ID: "s", StartedAt: started}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertSpan(context.Background(), Span{
		ID: "good", SessionID: "s", Command: "go", Argv: []string{"go"}, Mode: "pipe",
		StartedAt: started, EndedAt: started.Add(time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	// Corrupt span row.
	if _, err := s.db.ExecContext(context.Background(),
		`INSERT INTO spans(id, session_id, parent_span_id, command, argv_json, cwd, mode, started_at, ended_at)
		 VALUES(?, ?, '', ?, ?, '', 'pipe', ?, ?)`,
		"bad", "s", "go", "[bad json", "not-a-ts", "also-bad"); err != nil {
		t.Fatal(err)
	}

	var warned []error
	got, err := s.SpansForSession(context.Background(), "s", func(e error) { warned = append(warned, e) })
	if err != nil {
		t.Fatalf("SpansForSession: %v (should be lenient)", err)
	}
	if len(got) != 1 || got[0].ID != "good" {
		t.Fatalf("SpansForSession = %+v, want only the good span", got)
	}
	if len(warned) == 0 {
		t.Fatalf("warn callback not invoked")
	}
}

func TestStore_GetSession_ReturnsNotFoundWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "sessions.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	_, err = s.GetSession(context.Background(), "does-not-exist")
	if !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("GetSession on absent id returned %v, want ErrSessionNotFound", err)
	}
}

func TestStore_GetSession_ReturnsCorruptOnBadTimestamp(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "sessions.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Inject a corrupt row directly so we don't rely on InsertSession
	// being permissive — that would couple this test to insert validation.
	if _, err := s.db.ExecContext(context.Background(),
		`INSERT INTO sessions(id, started_at, tags_json) VALUES('corrupt', 'not-a-ts', '{}')`); err != nil {
		t.Fatalf("inject corrupt row: %v", err)
	}

	_, err = s.GetSession(context.Background(), "corrupt")
	if !errors.Is(err, ErrSessionCorrupt) {
		t.Fatalf("GetSession on corrupt row returned %v, want wrapping ErrSessionCorrupt", err)
	}
}

func TestStore_GetSession_RoundTripsHealthyRow(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "sessions.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	started := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	ended := started.Add(7 * time.Second)
	want := Session{
		ID:        "sess-rt",
		StartedAt: started,
		EndedAt:   &ended,
		Tags:      map[string]string{"k": "v"},
	}
	if err := s.InsertSession(context.Background(), want); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}

	got, err := s.GetSession(context.Background(), "sess-rt")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.ID != want.ID {
		t.Errorf("ID = %q, want %q", got.ID, want.ID)
	}
	if !got.StartedAt.Equal(want.StartedAt) {
		t.Errorf("StartedAt = %v, want %v", got.StartedAt, want.StartedAt)
	}
	if got.EndedAt == nil || !got.EndedAt.Equal(ended) {
		t.Errorf("EndedAt = %v, want %v", got.EndedAt, ended)
	}
	if got.Tags["k"] != "v" {
		t.Errorf("Tags = %v, want k=v", got.Tags)
	}
}

func ptrInt(i int) *int { return &i }
