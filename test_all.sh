#!/bin/bash
# Runs every check CI runs (.github/workflows/ci.yml). A missing tool is an
# error, not a skip: this script is the full gate, so it must not pass without
# running everything. Tool versions come from mise.toml (run `mise install`).
set -euo pipefail

cd "$(dirname "$0")"

# require checks that a tool runs, not only that it is on PATH: a mise shim
# with no version set passes `command -v` and fails when run.
require() {
    local tool=$1
    shift
    if ! "$tool" "$@" > /dev/null 2>&1; then
        echo "ERROR: $tool is missing or does not run. Install the tools with \`mise install\`." >&2
        exit 1
    fi
}

require git --version
require go version
require declscope -V=full
require golangci-lint version

echo "=== Running declscope shrink ==="
declscope shrink ./...

echo ""
echo "=== Running declscope ==="
declscope ./...

# CI fails if the tests rewrite tracked files (#203). A local tree may already
# have changes, so compare the tree before and after the tests instead.
tree_state() {
    git status --porcelain
    git diff
}
before=$(tree_state)

echo ""
echo "=== Running tests ==="
go test -timeout=20m -race ./...

echo ""
echo "=== Running E2E tests (e2e/internal) ==="
(cd e2e/internal && go test ./...)

echo ""
echo "=== Checking that the tests left tracked files unchanged ==="
if [ "$(tree_state)" != "$before" ]; then
    echo "ERROR: the tests changed files in the working tree:" >&2
    git status --short >&2
    exit 1
fi

echo ""
echo "=== Building ==="
go build -o bin/gormreuse ./cmd/gormreuse

echo ""
echo "=== Running golangci-lint ==="
golangci-lint run ./...

echo ""
echo "=== Running golangci-lint (e2e/internal) ==="
(cd e2e/internal && golangci-lint run ./...)

echo ""
echo "=== All checks passed ==="
