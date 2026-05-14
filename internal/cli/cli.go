// Package cli is the entry point for the `shtrace` binary. It wires up the
// subcommands (run, ls, show, ...) and is testable because Run accepts argv,
// stdout, and stderr explicitly — no os.Args / os.Exit at this layer.
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/mattn/go-isatty"

	"github.com/harakeishi/shtrace/internal/mcp"
	"github.com/harakeishi/shtrace/internal/report"
	"github.com/harakeishi/shtrace/internal/runner"
	"github.com/harakeishi/shtrace/internal/secret"
	"github.com/harakeishi/shtrace/internal/session"
	"github.com/harakeishi/shtrace/internal/storage"
)

// Run dispatches to the requested subcommand. argv follows the os.Args
// convention (argv[0] is the program name).
func Run(ctx context.Context, argv []string, stdout, stderr io.Writer) int {
	if len(argv) < 2 {
		_, _ = fmt.Fprintln(stderr, "usage: shtrace [--mode pipe|pty] <subcommand> [args...]")
		_, _ = fmt.Fprintln(stderr, "subcommands: run (default), ls, show, search, reindex, gc, report, session, shell-init, mcp")
		return 2
	}

	// Parse optional --mode flag before dispatching subcommands.
	// Re-check length after parsing because --mode consumes two tokens and
	// `shtrace --mode pty` (no subcommand) would otherwise panic on argv[1].
	mode, argv, modeErr := parseMode(argv)
	if modeErr != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: %v\n", modeErr)
		return 2
	}
	if len(argv) < 2 {
		_, _ = fmt.Fprintln(stderr, "usage: shtrace [--mode pipe|pty] <subcommand> [args...]")
		_, _ = fmt.Fprintln(stderr, "subcommands: run (default), ls, show, search, reindex, gc, report, session, shell-init, mcp")
		return 2
	}

	switch argv[1] {
	case "mcp":
		return runMCP(ctx, argv[2:], os.Stdin, stdout, stderr)
	case "ls":
		return runLs(ctx, argv[2:], stdout, stderr)
	case "show":
		return runShow(ctx, argv[2:], stdout, stderr)
	case "search":
		return runSearch(ctx, argv[2:], stdout, stderr)
	case "reindex":
		return runReindex(ctx, argv[2:], stdout, stderr)
	case "gc":
		return runGC(ctx, argv[2:], stdout, stderr)
	case "report":
		return runReport(ctx, argv[2:], stdout, stderr)
	case "session":
		return runSession(ctx, argv[2:], stdout, stderr)
	case "shell-init":
		return runShellInit(argv[2:], stdout, stderr)
	case "--":
		return runWrapped(ctx, mode, argv[2:], stdout, stderr)
	default:
		// Treat `shtrace cmd args...` the same as `shtrace -- cmd args...`
		return runWrapped(ctx, mode, argv[1:], stdout, stderr)
	}
}

