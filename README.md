# shtrace

> Shell-execution observability for AI-era engineering. Wrap a command, get a
> searchable, replayable record of what actually happened.

`shtrace` is a single Go binary that wraps any shell command and records its
stdout/stderr, exit code, working directory, and timing into a local on-disk
store. It is built so that humans (and AI agents) can later answer questions
like "did this test actually run?", "what did `pytest` print on that failed
CI job?", or "what changed between these two runs?" — without sending data to
a SaaS.

## Status

**Phase 1 (Collector MVP) — alpha.** Phase 2 (MCP server) and the rest of
Phase 3 (CI artifact + export/import) are planned but not yet implemented.
See [Roadmap](#roadmap).

## Why

Existing tools cover adjacent ground but not this one:

- Shell-history MCPs (e.g. `terminal-history-mcp`) record **what was run**,
  not **what was printed**.
- `tlog` / `sudoreplay` / `auditd` record PTY sessions for security audit,
  not for CI/AI verification.
- LLM observability (Langfuse, LangSmith, …) traces model calls, not the
  shell commands those agents execute.
- OpenTelemetry shell wrappers (`otel-cli`) emit spans but discard output.

`shtrace` records the **command + the full output stream + metadata**, in a
unified format that works the same way locally and in CI, and stays
self-hosted by design.

For the full design rationale, see the planning document referenced in the
repo history.

## Installation

Requires Go 1.24+.

```sh
go install github.com/harakeishi/shtrace/cmd/shtrace@latest
```

Or build from source:

```sh
git clone https://github.com/harakeishi/shtrace
cd shtrace
go build ./cmd/shtrace
```

The result is a single static binary with no CGo dependency (uses
`modernc.org/sqlite`).

## Quickstart

Wrap any command:

```sh
shtrace -- go test ./...
shtrace -- pytest tests/
shtrace -- sh -c 'echo hello; echo oops >&2; exit 3'
```

The wrapped command's stdout, stderr, and exit code pass through unchanged.
Everything is also recorded.

List recent sessions:

```sh
$ shtrace ls
2026-05-12T10:00:00Z  019e1cb2-…  spans=1  sh
2026-05-12T09:55:00Z  019e1cb1-…  spans=4  go
```

Inspect a session:

```sh
$ shtrace show <session-id>
== span 019e1cb2-…  cmd=sh  exit=3  mode=pipe
hello
oops
```

`show` routes recorded stdout to its own stdout and recorded stderr to its
own stderr, so `shtrace show <id> > out.log 2> err.log` separates them again.

JSON output for scripting:

```sh
shtrace ls --json | jq '.[0].id'
```

## HTML report

`shtrace report` renders one session into a single self-contained HTML file
(inline CSS, no external assets) that can be opened with `file://` — no
server needed. This is what reviewers will open after downloading the GitHub
Actions artifact in the Phase 3 PR-verification flow.

```sh
shtrace report --latest --output report.html
# or pick an explicit session:
shtrace report --session <session-id> --output report.html
# or stream to stdout for piping:
shtrace report --latest > report.html
```

The report includes session metadata, a chronological timeline of spans,
each span's stdout/stderr (colour-coded), exit code, mode, and duration.
Browser find-in-page (Ctrl/Cmd+F) is sufficient for the Phase 3 scope; full
text search and asciinema-style replay live in Phase 4.

## Automatic session grouping (shell-init)

By default each `shtrace` invocation starts a fresh session. To group every
command you run in a terminal window into **one session** automatically, add
this line to your `~/.bashrc` or `~/.zshrc`:

```sh
# ~/.bashrc  (or ~/.zshrc)
eval "$(shtrace shell-init bash)"   # use zsh for zsh
```

After opening a new terminal, `SHTRACE_SESSION_ID` is exported automatically.
Every subsequent `shtrace` call in that terminal joins the same session:

```sh
shtrace -- go test ./...
shtrace -- pytest tests/
shtrace show $SHTRACE_SESSION_ID    # see both runs together
```

If `SHTRACE_SESSION_ID` is already set (e.g. from a parent CI job), the
snippet is a no-op, so it is safe to add unconditionally.

You can also generate a session ID manually:

```sh
export SHTRACE_SESSION_ID="$(shtrace session new)"
```

## How it works

### Recording modes

- **mode B (pipe)** — when shtrace's stdout is not a TTY (CI, redirects,
  pipes). Captures stdout and stderr as separate streams. **This is what is
  implemented today.**
