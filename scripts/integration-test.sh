#!/usr/bin/env bash
# Phase 1 dogfooding workload (Issue #6).
#
# Intended invocation:
#
#     shtrace bash scripts/integration-test.sh
#
# The outer shtrace becomes span #1 of a new session and propagates
# SHTRACE_SESSION_ID into this script's environment. Each nested `shtrace`
# call below joins that session as a child span. The verifier in
# scripts/integration-test-verify.sh asserts on the recorded state and is
# expected to run after this script exits.
#
# Each block targets one property of the Phase 1 collector:
#   1. nested shtrace invocations share the outer session_id
#   2. stderr stays tagged stream=stderr (not folded into stdout)
#   3. the built-in masker redacts a fake AWS access key in the recorded log
#   4. the streaming masker's tail buffer survives output > 256 B (safetyTail)

set -euo pipefail

if [ -z "${SHTRACE_SESSION_ID:-}" ]; then
    echo "integration-test: SHTRACE_SESSION_ID must be set by the outer shtrace wrapper" >&2
    echo "integration-test: invoke this script as 'shtrace bash $0'" >&2
    exit 64
fi

echo "integration-test: starting (session=$SHTRACE_SESSION_ID)"

# 1. nested span: plain stdout
shtrace -- sh -c 'echo nested-stdout-line'

# 2. nested span: stdout + stderr in one command, so the verifier can confirm
#    on-stderr is recorded under stream=stderr and never under stream=stdout
shtrace -- sh -c 'echo on-stdout; echo on-stderr 1>&2'

# 3. nested span: a fake AWS access key literal that the built-in masker must
#    redact. AKIAIOSFODNN7EXAMPLE is the canonical placeholder from AWS docs;
#    it matches AKIA[0-9A-Z]{16} in internal/secret/masker.go.
shtrace -- sh -c 'echo "leaked AKIAIOSFODNN7EXAMPLE token"'

# 4. nested span: > 256 B of stdout in one command, so the streaming masker
#    exercises its tail-buffer flush path (safetyTail = 256 B in
#    internal/runner/pipe.go). 30 lines * ~12 chars/line is ~360 B.
shtrace -- sh -c 'i=1; while [ $i -le 30 ]; do echo "long-line-$i"; i=$((i + 1)); done'

echo "integration-test: done"
