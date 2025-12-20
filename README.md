# gormreuse

[![Go Reference](https://pkg.go.dev/badge/github.com/mpyw/gormreuse.svg)](https://pkg.go.dev/github.com/mpyw/gormreuse)
[![Go Report Card](https://goreportcard.com/badge/github.com/mpyw/gormreuse)](https://goreportcard.com/report/github.com/mpyw/gormreuse)
[![Codecov](https://codecov.io/gh/mpyw/gormreuse/graph/badge.svg)](https://codecov.io/gh/mpyw/gormreuse)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

> [!NOTE]
> This project was written by AI (Claude Code).

A Go linter that detects unsafe [`*gorm.DB`](https://pkg.go.dev/gorm.io/gorm#DB) instance reuse after chain methods.

## Background

[GORM](https://pkg.go.dev/gorm.io/gorm)'s chain methods ([`Where`](https://pkg.go.dev/gorm.io/gorm#DB.Where), [`Order`](https://pkg.go.dev/gorm.io/gorm#DB.Order), etc.) modify internal state. Reusing the same [`*gorm.DB`](https://pkg.go.dev/gorm.io/gorm#DB) instance after chain methods can cause query conditions to accumulate unexpectedly.

```go
q := db.Where("active = ?", true)
q.Find(&users)  // SELECT * FROM users WHERE active = true
q.Find(&admins) // Bug: SELECT * FROM users WHERE active = ? AND active = ?
```

## Installation & Usage

### Using [`go install`](https://pkg.go.dev/cmd/go#hdr-Compile_and_install_packages_and_dependencies)

```bash
go install github.com/mpyw/gormreuse/cmd/gormreuse@latest
gormreuse ./...
```

### Using [`go tool`](https://pkg.go.dev/cmd/go#hdr-Run_specified_go_tool) (Go 1.24+)

```bash
# Add to go.mod as a tool dependency
go get -tool github.com/mpyw/gormreuse/cmd/gormreuse@latest

# Run via go tool
go tool gormreuse ./...
```

### Using [`go run`](https://pkg.go.dev/cmd/go#hdr-Compile_and_run_Go_program)

```bash
go run github.com/mpyw/gormreuse/cmd/gormreuse@latest ./...
```

> [!CAUTION]
> To prevent supply chain attacks, pin to a specific version tag instead of `@latest` in CI/CD pipelines (e.g., `@v0.3.0`).

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-test` | `true` | Analyze test files (`*_test.go`) — built-in driver flag |

Generated files (containing `// Code generated ... DO NOT EDIT.`) are always excluded and cannot be opted in.

### Examples

```bash
# Exclude test files from analysis
gormreuse -test=false ./...
```

## Detection Model: Pollute Semantics

This linter uses a "pollute" model inspired by Rust's move semantics. The core concept:

1. **Safe Methods** (`Session`, `WithContext`) return an **immutable** copy
2. **Chain Methods** (all others including finishers) **pollute** the receiver if it's mutable-derived
3. Using a **polluted** mutable instance is a **violation**

### Method Classification

| Category        | Methods                  | Description                    |
| --------------- | ------------------------ | ------------------------------ |
| Safe Methods    | [`Session`](https://pkg.go.dev/gorm.io/gorm#DB.Session), [`WithContext`](https://pkg.go.dev/gorm.io/gorm#DB.WithContext) | Return new immutable instance  |
| DB Init Methods | [`Begin`](https://pkg.go.dev/gorm.io/gorm#DB.Begin), [`Transaction`](https://pkg.go.dev/gorm.io/gorm#DB.Transaction)   | Create new DB instance         |
| Chain Methods   | All others               | Pollute mutable receiver       |

### Automatic Pollution Sources

The linter conservatively marks `*gorm.DB` as polluted in these scenarios:

| Operation                | Description                                              |
| ------------------------ | -------------------------------------------------------- |
| Interface method call    | `repo.Query(db)` - Can't statically analyze              |
| Channel send             | `ch <- db` - May be received and used elsewhere          |
| Slice/Map storage        | `[]*gorm.DB{db}` - May be accessed elsewhere             |
| Interface conversion     | `interface{}(db)` - May be extracted via type assertion  |
| Non-pure function call   | `helper(db)` - Unless marked with `gormreuse:pure`       |
| Struct field access      | `h.db.Find(nil)` - Traces back to the stored value       |

Note: Simple struct literal storage (`_ = &S{db: q}`) without actual field usage does NOT pollute.

### Examples

#### Safe: Reuse from immutable

```go
// Session at end creates immutable - safe to reuse
q := db.Where("active = ?", true).Session(&gorm.Session{})
q.Count(&count)  // OK
q.Find(&users)   // OK - q is immutable

// Branching from immutable is safe
q := db.Where("base").Session(&gorm.Session{})
q.Where("a").Find(&users1)  // OK - creates new mutable, pollutes it
q.Where("b").Find(&users2)  // OK - creates another new mutable
```

#### Violation: Reuse from mutable

```go
// Mutable instance reused after pollution
q := db.Where("active = ?", true)  // mutable
q.Count(&count)  // pollutes q
q.Find(&users)   // VIOLATION: q already polluted

// Session in middle doesn't help - result is still mutable
q := db.Session(&gorm.Session{}).Where("x")  // mutable!
q.Count(&count)  // pollutes q
q.Find(&users)   // VIOLATION

// Session on polluted value is also a violation
q := db.Where("x")
q.Find(&users)                       // pollutes q
q.Session(&gorm.Session{}).Count(&c) // VIOLATION: using polluted q
```

## Directives

### `gormreuse:ignore`

Suppress warnings for a specific line:

```go
q := db.Where("active = ?", true)
q.Find(&users)
//gormreuse:ignore
q.Count(&count)  // Suppressed
```

Or suppress for an entire function:

```go
//gormreuse:ignore
func legacyCode(db *gorm.DB) {
    // All violations in this function are suppressed
}
```

**Note:** Unused `gormreuse:ignore` directives are reported as warnings. This helps keep the codebase clean by identifying stale ignore comments.

### `gormreuse:pure`

Mark a function as not polluting its `*gorm.DB` argument:

```go
//gormreuse:pure
func countOnly(db *gorm.DB) int64 {
    var count int64
    db.Count(&count)
    return count
}
```

## Documentation

- [CLAUDE.md](./CLAUDE.md) - AI assistant guidance for development

## Development

```bash
# Run tests
go test ./...

# Build CLI
go build -o bin/gormreuse ./cmd/gormreuse

# Run linter on a project
./bin/gormreuse ./...
```

## Related Tools

- [goroutinectx](https://github.com/mpyw/goroutinectx) - Goroutine context propagation linter
- [zerologlintctx](https://github.com/mpyw/zerologlintctx) - Zerolog context propagation linter
- [ctxweaver](https://github.com/mpyw/ctxweaver) - Code generator for context-aware instrumentation

## License

MIT License
