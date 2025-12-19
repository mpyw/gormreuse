# gormreuse

[![Go Reference](https://pkg.go.dev/badge/github.com/mpyw/gormreuse.svg)](https://pkg.go.dev/github.com/mpyw/gormreuse)
[![Go Report Card](https://goreportcard.com/badge/github.com/mpyw/gormreuse)](https://goreportcard.com/report/github.com/mpyw/gormreuse)
[![Codecov](https://codecov.io/gh/mpyw/gormreuse/graph/badge.svg)](https://codecov.io/gh/mpyw/gormreuse)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

> [!NOTE]
> This project was written by AI (Claude Code).

A Go linter that detects unsafe `*gorm.DB` instance reuse after chain methods.

## Background

GORM's chain methods (`Where`, `Order`, etc.) modify internal state. Reusing the same `*gorm.DB` instance after chain methods can cause query conditions to accumulate unexpectedly.

```go
q := db.Where("active = ?", true)
q.Find(&users)  // SELECT * FROM users WHERE active = true
q.Find(&admins) // Bug: SELECT * FROM users WHERE active = ? AND active = ?
```

## Installation

```bash
go install github.com/mpyw/gormreuse/cmd/gormreuse@latest
```

## Usage

```bash
# Run directly
gormreuse ./...

# Or as a vet tool
go vet -vettool=$(which gormreuse) ./...
```

## Detection Model: Pollute Semantics

This linter uses a "pollute" model inspired by Rust's move semantics. The core concept:

1. **Safe Methods** (`Session`, `WithContext`) return an **immutable** copy
2. **Chain Methods** (all others including finishers) **pollute** the receiver if it's mutable-derived
3. Using a **polluted** mutable instance is a **violation**

### Method Classification

| Category        | Methods                  | Description                    |
| --------------- | ------------------------ | ------------------------------ |
| Safe Methods    | `Session`, `WithContext` | Return new immutable instance  |
| DB Init Methods | `Begin`, `Transaction`   | Create new DB instance         |
| Chain Methods   | All others               | Pollute mutable receiver       |

### Automatic Pollution Sources

The linter conservatively marks `*gorm.DB` as polluted in these scenarios:

| Operation                | Description                                              |
| ------------------------ | -------------------------------------------------------- |
| Interface method call    | `repo.Query(db)` - Can't statically analyze              |
| Channel send             | `ch <- db` - May be received and used elsewhere          |
| Slice/Map storage        | `[]*gorm.DB{db}` - May be accessed elsewhere             |
| Struct field storage     | `&S{db: db}` - May be accessed elsewhere                 |
| Interface conversion     | `interface{}(db)` - May be extracted via type assertion  |
| Non-pure function call   | `helper(db)` - Unless marked with `gormreuse:pure`       |

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

### `gormreuse:pure` (Future)

Mark a function as not polluting its `*gorm.DB` argument:

```go
//gormreuse:pure
func countOnly(db *gorm.DB) int64 {
    var count int64
    db.Count(&count)
    return count
}
```

Note: This directive is planned for future implementation.

## Development

```bash
# Run tests
go test ./...

# Build CLI
go build -o bin/gormreuse ./cmd/gormreuse

# Run linter on a project
./bin/gormreuse ./...
```

## License

MIT License
