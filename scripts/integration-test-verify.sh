#!/usr/bin/env bash
# Verifier for the Phase 1 dogfooding workload (Issue #6).
#
# Run immediately after `shtrace bash scripts/integration-test.sh` against the
# same SHTRACE_DATA_DIR. Asserts:
#
#   1. exactly one session was recorded
#   2. at least 5 spans were recorded (outer bash + 4 nested commands)
#   3. the AKIA secret literal does NOT appear in any JSONL log, and the
#      redaction marker '***' DOES appear
#   4. stderr is recorded under stream=stderr (and the on-stderr line is not
#      misrouted into stream=stdout)
#
# Exits non-zero on the first failed assertion summary. Diagnostic output goes
# to stderr so the script is safe to compose into larger CI pipelines.

set -euo pipefail

if [ -z "${SHTRACE_DATA_DIR:-}" ]; then
    echo "verify: SHTRACE_DATA_DIR not set" >&2
    exit 64
fi
if ! command -v jq >/dev/null 2>&1; then
    echo "verify: jq is required" >&2
    exit 64
fi
if ! command -v shtrace >/dev/null 2>&1; then
    echo "verify: shtrace must be on PATH" >&2
    exit 64
fi

fail=0
note() { printf 'verify: %s\n' "$*" >&2; }
err()  { printf 'verify FAIL: %s\n' "$*" >&2; fail=1; }

# --- assertion 1: exactly one session ---
sessions_json=$(shtrace ls --json)
session_count=$(printf '%s' "$sessions_json" | jq 'length')
if [ "$session_count" != "1" ]; then
    err "expected 1 session, got $session_count (ls --json: $sessions_json)"
    # Bail out — without a session id the remaining checks are meaningless.
    exit 1
fi
session_id=$(printf '%s' "$sessions_json" | jq -r '.[0].id')
note "session=$session_id"

# --- assertion 2: at least 5 span log files (1 outer + 4 nested) ---
log_dir="$SHTRACE_DATA_DIR/outputs/$session_id"
if [ ! -d "$log_dir" ]; then
    err "log dir $log_dir missing"
    exit 1
fi
expected_min_spans=5
span_count=$(find "$log_dir" -maxdepth 1 -name '*.log' | wc -l | tr -d ' ')
if [ "$span_count" -lt "$expected_min_spans" ]; then
    err "expected >= $expected_min_spans spans, got $span_count under $log_dir"
else
    note "$span_count span log files"
fi

# --- assertion 3: secret masking ---
# A leaked secret in any JSONL log is a hard failure. The redaction marker
# must be present at least once so we know the masker actually fired (and
# we're not just looking at empty logs).
if grep -rE 'AKIA[0-9A-Z]{16}' "$log_dir" >/dev/null; then
    err "AWS-style secret leaked into recorded log under $log_dir"
else
    note "no AKIA secret in recorded logs"
fi
if ! grep -rqF '***' "$log_dir"; then
    err "redaction marker '***' not found in any recorded log"
else
    note "redaction marker present"
fi

# --- assertion 4: stderr separated from stdout ---
if ! grep -rqF '"stream":"stderr"' "$log_dir"; then
    err "no chunk tagged stream=stderr in any log"
fi
if ! grep -rqF '"stream":"stdout"' "$log_dir"; then
    err "no chunk tagged stream=stdout in any log"
fi

# The 'on-stderr' string from the nested span must be tagged stream=stderr
# and must never appear under stream=stdout. Using jq instead of grep avoids
# brittle regex-inside-JSON escaping concerns and correctly handles a chunk
# that bundles the line with surrounding stdout text.
found_stderr_marker=0
found_stdout_misroute=0
while IFS= read -r logfile; do
    while IFS= read -r entry; do
        case "$entry" in
            stderr)          found_stderr_marker=1 ;;
            stdout-misroute) found_stdout_misroute=1 ;;
        esac
    done < <(jq -r '
        select(.data | test("on-stderr"))
        | if .stream == "stderr" then "stderr" else "stdout-misroute" end
    ' "$logfile")
done < <(find "$log_dir" -maxdepth 1 -name '*.log')

if [ "$found_stderr_marker" -eq 0 ]; then
    err "'on-stderr' not recorded under stream=stderr in any log"
fi
if [ "$found_stdout_misroute" -ne 0 ]; then
    err "'on-stderr' leaked into stream=stdout"
fi

if [ "$fail" -ne 0 ]; then
    exit 1
fi
note "all integration-test assertions passed"