// runWrapped executes `cmd args...`, records stdout/stderr, and persists span
// metadata. This is the core MVP path.
func runWrapped(ctx context.Context, mode string, cmdArgs []string, stdout, stderr io.Writer) int {
	if len(cmdArgs) == 0 {
		_, _ = fmt.Fprintln(stderr, "shtrace: no command to run")
		return 2
	}

	env := envMap()
	dataDir, err := storage.ResolveDataDir(env, runtime.GOOS)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: %v\n", err)
		return 1
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: mkdir data dir: %v\n", err)
		return 1
	}

	sessCtx, err := session.FromEnv(env, session.DefaultIDGenerator())
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: %v\n", err)
		return 1
	}

	store, err := storage.Open(dataDir + "/sessions.db")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: open store: %v\n", err)
		return 1
	}
	defer func() { _ = store.Close() }()
	if err := store.Migrate(ctx); err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: migrate: %v\n", err)
		return 1
	}

	startedAt := time.Now().UTC()
	if sessCtx.IsRoot {
		if err := store.InsertSession(ctx, storage.Session{
			ID:        sessCtx.SessionID,
			StartedAt: startedAt,
			Tags:      sessCtx.Tags,
		}); err != nil {
			_, _ = fmt.Fprintf(stderr, "shtrace: insert session: %v\n", err)
			return 1
		}
	} else {
		// Child invocation: parent may have written to a different on-disk
		// DB (e.g., different SHTRACE_DATA_DIR), so guarantee the session
		// row exists locally before any FK-constrained span insert.
		if err := store.EnsureSession(ctx, storage.Session{
			ID:        sessCtx.SessionID,
			StartedAt: startedAt,
			Tags:      sessCtx.Tags,
		}); err != nil {
			_, _ = fmt.Fprintf(stderr, "shtrace: ensure session: %v\n", err)
			return 1
		}
	}

	logPath := storage.OutputPath(dataDir, sessCtx.SessionID, sessCtx.SpanID)
	if err := os.MkdirAll(parentDir(logPath), 0o755); err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: mkdir outputs: %v\n", err)
		return 1
	}
	logFile, err := os.Create(logPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: open log: %v\n", err)
		return 1
	}
	defer func() { _ = logFile.Close() }()

	cwd, cwdErr := os.Getwd()
	if cwdErr != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: warning: could not determine working directory: %v\n", cwdErr)
	}
	childEnv := append(os.Environ(), envMapToSlice(sessCtx.ChildEnv())...)

	jsonl := storage.NewJSONLWriter(logFile, nil)

	// Scan env vars for high-entropy values and extend the masker so those
	// values are also redacted from I/O streams (not just env display).
	// Reuse the env map already built above to avoid a second os.Environ() call.
	_, envSecrets := secret.MaskEnv(env)
	masker, err := secret.NewMaskerWithLiterals(nil, envSecrets)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: build masker: %v\n", err)
		return 1
	}

	// Resolve the effective mode. isatty.IsTerminal is evaluated once here so
	// the check is not duplicated between auto-detection and explicit --mode.
	//
	// PTY requires stdout to be a real terminal. If it is not (e.g. redirected
	// to a file, pipe, or non-*os.File writer), fall back to pipe so output is
	// not silently lost and ioctls don't fail mid-run.
	var ptyTty *os.File
	if f, ok := stdout.(*os.File); ok && isatty.IsTerminal(f.Fd()) {
		ptyTty = f
	}
	switch {
	case mode == "":
		if ptyTty != nil {
			mode = "pty"
		} else {
			mode = "pipe"
		}
	case mode == "pty" && ptyTty == nil:
		_, _ = fmt.Fprintf(stderr, "shtrace: warning: --mode pty requested but stdout is not a TTY; falling back to pipe\n")
		mode = "pipe"
	}

	var res runner.Result
	var runErr error
	switch mode {
	case "pty":
		res, runErr = runner.RunPTY(ctx, runner.PTYOptions{
			Argv:   cmdArgs,
			Env:    childEnv,
			Cwd:    cwd,
			Writer: jsonl,
			Tty:    ptyTty,
			Stderr: stderr,
			Masker: masker,
		})
	default: // "pipe"
		res, runErr = runner.RunPipe(ctx, runner.PipeOptions{
			Argv:   cmdArgs,
			Env:    childEnv,
			Cwd:    cwd,
			Writer: jsonl,
			Stdout: stdout,
			Stderr: stderr,
			Masker: masker,
		})
	}
	if runErr != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: run: %v\n", runErr)
		return 1
	}

	endedAt := time.Now().UTC()
	exitCode := res.ExitCode
	if err := store.InsertSpan(ctx, storage.Span{
		ID:           sessCtx.SpanID,
		SessionID:    sessCtx.SessionID,
		ParentSpanID: sessCtx.ParentSpanID,
		Command:      cmdArgs[0],
		Argv:         masker.MaskArgv(cmdArgs),
		Cwd:          cwd,
		Mode:         mode,
		StartedAt:    startedAt,
		EndedAt:      endedAt,
		ExitCode:     &exitCode,
	}); err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: insert span: %v\n", err)
		return 1
	}
	if sessCtx.IsRoot {
		ended := endedAt
		// InsertSession uses ON CONFLICT DO UPDATE (UPSERT), so calling it a
		// second time for the same session ID is safe: it updates ended_at
		// without violating the UNIQUE constraint.
		if err := store.InsertSession(ctx, storage.Session{
			ID:        sessCtx.SessionID,
			StartedAt: startedAt,
			EndedAt:   &ended,
			Tags:      sessCtx.Tags,
		}); err != nil {
			// The wrapped command's exit code still propagates — failing
			// the whole invocation just because we couldn't stamp ended_at
			// would be worse than reporting the issue on stderr.
			_, _ = fmt.Fprintf(stderr, "shtrace: finalize session: %v\n", err)
		}
	}

	// Best-effort FTS indexing: errors here must not affect the wrapped
	// command's exit code.
	//
	// Sync the log file first so that IndexSpan's os.Open sees all written
	// data even though logFile is still open (closed by the deferred Close).
	// If Sync fails, skip FTS indexing entirely to avoid indexing truncated
	// content that would silently corrupt the index.
	if syncErr := logFile.Sync(); syncErr != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: log sync (index will be skipped): %v\n", syncErr)
	} else if fts, ftsErr := storage.OpenFTS(storage.FTSPath(dataDir)); ftsErr != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: fts open (index will be skipped): %v\n", ftsErr)
	} else {
		defer func() { _ = fts.Close() }()
		if migrErr := fts.MigrateFTS(ctx); migrErr != nil {
			_, _ = fmt.Fprintf(stderr, "shtrace: fts migrate: %v\n", migrErr)
		} else if indexErr := fts.IndexSpan(ctx, sessCtx.SpanID, sessCtx.SessionID, logPath); indexErr != nil {
			_, _ = fmt.Fprintf(stderr, "shtrace: fts index: %v\n", indexErr)
		}
	}

	return exitCode
}

