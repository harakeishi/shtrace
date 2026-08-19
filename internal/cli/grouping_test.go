package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/harakeishi/shtrace/internal/storage"
)

func deleteSessionForTest(t *testing.T, sessionID string) {
	t.Helper()
	dataDir, err := storage.ResolveDataDir(envMap(), runtime.GOOS)
	if err != nil {
		t.Fatalf("resolve data dir: %v", err)
	}
	store, err := storage.Open(dataDir + "/sessions.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = store.Close() }()
	if err := store.DeleteSession(context.Background(), sessionID); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	_ = os.RemoveAll(storage.OutputPath(dataDir, sessionID, ""))
}

// listSessionIDs returns recorded session ids, newest first.
func listSessionIDs(t *testing.T) []string {
	t.Helper()
	var so, se bytes.Buffer
	if code := Run(context.Background(), []string{"shtrace", "ls", "--json"}, &so, &se); code != 0 {
		t.Fatalf("ls --json exit = %d: %s", code, se.String())
	}
	var entries []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(so.Bytes(), &entries); err != nil {
		t.Fatalf("decode ls: %v (raw=%q)", err, so.String())
	}
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.ID)
	}
	return ids
}

// Two runs from the same parent process (the test binary) must land in one
// session — this is what makes "one agent instance = one session" work for
// agents that spawn a throwaway shell per command.
func TestRun_GroupsConsecutiveRunsByParentProcess(t *testing.T) {
	runHarness(t, "shtrace", "--", "sh", "-c", "echo first")

	var so, se bytes.Buffer
	if code := Run(context.Background(), []string{"shtrace", "--", "sh", "-c", "echo second"}, &so, &se); code != 0 {
		t.Fatalf("second run exit = %d: %s", code, se.String())
	}

	ids := listSessionIDs(t)
	if len(ids) != 1 {
		t.Fatalf("got %d sessions, want 1 (both runs share the parent process): %v", len(ids), ids)
	}

	var showOut, showErr bytes.Buffer
	if code := Run(context.Background(), []string{"shtrace", "show", ids[0]}, &showOut, &showErr); code != 0 {
		t.Fatalf("show exit = %d: %s", code, showErr.String())
	}
	out := showOut.String()
	if strings.Count(out, "== span") != 2 {
		t.Fatalf("expected 2 spans in the shared session, got:\n%s", out)
	}
	if !strings.Contains(out, "first") || !strings.Contains(out, "second") {
		t.Fatalf("both commands should be recorded, got:\n%s", out)
	}
}

// Distinct parents must not be merged: two agent instances running side by
// side each get their own session.
func TestRun_DoesNotGroupAcrossDifferentParents(t *testing.T) {
	runHarness(t, "shtrace", "--", "sh", "-c", "echo first")

	// A different parent key stands in for a second agent process, since a
	// test cannot change its own ppid.
	dataDir, err := storage.ResolveDataDir(envMap(), runtime.GOOS)
	if err != nil {
		t.Fatalf("resolve data dir: %v", err)
	}
	store, err := storage.Open(dataDir + "/sessions.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = store.Close() }()

	ctx := context.Background()
	if err := store.InsertSession(ctx, storage.Session{ID: "other-agent-session", StartedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	claimed, err := store.ClaimSessionForParent(ctx, "some-other-parent:123", "other-agent-session")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed != "other-agent-session" {
		t.Fatalf("a different parent joined an existing session: %q", claimed)
	}

	ids := listSessionIDs(t)
	if len(ids) != 2 {
		t.Fatalf("got %d sessions, want 2 (one per parent): %v", len(ids), ids)
	}
}

// An explicit SHTRACE_SESSION_ID must still win, so nested runs and CI keep
// their existing behaviour.
func TestRun_ExplicitSessionIDTakesPrecedenceOverParentGrouping(t *testing.T) {
	runHarness(t, "shtrace", "--", "sh", "-c", "echo grouped")

	before := listSessionIDs(t)
	if len(before) != 1 {
		t.Fatalf("setup: got %d sessions, want 1: %v", len(before), before)
	}

	t.Setenv("SHTRACE_SESSION_ID", "explicit-session-id")
	var so, se bytes.Buffer
	if code := Run(context.Background(), []string{"shtrace", "--", "sh", "-c", "echo explicit"}, &so, &se); code != 0 {
		t.Fatalf("explicit run exit = %d: %s", code, se.String())
	}

	after := listSessionIDs(t)
	found := false
	for _, id := range after {
		if id == "explicit-session-id" {
			found = true
		}
	}
	if !found {
		t.Fatalf("explicit session id was not used: %v", after)
	}
	if len(after) != 2 {
		t.Fatalf("got %d sessions, want 2 (grouped + explicit): %v", len(after), after)
	}
}

// Grouping must not reuse a session that GC removed.
func TestRun_StartsFreshSessionWhenGroupedSessionWasDeleted(t *testing.T) {
	runHarness(t, "shtrace", "--", "sh", "-c", "echo first")
	ids := listSessionIDs(t)
	if len(ids) != 1 {
		t.Fatalf("setup: got %d sessions: %v", len(ids), ids)
	}

	var so, se bytes.Buffer
	if code := Run(context.Background(), []string{"shtrace", "gc"}, &so, &se); code != 0 {
		t.Fatalf("gc exit = %d: %s", code, se.String())
	}
	// Force removal regardless of TTL by deleting through the store directly.
	deleteSessionForTest(t, ids[0])

	var so2, se2 bytes.Buffer
	if code := Run(context.Background(), []string{"shtrace", "--", "sh", "-c", "echo second"}, &so2, &se2); code != 0 {
		t.Fatalf("second run exit = %d: %s", code, se2.String())
	}

	after := listSessionIDs(t)
	if len(after) != 1 {
		t.Fatalf("got %d sessions, want 1 fresh session: %v", len(after), after)
	}
	if after[0] == ids[0] {
		t.Fatalf("reused the deleted session id %q", ids[0])
	}
}
