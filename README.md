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

**Phase 1 (Collector MVP) — alpha.** Phases 2 (MCP server) and 3 (CI artifact
+ HTML report) are planned but not yet implemented. See [Roadmap](#roadmap).

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
| 3. PR verification + HTML report | `shtrace export/import`, `shtrace report --html`, GitHub Actions workflow, `shtrace pr-comment` | planned |
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
go test -bench=. ./...   # benchmarks must stay green; see SLIs below
```

The codebase follows a strict TDD discipline — write a failing test first,
then the minimum implementation that makes it pass.

### Performance SLIs

`shtrace` is a wrapper, so its overhead has to be invisible against the
wrapped command. Concrete SLIs (committed-to ceilings, all measured as
**mean ns/op** by `go test -bench`; aggregate across `-count=N` runs with
`benchstat` if you need a distribution):

| SLI | Target (mean) | Benchmark |
|---|---|---|
| Wrapping a near-zero-output command (spawn floor) | wrapped wall-clock of `sh -c 'printf hi'` < 5 ms | `BenchmarkRunPipe_SpawnFloor` |
| Masker + recorder streaming throughput (in-process, no child) | ≥ 10 MB/s for printable ASCII | `BenchmarkForwardStream_Throughput` (reports MB/s) |
| Span insert (sqlite WAL, `synchronous=NORMAL`) | `InsertSpan` < 5 ms | `BenchmarkInsertSpan` |
| Session list (50 rows out of 1 000) | < 10 ms | `BenchmarkListSessions` |
| Spans-for-session (100 rows) | < 10 ms | `BenchmarkSpansForSession` |
| FTS first-time index of one span (100 lines, fresh `span_id`) | < 10 ms | `BenchmarkFTSIndexSpan` |
| FTS search across 100 indexed spans (~20 % selectivity) | < 20 ms | `BenchmarkFTSSearch` |

The spawn-floor SLI measures wrapped wall-clock, not "wrapper overhead
relative to a bare command" — the latter requires a separate baseline run
and is currently out of scope. The storage SLIs assume the pragma settings
applied by `storage.Open` (WAL journal, default `synchronous=NORMAL`); if a
future change tightens durability to `FULL`, retune the ceilings or document
the new floor.

These targets are intentionally generous for the v0.x line — the goal is to
catch regressions, not to chase microseconds. Once shtrace is integrated into
a real pytest/Go test suite (Phase 3.5), the headline SLI becomes "`shtrace
pytest tests/` adds ≤ X % wall-clock over a bare `pytest tests/` on the same
host", with X to be pinned from production measurements.

## License

MIT. See [LICENSE](./LICENSE).