func runLs(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	jsonOut := false
	for _, a := range args {
		if a == "--json" {
			jsonOut = true
		}
	}

	env := envMap()
	dataDir, err := storage.ResolveDataDir(env, runtime.GOOS)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: %v\n", err)
		return 1
	}
	store, err := storage.Open(dataDir + "/sessions.db")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: open store: %v\n", err)
		return 1
	}
	defer func() { _ = store.Close() }()
	if err := store.Migrate(ctx); err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: migrate: %v\n", err)
		return 1
	}

	warn := func(e error) { _, _ = fmt.Fprintf(stderr, "shtrace: warning: %v\n", e) }
	sessions, err := store.ListSessions(ctx, 50, warn)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: list: %v\n", err)
		return 1
	}

	if jsonOut {
		type entry struct {
			ID        string            `json:"id"`
			StartedAt string            `json:"started_at"`
			Tags      map[string]string `json:"tags"`
		}
		entries := make([]entry, 0, len(sessions))
		for _, s := range sessions {
			entries = append(entries, entry{ID: s.ID, StartedAt: s.StartedAt.Format(time.RFC3339Nano), Tags: s.Tags})
		}
		b, _ := json.Marshal(entries)
		_, _ = fmt.Fprintln(stdout, string(b))
		return 0
	}

	for _, s := range sessions {
		spans, err := store.SpansForSession(ctx, s.ID, warn)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "shtrace: spans for %s: %v\n", s.ID, err)
		}
		cmdSummary := ""
		if len(spans) > 0 {
			cmdSummary = spans[0].Command
		}
		_, _ = fmt.Fprintf(stdout, "%s  %s  spans=%d  %s\n", s.StartedAt.Format(time.RFC3339), s.ID, len(spans), cmdSummary)
	}
	return 0
}

