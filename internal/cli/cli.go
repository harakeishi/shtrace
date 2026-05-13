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
	"time"

	"github.com/harakeishi/shtrace/internal/runner"
	"github.com/harakeishi/shtrace/internal/secret"
	"github.com/harakeishi/shtrace/internal/session"
	"github.com/harakeishi/shtrace/internal/storage"
)

// Run dispatches to the requested subcommand. argv follows the os.Args
// convention (argv[0] is the program name).
func Run(ctx context.Context, argv []string, stdout, stderr io.Writer) int {
	if len(argv) < 2 {
		fmt.Fprintln(stderr, "usage: shtrace <subcommand> [args...]")
		fmt.Fprintln(stderr, "subcommands: run (default), ls, show, session, shell-init")
		return 2
	}

	switch argv[1] {
	case "ls":
		return runLs(ctx, argv[2:], stdout, stderr)
	case "show":
		return runShow(ctx, argv[2:], stdout, stderr)
	case "session":
		return runSession(ctx, argv[2:], stdout, stderr)
	case "shell-init":
		return runShellInit(argv[2:], stdout, stderr)
	case "--":
		return runWrapped(ctx, argv[2:], stdout, stderr)
	default:
		// Treat `shtrace cmd args...` the same as `shtrace -- cmd args...`
		return runWrapped(ctx, argv[1:], stdout, stderr)
	}
}

// runWrapped executes `cmd args...`, records stdout/stderr, and persists span
// metadata. This is the core MVP path.
func runWrapped(ctx context.Context, cmdArgs []string, stdout, stderr io.Writer) int {
	if len(cmdArgs) == 0 {
		fmt.Fprintln(stderr, "shtrace: no command to run")
		return 2
	}

	env := envMap()
	dataDir, err := storage.ResolveDataDir(env, runtime.GOOS)
	if err != nil {
		fmt.Fprintf(stderr, "shtrace: %v\n", err)
		return 1
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		fmt.Fprintf(stderr, "shtrace: mkdir data dir: %v\n", err)
		return 1
	}

	sessCtx, err := session.FromEnv(env, session.DefaultIDGenerator())
	if err != nil {
		fmt.Fprintf(stderr, "shtrace: %v\n", err)
		return 1
	}

	store, err := storage.Open(dataDir + "/sessions.db")
	if err != nil {
		fmt.Fprintf(stderr, "shtrace: open store: %v\n", err)
		return 1
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		fmt.Fprintf(stderr, "shtrace: migrate: %v\n", err)
		return 1
	}

	startedAt := time.Now().UTC()
	if sessCtx.IsRoot {
		if err := store.InsertSession(ctx, storage.Session{
			ID:        sessCtx.SessionID,
			StartedAt: startedAt,
			Tags:      sessCtx.Tags,
		}); err != nil {
			fmt.Fprintf(stderr, "shtrace: insert session: %v\n", err)
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
			fmt.Fprintf(stderr, "shtrace: ensure session: %v\n", err)
			return 1
		}
	}

	logPath := storage.OutputPath(dataDir, sessCtx.SessionID, sessCtx.SpanID)
	if err := os.MkdirAll(parentDir(logPath), 0o755); err != nil {
		fmt.Fprintf(stderr, "shtrace: mkdir outputs: %v\n", err)
		return 1
	}
	logFile, err := os.Create(logPath)
	if err != nil {
		fmt.Fprintf(stderr, "shtrace: open log: %v\n", err)
		return 1
	}
	defer logFile.Close()

	cwd, _ := os.Getwd()
	childEnv := append(os.Environ(), envMapToSlice(sessCtx.ChildEnv())...)

	jsonl := storage.NewJSONLWriter(logFile, nil)

	// Construct the masker once and share between the runner and the
	// argv-persisting code so that any future user-supplied patterns apply
	// uniformly.
	masker := secret.DefaultMasker()

	res, runErr := runner.RunPipe(ctx, runner.PipeOptions{
		Argv:   cmdArgs,
		Env:    childEnv,
		Cwd:    cwd,
		Writer: jsonl,
		Stdout: stdout,
		Stderr: stderr,
		Masker: masker,
	})
	if runErr != nil {
		fmt.Fprintf(stderr, "shtrace: run: %v\n", runErr)
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
		Mode:         "pipe",
		StartedAt:    startedAt,
		EndedAt:      endedAt,
		ExitCode:     &exitCode,
	}); err != nil {
		fmt.Fprintf(stderr, "shtrace: insert span: %v\n", err)
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
			fmt.Fprintf(stderr, "shtrace: finalize session: %v\n", err)
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
		fmt.Fprintf(stderr, "shtrace: %v\n", err)
		return 1
	}
	store, err := storage.Open(dataDir + "/sessions.db")
	if err != nil {
		fmt.Fprintf(stderr, "shtrace: open store: %v\n", err)
		return 1
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		fmt.Fprintf(stderr, "shtrace: migrate: %v\n", err)
		return 1
	}

	warn := func(e error) { fmt.Fprintf(stderr, "shtrace: warning: %v\n", e) }
	sessions, err := store.ListSessions(ctx, 50, warn)
	if err != nil {
		fmt.Fprintf(stderr, "shtrace: list: %v\n", err)
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
		fmt.Fprintln(stdout, string(b))
		return 0
	}

	for _, s := range sessions {
		spans, err := store.SpansForSession(ctx, s.ID, warn)
		if err != nil {
			fmt.Fprintf(stderr, "shtrace: spans for %s: %v\n", s.ID, err)
		}
		cmdSummary := ""
		if len(spans) > 0 {
			cmdSummary = spans[0].Command
		}
		fmt.Fprintf(stdout, "%s  %s  spans=%d  %s\n", s.StartedAt.Format(time.RFC3339), s.ID, len(spans), cmdSummary)
	}
	return 0
}

