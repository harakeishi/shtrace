package storage

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func openTestStoreForParentMap(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return store
}

func seedSession(t *testing.T, store *Store, id string) {
	t.Helper()
	if err := store.InsertSession(context.Background(), Session{
		ID:        id,
		StartedAt: time.Now().UTC(),
		Tags:      map[string]string{},
	}); err != nil {
		t.Fatalf("insert session %s: %v", id, err)
	}
}

func TestClaimSessionForParent_FirstCallerWins(t *testing.T) {
	ctx := context.Background()
	store := openTestStoreForParentMap(t)
	seedSession(t, store, "session-a")

	got, err := store.ClaimSessionForParent(ctx, "parent-1", "session-a")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if got != "session-a" {
		t.Fatalf("claim returned %q, want session-a", got)
	}
}

func TestClaimSessionForParent_SecondCallerJoinsWinner(t *testing.T) {
	ctx := context.Background()
	store := openTestStoreForParentMap(t)
	seedSession(t, store, "session-a")
	seedSession(t, store, "session-b")

	if _, err := store.ClaimSessionForParent(ctx, "parent-1", "session-a"); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	// A second process under the same parent must join the existing session,
	// not overwrite the mapping with its own candidate id.
	got, err := store.ClaimSessionForParent(ctx, "parent-1", "session-b")
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if got != "session-a" {
		t.Fatalf("second claim returned %q, want the already-claimed session-a", got)
	}
}

func TestClaimSessionForParent_DistinctParentsAreIndependent(t *testing.T) {
	ctx := context.Background()
	store := openTestStoreForParentMap(t)
	seedSession(t, store, "session-a")
	seedSession(t, store, "session-b")

	if _, err := store.ClaimSessionForParent(ctx, "parent-1", "session-a"); err != nil {
		t.Fatalf("claim parent-1: %v", err)
	}
	got, err := store.ClaimSessionForParent(ctx, "parent-2", "session-b")
	if err != nil {
		t.Fatalf("claim parent-2: %v", err)
	}
	if got != "session-b" {
		t.Fatalf("parent-2 got %q, want its own session-b", got)
	}
}

// Concurrent claims model two agent commands launched at once under the same
// parent: exactly one candidate must win and both callers must agree.
func TestClaimSessionForParent_ConcurrentClaimsAgree(t *testing.T) {
	ctx := context.Background()
	store := openTestStoreForParentMap(t)

	const n = 8
	for i := 0; i < n; i++ {
		seedSession(t, store, "candidate-"+string(rune('a'+i)))
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results []string
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got, err := store.ClaimSessionForParent(ctx, "shared-parent", "candidate-"+string(rune('a'+i)))
			if err != nil {
				t.Errorf("claim %d: %v", i, err)
				return
			}
			mu.Lock()
			results = append(results, got)
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	if len(results) != n {
		t.Fatalf("got %d results, want %d", len(results), n)
	}
	for _, got := range results {
		if got != results[0] {
			t.Fatalf("claims disagreed: %v", results)
		}
	}
}

// A mapping whose session was deleted must not resurrect the dead id.
func TestClaimSessionForParent_IgnoresMappingToDeletedSession(t *testing.T) {
	ctx := context.Background()
	store := openTestStoreForParentMap(t)
	seedSession(t, store, "session-a")

	if _, err := store.ClaimSessionForParent(ctx, "parent-1", "session-a"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := store.DeleteSession(ctx, "session-a"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	seedSession(t, store, "session-b")
	got, err := store.ClaimSessionForParent(ctx, "parent-1", "session-b")
	if err != nil {
		t.Fatalf("claim after delete: %v", err)
	}
	if got != "session-b" {
		t.Fatalf("claim returned %q, want session-b (stale mapping must not win)", got)
	}
}

func TestDeleteSession_RemovesParentMapping(t *testing.T) {
	ctx := context.Background()
	store := openTestStoreForParentMap(t)
	seedSession(t, store, "session-a")
	if _, err := store.ClaimSessionForParent(ctx, "parent-1", "session-a"); err != nil {
		t.Fatalf("claim: %v", err)
	}

	if err := store.DeleteSession(ctx, "session-a"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	var count int
	if err := store.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM parent_sessions WHERE session_id = ?`, "session-a").Scan(&count); err != nil {
		t.Fatalf("count mappings: %v", err)
	}
	if count != 0 {
		t.Fatalf("mapping rows remaining after delete = %d, want 0", count)
	}
}