func runShow(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, "usage: shtrace show <session_id>")
		return 2
	}
	sessionID := args[0]

	env := envMap()
	dataDir, err := storage.ResolveDataDir(env, runtime.GOOS)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: %v\n", err)
		return 1
	}
	store, err := storage.Open(dataDir + "/sessions.db")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: open store: %v\n", err)
		return 1
	}
	defer func() { _ = store.Close() }()
	if err := store.Migrate(ctx); err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: migrate: %v\n", err)
		return 1
	}

	warn := func(e error) { _, _ = fmt.Fprintf(stderr, "shtrace: warning: %v\n", e) }
	spans, err := store.SpansForSession(ctx, sessionID, warn)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: spans: %v\n", err)
		return 1
	}
	if len(spans) == 0 {
		_, _ = fmt.Fprintf(stderr, "shtrace: no spans for session %s\n", sessionID)
		return 1
	}

	for _, sp := range spans {
		_, _ = fmt.Fprintf(stdout, "== span %s  cmd=%s  exit=%v  mode=%s\n", sp.ID, sp.Command, derefInt(sp.ExitCode), sp.Mode)
		logPath := storage.OutputPath(dataDir, sessionID, sp.ID)
		b, err := os.ReadFile(logPath)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "shtrace: read log %s: %v\n", logPath, err)
			continue
		}
		// Render JSON Lines, routing each chunk to stdout or stderr based on
		// its recorded stream label so callers can pipe show's output the
		// same way they would the original command's.
		corrupt := 0
		for i, line := range splitLines(b) {
			if len(line) == 0 {
				continue
			}
			var c storage.Chunk
			if err := json.Unmarshal(line, &c); err != nil {
				corrupt++
				_, _ = fmt.Fprintf(stderr, "shtrace: skipped corrupt line %d in %s: %v\n", i+1, logPath, err)
				continue
			}
			switch storage.Stream(c.Stream) {
			case storage.StreamStderr:
				_, _ = fmt.Fprint(stderr, c.Data)
			default:
				// stdout, pty (mode A merged), or any future label
				_, _ = fmt.Fprint(stdout, c.Data)
			}
		}
		_, _ = fmt.Fprintln(stdout)
		if corrupt > 0 {
			_, _ = fmt.Fprintf(stderr, "shtrace: %d corrupt line(s) skipped in %s\n", corrupt, logPath)
		}
	}
	return 0
}

// runSearch performs a full-text search over recorded span outputs.
func runSearch(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, "usage: shtrace search <query>")
		return 2
	}
	query := strings.Join(args, " ")

	env := envMap()
	dataDir, err := storage.ResolveDataDir(env, runtime.GOOS)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: %v\n", err)
		return 1
	}

	ftsPath := storage.FTSPath(dataDir)
	if _, statErr := os.Stat(ftsPath); statErr != nil {
		if os.IsNotExist(statErr) {
			_, _ = fmt.Fprintln(stderr, "shtrace: no search index found — run 'shtrace reindex' to build it from existing sessions")
		} else {
			_, _ = fmt.Fprintf(stderr, "shtrace: stat fts index: %v\n", statErr)
		}
		return 1
	}

	fts, err := storage.OpenFTS(ftsPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: open fts: %v\n", err)
		return 1
	}
	defer func() { _ = fts.Close() }()
	if err := fts.MigrateFTS(ctx); err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: fts migrate: %v\n", err)
		return 1
	}

	results, err := fts.Search(ctx, query, 20)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: search: %v\n", err)
		return 1
	}
	if len(results) == 0 {
		_, _ = fmt.Fprintln(stdout, "no results")
		return 0
	}
	for _, r := range results {
		_, _ = fmt.Fprintf(stdout, "session=%s span=%s\n  %s\n", r.SessionID, r.SpanID, r.Snippet)
	}
	return 0
}