- **mode A (PTY)** — when shtrace's stdout is a TTY. Will allocate a PTY to
  preserve color/progress-bar output. **Not yet implemented.**

Both modes share a single on-disk JSON Lines schema (`stream` field
disambiguates `stdout` / `stderr` / `pty`).

### Secret masking (fail-secure)

The recorded log is scrubbed; the user's own terminal stream is left
untouched. Built-in patterns cover AWS access keys, GitHub PATs
(`ghp_…`/`gho_…`/…), OpenAI API keys, `Bearer …` tokens, and JWTs. The
masker buffers a small trailing window between reads so a secret that
straddles a pipe-buffer boundary still gets caught.

### Session/span propagation

Each invocation gets a UUIDv7 `span_id`. The root invocation gets a fresh
`session_id`; nested `shtrace` calls inherit it via three environment
variables:

| Variable | Purpose |
|---|---|
| `SHTRACE_SESSION_ID` | Joins an existing session |
| `SHTRACE_PARENT_SPAN_ID` | Parent of the new span |
| `SHTRACE_TAGS` | JSON object propagated to all child spans |

This means `shtrace make all` whose `Makefile` calls `shtrace pytest` records
one session containing both spans, with parent/child linkage preserved.

### Storage layout

```
$DATA_DIR/
├── sessions.db                       # SQLite (WAL), metadata only
└── outputs/<session_id>/<span_id>.log # JSON Lines, one chunk per line
```

`$DATA_DIR` is resolved in this order:

1. `$SHTRACE_DATA_DIR` (explicit override)
2. `$GITHUB_WORKSPACE/.shtrace` when `GITHUB_ACTIONS=true`
3. `$XDG_DATA_HOME/shtrace`
4. `~/.local/share/shtrace` (Linux)
5. `~/Library/Application Support/shtrace` (macOS)

A single corrupt row in `sessions.db` does **not** take the whole listing
offline — the row is skipped and a warning is printed to stderr.

## Configuration

All configuration is via environment variables. There is no config file (by
design — CI integration should be a single env var, not a checked-in file).

| Variable | Default | Purpose |
|---|---|---|
| `SHTRACE_DATA_DIR` | platform-specific | Where to store sessions and logs |
| `SHTRACE_SESSION_ID` | unset (new session) | Join an existing session |
| `SHTRACE_PARENT_SPAN_ID` | unset | Parent span id for nested calls |
| `SHTRACE_TAGS` | `{}` | JSON object of tags propagated to child spans |

## Roadmap

| Phase | Scope | Status |
|---|---|---|
| 1. Collector MVP | `shtrace <cmd>`, `ls`, `show`, mode B, secret masking, session propagation, SQLite + JSON L, `shell-init` | partially done (mode A, FTS5, GC, entropy masking still pending) |
| 2. MCP server | `shtrace mcp` stdio server with `get_session`, `search_commands`, `detect_test_runs`, `compare_runs` | planned |
| 3. PR verification + HTML report | `shtrace report` (done), `shtrace export/import`, GitHub Actions workflow, `shtrace pr-comment` | in progress |
| 4. Web UI | `shtrace serve` with dynamic search, asciinema-style replay, diff view | stretch goal |
| 5. attest layer | AI execution-verification score | stretch goal |

## Non-goals

- SaaS hosting
- Multi-host aggregation
- Telemetry of any kind (the binary never phones home)
- Windows support (Linux and macOS only)
- Automatic shell hooks / aliases — `shtrace` records what it is explicitly
  asked to wrap; wrapping is the caller's responsibility (see the plan for
  the rationale behind this choice over the shell-hook approach)

## Contributing

This is a personal OSS project. Issues and PRs welcome; please open an issue
before starting non-trivial work so we can agree on scope.

Development workflow:

```sh
go test -race ./...      # all tests must pass under -race
go vet ./...
```

The codebase follows a strict TDD discipline — write a failing test first,
then the minimum implementation that makes it pass.

## License

MIT. See [LICENSE](./LICENSE).
