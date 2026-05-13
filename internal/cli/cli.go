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
	"runtime"
	"strings"
	"time"

	"github.com/mattn/go-isatty"

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
		_, _ = fmt.Fprintln(stderr, "subcommands: run (default), ls, show, search, reindex, session, shell-init")
		return 2
	}

	// Parse optional --mode flag before dispatching subcommands.
	mode, argv, modeErr := parseMode(argv)
	if modeErr != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: %v\n", modeErr)
		return 2
	}

	switch argv[1] {
	case "ls":
		return runLs(ctx, argv[2:], stdout, stderr)
	case "show":
		return runShow(ctx, argv[2:], stdout, stderr)
	case "search":
		return runSearch(ctx, argv[2:], stdout, stderr)
	case "reindex":
		return runReindex(ctx, argv[2:], stdout, stderr)
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

	// Choose mode A (PTY) or mode B (pipe).
	// Auto-detect: use PTY when stdout is a real terminal, unless overridden.
	if mode == "" {
		if f, ok := stdout.(*os.File); ok && isatty.IsTerminal(f.Fd()) {
			mode = "pty"
		} else {
			mode = "pipe"
		}
	}

	// Resolve the effective mode: if PTY was requested but stdout is not a
	// *os.File the output would be silently lost, so fall back to pipe.
	var ptyTty *os.File
	if mode == "pty" {
		if f, ok := stdout.(*os.File); ok {
			ptyTty = f
		} else {
			_, _ = fmt.Fprintf(stderr, "shtrace: warning: --mode pty requested but stdout is not a terminal; falling back to pipe\n")
			mode = "pipe"
		}
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
		mode = val
	}
	return mode, out, nil
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