// runReindex rebuilds the FTS index from the raw JSON Lines files on disk.
func runReindex(ctx context.Context, _ []string, stdout, stderr io.Writer) int {
	env := envMap()
	dataDir, err := storage.ResolveDataDir(env, runtime.GOOS)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: %v\n", err)
		return 1
	}

	store, err := storage.Open(dataDir + "/sessions.db")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: open store: %v\n", err)
		return 1
	}
	defer func() { _ = store.Close() }()
	if err := store.Migrate(ctx); err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: migrate: %v\n", err)
		return 1
	}

	fts, err := storage.OpenFTS(storage.FTSPath(dataDir))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: open fts: %v\n", err)
		return 1
	}
	defer func() { _ = fts.Close() }()
	if err := fts.MigrateFTS(ctx); err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: fts migrate: %v\n", err)
		return 1
	}

	_, _ = fmt.Fprintln(stdout, "reindexing...")
	if err := storage.ReindexAll(ctx, fts, store, dataDir); err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: reindex: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintln(stdout, "done")
	return 0
}

// runGC removes sessions that exceed the configured TTL or push total output
// storage over the size cap. Reads SHTRACE_TTL_DAYS and SHTRACE_MAX_SIZE_BYTES
// from the environment. Supports --dry-run to preview without deleting.
func runGC(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	dryRun := false
	for _, a := range args {
		if a == "--dry-run" {
			dryRun = true
		}
	}

	env := envMap()
	dataDir, err := storage.ResolveDataDir(env, runtime.GOOS)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: %v\n", err)
		return 1
	}

	store, err := storage.Open(dataDir + "/sessions.db")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: open store: %v\n", err)
		return 1
	}
	defer func() { _ = store.Close() }()
	if err := store.Migrate(ctx); err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: migrate: %v\n", err)
		return 1
	}

	// Attach FTS index when it exists; skip quietly when it doesn't.
	var fts *storage.FTSStore
	ftsPath := storage.FTSPath(dataDir)
	if _, statErr := os.Stat(ftsPath); statErr == nil {
		fts, err = storage.OpenFTS(ftsPath)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "shtrace: open fts (index cleanup skipped): %v\n", err)
		} else {
			defer func() { _ = fts.Close() }()
			if migrErr := fts.MigrateFTS(ctx); migrErr != nil {
				_, _ = fmt.Fprintf(stderr, "shtrace: fts migrate (index cleanup skipped): %v\n", migrErr)
				fts = nil
			}
		}
	}

	cfg := storage.GCConfigFromEnv(env)
	if dryRun {
		_, _ = fmt.Fprintf(stdout, "dry-run  ttl=%d days  max-size=%d bytes\n",
			cfg.EffectiveTTLDays(), cfg.EffectiveMaxBytes())
	}

	result, err := storage.RunGC(ctx, store, fts, dataDir, cfg, dryRun)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: gc: %v\n", err)
		return 1
	}

	verb := "removed"
	if dryRun {
		verb = "would remove"
	}
	_, _ = fmt.Fprintf(stdout, "%s %d session(s), %d bytes reclaimed\n",
		verb, result.SessionsRemoved, result.BytesReclaimed)
	for _, id := range result.Sessions {
		_, _ = fmt.Fprintf(stdout, "  %s\n", id)
	}
	return 0
}

// reportUsage is the single source of truth for the help line so all error
// paths agree on the recommended invocation.
const reportUsage = "usage: shtrace report (--session <id> | --latest) [--output <path>]"

