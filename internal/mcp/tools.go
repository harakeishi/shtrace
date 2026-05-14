package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/harakeishi/shtrace/internal/storage"
)

// --- get_session -------------------------------------------------------

type getSessionArgs struct {
	SessionID string `json:"session_id"`
}

type sessionResult struct {
	Session storage.Session `json:"session"`
	Spans   []storage.Span  `json:"spans"`
}

func (s *Server) toolGetSession(ctx context.Context, raw json.RawMessage) (any, error) {
	var args getSessionArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("get_session: invalid args: %w", err)
	}
	if args.SessionID == "" {
		return nil, errors.New("get_session: session_id is required")
	}

	sess, err := s.store.GetSession(ctx, args.SessionID)
	if err != nil {
		if errors.Is(err, storage.ErrSessionNotFound) {
			return nil, fmt.Errorf("session %q not found", args.SessionID)
		}
		return nil, fmt.Errorf("get_session: %w", err)
	}

	spans, err := s.store.SpansForSession(ctx, args.SessionID, nil)
	if err != nil {
		return nil, fmt.Errorf("get_session: spans: %w", err)
	}

	return sessionResult{Session: sess, Spans: spans}, nil
}

// --- search_commands ----------------------------------------------------

type searchCommandsArgs struct {
	Query string `json:"query"`
	Limit int    `json:"limit"`
}

type searchCommandResult struct {
	SpanID    string `json:"span_id"`
	SessionID string `json:"session_id"`
	Snippet   string `json:"snippet"`
}

func (s *Server) toolSearchCommands(ctx context.Context, raw json.RawMessage) (any, error) {
	var args searchCommandsArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("search_commands: invalid args: %w", err)
	}
	if args.Query == "" {
		return nil, errors.New("search_commands: query is required")
	}
	if s.fts == nil {
		return nil, errors.New("search_commands: FTS index not available — run 'shtrace reindex' first")
	}

	limit := args.Limit
	if limit <= 0 {
		limit = 20
	}

	results, err := s.fts.Search(ctx, args.Query, limit)
	if err != nil {
		return nil, fmt.Errorf("search_commands: %w", err)
	}

	out := make([]searchCommandResult, 0, len(results))
	for _, r := range results {
		out = append(out, searchCommandResult{
			SpanID:    r.SpanID,
			SessionID: r.SessionID,
			Snippet:   r.Snippet,
		})
	}
	return out, nil
}

// --- detect_test_runs ---------------------------------------------------

type detectTestRunsArgs struct {
	SessionID string `json:"session_id"`
}

func (s *Server) toolDetectTestRuns(ctx context.Context, raw json.RawMessage) (any, error) {
	var args detectTestRunsArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("detect_test_runs: invalid args: %w", err)
	}
	if args.SessionID == "" {
		return nil, errors.New("detect_test_runs: session_id is required")
	}

	if _, err := s.store.GetSession(ctx, args.SessionID); err != nil {
		if errors.Is(err, storage.ErrSessionNotFound) {
			return nil, fmt.Errorf("session %q not found", args.SessionID)
		}
		return nil, fmt.Errorf("detect_test_runs: %w", err)
	}

	spans, err := s.store.SpansForSession(ctx, args.SessionID, nil)
	if err != nil {
		return nil, fmt.Errorf("detect_test_runs: spans: %w", err)
	}

	runs := DetectTestRuns(spans, s.dataDir)
	if runs == nil {
		runs = []TestRun{}
	}
	return runs, nil
}

// --- compare_runs -------------------------------------------------------

type compareRunsArgs struct {
	SessionA string `json:"session_a"`
	SessionB string `json:"session_b"`
}

// testKey uniquely identifies a test run for comparison purposes.
// We use "framework:command" as the key since there are no named test cases
// at the span level — only aggregate pass/fail counts per command.
type testKey struct{ framework, command string }

