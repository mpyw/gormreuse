# CLAUDE.md

This file provides guidance to Claude Code when working with code in this repository.

## Project Overview

**gormreuse** is a Go linter that detects unsafe [`*gorm.DB`](https://pkg.go.dev/gorm.io/gorm#DB) instance reuse after chain methods. It uses [SSA](https://pkg.go.dev/golang.org/x/tools/go/ssa) (Static Single Assignment) form to track `*gorm.DB` values through variable assignments and method chains.

### Core Concept: Mutable Branching Problem

**The fundamental problem this linter detects is when a mutable `*gorm.DB` branches into multiple code paths.**

GORM's chain methods (like `Where`, `Order`, `Limit`) create **shallow clones** that share the internal `Statement` object. When multiple chains branch from the same mutable root, they interfere with each other:

```go
q := db.Where("x")           // q is mutable (derived from chain method)
branch1 := q.Where("a")      // Branch 1: shares Statement with q
branch2 := q.Where("b")      // Branch 2: PROBLEM! Also shares Statement
// branch1 and branch2 interfere - conditions accumulate unexpectedly
```

**"Reused" means "second branch from the same mutable root"**. The linter warns when a mutable value is used to create multiple independent chains:

```go
q := db.Where("x")
q.Where("a").Find(nil)       // First branch from q - OK
q.Where("b")                 // Second branch - VIOLATION! (even without Find)
```

#### IIFE Chains Count as Single Branch

When a mutable value is used inside an IIFE that returns the chain, and that chain is immediately consumed, it counts as **one branch**, not multiple:

```go
q := db.Where("x")
_ = func() *gorm.DB {
    return q.Where("y")
}().Find(nil)                // First branch (entire IIFE chain) - OK
q.Count(nil)                 // Second branch - VIOLATION!
```

The IIFE chain `q.Where("y")...Find(nil)` is a single chain from q. Only `q.Count(nil)` is a separate branch.

**Solution**: Use `Session()` to create isolation before branching:

```go
q := db.Session(&gorm.Session{}).Where("x")  // q is immutable (Session creates isolation)
branch1 := q.Where("a")                       // OK: creates new Statement
branch2 := q.Where("b")                       // OK: creates new Statement
```

### Detection Model: Pollute Semantics

The linter uses a "pollute" model inspired by Rust's move semantics:

1. **Immutable-returning methods** ([`Session`](https://pkg.go.dev/gorm.io/gorm#DB.Session), [`WithContext`](https://pkg.go.dev/gorm.io/gorm#DB.WithContext), [`Debug`](https://pkg.go.dev/gorm.io/gorm#DB.Debug), [`Open`](https://pkg.go.dev/gorm.io/gorm#Open), [`Begin`](https://pkg.go.dev/gorm.io/gorm#DB.Begin), [`Transaction`](https://pkg.go.dev/gorm.io/gorm#DB.Transaction)) return a new immutable instance
2. **Chain Methods** (all others including finishers) pollute the receiver if it's mutable-derived
3. Using a polluted mutable instance (second branch) is a violation

### Directives

- `//gormreuse:ignore` - Suppress warnings for the next line or same line
- `//gormreuse:pure` - Mark function/method as not polluting its `*gorm.DB` argument

> **Important**: All user-defined functions/methods that accept or return `*gorm.DB` are treated as polluting by default. You must add `//gormreuse:pure` to any helper function that safely wraps `*gorm.DB` without polluting it.

## Architecture

```
gormreuse/
├── cmd/
│   └── gormreuse/              # CLI entry point (singlechecker)
│       └── main.go
├── docs/
│   └── PURE_VALIDATION_DESIGN.md  # Design doc for 3-state purity model
├── internal/                   # SSA-based analysis (modular design)
│   ├── analyzer.go             # Analyzer orchestrator, entry point
│   ├── exports.go              # Re-exports for backward compatibility
│   ├── purity_adapter.go       # Adapter for purity validation
│   ├── typeutil/               # Type utilities
│   │   └── gorm.go             # GORM type checks, immutable method list
│   ├── directive/              # Comment directive handling
│   │   ├── common.go           # Shared directive utilities
│   │   ├── ignore.go           # //gormreuse:ignore handling
│   │   └── pure.go             # //gormreuse:pure handling
│   └── ssa/                    # SSA analysis package
│       ├── doc.go              # Package documentation
│       ├── tracer.go           # SSATracer, RootTracer
│       ├── pollution.go        # CFGAnalyzer, PollutionTracker
│       ├── handler.go          # Instruction handlers (type switch dispatch)
│       └── purity/             # [WIP] 3-state purity analysis
│           ├── state.go        # PurityState type (Clean/Polluted/Depends)
│           ├── inference.go    # Purity state inference (Inferencer)
│           └── validator.go    # Pure function contract validator
├── testdata/
│   └── src/
│       ├── gormreuse/          # Test fixtures
│       │   ├── basic.go        # Basic patterns
│       │   ├── advanced.go     # Complex patterns
│       │   ├── evil.go         # Edge cases, closures, defer, goroutines
│       │   ├── ignore.go       # Directive tests
│       │   └── pure_validation.go  # Pure function validation tests
│       └── gorm.io/gorm/       # Library stub
├── e2e/
│   └── internal/               # SQL behavior verification tests (separate module)
│       └── realexec_test.go
├── analyzer.go                 # Public analyzer definition
├── analyzer_test.go            # Integration tests
└── README.md
```

### Design Patterns

The codebase uses several design patterns inspired by [zerologlintctx](https://github.com/mpyw/zerologlintctx):

1. **Composition over Inheritance**: `Analyzer` composes `RootTracer`, `CFGAnalyzer`, `PollutionTracker`
2. **Type Switch Dispatch**: `DispatchInstruction` routes SSA instructions to handlers via type switch (O(1) dispatch)
3. **Validated State Pattern**: `traceResult` type with explicit states (`Immutable`, `MutableRoot`)
4. **Mechanism vs Policy Separation**:
   - `SSATracer`: HOW to traverse SSA values (mechanism)
   - `RootTracer`: WHAT constitutes a mutable root (policy)

### Key Design Decisions

1. **SSA-based analysis**: Uses `go/ssa` to track `*gorm.DB` values through method chains
2. **Pollute model**: Tracks pollution state of mutable values (branching = pollution)
3. **Branch detection**: Detects when mutable `*gorm.DB` is used in multiple code paths
4. **Mutable root finding**: Traces back to find the origin of each chain
5. **Conservative approach**: Prefer false positives over false negatives (reduces false-negatives)
6. **IIFE return tracing**: Traces through immediately invoked function expressions to find mutable roots
7. **Method value tracking**: Detects bound methods (e.g., `find := q.Find; find(nil)`) via `$bound` suffix in SSA

### Pollution Sources (Safe Side)

The linter marks `*gorm.DB` as polluted in these scenarios:

- **Interface method calls**: Assumed to pollute (can't statically analyze)
- **Channel send**: `ch <- db` marks db as polluted
- **Slice/Array storage**: `[]*gorm.DB{db}` marks db as polluted
- **Map storage**: `map[string]*gorm.DB{"k": db}` marks db as polluted
- **Interface conversion**: `interface{}(db)` marks db as polluted (type assertion may extract)
- **Function arguments**: Non-pure functions receiving `*gorm.DB` assumed to pollute
- **Struct field access**: `h.field.Find(nil)` traces back to the original value stored in field

Note: Simple struct literal storage (`_ = &S{db: q}`) without actual field usage does NOT pollute.
The linter tracks actual usage through struct fields, not just storage.

### SSA Tracking Strategy

```
Pollute Model:
  Immutable: Created by Immutable-returning Methods - can be reused freely
  Mutable:   Created by Chain Methods - gets polluted on first use (any branch)

Branch Detection:
  q := db.Where("x")
  q.Where("a").Find(nil)  <- First branch from q - OK
  q.Where("b")            <- Second branch from q - VIOLATION (even without Find)
  q.Count(nil)            <- Third branch from q - VIOLATION

Mutable Root Finding:
  q := db.Where("x").Session(...)  <- q is immutable (Session at end)
  q := db.Session(...).Where("x")  <- q is mutable (Chain after Session)

IIFE Return Tracing (single chain):
  q := db.Where("x")
  _ = func() *gorm.DB {
    return q.Where("y")
  }().Find(nil)           <- First branch (IIFE chain = single branch) - OK
  q.Count(nil)            <- Second branch - VIOLATION

  IIFE that returns a chain and is immediately consumed = ONE branch.
  Only subsequent uses of q from outside the IIFE are separate branches.

Bound Method Tracking:
  find := q.Find    <- MakeClosure with receiver q in Bindings[0]
  find(nil)         <- SSA: Call with Value=*ssa.MakeClosure, Fn.Name()="Find$bound"
                   <- Receiver extracted from Bindings[0] for pollution tracking
```

## Development Commands

```bash
# Run tests
go test ./...

# Run tests with verbose output
go test -v ./...

# Build CLI
go build -o bin/gormreuse ./cmd/gormreuse

# Run linter on itself
go vet -vettool=./bin/gormreuse ./...

# Run E2E tests (SQL behavior verification)
cd e2e/internal && go test -v ./...
```

## Testing Strategy

- Use `analysistest` for all analyzer tests
- Test fixtures use `// want` comments for expected diagnostics
- Test structure:
  - `===== SHOULD REPORT =====` - Cases that should trigger warnings
  - `===== SHOULD NOT REPORT =====` - Negative cases

### Testdata Organization

```
testdata/src/gormreuse/
├── basic.go         # Basic reuse patterns, Session at end/middle
├── advanced.go      # Derived variables, helper functions, conditional reuse
├── evil.go          # Edge cases: closures, defer, goroutines, struct fields, loops
└── ignore.go        # //gormreuse:ignore directive tests
```

### E2E Tests

The `e2e/internal/` directory contains tests that verify actual GORM SQL behavior using sqlmock. This is a separate Go module to avoid dependency conflicts. These tests document GORM's clone/pollution behavior.

## Code Style

- Follow standard Go conventions
- Use `go/analysis` framework
- Unexported types by default; only export what's needed

## Refactoring Guidelines

When refactoring code, follow these rules strictly:

1. **Main code refactoring**: Tests must NEVER fail
   - Run `go test ./...` after each change
   - If tests fail, revert and try a different approach

2. **Test code refactoring**: Coverage must NEVER drop
   - Check coverage before: `go test -coverprofile=/tmp/before.out -coverpkg=./... .`
   - Check coverage after: `go test -coverprofile=/tmp/after.out -coverpkg=./... .`
   - Compare with `go tool cover -func=/tmp/before.out` vs `go tool cover -func=/tmp/after.out`

## Known Limitations

- **Closure assignment**: `f := func() { q = db.Where(...) }; f()` - cross-closure assignment not tracked
- **Defer inside for loop**: `for range items { defer func() { q.Find(nil) }() }` - closure deferred multiple times not fully tracked
- **Nested defer/goroutine**: `go func() { defer q.Find(nil) }()` - deep nested defer/goroutine chains not fully tracked

These are documented in `testdata/src/gormreuse/evil.go` with `[LIMITATION]` markers.

## Known Bugs

### IIFE Return Chain False Positive

**Status**: Bug in analyzer and test expectations

The analyzer incorrectly reports violations on IIFE return chains that should count as a single branch:

```go
q := db.Where("x")
_ = func() *gorm.DB {
    return q.Where("y")
}().Find(nil)    // BUG: Currently reports violation (should be OK - first branch)
q.Count(nil)     // Correctly reports violation (second branch)
```

**Root Cause**: The analyzer processes closures as separate functions. Inside the IIFE, `q.Where("y")` is treated as a "terminal" call (its referrer is `return`, not another gorm method), which marks `q` as polluted before the outer function processes `.Find(nil)`.

**Affected Test Functions** (in `evil.go`):
- `iifeReturnChain` (line 686)
- `chainedIIFE` (line 2024)
- `tripleNestedIIFE` (line 1936)
- `tripleNestedIIFEWithBranch` (line 1950)
- `iifeMultipleReturns` (line 1972)
- `structFieldIIFE` (line 2010)
- `iifeWithPhiNode` (line 2061)

**Fix Required**: The analyzer should recognize that IIFE return chains consumed immediately are a single branch, not separate branches.

## Work in Progress

### Pure Function Validation (3-State Model)

**Design Document**: [`docs/PURE_VALIDATION_DESIGN.md`](docs/PURE_VALIDATION_DESIGN.md)

The `//gormreuse:pure` directive validation is being enhanced with a 3-state purity model:

| State | Meaning | Example |
|-------|---------|---------|
| `Clean` | Always immutable | `db.Session(&gorm.Session{})` |
| `Polluted` | Tainted, unsafe | `db.Where("x")` after terminal use |
| `Depends(param)` | Depends on argument | `return db` (identity function) |

This enables accurate validation of pure functions that return their arguments unchanged:

```go
//gormreuse:pure
func identity(db *gorm.DB) *gorm.DB {
    return db  // OK: Depends(db) - doesn't pollute, state depends on caller
}
```

**Implementation Status**: See `docs/PURE_VALIDATION_DESIGN.md` for detailed design and progress.

## Related Projects

- [goroutinectx](https://github.com/mpyw/goroutinectx) - Goroutine context propagation linter
- [zerologlintctx](https://github.com/mpyw/zerologlintctx) - Zerolog context propagation linter
- [ctxweaver](https://github.com/mpyw/ctxweaver) - Code generator for context-aware instrumentation