// parseReportArgs is split out from runReport so the flag-validation rules
// can be unit-tested without spinning up a Store.
func parseReportArgs(args []string) (sessionID, output string, latest bool, err error) {
	// reportFlagTokens are the literal flag spellings runReport accepts.
	// takeValue uses this set to detect "flag-eats-flag" patterns where a
	// user forgets a value and the parser silently assigns the next flag
	// token as the value (e.g. `--session --output foo` would otherwise
	// produce sessionID = "--output" and a confusing positional error).
	// We check the full set rather than `--` prefix only, so single-dash
	// flags (`-o`) are also caught.
	reportFlagTokens := map[string]bool{
		"--session": true, "--output": true, "--latest": true, "-o": true,
	}
	takeValue := func(flag, raw string) (string, error) {
		if raw == "" {
			return "", fmt.Errorf("%s requires a non-empty value", flag)
		}
		if reportFlagTokens[raw] || strings.HasPrefix(raw, "--") {
			return "", fmt.Errorf("%s requires a value but got the next flag %q", flag, raw)
		}
		return raw, nil
	}

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--session":
			if i+1 >= len(args) {
				return "", "", false, fmt.Errorf("--session requires a value")
			}
			v, e := takeValue("--session", args[i+1])
			if e != nil {
				return "", "", false, e
			}
			sessionID = v
			i++
		case strings.HasPrefix(a, "--session="):
			v, e := takeValue("--session", strings.TrimPrefix(a, "--session="))
			if e != nil {
				return "", "", false, e
			}
			sessionID = v
		case a == "--output", a == "-o":
			if i+1 >= len(args) {
				return "", "", false, fmt.Errorf("%s requires a value", a)
			}
			v, e := takeValue(a, args[i+1])
			if e != nil {
				return "", "", false, e
			}
			output = v
			i++
		case strings.HasPrefix(a, "--output="):
			v, e := takeValue("--output", strings.TrimPrefix(a, "--output="))
			if e != nil {
				return "", "", false, e
			}
			output = v
		case a == "--latest":
			latest = true
		default:
			return "", "", false, fmt.Errorf("unknown report flag %q", a)
		}
	}
	if sessionID == "" && !latest {
		return "", "", false, fmt.Errorf("either --session <id> or --latest is required")
	}
	if sessionID != "" && latest {
		return "", "", false, fmt.Errorf("--session and --latest are mutually exclusive")
	}
	return sessionID, output, latest, nil
}

// runReport generates a self-contained HTML report for a single session.
//
// Usage:
//
//	shtrace report --session <id>           # write to stdout
//	shtrace report --session <id> --output report.html
//	shtrace report --latest --output report.html
//
// The output is one HTML file with inline CSS and no external assets, so it
// can be viewed with `file://` after `gh run download` (Phase 3 workflow).
func runReport(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	sessionID, output, latest, err := parseReportArgs(args)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: %v\n", err)
		_, _ = fmt.Fprintln(stderr, reportUsage)
		return 2
	}

	env := envMap()
	dataDir, err := storage.ResolveDataDir(env, runtime.GOOS)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: %v\n", err)
		return 1
	}
	store, err := storage.Open(dataDir + "/sessions.db")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: open store: %v\n", err)
		return 1
	}
	defer func() { _ = store.Close() }()
	if err := store.Migrate(ctx); err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: migrate: %v\n", err)
		return 1
	}

	warn := func(e error) { _, _ = fmt.Fprintf(stderr, "shtrace: warning: %v\n", e) }

	// When --output is omitted, stream to stdout — composes with shell
	// redirection (`shtrace report --latest > r.html`). When set, render
	// into a temp file in the destination directory and atomically rename
	// on success; on any failure the destination path is left untouched
	// (including a pre-existing file at that path).
	if output == "" {
		if _, err := report.Render(ctx, store, stdout, report.Options{
			SessionID: sessionID,
			Latest:    latest,
			DataDir:   dataDir,
			Warn:      warn,
		}); err != nil {
			_, _ = fmt.Fprintf(stderr, "shtrace: %v\n", err)
			return 1
		}
		return 0
	}

	// filepath.Dir keeps CreateTemp and Rename on the same filesystem.
	// parentDir("/x.html") returns "" → os.TempDir() at create time, which
	// produces a cross-device Rename failure on hosts where /tmp is a
	// separate mount (Docker, many CI runners).
	tmpDir := filepath.Dir(output)
	f, err := os.CreateTemp(tmpDir, ".shtrace-report-*.html")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: open output: %v\n", err)
		return 1
	}
	tmpPath := f.Name()
	// closed tracks whether we've already closed f, so the deferred
	// cleanup doesn't double-close and obscure a real error from the
	// first close. tmpPath is cleared once ownership transfers to the
	// final path via Rename, suppressing the deferred Remove.
	closed := false
	defer func() {
		if !closed {
			_ = f.Close()
		}
		if tmpPath != "" {
			_ = os.Remove(tmpPath)
		}
	}()

	resolvedID, err := report.Render(ctx, store, f, report.Options{
		SessionID: sessionID,
		Latest:    latest,
		DataDir:   dataDir,
		Warn:      warn,
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: %v\n", err)
		return 1
	}

	// Surface a Close failure (ENOSPC, EIO) so we never Rename a truncated
	// file into place. The deferred Close still runs but as a no-op
	// because we set closed=true.
	if err := f.Close(); err != nil {
		closed = true
		_, _ = fmt.Fprintf(stderr, "shtrace: close output: %v\n", err)
		return 1
	}
	closed = true

	if err := os.Rename(tmpPath, output); err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: rename output: %v\n", err)
		return 1
	}
	tmpPath = "" // suppress deferred Remove now that the file is at output

	_, _ = fmt.Fprintf(stdout, "wrote report for session %s to %s\n", resolvedID, output)
	return 0
}

