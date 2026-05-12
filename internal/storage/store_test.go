package storage

import (
	"context"
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
	defer s.Close()

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

	sessions, err := s.ListSessions(context.Background(), 10)
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

	spans, err := s.SpansForSession(context.Background(), "sess-1")
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

func TestStore_ListSessions_OrderedByStartedDesc(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "sessions.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
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

	got, err := s.ListSessions(context.Background(), 10)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(got) != 2 || got[0].ID != "newer" || got[1].ID != "older" {
		t.Fatalf("ListSessions order = %+v, want newest first", got)
	}
}

func ptrInt(i int) *int { return &i }
