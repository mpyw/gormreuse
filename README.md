# gormreuse

[![Go Reference](https://pkg.go.dev/badge/github.com/mpyw/gormreuse.svg)](https://pkg.go.dev/github.com/mpyw/gormreuse)
[![Go Report Card](https://goreportcard.com/badge/github.com/mpyw/gormreuse)](https://goreportcard.com/report/github.com/mpyw/gormreuse)
[![Codecov](https://codecov.io/gh/mpyw/gormreuse/graph/badge.svg)](https://codecov.io/gh/mpyw/gormreuse)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

> [!NOTE]
> This project was written by AI (Claude Code).

A Go linter that detects unsafe [`*gorm.DB`](https://pkg.go.dev/gorm.io/gorm#DB) instance reuse after chain methods.

## Background

[GORM](https://pkg.go.dev/gorm.io/gorm)'s Traditional API chain methods ([`Where`](https://pkg.go.dev/gorm.io/gorm#DB.Where), [`Order`](https://pkg.go.dev/gorm.io/gorm#DB.Order), etc.) modify internal state. Reusing the same [`*gorm.DB`](https://pkg.go.dev/gorm.io/gorm#DB) instance after chain methods can cause query conditions to accumulate unexpectedly.

This issue is documented in [GORM's official guide on Method Chaining](https://gorm.io/docs/method_chaining.html#Reusability-and-Safety). While GORM's Generics API (v1.30.0+) provides a safer alternative, many production codebases still use the Traditional API and will continue to maintain it for years. This linter helps catch these bugs in real-world scenarios.

```go
q := db.Where("active = ?", true)
q.Find(&users)  // SELECT * FROM users WHERE active = true
q.Find(&admins) // Bug: Conditions accumulate unexpectedly
```

## Installation & Usage

### Using [`go install`](https://pkg.go.dev/cmd/go#hdr-Compile_and_install_packages_and_dependencies)

```bash
go install github.com/mpyw/gormreuse/cmd/gormreuse@latest
gormreuse ./...
```

### Using [`go vet`](https://pkg.go.dev/cmd/go#hdr-Report_likely_mistakes_in_packages)

Since gormreuse has no custom flags, it can be run via `go vet`:

```bash
go install github.com/mpyw/gormreuse/cmd/gormreuse@latest
go vet -vettool=$(which gormreuse) ./...
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
> To prevent supply chain attacks, pin to a specific version tag instead of `@latest` in CI/CD pipelines (e.g., `@v0.11.5`).
> All versions prior to v0.11.0 have been retracted due to critical bugs.

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-test` | `true` | Analyze test files (`*_test.go`) — built-in driver flag |
| `-fix` | `false` | Apply suggested fixes automatically — built-in driver flag |

Generated files (containing `// Code generated ... DO NOT EDIT.`) are always excluded and cannot be opted in.

### Examples

```bash
# Exclude test files from analysis
gormreuse -test=false ./...

# Apply automatic fixes
gormreuse -fix ./...
```

## Automatic Fixes

The `-fix` flag enables automatic repair of violations using two complementary strategies:

### Strategy 1: Reassignment (Creating New Roots)

Adds reassignment for non-finisher method calls, creating a new mutable root at each step:

```go
// Before
q := db.Where("base")
q.Where("a")           // VIOLATION: 1st branch (non-finisher)
q.Where("b").Find(nil) // VIOLATION: 2nd branch (finisher)

// After -fix
q := db.Where("base")
q = q.Where("a")       // ← Reassignment creates new root
q.Where("b").Find(nil) // OK: first branch from new root
```

**When applied:**
- Non-finisher methods (chain builders like `Where`, `Order`, `Limit`)
- Result is not assigned to a variable
- Expression used as a statement (not part of a larger expression)

### Strategy 2: Session (Making Roots Immutable)

Adds `.Session(&gorm.Session{})` to make branch roots immutable when reassignment alone isn't sufficient:

```go
// Before
q := db.Where("base")
q.Where("a").Find(nil) // First branch
q.Where("b").Find(nil) // VIOLATION: second branch

// After -fix
q := db.Where("base").Session(&gorm.Session{})  // ← Immutable root
q.Where("a").Find(nil) // OK: independent chain from immutable root
q.Where("b").Find(nil) // OK: independent chain from immutable root
```

**When applied:**
- A root still has 2+ branches after simulating Phase 1 reassignments
- Special handling for Phi nodes (conditional branches): adds Session to each incoming edge

### Combined Example

Both strategies work together for complex cases:

```go
// Before
q := db.Where("base")
q.Where("a")           // non-finisher
q.Where("b").Find(nil) // finisher
q.Where("c")           // non-finisher
q.Where("d").Find(nil) // finisher

// After -fix (both strategies applied)
q := db.Where("base")
q = q.Where("a").Session(&gorm.Session{})  // Reassignment + Session
q.Where("b").Find(nil)
q = q.Where("c")                            // Reassignment
q.Where("d").Find(nil)
```

> [!TIP]
> The fix generator intelligently determines which strategy to apply. Run `gormreuse -fix ./...` to automatically repair all violations in your codebase.

## Detection Model: Mutable Branching

This linter detects when a **mutable [`*gorm.DB`](https://pkg.go.dev/gorm.io/gorm#DB) branches into multiple code paths**. The core concept:

1. **Immutable-returning methods** ([`Session`](https://pkg.go.dev/gorm.io/gorm#DB.Session), [`WithContext`](https://pkg.go.dev/gorm.io/gorm#DB.WithContext), [`Debug`](https://pkg.go.dev/gorm.io/gorm#DB.Debug), etc.) return an **immutable** instance that can branch freely
2. **All other methods** on a mutable instance create a **branch** that consumes the instance
3. **Second branch** from the same mutable root is a **violation**

### Method Classification

> [!IMPORTANT]
> This linter analyzes the **Traditional API** ([`*gorm.DB`](https://pkg.go.dev/gorm.io/gorm#DB)) only. The Generics API ([`gorm.G[T]`](https://pkg.go.dev/gorm.io/gorm#G), available since v1.30.0) returns different types ([`Interface[T]`](https://pkg.go.dev/gorm.io/gorm#Interface), [`ChainInterface[T]`](https://pkg.go.dev/gorm.io/gorm#ChainInterface)) and is automatically excluded from analysis.

| Category                    | Methods                  | Description                    |
| --------------------------- | ------------------------ | ------------------------------ |
| Immutable-Returning Methods | [`Session`](https://pkg.go.dev/gorm.io/gorm#DB.Session), [`WithContext`](https://pkg.go.dev/gorm.io/gorm#DB.WithContext), [`Debug`](https://pkg.go.dev/gorm.io/gorm#DB.Debug), [`Open`](https://pkg.go.dev/gorm.io/gorm#Open), [`Begin`](https://pkg.go.dev/gorm.io/gorm#DB.Begin), [`Transaction`](https://pkg.go.dev/gorm.io/gorm#DB.Transaction) | Return new immutable instance |
| All Other Methods           | [`Where`](https://pkg.go.dev/gorm.io/gorm#DB.Where), [`Find`](https://pkg.go.dev/gorm.io/gorm#DB.Find), [`Count`](https://pkg.go.dev/gorm.io/gorm#DB.Count), [`Order`](https://pkg.go.dev/gorm.io/gorm#DB.Order), etc. | Create a branch from receiver |

### Automatic Pollution Sources

The linter conservatively marks [`*gorm.DB`](https://pkg.go.dev/gorm.io/gorm#DB) as polluted in these scenarios:

| Operation                | Description                                              |
| ------------------------ | -------------------------------------------------------- |
| Interface method call    | `repo.Query(db)` - Can't statically analyze              |
| Channel send             | `ch <- db` - May be received and used elsewhere          |
| Slice/Map storage        | `[]*gorm.DB{db}` - May be accessed elsewhere             |
| Interface conversion     | `interface{}(db)` - May be extracted via type assertion  |
| Non-pure function call   | `helper(db)` - Unless marked with `//gormreuse:pure`     |
| Struct field access      | `h.db.Find(nil)` - Traces back to the stored value       |

Note: Simple struct literal storage (`_ = &S{db: q}`) without actual field usage does NOT pollute.

### Examples

#### Safe: Branching from immutable

```go
// Session at end creates immutable - safe to branch multiple times
q := db.Where("active = ?", true).Session(&gorm.Session{})
q.Count(&count)  // OK - first branch from q
q.Find(&users)   // OK - q is immutable, can branch freely

// Each branch from immutable creates independent mutable chains
q := db.Where("base").Session(&gorm.Session{})
q.Where("a").Find(&users1)  // OK - branch 1 (independent chain)
q.Where("b").Find(&users2)  // OK - branch 2 (independent chain)
```

#### Violation: Multiple branches from mutable

```go
// Second branch from mutable is a violation
q := db.Where("active = ?", true)  // q is mutable
q.Find(&users)   // first branch from q - OK
q.Count(&count)  // VIOLATION: second branch from q

// Even without "finisher" - any method creates a branch
q := db.Where("x")
q.Where("a")     // first branch from q - OK
q.Where("b")     // VIOLATION: second branch from q

// Session in middle doesn't help - result is still mutable
q := db.Session(&gorm.Session{}).Where("x")  // q is mutable!
q.Find(&users)   // first branch - OK
q.Count(&count)  // VIOLATION: second branch

// Using immutable-returning method on polluted value is also a violation
q := db.Where("x")
q.Find(&users)                       // first branch - OK
q.Session(&gorm.Session{}).Count(&c) // VIOLATION: second branch from q
```

> [!IMPORTANT]
> **Chaining without reassignment is a violation!** Each statement using the same variable creates a separate branch:
> ```go
> q := db.Where("base")
> q.Where("a")           // first branch - OK
> q.Where("b")           // VIOLATION: second branch
> q.Find(&users)         // VIOLATION: third branch
> ```
> **Solution**: Reassign the result or use method chaining in a single expression:
> ```go
> // Option 1: Reassign each step
> q := db.Where("base")
> q = q.Where("a")
> q = q.Where("b")
> q.Find(&users)         // OK - first branch from final q
>
> // Option 2: Single chained expression
> db.Where("base").Where("a").Where("b").Find(&users)  // OK - single chain
> ```

#### Safe: Variable reassignment

Variable reassignment creates a **new mutable root**, so the variable can be used fresh:

```go
q := db.Where("x")
q.Find(&users)        // first branch from original q - OK

q = db.Where("y")     // reassignment creates NEW mutable root
q.Find(&admins)       // first branch from new q - OK

q = db.Where("z")     // another reassignment
q.Count(&count)       // first branch from newest q - OK
```

This is safe because internally, the linter uses [SSA (Static Single Assignment)](https://pkg.go.dev/golang.org/x/tools/go/ssa) form where each assignment creates a distinct value. The new value has no relationship to the previous one.

```go
// Reassignment in loops is also safe
for _, filter := range filters {
    q := db.Where(filter)  // new mutable root each iteration
    q.Find(&results)       // OK - first branch in this iteration
}

// Conditional reassignment
q := db.Where("base")
if condition {
    q = db.Where("alt")    // reassignment on this path
}
q.Find(&users)             // OK - first branch from whichever root
```

## Directives

- Directives can be combined with commas: `//gormreuse:pure,immutable-return`
- Trailing comments use `//`: `//gormreuse:ignore // reason here`

### `//gormreuse:ignore`

Suppress warnings for a specific line:

```go
q := db.Where("active = ?", true)
q.Find(&users)
//gormreuse:ignore // intentional reuse for pagination
q.Count(&count)  // Suppressed
```

Or suppress for an entire function:

```go
//gormreuse:ignore
func legacyCode(db *gorm.DB) {
    // All violations in this function are suppressed
}
```

Or suppress for an entire file (place before package declaration):

```go
//gormreuse:ignore

package mypackage

// All violations in this file are suppressed
```

> [!WARNING]
> Unused `//gormreuse:ignore` directives are reported as warnings for line-level and function-level ignores. This helps keep the codebase clean by identifying stale ignore comments. File-level ignores do not trigger unused warnings.

### `//gormreuse:pure`

Mark a function as not polluting its [`*gorm.DB`](https://pkg.go.dev/gorm.io/gorm#DB) argument:

```go
//gormreuse:pure
func withTenant(db *gorm.DB, tenantID int) *gorm.DB {
    return db.Session(&gorm.Session{}).Where("tenant_id = ?", tenantID)
}
```

> [!TIP]
> All user-defined functions/methods that accept or return [`*gorm.DB`](https://pkg.go.dev/gorm.io/gorm#DB) are treated as polluting by default. You must add `//gormreuse:pure` to any helper function that safely wraps [`*gorm.DB`](https://pkg.go.dev/gorm.io/gorm#DB) without polluting it.

> [!WARNING]
> The linter validates that functions marked `//gormreuse:pure` actually satisfy the pure contract:
>
> ```go
> //gormreuse:pure
> func badPure(db *gorm.DB) {
>     db.Where("x")  // ERROR: pure function pollutes *gorm.DB argument by calling Where
> }
> ```
>
> Valid pure functions must:
> - NOT call polluting methods ([`Where`](https://pkg.go.dev/gorm.io/gorm#DB.Where), [`Find`](https://pkg.go.dev/gorm.io/gorm#DB.Find), etc.) directly on [`*gorm.DB`](https://pkg.go.dev/gorm.io/gorm#DB) **arguments**
> - MAY call polluting methods on **immutable values** (e.g., `db.Session(&gorm.Session{}).Where(...)` is OK)
>
> **Note**: Pure functions MAY return mutable [`*gorm.DB`](https://pkg.go.dev/gorm.io/gorm#DB). Callers must treat the return value as potentially mutable:
> ```go
> q := withTenant(db, 1)  // q is mutable!
> q.Find(&users)          // first branch - OK
> q.Count(&count)         // VIOLATION - second branch from mutable q
> ```

### `//gormreuse:immutable-return`

Mark a function as returning an **immutable** [`*gorm.DB`](https://pkg.go.dev/gorm.io/gorm#DB) (like builtin [`Session`](https://pkg.go.dev/gorm.io/gorm#DB.Session), [`WithContext`](https://pkg.go.dev/gorm.io/gorm#DB.WithContext)):

```go
//gormreuse:immutable-return
func GetDB() *gorm.DB {
    return globalDB.Session(&gorm.Session{})
}

func useIt() {
    db := GetDB()
    db.Where("x").Find(&users)
    db.Where("y").Find(&admins) // OK - GetDB returns immutable
}
```

> [!TIP]
> Use this directive for DB connection helpers that return a fresh, immutable [`*gorm.DB`](https://pkg.go.dev/gorm.io/gorm#DB) instance. This allows callers to reuse the returned value freely without worrying about pollution.

### `//gormreuse:pure,immutable-return`

The recommended pattern for DB connection helpers - combines both guarantees:

```go
//gormreuse:pure,immutable-return
func GetTenantDB(db *gorm.DB, tenantID int) *gorm.DB {
    return db.Session(&gorm.Session{}).Where("tenant_id = ?", tenantID)
}

func useIt(db *gorm.DB) {
    tenant1 := GetTenantDB(db, 1)
    tenant2 := GetTenantDB(db, 2)
    tenant1.Where("x").Find(&users)   // OK
    tenant2.Where("y").Find(&admins)  // OK
    tenant1.Count(&count)             // OK - immutable-return
    db.Find(&all)                     // OK - pure (db not polluted)
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
