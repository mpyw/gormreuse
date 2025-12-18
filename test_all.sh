#!/bin/bash
set -e

echo "=== Running tests ==="
go test -v -race ./...

echo ""
echo "=== Building ==="
go build -o bin/gormreuse ./cmd/gormreuse

echo ""
echo "=== Running golangci-lint ==="
if command -v golangci-lint &> /dev/null; then
    golangci-lint run ./...
else
    echo "golangci-lint not installed, skipping"
fi

echo ""
echo "=== All checks passed ==="