func envMap() map[string]string {
	env := os.Environ()
	out := make(map[string]string, len(env))
	for _, kv := range env {
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				out[kv[:i]] = kv[i+1:]
				break
			}
		}
	}
	return out
}

func envMapToSlice(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}

func parentDir(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[:i]
		}
	}
	return "."
}

func derefInt(p *int) any {
	if p == nil {
		return "?"
	}
	return *p
}

// runSession handles the `shtrace session <verb>` subtree.
// context.Context is accepted for API consistency with other handlers; it is
// currently unused because IDGenerator.NewSessionID has no cancellation point.
func runSession(_ context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, "usage: shtrace session <verb>")
		_, _ = fmt.Fprintln(stderr, "verbs: new")
		return 2
	}
	switch args[0] {
	case "new":
		id, err := session.DefaultIDGenerator().NewSessionID()
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "shtrace: generate session id: %v\n", err)
			return 1
		}
		_, _ = fmt.Fprintln(stdout, id)
		return 0
	default:
		_, _ = fmt.Fprintf(stderr, "shtrace: unknown session verb %q\n", args[0])
		return 2
	}
}

// runShellInit outputs a shell snippet that, when eval'd in a user's rc file,
// automatically exports SHTRACE_SESSION_ID for every new terminal session.
//
// Usage:
//
//	shtrace shell-init bash
//	shtrace shell-init zsh
func runShellInit(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, "usage: shtrace shell-init <bash|zsh>")
		return 2
	}
	shell := args[0]
	switch shell {
	case "bash", "zsh":
		// Use the absolute path of the running binary so the snippet works
		// correctly even when PATH is not yet fully configured (e.g. early in
		// .bashrc) or when multiple shtrace versions coexist.
		self, err := os.Executable()
		if err != nil {
			// Rare (e.g. procfs unavailable, AppArmor restriction), but fall
			// back to the bare name so the snippet is still usable. Warn so
			// the user can diagnose if the generated snippet does not work.
			_, _ = fmt.Fprintf(stderr, "shtrace: warning: could not resolve binary path (%v); falling back to 'shtrace'\n", err)
			self = "shtrace"
		} else if strings.ContainsRune(self, '\x00') {
			// A NUL byte in a shell snippet would terminate the string early
			// in most shells. Guard defensively even though os.Executable()
			// should never return a NUL-containing path.
			_, _ = fmt.Fprintf(stderr, "shtrace: warning: binary path contains NUL byte; falling back to 'shtrace'\n")
			self = "shtrace"
		}
		// The snippet is intentionally POSIX-compatible so the same text
		// works for both bash and zsh without separate branches.
		// shellQuote wraps the path in single-quotes so that spaces and
		// special characters in the binary path do not break the snippet.
		_, _ = fmt.Fprintf(stdout, "if [ -z \"${SHTRACE_SESSION_ID:-}\" ]; then\n  export SHTRACE_SESSION_ID=\"$(%s session new)\"\nfi\n", shellQuote(self))
		return 0
	default:
		_, _ = fmt.Fprintf(stderr, "shtrace: unsupported shell %q (supported: bash, zsh)\n", shell)
		return 2
	}
}

