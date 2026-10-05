#!/usr/bin/env bash
# One entry point for the checks that otherwise have to be typed one at a time.
#
#   scripts/test.sh            full run: gofmt, build, vet, tests, verify
#   scripts/test.sh --short    skip the packages that shell out to toolchains
#   scripts/test.sh --race     the mode CI runs (race detector)
#   scripts/test.sh --fast     gofmt, build, vet, verify only; no test run
#
# Every step is a plain `go` invocation, so this needs nothing beyond the Go
# toolchain: no make, no task runner, nothing to install first.

set -uo pipefail

cd "$(dirname "$0")/.." || exit 1

short=""
race=""
fast=""
for arg in "$@"; do
  case "$arg" in
  --short) short="-short" ;;
  --race) race="-race" ;;
  --fast) fast=1 ;;
  *)
    echo "unknown flag: $arg" >&2
    exit 2
    ;;
  esac
done

failed=""

# Acceptance evidence is a directory of captured editor sessions and fixtures;
# those files are inputs to external tooling rather than ours, so they keep
# whatever formatting they came with.
step() {
  local name="$1"
  shift
  printf '\n=== %s ===\n' "$name"
  if "$@"; then
    return 0
  fi
  failed="$failed $name"
  return 0
}

step "gofmt" bash -c '
  # Match "evidence" without a separator: gofmt prints backslash paths on
  # Windows, so filtering on "evidence/" leaves every fixture listed and fails
  # the run on a clean tree.
  out=$(gofmt -l internal cmd test 2>/dev/null | grep -v evidence)
  if [ -n "$out" ]; then
    echo "$out"
    exit 1
  fi'

step "build" go build ./...
step "vet" go vet ./...

if [ -z "$fast" ]; then
  step "test" go test -count=1 $short $race -timeout 90m ./...
fi

# verify is the same binary CI ships, so this also covers the conformance
# registry: it resolves every probe symbol and compares the score against the
# committed baseline.
step "verify" go run ./cmd/omnilsp verify --min 90

if [ -n "$failed" ]; then
  printf '\nFAILED:%s\n' "$failed" >&2
  exit 1
fi
printf '\nall checks passed\n'