func runShow(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: shtrace show <session_id>")
		return 2
	}
	sessionID := args[0]

	env := envMap()
	dataDir, err := storage.ResolveDataDir(env, runtime.GOOS)
	if err != nil {
		fmt.Fprintf(stderr, "shtrace: %v\n", err)
		return 1
	}
	store, err := storage.Open(dataDir + "/sessions.db")
	if err != nil {
		fmt.Fprintf(stderr, "shtrace: open store: %v\n", err)
		return 1
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		fmt.Fprintf(stderr, "shtrace: migrate: %v\n", err)
		return 1
	}

	warn := func(e error) { fmt.Fprintf(stderr, "shtrace: warning: %v\n", e) }
	spans, err := store.SpansForSession(ctx, sessionID, warn)
	if err != nil {
		fmt.Fprintf(stderr, "shtrace: spans: %v\n", err)
		return 1
	}
	if len(spans) == 0 {
		fmt.Fprintf(stderr, "shtrace: no spans for session %s\n", sessionID)
		return 1
	}

	for _, sp := range spans {
		fmt.Fprintf(stdout, "== span %s  cmd=%s  exit=%v  mode=%s\n", sp.ID, sp.Command, derefInt(sp.ExitCode), sp.Mode)
		logPath := storage.OutputPath(dataDir, sessionID, sp.ID)
		b, err := os.ReadFile(logPath)
		if err != nil {
			fmt.Fprintf(stderr, "shtrace: read log %s: %v\n", logPath, err)
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
				fmt.Fprintf(stderr, "shtrace: skipped corrupt line %d in %s: %v\n", i+1, logPath, err)
				continue
			}
			switch storage.Stream(c.Stream) {
			case storage.StreamStderr:
				fmt.Fprint(stderr, c.Data)
			default:
				// stdout, pty (mode A merged), or any future label
				fmt.Fprint(stdout, c.Data)
			}
		}
		fmt.Fprintln(stdout)
		if corrupt > 0 {
			fmt.Fprintf(stderr, "shtrace: %d corrupt line(s) skipped in %s\n", corrupt, logPath)
		}
	}
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
func runSession(_ context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: shtrace session <verb>")
		fmt.Fprintln(stderr, "verbs: new")
		return 2
	}
	switch args[0] {
	case "new":
		id, err := session.DefaultIDGenerator().NewSessionID()
		if err != nil {
			fmt.Fprintf(stderr, "shtrace: generate session id: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, id)
		return 0
	default:
		fmt.Fprintf(stderr, "shtrace: unknown session verb %q\n", args[0])
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
		fmt.Fprintln(stderr, "usage: shtrace shell-init <bash|zsh>")
		return 2
	}
	shell := args[0]
	switch shell {
	case "bash", "zsh":
		// The snippet is intentionally POSIX-compatible so the same code
		// works for both shells without separate branches.
		fmt.Fprint(stdout, `if [ -z "${SHTRACE_SESSION_ID:-}" ]; then
  export SHTRACE_SESSION_ID="$(shtrace session new)"
fi
`)
		return 0
	default:
		fmt.Fprintf(stderr, "shtrace: unsupported shell %q (supported: bash, zsh)\n", shell)
		return 2
	}
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
