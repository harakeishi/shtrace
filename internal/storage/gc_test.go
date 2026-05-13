package storage_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/harakeishi/shtrace/internal/storage"
)

// setupGCStore opens a store and runs Migrate in a temp dir.
func setupGCStore(t *testing.T) (store *storage.Store, dataDir string) {
	t.Helper()
	dataDir = t.TempDir()
	s, err := storage.Open(filepath.Join(dataDir, "sessions.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return s, dataDir
}

// writeOutputFile creates a fake output log file for a session/span pair and
// returns the number of bytes written.
func writeOutputFile(t *testing.T, dataDir, sessionID, spanID, content string) int {
	t.Helper()
	dir := filepath.Join(dataDir, "outputs", sessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	p := filepath.Join(dir, spanID+".log")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}
	return len(content)
}

func insertTestSession(t *testing.T, store *storage.Store, id string, startedAt time.Time) {
	t.Helper()
	ended := startedAt.Add(time.Minute)
	err := store.InsertSession(context.Background(), storage.Session{
		ID:        id,
		StartedAt: startedAt,
		EndedAt:   &ended,
		Tags:      map[string]string{},
	})
	if err != nil {
		t.Fatalf("insert session %s: %v", id, err)
	}
}

func TestRunGC_TTL(t *testing.T) {
	store, dataDir := setupGCStore(t)
	ctx := context.Background()

	now := time.Now().UTC()
	old := now.AddDate(0, 0, -40) // 40 days ago — older than default 30-day TTL
	fresh := now.AddDate(0, 0, -5) // 5 days ago — within TTL

	insertTestSession(t, store, "old-session", old)
	insertTestSession(t, store, "fresh-session", fresh)
	writeOutputFile(t, dataDir, "old-session", "span1", "old output data")
	writeOutputFile(t, dataDir, "fresh-session", "span2", "fresh output data")

	cfg := storage.GCConfig{TTLDays: 30, MaxSizeBytes: defaultMaxForTest}
	result, err := storage.RunGC(ctx, store, nil, dataDir, cfg, false)
	if err != nil {
		t.Fatalf("RunGC: %v", err)
	}

	if result.SessionsRemoved != 1 {
		t.Errorf("want 1 session removed, got %d", result.SessionsRemoved)
	}
	if len(result.Sessions) != 1 || result.Sessions[0] != "old-session" {
		t.Errorf("want [old-session], got %v", result.Sessions)
	}
	if result.BytesReclaimed == 0 {
		t.Error("want non-zero BytesReclaimed")
	}

	// old-session output dir should be gone
	if _, err := os.Stat(filepath.Join(dataDir, "outputs", "old-session")); !os.IsNotExist(err) {
		t.Error("expected old-session output dir to be deleted")
	}
	// fresh-session output dir must survive
	if _, err := os.Stat(filepath.Join(dataDir, "outputs", "fresh-session")); err != nil {
		t.Errorf("fresh-session output dir missing: %v", err)
	}
}

func TestRunGC_DryRun(t *testing.T) {
	store, dataDir := setupGCStore(t)
	ctx := context.Background()

	old := time.Now().UTC().AddDate(0, 0, -40)
	insertTestSession(t, store, "old-session", old)
	writeOutputFile(t, dataDir, "old-session", "span1", "some data")

	cfg := storage.GCConfig{TTLDays: 30, MaxSizeBytes: defaultMaxForTest}
	result, err := storage.RunGC(ctx, store, nil, dataDir, cfg, true /* dryRun */)
	if err != nil {
		t.Fatalf("RunGC dry-run: %v", err)
	}

	if result.SessionsRemoved != 1 {
		t.Errorf("dry-run: want 1 reported, got %d", result.SessionsRemoved)
	}
	// Nothing should be deleted in dry-run.
	if _, err := os.Stat(filepath.Join(dataDir, "outputs", "old-session")); err != nil {
		t.Error("dry-run: output dir should still exist")
	}
	// DB row should still be there.
	sessions, err := store.ListSessions(ctx, 10, nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(sessions) != 1 {
		t.Errorf("dry-run: want session still in DB, got %d sessions", len(sessions))
	}
}

func TestRunGC_SizeCap(t *testing.T) {
	store, dataDir := setupGCStore(t)
	ctx := context.Background()

	now := time.Now().UTC()
	// Both sessions are fresh (within TTL), but together exceed the size cap.
	insertTestSession(t, store, "sess-a", now.Add(-2*time.Hour))
	insertTestSession(t, store, "sess-b", now.Add(-1*time.Hour))

	_ = writeOutputFile(t, dataDir, "sess-a", "spanA", string(make([]byte, 600)))
	nB := writeOutputFile(t, dataDir, "sess-b", "spanB", string(make([]byte, 600)))

	// Cap at nB+1 bytes so sess-a (oldest) must be removed to fit, with a
	// non-zero margin that avoids relying on exact boundary equality.
	cfg := storage.GCConfig{TTLDays: 365, MaxSizeBytes: int64(nB) + 1}
	result, err := storage.RunGC(ctx, store, nil, dataDir, cfg, false)
	if err != nil {
		t.Fatalf("RunGC size-cap: %v", err)
	}

	if result.SessionsRemoved != 1 {
		t.Errorf("size-cap: want 1 removed, got %d", result.SessionsRemoved)
	}
	if len(result.Sessions) != 1 || result.Sessions[0] != "sess-a" {
		t.Errorf("size-cap: want [sess-a] removed, got %v", result.Sessions)
	}
	// sess-b must survive.
	if _, err := os.Stat(filepath.Join(dataDir, "outputs", "sess-b")); err != nil {
		t.Errorf("sess-b output dir missing: %v", err)
	}
}

func TestRunGC_NothingToRemove(t *testing.T) {
	store, dataDir := setupGCStore(t)
	ctx := context.Background()

	fresh := time.Now().UTC().AddDate(0, 0, -1)
	insertTestSession(t, store, "fresh", fresh)
	writeOutputFile(t, dataDir, "fresh", "span1", "data")

	cfg := storage.GCConfig{TTLDays: 30, MaxSizeBytes: defaultMaxForTest}
	result, err := storage.RunGC(ctx, store, nil, dataDir, cfg, false)
	if err != nil {
		t.Fatalf("RunGC: %v", err)
	}
	if result.SessionsRemoved != 0 {
		t.Errorf("want 0 removed, got %d", result.SessionsRemoved)
	}
}

func TestGCConfigFromEnv(t *testing.T) {
	cfg := storage.GCConfigFromEnv(map[string]string{
		"SHTRACE_TTL_DAYS":      "7",
		"SHTRACE_MAX_SIZE_BYTES": "1048576",
	})
	if cfg.EffectiveTTLDays() != 7 {
		t.Errorf("want TTLDays=7, got %d", cfg.EffectiveTTLDays())
	}
	if cfg.EffectiveMaxBytes() != 1048576 {
		t.Errorf("want MaxBytes=1048576, got %d", cfg.EffectiveMaxBytes())
	}
}

func TestGCConfigFromEnv_Defaults(t *testing.T) {
	cfg := storage.GCConfigFromEnv(map[string]string{})
	if cfg.EffectiveTTLDays() != 30 {
		t.Errorf("want default TTLDays=30, got %d", cfg.EffectiveTTLDays())
	}
	if cfg.EffectiveMaxBytes() != 10*1024*1024*1024 {
		t.Errorf("want default MaxBytes=10GB, got %d", cfg.EffectiveMaxBytes())
	}
}

// defaultMaxForTest is a large cap that the test data will never exceed.
const defaultMaxForTest = 10 * 1024 * 1024 * 1024
