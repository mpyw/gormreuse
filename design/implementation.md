# Implementation notes

This file records design decisions and implementation details. The short, always-loaded instructions are in [AGENTS.md](../AGENTS.md).

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
4. A **use of a `Session`/`Debug` copy** after its mutable root changed is a **violation** (see below)

#### Session and Debug copies (#174)

The name-based "immutable-returning" class is not exact. In GORM v1.31, `Session` builds a new `*DB` with `Statement: db.Statement` and clones the Statement only when `Context`, `PrepareStmt` or `SkipHooks` is set. `Debug` is `getInstance()` followed by `Session` with only a `Logger`, and `getInstance()` returns the receiver itself when it is `clone == 0`. The E2E tests in `e2e/internal` (`TestCopyOfMidChain_*`, `TestSessionInitialized_*`) check each claim below.

| Call on a `clone == 0` receiver | Effect | Model |
| --- | --- | --- |
| `Session(&gorm.Session{})`, `Debug()`, `Session` with only other options | Copy points at the receiver's Statement. Each use of the copy forks its own Statement (`clone == 2`), so it never changes the receiver. But it reads the receiver's Statement as it is at that moment. | Pure use of the receiver's root at the call. Each later use of the copy is an **alias use** of that root. |
| `WithContext`, `Session` with `Context`, `PrepareStmt`, `SkipHooks` | Statement cloned at the call | Pure use only (unchanged) |
| `Session` with `NewDB` | `clone == 1`: each use starts a new Statement | Pure use only (unchanged) |
| `Session` with `Initialized` | Returns `tx.getInstance()`: a `clone == 0` handle with its own copy | Pure use of the receiver; the result is a **new mutable root** |

`typeutil.ImmutableBuiltinEffect` decides this from the `&gorm.Session{...}` literal. In SSA the literal is an `Alloc` with one `FieldAddr`+`Store` per set field. Any other use of the `Alloc`, a store in another block than the `Alloc`, or a non-constant bool makes the field unknown. An unknown config is assumed to both share and return a mutable value. A `Context` set to any non-constant value counts as set, matching the existing trust in `WithContext(ctx)`.

`RootTracer.FindAliasedRoots` finds the copies a value reaches through `FindAllMutableRoots` (so Phi, variables and closure captures work) and returns each copy's receiver root, following copies of copies. The call handler records an alias use for a gorm method receiver, a bound-method receiver, and a `*gorm.DB` argument of a non-gorm function. `DetectViolations` reports an alias use when a polluting **or assignment** use of the root lies between the copy and the use and reaches the use. Assignment uses count here because `q = q.Where(...)` changes a `clone == 0` Statement in place. A change before the copy is not counted: it already makes the `Session`/`Debug` call itself a second branch, so counting it again would duplicate that report.

**Rejected: count the copy as a branch of the receiver.** The issue suggested this. It would also report `q.Session(&gorm.Session{}).Count(); q.Session(&gorm.Session{}).Find()` and "copy used, then the original changed", which are safe at runtime (`TestSessionBeforeFinisher`, `TestCopyOfMidChain_UseDoesNotChangeOriginal`). The alias-use model reports only the order that leaks.

Alias violations carry no suggested fix. The reuse-fix strategies rewrite the root's branches, and the right fix (`.Session(&gorm.Session{})` at the end of the root's chain, or an isolating copy) depends on intent.

Limitations:

