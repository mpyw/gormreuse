# golangci-lint Integration Guide

This guide explains how to integrate gormreuse with [golangci-lint](https://golangci-lint.run/), the popular Go linters aggregator.

## Table of Contents

- [Module Plugin System (Recommended)](#module-plugin-system-recommended)
- [Go Plugin System (Advanced)](#go-plugin-system-advanced)
- [CI/CD Integration](#cicd-integration)
- [Configuration Options](#configuration-options)
- [Troubleshooting](#troubleshooting)

## Module Plugin System (Recommended)

The Module Plugin System is the easiest way to use custom linters with golangci-lint. It doesn't require matching Go versions or CGO.

### Prerequisites

- golangci-lint v1.55.0 or later
- Go 1.21 or later

### Setup Steps

#### 1. Create `.custom-gcl.yml`

Create a file named `.custom-gcl.yml` in your project root:

```yaml
version: v1.63.4  # Specify golangci-lint version
plugins:
  - module: github.com/mpyw/gormreuse
    import: github.com/mpyw/gormreuse
    version: v0.9.0  # Pin to specific gormreuse version
```

> **Security Note**: Always pin to specific versions in production. Check the [latest release](https://github.com/mpyw/gormreuse/releases) for the most recent version.

#### 2. Build Custom Binary

Run the following command to build a custom golangci-lint binary with gormreuse:

```bash
golangci-lint custom
```

This creates:
- `./custom-gcl` on Linux/macOS
- `./custom-gcl.exe` on Windows

The build process may take a few minutes as it compiles golangci-lint with the plugin.

#### 3. Configure `.golangci.yaml`

Add gormreuse to your golangci-lint configuration:

```yaml
linters:
  enable:
    - gormreuse
    # ... other linters

linters-settings:
  gormreuse:
    # Currently only -test flag is supported
    # See: https://github.com/mpyw/gormreuse#flags
```

#### 4. Run Analysis

Use the custom binary instead of the standard `golangci-lint`:

```bash
# Run on entire project
./custom-gcl run ./...

# Run on specific packages
./custom-gcl run ./internal/...

# Run with specific linters only
./custom-gcl run --disable-all --enable=gormreuse ./...
```

### Directory Structure Example

```
your-project/
├── .custom-gcl.yml          # Plugin configuration
├── .golangci.yaml           # Linter configuration
├── custom-gcl               # Generated binary (gitignored)
├── go.mod
├── go.sum
└── ...
```

Add `custom-gcl` and `custom-gcl.exe` to your `.gitignore`:

```gitignore
# golangci-lint custom binary
/custom-gcl
/custom-gcl.exe
```

## Go Plugin System (Advanced)

The Go Plugin System allows using gormreuse as a shared library plugin. This approach requires careful version matching and CGO support.

> **Warning**: This method is more complex and fragile. Use Module Plugin System unless you have specific requirements.

### Prerequisites

- `CGO_ENABLED=1`
- Exact Go version match between plugin and golangci-lint
- All dependency versions must match golangci-lint's versions

### Setup Steps

#### 1. Check golangci-lint Dependencies

First, check which Go version and dependencies golangci-lint uses:

```bash
go version -m $(which golangci-lint)
```

This shows the Go version and all module versions used by golangci-lint.

#### 2. Create Plugin Wrapper

Create `plugin/gormreuse.go`:

```go
package main

import (
	"golang.org/x/tools/go/analysis"
	"github.com/mpyw/gormreuse"
)

// New is required by golangci-lint plugin system
func New(conf any) ([]*analysis.Analyzer, error) {
	return []*analysis.Analyzer{gormreuse.Analyzer}, nil
}
```

#### 3. Match Dependency Versions

Create or update `go.mod` to match golangci-lint's dependency versions:

```go.mod
module github.com/youruser/gormreuse-plugin

go 1.24  // Must match golangci-lint's Go version

require (
	github.com/mpyw/gormreuse v0.9.0
	golang.org/x/tools v0.x.x  // Must match golangci-lint's version
	// ... all other overlapping dependencies
)
```

#### 4. Build Plugin

```bash
go build -buildmode=plugin -o gormreuse.so plugin/gormreuse.go
```

#### 5. Configure golangci-lint

In `.golangci.yaml`:

```yaml
linters-settings:
  custom:
    gormreuse:
      path: ./gormreuse.so
      description: Detects unsafe GORM *DB instance reuse
      original-url: github.com/mpyw/gormreuse
```

#### 6. Run Analysis

```bash
golangci-lint run ./...
```

### Troubleshooting Go Plugin System

Common issues:

1. **Plugin was built with a different version of package X**
   - Solution: Ensure ALL overlapping dependencies match exactly

2. **Plugin compiled with different Go version**
   - Solution: Use the same Go version as golangci-lint binary

3. **CGO_ENABLED errors**
   - Solution: Set `CGO_ENABLED=1` before building plugin

## CI/CD Integration

### GitHub Actions (Module Plugin System)

```yaml
name: Lint

on: [push, pull_request]

jobs:
  golangci:
    name: golangci-lint with gormreuse
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4

      - uses: actions/setup-go@v5
        with:
          go-version: '1.24'

      - name: Install golangci-lint
        run: |
          curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b $(go env GOPATH)/bin v1.63.4

      - name: Build custom golangci-lint
        run: golangci-lint custom

      - name: Run linters
        run: ./custom-gcl run ./...
```

### GitLab CI

```yaml
lint:
  image: golang:1.24
  stage: test
  script:
    - curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b $(go env GOPATH)/bin v1.63.4
    - golangci-lint custom
    - ./custom-gcl run ./...
  cache:
    paths:
      - .cache/golangci-lint
```

### Docker

Create a custom Docker image:

```dockerfile
FROM golangci/golangci-lint:v1.63.4

WORKDIR /workspace

# Copy plugin configuration
COPY .custom-gcl.yml .

# Build custom binary
RUN golangci-lint custom

# Use custom binary
ENTRYPOINT ["./custom-gcl"]
CMD ["run", "./..."]
```

Build and use:

```bash
docker build -t golangci-lint-gormreuse .
docker run --rm -v $(pwd):/workspace golangci-lint-gormreuse
```

## Configuration Options

Currently, gormreuse supports the `-test` flag (enabled by default):

```yaml
linters-settings:
  gormreuse:
    # Configuration will be expanded in future versions
    # See: https://github.com/mpyw/gormreuse/issues/17
```

The linter respects inline directives:

```go
//gormreuse:ignore
q.Count(&count)  // Suppressed

//gormreuse:pure
func helper(db *gorm.DB) *gorm.DB {
    return db.Session(&gorm.Session{})
}

//gormreuse:immutable-return
func GetDB() *gorm.DB {
    return globalDB.Session(&gorm.Session{})
}
```

See [README - Directives](../README.md#directives) for more details.

## Troubleshooting

### "gormreuse not found" after enabling

**Problem**: After adding `gormreuse` to `.golangci.yaml`, you get "linter not found" error.

**Solution**: You must use the custom binary built with `.custom-gcl.yml`:
```bash
./custom-gcl run ./...  # Not: golangci-lint run ./...
```

### Custom binary not created

**Problem**: `golangci-lint custom` doesn't create `./custom-gcl`.

**Solution**:
1. Ensure `.custom-gcl.yml` exists in the current directory
2. Check golangci-lint version: `golangci-lint --version` (requires v1.55.0+)
3. Check Go version: `go version` (requires Go 1.21+)

### Slow build times

**Problem**: `golangci-lint custom` takes too long.

**Solution**: This is expected for the first build. Subsequent builds are faster. Consider:
- Building once and caching the binary in CI/CD
- Using a Docker image with the plugin pre-built

### Version conflicts

**Problem**: Plugin fails to load due to dependency version mismatch.

**Solution**:
- **Module Plugin System**: No action needed, versions are managed automatically
- **Go Plugin System**: Carefully match ALL dependency versions with `go version -m $(which golangci-lint)`

### CI/CD performance optimization

**Problem**: Building custom binary in every CI run is slow.

**Solution**: Cache the custom binary:

```yaml
# GitHub Actions
- uses: actions/cache@v4
  with:
    path: ./custom-gcl
    key: custom-gcl-${{ hashFiles('.custom-gcl.yml') }}

- name: Build custom golangci-lint
  if: steps.cache.outputs.cache-hit != 'true'
  run: golangci-lint custom
```

### False positives

**Problem**: The linter reports violations in legitimate code.

**Solution**: Use `//gormreuse:ignore` directive:

```go
//gormreuse:ignore // intentional reuse for pagination
q.Count(&count)
```

If you believe it's a genuine bug, please [report it](https://github.com/mpyw/gormreuse/issues).

## Additional Resources

- [golangci-lint Module Plugin Documentation](https://golangci-lint.run/docs/plugins/module-plugins/)
- [golangci-lint Go Plugin Documentation](https://golangci-lint.run/docs/plugins/go-plugins/)
- [gormreuse GitHub Repository](https://github.com/mpyw/gormreuse)
- [GORM Method Chaining Documentation](https://gorm.io/docs/method_chaining.html)

## Getting Help

If you encounter issues not covered in this guide:

1. Check [existing issues](https://github.com/mpyw/gormreuse/issues)
2. Review [golangci-lint plugin documentation](https://golangci-lint.run/docs/plugins/)
3. [Open a new issue](https://github.com/mpyw/gormreuse/issues/new) with:
   - Your golangci-lint version (`golangci-lint --version`)
   - Your Go version (`go version`)
   - Your `.custom-gcl.yml` and `.golangci.yaml` configuration
   - Full error output