type compareEntry struct {
	Framework string `json:"framework"`
	Command   string `json:"command"`
	// Status of the run in each session. "pass", "fail", "unknown", or "absent".
	StatusA string `json:"status_a"`
	StatusB string `json:"status_b"`
	Change  string `json:"change"` // "pass→fail", "fail→pass", "unchanged", "added", "removed"
}

type compareResult struct {
	SessionA string         `json:"session_a"`
	SessionB string         `json:"session_b"`
	Entries  []compareEntry `json:"entries"`
}

func (s *Server) toolCompareRuns(ctx context.Context, raw json.RawMessage) (any, error) {
	var args compareRunsArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("compare_runs: invalid args: %w", err)
	}
	if args.SessionA == "" || args.SessionB == "" {
		return nil, errors.New("compare_runs: session_a and session_b are required")
	}

	runsA, err := s.detectForSession(ctx, args.SessionA)
	if err != nil {
		return nil, fmt.Errorf("compare_runs session_a: %w", err)
	}
	runsB, err := s.detectForSession(ctx, args.SessionB)
	if err != nil {
		return nil, fmt.Errorf("compare_runs session_b: %w", err)
	}

	mapA := indexRuns(runsA)
	mapB := indexRuns(runsB)

	seen := map[testKey]bool{}
	var entries []compareEntry

	for k, runA := range mapA {
		seen[k] = true
		statusA := runStatus(runA)
		if runB, ok := mapB[k]; ok {
			statusB := runStatus(runB)
			entries = append(entries, compareEntry{
				Framework: k.framework,
				Command:   k.command,
				StatusA:   statusA,
				StatusB:   statusB,
				Change:    changeLabel(statusA, statusB),
			})
		} else {
			entries = append(entries, compareEntry{
				Framework: k.framework,
				Command:   k.command,
				StatusA:   statusA,
				StatusB:   "absent",
				Change:    "removed",
			})
		}
	}

	for k, runB := range mapB {
		if seen[k] {
			continue
		}
		statusB := runStatus(runB)
		entries = append(entries, compareEntry{
			Framework: k.framework,
			Command:   k.command,
			StatusA:   "absent",
			StatusB:   statusB,
			Change:    "added",
		})
	}

	return compareResult{
		SessionA: args.SessionA,
		SessionB: args.SessionB,
		Entries:  entries,
	}, nil
}

func (s *Server) detectForSession(ctx context.Context, sessionID string) ([]TestRun, error) {
	if _, err := s.store.GetSession(ctx, sessionID); err != nil {
		if errors.Is(err, storage.ErrSessionNotFound) {
			return nil, fmt.Errorf("session %q not found", sessionID)
		}
		return nil, err
	}
	spans, err := s.store.SpansForSession(ctx, sessionID, nil)
	if err != nil {
		return nil, err
	}
	return DetectTestRuns(spans, s.dataDir), nil
}

func indexRuns(runs []TestRun) map[testKey]TestRun {
	m := make(map[testKey]TestRun, len(runs))
	for _, r := range runs {
		// Normalise the command to just the base command so that minor argv
		// differences (e.g. file paths) don't prevent matching across sessions.
		k := testKey{
			framework: r.Framework,
			command:   normaliseCommand(r.Command),
		}
		m[k] = r
	}
	return m
}

func normaliseCommand(cmd string) string {
	// Keep only the first token (the binary name) so "go test ./..." and
	// "go test ./pkg" both collapse to "go".
	parts := strings.Fields(cmd)
	if len(parts) == 0 {
		return cmd
	}
	return parts[0]
}

func runStatus(r TestRun) string {
	if r.ExitCode != nil {
		if *r.ExitCode == 0 {
			return "pass"
		}
		return "fail"
	}
	if r.Failed != nil && *r.Failed > 0 {
		return "fail"
	}
	if r.Passed != nil {
		return "pass"
	}
	return "unknown"
}

func changeLabel(a, b string) string {
	if a == b {
		return "unchanged"
	}
	return a + "→" + b
}