- A copy taken through a method value (`s := q.Session; c := s(cfg)`) is not recognized as a copy; `c` is treated as a mutable root of its own, as before.
- `go`/`defer` uses of a copy are not alias uses.
- A copy returned from a function, or stored in a struct field and read in another function, is not followed. A `//gormreuse:pure` function that returns a copy of its argument hides the sharing from its caller (related: #187, #188).
- The `//gormreuse:immutable-return` body check still trusts `Session(&gorm.Session{Initialized: true})` as immutable (related: #194), and the fix generator's Phi-edge check treats it as already immutable.

### Directives

- `//gormreuse:ignore` - Suppress warnings for the same line, and for the next line when the comment is the first token on its line. A trailing comment after code covers its own line only (#189). `BuildIgnoreMap` decides this from the smallest column at which an AST node starts or ends on the comment's line.

  `directive.Ignores` applies one scope rule to every diagnostic (#190). `RunSSA` sends each report through `Ignores.Ignore(pos)`, and `run` sends directive problems through it too. Lookups go from the most specific scope to the widest, and mark the first match used:

  | Scope | Rule |
  | --- | --- |
  | Line | The diagnostic's own line, or any line of the innermost statement that holds it. Blocks and labeled statements are skipped. For `if`, `for`, `range`, `switch`, `select` and case clauses only the header (up to `{` or `:`) counts, so a header ignore does not reach the body. An ignore comment inside a closure nested in the statement belongs to the closure, not to the outer statement |
  | Function | A `func` declaration whose doc comment holds the ignore: from the doc comment to the closing brace, closures included. A closure has no function-level ignore of its own |
  | File | The whole file |

  An ignore only suppresses reports. Before #190 a function-level ignore skipped the function in every pass. That hid contract violations while the directive stayed trusted (a leaking `pure` function was trusted at call sites), never covered closures, and marked the ignore used even when nothing was suppressed. Now every pass runs, so `failedPure` and `needsImmutableParam` see ignored functions, and an ignore is used only when it suppressed a report. Unused-ignore reports go straight to `pass.Reportf`: an ignore never silences another ignore's staleness. So do `panicGuard` internal-error reports, which can now come from an ignored function too, since it is analyzed.
- `//gormreuse:pure` - Mark function/method/closure as not polluting its `*gorm.DB` argument
- `//gormreuse:immutable-return` - Mark function/method/closure as returning immutable `*gorm.DB` (like Session/WithContext). **Body contract**: when the function actually returns a provably-mutable value — one whose root is a gorm chain-method call, e.g. `db.Where(...)` or `Session().Where(...)` (the trailing chain re-forks a fresh `clone==0` Statement) — the directive is reported at the declaration, since the linter would otherwise trust it and silently allow unsafe reuse of the return value at call sites. Roots the tracer treats as mutable only conservatively (a bare `*gorm.DB` parameter, or a call into an unmarked user function/closure) are given the benefit of the doubt and not reported.
- `//gormreuse:immutable-param` - Opt a function's `*gorm.DB` parameters out of the Phase 1b mutable-by-default treatment: they are treated as immutable inside the function (the caller is responsible for passing an isolated value). **Caller-side contract**: when the function actually branches such a parameter, passing a mutable `*gorm.DB` at a call site is reported (isolate with `.Session(&gorm.Session{})` first, or make the caller `immutable-param` too so the contract propagates). The check runs wherever the callee is known statically: a direct call, a function or method value, a generic instance, and the same calls under `go` and `defer` (#192). The `go`/`defer` path (`processGormDBCallCommonWith`) shares `checksImmutableParamArg` with the call path. An interface method call or a call through a func-typed parameter has no static callee, so its contract cannot be checked. A callee in another package is not checked either (#195).
- `//gormreuse:immutable-input(name)` - Declare that the function passes an **immutable** `*gorm.DB` to its callback parameter `name` (a user-defined equivalent of gorm's `Transaction`/`FindInBatches`). The named callback's `*gorm.DB` parameter is then treated as immutable, so reuse inside the callback is allowed. **Body contract**: if the function actually passes a mutable value to the callback, it is reported. Reported unused when `name` isn't a parameter, isn't a function type, or the callback has no `*gorm.DB` parameter.

Also: gorm's built-in `Transaction` (via `Begin`) and `FindInBatches` (via `Session(&Session{NewDB: true})`) pass a fresh (`clone>0`) handle to their callbacks, so reuse inside those callbacks is always allowed. `Connection` is deliberately **not** in this set: it calls `fc(db.getInstance())`, which is a `clone==0` handle, so its callback's `tx` is an ordinary mutable parameter and reuse inside it is reported (#173). `e2e/internal` guards all three premises against real gorm.

Only `//gormreuse:name[,name...]` (lowercase, no spaces, line comment) is a directive. `readDirective` in `directive.go` reads every comment once, in this order:

1. Cut the comment at the first `//` after the leading `//`. The rest is a reason: `//gormreuse:ignore // reason`, `//gormreuse:ignore //reason` and `//gormreuse:ignore//reason` are all a bare ignore. `//` is the only reason separator; ` - reason` was never accepted.
2. Read what is left with `ast.ParseDirective`, with no normalization. A comment whose body starts with `gormreuse:` but is not a gormreuse directive is reported as `malformed gormreuse directive: write it as //gormreuse:name`. So is an empty part in the list (`pure,`, `pure, immutable-return`).
3. Check every part of the comma list. A part that is not `ignore`, `pure`, `immutable-return`, `immutable-param` or `immutable-input(name)` is reported as `unknown directive gormreuse:<part> (want ...)`. A malformed `immutable-input(...)` part, such as `immutable-input(cb` or `immutable-input()`, is reported as `malformed gormreuse:<part> directive: write it as immutable-input(name)`.
4. Any text left after the name is reported as `gormreuse:<name> takes no argument; write a reason after //`.

A comment with a problem has no effect at all, even when only one part of its list is wrong (#156). Before #156, `hasDirective` looked for one known name in the list, so `//gormreuse:pure,imutable-return` still marked the function pure and nothing reported the typo. Keeping the valid parts was rejected: the user meant the whole list, and a half-applied list hides the typo.

Problems are reported by `FindDirectiveProblems` through `pass.Reportf`, at the comment. Generated files are skipped with the rest of the analysis.

Directives can be combined with commas: `//gormreuse:pure,immutable-return`

**Generic instances and method values (#180).** A call can resolve to a stand-in for the function the directive is written on: an instance of a generic function or method (`genBranch[int]`), or a bound method wrapper (`f := r.m`, named `m$bound`). The directive sets and the maps keyed by `*ssa.Function` (`failedPure`, `needsImmutableParam`, the Scopes and immutable callback sets) are keyed by the declared function. `directive.DeclaredFunc` maps a stand-in to it: an instance to its `Origin()`, a bound wrapper to the declared method (`Prog.FuncValue`). Every lookup goes through it: `DirectiveFuncSet.Contains`, `ImmutableInputSet.Callbacks`, the `failedPure` check in `IsPureFunction`, the immutable-param caller check, and the callback collectors (`scopesCallbackResolver` for Scopes/Preload, `callbackFuncValue` for Transaction/FindInBatches and immutable-input). Code that reads the arguments of a call keeps the resolved callee: a bound wrapper has no receiver argument, so the receiver shift is decided from its own signature. An interface method value has no declared body, so `DeclaredFunc` leaves it unchanged and it carries no directive.

All internal positions (file keys, ignore lines, directive placement, the generated-file skip, the disk fallback in `funcset.go`, fix offsets) use `PositionFor(pos, false)`, so `//line` directives cannot move them (#151, #152, #153). Only the `root at file:line` text in messages stays adjusted, to match the position the driver prints.

**Directive placement for closures:**

```go
// Next-line pattern (before closure):
//gormreuse:pure
helper := func(q *gorm.DB) { ... }

// Same-line pattern (after opening brace):
helper := func(q *gorm.DB) { //gormreuse:pure
    ...
}

// Multi-assignment (all direct closures get the directive):
//gormreuse:pure
a, b := func(q *gorm.DB) { _ = q }, func(q *gorm.DB) { _ = q }
```

> **Important**: User-defined functions that accept `*gorm.DB` are treated as polluting **unless** the function returns `*gorm.DB` and the result is assigned (e.g., `q = helper(q)`). In that case, it's treated like a gorm method assignment. Use `//gormreuse:pure` to mark functions that don't pollute arguments even when result is discarded, and `//gormreuse:immutable-return` to mark functions whose return value can be safely reused (like DB connection helpers).

> **Note**: Unused directives are reported as warnings:
> - Unused `//gormreuse:ignore` - line/function-level ignores that suppress no diagnostic
> - Unused `//gormreuse:pure` - directives that don't match any function
> - Unused `//gormreuse:immutable-return` - directives that don't match any function
> - Unused `//gormreuse:immutable-param` - directives that don't match any function (no `*gorm.DB` parameter)
> - **Redundant** `//gormreuse:immutable-param` - directive is signature-valid but has no effect: the parameter is never reused, so even treated as mutable it would produce no violation to suppress. Reported at the function declaration. Skipped when combined with `//gormreuse:pure` (a valid pure function cannot branch its parameter, so immutable-param is redundant there by construction). This is callee-side only, matching Phase 1b stage 2a.
> - For combined directives (`//gormreuse:pure,immutable-return`), if either part is used, no unused warning is reported

## Architecture

```
gormreuse/
├── analyzer.go                 # Public analyzer definition (go/analysis entry point)
├── analyzer_test.go            # Integration tests using analysistest
├── cmd/gormreuse/main.go       # CLI entry point (singlechecker)
├── plugin/plugin.go            # golangci-lint module plugin (no settings; shares Analyzer)
│
├── internal/                   # Internal implementation
│   ├── analyzer.go             # SSA analysis orchestrator (RunSSA entry point)
│   │
│   ├── directive/              # Comment directive handling
│   │   ├── directive.go        # Directive parsing and problems (parse, FindDirectiveProblems, IsIgnore/IsPure)
│   │   ├── ignore.go           # //gormreuse:ignore - Ignores scope rule, IgnoreMap, unused tracking
│   │   └── funcset.go          # DirectiveFuncSet - directive-marked function sets
│   │
│   ├── scopeswarn/             # TEMPORARY Session-returned-from-Scopes warning (GORM bug #7592)
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
│   │       └── validator.go    # Validator.Validate - checks pure contracts
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
└── e2e/internal/               # SQL behavior verification (separate module)
```

The `plugin` package registers `Analyzer` with golangci-lint's module plugin system. `Analyzer` reads no flags, so the plugin hands it over as it is and rejects any settings. It lives in the main module: `plugin-module-register` adds nothing beyond `golang.org/x/tools`, and a nested module would need its own tag per release.

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
│    1. Suppress reports by ignore scope (directive.Ignores)             │
│    2. Validate pure functions (purity.Validator)                       │
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

### Deterministic Reporting

The same input must give the same diagnostics and fixes on every run (#184). Golden tests, golangci-lint caching, and `-fix` depend on it. Two rules keep it so:

- `Tracker.DetectViolations` visits roots in a fixed order, never in map order. The order is root position, then earliest use, then SSA name, function, and instruction text. The later keys only matter for roots without a position, such as Phi nodes.
- The checker reports each position once. When several roots are reused at one position (`use2(q, r)`), the diagnostic takes the message of the first root in that order, which is the root that appears first in the source. It carries the fixes of every root there whose fix was not emitted yet, merged into one `SuggestedFix`, because drivers apply only the first fix of a diagnostic. Before #184, the first violation in map order won the position and the fixes of the other roots were dropped, so `-fix` needed several passes.

`TestDeterministicDiagnostics` runs the analyzer 50 times over `testdata/src/determinism` and requires identical output. Range over a map only where the order cannot reach a diagnostic, or sort first.

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

### Skipped files and internal errors

Code is left out of the analysis in two ways only, and neither is silent (#186).

- **Generated files.** `isGeneratedFile` in `analyzer.go` decides this per file. A package that imports `"C"` reaches the analyzer as cmd/cgo's output, not as the user's files. Each output file starts with `// Code generated by cmd/cgo; DO NOT EDIT.`, then a `//line` directive back to the user's file, then the user's file with its comments. `ast.IsGenerated` alone would skip every user file of a cgo package. So when that header is followed by a `//line` directive before the package clause, the header is ignored and the user's own comments decide. A user file marked `// Code generated ... DO NOT EDIT.` is still skipped. cgo's own files, such as `_cgo_gotypes.go`, have no such `//line` directive and are still skipped. All positions still come from `PositionFor(pos, false)`, so the file keys are the cgo output files, consistently. `testdata/src/cgofile` covers this with a real cgo build, and is skipped without a C compiler.
- **A panic while analyzing a function.** `panicGuard` in `internal/analyzer.go` recovers it, so one function cannot abort the run for the package. It reports `gormreuse: internal error analyzing <func>: <panic>; please report this at ...` at the function, once per function, through `pass.Reportf`. No ignore directive suppresses it. Before #186 the panic was dropped with every diagnostic of the function. `GORMREUSE_DEBUG_PANIC=1` still re-panics with the function name, for a stack trace.

The `-fix` drivers do not apply fixes in cgo files: they skip every fix that edits a file `ast.IsGenerated` reports, and the cgo output carries cgo's header. The diagnostics are still reported.

### Pollution Sources (Safe Side)

The linter marks `*gorm.DB` as polluted in these scenarios:

- **Interface method calls**: Assumed to pollute (can't statically analyze)
- **Channel send**: `ch <- db` marks db as polluted
- **Slice/Array storage**: `[]*gorm.DB{db}` marks db as polluted
- **Map storage**: `map[string]*gorm.DB{"k": db}` marks db as polluted
- **Interface conversion**: `interface{}(db)` marks db as polluted (type assertion may extract)
- **Function arguments**: Non-pure functions receiving `*gorm.DB` pollute if result is discarded (not assigned)
- **Struct field access**: `h.field.Find(nil)` traces back to the original value stored in field, including fields reached through embedding (`outer{db: q}`, where `db` is promoted from an embedded struct)

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

Struct Field Tracking (field path, not base identity):
  h := outer{db: q}    <- SSA stages the literal in a `local outer (complit)`
                       <- temporary and copies the whole struct into h
  h.db.Find(nil)       <- read emits a FRESH FieldAddr chain: &(&h.embedded).db
  h.db.Count(nil)      <- and another one, sharing nothing with the store

  Field stores are therefore matched by (root address, field index path) and
  whole-aggregate copies into any prefix of that path are followed back to the
  address they were loaded from. Matching the immediate base value instead would
  miss every nested or promoted field.

Field Store Order (#182):
  r.db = r.db.Where("t")  <- SSA: t1 = *(&r.db); t2 = Where(t1); *(&r.db) = t2
  r.db.Find(nil)          <- load sees *(&r.db) = t2 only

  A load matches only the stores that reach it (fieldStores): the last store to
  the field in its own block before it, else the last store in each predecessor
  block found by walking the CFG backward. A store ends the walk on its path, so
  a later store replaces an earlier one. Before #182 a load saw every store in
  the function, so the RHS load above resolved to t2 and Where("t") counted as
  the first branch of its own result.

  for _, c := range conds {
    r.db = r.db.Where(c)  <- the load sees this store from the previous iteration
  }
  r.db.Find(nil)

  In a loop the call is still a root of its own receiver. isLoopFieldRebuild
  treats it as an assignment, like the loop-header Phi of `q = q.Where(c)`. The
  tracker also records it as a loop-carried use: a polluting use of the call's
  result that can reach the call (`r.db.Find(nil)` later in the same body) is
  reported at the call, whatever the positions.

  Decisions and limitations:
  - The Scopes callback resolver still reads every store to a field, since a
    larger callback set only adds mutable roots.
  - Only stores to the same (root address, field path) count. A write through
    another pointer (`p := &r.db; *p = x`) or inside a callee neither reaches a
    load nor replaces a store, as before.
  - The loop-carried check covers fields only. The local form
    `for { q = q.Where(c); q.Find(nil) }` is still not reported.
  - Outside a loop the rebuild is not an assignment: `r.db = q; r.db =
    r.db.Where("y"); q.Find(nil)` is reported. Inside a loop, as with locals,
    the value read on the first iteration is not counted as branched.

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

User-Defined Function Reassignment:
  q := db.Where("x")
  q = buildQuery(q, "y")   <- Result assigned: treated like gorm method assignment
  q = buildQuery(q, "z")   <- Result assigned: new mutable root (NO false positive)
  q.Find(nil)              <- First branch from latest q - OK

  buildQuery(q, "w")       <- Result discarded: q is polluted
  q.Find(nil)              <- VIOLATION (q was polluted by discarded call)

  When user-defined function returns *gorm.DB and result is assigned,
  it uses RecordAssignment (like gorm methods) instead of MarkPolluted.
```

### Which merges are reassignments (#175)

A straight-line `q = q.Where("a")` needs no special case: the new SSA value is its own root, and `q.Where("a")` is a branch of the old one. The special case is a merge. In `if c { q = q.Where("a") }; q.Find(nil)`, the use reads `Phi(q₁, q₂)`, and `FindMutableRoot` resolves that Phi to `q₁`. If `q.Where("a")` polluted `q₁`, `q.Find` would be a false second branch. So `isAssignment` records such a call with `RecordAssignment`, which does not pollute.

`isAssignment(call, root)` follows the call's result forward through chained gorm calls and Phi nodes. It returns true only when it reaches a Phi, or a Store to an Alloc or to a captured variable (`FreeVar`, #177), whose `FindAllMutableRoots` include `root`. Only that merge can hand `root` back to a later use.

| Code | Merge | Result |
| --- | --- | --- |
| `if c { q = q.Where("a") }` | `Phi(q₁, q₂)` holds `q₁` | assignment |
| `for … { q = q.Where(x) }` | loop-header Phi | assignment |
| `if a { q = q.Where("a"); if b { … }; q = q.Where("c") }` | reaches `Phi(q₁, …)` through the inner Phi and `Where("c")` | assignment |
| `if c { q2 = q.Where("a") }` | `Phi(nil, q₂)` | branch of `q` |
| `q2 := q.Where("a")`, `q2` captured by a closure | Store to `q2`'s Alloc | branch of `q` |
| `q := db.Where("b"); if c { q = db.Where("a") }` | `Phi(q₁, q₂)`, both branches of `db` | branch of `db` |
| `for v := range seq { q = q.Where(v) }` | Store to the captured `q` (`FreeVar`) | assignment |
| `for v := range seq { last = q.Where(v) }` | Store to the captured `last` | branch of `q` |

Before #175, any Phi or Store to an Alloc counted as an assignment. A chain derived into a different variable was then never a branch, and branching a mutable `db` parameter twice under a condition was missed.

Limitation: the check asks whether the merge *can* yield `root`, not whether it does on the path that ran the call. `if c { q2 = q.Where("a") }; if d { q = q2 }` reaches `Phi(q₁, …)` and is still treated as an assignment.

### Range-over-func Loops

SSA compiles the body of `for v := range seq` (Go 1.23+) into a synthetic closure that the iterator calls once per element. go/ssa marks it with `Synthetic == "range-over-func yield"`. The closure has no back-edge, so `DetectLoops` treats every block of it as in a loop (#177). A root defined outside the body and used inside it is then reported, as in a plain `for range`. A root defined inside the body is in a loop block, so it is fresh on each iteration.

The body reaches outer variables through `FreeVar`s, so `q = q.Where(v)` inside it is a `Store` to a `FreeVar`, not to an `Alloc`. `isAssignment` treats a `FreeVar` as a merge point like an `Alloc` (see the previous section). Its values are those of the captured variable, so the store is an assignment only when that variable can hold `root`. That is the in-place update of a plain loop. A store into another variable, such as `last = q.Where(v)`, still counts as a use of `q`.

Only the body closure itself is treated as a loop. A closure nested inside the body (an IIFE, a `defer`) is not, which matches how a closure nested in a plain loop is handled.

## Pure Contract Validation

`purity.Validator` checks the body of every `//gormreuse:pure` function. It follows "parameter-derived" values and reports when one is polluted or escapes.

| Step | What it does |
| --- | --- |
| Seed | Every parameter whose type holds a `*gorm.DB` without going through an interface: `*gorm.DB`, `**gorm.DB`, `[]*gorm.DB` (variadic too), maps, channels, arrays, and structs with such a field. A method's receiver is not seeded. |
| Propagate | Through Phi, Extract, call results, loads (`*p`), channel receives, `FieldAddr`/`Field`/`IndexAddr`/`Index`/`Lookup`/`Slice`, conversions, range iteration, and stores into a local `Alloc` (`p := &db`, `h := holder{db: db}`). It repeats until nothing changes, so loop Phis and later stores are seen. |
| Closures | A `MakeClosure` whose bindings are derived seeds the closure's free variables and validates the closure body as part of the pure function (IIFE, `go func(){...}()`, stored closures). |
| Check | `Call`, `Go` and `Defer` share one check. `*gorm.DB` method values (`Find$bound`) are checked where they are created. Stores outside local variables and `pollutionsource.Leak` cover escapes. |

A violation is **definitive** (`Violation.Definitive`) when the argument is polluted by a gorm chain method, escapes, or is passed to a pure function that already failed. Only definitive violations put the function in `failedPure`, which makes the tracer stop trusting it at call sites. Passing the argument to a function that is not marked pure stays conservative and does not revoke trust (`nestedClosureOuterPureViolation`, PV117).

Failure is transitive (#187). `validatePureFuncs` in `internal/analyzer.go` validates all pure functions repeatedly until `failedPure` stops growing, then reports from a final round. The set only grows, and a larger set only adds violations, so this ends within one round per pure function.

Decisions and limitations:

- **The receiver is not seeded.** It usually holds the application's root handle, and the directive's signature check also ignores receivers (`OrmWrapper.GetDB` is reported unused). Seeding it would report every `r.db.Where(...)` in a pure method.
- **Interfaces are not followed.** A `*gorm.DB` boxed in an `interface{}` is caught only when it is passed or stored directly (`pollutionsource.UnwrapGormDB`). Types that hold one only through an interface are not seeded.
- **Builtins.** `len(dbs)` and `cap(dbs)` cannot pollute and are not reported. `append` and `copy` count every derived argument. Other builtins count only a `*gorm.DB` argument itself.
- **Captured variables.** A closure that assigns a derived value to a variable captured from the pure function (`func() { x = db }()`) is reported as a store through a pointer. The free variable is not traced back to the enclosing `Alloc`.
- **Returning the argument.** A pure function that returns its argument is still trusted as returning a fresh value, so `pureReturnsArg(db).Find(nil)` in another pure function is not reported. That is #188.

## Reassignment Fix Target

The reassignment fix (`q.Where("a")` → `q = q.Where("a")`) rewrites one variable: the one that holds the reused root (#198). `findReassignTarget` in `internal/fix/generator.go` finds it from the root's own definition (`q := <call>`, `q = <call>` or `var q = <call>`; a root that is not a call, such as a Phi, gets no reassignment). A statement is rewritten only when its own top-level call is the use and its receiver is an identifier that `types.Info.Uses` resolves to that variable.

No statement is rewritten, and the root gets a Session instead, when:

| Case | Example | Why |
| --- | --- | --- |
| The root is not assigned to one local variable | `r.db.Where(...)`, a package-level `DB` | There is no local to rebind |
| A use does not mention the variable | `r := q; r.Where("a")`, a method value `where := q.Where` | Rebinding `q` leaves that branch on the old root |
| A closure captures the variable, unless it is called inline | `go func() { q.Where("a") }()`, `defer func() { q.Find(nil) }()`, `fn := func() { ... }` | A goroutine that writes `q` races with its parent. A deferred or stored closure would see the new value |

An inline closure `func() { ... }()` runs in program order, so writing a captured variable from it is safe. A closure passed to a call, such as `Transaction`, `t.Run` or `wg.Go`, may run later or on another goroutine, so it counts as a capture. A variable declared inside such a closure is local to it and is rewritten as usual.

The old resolver took the receiver of the outermost method call around the use. Inside `DB.Transaction(func(tx) { ... })` that rewrote the package-level `DB`, and in `base.Or(q)` it rebound `base`. `TestGoldenFilesCompile` now type-checks every `.golden`, and `testdata/src/gormreuse/fix_target.go` and `testdata/src/converge/fix_target.go` hold the cases.

## Development Commands

Develop on Go 1.27 or later: the fixtures use generic methods and promoted
struct-literal keys, so the test harness needs a 1.27 toolchain to compile them.
Both the main module and the E2E module require Go 1.27 in their `go.mod` files.

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
cd e2e/internal && go test -v

# Regenerate the .golden and .diff fixtures after changing detection or fixes
GORMREUSE_UPDATE_GOLDEN=1 go test -run TestGoldenFiles .
```

## Releasing

Releases are cut from GitHub Actions, never locally:

- **Tag and Release** (`tag_and_release.yml`) — the normal path: creates the `vX.Y.Z` tag, then builds and publishes.
- `tag.yml` / `release.yml` are the two halves, dispatchable on their own when a tag already exists.

`tag.yml` tags the commit the workflow was dispatched on, and refuses unless that commit is on `main` and has a successful CI run from a push to `main`. Dispatch it from `main` after CI on the merge commit has passed. Workflow actions are pinned by full commit SHA, with the release in a trailing comment; dependabot updates both.

`release.yml` runs goreleaser (`.goreleaser.yaml`) once for all six targets — darwin/linux/windows x amd64/arm64, `CGO_ENABLED=0` — and uploads the archives plus `checksums.txt` to the release. Those assets are what makes `mise use -g "github:mpyw/gormreuse"` work, so a release with no assets breaks the documented install path. goreleaser is pinned to 2.17.1: 2.18.0 attests every build artifact, which fails the job.

Validate config changes with `goreleaser check`, and dry-run with `goreleaser release --snapshot --clean --skip=publish`.

## Testing Strategy

- Use `analysistest` for all analyzer tests
- Test fixtures use `// want` comments for expected diagnostics
- Test structure:
  - `===== SHOULD REPORT =====` - Cases that should trigger warnings
  - `===== SHOULD NOT REPORT =====` - Negative cases

### Suggested-fix fixtures

Each fixture `testdata/src/gormreuse/X.go` has two committed files that record its suggested fixes:

| File | Content | Checked by |
| --- | --- | --- |
| `X.go.golden` | The source with every suggested fix applied | `TestSuggestedFixes`, `TestGoldenFiles`, and `TestGoldenFilesCompile` (the whole package must type-check with every `.golden` swapped in) |
| `X.go.diff` | A full-context unified diff from `X.go` to `X.go.golden`, for review | `TestGoldenFiles` |

A plain `go test` only compares these files. It never writes them, so CI fails when they are stale. To regenerate them after a change to detection or fixes:

```bash
GORMREUSE_UPDATE_GOLDEN=1 go test -run TestGoldenFiles .
```

This rewrites every `.golden` and `.diff` file and deletes the ones whose source fixture is gone. Read each changed `.diff` before you commit it: it shows exactly what `-fix` now does to the fixture. The failure message of `TestGoldenFiles` prints the same command. Rendering a `.diff` needs `diff` on `PATH`, and a failing `diff` fails the test.

`TestFixesConverge` applies the fixes to each fixture in `testdata/src/converge`, then lints the fixed output again. It fails when the directory has no fixture, when a fixture's fixes change nothing, when the fixed output does not type-check, or when any diagnostic remains. So every fixture there must need a fix, and one pass of fixes must resolve all of it.

> **Trap: never name a fixture `*_test.go` unless you mean to.** `analysistest`
> loads a package's `*_test.go` files as part of both the normal package and the
> test variant, so every diagnostic in such a fixture is collected **twice**
> (this produced 343 duplicated diagnostics in PR #57). Use a plain `.go` name.
> The one intentional exception is `testdata/src/filefilter/code_test.go`, which
> exists specifically to test the analyzer's `-test` flag handling.

### Testdata Organization

```
testdata/src/gormreuse/
├── basic.go         # Basic reuse patterns, Session at end/middle
├── advanced.go      # Derived variables, helper functions, conditional reuse
├── evil.go          # Edge cases: closures, defer, goroutines, struct fields, loops
├── session_alias.go # Session/Debug copies sharing a Statement; Session{Initialized} (#174)
└── ignore.go        # //gormreuse:ignore directive tests
```

Directives across packages are tested by `TestCrossPackageDirectives`: `testdata/src/purelib` declares them and `testdata/src/externalpure` calls them. Both packages' diagnostics are checked. The cases that are wrong today because the analyzer exports no facts (#195) assert the current behaviour and are marked `[LIMITATION] #195`. Update them when #195 is fixed.

### E2E Tests

The `e2e/internal/` directory contains tests that verify actual GORM SQL behavior using sqlmock. This is a separate Go module (its own `go.mod`) to avoid dependency conflicts, and CI tests and lints it on every PR (`cd e2e/internal && go test ./... && golangci-lint run ./...`). Dependabot covers it, so GORM bumps arrive as PRs; re-check go-gorm/gorm#7592 on each (see the README). Every test asserts (`t.Errorf`) — the suite guards the linter's core clone-semantics premise: mid-chain (`clone==0`) reuse accumulates conditions; root / `Session()` / `Transaction`- and `FindInBatches`-callback (`clone>0`) handles fork a fresh Statement and are safe; a `Connection`-callback handle is `clone==0` and accumulates. `scopes_session_test.go` pins which Session-family calls inside a `Scopes` callback leak the transaction (go-gorm/gorm#7592): only a returned one. It also pins the exceptions in [Session and Debug copies](#session-and-debug-copies-174): a `Session()`/`Debug()` copy of a mid-chain value sees later changes to it, and `Session{Initialized: true}` returns a mid-chain value.

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

- **immutable-param through an interface**: `var a applier = repo{}; a.apply(q)` calls an interface method, so the callee and its directive are unknown and the caller-side contract is not checked. The same holds for a call through a func-typed parameter
- **Directives across packages (#195)**: a callee's `pure` and `immutable-return` are read back from its source on disk and trusted, even when its body breaks the contract. The `immutable-param` caller contract is not checked, and `immutable-input(name)` is ignored, for a callee in another package

These are documented in `testdata/src/gormreuse/evil.go` and `testdata/src/externalpure/use.go` with `[LIMITATION]` markers.

**Closure use ordering (#68)**: uses inside a closure that is invoked at a *single* later call site (`f := func() { q.Find(nil) }; …; f()`) are ordered by that **call-site position**, not the closure body's source position — so define-early/call-late reuse is reported at the call site and the earlier direct use is correctly treated as the first branch. This applies only to the unambiguous single-invocation case; IIFEs (invoked inline), deferred/spawned closures, and closures invoked from multiple sites keep their body positions.

**Closure use reachability (#179)**: the same single invocation, and also an IIFE, gives the closure an invocation site: the block of its one plain call (`Tracker.RecordInvocationSite`). When two uses are in different functions, `isReachable` lifts each use through these sites to the innermost function that holds both, and asks the CFG whether the first site reaches the second. So `if c { f() } else { q.Count(nil) }`, `if c { f(); return }; q.Count(nil)` and one IIFE per branch are not reported. Positions still decide the order. If the two sites are the same block, or no common function is reached, the pair counts as reachable, as before #179. A closure with no single plain call (more than one call, `defer`, `go`, passed as an argument, stored) has no site, so its uses stay conservative. A nested closure without its own site does not inherit its parent's site either: it can escape the parent and run after it.

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

### Scopes Session-family warning (temporary, #196)

`internal/scopeswarn` works around go-gorm/gorm#7592. It reports a `Session`/`WithContext`/`Debug` call only when its result is what a `Scopes` callback returns.

Why only the returned value (checked in `e2e/internal/scopes_session_test.go`):

- gorm runs the create/update/delete callbacks on the value the last scope returned (`db = scope(db)` in `executeScopes`).
- `BeginTransaction` stores its flag with `InstanceSet`, which calls `getInstance`. On a `clone>0` value that clones the Statement, so `CommitOrRollbackTransaction` never finds the flag and the transaction stays open.
- `Session`/`WithContext`/`Debug` return `clone>0`. Every other chain method returns a `clone==0` value from `getInstance`.

So `return q.Session(...).Where("x")` is safe, and `return other.Session(...)` leaks even though `other` is not the callback parameter. The issue's suggestion to restrict the warning to values derived from the parameter was wrong; the rule keys off the return value instead.

How the rule finds its input:

- **Callbacks**: `tracer.CollectScopesOnlyCallbacks`, the Scopes half of the resolver that reuse detection uses (`scopesCallbackResolver`). It follows the varargs array, slice literals, `append`, Phi edges, local, captured and package-level variables, struct fields, and method values (both the `$bound` wrapper and the method). Package-level initializers live in the synthetic `init`, which is not in `SrcFuncs`, so the resolver scans it for stores to globals.
- **Returned value**: followed through Phi edges, local and captured variables, `Extract`, and the returns of static callees (helpers, invoked closures). A callee's parameter maps back to the caller's argument, so `return passThrough(q.Debug())` is reported. Calls are followed to depth 16 and never into a function already on the stack.

Limitations:

- A dynamic callee (a func-typed parameter, an interface method) ends the trace, so a Session result returned through one is not reported.
- A callback that returns a root `*gorm.DB` (also `clone>0`, for example a package-level `DB`) leaks the same way. It is not reported, since the warning is about the three Session-family methods.
- The rule does not model the order of several scopes; each callback that returns a Session-family result is reported, even when a later scope returns a `clone==0` value.
- A package-level closure is now a known Scopes callback, but reuse detection still does not analyze its body, because it is not in `SrcFuncs` (#183).

## Related Projects

- [goroutinectx](https://github.com/mpyw/goroutinectx) - Goroutine context propagation linter
- [zerologlintctx](https://github.com/mpyw/zerologlintctx) - Zerolog context propagation linter
- [ctxweaver](https://github.com/mpyw/ctxweaver) - Code generator for context-aware instrumentation
