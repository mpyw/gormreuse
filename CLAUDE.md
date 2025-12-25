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

### Detection Model: Mutable Branching

The linter detects when a mutable `*gorm.DB` branches into multiple code paths:

1. **Immutable-returning methods** ([`Session`](https://pkg.go.dev/gorm.io/gorm#DB.Session), [`WithContext`](https://pkg.go.dev/gorm.io/gorm#DB.WithContext), [`Debug`](https://pkg.go.dev/gorm.io/gorm#DB.Debug), [`Open`](https://pkg.go.dev/gorm.io/gorm#Open), [`Begin`](https://pkg.go.dev/gorm.io/gorm#DB.Begin), [`Transaction`](https://pkg.go.dev/gorm.io/gorm#DB.Transaction)) return a new immutable instance
2. **All other methods** on a mutable instance create a **branch** that consumes the instance
3. **Second branch** from the same mutable root is a **violation**

### Directives

- `//gormreuse:ignore` - Suppress warnings for the next line or same line
- `//gormreuse:pure` - Mark function/method as not polluting its `*gorm.DB` argument
- `//gormreuse:immutable-return` - Mark function/method as returning immutable `*gorm.DB` (like Session/WithContext)

Directives can be combined with commas: `//gormreuse:pure,immutable-return`

Trailing comments use `//`: `//gormreuse:ignore // reason here`

> **Important**: All user-defined functions/methods that accept or return `*gorm.DB` are treated as polluting by default. Use `//gormreuse:pure` to mark functions that don't pollute arguments, and `//gormreuse:immutable-return` to mark functions whose return value can be safely reused (like DB connection helpers).

## Architecture

```
gormreuse/
├── analyzer.go                 # Public analyzer definition (go/analysis entry point)
├── analyzer_test.go            # Integration tests using analysistest
├── cmd/gormreuse/main.go       # CLI entry point (singlechecker)
│
├── internal/                   # Internal implementation
│   ├── analyzer.go             # SSA analysis orchestrator (RunSSA entry point)
│   │
│   ├── directive/              # Comment directive handling
│   │   ├── directive.go        # Directive detection (hasDirective, IsIgnore/IsPure)
│   │   ├── ignore.go           # //gormreuse:ignore - IgnoreMap, unused tracking
│   │   └── pure.go             # //gormreuse:pure - PureFuncSet, function key matching
│   │
│   ├── ssa/                    # SSA-based analysis (modular subpackages)
│   │   ├── analyzer.go         # Analyzer - orchestrates analysis phases
│   │   │
│   │   ├── tracer/             # Value tracing to find mutable roots
│   │   │   └── root.go         # RootTracer - traces SSA values to mutable origins
│   │   │
│   │   ├── pollution/          # Pollution state tracking
│   │   │   └── tracker.go      # Tracker - records uses, detects violations
│   │   │
│   │   ├── cfg/                # Control flow graph analysis
│   │   │   └── analyzer.go     # Analyzer - loop detection, reachability
│   │   │
│   │   ├── handler/            # SSA instruction handlers
│   │   │   └── call.go         # Handlers for Call, Go, Defer, Send, Store, etc.
│   │   │
│   │   └── purity/             # Pure function validation for //gormreuse:pure
│   │       └── validator.go    # ValidateFunction - checks pure contracts
│   │
│   └── typeutil/               # Type utilities
│       └── gorm.go             # IsGormDB, IsImmutableReturningBuiltin
│
├── testdata/src/               # Test fixtures
│   ├── gormreuse/              # Analyzer test cases
│   │   ├── basic.go            # Basic patterns
│   │   ├── advanced.go         # Complex patterns
│   │   ├── evil.go             # Edge cases with [LIMITATION] markers
│   │   ├── ignore.go           # //gormreuse:ignore tests
│   │   └── directive_validation.go  # Directive tests (pure, immutable-return)
│   └── gorm.io/gorm/           # GORM stub for testing
│
└── testdata/
    ├── cmd/gengolden/          # Golden file generator
    └── e2e/                    # SQL behavior verification (separate module)
```

### Analysis Pipeline

```
┌─────────────────────────────────────────────────────────────────────────┐
│                         analyzer.go (public)                            │
│  Analyzer.Run() → buildssa → internal.RunSSA()                         │
└─────────────────────────────────────────────────────────────────────────┘
                                    │
                                    ▼
┌─────────────────────────────────────────────────────────────────────────┐
│                      internal/analyzer.go                               │
│  For each function:                                                     │
│    1. Check ignores (directive.IgnoreMap)                              │
│    2. Validate pure functions (purity.ValidateFunction)                │
│    3. Detect violations (ssa.Analyzer)                                 │
└─────────────────────────────────────────────────────────────────────────┘
                                    │
                                    ▼
┌─────────────────────────────────────────────────────────────────────────┐
│                        ssa/analyzer.go                                  │
│  Three-phase analysis:                                                  │
│    PHASE 1: TRACKING    - Process instructions, record usages          │
│    PHASE 2: DETECTION   - DetectViolations() via CFG reachability      │
│    PHASE 3: COLLECTION  - Return violations list                       │
└─────────────────────────────────────────────────────────────────────────┘
                                    │
                    ┌───────────────┼───────────────┐
                    ▼               ▼               ▼
              ┌──────────┐   ┌──────────┐   ┌──────────┐
              │  tracer  │   │ pollution│   │   cfg    │
              │ RootTracer│   │ Tracker  │   │ Analyzer │
              └──────────┘   └──────────┘   └──────────┘
                    │               │               │
                    └───────────────┼───────────────┘
                                    ▼
                            ┌──────────────┐
                            │   handler    │
                            │  Dispatch()  │
                            └──────────────┘
```

### Design Patterns

1. **Composition over Inheritance**: `Analyzer` composes `RootTracer`, `cfg.Analyzer`, `pollution.Tracker`
2. **Type Switch Dispatch**: `handler.Dispatch()` routes SSA instructions to handlers via type switch
3. **Two-Phase Detection**: First collect all uses, then detect violations via CFG reachability
4. **Separation of Concerns**:
   - `tracer/`: WHERE is the mutable root? (value tracing)
   - `pollution/`: WHAT uses exist and are they violations? (state tracking)
   - `cfg/`: CAN use A reach use B? (control flow)
   - `handler/`: HOW to process each instruction type? (dispatch)

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

Reassignment Behavior:
  q := db.Where("x")   <- SSA: q_1 = call db.Where("x")
  q.Find(nil)          <- First branch from q_1 - OK
  q = db.Where("y")    <- SSA: q_2 = call db.Where("y") (NEW SSA value)
  q.Find(nil)          <- First branch from q_2 - OK (no relation to q_1)

  Each assignment creates a new SSA value, so reassignment = new mutable root.
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
cd testdata/e2e && go test -v

# Generate golden files for suggested fixes
go run ./testdata/cmd/gengolden/main.go
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

The `testdata/e2e/` directory contains tests that verify actual GORM SQL behavior using sqlmock. This is a separate Go module to avoid dependency conflicts. These tests document GORM's clone/pollution behavior.

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
- **IIFE/closure stored result**: When IIFE/closure result is stored (not directly chained), branch tracking differs from runtime order

These are documented in `testdata/src/gormreuse/evil.go` with `[LIMITATION]` markers.

### IIFE/Closure Stored Result Limitation

When a closure result is stored (in a variable or struct field) rather than directly chained to a gorm method, branch tracking may differ from runtime behavior:

```go
// OK: IIFE result directly chained
_ = func() *gorm.DB { return q.Where("y") }().Find(nil)  // ONE branch
q.Count(nil)  // violation (second branch)

// LIMITATION: IIFE result stored - both q and q2 are treated as branching from q
q2 := func() *gorm.DB { return q.Where("y") }()  // First branch from q
q2.Find(nil)  // violation (q2 traces back to q, which is already used)
q.Count(nil)  // also violation

// LIMITATION: Stored closure with interleaved use
fn := func() *gorm.DB { return q.Where("y") }
q2 := fn()
q.Count(nil)  // Expected: violation. Actual: may not detect correctly
q2.Find(nil)  // Expected: OK. Actual: may report as violation
```

**Root Cause**: SSA analysis processes closure bodies before closure call sites. The branch tracking order differs from runtime execution order.

## Related Projects

- [goroutinectx](https://github.com/mpyw/goroutinectx) - Goroutine context propagation linter
- [zerologlintctx](https://github.com/mpyw/zerologlintctx) - Zerolog context propagation linter
- [ctxweaver](https://github.com/mpyw/ctxweaver) - Code generator for context-aware instrumentation