// shellQuote wraps s in POSIX single-quotes so it can be safely embedded in a
// shell snippet even when s contains spaces or other special characters.
// Single-quote characters within s are handled via the '\''-idiom.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// parseMode extracts an optional --mode <value> (or --mode=<value>) flag from
// argv (argv[0] is the program name) and returns the mode string and the
// remaining argv. argv is returned unmodified if --mode is absent.
// Returns ("", argv, error) when the flag is malformed or an unsupported value.
func parseMode(argv []string) (mode string, rest []string, err error) {
	out := make([]string, 0, len(argv))
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		var val string
		switch {
		case arg == "--mode":
			if i+1 >= len(argv) {
				return "", argv, fmt.Errorf("--mode requires a value (pipe or pty)")
			}
			val = argv[i+1]
			i++ // skip value token
		case strings.HasPrefix(arg, "--mode="):
			val = strings.TrimPrefix(arg, "--mode=")
		default:
			out = append(out, arg)
			continue
		}
		if val != "pipe" && val != "pty" {
			return "", argv, fmt.Errorf("--mode %q is invalid: must be pipe or pty", val)
		}
		if mode != "" && mode != val {
			return "", argv, fmt.Errorf("--mode specified multiple times with conflicting values (%q and %q)", mode, val)
		}
		// Same-value duplicates (e.g. --mode pty --mode pty) are idempotent and accepted.
		mode = val
	}
	return mode, out, nil
}

// runMCP starts a stdio MCP server backed by the local shtrace data directory.
// It reads JSON-RPC 2.0 requests from r (os.Stdin in production) and writes
// responses to stdout until EOF.
func runMCP(ctx context.Context, _ []string, r io.Reader, stdout, stderr io.Writer) int {
	env := envMap()
	dataDir, err := storage.ResolveDataDir(env, runtime.GOOS)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: %v\n", err)
		return 1
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: mkdir data dir: %v\n", err)
		return 1
	}

	store, err := storage.Open(dataDir + "/sessions.db")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: open store: %v\n", err)
		return 1
	}
	defer func() { _ = store.Close() }()
	if err := store.Migrate(ctx); err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: migrate: %v\n", err)
		return 1
	}

	// FTS index is optional: if absent, search_commands returns an error but
	// the other three tools still work.
	var fts *storage.FTSStore
	ftsPath := storage.FTSPath(dataDir)
	if _, statErr := os.Stat(ftsPath); statErr == nil {
		fts, err = storage.OpenFTS(ftsPath)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "shtrace: open fts (search_commands disabled): %v\n", err)
		} else {
			defer func() { _ = fts.Close() }()
			if migrErr := fts.MigrateFTS(ctx); migrErr != nil {
				_, _ = fmt.Fprintf(stderr, "shtrace: fts migrate (search_commands disabled): %v\n", migrErr)
				fts = nil
			}
		}
	}

	srv := mcp.NewServer(store, fts, dataDir)
	if err := srv.Serve(ctx, r, stdout); err != nil && ctx.Err() == nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: mcp: %v\n", err)
		return 1
	}
	return 0
}

func splitLines(b []byte) [][]byte {
	var out [][]byte
	start := 0
	for i := 0; i < len(b); i++ {
		if b[i] == '\n' {
			out = append(out, b[start:i])
			start = i + 1
		}
	}
	if start < len(b) {
		out = append(out, b[start:])
	}
	return out
}
