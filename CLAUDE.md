# CLAUDE.md

This file provides guidance to Claude Code when working with code in this repository.

## Project Overview

**gormreuse** is a Go linter that detects unsafe [`*gorm.DB`](https://pkg.go.dev/gorm.io/gorm#DB) instance reuse after chain methods. It uses [SSA](https://pkg.go.dev/golang.org/x/tools/go/ssa) (Static Single Assignment) form to track `*gorm.DB` values through variable assignments and method chains.

### Detection Model: Pollute Semantics

The linter uses a "pollute" model inspired by Rust's move semantics:

1. **Safe Methods** ([`Session`](https://pkg.go.dev/gorm.io/gorm#DB.Session), [`WithContext`](https://pkg.go.dev/gorm.io/gorm#DB.WithContext)) return an immutable copy
2. **Chain Methods** (all others including finishers) pollute the receiver if mutable
3. Using a polluted mutable instance is a violation

### Directives

- `//gormreuse:ignore` - Suppress warnings for the next line or same line
- `//gormreuse:pure` - Mark function as not polluting its `*gorm.DB` argument

## Architecture

```
gormreuse/
├── cmd/
│   └── gormreuse/              # CLI entry point (singlechecker)
│       └── main.go
├── internal/                   # SSA-based analysis (modular design)
│   ├── analyzer.go             # Analyzer orchestrator, entry point
│   ├── tracing.go              # SSATracer - common SSA traversal patterns
│   ├── root_tracer.go          # RootTracer - mutable root detection
│   ├── pollution_tracker.go    # PollutionTracker - pollution state tracking
│   ├── cfg_analyzer.go         # CFGAnalyzer - control flow graph analysis
│   ├── instruction_handlers.go # InstructionHandler - Strategy pattern handlers
│   ├── types.go                # Type utilities, method classification
│   └── ignore.go               # Ignore directive handling
├── testdata/
│   └── src/
│       ├── gormreuse/          # Test fixtures
│       │   ├── basic.go        # Basic patterns
│       │   ├── advanced.go     # Complex patterns
│       │   ├── evil.go         # Edge cases, closures, defer, goroutines
│       │   └── ignore.go       # Directive tests
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
2. **Strategy Pattern**: `InstructionHandler` interface with `CallHandler`, `GoHandler`, `DeferHandler`, etc.
3. **Validated State Pattern**: `traceResult` type with explicit states (`Immutable`, `MutableRoot`)
4. **Mechanism vs Policy Separation**:
   - `SSATracer`: HOW to traverse SSA values (mechanism)
   - `RootTracer`: WHAT constitutes a mutable root (policy)

### Key Design Decisions

1. **SSA-based analysis**: Uses `go/ssa` to track `*gorm.DB` values through method chains
2. **Pollute model**: Tracks pollution state of mutable values
3. **Terminal call detection**: Only processes calls that consume the chain (not chain construction)
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
  Immutable: Created by Safe Methods - can be reused freely
  Mutable:   Created by Chain Methods - gets polluted on first terminal use

Terminal Call Detection:
  q.Find(&users)              <- Terminal: result not used in chain
  q.Where("...").Find(&users) <- Where is non-terminal, Find is terminal

Mutable Root Finding:
  q := db.Where("x").Session(...)  <- q is immutable (Session at end)
  q := db.Session(...).Where("x")  <- q is mutable (Chain after Session)

IIFE Return Tracing:
  _ = func() *gorm.DB {
    return q.Where("x")
  }().Find(nil)  <- Traces through IIFE return to find q as mutable root

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

## Related Projects

- [goroutinectx](https://github.com/mpyw/goroutinectx) - Goroutine context propagation linter
- [zerologlintctx](https://github.com/mpyw/zerologlintctx) - Zerolog context propagation linter
- [ctxweaver](https://github.com/mpyw/ctxweaver) - Code generator for context-aware instrumentation
