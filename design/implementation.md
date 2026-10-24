# Implementation notes

These notes are the contributor spec of gormreuse. They record how the analysis works, the rules it follows, the approaches it rejected, and its limits. The short, always-loaded instructions are in [AGENTS.md](../AGENTS.md).

| Part | Sections |
| --- | --- |
| Model | [Overview](#overview), [Detection model](#detection-model) |
| Pipeline | [Architecture](#architecture), [Analysis pipeline](#analysis-pipeline), [Deterministic reporting](#deterministic-reporting), [Skipped files and internal errors](#skipped-files-and-internal-errors) |
| Subsystems | [Call sites](#call-sites), [Callbacks](#callbacks), [Control flow](#control-flow), [Root tracing](#root-tracing), [Uses and pollution](#uses-and-pollution), [Directives](#directives), [Directive contracts](#directive-contracts), [Suggested fixes](#suggested-fixes), [Scopes Session-family warning](#scopes-session-family-warning-temporary) |
| Process | [Development](#development), [Testing Strategy](#testing-strategy), [Releasing](#releasing), [Known Limitations](#known-limitations) |

Issue and pull request numbers such as (#303) point to the change that set a rule. History, measurements and long rationale are folded into `<details>` blocks.

## Overview

**gormreuse** is a Go linter. It detects unsafe reuse of a [`*gorm.DB`](https://pkg.go.dev/gorm.io/gorm#DB) after chain methods. It uses [SSA](https://pkg.go.dev/golang.org/x/tools/go/ssa) (Static Single Assignment) form to follow `*gorm.DB` values through variables and method chains.

### The problem: mutable branching

> [!IMPORTANT]
> The linter reports a mutable `*gorm.DB` that branches into more than one chain. "Reused" means "the second branch from the same mutable root".

GORM's chain methods (`Where`, `Order`, `Limit`, ...) return **shallow clones** that share the internal `Statement`. Two chains from one mutable root write into the same `Statement`, so their conditions accumulate. `q := db.Where("x"); q.Where("a").Find(nil); q.Where("b")` reports the second branch, even without a finisher. An IIFE chain consumed at once is one branch. `Session()` before the chain makes `q` immutable, and is the fix.

<details><summary>Examples: two branches, an IIFE chain, the Session fix</summary>

<table>
<tr><th>Code</th><th>Verdict</th></tr>
<tr><td>

```go
q := db.Where("x")      // q is mutable (a chain result)
branch1 := q.Where("a") // branch 1: shares q's Statement
branch2 := q.Where("b") // branch 2: also shares it
```

</td><td>

The second branch is reported. `branch1` and `branch2` interfere.

</td></tr>
<tr><td>

```go
q := db.Where("x")
q.Where("a").Find(nil) // first branch: OK
q.Where("b")           // second branch: reported
```

</td><td>

The second branch is reported even without a finisher.

</td></tr>
<tr><td>

```go
q := db.Where("x")
_ = func() *gorm.DB {
    return q.Where("y")
}().Find(nil) // one branch: the whole IIFE chain
q.Count(nil)  // second branch: reported
```

</td><td>

An IIFE that returns a chain, consumed at once, counts as **one** branch. Only later uses of `q` outside it are separate branches.

</td></tr>
<tr><td>

```go
q := db.Session(&gorm.Session{}).Where("x")
branch1 := q.Where("a") // new Statement
branch2 := q.Where("b") // new Statement
```

</td><td>

Not reported. `Session()` before the chain makes `q` immutable. The fix for a reuse is an isolating `Session`.

</td></tr>
</table>

</details>

### Detection model

Every gorm method call on a value is a read, a state-only call or a branch. `typeutil.receiverUses` is the one table of the methods whose call does not branch its receiver. `typeutil.IsImmutableReturningBuiltin` covers all of them, reads and state-only calls alike.

| Class | Methods | A call is |
| --- | --- | --- |
| Immutable-returning | [`Session`](https://pkg.go.dev/gorm.io/gorm#DB.Session), [`WithContext`](https://pkg.go.dev/gorm.io/gorm#DB.WithContext), [`Debug`](https://pkg.go.dev/gorm.io/gorm#DB.Debug), [`Open`](https://pkg.go.dev/gorm.io/gorm#Open), [`Begin`](https://pkg.go.dev/gorm.io/gorm#DB.Begin) | A **read** (pure use) of the receiver's root, not a branch. The result is a new `clone > 0` instance (`clone == 1` or `2`). The exceptions are in [Session and Debug copies](#session-and-debug-copies) |
| Other reads | [`Transaction`](https://pkg.go.dev/gorm.io/gorm#DB.Transaction), `ToSQL` | A read of the receiver's root, like `Session` |
| State only | `AddError`, `DB`, `Get`, `InstanceGet`, `Callback` | Not a use at all. They touch no clause ([Methods that do not branch](#methods-that-do-not-branch)) |
| Chain and everything else | `Where`, `Find`, `Migrator`, ... | A **branch** that consumes the receiver |

`Transaction` returns `error`, so only the read applies. It runs its callback on a `Begin` result. Its callback's fresh `tx` is a separate exemption ([Callbacks](#callbacks)).

The rules built on these classes:

| # | Rule | Section |
| --- | --- | --- |
| 1 | The second branch from the same mutable root is a violation | [Uses and pollution](#uses-and-pollution) |
| 2 | A use of a `Session`/`Debug` copy after its mutable root changed is a violation | [Session and Debug copies](#session-and-debug-copies) |
| 3 | A `*gorm.DB` argument of a query method is a read of its root. A use of the outer chain after the argument's root changed is a violation | [Subquery and condition arguments](#subquery-and-condition-arguments) |
| 4 | Passing a `*gorm.DB` to a function that is not trusted `//gormreuse:pure` is a branch | [Pollution sources](#pollution-sources) |
| 5 | A use that may run more than once for one run of its root's definition is a reuse | [Uses that run more than once](#uses-that-run-more-than-once) |

> [!IMPORTANT]
> Passing `q` to a user function without `//gormreuse:pure` is a branch of `q`, like a gorm method call. When the function returns a `*gorm.DB`, its call is also handled like a gorm method in a merge. `if c { q = helper(q) }` is an assignment (`handleArgs` calls `isAssignment`, see [Which merges are reassignments](#which-merges-are-reassignments)). Its result is a new mutable root, unless the helper is [inferred](#inferred-immutable-return) `immutable-return`. Use `//gormreuse:pure` for functions that do not pollute their arguments. Use `//gormreuse:immutable-return` for functions whose result can be reused, such as DB connection helpers.

### Principles

<details><summary>Principles and package roles</summary>

| Principle | Meaning |
| --- | --- |
| SSA-based analysis | `go/ssa` follows `*gorm.DB` values through method chains |
| Pollute model | The linter tracks the pollution state of mutable values. An immutable value (from an immutable-returning method) can be reused freely. A mutable value (from a chain method) is polluted by its first branch |
| Mutable root finding | Each chain is traced back to its origin, its mutable root |
| Conservative | Prefer false positives over false negatives under uncertain control flow |
| IIFE return tracing | Immediately invoked function expressions are traced through to their returned roots |
| Method value tracking | `callsite.SiteOf` reads a call through a method value (`find := q.Find; find(nil)`) as the method call it stands for |
| Two-phase detection | First record every use, then detect violations with CFG reachability |
| Composition | `Analyzer` composes `RootTracer`, `cfg.Analyzer` and `pollution.Tracker` |
| Type-switch dispatch | `handler.Dispatch()` routes each SSA instruction to its handler with a type switch |

Each package answers one question:

| Package | Question |
| --- | --- |
| `tracer/` | WHERE is the mutable root? (value tracing) |
| `pollution/` | WHAT uses exist, and are they violations? (state tracking) |
| `cfg/` | CAN use A reach use B? MAY a use run again? (control flow) |
| `handler/` | HOW is each instruction type processed? (dispatch) |

</details>

## Architecture

`analyzer.go` is the public `Analyzer`. `internal/run.go` runs the package-level passes, and `internal/ssa` holds the per-function analysis. `internal/fix` builds the suggested fixes. The tree below lists every Go file, and `TestImplementationNotesArchitectureTree` keeps it complete.

<details><summary>File tree and the golangci-lint plugin</summary>

```
gormreuse/
├── analyzer.go                  # Public Analyzer: file filtering, directive scans, buildssa, then internal.RunSSA
├── analyzer_test.go             # analysistest runs over the testdata packages
├── analyzer_internal_test.go    # Generated-file decision (cgo output)
├── golden_test.go               # .golden/.diff fixtures: check, compile, regenerate
├── convergence_test.go          # TestFixesConverge: one pass of fixes leaves no diagnostic
├── determinism_test.go          # TestDeterministicDiagnostics: same output on every run
├── limitation_test.go           # Known Limitations lists every [LIMITATION] fixture, here and in the README
├── docs_test.go                 # These notes name only code that exists; the README pins name the latest release
├── growth_test.go               # TestAllocationGrowth: allocation and search-step growth on generated shapes (#302, #350)
├── cmd/gormreuse/
│   ├── main.go                  # CLI entry point (singlechecker)
│   └── *_test.go                # Build the command and run it (go vet, //line, facts, inspector paths)
├── plugin/plugin.go             # golangci-lint module plugin (no settings; shares Analyzer)
│
├── internal/
│   ├── run.go                   # RunSSA: every package-level pass, in order
│   ├── checker.go               # checker: every function's analysis, then its reports and fixes
│   ├── report.go                # The order of reports, unused directives and ignores, and merging fixes
│   ├── cgo.go                   # IsCgoOutput: the files of cmd/cgo's output, whose fixes are dropped (#346)
│   ├── pure.go                  # validatePureFuncs: the pure functions' contracts, to a fixed point
│   ├── immutable.go             # Inferring and checking immutable-return, and the need for immutable-param and its body rule
│   ├── infer.go                 # Inferring immutable-param and immutable-input (#349)
│   ├── fact.go                  # exportFacts: the facts importing packages read
│   ├── distrust.go              # distrustContracts: contracts that rely on a broken one stay out of the facts (#314)
│   ├── guard.go                 # panicGuard: one function's panic does not abort the package
│   ├── effort/effort.go         # Step, Count: the steps of the searches, which the growth gate counts (#350)
│   ├── directive/               # Comment directives
│   │   ├── directive.go         # readDirective, FindDirectiveProblems
│   │   ├── ignore.go            # //gormreuse:ignore scopes (Ignores), unused tracking
│   │   ├── funcset.go           # DirectiveFuncSet: pure / immutable-return / immutable-param sets, Infer
│   │   ├── param_read.go        # ParamRead: the reads of its parameters an inferred function returns (#342)
│   │   ├── placement.go         # placement: which function a directive comment is written on
│   │   ├── immutable_input.go   # ImmutableInputSet: immutable-input(name) callbacks
│   │   ├── func.go              # FuncKey: a function as a directive names it
│   │   ├── removal.go           # Removals: fixes that delete a directive name (#248)
│   │   ├── signature.go         # Signature checks per directive
│   │   └── typestr.go           # Type and receiver strings for matching
│   ├── facts/facts.go           # facts.Func: directives exported to importing packages (#195)
│   ├── fix/                     # Suggested fixes
│   │   ├── generator.go         # Generator, NewGenerator, Generate, GenerateIsolation: fix dispatch
│   │   ├── param.go             # Parameter roots: immutable-param or a Session at the top of the body (#201)
│   │   ├── reassign.go          # Reassignment target and discarded chains to reassign (#198, #199)
│   │   ├── virtual.go           # Virtual roots: the reassignment simulation (#202)
│   │   ├── use.go               # Order of a root's uses, and whether one runs again
│   │   ├── session.go           # Where Sessions go, and their edits
│   │   ├── merge.go             # Values that merge with a root: Phi edges, other stores (#247)
│   │   ├── change.go            # In-place changes outside the variable (#243), errors left on the root (#372)
│   │   ├── holder.go            # Struct stores of a root that other code reaches: escapes for #243 (#310)
│   │   ├── handoff.go           # Escapes that change nothing: finisher-only helpers, recursive calls (#347)
│   │   ├── closure.go           # Closures: captured variables, reassignments in a closure
│   │   ├── gorm.go              # gorm.io/gorm name and import, finishers, state fields
│   │   ├── call.go              # Calls in the syntax: the call at an Lparen, direct and inline calls
│   │   └── stmt.go              # The innermost statement at a position
│   ├── goldentest/goldentest.go # Shared golden machinery (ApplyFixes, UpdateCommand)
│   ├── scopeswarn/scopeswarn.go # TEMPORARY Session-returned-from-Scopes warning (GORM bug #7592)
│   ├── typeutil/gorm.go         # IsGormDB, IsImmutableReturningBuiltin, IsFinisher, and whether a type holds a *gorm.DB
│   └── ssa/
│       ├── analyzer.go          # Per-function Analyzer: tracking, then detection
│       ├── origin/origin.go     # Declared: the declared function behind a generic instance, a method value or a method expression
│       ├── callsite/            # What a call, go or defer instruction runs
│       │   ├── site.go          # Site, SiteOf, EachSite: callee, receiver, arguments
│       │   ├── session.go       # SessionEffect: what a Session/Debug call shares (#174)
│       │   ├── funcvar.go       # FuncVars: package-level and local variables that hold one function
│       │   ├── local.go         # LocalStores: every value stored into a local variable, when all are seen
│       │   ├── freevar.go       # FreeVarBinding: the value a closure captured
│       │   ├── closure.go       # ClosureRuns, ClosureCalls: where a closure value may run (#305)
│       │   └── walk.go          # WalkInstrs: every instruction, nested closures included
│       ├── callback/            # Functions that gorm or an immutable-input function calls back
│       │   ├── resolve.go       # resolveMayFuncs (may-hold), resolveExactFunc (exact): what a func value is
│       │   ├── collect.go       # Scopes/Preload, Transaction/FindInBatches, immutable-input collectors; CollectedCallbackArgs
│       │   ├── holder.go        # variables and struct fields that hold one callback (#257)
│       │   └── reach.go         # DropReachableCallbacks: drop callbacks that something else calls (#191)
│       ├── tracer/              # Where a *gorm.DB value's mutable root is
│       │   ├── root.go          # RootTracer, NewRootTracer, RootBlock: trace dispatch, calls, immutable sources
│       │   ├── store.go         # AllocStoredValues, FieldStoredValues: which stores reach a load
│       │   ├── field.go         # FieldRoot, SameField: field paths and roots of returned structs
│       │   ├── pointee.go       # PointeeRoot, PointeeRoots: what a call given a variable's address may store there (#345)
│       │   ├── elem.go          # ElemRoot: a value read from a slice, an array, a map, a channel or a pointer parameter (#374)
│       │   ├── param_read.go    # ParamReadValues: a getter's call reads the field of its own argument (#342)
│       │   ├── prove.go         # ProvesImmutable, ProvenGlobalsOf: positive proofs for the inferences (#371)
│       │   ├── freevar.go       # traceFreeVar: roots through a closure's captured variables
│       │   ├── phi.go           # Phi edges, loop variable swaps through Phis
│       │   ├── pure.go          # PureVerdicts, IsPureFunction: pure calls and the arguments they return
│       │   ├── alias.go         # FindAliasedRoots: Session/Debug copies and read-only arguments a value reads
│       │   ├── state.go         # SessionDropsState: Statement state that a Session drops (#344)
│       │   └── closure.go       # ClosureCapturesGormDB, IIFE returns, stored closure results, closure factories (#251)
│       ├── pollution/tracker.go # Tracker: records uses, DetectViolations
│       ├── pollutionsource/     # Which uses leak a *gorm.DB; read-only query arguments
│       ├── cfg/                 # Control flow of one function
│       │   ├── analyzer.go      # Analyzer: the CFG cache of a pass (#299); CanReach: block reachability
│       │   ├── loop.go          # LoopInfo, DetectLoops: natural loops, range-over-func bodies (#177)
│       │   └── repeat.go        # Repeats.RepeatsAfter: may a use run again for one run of its root's definition (#249)
│       ├── handler/call.go      # handleSite for calls, go and defer; Send, Store, MapUpdate, MakeInterface
│       ├── stmtwrite/           # Cache.Of, Write: does a body write a *gorm.DB parameter's query through a direct path (#405)
│       └── purity/              # Body contracts of the directives
│           ├── validator.go     # pure (Validator, ReturnedParams)
│           ├── immutable_return.go  # immutable-return (ImmutableReturnValidator), InferImmutableReturns (#261)
│           ├── immutable_input.go   # immutable-input(name) (ValidateImmutableInputs)
│           └── violation.go     # Violation
│
├── testdata/src/                # See Testdata organization
└── e2e/internal/                # Runtime checks against real GORM with sqlmock (separate module)
```

The `plugin` package registers `Analyzer` with golangci-lint's module plugin system. `Analyzer` reads no flags, so the plugin hands it over as it is and rejects any settings. It lives in the main module. `plugin-module-register` adds nothing beyond `golang.org/x/tools`, and a nested module would need its own tag per release.

</details>

### Analysis pipeline

`analyzer.go` builds the directive sets, and runs buildssa only when gorm is reachable. `RunSSA` then runs these passes, and every report goes through `directive.Ignores`:

| # | Pass |
| --- | --- |
| 0 | Import facts, infer immutable-return functions (#261) |
| 1 | Validate pure functions to a fixed point (`purity.Validator`) |
| 2 | Collect Scopes and immutable callbacks (`callback` package), infer immutable-input and immutable-param functions (#349) |
| 3 | Check immutable-input and immutable-return bodies |
| 4 | Scopes Session warning (`scopeswarn`) |
| 5 | The [immutable-param body rule](#immutable-param-body-rule) (#346). Then, for each function: detect violations (`ssa.Analyzer`: track uses, then `DetectViolations` by CFG reachability), and add fixes. Last, the contract violations of the package initializer (#346) |
| 6 | Redundant and unused directives, [distrusted contracts](#distrusted-contracts), export facts, unused ignores |

<details><summary>Pipeline diagram and shared state</summary>

```
┌─────────────────────────────────────────────────────────────────────────┐
│                         analyzer.go (public)                            │
│  run(): directive sets, buildssa (only if gorm is reachable) → RunSSA   │
└─────────────────────────────────────────────────────────────────────────┘
                                    │
                                    ▼
┌─────────────────────────────────────────────────────────────────────────┐
│                      internal/run.go       RunSSA                       │
│  Every report goes through directive.Ignores. Package-level passes:     │
│    0. Import facts, infer immutable-return functions (#261)             │
│    1. Validate pure functions to a fixed point (purity.Validator)       │
│    2. Collect Scopes and immutable callbacks, infer immutable-input     │
│       and immutable-param functions (#349)                              │
│    3. Check immutable-input and immutable-return bodies                 │
│    4. Scopes Session warning (scopeswarn)                               │
│    5. immutable-param body rule. For each function: detect violations   │
│       (ssa.Analyzer), add fixes. The package initializer's contracts    │
│    6. Redundant and unused directives, distrusted contracts, export     │
│       facts, unused ignores                                             │
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

Shared state per pass:

| Object | Made in | Shared by |
| --- | --- | --- |
| `cfg.Analyzer` (#299) | `run.go`, once per pass | The checker (the tracker and handler context of every function) and `fix.Generator`. The tracer keeps its own for `closureRunsRepeatedly` |
| `RootTracer` (#325) | `run.go`, once for the reuse analysis of every function | Every function of the pass. `computeNeedsImmutableParam` makes one for its counterfactual analyses. `inferImmutableInputs` and `inferImmutableParams` make one per step, since their sets grow (#349) |

An `Analyzer` is not safe for concurrent use. A pass runs on one goroutine.

</details>

### Positions

All internal positions use `PositionFor(pos, false)`, so `//line` directives cannot move them (#151, #152, #153). This covers file keys, ignore lines, directive placement, the generated-file skip and fix offsets. It also covers the line and column that indent the parameter Session fix: a `//line` without a column sets every adjusted column to 0 (#244).

<details><summary>The one adjusted position, and its guard</summary>

Only the `root at file:line` text in messages stays adjusted, to match the position the driver prints. For a range variable of a range-over-func loop, the root is a parameter of the synthetic yield function. Its own position is in the declaration of the yield type, such as `iter.Seq` in the standard library. So `rootPos` names the range variable in the `for` statement instead (#378). `TestNoAdjustedPositions` in `internal/position_guard_test.go` fails on any other `Position` call, or `PositionFor` without `false`, in non-test code under `internal/`. Its allowlist names the message function `Tracker.loc`.

</details>

### Deterministic reporting

The same input must give the same diagnostics and fixes on every run (#184). Golden tests, golangci-lint caching and `-fix` depend on it.

| Rule | How |
| --- | --- |
| Roots are visited in a fixed order | `Tracker.DetectViolations` never uses map order. The order is root position, then earliest use, then SSA name, function and instruction text. The later keys matter only for roots without a position, such as Phi nodes |
| Each position is reported once | Several roots can be reused at one position (`use2(q, r)`). The diagnostic then takes the message of the first root in that order: the root that appears first in the source. It carries the fixes of every root there, merged into one `SuggestedFix`, because drivers apply only the first fix of a diagnostic |
| Every diagnostic of a root carries the root's fix (#246) | Each fix is complete on its own (#200), so the copies are identical, and drivers apply a shared edit once. The checker generates each root's fix once and reuses it |
| An ignored root gets no fix (#312) | A root with a diagnostic that an ignore suppresses gets no fix at all. `checker.analyze` runs on every function of the package before `checker.report` reports any. It asks `Ignores.Covers`, which marks nothing used, of each violation, and collects the roots that an ignore covers (#346) |
| Reports are sorted (#273) | `internal.ReportInOrder` holds back every diagnostic of the pass and reports them sorted by file name, offset and message |

<details><summary>Tests</summary>

`TestDeterministicDiagnostics` runs the analyzer 50 times over `testdata/src/determinism` and requires identical output, reported in position order. `testdata/src/determinism/unused.go` holds unused directives of several kinds for the order check.

</details>

> [!WARNING]
> Range over a map only where the order cannot reach a diagnostic, or sort first.

<details><summary>Why each rule exists</summary>

- **Merged fixes (#184).** Before, the first violation in map order won the position, and the fixes of the other roots were dropped. So `-fix` needed several passes.
- **Every diagnostic carries the fix (#246).** Before, the root was marked fixed on its first violation. That happened even when an ignore suppressed that diagnostic, or another function had already reported the position, so the fix was lost. An editor also offered the quick fix only on the first diagnostic.
- **No fix for an ignored root (#312).** The ignore says that the reuse is meant. The fix changes it too: it isolates the root, or reassigns the ignored statement. The next run would then report the ignore as unused. Deleting the ignore in the same fix was rejected. The fix would have to know that the ignore covers no other diagnostic, of any kind or variant. It would also have to know that it resolves every diagnostic of the root, before the pass has reported them all. `testdata/src/gormreuse/fix_ignored_root.go` holds the cases. Before #346 the checker asked only the violations of the function it reported. A stored closure is a function of its own. So an ignore in `f := func() { q.Find(nil); q.Count(nil) //gormreuse:ignore }` left the fix on `q.First` in the enclosing function. The next run then reported the ignore unused. `testdata/src/converge/fix_escape.go` checks that the fix of another root leaves the ignore used.
- **Sorted reports (#273).** `RunSSA` and the directive checks do not report in position order: the unused-directive reports come from map ranges. The text output is sorted by the driver anyway, but `-json` lists diagnostics in report order. File names are compared, not `token.Pos`, because the loader parses files concurrently and their order in the file set can vary.

</details>

### Skipped files and internal errors

Code is left out of the analysis in two ways only, and neither is silent (#186).

| Case | Handling |
| --- | --- |
| Generated file | `isGeneratedFile` in `analyzer.go` decides per file. A file marked `// Code generated ... DO NOT EDIT.` is skipped, with the cgo exception below |
| Panic while analyzing a function | `panicGuard` in `internal/guard.go` recovers it. It reports `gormreuse: internal error analyzing <func>: <panic>; please report this at ...` at the function, once per function, through `pass.Reportf`. No ignore directive suppresses it |
| Panic in a package-wide stage | `panicGuard.runPackage` reports `gormreuse: internal error in <stage> of package <path>: ...` at the package clause of the first file |

`GORMREUSE_DEBUG_PANIC=1` re-panics with the function name, for a stack trace.

**A panic leaves the conservative result (#315).** `panicGuard.run` reports whether the work finished.

| What panicked | Result |
| --- | --- |
| A function's `pure`, `immutable-return` or `immutable-input` check | The function breaks the contract: it is not trusted and exports no fact for it |
| A function's `immutable-param` counterfactual | The function needs the directive, so no fix deletes it |
| A package-wide stage | Nothing is inferred immutable-return. No callback is exempt as immutable. Every `immutable-return` and `immutable-input` contract is left out of the facts |
| Collecting the Scopes callbacks | None is collected, so their parameters are not always mutable. This one reports less, and the internal error says so |
| Collecting the package-level function variables | No call resolves through a variable, so such calls have no known callee (#348) |
| Reporting unused or redundant directives or unused ignores | The rest of those reports is lost, and the internal error says so (#348) |

<details><summary>Test</summary>

`TestPanicInjection` in `internal/guard_injection_test.go` injects a panic into each stage.

</details>

<details><summary>cgo output and the generated-file check</summary>

A package that imports `"C"` reaches the analyzer as cmd/cgo's output, not as the user's files. Each output file starts with `// Code generated by cmd/cgo; DO NOT EDIT.`. Then comes a `//line` directive back to the user's file, then the user's file with its comments. `ast.IsGenerated` alone would skip every user file of a cgo package.

So when that header is followed by a `//line` directive before the package clause, the header is ignored, and the user's own comments decide. A user file marked `// Code generated ... DO NOT EDIT.` is still skipped. cgo's own files, such as `_cgo_gotypes.go`, have no such `//line` directive and are still skipped. All positions still come from `PositionFor(pos, false)`, so the file keys are the cgo output files, consistently. `testdata/src/cgofile` covers this with a real cgo build, and is skipped without a C compiler.

A diagnostic in a cgo file has no fix (#346). Its edits would be offsets in cgo's output. But `-json` names the user's file, through the `//line` directive. A consumer that applied the edits to that file would corrupt it. `ReportInOrder` drops the fixes of a diagnostic with an edit in a file that `IsCgoOutput` reports. The diagnostics are still reported. The `-fix` drivers already skipped those fixes, since they skip every fix that edits a file `ast.IsGenerated` reports. `TestCgoFileHasNoFixes` checks it.

Before #186 a panic was dropped with every diagnostic of the function. Since #190 an ignored function is analyzed too, so a `panicGuard` report can come from one.

</details>

## Call sites

A call, a `go` statement and a `defer` statement are each one `callsite.Site`. `callsite.SiteOf` reads the instruction, and `callsite.EachSite` visits every one. Every reader of a call reads it through the `Site` (#257). The readers are the handler, the tracer, the pure validator and the body checks. So are the callback collectors, `DropReachableCallbacks`, the Scopes warning, the read-only stdlib check, the closure-result check and the fix generator.

| Kind | Code | `Declared` | `Recv` and `Args` |
| --- | --- | --- | --- |
| `SiteStatic` | `f(x)`, `r.m(x)` | The function | As written |
| `SiteBound` | `w := r.m; w(x)` | The declared method (`origin.Declared` of the `m$bound` wrapper) | `Recv` is `MakeClosure.Bindings[0]`; `Args` are the other arguments |
| `SiteThunk` | `T.m(r, x)`, `(*T).m(r, x)` | The declared method (`origin.Declared` of the `m$thunk` wrapper) | `Recv` is the first argument; `Args` the rest |
| `SiteFuncVar` | `var f = func(...)`, then `f(x)` | The closure the variable holds | As written |
| `SiteClosure` | `func() {...}()` | The function literal | As written |
| `SiteInvoke` | `i.m(x)` | None, or the `*gorm.DB` method when `i` boxes a `*gorm.DB` right before the call ([Interface method calls](#interface-method-calls)) | An interface method: an unknown callee. `Recv` is the boxed `*gorm.DB` when there is one |
| `SiteDynamic` | A func-typed parameter, field or merged func value | None | An unknown callee |
| `SiteBuiltin` | `append`, `len` | None | |

`Site.Mode` is `SiteCall`, `SiteGo` or `SiteDefer`. Code that reads the arguments of a call reads them from the `Site`, which aligns them to the declared method. A bound wrapper has no receiver argument, and a thunk takes the receiver as its first one. `Site.ArgParam` maps an argument to its parameter. A static method call's arguments start with the receiver.

### Declared functions: generic instances, method values and method expressions

A call can resolve to a stand-in for the function the directive is written on (#180):

<details><summary>The stand-ins</summary>

| Stand-in | Example | SSA name |
| --- | --- | --- |
| An instance of a generic function or method | `genBranch[int]` | |
| A bound method wrapper | `f := r.m` | `m$bound` |
| A method expression thunk (#258) | `T.m(r, x)` | `m$thunk` |

</details>

The directive sets, and the maps keyed by `*ssa.Function` (`PureVerdicts.Failed`, `needsImmutableParam`, the Scopes and immutable callback sets), are keyed by the declared function. `origin.Declared` maps a stand-in to it: an instance to its `Origin()`, a bound wrapper or a thunk to the declared method (`Prog.FuncValue`).

`Prog.FuncValue` finds only the methods of the packages that the program created. buildssa creates the analyzed package and its direct imports. So a package that reaches gorm only through another package has no gorm method in the program, and `w := q.Where` gave a wrapper that was no gorm method. Each call through it was missed, and `-fix` rewrote the other uses as if `w` were not there (#374). go/ssa builds such a method from type information when the wrapper calls it, so `origin.Declared` then takes the wrapper's callee (`wrappedMethod`).

<details><summary>Every lookup that goes through it</summary>

Every lookup goes through it: `DirectiveFuncSet.Contains`, `ImmutableInputSet.Callbacks`, the `PureVerdicts.HasFailed` check in `IsPureFunction`, the immutable-param caller check, and the callback collectors (`callback/resolve.go`: `resolveMayFuncs` for Scopes/Preload, `resolveExactFunc` for Transaction/FindInBatches and immutable-input). An interface method value or expression has no declared body, so `origin.Declared` leaves it unchanged, and it carries no directive.

</details>

### Method values

The tracer resolves a call `w(x)` through a method value `w := r.m` as the call `r.m(x)` (#178). `traceCall`, `traceAll`, `isImmutableSource`, `pureReturnedArgs` and the Session/Debug copy checks (`aliasSharesStatement`, `Site.SessionEffect`, `collectAliasedRoots`) read the call through it. So do the call handler (for a plain call, `go` and `defer`), the pure validator and `pollutionsource.CallReadOnlyArgs`.

<details><summary>Each call shape and its result</summary>

| Code | Result |
| --- | --- |
| `w := q.Where; w("a")` | A branch of `q`'s root (call handler), and a new mutable root |
| `w := q.Where; q = w("a")` under a condition or in a loop | An assignment of `q`, as for `q = q.Where("a")` (#254) |
| `f := q.Find; defer f(nil)` | A deferred branch of `q`'s root, as for `defer q.Find(nil)` (#254) |
| `w := db.Session(&gorm.Session{}).Where` | Each call is a fresh root. No reuse |
| `s := q.Session; c := s(&gorm.Session{})` | `c` is a copy that shares `q`'s Statement, as with `q.Session(...)` |
| `s(&gorm.Session{Initialized: true})` | A mutable root |
| `f := r.build; f()` | A call of the user method `build`, like `r.build()` |
| `f := o.open; q, err := f()` | The call is a mutable root, as for a tuple result of `o.open()` (#176) |
| `f := h.same; f(q)`, `same` pure and returning its argument | An alias of `q`'s root, as for `h.same(q)` (#188) |
| `firstPage(q.Find)`, also through a variable that holds `q.Find` | A branch of `q`'s root at the call, as for a closure that calls `q.Find` (#374) |
| `j := job{find: q.Find}`, `[]finder{q.Find}`, `m[k] = q.Find`, `ch <- q.Find` | A branch of `q`'s root at the store or the send (#374). A store into a local variable is not: `FuncVars` reads the calls through it |

</details>

**A method value handed on (#374).** A method value passed to a function, or stored anywhere but in a local variable, may be called there. The call through it is no call site of `q`: it is a dynamic call, or a call in another function. So the hand-off is the branch, as `helper(q)` is (`boundGormReceiver`, `handOn`). It is reported when it runs again, as in a loop. Before #374 `firstPage(q.Find); q.Count(&n)` and `j := job{find: q.Find}; j.find(&a); q.Count(&n)` were missed, while the same code with a closure was reported. `-fix` offers no fix for such a root, as for any hand-off.

<details><summary>Interface method values, wrappers and the position guard</summary>

An interface method value has no declared method, so `origin.Declared` keeps the wrapper. Its signature has no receiver. A call through it is handled like any other call whose result holds a `*gorm.DB`: a mutable root, as for an interface call `r.Query()` (#176).

A method value's wrapper is never analyzed as a closure of its caller (`processFunction`, `isMethodValue`). That holds even when its receiver holds a `*gorm.DB`, so that the `MakeClosure` captures one. Analyzing it read the wrapper's parameter as a mutable parameter, and reported its forwarding call to an `immutable-param` method at no position (#255).

A function literal that captures only a method value is analyzed (#341). In `find := q.Find; func() { find(&a) }()` the literal captures the variable `find`, whose type is a func, not a `*gorm.DB`. `ClosureCapturesGormDB` also accepts a captured variable that may hold a method value of a receiver that holds a `*gorm.DB` (`closureHoldsGormMethodValue`). Before #341 the closure was skipped, so its call of `q.Find` was no branch.

As a last guard, `Analyzer.Analyze` moves a violation without a position to its call, then to the analyzed function (`positionedViolation`). A diagnostic at no position cannot be suppressed, and golangci-lint drops it.

The fixes for a method value are in [Suggested fixes](#method-values-and-fixes).

</details>

<details><summary>Rejected: trace into the wrapper as an IIFE</summary>

This was the old behavior. The wrapper's body is one `r.m(x)` call, so every call through `w` traced to that one call. They shared one root. Two calls through a method value of an immutable receiver were a false second branch. On a mutable receiver the chained use was reported again, with no root position, since the wrapper call has none.

</details>

### Method expressions

A method expression `T.m(r, x)`, or `(*T).m`, is a call of a synthetic `m$thunk` function whose first parameter is the receiver (#258). It is a `SiteThunk`, read as the static call `r.m(x)`. A package-level variable that holds a method expression (`var find = (*Repo).find`) is read the same way (#257).

<details><summary>Which directives apply, and the cases not followed</summary>

So every directive of `m` applies: pure, immutable-return, immutable-param, immutable-input, and the returned arguments of a pure method. This holds for a method of another package too. `(*gorm.DB).Find(q, nil)` is a gorm method call on `q`. The immutable-param isolation fix counts the receiver argument of a method expression (`receiverArgs`). `callback.DropReachableCallbacks` reads a thunk through `origin.Declared` too.

| Case | Handling |
| --- | --- |
| A method promoted through an embedded field: `type W struct{ *gorm.DB }; W.Find(w, nil)` | The thunk selects the field inside its body. `Recv` is then `w`, not `w.DB`, and the tracer does not follow it to the field. Reuse through it is missed (`exprPromoted`) |
| An interface method expression | No declared method, so it is a call of an unknown function |

</details>

### Interface method calls

An interface method call `i.m(x)` has no declared callee. When `i` boxes a `*gorm.DB` right before the call (the receiver is a `MakeInterface` of a `*gorm.DB`), the call runs that value's method. `callsite.SiteOf` then gives the site that method as `Declared`, and the boxed value as `Recv` (`siteBoxedMethod`, #374). So `var qi Querier = q; qi.Where("a").Find(&us); q.Find(&us)` is a second branch of `q`, and `qi.Session(...)` reads `q` as `q.Session(...)` does. Before #374 the call was no use of `q`.

Limitations:

- An interface value that comes from elsewhere, such as a parameter or a field, has no known dynamic type. A call through it stays an unknown call.
- A receiver whose type is a type parameter (`func twice[T interface{ Find(any, ...any) *gorm.DB }](q T)`) is not a `*gorm.DB` in the generic body, so its calls are missed (`irGenericReceiver`).

### Function variables

A call through a variable that holds one function is read as the call of that function. `callsite.FuncVars` resolves the variable. It remembers the result for each variable, so a variable called at n sites is scanned once.

| Variable | Resolved when | Site |
| --- | --- | --- |
| Package-level `var f = func(...)` (#183) | The package stores to it once, the stored value is a function, its address is used only to load and store it, and it is unexported | `SiteFuncVar` (`callsite.CollectFuncVars`) |
| Local variable captured by a closure (#304) | It has exactly one `Store`, in the function that declares it. Its address is only loaded, stored to, or bound to a closure, at any depth of nesting | The call of the stored value (`FuncVars.LocalValue`) |

<details><summary>Why each local condition holds</summary>

| Condition | Why |
| --- | --- |
| Exactly one `Store`, in the declaring function | A second store may give it another function. A store in a closure may run at any point |
| The address is only loaded, stored to, or bound to a closure | Any other use, such as `&f` passed to a call, may write it |

</details>

Otherwise the call may run another function: a second assignment, a write through a pointer, or an assignment in another package. An unresolved call stays a dynamic call. It pollutes its arguments, and its result is a mutable root (#176).

<details><summary>Package-level closures and local function variables in detail</summary>

**Package-level closures (#183).** buildssa puts only declared functions and their literals in `SrcFuncs`. The closure of `var f = func(...)` belongs to the synthetic package `init`.

- `runFuncs` adds the anonymous functions of `init`, nested ones included, to every pass. Their bodies get reuse detection and directive validation like any closure.
- A call through `f` carries every directive of the closure: `pure`, `immutable-return`, the `immutable-param` caller contract, and since #257 `immutable-input`. Its callbacks are collected, and forwarding a callback to the variable keeps the promise. A Transaction callback or an `immutable-input` callback can also be held by such a variable ([Callbacks held in a variable or a field](#callbacks-held-in-a-variable-or-a-field)).
- A diagnostic names the closure by its variable, not `init$1` (#259). Every message that names a called function goes through `callsite.FuncVars.FuncName`. It also names a method value or method expression wrapper (`m$bound`, `m$thunk`) by its method, and a generic instance by its generic function. A closure held by a variable that is not resolved keeps its SSA name.

**Local function variables (#304).** A local variable that holds a function and is never captured is no variable in SSA: `f := func(...) {...}; f(x)` calls the `MakeClosure` (or the `*ssa.Function`) directly. Once a closure captures `f`, `f` lives in an `Alloc`. Examples are a closure that calls it, a recursive closure, or an IIFE that reads it. The closure then reads `f` through a `FreeVar`, and each call loads it first.

`FuncVars.LocalValue` resolves a load of a local `Alloc`, directly or through a chain of `FreeVar`s, to the value stored into it. `SiteOf` reads the call as the call of that value:

| Stored value | Site |
| --- | --- |
| A function literal | `SiteClosure` |
| A method value | `SiteBound` |
| A function | `SiteStatic` or `SiteThunk` |
| A load of another such variable, or of a package-level one | Followed |

So the call is read exactly as the same call without the capture. A read before the store holds nil, and calling it panics, so every call that runs, runs the stored value. A store in a loop stores a new closure of the same function on each iteration.

The immutable-return body check (through the `Site` callee since #315) and the closure-factory rule (`closureCallOwnsRoots`) ask the same function. There is one sole-store implementation (`funcVarSoleStore`), which replaced the body check's own copy. The zero `FuncVars` (`SiteOfWithoutFuncVars`) resolves neither package-level nor local variables.

</details>

Limitations:

- A call through an exported, reassigned or address-taken `var f = func(...)` does not honour the closure's directives.
- A local variable that a branch assigns (`if c { f = g }`), or that a closure assigns, stays dynamic.
- A variable or field that holds a method value (`var f = r.m`) is not resolved by `FuncVars`.

<details><summary>History (#304, #301, #303)</summary>

- Before #304 `SiteOf` resolved only a static callee or a load of a package-level variable. Every call through a captured `f` was a `SiteDynamic`. Its directives were dropped, and the `immutable-param` caller contract was skipped. An inferred immutable-return was lost. An `immutable-input` callback was not collected, such as the recursive `transaction` wrapper in gorm's own transaction tests.
- Before #183 the body of a package-level closure was never analyzed. A `//gormreuse:pure` on it was never validated, and a call through `f` carried none of its directives.
- A closure called at n sites asks for its directive n times. Each answer walked the enclosing statement until #301, which [places every directive of a file once](#directive-lookup-cost).
- A closure that is stored in a variable read every store of a captured, reassigned variable until #303. It now reads the stores that reach its `MakeClosure` or run after it. So after `db = db.Session(&gorm.Session{})`, a factory `base := func() *gorm.DB { return db.Model(nil) }` that an IIFE also calls is a factory again (`cfvFactoryReassigned`).

</details>

## Callbacks

Some functions hand a fresh `*gorm.DB` to a callback. Reuse inside such a callback is always allowed, because its parameter is immutable. The callback sets are keyed by the declared function, not by the call that passes it.

| Callee | Callback receives | Exempt |
| --- | --- | --- |
| gorm's `Transaction` | `Begin`'s result, `clone > 0` | Yes |
| gorm's `FindInBatches` | `Session(&Session{NewDB: true})`'s result, `clone > 0` | Yes |
| gorm's `Connection` | `fc(db.getInstance())`, a `clone == 0` handle | **No.** Its callback's `tx` is an ordinary mutable parameter, and reuse inside it is reported (#173). Like a Scopes callback's, it stays mutable under `immutable-param` (#348): `CollectConnectionCallbacks` adds the callbacks to the set the tracers read. The fixes read the Scopes set only, so a Connection callback still gets the body Session |
| A function marked `//gormreuse:immutable-input(name)`, or inferred so (#349) | What its body passes; checked ([immutable-input](#immutable-input-body-contract), [inferred](#inferred-immutable-param-and-immutable-input)) | Yes |
| `Scopes`, `Preload` | The query being built | No. Collected for the parameter rules and the [Scopes warning](#scopes-session-family-warning-temporary) |

<details><summary>Collectors, callback positions, go and defer</summary>

`e2e/internal` guards the three gorm premises against real gorm. The collectors are in `callback/collect.go`: `CollectImmutableCallbacks`, `CollectImmutableInputCallbacks` and the Scopes/Preload collectors. `callback.CollectedCallbackArgs` lists the callback positions of a call, and every collector uses it.

**Callback positions.** A reference counts as a callback position when it is the Transaction/FindInBatches callback argument or an `immutable-input(name)` argument. The Transaction/FindInBatches positions come from the declared parameter types: a parameter of a function type with a `*gorm.DB` parameter. Resolving each argument decided the position from what the value is. A function boxed in FindInBatches's `dest interface{}` then counted, and a callback read from a holder did not.

**Callbacks under `go` and `defer` (#256).** The callee hands the callback the same value however the callee itself is invoked. So the collectors read the call of every `CallInstruction`, not only `*ssa.Call`, through `callsite.EachSite`. `DropReachableCallbacks`, the immutable-param caller check and the immutable-input body check also read every `CallInstruction`. Before #256, `go db.Transaction(func(tx *gorm.DB) error {...})` and `defer withFresh(db, cb)` lost the exemption. A callback of a deferred `Scopes` call lost the Session warning.

</details>

### Named callbacks

A function's `*gorm.DB` parameter receives a fresh value only when every caller is a callback position (#191). `callback.DropReachableCallbacks` runs after both collectors. It removes a function from the set when:

| Case | Why |
| --- | --- |
| Any reference that is not an argument in a callback position. Examples: a direct call (also under `go`/`defer`, and recursion), a method expression `T.m`, a store, a `Phi`, a capture, a return, an ordinary argument | That reference can hand the parameter a mid-chain value |
| The function or method is exported | Another package can call it, and the analyzer cannot see that call. Facts do not change this (#195). They flow from a package to the packages that import it, so the declaring package is analyzed before any caller in another package |
| A method whose name matches an interface call, interface method value or interface method expression in the package | Dynamic dispatch can reach it |

<details><summary>Closures, dropped functions and captures</summary>

A closure (`MakeClosure`, including a bound method value `r.m`) is followed: it counts only when all its uses are callback positions. Any other wrapper, such as an interface conversion, counts as a non-callback reference, because both callback positions are func-typed. The scan covers the source functions, their nested literals and the package `init` (package-level variable initializers).

A function that is dropped falls back to the rules for an ordinary parameter. It can opt in with `immutable-param`, whose caller-side check then covers the direct calls.

A capture of a local variable holding the function (`cb := f; go func() { _ = db.Transaction(cb) }()`) is a non-callback reference. It may report a callback that is only ever passed to Transaction. That direction is the conservative one.

</details>

### Callbacks held in a variable or a field

A callback can be read from a holder (#257). `callback/holder.go` finds the holders of the package, and every reader takes the full `Site` with `FuncVars`.

| Holder | Condition | A read is |
| --- | --- | --- |
| Package-level variable | In `callsite.FuncVars`: unexported, stored once, its address only loaded and stored | `*g` |
| Struct field (keyed by its `*types.Var`, generic instances by their origin) | Unexported. Exactly one `Store` to it in the source functions and the package `init`. Every `FieldAddr` of it only loaded or stored. No `ChangeType`/`Convert` of its struct (or a pointer to it) anywhere in the package | `*(&x.f)`, or `x.f` on a struct in a register |

<details><summary>How a holder is resolved</summary>

The single store may run many times, but every value it stores is the same function or a closure of it. So every read of the holder is that function or nil.

- `resolveExactFunc` follows a read of a holder to the stored value.
- `DropReachableCallbacks` accepts a store into a holder as a callback reference in one case. Every read of the holder must itself be used only as a callback argument (`holders.feedsOnlyCallbacks`, through the same `refReachesOnlyCallback`).
- A read that calls the function, passes it to an ordinary parameter, or stores it elsewhere drops it, as a direct reference does.
- A cycle of holders feeding each other is dropped.

</details>

<details><summary>Decisions</summary>

- **Exactness rests on `DropReachableCallbacks`, not on the resolver.** The exemption only claims that the function's parameter receives fresh values. That holds when every reference reaches only callback positions, whatever the resolver found. A wrong resolution can only exempt a function whose every reference is a callback position, or miss one, which reports more.
- **Field holders are per field, not per object.** Two `svc` values share the field's holder. That is why one store in the whole package is required: with two stores, a read may hold either function.

</details>

**Not followed.** Each of these keeps its callback's parameter mutable:

- A local struct field stored twice.
- A field assigned by reflection, or through `unsafe` arithmetic beyond a plain `unsafe.Pointer` conversion of the struct.
- An exported field or variable.
- A variable or field that holds a method value (`var f = r.m`).

<details><summary>Before #257</summary>

The collectors and `DropReachableCallbacks` read each call through its static callee alone. `resolveExactFunc` resolved only a function, a closure and an interface conversion. So these all reported reuse inside the callback:

- `db.Transaction(txBody)` with `var txBody = func(tx *gorm.DB) error {...}`
- `db.Transaction(s.cb)` with a struct field
- a call of an `immutable-input` closure held by a package-level variable
- `tx := db.Transaction; tx(cb)`

Forwarding an `immutable-input` callback to such a variable reported `callback cb escapes (passed to a function value)`.

</details>

## Control flow

The `cfg` package answers two questions for one function: can block A reach block B (`CanReach`), and may a use run again (`cfg.Repeats`). `run.go` makes one `cfg.Analyzer` per pass (#299).

<details><summary>What the analyzer caches</summary>

| Cached per function | Filled |
| --- | --- |
| The blocks each block reaches, as a bitset indexed by `b.Index` | On the first `CanReach` from that block |
| `LoopInfo` (`DetectLoops`) | Once |
| `Repeats` on its own (`Repeats`), as a closure of each parent `Repeats` (`Closure`), and the closure's sites | Once |
| Whether a closure's sites repeat after a definition (`sitesRepeatAfter`) | Once per definition block |

A block that is not `fn.Blocks[b.Index]`, such as one built by hand, is still searched.

</details>

<details><summary>Before the cache (#299), and why it is fast</summary>

| Cached | Before #299 |
| --- | --- |
| Reach bitsets | A breadth-first search on every `CanReach` |
| `LoopInfo` | Once per function analysed, and once per use in the fixes |
| `Repeats` | `RepeatsOf` rebuilt the chain, with `DetectLoops`, on every use the fixes asked about |
| `sitesRepeatAfter` | Asked again at every depth, so `k` sites per level of `d` nested closures took `k^d` steps |

- `Closure` stops comparing sites once two may both run (`multi` answers every question then).
- Whether two sites in one function may both run takes one pass over the function. Each block is labelled with at most two of the sites whose paths reach it (`repeatRunReachesAnother`). So a closure called on `n` exclusive branches takes time linear in the function, not `n` reachability questions.
- The sites of a closure are found from its referrers, not by scanning the declaring function. A chain of `n` closures that call each other is not quadratic either (`loopclosurecases`, `closurechain` and `closurecallers` in the growth gate).
- `mergedStoreReadTwice` stops at the second use after a store.
- `cfg.Analyzer.reachSet` resliced its queue's head off, so the queue was reallocated again and again for every source block. It is now allocated once.

The results do not change. The goldens, and `-json` and `-fix -diff` over `e2e/internal`, the testdata and gorm's source, are byte-identical.

</details>

### Loops

`DetectLoops` finds natural loops and keeps one block set per loop header. `IsInLoopWithout(use, def)` asks for a loop that holds the use but not the definition.

| Rule | Detail |
| --- | --- |
| Nested loops (#252) | Each loop has its own block set. A definition runs as often as a use only when they are in the same loop. So `for … { q := …; for … { q.Find(nil) } }` and a use in a later loop are reported |
| `for cond` loops (#252) | Back-edges are found by block index, and go/ssa puts the header of `for cond { … }` after its body. So the edge from the header to the body looks like a back-edge, with the body as its header. The walk from a header keeps to the blocks that the header reaches |
| Range-over-func bodies (#177) | See below |
| A root defined in the loop body (#375) | Two uses of it on exclusive branches are not a reuse. See below |

**Range-over-func loops (#177).** SSA compiles the body of `for v := range seq` (Go 1.23+) into a synthetic closure. The iterator calls it once per element. go/ssa marks it with `Synthetic == "range-over-func yield"`. The closure has no back-edge, so `DetectLoops` treats every block of it as in a loop.

- A root defined outside the body and used inside it is reported, as in a plain `for range`.
- A root defined inside the body is in a loop block, so it is fresh on each iteration.
- The body reaches outer variables through `FreeVar`s, so `q = q.Where(v)` inside it is a `Store` to a `FreeVar`, not to an `Alloc`. `isAssignment` treats a `FreeVar` as a merge point like an `Alloc` ([Which merges are reassignments](#which-merges-are-reassignments)). Its values are those of the captured variable, so the store is an assignment only when that variable can hold `root`. That is the in-place update of a plain loop. A store into another variable, such as `last = q.Where(v)`, still counts as a use of `q`.
- A closure nested inside the body (an IIFE, a `defer`) runs once per run of the body, so it repeats as the body does.
- A store in the body is a store at the loop (#341). The iterator calls the body only while the loop statement runs. So a read after the loop sees the body's stores. It also sees the value before the loop, since the body may run no time. That is what the loop-header Phi of a plain loop gives ([Captured variables](#captured-variables)).
- For the same reason, a read in the body sees only the stores that reach the loop, not those after it.

**A root defined in the loop body (#375).** Each run of a call defines a new root. Take `for … { q := s.Where(…); if c { q.Delete(…); continue }; q.Update(…) }`. The only path from one use to the other goes through the back-edge, and that path runs `s.Where` again. So the two uses never share a root. `Tracker.reaches` asks for a path, from either use to the other, that does not enter the block of the root's call (`cfg.Analyzer.CanReachAvoiding`). It asks only when all of these hold:

| Condition | Why |
| --- | --- |
| The root is a `*ssa.Call` | A Phi, a parameter or a loaded field is the same value on every path |
| Both uses are direct (`UsageInfo.Direct`): the operand is the root, or a chain of `*gorm.DB` method calls on it, and the site names its function | Such a use sees the root of the latest run of the call. A value that a merge, a variable, a field, a method value or a closure holds may be the root of an earlier iteration. `prev := q` on one iteration and `prev.Find(nil)` on the next is a reuse (`exclusiveLoopPrevious`) |
| Both uses are in the function that defines the root | A closure use has a block of another function |
| The plain reachability holds | So the rule only removes reports |

Uses that run in one iteration are still reported (`exclusiveLoopSameIteration`), and so is a root defined before the loop (`exclusiveRootOutsideLoop`). A root assigned on some iterations only is held by a Phi, so its uses are not direct (`exclusiveLoopKept`). The loop-carried check of #182 keeps the plain reachability: its use is the call itself, so every path to it runs the call.

<details><summary>Decisions (#375)</summary>

- **A call is taken to return a new value on each run.** The loop rules take it so already: one use of a root defined in the loop body is not a reuse. A helper that returns the same mutable `*gorm.DB` on each call is missed in both cases.
- **Both directions are asked.** A use may run before another one that comes earlier in the source, as through a `goto`. The pair is still checked only when the earlier use reaches the later one, as before.
- **The block of the call is avoided, not the call itself.** A direct use is dominated by the call. So a use in that block comes after the call, and a path that enters the block runs the call before it reaches the use.
- **Rejected: avoid the definition for every use.** It missed `exclusiveLoopPrevious`, where one branch uses a variable that holds the root of the previous iteration.
- **A defer is checked against the uses that may run in the same run of the function.** A deferred call runs when the function returns, so it runs after every use on the path that registered it, and after none on an exclusive path. Before, every recorded use counted, so `if c { defer q.Find(&us) } else { defer q.Count(&n) }` was reported.

</details>

<details><summary>Before #252 and #341</summary>

- **Nested loops.** `markLoopBlocks` used one set for every loop as its visited set. So the walk of an outer loop stopped at an inner header marked before it. The outer loop's blocks before the inner loop were then in no loop. Any loop counted, and a definition in any loop was taken to run as often as the use.
- **`for cond` loops.** The body taken as a header does not dominate the edge's source. So the walk from it went on through the blocks before the loop, up to the entry. A root defined at the entry, such as a parameter, then counted as defined in a loop. A use before the loop in a closure counted as repeating.
- **Range-over-func reads after the loop (#341).** Such a read saw only the value before the loop. So `for k, v := range maps.All(f) { q = q.Where(k, v) }; q.Count(&n); q.Find(&us)` was missed.
- **Range-over-func reads in the body (#341).** Such a read also saw every later store, as a spawned closure does. So `q = db.Where("late")` after the loop made a query in the body a reuse on every iteration.

</details>

<details><summary>Effect on gorm's own source (#252)</summary>

In gorm's root module the loop fixes add six reports (154 to 160). Each is a `*gorm.DB` parameter branched in a loop: in `Scan` (three), `CreateInBatches` (the parameter of its batch closure), `SaveAfterAssociations` and `SetupUpdateReflectValue`. Before, the parameter counted as defined in a loop. A `for cond` loop in the function marked the entry block, or the use's block was left out of an outer loop. None comes from the closure rules. The `tests` module is unchanged (34).

</details>

### Uses that run more than once

A use that may run more than once for one run of its root's definition reuses the root, even with one use in the source. `cfg.Repeats.RepeatsAfter(use, def)` is the one query for it (#249).

| Asked by | For |
| --- | --- |
| The handler | Every root of a branch, of a call, a `go` or a `defer` (#306), a send or a store (#374) |
| The suggested fixes | `usedAgainInLoop`, `changeSeenAfterEscape`, the Session of a reassignment in a loop. So a fix and the diagnostic agree |
| `RunsAgain(block)` | Only whether a block can run after itself, for a loop-carried read (`mergedStoreReadTwice`) |

`RepeatsAfter` is true when:

| Case | Example |
| --- | --- |
| The use is in a loop that does not hold `def` | `q := …; for … { q.Find(nil) }` |
| The use is in a closure that runs at two sites that may both run for one run of its `MakeClosure` | `f := func() { q.Find(nil) }; f(); f()` |
| A site where the closure runs is in a loop of the declaring function that does not hold `def`, at any depth | `for … { f() }` |
| A site is in another closure that runs more than once, or in a loop of it (#305) | `run := func() { for … { update(id) } }; run()` |

A site is any use of the closure value: a call, a `go` or `defer` statement, an argument, a store.

<details><summary>Examples of each case</summary>

<table>
<tr><th>Case</th><th>Example</th></tr>
<tr><td>

The use is in a loop that does not hold `def`. This includes a root of an outer loop used in an inner loop, and a root of one loop used in a later loop.

</td><td>

```go
q := …
for … { q.Find(nil) }
```

</td></tr>
<tr><td>

The use is in a closure that runs at two sites that may both run for one run of its `MakeClosure` (one can reach the other).

</td><td>

```go
f := func() { q.Find(nil) }
f(); f()
g.Go(f); g.Go(f)
```

</td></tr>
<tr><td>

A site where the closure runs is in a loop of the declaring function that does not hold `def`, at any depth.

</td><td>

```go
for … { f() }
for … { func() { … }() }
for … { go func() { … }() }
for … { g.Go(func() error { … }) }
for … { funcs = append(funcs, func() { … }) }
```

</td></tr>
<tr><td>

A site is in another closure that runs more than once, or in a loop of it (#305).

</td><td>

```go
run := func() { for … { update(id) } }; run()
db.Transaction(func(tx *gorm.DB) error {
    update(1); return update(2)
})
go func() { for … { update(id) } }()
```

</td></tr>
</table>

</details>

**Sites.** A site is any use of the closure value: a call, a `go` or `defer` statement, an argument, a store. `callsite.ClosureRuns` finds them.

<details><summary>How sites are found</summary>

| Use of the closure value | Handling |
| --- | --- |
| A comparison with nil (`if f != nil { f() }`) | Not a site: it does not run the closure (#309) |
| A Phi of the closure, or a local variable that holds it | Followed to their uses. `held = f` in a loop with `held()` after it is one site after the loop |
| A conversion to a named func type (`ChangeType`, #341) | Followed to its uses. `h := handlerFunc(func() { … }); h(); h()` is two sites. Before #341 the conversion was one site, and the body counted as run once |
| A closure that captures that variable, at any depth | Followed. A call of `update` inside `run` is a site in `run`, and asks `run`'s own `Repeats` |
| A slice element or a struct field | Not followed. The store is one site, and a call through it is a dynamic call (`repeatsClosureCalledThroughSlice`) |
| Calls on two exclusive branches (`if c { f() } else { f() }`) | Not two runs |

`ClosureRuns` also gives the checker and the tracer a closure's plain calls (`callsite.ClosureCalls`). These are the single call that orders and places a closure's uses ([Closure uses](#closure-uses)), and the calls at which a captured variable is read (#251). Before, each of the three walked the referrers in its own way. A call through a captured variable was then a site for one and not for the others.

</details>

<details><summary>Where the definition is</summary>

| Definition | Handling |
| --- | --- |
| In a function that declares the use's function, at any depth | Asked at the site where the closure runs |
| In a closure that the use's function runs, such as a root returned by an IIFE | Moved to the block where that closure runs when it runs at one site, or where it is declared otherwise. Before, a root an IIFE returned in a loop was reported on every iteration |
| In an unrelated function, or a global | Outside every loop and closure |

A closure analysed on its own (each closure is also a function of the package) asks only its own loops. The analysis of the function that declares it asks the sites, so one reuse is not reported twice.

</details>

<details><summary>Decisions</summary>

- **Sites in two functions may both run (#305).** Take a closure called once in the declaring function and once inside another closure, or inside two closures. It is taken to run twice. This holds even when the calls are on exclusive branches (`rcsExclusiveAcrossClosures`). Ordering the two would mean lifting each site to the sites of the closure that holds it. A site in the closure itself, or in one nested in it, is a recursive call, so the closure runs again. Two closures that call each other each ask for the other's sites. A question asked again while it is being answered counts as a repeat, since they recurse.
- **A closure made on each iteration (#306).** Take this loop: `for … { save := func() { … }; if c { save() } else { save() } }`. It makes a new closure on each iteration, and each runs once. Two sites in the declaring function may both run only on a path that does not run the `MakeClosure` again. The path through the back-edge does, so the two calls are exclusive. This holds only when every site uses the `MakeClosure` itself. A Phi or a variable may still hold the closure of an earlier iteration, as `if c { last = save }; last()` does. Then any path counts, and one site that can reach itself runs again (`rcsPerIterationKept`).
- **Every root of a merge (#306).** Take `q := db.Model(nil); if c { q = q.Where("a") }`. A use of `q` in a loop then reuses both roots. Each root that the merge may hold is asked, with two exceptions. One is a root the use reassigns. The other is an immutable call such as a `Session` result, which `FindAllMutableRoots` keeps (`rcsMergedSessionInLoop`). The violations share the position, so one diagnostic carries a fix for every root. The tracker files the use under the other roots as a may branch. `Tracker.CollectViolations` lists the may branch at the violation's position, so their fix has a use.
- **A closure passed to another function runs once** unless the site is in a loop or there are two sites. `slices.SortFunc(s, func(a, b T) int { q.Find(nil); … })` is missed. Counting every argument as a repeat would report each `db.Transaction(func(tx *gorm.DB) error { … })`.
- **Where it is reported** follows the closure's position rules ([Closure uses](#closure-uses)): at the body, or at the one later call site. `f := func() { q.Find(nil) }; for … { f() }` is reported at `f()`.

</details>

<details><summary>History (#305, #306, #309)</summary>

- Before #305 the capturing `MakeClosure` was one site in the declaring function. So `update` called in a loop of `run`, or twice in a `Transaction` callback, an IIFE or a goroutine, was counted once.
- Before #306 the back-edge made every pair of sites in a loop reach each other, and the per-iteration closure case was reported.
- Before #306 only the first root of a merge (`FindMutableRoot`) was asked. The first run reported only `Model`, its fix added a Session there, and a second run reported `Where`. `AllUses` did not list the may branch, so the other roots' fix had no use.
- Before #309 the `last != nil` of the `rcsPerIterationKept` fixture counted as a second site, and a single site was not asked.

</details>

## Root tracing

`tracer.RootTracer` answers where a `*gorm.DB` value's mutable root is.

| Query | Answer |
| --- | --- |
| `FindMutableRoot` | The single root: the first edge of a Phi, or the first store into a variable, that has a root (#340) |
| `FindAllMutableRoots` | Every root the value may hold, through Phi, variables and closure captures |
| `HasMutableRoot` | Whether a root is in that set. A list of 16 or more roots is kept as a set |
| `FindAliasedRoots` | The roots a value reads through `Session`/`Debug` copies and read-only arguments ([Session and Debug copies](#session-and-debug-copies)) |
| `IsImmutableCall` | Whether a call's result is immutable: its last call is an immutable-returning builtin or an `immutable-return` function |
| `RootBlock` | The defining block of a root ([Defining block](#defining-block)) |

<details><summary>Which code makes a root</summary>

An immutable value has no root. A mutable value comes from a chain method.

| Code | Root |
| --- | --- |
| `q := db.Where("x").Session(...)` | None: `q` is immutable, since the Session is at the end |
| `q := db.Session(...).Where("x")` | `Where`: a chain after a Session is mutable |
| `q := db.Where("x")`, then `q = db.Where("y")` | Two roots. Each assignment is a new SSA value, so a reassignment is a new mutable root, unrelated to the old one |
| `q = buildQuery(q, "y")` | `buildQuery(q, "y")` is the first branch of the old `q`, and the call is a new mutable root |
| `buildQuery(q, "w")`, result discarded | A branch of `q`. A later `q.Find(nil)` is a second branch |
| `r := buildQuery(q, "v")` | A branch of `q`, as for `r := q.Where("v")` |

The function may return `*gorm.DB`, and `isAssignment` may find a merge that hands `q`'s root back. Then `handleArgs` uses `RecordAssignment`, as `handleReceiver` does for a gorm method ([Which merges are reassignments](#which-merges-are-reassignments)).

</details>

### Results the tracer cannot see through

`traceCall` makes a call its own mutable root when its result holds a `*gorm.DB` and the callee is not known to return an immutable value (#176).

| Call | Root | Immutable when |
| --- | --- | --- |
| `q := helper(db)`, user function without `immutable-return` | The call | Its body proves an immutable result ([Inferred immutable-return](#inferred-immutable-return)) |
| `q, err := helper(db)`, tuple result | The call, reached through the `Extract`. For a trusted pure callee, the returned argument's root (#188) | The callee is `immutable-return`, declared or inferred, or is pure and returns only immutable arguments |
| `q := f(db)`, `f` a func-typed parameter, field or merged func value | The call | Never |
| `q := r.Query()`, interface method | The call | Never |

<details><summary>Decisions</summary>

- **A trusted pure function's tuple result is an alias of its argument** when its body may return that argument, as for a single result ([Returned arguments](#returned-arguments)).
- **`immutable-return` applies to a tuple result.** The signature check already accepts any result list with a `*gorm.DB`. `ImmutableReturnValidator.Validate` already checks every `*gorm.DB` operand of each `return`. So `//gormreuse:immutable-return` on `func open() (*gorm.DB, error)` makes its `*gorm.DB` immutable at call sites, and returning `db.Where(...)` from it is reported at the declaration.
- **The tuple call is the root**, not the `Extract`. An `Extract` has no source position, so the message could not say `root at file:line`. This matches what `traceCall` already did for a closure with a stored tuple result.
- **A dynamic or interface callee is mutable.** Nothing says what it returns. A repository interface whose method returns a fresh handle, such as `r.DB()`, is reported when its result is branched twice. Write `r.DB().Session(&gorm.Session{})`, or `//gormreuse:ignore`. No directive applies to an interface method, so `immutable-return` on the implementation does not help a call through the interface.
- **`q, err = helper(q)` is a reassignment**, like `q = helper(q)`. `handleArgs` accepts a tuple result, and `isAssignment` follows the `*gorm.DB` `Extract` to the merge.
- **No suggested fix for a tuple root.** `-fix` appends `Session()` to the root's call, and `helper(db).Session(...)` does not compile when `helper` returns two values. `Generate` returns no fix when the root is not a `*gorm.DB`.

</details>

> [!WARNING]
> Every `*gorm.DB` element of one tuple shares the call as its root. `a, b := pair(db)` followed by `a.Find(nil); b.Find(nil)` is reported as a second branch. This errs on the side of reporting (`opaqueTuplePairShared` in `testdata/src/gormreuse/opaque_results.go`).

<details><summary>Before #176</summary>

`traceCall` returned `nil` (immutable) for a user function without `immutable-return` and for a tuple result. That went against the conservative principle.

</details>

### Closure results

A call of a function literal is traced into its returns (`traceClosureReturns`), like an IIFE. Two rules decide when the call is not simply the closure's returned root:

- A closure whose `*gorm.DB` returns all have no root gives the call no root (`closureReturnsImmutable`, #308).
- A factory that may run more than once makes each call its own root (`closureCallOwnsRoots`, #251).

<details><summary>The closure shapes</summary>

<table>
<tr><th>Closure</th><th>Call</th></tr>
<tr><td>

Returns a captured value with no root, such as a `Transaction` callback's `tx` (#308).

```go
get := func() *gorm.DB { return conn }
```

</td><td>

No root. `closureReturnsImmutable` searches each return with `FindAllMutableRoots`. The closure returns no mutable value when it has a `*gorm.DB` return and none of them has a root. Then `traceCall` and `traceAll` give the call no root.

</td></tr>
<tr><td>

A factory that may run more than once (#251).

```go
base := func() *gorm.DB {
    return db.Model(&User{})
}
base().Find(&a); base().Find(&b)
```

</td><td>

Each call is its own root (`closureCallOwnsRoots`). Each call builds a new `Statement`.

</td></tr>
<tr><td>

Returns a captured value with roots, or a parameter. Returns one of several roots. Has no return, or only panics.

</td><td>

Its own root, or the returned root for an IIFE chain.

</td></tr>
</table>

</details>

**Factory rule (#251).** `closureCallOwnsRoots` makes the call its own root when both hold:

- The closure may run more than once for one run of its `MakeClosure`, as `cfg.Repeats` counts it (#249). That means at two sites that may both run, or at a site in a loop that does not create it. `closureRunsRepeatedly` asks this of `RepeatsOf`, on the tracer's own `cfg.Analyzer`. A closure called once in a loop is already safe without this rule, since `RepeatsAfter` places a root inside it at its one call site. Two calls are not.
- Each root is a gorm method call in the closure whose result is only returned. Every `*gorm.DB` operand of the chain it ends is immutable or another such call in the closure.

<details><summary>Shared chains, decisions and the growth test</summary>

A chain from a captured mutable root, a closure parameter or a call of another function is shared by every run. It keeps its root and stays reported (`cfFactoryCapturedRoot`, `cfFactoryParam`, `cfFactoryHelper`). So does a returned value that the body also uses (`cfFactoryBodyUse`). The rule does not count the runs itself, so the checker, the fixes and the tracer agree on when a closure runs again.

Decisions for `closureReturnsImmutable` (#308):

- **A search of its own.** The single-root trace followed only the first store into a variable before #340. So a nil from it did not prove that there is no root (`iwClosureCapturedReassigned`). `traceAll` shares its `visited` set with the rest of the search, and a value it has visited traces to nothing there, whatever its roots. So each return is searched afresh, and the answer is kept per closure.
- **Recursion.** A closure being checked counts as returning a mutable value. A recursive closure ends, and its calls stay roots.
- **Inference does the rest.** Most such closures are also [inferred](#inferred-immutable-return) `immutable-return` since #308. That covers calls the tracer reads through `returnsImmutable`. The search here also covers values that no body proves immutable, such as an exempt callback parameter.

`TestAllocationGrowth` (`closuregetters`) holds a chain of `n` closures, each returning the call of the one before it, to linear growth.

</details>

<details><summary>Before #308 and #251</summary>

- `traceClosureReturns` gave a nil root both when no return had a root and when the returns had several roots. `traceCall` then made the call a mutable root in both cases. So with `conn` immutable, `get := func() *gorm.DB { return conn }` made `d := get(); d.Find(nil); d.Count(nil)` a report.
- Every call of a factory shared the root inside its body. Calling it twice, or once in a loop, reported the second query.

</details>

### Stores: which store a load sees

A load from memory (a field, a global, a captured variable) is matched to the stores that reach it, with a CFG walk (#182). The tracer keeps no SSA value for a field, so it matches stores by **(root address, field index path)**.

<details><summary>The three store cases, with SSA</summary>

<table>
<tr><th>Code</th><th>What the load sees</th></tr>
<tr><td>

```go
h := outer{db: q} // staged in a `local outer (complit)`
                   // temporary, copied whole into h
h.db.Find(nil)  // a FRESH FieldAddr chain: &(&h.embedded).db
h.db.Count(nil) // another one, sharing nothing with the store
```

</td><td>

The store of `q`. Whole-aggregate copies into any prefix of the path are followed back to the address they were loaded from. Matching the immediate base value instead would miss every nested or promoted field.

</td></tr>
<tr><td>

```go
r.db = r.db.Where("t")
// t1 = *(&r.db); t2 = Where(t1); *(&r.db) = t2
r.db.Find(nil) // sees *(&r.db) = t2 only
```

</td><td>

Only the stores that reach it (`fieldStores`). That is the last store to the field in its own block before it. Otherwise it is the last store in each predecessor block, found by walking the CFG backward. A store ends the walk on its path, so a later store replaces an earlier one.

</td></tr>
<tr><td>

```go
for _, c := range conds {
    r.db = r.db.Where(c) // sees this store
}                        // from the previous iteration
r.db.Find(nil)
```

</td><td>

In a loop the call is still a root of its own receiver. `isLoopFieldRebuild` treats it as an assignment, like the loop-header Phi of `q = q.Where(c)`. The tracker also records it as a loop-carried use. A polluting use of the call's result that can reach the call (`r.db.Find(nil)` later in the same body) is reported at the call, whatever the positions.

</td></tr>
</table>

</details>

Decisions and limitations (#182):

- The Scopes callback resolver still reads every store to a field, since a larger callback set only adds mutable roots.
- Only stores to the same (root address, field path) count. A write through another pointer (`p := &r.db; *p = x`) or inside a callee neither reaches a load nor replaces a store.
- The loop-carried check covers fields and globals only. The local form `for … { q = q.Where(c); q.Find(nil) }` is not reported (`fsoLocalLoopCarried`, see [Known Limitations](#known-limitations)).
- Outside a loop the rebuild is not an assignment: `r.db = q; r.db = r.db.Where("y"); q.Find(nil)` is reported. Inside a loop, as with locals, the value read on the first iteration is not counted as branched.

<details><summary>Before #182</summary>

A load saw every store in the function. The right-hand load of `r.db = r.db.Where("t")` resolved to the stored result (t2 above), and `Where("t")` counted as the first branch of its own result.

</details>

#### Captured variables

A variable that a closure captures and that is reassigned lives in an `Alloc`, with no Phi (#251). `allocReachingStoreValues` matches its stores with the same CFG-ordered search (`observedFieldStores` with an empty path):

| Read | Stores it sees |
| --- | --- |
| In the function that declares the variable | The stores that reach the read |
| In a closure declared there whose only uses are plain calls (`f()`, an IIFE) | The stores that reach any of those calls |
| In a range-over-func body, or in a closure nested only in such bodies (#341) | The stores that reach the body's `MakeClosure` in the declaring function (`missesLaterStores`) |
| In any other closure: deferred, spawned, stored, passed on (errgroup's `Go`, a `Transaction` callback), or nested in another closure (#303) | The stores that reach the `MakeClosure` in the declaring function (the outermost one, for a nested closure), plus every store that can run after it (`fieldStoresAfter`) |

Every search also sees the stores of a range-over-func body, as stores at its `MakeClosure` (`yieldStores`, #341). Such a store does not end the backward walk, since the body may run no time. The stores of a loop nested in the body count, where its `MakeClosure` is. A read in a body does not see the body's own stores. The receiver of `q = q.Where(v)` in the body resolves to the value before the loop. `isAssignment` reads the store to the `FreeVar` as the reassignment.

<details><summary>Why the MakeClosure, and fields through a captured pointer</summary>

**Why the `MakeClosure` (#303).** A closure cannot run before it is created. Once created it may run at any later point, also after the function returns. So the value it reads is the one held at its creation or one stored later. A store after the `MakeClosure`, or later in a loop that creates it, is still seen (`cosStoreAfterSpawn`, `cosStoreInLoop`). The rule is not tighter for a deferred closure, such as the stores that reach the function's exits. A deferred closure can also be stored or passed on before it runs.

**Fields through a captured pointer (#303).** A field read through a pointer variable that a closure captures, such as `d := &deps{db: db}`, follows the same rule. `fieldStoredValuesAt` passes the load to `observedFieldStores` also when the address is defined in an enclosing function. A captured pointer variable lives in an `Alloc`, so `d.db = ...` stores through a fresh load of it, not through the pointer the variable holds. `storeTarget` matches these stores to that pointer when it is the only value ever stored into the variable. With more than one store in the variable, a load of it may hold another pointer, so a store through it is not matched.

</details>

Limitations:

- The single-root trace takes the first store, in block order, that has a root, as `tracePhi` does for the edges of a Phi (`traceStores`, #340).
- A store made inside a closure other than a range-over-func body is not seen (`closureModifiesCaptured`). This also fixes `p := &q; *p = db.Where("y")`, which is a direct store to `q`'s `Alloc` (`reassignPointerDeref`).
- The fix's `mergedEdges` still gives a Session to a store that may run before two reads of the variable. A store whose own reads need none can get a redundant one.
- `mergedEdges` does not see the stores of a range-over-func body. Take a mutable value before the loop, as in `q := db.Model(&User{}); for k, v := range seq { q = q.Where(k, v) }; q.Count(&n); q.Find(&us)`. Then `-fix` adds a Session only at `Model` (`rangeFuncBodyStoreAfterIf`). The next run then reports the `Where` in the body, and its fix ends it with a Session. When the value before the loop is immutable, as in #341's `WithContext`, one run fixes it (`RangeFuncFilter` in `e2e/internal/fixsql`).

<details><summary>History (#251, #303, #340)</summary>

- Before #340 the single-root trace took the first store. An immutable one, such as `q := db.WithContext(ctx)` before `if name != "" { q = q.Where(...) }`, then hid the mutable value of the other. A captured `q`, a struct field or a global was not reported where a plain local was.
- Before #251 every read of a captured, reassigned variable resolved to its first store, often the parameter it was initialized with. So `db = db.WithContext(ctx); db.Find(nil); func() { db.Count(nil) }()` reported two branches of the parameter, and `-fix` added a redundant Session.
- Before #303 a closure that is not a plain call saw every store. `db = db.WithContext(ctx)` followed by `g.Go(func() error { return db.First(&u, id).Error })` in a loop resolved the read to the parameter and reported it. So did `for _, u := range slices.All(us) { db.Model(&u).Update(...) }`.
- Before #303 `fieldStoredValuesAt` passed a nil load for a captured pointer, so every store was seen. Stores through the variable were missed, in the declaring function too. `d.db = d.db.WithContext(ctx)` did not replace the parameter, and a store of a mutable value after the closure was not seen (`cosFieldStoreAfterSpawn`).

</details>

#### Store search cost

The tracer keeps these searches in a `storeMemo`, per address, field path and function (#325):

<details><summary>The memo</summary>

| Kept | Per | Before #325 |
| --- | --- | --- |
| The stores that reach the start of a block (`fieldStoresInto`) | Block | A backward walk for every read. Only the part of the read's block before it is still scanned per read |
| What the loads in a closure see (`observedFieldStores`) | Closure | Every load walked back from every call site of the closure. A closure called at `n` sites with `n/10` loads took 9 s at `n = 800` |
| The stores of a range-over-func body (`yieldStores`, #341) | Body | Not seen before #341. Without the memo, a walk back from the last of `n` loops scanned the `n - 1` bodies before it again |

The results are the same as the walks', so no report changes. A closure with one read returns that read's stores, which are in program order already. Otherwise `sortStoresInProgramOrder` merges the reads, and places a body's stores at its `MakeClosure`. It finds each `MakeClosure` with one scan of the function. Looking it up for every comparison scanned the function once per body, which made `n` loops cubic (`rangeloops` in the growth gate). `FieldStoredValues`, which the callback resolver calls with a nil load, keeps nothing (a nil `storeMemo`).

</details>

### Struct roots

Reuse through a struct that holds a `*gorm.DB` is followed through four shapes (#181, #250).

| Shape | SSA | Handling |
| --- | --- | --- |
| Promoted methods of an embedded `*gorm.DB`: `e := E{q}; e.Where("a")` | `t4 = *t0; t5 = t4.DB`: a `Field` of a struct in a register, not a `FieldAddr` | `fieldValueStoredValues` turns the `Field` chain over the load into (loaded address, field path), and matches stores as for a `FieldAddr` chain. Over a Phi, each edge is resolved the same way |
| A field of a returned struct: `s := mk(db); s.DB.Where("a")` | `*t0 = mk(db)`, or `mk(db).DB` directly | The field is a `FieldRoot{call, path}` (below) |
| A field of a returned pointer (#250): `p := mkPtr(db); p.DB.Where("a")` | `t1 = &t0.DB; *t1`, a `FieldAddr` chain on the call | `reachingFieldStores` matches stores through `t0` as for a local. When some path from the call reaches the load with no store, the field is also the `FieldRoot{call, path}` |
| A struct captured by a closure: `f := func() { h.q.Where("b") }` | `&h.q` on a `FreeVar`. With `h := &T{...}`, `&(*h).q` on a fresh load of the captured pointer variable | `ClosureCapturesGormDB` looks through struct fields (`closureHoldsGormDB`), so the closure is analyzed. `fieldBaseAddrs` maps a `FreeVar` to its binding, and a load of a pointer variable to each pointer stored in it. The stores are searched in the function that defines that address |

**`FieldRoot`.** A call returning `*gorm.DB` is a mutable root. A call returning a struct that holds one has no SSA value for "field `DB` of this call", and every read is a new instruction. So the tracer makes a `FieldRoot` that implements `ssa.Value`. It interns one per (call, field path) in `RootTracer.fieldRoots`. Reads of the same field of the same call share it. Another field or another call is a different root. Its position is the call's. A call to a builtin or a `//gormreuse:immutable-return` function yields no root.

<details><summary>One rule for value and pointer results</summary>

**One rule for value and pointer results (#250).** A field of a struct returned by value is a root. So is the same field reached through a returned pointer:

- The rule then matches "a call returning `*gorm.DB` is a mutable root". A helper such as `newTestApp(t)` that returns `*gorm.DB`, a struct or a pointer to a struct is treated the same. In each case `//gormreuse:immutable-return` on the helper marks its handles as safe.
- A returned pointer is not like a parameter. The caller usually gets a struct the callee has just built, so a mutable chain in it is as likely as in a returned value. The parameter trade-off stays because a parameter is most often the application's shared handle.
- Making the value case match the parameter case instead would drop the #181 detection and its E2E evidence.

The pointer walk stops at the call (`reachingFieldStores`): a store above it on a loop path wrote the struct of an earlier iteration. Only a `FieldAddr` chain directly on the call is followed. A pointer field inside the result (`p.inner.DB` with `inner *T`) is not.

</details>

#### Defining block

The loop check reports a use in a loop of a root defined outside it. `tracer.RootBlock` gives each root kind its defining block, and `cfg.Repeats.RepeatsAfter` takes that block with the use's block (#250). The suggested fixes ask the same query.

<details><summary>The block of each root kind</summary>

| Root kind | Defining block |
| --- | --- |
| A call, a Phi (instructions) | Its own block |
| `FieldRoot` | The block of its call (`FieldRoot.Block`) |
| `PointeeRoot` | The block of its call (`PointeeRoot.Block`, [Addresses of variables](#addresses-of-variables)) |
| `*ssa.Parameter` | The entry block of its function. That block is in a loop only for a range-over-func body, whose parameters are the values of the current iteration. So `for q := range seq { q.Find(nil) }` is not reported |
| Anything else | None, so outside. The tracer returns no other kind: it follows a `FreeVar` to its binding and a global to its stores, and a constant is no root |

A root defined in another function, such as an enclosing function's root used in a closure, has a block that is not in the closure's loops. It is outside, and is placed as [Uses that run more than once](#uses-that-run-more-than-once) describes.

</details>

Decisions and limitations:

- No fix is suggested for a `FieldRoot`. `Session()` cannot be appended to a call that returns a struct. It belongs where the function builds the struct.
- A field read through a parameter or a receiver is not a mutable root, whether the struct is passed by value or by pointer. A getter that returns such a field is inferred `immutable-return` (#308), so `d := r.DB()` is read like `r.db`. Each call reads the field of its own receiver (#342), so `h := &Holder{db: mk(db)}; h.DB()` has `mk(db)`'s root ([Parameter reads](#parameter-reads)).
- A write to a returned struct inside another function (`p.Reset()`, `reset(&s)`) is not seen. A later read still sees the call's field, so it can be reported though the field was replaced. A closure capturing such a parameter sees only the stores in the enclosing function, as the function itself does.
- A captured pointer variable is resolved by its stores in program order: the single-root trace takes the first pointer stored into it. After `h = &T{q: b}`, a use of `h.q` can still be matched to the first value. A store through `h` is not matched to either pointer. A captured `*gorm.DB` variable is read by the stores that reach the read instead ([Captured variables](#captured-variables)). With one pointer ever stored, the fields are read the same way (#303).
- A struct value that a Phi merges (`e := E{a}; if c { e = E{b} }`) yields the field of every edge. The single-root trace takes the first edge, so the suggested fix makes only that edge immutable. A struct from a channel receive, or a map or slice element, is not followed.
- A `Field` read over a load uses the CFG-ordered store matching of #182 (`fieldStores`). So `e.DB = other` between two promoted calls is not a second branch. A read inside a closure has no CFG path to the stores in the enclosing function. So it matches all of them, and the single-root trace takes the first. This keeps reuse reported when the closure may run before a later store.

<details><summary>Before #181 and #250</summary>

- The three #181 shapes (promoted methods, returned struct, captured struct) lost the root, so reuse through them was missed.
- Before #250 a field of a struct returned by value was a root, and the same field reached through a returned pointer was not.
- The loop check used to treat every value that is not an instruction as outside. A `FieldRoot` from a call inside the loop was then reported on every iteration.

</details>

### Addresses of variables

A variable whose address is taken lives in an `Alloc`. A call such as `applyP(&q)` gets the `Alloc` itself as its argument, of type `**gorm.DB`, with no load of `q` (#345). The callee may change `*p` in place, or store another chain in it:

```go
func applyP(p **gorm.DB)               { (*p).Where("t = 1") }
func scanInto(p **gorm.DB, db *gorm.DB) { *p = db.Where("x = 1") }
```

So the call branches the value `q` holds, and a later read of `q` also sees what the call may store.

| Part | Handling |
| --- | --- |
| The call | `handleArgs` asks `RootTracer.PointeeRoots` for the roots of the value `q` holds where the call runs. These are the stores that reach the call, as a load there would see them. The call branches them with `Escapes` set, as `applyTenant(q)` does. A `//gormreuse:pure` callee returns before this, so it changes nothing |
| A later read of `q` | The read sees the stores that reach it. It also sees a `PointeeRoot{call, var}` for each call that takes the address and reaches the read with no store in between (`pointeeRootsAt`). The call may leave `q` as it was, so it does not hide the stores before it. The `PointeeRoot`s come after the stores, so the single-root trace takes a store first |
| A read in a closure | It sees every such call, as a nil load sees every store. This keeps reuse reported when the closure may run after the call |
| The fix | No fix is suggested for a `PointeeRoot`, as for a `FieldRoot`: the call returns no `*gorm.DB` to add `Session()` to. A stored root keeps its fix only when nothing hands the address on ([Struct stores](#struct-stores)) |

The calls that take each variable's address, and the calls that reach the start of each block, are kept in `RootTracer.pointees`. The reads of one variable share them (`pointerescape` in the growth gate).

Limitations:

- Only the address of a variable is followed. The address of a struct field (`applyP(&s.db)`) is not a branch, so a single later use is missed (`fixEscapePointerFieldAddr`). The fix is withheld for it: the struct is handed on.
- An address wrapped in an interface (`json.Unmarshal(b, &q)`) is not followed. Detection treats it as any other call: the read after it sees the stores before it.

<details><summary>Before #345</summary>

Neither was seen. `applyP(&q); q.Find(&a)` was not reported, and `-fix` added a Session at `q` that made the helper change a copy. `var q *gorm.DB; scanInto(&q, db); q.Find(&a); q.Count(&n)` was not reported at all: no store into `q` reaches the reads.

</details>

### Reads from containers

A `*gorm.DB` read from a container is a `tracer.ElemRoot` (#374). The reads are an element of a slice or an array (through `IndexAddr` or `Index`, also in a range loop), a value of a map (`Lookup`, also comma-ok, and the values of a range loop), a value received from a channel (`<-ch`, also comma-ok and in a range loop), and what a pointer parameter points to (`*pq` for `pq **gorm.DB`).

Storing a value into a container is already a branch of it ([Pollution sources](#pollution-sources)). So a read is not traced to what was stored: with the store, every read would be a second branch. A read is a root of its own instead:

| Read | Root |
| --- | --- |
| Two reads of one element: `s[0]` twice, `m["k"]` twice, `*pq` twice | One `ElemRoot`, interned by (container, key). A constant key is compared by value |
| An element written after a read, as in `s[0] = s[0].Where("b")` or `*pq = (*pq).Where("t")` | A root per read. A write before every read, such as the one that fills a composite literal, does not count (`elemWrittenBetween`) |
| A receive, or a step of a range loop over a map | A root per read |
| A container that holds no mutable value | No root |

`elemContainerHoldsMutable` decides the last row. It collects the values that may be the container itself (`elemContainerAliases`): what it is made from, slices of it, merges, the results of `append`, a local variable that holds it, and a closure's capture of it. The container may hold a mutable value when:

- one of them is a parameter or the result of a call, whose elements come from code not seen here;
- one of them is passed to a function that may reach gorm, which may store into it. A function in a package that does not reach gorm, such as `slices.Reverse`, makes no `*gorm.DB`, and `append`, `copy` and `len` are builtins; or
- a value stored into one of them (`containerElems`), sent on it or appended to it has a mutable root.

The answer is kept for every value of the set. The reads and writes of a container's elements are found in one pass over its referrers (`elemAccessOf`), so `n` reads of a map literal's `n` keys are linear (`mapreads` in the growth gate). No fix is offered for an `ElemRoot`: the call that made the value is not known.

Decisions:

- **A container read from a field or a package-level variable is not followed.** It gives no root, as a field read through a parameter does not (`crFieldContainer`). Most such containers are registries of connections, whose values are immutable.
- **A parameter's container is mutable**, as a `*gorm.DB` parameter is. `func run(qs []*gorm.DB) { for _, q := range qs { q.Count(&n); q.Find(&us) } }` is reported.
- **Rejected: trace a read to the values stored.** `m := map[string]*gorm.DB{"k": q}; m["k"].Find(&us)` would then be a second branch of `q`, after the store.

<details><summary>Before #374</summary>

The tracer gave every such read no root, and no root reads as immutable. So `q := m["adult"]; q.Count(&n); q.Find(&us)`, `for _, q := range qs { q.Where("a").Find(&us); q.Where("b").Find(&us) }`, `q := <-ch` and `(*pq).Find(nil); (*pq).Count(nil)` were all missed. An inferred `immutable-param` also took such an argument as immutable (#371).

</details>

### Package-level variables

A package-level variable reaches the analysis in two ways. It can be a `*gorm.DB` that a function loads and stores. It can be a `var f = func(...)` closure ([Function variables](#function-variables)).

A load `*G` of a `*gorm.DB` global is matched to the stores to `G` in the load's own function that reach it (#183). `globalStoredValues` does this with the same CFG walk as a field (`fieldStores`, #182). `isLoopFieldRebuild` treats `for … { G = G.Where(c) }` like the field form.

| Code | The load sees | Result |
| --- | --- | --- |
| `G = db.Session(s).Where("t"); G.Where("a").Find(nil); G.Where("b").Find(nil)` | The store | Second branch reported |
| `G = G.Where("t"); G.Find(nil); G.Count(nil)` | Nothing on the right, the store after it | Second branch reported |
| `G.Where("a").Find(nil); G.Where("b").Find(nil)`, no store in the function | Nothing | Immutable |

> [!IMPORTANT]
> The last row is a deliberate trade-off. A global that a function only reads usually holds a `gorm.Open` result, which is safe to branch. Treating every read of a global as mutable would report each second use of a shared handle. So the function's own stores decide, and a read that no store reaches is immutable.

The `//gormreuse:immutable-return` body check reads globals the other way. It reports a returned global only when every assignment in the package is provably mutable ([Immutable-return body contract](#immutable-return-body-contract)). Both directions give the benefit of the doubt when the value is unknown.

Limitations:

- Stores in other functions are not seen. That includes the package initializer (`var gq = g.Where("pkg")`), a closure the function calls, and the enclosing function of a closure that loads the global.
- A store to a global is not a merge point. `G = db.Where("x"); if c { G = G.Where("a") }; G.Find(nil)` is reported, because `Where("a")` counts as a branch of the stored root. A struct field behaves the same.

### Session and Debug copies

The name-based "immutable-returning" class is not exact (#174). In GORM v1.31, `Session` builds a new `*DB` with `Statement: db.Statement`. It clones the Statement only when `Context`, `PrepareStmt` or `SkipHooks` is set. `Debug` is `getInstance()` followed by `Session` with only a `Logger`. `getInstance()` returns the receiver itself when it is `clone == 0`. The E2E tests in `e2e/internal` (`TestCopyOfMidChain_*`, `TestSessionInitialized_*`) check each claim below.

| Call on a `clone == 0` receiver | Effect | Model |
| --- | --- | --- |
| `Session(&gorm.Session{})`, `Debug()`, `Session` with only other options | The copy points at the receiver's Statement. Each use of the copy forks its own Statement (`clone == 2`), so it never changes the receiver. But it reads the receiver's Statement as it is at that moment | A pure use of the receiver's root at the call. Each later use of the copy is an **alias use** of that root |
| `WithContext`, `Session` with `Context`, `PrepareStmt`, `SkipHooks` | Statement cloned at the call | Pure use only |
| `Session` with `NewDB` | `clone == 1`: each use starts a new Statement | Pure use only |
| `Session` with `Initialized` | Returns `tx.getInstance()`: a `clone == 0` handle with its own copy | Pure use of the receiver. The result is a **new mutable root** |

<details><summary>Examples</summary>

<table>
<tr><th>Code</th><th>Verdict</th></tr>
<tr><td>

```go
s := q.Session(&gorm.Session{})
q = q.Where("late") // changes q's Statement in place
s.Find(nil)         // reads "late"
```

</td><td>

Reported at `s.Find`: an alias use after its root changed.

</td></tr>
<tr><td>

```go
q.Session(&gorm.Session{}).Count(nil)
q.Session(&gorm.Session{}).Find(nil)
```

</td><td>

Not reported. Each copy reads `q`, and nothing changed it in between (`TestSessionBeforeFinisher`).

</td></tr>
</table>

</details>

<details><summary>How the Session config is read</summary>

**The config.** `callsite.Site.SessionEffect` decides the row from the `&gorm.Session{...}` literal. In SSA the literal is an `Alloc` with one `FieldAddr`+`Store` per set field.

- Any other use of the `Alloc`, a store in another block than the `Alloc`, or a non-constant bool makes the field unknown.
- An unknown config is assumed to both share and return a mutable value.
- A `Context` set to any non-constant value counts as set, matching the existing trust in `WithContext(ctx)`.

</details>

**The alias use.** `RootTracer.FindAliasedRoots` finds the copies a value reaches through `FindAllMutableRoots`, so Phi, variables and closure captures work. It returns each copy's receiver root, following copies of copies. A receiver that may hold several roots gives each of them, through `MutableRoots` (`aliasedRootsOf`, #341). So does a read-only argument. After `src := a; if c { src = b }`, a copy of `src` reads `b`'s Statement as much as `a`'s. With only the first root (`FindMutableRoot`), whether `b.Where("x").Find(&x); cp.Find(&y)` is reported would depend on the order of the Phi's edges. An immutable value that the merge may hold, such as a `Session` result, has no Statement to change and is left out. The call handler records an alias use for a gorm method receiver, a bound-method receiver, and a `*gorm.DB` argument of a non-gorm function.

`DetectViolations` reports an alias use when a polluting **or assignment** use of the root lies between the copy and the use, and reaches the use. Assignment uses count here because `q = q.Where(...)` changes a `clone == 0` Statement in place. A change before the copy is not counted. It already makes the `Session`/`Debug` call itself a second branch, so counting it again would duplicate that report.

Alias violations carry no suggested fix. The reuse-fix strategies rewrite the root's branches. The right fix depends on intent: `.Session(&gorm.Session{})` at the end of the root's chain, or an isolating copy.

Limitations:

- A `go`/`defer` use of a copy is an alias use at the statement's position. A change after the statement is missed, although a deferred or spawned call can run after it (`siteDeferSessionCopyBranchAfterDefer`).
- A copy returned from a function, or stored in a struct field and read in another function, is not followed.
- A `//gormreuse:pure` function that returns a copy of its argument hides the sharing from its caller. The copy is not the argument itself, so it is not in `PureVerdicts.Returned` ([Returned arguments](#returned-arguments)). Treating it as the argument would count each use of the copy as a branch, which the rejected approach below shows is wrong.
- The fix generator's Phi-edge check treats `Session(&gorm.Session{Initialized: true})` as already immutable. The `//gormreuse:immutable-return` body check reports it ([Immutable-return body contract](#immutable-return-body-contract)).

<details><summary>Rejected: count the copy as a branch of the receiver</summary>

The issue suggested this. It would also report `q.Session(&gorm.Session{}).Count(); q.Session(&gorm.Session{}).Find()`, and "copy used, then the original changed". Both are safe at runtime (`TestSessionBeforeFinisher`, `TestCopyOfMidChain_UseDoesNotChangeOriginal`). The alias-use model reports only the order that leaks.

</details>

### Subquery and condition arguments

A `*gorm.DB` passed as an argument of a query method is only read (#185). GORM v1.31 never changes such an argument. `e2e/internal/subquery_test.go` checks each claim below.

| Shape | When GORM reads the argument | How |
| --- | --- | --- |
| Subquery bound to `?` (`Where`, `Or`, `Not`, `Having`, `Table`, `Select`, `Joins` with SQL, `Find` conditions, `Update` value) | When the outer query builds its SQL: at its finisher | `Statement.AddVar` runs `v.Session(&Session{DryRun: true}).getInstance()`, a cloned Statement, or copies its `Statement.SQL` for a `Raw` argument |
| `Raw`, `Exec` | At the call | The SQL is built at once |
| Group condition `Where(sub)`, `Or(sub)`, `Not(sub)` | At the call | `BuildCondition` copies the `WHERE` expressions. It also runs `sub`'s pending `Scopes`. Running them on a `clone == 0` value changes it in place, but a second run finds them already applied, so the result is the same |
| `Joins("Company", cond)` | At the call | `joins` copies `Selects`, `Omits` and the `WHERE` clause |

<details><summary>Which parameters only read</summary>

**Which parameters.** `pollutionsource.readOnlyArgMethods` lists the methods and the first parameter that only reads. Every later parameter reads too.

- `Find`/`First`/`Last`/`Take`/`Delete`/`FirstOrInit`/`FirstOrCreate` take a destination or model first, which GORM writes to or parses. That parameter is not in the list.
- `FirstOrInit` and `FirstOrCreate` pass their conditions to `Find` (#253).
- `Model`, `Order`, `Clauses`, `Assign` and `Attrs` are not in the list.
- A call through a method value (`where := db.Where; where("id IN (?)", sub)`) is read as the method call it stands for. `collectAliasedRoots` resolves it with `callsite.Site`, and `pollutionsource.CallReadOnlyArgs` does the same for the handler and `Leak` (#178).

</details>

**The model**, as the issue proposed: an argument is a read at some point between the call and the end of the outer chain.

| Time | Rule |
| --- | --- |
| At the call | A pure use of its root (`siteUse.read`), like a `Session` call. A branch or a finisher of the root that reaches the call is reported as a second branch. A finisher counts too. It leaves `Dest`, `Model` (set from `Dest` when empty) and the parsed schema on a `clone == 0` Statement, so the read can differ. The issue listed `s.Count(&n)` before a subquery of `s` as a false positive. It stays reported |
| Later | Every use of the outer chain reads it again. `FindAliasedRoots` returns the argument's root for each use of a value whose chain holds such a call, with the call as `Copy` and `Argument` set. `DetectViolations` then treats it as a `Session`/`Debug` alias use. It reports a polluting or assignment use of the root after the call and before the use, that reaches the use |

<details><summary>The chain walk, the varargs array and wrappers</summary>

The later check walks every gorm call of the chain, through `Session`/`WithContext` too, since a cloned Statement still holds the argument. A `Session`/`Debug` copy below another chain call is not an alias there: that call forked its own Statement. A subquery of a subquery is followed.

**The varargs array** is not a leak when its every slice goes only to read-only parameters (`pollutionsource.isReadOnlyArray`). `Leak` applies this to the main handler and to the `pure` validator alike. A `[]any{sub}` literal spread into `Where` is read. One that is also passed elsewhere still leaks.

**Wrappers (#253).** A subquery inside these is read like a bare one: `gorm.Expr(sql, sub)`, `sql.Named(name, sub)`, a local map literal, or a local `clause.Expr` literal (or a pointer to one). GORM binds it with `Statement.AddVar` when the outer query builds its SQL (`e2e/internal/subquery_wrapper_test.go`).

- `isReadOnlyValue` accepts a wrapper only when it is used, and its every use is a read-only argument or another such wrapper. Stores into the wrapper itself are allowed.
- Then `Leak` skips the store into it, and the handler and the `pure` validator skip the `sql.Named` call (`IsReadOnlyWrapperCall`).
- `ReadOnlyGormArgs` returns the subquery inside (`wrappedGormDBs`), so the "changed before the outer query runs" check applies too.
- `pollutionsource.wrapperFuncs` lists the wrapper functions.
- Any other use of a wrapper, such as a field read, a map lookup or a call of another function, keeps it a leak.
- Only these wrappers are known. A user function that wraps its argument the same way is still a call of a function that is not `pure`.

</details>

Decisions and limitations:

- **Eager reads are treated as lazy.** `Raw`, `Exec`, group conditions and `Joins("Company", cond)` read at the call. A change after the call does not reach them. They are still reported, as for a subquery. Telling them apart would need per-method timing for little gain, and the report is a false positive only for this order.
- **One report per argument.** Each call of `x.Order("id").Find(nil)` is a use of `x`. A changed argument is reported once per call that took it, at the earliest use that sees the change. `Session`/`Debug` alias uses keep one report per use.
- **No suggested fix** for the later-change report, as for alias uses. A `.Session(&gorm.Session{})` at the end of the argument's chain fixes both reports.
- **Not followed:** an outer chain returned from a function, or stored in a field and used elsewhere, as for `Session`/`Debug` copies. An outer chain used in `go`/`defer` is an alias use at the statement's position, as for a copy. A `//gormreuse:pure` function may take a `*gorm.DB` argument and pass it to `Where`, and its caller does not see that the result holds it.

<details><summary>Before #185</summary>

A `*gorm.DB` passed as an argument of a query method counted as a branch, through the varargs slice store or the interface conversion. So `db.Where("id IN (?)", sub)` twice was reported.

</details>

### Tracer cost

Every gorm call asks `FindAliasedRoots` about its receiver, and each step down the chain asks `FindAllMutableRoots` again (#260). With `n` conditional reassignments `if c { q = q.Where(...) }` that was cubic in `n` (about 10 s at `n = 400`). Memos on the `RootTracer` keep it near quadratic. None of them changes a report.

<details><summary>The memos</summary>

| Memo | Rule |
| --- | --- |
| `FindAllMutableRoots` result | Kept per value and `LoopInfo`. The result depends on nothing else once the pure verdicts are final, which is before any `RootTracer` is made. A `LoopInfo` with no loop header keys as `nil` (`rootsLoopInfo`, #300) |
| `mayAlias` | Kept per value and `copies` flag: whether `collectAliasedRoots` can add a root from there. A search that cannot is skipped |
| `mayAlias` least fixed point (#300) | `mayAliasVisit` finds the cycles of the search graph with Tarjan's algorithm, and answers each one when it is complete |
| One tracer per pass (#325) | `run.go` shares one `RootTracer` across every function |
| `HasMutableRoot` | A list of 16 or more roots is kept as a set |
| `FindMutableRoot` result (#350) | Kept per value and `LoopInfo`, for the same reason as `FindAllMutableRoots`. The handlers, the alias search and the fixes ask for the root of the same receivers |
| `mayAliasStep` (#350) | Kept per candidate root: whether the step through it may add a root. `n` spawned reads of a field reassigned `n` times share their `n` candidates. An answer that relies on a search still on the Tarjan stack is kept only when it is true |

`TestAllocationGrowth` in `growth_test.go` holds these shapes (`filters`, `sessfilters`, `purechain`) to quadratic growth, and `loops` to near-linear growth in allocations and quadratic growth in steps ([Allocation-growth gate](#allocation-growth-gate)).

</details>

<details><summary>Why each memo is safe, and the measurements</summary>

- **`LoopInfo` key.** The tracer reads a `LoopInfo` only through `IsLoopHeader`. With every answer false, each reader does what it does for `nil`. So every function without loops, such as most closures, shares one entry per value instead of tracing a parent's value once per closure. A `LoopInfo` with headers keeps its own entry, since a trace from `v` can reach that function's blocks, for example through a closure that `v` calls.
- **`mayAlias` skip.** Every call the skipped search would visit adds nothing either, so what is added, and in what order, stays the same.
- **`mayAlias` cycles (#300).** The searches form a graph, and a loop makes a cycle in it. Every search on a cycle reaches every other one, so they all get the same answer. A cycle that adds nothing is answered false and skipped too. Before #300, a search that reached itself was assumed to add a root, so a search inside a loop was never skipped. `n` sequential loops that rebuild one query then took time cubic in `n` (5.6 s at `n = 800`).
- **One tracer per pass (#325).** Each closure is also a function of the package and is analysed on its own. That analysis traces the values of the function that declares it again. Take `n` spawned closures that each read a query reassigned `n` times. With a tracer per function, they traced the parent's `n` loads, each with up to `n` roots, once per closure. That was cubic: 85 s at `n = 800`. The memos depend only on the values asked about, and the sets the tracer holds are final before it is made. Sharing them changes no report.
- **`HasMutableRoot`.** It answers `isAssignment`'s membership test. After `n` such loops the receiver has `n` roots, and `isAssignment` asks about each of them, so a linear search was cubic too. The allocation gate cannot see this, since the search allocates nothing, so `TestHasMutableRoot` checks that a long list is kept as a set.
- **The `loops` shape** also copied its roots once per Phi in `traceAllPhiEdges`, and `pollution.sortedRoots` built the names of its Phi roots once per comparison. Both, and the `cfg.Analyzer.reachSet` queue, are now done once, which keeps the shape linear in allocations. It is still quadratic in time (0.8 s at `n = 1600`). The receiver of each of the `n` calls has up to `n` roots, and each Phi keeps its own list.

**Rejected: one shared `visited` set for the whole search.** It visits each value once. But through a loop it visits roots in a different order, and the order of alias uses picks the message when two reports share a position.

</details>

## Uses and pollution

The handler turns each instruction into uses of roots. `pollution.Tracker` records them, and `DetectViolations` reports a branch that another branch of the same root reaches.

| Use | Recorded by | Meaning |
| --- | --- | --- |
| Branch | The receiver of a chain method, an argument of a function that is not `pure` | Pollutes the root. A second branch that one reaches is a violation |
| Read (`siteUse.read`) | `Session`-like calls, read-only arguments | A branch that reaches it is reported |
| Assignment (`RecordAssignment`) | A call whose result reassigns the variable through a merge | Does not pollute |
| Alias use | A use of a Session/Debug copy, or of a chain that holds a subquery | Reported when the root changed in between |
| May branch (`Tracker.RecordMayBranch`) | A use of a value that may hold several roots | Checked against the root's own uses |
| Branch use | A `go` or `defer` branch | Seen by a later `go` or `defer`, skipped by position-ordered detection |

### Methods that do not branch

`typeutil.receiverUses` is the one table of methods whose call does not branch its receiver (#307). GORM v1.31.2 does the following for each of them on a `clone == 0` receiver. `e2e/internal/state_methods_test.go` checks each claim with sqlmock.

| Method | What GORM does | Model |
| --- | --- | --- |
| `Session`, `WithContext`, `Debug`, `Open`, `Begin`, `Transaction` | See [Detection model](#detection-model) | Read |
| `ToSQL` | Runs its callback on `db.Session(&Session{DryRun: true}).getInstance()`, a cloned Statement. The receiver is unchanged, but the SQL holds the receiver's clauses | Read: a branch before it is reported |
| `AddError` | Sets `db.Error` (and `Statement.Result.Error`). Touches no clause | State only |
| `DB` | Returns the `*sql.DB` behind the connection pool of the Statement or the `Config` | State only |
| `Get`, `InstanceGet` | Read `Statement.Settings` | State only |
| `Callback` | Returns the callback manager of the `Config` | State only |
| `Commit`, `Rollback` | Pass the error of the connection pool's `Commit` or `Rollback` (or GORM's invalid-transaction error) to `AddError`, and return the receiver itself. Touch no clause (#375) | State only, returning the receiver |

A read is `siteUse.read`, as for `Session`: a branch of the root that reaches the call is reported. A state-only call is skipped by `handleReceiver` before anything is recorded. `IsImmutableReturningBuiltin` covers both, so the `pure` validator, the immutable-return check and the fixes treat them as not branching too.

`Commit` and `Rollback` return their receiver, so `IsImmutableReturningBuiltin` leaves them out: their result is as mutable as the receiver. `typeutil.ReturnsReceiver` makes the tracer follow the result to the receiver's roots, as for a `pure` function that returns its argument. So `tx.Where("a").Find(nil); tx.Commit().Where("b").Find(nil)` is still a reuse (`stateMethodsChainAfterCommit`). The `pure` validator does not count a call of them as a branch (`IsStateOnlyMethod`, `stateMethodsPure`). `SavePoint` and `RollbackTo` stay branches. They hand the receiver to the dialector, which runs SQL on it, and `SavePoint` writes `Statement.ConnPool`.

<details><summary>Decisions, and the test that keeps the table in line</summary>

Decisions:

- **`AddError` is state only, although a later chain inherits the error.** All chain calls of a `clone == 0` value return that same `*gorm.DB`. So `q.AddError(err); q.Find(&users)` runs no SQL and returns `err` (`TestStateMethods_AddErrorIsInherited`). Stopping the chain is what `AddError` is for, and GORM's own callbacks use it that way. This linter reports clauses that one chain leaves for another. An error is outside that model, just as a failed `Find` leaves its error on the value. With the error cleared, the later query is the same (`TestStateMethods_TouchNoClause`).
- **`Get` and `InstanceGet` after a branch are not reported.** A branch can store a setting with `Set` or `InstanceSet`, and a later `Get` sees it. No query is built from that read. So it is treated as a field read like `db.Statement`, which is not a use either.
- **`Migrator` and `AutoMigrate` stay branches.** `Migrator` runs the receiver's pending scopes in place. The `Migrator` it returns holds a `Session` copy that shares the receiver's Statement. A condition added to the receiver after the call leaks into the queries the `Migrator` runs (`TestStateMethods_MigratorSharesStatement`). The copy cannot be followed through the `Migrator` interface. `Association`, `Connection`, `Row`, `Rows`, `ScanRows`, `SetupJoinTable` and `Use` stay branches too. `gormNonChainBranches` in `e2e/internal/gorm_stub_test.go` gives the reason for each.
- **Methods promoted from `*gorm.Config` are not in the table.** `Name`, `Explain`, `Apply` and the other dialector methods are calls on the embedded `Config` in SSA. They are not calls on the `*gorm.DB`. They were never uses.

`TestGormReceiverUsesMatchGorm` keeps the table in line with GORM. Every method declared on the real `*gorm.DB` that returns no `*gorm.DB` must be in `receiverUses` or in `gormNonChainBranches`. Every name in either must still be a method of it. `TestGormStateOnlyChainsMatchGorm` covers the methods that return a `*gorm.DB` (#375). It reads their source in GORM's module. A method whose body uses its receiver only to call `AddError`, to read a field, or to return it must be a state-only entry that returns the receiver, and every such entry must have such a body.

</details>

<details><summary>Before #307 and #375</summary>

Every gorm method branched its receiver unless it returns an immutable copy. So `sqlDB, _ := db.DB(); db.AutoMigrate(...)` on a parameter was reported. So was each `db.AddError(...)` after a branch in a GORM callback. In the gorm corpus (the root module of `go-gorm/gorm` at commit b3d3bf2), 83 of 149 reports were of this kind.

Before #375 `Commit` and `Rollback` were branches. So `tx.Where("id = ?", 1).Update("name", "x"); tx.Rollback(); tx.Commit()` reported both. `TestGormReceiverUsesMatchGorm` checked only the methods that return no `*gorm.DB`.

</details>

### Pollution sources

On the safe side, the linter marks a `*gorm.DB` as polluted in these cases:

| Case | Example | Note |
| --- | --- | --- |
| Interface method call | | Assumed to pollute: it cannot be analyzed statically |
| Channel send | `ch <- db` | A branch of every root the value may hold, reported when it runs again (#374) |
| Slice or array storage, `append` | `[]*gorm.DB{db}`, `qs = append(qs, db)` | The same |
| Map storage | `map[string]*gorm.DB{"k": db}` | The same |
| A method value handed on (#374) | `firstPage(q.Find)`, `job{find: q.Find}` | A branch of the receiver ([Method values](#method-values)) |
| Interface conversion | `interface{}(db)` | A type assertion may extract it. A read-only argument of a query method is the exception (#185) |
| Function argument | `helper(db)` | A branch when the callee is not trusted as pure. A `*gorm.DB` result assigned back in a merge is an assignment, as for a gorm method |
| A struct that holds it, handed to a call (#341) | `useDeps(deps{DB: q})`, `r := &repo{db: q}; r.active()` | A branch of each root that the struct's `*gorm.DB` fields hold, as for `helper(q)`. See below |
| Struct field access | `h.field.Find(nil)` | Traced back to the value stored in the field, including fields reached through embedding (`outer{db: q}`, where `db` is promoted from an embedded struct) |

> [!NOTE]
> A struct literal store alone (`_ = &S{db: q}`) does NOT pollute. The linter tracks actual use through struct fields, not the store.

**Sends and stores (#374).** A send, a store into a slice element or a map, and `append` are handled as a call that takes the value (`handOn`, through `siteUse.branch`). The first root gets the branch, every other root of a merge gets a may branch, and a hand-off that runs again for one run of its root is reported (`cfg.Repeats`). Before #374 they only marked the first root (`FindMutableRoot`) polluted. So `for … { ch <- q }`, `qs = append(qs, q)`, `qs[i] = q` and `m[k] = q` in a loop were missed, while `for … { use1(q) }` was reported. `q := a; if c { q = b }; ch <- q; b.Find(&us)` marked only `a`.

**Structs handed on (#341).** A callee that gets a struct can reach the `*gorm.DB` inside it as well as one passed directly. So `useDeps(deps{DB: q}); q.Find(&us)` is a reuse. At run time the helper's condition reaches the later query (`e2e/internal/handoff_test.go`). `-fix` takes these hand-offs as escapes ([Struct stores](#struct-stores)), and `handleHolders` makes them branches:

| Rule | Detail |
| --- | --- |
| What is handed on | An argument or the receiver of a call, `go` or `defer`, also through a method value. It is a struct, its address or a copy, also boxed in an interface. A closure that captures the struct and makes such a call makes it at its own site |
| What it holds | `RootTracer.GormDBFieldValues`: the `*gorm.DB` fields it holds inline, at any depth of struct fields, embedded or not. Each field is read at the call as a field load is (`fieldStoredValuesAt`), so a field replaced before the call holds the new value. A pointer field is not followed |
| Not a branch | A `*gorm.DB` method. A `pure` function. An inferred getter, whose call is read as the field it returns (`HasParamReads`, #342). A method of `gorm.Statement`, which reads only the configuration of its `DB` (`stmt.Parse(&User{})`). A static call into a package that does not reach gorm, with no function argument (`reflect.ValueOf(s)`) |
| Not followed | A struct sent on a channel, stored in a slice, a map or memory other than a variable, or packed into a variadic argument (`fmt.Println(s)`). A call through an interface, whose receiver is boxed before the call |
| No root | The field of a struct that a call returned (`FieldRoot`). `r := newRepo(db); r.a(); r.b()` stays unreported, as a call's struct is not a local one |

Decisions:

- **The same rule as `helper(q)`.** A callee that only reads the struct is still a branch, as `voidFunc(q)` is. Mark a read-only helper `//gormreuse:pure`.
- **A getter only reads.** `r.DB()` is traced as `r.db` since #342, so the query on its result is the branch. Counting the call too made `r.DB().Find(&a)` two branches. `r.DB().Find(&a); r.DB().Find(&b)` is still reported, as both queries run on `q`. A getter that also branches the field in its body is missed.
- **No fix.** `-fix` withholds the Session for a stored root that other code reaches ([Struct stores](#struct-stores)), and the reports here are such roots.

<details><summary>Before #341, and the effect on real code</summary>

Before #341 only a `*gorm.DB` argument, or one boxed in an interface, was a branch. So `useDeps(deps{DB: q}); q.Find(&us)` was missed.

On gorm's own repository the root module goes from 66 to 69 reports. Two pass a Statement that holds the query callback's handle to the Dialector interface's BindVarTo, in a loop. One hands gorm's preload builder to the user's `Preload` callback, which changes the handle in place. `tests`, new-api and gin-vue-admin are unchanged. Without the `gorm.Statement` rule, five more reports came from Statements that gorm builds around a handle and calls the methods of, such as Build and Parse.

</details>

### One handler for every call site

`handler.handleSite` handles a call, a `go` statement and a `defer` statement alike (#254). It does the same thing whether the site names its function directly, through a method value or through a method expression.

| Part of the site | Handling |
| --- | --- |
| Receiver of a `*gorm.DB` method | A branch of its root. A method that returns an immutable copy (`Session`, `Debug`, ...) only reads it. A plain call whose result reassigns the variable is an assignment (`isAssignment`) |
| Argument of a function that is not `pure` | A branch, also when it is boxed in an interface (`pollutionsource.UnwrapGormDB`), and when the callee is unknown (`go fn(q)` with `fn` a parameter). The `immutable-param` caller contract is checked for each argument |
| Read-only argument of a `*gorm.DB` method | A read (#185) |
| A Session/Debug copy, or a chain that holds a subquery | An alias use |
| A branch on every iteration of a loop that the root is defined outside of | Reported at once (`cfg.Repeats`). This covers `for … { go q.Find(nil) }`, `for … { defer q.Find(nil) }` and `for … { helper(q) }`, as `helper(q); helper(q)` already was |

`handleArgs` skips the receiver of a gorm method (`Site.ArgParam`), so a receiver is recorded once (#202).

The mode (`Site.Mode`) decides only two things:

| Mode | How an earlier change is found | What a branch records |
| --- | --- | --- |
| Plain call | Left to `DetectViolations`, which orders uses by position | A use |
| `go` | The changes that reach its block (`IsPollutedAt`) | A branch use |
| `defer` | The changes that may run in the same run of the function, before or after it (`IsPollutedAround`, #375) | A branch use |

<details><summary>go/defer ordering and read-only varargs</summary>

`go` and `defer` statements are handled after the other instructions of their function, so every plain use is recorded by then. A root with no plain branch, as in `go q.Count(nil); go q.Find(nil)`, names as its first branch the earliest branch use recorded before the reported one (#309).

`pollutionsource.isReadOnlyUse` accepts a `go`/`defer` statement, so the varargs of `defer db.Find(&rows, "id IN (?)", sub)` are not a leak. Since #309 so does `isReadOnlyVariadicArg`, so `defer log.Printf("%v", q)` is not a leak either, for the handler and the pure validator alike.

</details>

**A value that may hold several roots (#252).** Take `q := b; if c { q = a }`. `q` resolves to `b` (`FindMutableRoot`), and `FindAllMutableRoots` returns `a` too. The other roots get a *may branch* (`Tracker.RecordMayBranch`). `DetectViolations` checks a may branch against the root's own uses, both ways: a use of `a` after it, and a polluting use of `a` before it. The second also catches `boundMethodWithPhiOnePollutedReverse`, where the polluting branch is in a block that is handled after the merge. A `go`/`defer` checks its first root against may branches too.

<details><summary>Decisions</summary>

- **Two may branches are never checked against each other.** Two values that each may hold a root can be exclusive. In `chaosVariableSwapLoop`, `q1` and `q2` swap in a loop. Each may hold either original root, but never the same one. Reporting such pairs would be a false positive for every swap.
- **Rejected: record the use for every root.** It reported the swap above, and every pair of values that share a root only through correlated merges.
- **A may branch is recorded only for a root defined before the site in this run** (`siteUse.definedBefore`). Its block dominates the site's, or it is outside every loop, or in another function. In a loop body, a value read before a root's definition holds the root of the previous iteration. A may branch of it would pair with this iteration's uses (`chaosUltimateStressTest` in `nested_chaos.go`).
- A root with may branches has hidden uses for the fix (`Violation.HiddenUses`). The fix cannot order them, so it adds a Session to every root with a later use.

</details>

<details><summary>Before #254 and #252</summary>

- Only the receiver of a plain call was checked for a branch on every loop iteration. In gorm's own code the wider check adds two reports. Both are a `*gorm.DB` parameter passed to a callee in a loop: `plugin.Initialize(db)` in `Config.AfterInitialize`, and `preloadEntryPoint(db, ...)` recursing per relation.
- The method-value path always recorded a branch, so `if c { w := q.Where; q = w("a") }; q.Find(nil)` was reported.
- The `go`/`defer` path read a method value as its `m$bound` wrapper, which has no receiver, so `f := q.Find; defer f(nil); q.Count(nil)` was missed. It also had no `pure` exemption, did not unwrap boxed arguments, ignored read-only arguments and calls with no static callee, and recorded no alias uses.
- Before #252 only the first root of a merge got the use. The others were only checked for an earlier change, so `q.Find(nil); a.Find(nil)` was missed.

</details>

### Which merges are reassignments

A straight-line `q = q.Where("a")` needs no special case. The new SSA value is its own root, and `q.Where("a")` is a branch of the old one. The special case is a merge (#175). In `if c { q = q.Where("a") }; q.Find(nil)`, the use reads `Phi(q₁, q₂)`, and `FindMutableRoot` resolves that Phi to `q₁`. If `q.Where("a")` polluted `q₁`, `q.Find` would be a false second branch. So `isAssignment` records such a call with `RecordAssignment`, which does not pollute.

`isAssignment(call, root)` follows the call's result forward through chained gorm calls and Phi nodes. It returns true only when it reaches a merge whose `FindAllMutableRoots` include `root`. A merge is a Phi, or a Store to an `Alloc` or to a captured variable (`FreeVar`, #177). Only that merge can hand `root` back to a later use.

| Code | Merge | Result |
| --- | --- | --- |
| `if c { q = q.Where("a") }` | `Phi(q₁, q₂)` holds `q₁` | assignment |
| `for … { q = q.Where(x) }` | Loop-header Phi | assignment |
| `if a { q = q.Where("a"); if b { … }; q = q.Where("c") }` | Reaches `Phi(q₁, …)` through the inner Phi and `Where("c")` | assignment |
| `if c { q2 = q.Where("a") }` | `Phi(nil, q₂)` | branch of `q` |
| `q2 := q.Where("a")`, `q2` captured by a closure | Store to `q2`'s `Alloc` | branch of `q` |
| `q := db.Where("b"); if c { q = db.Where("a") }` | `Phi(q₁, q₂)`, both branches of `db` | branch of `db` |
| `for v := range seq { q = q.Where(v) }` | Store to the captured `q` (`FreeVar`) | assignment |
| `for v := range seq { last = q.Where(v) }` | Store to the captured `last` | branch of `q` |

> [!WARNING]
> The check asks whether the merge *can* yield `root`, not whether it does on the path that ran the call. `if c { q2 = q.Where("a") }; if d { q = q2 }` reaches `Phi(q₁, …)` and is still treated as an assignment (`derivedThenMergedBack`).

<details><summary>Before #175</summary>

Any Phi or Store to an `Alloc` counted as an assignment. A chain derived into a different variable was then never a branch. Branching a mutable `db` parameter twice under a condition was missed.

</details>

### Closure uses

A use inside a closure belongs to another function than the uses around it. Two rules decide its order and its reachability.

| Rule | What it does |
| --- | --- |
| Ordering (#68) | Take a closure called at a *single* later call site: `f := func() { q.Find(nil) }; …; f()`. Its uses are ordered by that **call-site position**, not by the body's source position. A closure defined early and called late is reported at the call site, and the earlier direct use is the first branch (`storedClosureReordered` in `evil.go`) |
| Reachability (#179) | The same single invocation, and also an IIFE, gives the closure an invocation site: the block of its one plain call (`Tracker.RecordInvocationSite`). When two uses are in different functions, `isReachable` lifts each use through these sites to the innermost function that holds both. It then asks the CFG whether the first site reaches the second |

Only the single-call case moves. IIFEs (called in place), deferred or spawned closures, and closures called from several sites keep their body positions.

<details><summary>Examples and conservative cases</summary>

With reachability, these are not reported: `if c { f() } else { q.Count(nil) }`, `if c { f(); return }; q.Count(nil)`, and one IIFE per branch. Positions still decide the order.

Conservative cases:

- If the two sites are the same block, or no common function is reached, the pair counts as reachable, as before #179.
- A closure with no single plain call (more than one call, `defer`, `go`, passed as an argument, stored) has no site, so its uses stay conservative.
- A nested closure without its own site does not inherit its parent's site either. It can escape the parent and run after it.

</details>

## Directives

Directives are line comments of the form `//gormreuse:name[,name...]`, lowercase, with no spaces. They can be combined with commas: `//gormreuse:pure,immutable-return`.

| Directive | Effect | Contract checked |
| --- | --- | --- |
| `//gormreuse:ignore` | Suppresses reports in its scope ([Ignore scopes](#ignore-scopes)) | Reported unused when it suppresses nothing |
| `//gormreuse:pure` | Marks a function, method or closure as not polluting its `*gorm.DB` argument | Body ([Pure contract](#pure-contract)) |
| `//gormreuse:immutable-return` | Marks a function, method or closure as returning an immutable `*gorm.DB`, like `Session`/`WithContext` | Body: a provably mutable return is reported ([Immutable-return body contract](#immutable-return-body-contract)) |
| `//gormreuse:immutable-param` | Treats the function's `*gorm.DB` parameters as immutable inside it. The caller is responsible for passing an isolated value | Callers: a mutable argument to a parameter the function branches is reported ([immutable-param caller contract](#immutable-param-caller-contract)). Body: a discarded chain on the parameter is reported ([immutable-param body rule](#immutable-param-body-rule)) |
| `//gormreuse:immutable-input(name)` | Declares that the function passes an **immutable** `*gorm.DB` to its callback parameter `name`, a user-defined equivalent of gorm's `Transaction`/`FindInBatches`. The callback's `*gorm.DB` parameter is then immutable, so reuse inside the callback is allowed | Body: a mutable value passed to the callback is reported ([immutable-input body contract](#immutable-input-body-contract)) |

Generated files are skipped with the rest of the analysis. Their directives are never trusted ([Directives across packages](#directives-across-packages)).

### Syntax

`readDirective` in `directive.go` reads every comment once, in this order:

<details><summary>The four parsing steps and their messages</summary>

| Step | Rule | Problem reported |
| --- | --- | --- |
| 1 | Cut the comment at the first `//` after the leading `//`. The rest is a reason. `//gormreuse:ignore // reason`, `//gormreuse:ignore //reason` and `//gormreuse:ignore//reason` are all a bare ignore. `//` is the only reason separator; ` - reason` was never accepted | |
| 2 | Read what is left with `ast.ParseDirective`, with no normalization | A comment whose body starts with `gormreuse:` but is not a gormreuse directive: `malformed gormreuse directive: write it as //gormreuse:name`. So is an empty part in the list (`pure,`, `pure, immutable-return`) |
| 3 | Check every part of the comma list | A part that is not `ignore`, `pure`, `immutable-return`, `immutable-param` or `immutable-input(name)`: `unknown directive gormreuse:<part> (want ...)`. A malformed `immutable-input(...)` part, such as `immutable-input(cb` or `immutable-input()`: `malformed gormreuse:<part> directive: write it as immutable-input(name)` |
| 4 | Check for text after the name | `gormreuse:<name> takes no argument; write a reason after //` |

</details>

> [!IMPORTANT]
> A comment with a problem has no effect at all, even when only one part of its list is wrong (#156). Keeping the valid parts was rejected: the user meant the whole list, and a half-applied list hides the typo.

Problems are reported by `FindDirectiveProblems` through `pass.Reportf`, at the comment.

<details><summary>Before #156</summary>

`hasDirective` looked for one known name in the list. So `//gormreuse:pure,imutable-return` still marked the function pure, and nothing reported the typo.

</details>

### Ignore scopes

`//gormreuse:ignore` suppresses warnings for the same line. When the comment is the first token on its line, it also covers the next line. A trailing comment after code covers its own line only (#189). `buildIgnoreMap` (run by `Ignores.AddFile`) decides this from the smallest column at which an AST node starts or ends on the comment's line.

`directive.Ignores` applies one scope rule to every diagnostic (#190). `RunSSA` sends each report through `Ignores.Ignore(pos)`, and `run` sends directive problems through it too. Lookups go from the most specific scope to the widest, and mark the first match used:

| Scope | Rule |
| --- | --- |
| Line | The diagnostic's own line, or any line of the innermost statement that holds it. Blocks and labeled statements are skipped. For `if`, `for`, `range`, `switch`, `select` and case clauses only the header (up to `{` or `:`) counts, so a header ignore does not reach the body. An ignore comment inside a closure nested in the statement belongs to the closure, not to the outer statement |
| Function | A `func` declaration whose doc comment holds the ignore: from the doc comment to the closing brace, closures included. A closure has no function-level ignore of its own |
| File | The whole file |

> [!IMPORTANT]
> An ignore only suppresses reports. Every pass runs on ignored code, so `PureVerdicts.Failed` and `needsImmutableParam` see ignored functions. An ignore is used only when it suppressed a report.

<details><summary>Reports that bypass ignores</summary>

Two kinds of report bypass the ignores and go straight to `pass.Reportf`:

- Unused-ignore reports: an ignore never silences another ignore's staleness.
- `panicGuard` internal-error reports.

</details>

<details><summary>Before #190</summary>

A function-level ignore skipped the function in every pass. That hid contract violations while the directive stayed trusted: a leaking `pure` function was trusted at call sites. It never covered closures. It marked the ignore used even when nothing was suppressed.

</details>

### Placement

A directive applies to the function it is written on. `placement` decides which one.

<details><summary>The placement patterns</summary>

```go
// Doc comment of a declaration:
//gormreuse:pure
func helper(q *gorm.DB) { ... }

// Next-line pattern (before a closure):
//gormreuse:pure
helper := func(q *gorm.DB) { ... }

// Same-line pattern (after the opening brace):
helper := func(q *gorm.DB) { //gormreuse:pure
    ...
}

// Multi-assignment (all direct closures get the directive):
//gormreuse:pure
a, b := func(q *gorm.DB) { _ = q }, func(q *gorm.DB) { _ = q }
```

</details>

A same-line directive belongs to the innermost function whose `{` is before it on the line (#315). That can be a closure nested in a closure, or a closure in a declaration's body. `func outer(db *gorm.DB) { run(func(q *gorm.DB) { //gormreuse:pure` marks only the closure. The placement index keeps the closures' braces of each line for that check. The comment must also be inside the closure's body: in `a := func() {}; b := func() { //gormreuse:pure` it marks only `b` (#348).

`immutable-input` accepts the same placements as `pure`. `ImmutableInputSet` places it with a `DirectiveFuncSet` that has no signature check. Closure directives are keyed by the `FuncLit` position, and `Callbacks` looks a closure up through `fn.Syntax()`. A method's key names its receiver's type from the type checker. So a method declared through an alias receiver (`type R = repo; func (r *R) With`) gets the key of `repo`'s method. That is the key the lookup builds (#348). A comment attached to no function is reported as `not attached to a function or closure`.

<details><summary>History (#315, immutable-input placement)</summary>

- Before #315 the declaration path (`findDirectiveAfterFuncDeclBrace`) lacked the nested-closure check. It also made `outer` pure, and `immutable-input`, which shares the placement, attached to both.
- `immutable-input` used to be read only from a `FuncDecl` doc comment. On a closure or after the opening brace it had no effect and was not reported.

</details>

#### Directive lookup cost

Every call site asks the directive sets about its callee, so a lookup must not scan the file (#301):

<details><summary>How each lookup avoids a scan</summary>

| Lookup | How |
| --- | --- |
| Which function a directive is on | `placement.addFile` reads the comments of a file once, and keeps the groups that hold the directive. Only a file with such a group is walked, once, to place every closure's directive. A closure is matched only against the groups on its brace line, or on the line above it or its statement, in file order. So the first match is the same as a scan of all comments |
| `DirectiveFuncSet.Contains` | Remembered per declared function, misses included. `Add` and `Import` clear it. An inferred function is checked first, so `Infer` needs no clearing |
| Innermost statement of a report, for a line ignore | The file's statements and closures are kept in source order, with the index of the innermost one that holds each. A lookup is a binary search and a walk up the holders. Function-level ignore spans are searched the same way |

The cases `ignores`, `ignorefn`, `directives`, `unuseddir`, `helpersign`, `closurepure`, `closurepuresame` and `closurepurepkg` in the [allocation-growth gate](#allocation-growth-gate) cover these.

</details>

<details><summary>Before #301</summary>

A lookup scanned every comment of the file. It placed a closure's directive by walking its statement and the whole file again. An ignore lookup walked the file from its root. n closures with a directive in one function took 108 s at n = 800.

</details>

### Unused and redundant directives

Unused directives are reported as warnings.

| Report | When |
| --- | --- |
| Unused `//gormreuse:ignore` | A line or function-level ignore that suppresses no diagnostic |
| Unused `//gormreuse:pure` | The directive matches no function |
| Unused `//gormreuse:immutable-return` | The directive matches no function |
| Unused `//gormreuse:immutable-param` | The directive matches no function (no `*gorm.DB` parameter) |
| **Redundant** `//gormreuse:immutable-param` | Signature-valid, but with no effect: the parameter is never reused, so even treated as mutable it would produce no violation to suppress. Reported at the function declaration |
| Unused `//gormreuse:immutable-input(name)` | `name` is not a parameter, is variadic, is not a function type, or the callback has no `*gorm.DB` parameter. Also a comment attached to no function |

<details><summary>Combined and redundant directives</summary>

- The redundant check is skipped when combined with `//gormreuse:pure`. A valid pure function cannot branch its parameter, so immutable-param is redundant there by construction. The check is callee-side only: callers are not checked for it.
- For combined directives (`//gormreuse:pure,immutable-return`), if either part is used, no unused warning is reported.
- Each of these reports has a fix that deletes the name ([Contract and directive fixes](#contract-and-directive-fixes)).

</details>

**Variadic callbacks are not supported (#259).** For `func many(db *gorm.DB, cbs ...func(*gorm.DB))`, each element of `cbs` would be a callback. Support needs both sides. The body check would follow every element of the slice. The callback collectors read one callback per argument (`CollectedCallbackArgs`), and would have to resolve every function the caller packs into the variadic slice. Neither does today. So the directive is reported unused with `parameter "cbs" is variadic; variadic callbacks are not supported`. Before, it got the misleading `is not a function type`. The callbacks keep a mutable parameter, which reports more, not less.

## Directive contracts

Each directive is a promise the analyzer then trusts. The body checks keep the promise honest. Each check decides in one direction only.

| Directive | Check | Benefit of the doubt |
| --- | --- | --- |
| `pure` | `purity.Validator`: parameter-derived values must not be polluted or escape | No. A definitive violation revokes trust |
| `immutable-return` | `purity.ImmutableReturnValidator`: no **provably** mutable return | Yes: anything it cannot prove mutable passes |
| Inferred `immutable-return` | `purity.InferImmutableReturns`: every return **provably** immutable | No: positive proof only |
| `immutable-param` | Caller-side: no mutable argument to a parameter the function branches. Body: no discarded chain on the parameter (#346) | Calls with no static callee are not checked |
| Inferred `immutable-param` | `inferCallersPassImmutable`: every call passes an immutable value (#349) | No: positive proof only |
| `immutable-input(name)` | `ValidateImmutableInputs`: no mutable value handed to the callback | No: escapes are reported |
| Inferred `immutable-input(name)` | `purity.ProvesImmutableInput`: the body check passes (#349) | No: positive proof only |

### Pure contract

`purity.Validator` checks the body of every `//gormreuse:pure` function. It follows "parameter-derived" values, and reports when one is polluted or escapes.

| Step | What it does |
| --- | --- |
| Seed | Every parameter whose type holds a `*gorm.DB` without going through an interface: `*gorm.DB`, `**gorm.DB`, `[]*gorm.DB` (variadic too), maps, channels, arrays, and structs with such a field. A method's receiver is not seeded |
| Propagate | Through Phi, Extract, call results, loads (`*p`), channel receives, `FieldAddr`/`Field`/`IndexAddr`/`Index`/`Lookup`/`Slice`, conversions, range iteration, and stores into a local `Alloc` (`p := &db`, `h := holder{db: db}`). Also through `MakeInterface`, `ChangeInterface` and `TypeAssert` (#376), so `var x any = db; x.(*gorm.DB)` is derived. It repeats until nothing changes, so loop Phis and later stores are seen |
| Call results | Derived: the result of a gorm chain method on a derived receiver, or of a call that is not trusted as pure with a derived argument. So is the result of a closure that returns a derived value it captures (`func() *gorm.DB { return db }()`). So is the result of a trusted pure function when one of the parameters it may return (`ReturnedParams`) gets a derived argument. Session, WithContext, Debug and `immutable-return` functions return a fresh handle, so their results are not derived |
| Closures | A `MakeClosure` whose bindings are derived seeds the closure's free variables, and validates the closure body as part of the pure function (IIFE, `go func(){...}()`, stored closures). The `MakeClosure` is derived itself (#308). Stored in a variable that another closure captures, it makes that variable derived, so a call of it there is seen: `get := func() *gorm.DB { return db }; func() { get().Where("x") }()` is reported. A call through the variable is read as the call of the closure it holds (`FuncVars.LocalValue`, [Function variables](#function-variables)) |
| Check | `Call`, `Go` and `Defer` share one check. `*gorm.DB` method values (`Find$bound`) are checked where they are created. Stores outside local variables and `pollutionsource.Leak` cover escapes. So does a `Return` of the argument inside another value (`checkWrappedReturn`, #376) |
| Direct writes (#405) | `checkDirectWrites` reads the direct writes of the function from `stmtwrite.Cache`, which the pure check and the immutable-param inference of one pass share. The walk starts at each `*gorm.DB` parameter but the receiver, and at the loads of the variable that holds it when a closure captures it. That variable is go/ssa's spill of the parameter, or a copy `db := db` in an inner block before any branch: a variable of the parameter's name, stored in the function's first block, that the parameter is the one value stored into and whose address goes only to loads and closures. Any other variable of the body that is assigned the parameter is not one. The walk follows only a direct path: a field address of the handle (not `Error`, `RowsAffected` or `Config`), a field of that, a load of the Statement, a map or a slice from it, and an element address of the slice. A `Store` into such an address or the handle, a `MapUpdate` of such a map, and `delete`, `clear` or `copy` into such a map or slice, also deferred or spawned, are definitive pollution: `*db = gorm.DB{}`, `db.Statement = nil`, `db.Statement.Distinct = true`, `db.Statement.Clauses["X"] = c`, `db.Statement.Selects[0] = "id"`. A local that go/ssa keeps in a register, as in `st := db.Statement; st.Distinct = true`, is the same value. Anything else is not followed: `db.Statement.DB`, the parent of a `Session` or `Debug` copy that shares the Statement, a `*gorm.Statement` parameter, which may be a fresh Statement that no argument holds, a variable that is assigned again, captured or has its address taken, a copy, a `Session` or `Debug` result, a holder parameter, an `append` result, a lookup, a conversion, a merge, or a call. Each write records whether it is a Store of the parameter it writes (`stmtwrite.Write`). A write is not reported again only when it is such a Store that already has a definitive violation, such as the escape of `db.Statement.DB = db`. Another value stored over the argument's memory, as in `db.Statement.DB = other`, gives both the escape of that value and the write. |
| Interface calls | An interface method call (`IsInvoke`) on a derived interface that is a `MakeInterface` of the `*gorm.DB` itself (`var i wherer = db; i.Where("x")`) is the `*gorm.DB` method: definitive pollution, unless the method is `Session`, `WithContext` or `Debug`. On any other derived interface (`x.(wherer).Where("x")`, a boxed struct) it is a warning, as a call of a function that is not pure is (#376). The result is derived unless the method is one of those three |

A violation is **definitive** (`Violation.Definitive`) when the argument is polluted by a gorm chain method, escapes, or is passed to a pure function that already failed. Only definitive violations put the function in `PureVerdicts.Failed`, which makes the tracer stop trusting it at call sites. Passing the argument to a function that is not marked pure stays conservative and does not revoke trust (`nestedClosureOuterPureViolation`, PV117).

**Failure is transitive (#187).** `validatePureFuncs` in `internal/pure.go` validates the pure functions from a worklist until `PureVerdicts` stops changing, then reports from a final round. When a verdict changes, the pure functions that call or refer to that function (`pureCallers`, closures inside them included) are validated again. The failed set and each returned set only grow, and larger ones only add violations and returned parameters. So this ends, at the same verdicts in any order. A chain of `n` pure functions, each returning the next one's result, takes about `2n` validations. Repeating whole rounds took `n` rounds of `n` (#260).

<details><summary>Decisions and limitations</summary>

- **The receiver is not seeded.** It usually holds the application's root handle, and the directive's signature check also ignores receivers (`OrmWrapper.GetDB` is reported unused). Seeding it would report every `r.db.Where(...)` in a pure method.
- **A direct write is definitive (#405).** It changes the argument's query as a chain method does. Before #405 the check saw only calls, so `mark(db *gorm.DB) { db.Statement.Distinct = true }` passed, and `mark(sub)` before the outer query of `sub` was trusted as a read. A call of a Statement method such as `AddClause` stays a warning, as a call of any function that is not pure is. `stmtwrite` decides what is a direct write for this check and for the [inferred immutable-param](#inferred-immutable-param-and-immutable-input), which also reads the parameter's other uses itself. It takes any store into a field of the parameter, `Error`, `RowsAffected` and `Config` included, as a change in place, while this check allows those three. So `func h(tx *gorm.DB) { tx.Error = nil; ... }` is not inferred, though a pure function may write `db.Error`.
- **Only a direct path (#405 review).** Earlier versions of #405 followed the Statement's memory further: through local variables they proved to still hold it, copies, Session copies, holder parameters, append results and hand-offs to calls. Each proof had edges that reported read-only code, such as a Session with a config that only may share, a store into a part of a copy, an element at a non-constant index, a holder slot written before the read, or an append that allocates. A write through anything but a direct path from the parameter may write memory the argument does not hold, so it is a known limitation instead.
- **Interfaces the function boxes are followed (#376).** Before, `var x any = db; x.(*gorm.DB).Where("x")` and `var i wherer = db; i.Where("x")` passed, and callers trusted a function that pollutes its argument. Only an interface made from a derived value is derived. One read from a field, such as `db.Error`, is not, so `db.Error.Error()` stays allowed. Types that hold a `*gorm.DB` only through an interface are still not seeded.
- **A wrapped return is a leak (#376).** `ReturnedParams` and the caller's `pureReturnedArgs` follow a `*gorm.DB` result only. A pure function that returns the argument inside a struct, a pointer, an interface, or a closure that returns it (`return S{DB: db}`, `return &db`) would hide the alias at its callers: `s := Wrap(q); s.DB.Where("a"); q.Where("b")`. So such a return is a definitive violation, and callers stop trusting the function. Without the directive these calls are branches, and so they are again. Following the result's fields to the argument instead would need a `FieldRoot` per argument. A slice, a map or a channel result is not checked: the caller's store into it is a leak already. Neither is a closure that only uses what it captures: its body is checked as part of the function.
- **Variadic callees.** Packing the argument into the varargs of a call is not a slice-store leak. The call is judged as a call: `logv("x", db)` with `args ...any` is a warning, as `one(db)` with `arg any` is (#348). The array must be used only as the variadic argument, and `pollutionsource.Leak` still leaves out read-only callees such as `fmt.Println`.
- **Pure and immutable-param.** A `pure` function is left out of `computeNeedsImmutableParam`, since it cannot branch its parameter. One whose `pure` check failed is checked like any other, so its callers keep the caller check and it exports `BranchesImmutableParam` (#348).
- **Builtins.** `len(dbs)` and `cap(dbs)` cannot pollute and are not reported. `append` and `copy` count every derived argument. Other builtins count only a `*gorm.DB` argument itself.
- **Captured variables.** Take a closure that assigns a derived value to a variable captured from the pure function: `func() { x = db }()`. It is reported as a store through a pointer. The free variable is not traced back to the enclosing `Alloc`.

</details>

#### Returned arguments

A pure function does not pollute its argument, but it may return it: `func p(db *gorm.DB) *gorm.DB { return db }` (#188). The result is then the same `*gorm.DB`, so `r := p(q); r.Find(nil); q.Count(nil)` branches `q` twice. Each trusted pure function therefore records the parameters its result may be, in `PureVerdicts.Returned`.

| Step | What it does |
| --- | --- |
| Summary | `Validator.ReturnedParams` seeds one parameter at a time, propagates as above, and reports the parameter when a `Return` carries a derived value. Seeding one at a time keeps `func(a, b *gorm.DB) *gorm.DB { return a }` from aliasing `b` |
| Transitivity | The result of a trusted pure call is derived when it may return a derived argument, so a pure wrapper of `p` returns its argument too. `validatePureFuncs` repeats until the summaries stop growing |
| Caller | `RootTracer.pureReturnedArgs` traces the call result to the arguments it may be. The first one with a mutable root is the root. `traceAll` returns the roots of all of them. The summary is keyed by the declared function (`PureVerdicts.ReturnedArgs`). So a generic instance and a method value use it too. A method value call such as `same := r.Same; same(q)` has arguments that lack the receiver |
| Immutable argument | When no returned argument has a mutable root, the result is immutable only when every root of the callee's returns is one of its own parameters (`pureReturnsOnlyParams`). `p(db.Session(s))` is then immutable. `if c { return db.Session(s).Where("x") }; return db` can also return a fresh chain, so its call stays its own root |
| Inside a pure function | Branching such a result branches the argument. `p(db).Find(nil)` inside a pure function is a definitive violation, and that function is no longer trusted at its call sites |

<details><summary>Decisions and limitations</summary>

- **The argument wins when the result may be either.** A result that may be the argument or a fresh chain is treated as the argument. A branch from it is then counted against the argument, which can report a reuse on the fresh path. Treating it as fresh would miss the reuse on the other path.
- **A Session-isolated result is not an alias.** `db.Session(&gorm.Session{}).Where(...)` gets its own Statement copy at the `Where` (`TestPureSessionResultIsolated` in `e2e/internal`). So the documented `withTenant` pattern keeps a fresh root, as the README says.
- **A tuple result follows the same rule (#176).** `Validator.ReturnedParams` already looks at every operand of each `Return`, so `return db, nil` records `db`. `pureReturnedArgs` accepts a tuple result, and `pureReturnsOnlyParams` checks every `*gorm.DB` operand, in any position. The `*gorm.DB` elements of one tuple share the call as their root ([Results the tracer cannot see through](#results-the-tracer-cannot-see-through)), so they are all traced to the returned arguments.
- **Across packages.** `ReturnedParams` needs the callee's body. So the declaring package exports the summary and `pureReturnsOnlyParams` in its fact (#195, `pureReturnedArg` in `testdata/src/externalpure/use.go`).
- **The receiver** is not seeded, so a pure method that returns `r.db` is not an alias of anything.
- A pure function that returns a `Session` copy of its argument hides the sharing ([Session and Debug copies](#session-and-debug-copies)).

</details>

<details><summary>Cost (#297)</summary>

The arguments are traced in the same `visited` set as the rest of the search. Each one used to get a copy of it, so the other edge of a Phi walked the same values again. Take `n` statements `if c { q = p(q) } else { side() }`. The copy doubled the work at each one: 20 s and 2.5 GB at `n = 24`.

- In `trace`, sharing changes no answer. An argument whose search ends without a root leaves only values that have no root from any edge.
- In `traceAll`, `allRootsSearch.rooted` records the values that the search has finished, and whether each found a root. A pure call whose argument is finished with a root adds nothing, because those roots are in the result already. The call is not a root of its own. Before, such a call (`if c { q = p(q) }`, where the search reaches `q` first) was added as an extra root that nothing branches.
- An argument that is still being traced (a loop) or has no root makes the call its own root, as before.

`TestAllocationGrowth` (`purephi`, `purephiimm`) checks that the allocations grow at most twofold from `n = 16` to `n = 20`. The shapes without the `else` (`purephisafe`) and with loops (`pureloops`) still grow like `filters`, about 3x per doubling.

</details>

### Immutable-return body contract

`purity.ImmutableReturnValidator` checks the body of every `//gormreuse:immutable-return` function (#194). It reports the function at the declaration when some `*gorm.DB` it returns is **provably** mutable. Otherwise the linter would trust the directive and silently allow unsafe reuse of the return value at call sites. Anything it cannot prove gets the benefit of the doubt, because the directive exists to assert what the analyzer cannot see.

| Returned value | Provably mutable when |
| --- | --- |
| A gorm chain-method call (`Where`, `Order`, …), also `Session().Where(...)` | Always. The trailing chain forks a fresh `clone==0` Statement |
| `Session(cfg)` | `cfg` is a `&gorm.Session{...}` literal that sets `Initialized: true` (`callsite.Site.SessionSetsInitialized`). It returns `tx.getInstance()`, a `clone == 0` handle (#174). An unreadable config is not reported |
| A Phi | Any edge is |
| A call to a closure. That is an IIFE, a `MakeClosure`, or a captured local assigned exactly once (`f := func() ...; return f()`). It is also a package-level variable that always holds one (`var f = func() ...`, #315) | One of the closure's returns is, checked the same way. A closure marked `immutable-return` is trusted here and checked on its own. The closure is the call's `callsite.Site.Callee`, so the check resolves function variables as every other pass does |
| A load of a named result that a deferred closure stores to | Any value the closure stores is. A deferred closure may store on every path through it, with its `defer` dominating the return (or the return in the recover block). Then the values stored before it are hidden and not checked |
| A load of a package-level variable | It is unexported. Its address is used only to load and store it. It is assigned at least once in the package (the `init` function included). **Every** assigned value is provably mutable |
| Anything else the tracer returns: a bare `*gorm.DB` parameter, a named user function's result | Never. The tracer treats these as mutable only conservatively |

<details><summary>Decisions and limitations</summary>

- **Globals: check the package, not the benefit of the doubt.** The issue allowed either. An unexported variable can only be assigned in its package, so when every store there is mutable, every read is. One immutable store gives the benefit of the doubt, since a read may run after it. An example is `var db = x.Where(...)`, then `func init() { db = db.Session(...) }`. An exported variable may be assigned by another package, and a variable whose address escapes may be assigned through a pointer, so neither is reported.
- **Named functions are not followed.** A call into an unmarked named helper stays the benefit of the doubt, as the README says. Only closures, whose body belongs to the function, are followed.
- **Deferred writes are direct stores only.** A store made by a closure that the deferred closure calls is not seen. Neither is one made by a non-deferred closure (`f := func() { r = r.Where("x") }; f(); return`). A closure deferred through a local variable, `defer fix()`, is read through `FuncVars.LocalValue`, also when another closure captures `fix` (#348).
- **A captured closure variable** is followed only when it is assigned once and no closure assigns it.
- Before #315 the check read the raw call value, and missed a closure held by a package-level variable.

</details>

### Inferred immutable-return

`inferImmutableReturns` in `internal/immutable.go` adds functions that provably return an immutable value to the immutable-return set, with `DirectiveFuncSet.Infer` (#261). It runs right after the facts are imported and before any pass reads the set. Every consumer (the tracer, the pure validator, the callback and fix code, `exportFacts`) then sees them through `Contains`.

`purity.InferImmutableReturns` proves a function when every `*gorm.DB` operand of every `Return` is proved, through Phi edges:

| Value | Proved when |
| --- | --- |
| A `Call`, or an `Extract` of a tuple `Call` such as `gorm.Open`'s | `RootTracer.IsImmutableCall` accepts it with a tracer that has no immutable-return set. Its last call is `Session` (with a config literal whose `Initialized` is known false), `WithContext`, `Debug`, `Begin` or `gorm.Open`. Or the declared callee is trusted (declared, or imported from a fact) or already inferred |
| The nil constant | Always |
| A load of a local variable assigned once, such as one a closure captures (#308) | Its stored value is proved (`FuncVars.LocalValue`) |
| A load of a local variable assigned more than once, such as one a `t.Cleanup` closure captures (#342) | Every value stored into it is proved, and every store is in its function. No closure stores into it, and its address is used only to load and store it (`callsite.LocalStores`) |
| A read of a field through a parameter or a receiver (#308). This covers any depth of fields and pointer fields, also when a closure captures the parameter. A read of a package-level variable counts too | Every store in the function that reaches the read is proved, through the roots `FindAllMutableRoots` finds. No store in the package assigns the variable a provably mutable value (`ImmutableReturnValidator.AssignedMutable`). With no store, the value is immutable, as reuse detection reads it. A read through the function's own parameter is also one of its [parameter reads](#parameter-reads) |
| A type assertion, such as `ctx.Value(k).(*gorm.DB)` (#342) | What the interface holds is proved: a `MakeInterface` of a proved value, or a call that returns no `*gorm.DB`, which reuse detection reads as no root. An assertion of the function's own interface parameter is a parameter read |
| A call to a function with parameter reads | Every value the reads select at the call is proved. When the argument is the function's own parameter, or a field read through it, the read is the function's own too: `func Conn(h *Holder) *gorm.DB { return h.DB() }` reads `h.db` |
| Anything else. Examples are a `*gorm.DB` parameter, a field of a local struct, and a dynamic or interface call. A variable that a closure assigns is another, and a named result a deferred closure writes is one | Never |

A function needs at least one such result, so a function that only panics is not inferred. Candidates are the analyzed functions and closures with a body, not in a skipped file, and without the directive. Each has a `*gorm.DB` result, or a struct result whose `*gorm.DB` fields callers read as its `FieldRoot`s (#342).

**Struct results (#342).** A struct result is proved field by field. So `env := newEnv(); env.DB.Create(nil); env.DB.Find(nil)` is read like its `*gorm.DB`-returning twin. Without the proof, the field of the call is a [`FieldRoot`](#struct-roots), and only a directive makes it immutable. Its fields are the `*gorm.DB` fields of the struct and of the structs it holds by value (`ImmutableReturnFieldPaths`), the ones a `FieldRoot` names. The struct must be one of these:

- One the function builds (`&T{...}` or `T{...}`, also merged by a Phi). Its address is used only to fill in its fields, load them and return it.
- One that an accepted callee returns.

Each field holds the values stored into it that reach the return, or nil. These rule the function out: a struct passed to a call (`fill(env)`), stored elsewhere, or kept in a variable a closure captures. So does a struct the function did not build, such as its parameter. So does a field that holds a parameter read: a `FieldRoot` has no argument to select from. A `FieldRoot` whose path passes through a pointer field, which only a getter's read makes, stays a root even for an immutable-return callee (`paramReadCrossesPointer`). The proof says nothing about it.

<details><summary>Decisions</summary>

- **Positive proof, not the benefit of the doubt.** `ImmutableReturnValidator` reports only what is provably mutable, because a declared directive is the user's assertion. Inference has no assertion behind it, so it accepts only what is provably immutable. Between the two, a function stays unmarked.
- **A least fixed point.** A worklist proves each candidate. A candidate that fails only on calls to other candidates waits on them, and is proved again once the last of them is added. So each candidate is proved at most twice. The set only grows, and a larger set only proves more, so the result is the least fixed point, whatever the order. `TestInferImmutableReturns` checks every rotation of the candidates, forwards and backwards.
- **Recursion is not inferred.** A function is never added on the strength of itself. `if n > 0 { return rec(n-1) }; return db.Session(...)` is immutable. But proving it needs a greatest fixed point, and an assumed-true cycle is harder to trust (`notInferredRecursive`).
- **A broken declared contract: trusted at home, not abroad.** A declared `immutable-return` whose body breaks the contract is trusted in its package ([Directives across packages](#directives-across-packages)). A function that returns its result is inferred there too, so the mistake is still reported once. `distrustContracts` proves the inferred functions again with the broken ones distrusted. The ones that no longer hold export no fact, like the broken function itself ([Distrusted contracts](#distrusted-contracts)). The broken contracts are known only after the pure pass, which inference must precede. So this is a second fixed point, not a single one.
- **A declared directive stays used.** An inferable function with the directive is not a candidate, and keeps its body check. Its directive is not reported unused or redundant. It documents the intent, and a later change to the body is reported at the declaration instead of turning into reports at the call sites.
- **`Validate` skips inferred functions.** Their bodies were proved, and a report would say "declared" about a function without the directive.
- **No `-fix` to insert the directive.** The issue suggested it as an alternative to implicit inference. Inference is the same check the analyzer would make when validating an inserted directive, so inserting it adds a comment and no information.
- **What detection reads as immutable, inference accepts (#308).** Reuse detection treats two reads as immutable ([Struct roots](#struct-roots), [Package-level variables](#package-level-variables)). One is a field read through a parameter or a receiver. The other is a global that the function only reads. A wrapper should not change the answer, so inference accepts the reads that detection accepts. The rule is still a positive proof for everything else: a parameter, a chain and an unknown call stay unproved.
- **A getter of a variable the package assigns a chain to is not inferred.** A caller may run `r.db = r.db.Where("x")` before `r.DB()`. Detection sees such a store when it is in the reading function, but a getter is trusted at every call site. So inference refuses a read of a field or global that any function of the package, its initializer included, assigns a provably mutable value (`AssignedMutable`). That is the body check's rule for a global, made strict: the body check needs every assignment to be mutable to report, and inference refuses on one. A global whose address escapes is refused too. A store in another package, or through a field address passed to a function, is not seen, as for reads in detection.
- **A captured variable.** Take `conn := db.Session(&gorm.Session{}); get := func() *gorm.DB { return conn }`. `conn` lives in an `Alloc` that the closure reads through a `FreeVar`. `LocalValue` resolves it to the stored value, with the conditions of [Function variables](#function-variables). Since #342 a variable assigned more than once is proved by every value stored into it (`callsite.LocalStores`). So gorm's test helper shape `db, _ := gorm.Open(...); if testing.Verbose() { db = db.Debug() }; t.Cleanup(func() { db.DB() }); return db` is inferred. Every store is proved, not only the ones that reach the return: that is simpler, and a store that does not reach it is rare. A variable a closure assigns stays unproved, since the closure may run at any point.

</details>

<details><summary>History and measurements (#227, #261, #298, #308)</summary>

After #227 a helper's result is a mutable root. On gorm's own `tests` module most new reports then came from `db, err := OpenTestConnection(...)`. In services they came from helpers such as `r.conn(ctx)` returning `r.db.WithContext(ctx)`. Each needed a manual `//gormreuse:immutable-return` before #261.

The fixed point once proved a candidate again whenever any one of its dependencies was added, and queued it once per failed proof. That grew exponentially with a caller that switches over a chain `c0 → c1 → … → ck` (17.7 s at k = 32, #298). `TestAllocationGrowth` (`inferexp`) holds the allocations near linear. `TestInferImmutableReturnsGrowth` does the same for inference alone on a dense chain, where each `ci` switches over every later `cj`.

Before #308, inference refused field reads through a parameter and globals. So `func (r *Repo) DB() *gorm.DB { return r.db }` made `d := r.DB(); d.Find(nil); d.Count(nil)` a report, while `r.db.Find(nil); r.db.Count(nil)` was not one.

Measured with the command on gorm's repository (v1.31.2), #261:

| Module | Before | After | Removed reports |
| --- | --- | --- | --- |
| Root | 160 | 150 | 10 in `Migrator.AutoMigrate`, all on the results of `GetQueryAndExecTx`, which returns two `Session` results |
| `tests` | 34 | 15 | 19, all on the result of `OpenTestConnection`, which returns `gorm.Open`'s result or nil |

Each removed report branched a `clone > 0` handle, so none was a real reuse. No report was added.

#308 measured the same repository, and two real services (new-api and gin-vue-admin), before and after:

| Module | Before | After | Removed reports |
| --- | --- | --- | --- |
| gorm root | 66 | 66 | none |
| gorm `tests` | 15 | 14 | the Scopes warning at `scopes_test.go:60`, whose chain ends in `Find` ([Scopes Session-family warning](#scopes-session-family-warning-temporary)) |
| new-api | 34 | 31 | 3 test setup helpers that return `gorm.Open`'s result through a variable that a `t.Cleanup` closure captures |
| gin-vue-admin | 32 | 32 | none |

Each removed report branched a `clone > 0` handle. No report was added. The counts are diagnostic lines of `gormreuse ./...`, so they differ from the table above.

</details>

### Parameter reads

A getter such as `func (h *Holder) DB() *gorm.DB { return h.db }` is inferred (#308). A field read through a receiver is immutable where it is read. But the receiver may hold the caller's mutable value. So each call reads the field of its own receiver (#342). Trusting the call everywhere would miss both of these:

```go
//gormreuse:pure
func Tenant(q *gorm.DB) { h := &Holder{db: q}; h.DB().Where("tenant = ?", 1) } // pollutes q

h := &Holder{db: mk(db)}; d := h.DB(); d.Find(nil); d.Count(nil)             // a reuse of mk(db)
```

Before #342 both were missed. The forms written in place, `h.db.Where(...)` and `h.db.Find(nil); h.db.Count(nil)`, were reported. `e2e/internal/getter_receiver_test.go` shows the first one running `... WHERE a = 1 AND tenant = ?`.

A read through the function's own parameter does not make its result immutable. It makes the result what the caller's argument holds there. So inference records it as a `directive.ParamRead`: the parameter, and the path of fields a Go selector selects from it. `h.inner.db` is `h` with the indexes of `inner` and `db`, through pointers too. `DirectiveFuncSet.InferParamReads` keeps the reads with the inferred function, and `facts.Func.ParamReads` exports them with its `ImmutableReturn` fact. A function with no reads is immutable at every call, as before.

Each reader of an immutable-return call reads a function with reads as its selector written at the call:

| Reader | A call of a function with reads |
| --- | --- |
| The tracer (`traceCall`, `traceAll`) | The roots of the values the selector reads from the call's argument (`ParamReadValues`). They are the stores into the field that reach the call, matched as for a `FieldAddr` chain, or the `FieldRoot` of a struct a call returned. A pointer field ends one address, and the rest of the path is selected from each value stored into it. `returnsImmutable` and `IsImmutableCall` say no. A closure with reads is read the same way, before its returns are traced |
| The pure validator (`callResultDerived`) | Derived when the argument a read selects from is derived |
| The immutable-return body check | Through the tracer: `return h.DB()` with `h := &Holder{db: db.Where("x")}` is a provably mutable root |
| Inference of a caller | The values the selector reads at the call are proved. An argument selected from the caller's own parameter makes the read the caller's too |

<details><summary>Decisions and limitations</summary>

- **The safe case is unchanged.** Take a receiver that is a parameter, or a struct with no store into the field before the call. It has no stored value there. So the call is immutable, as `h.db` is. So is a receiver that holds a Session result.
- **Fields the package assigns (#308).** A getter of a field the package assigns a chain is not inferred. A call sees the stores that reach it in the caller, as detection does. The package-wide rule stays because neither sees a store in another method, such as `r.filter()` running `r.db = r.db.Where("x")`.
- **Every read counts.** That includes a read that a store in the getter always overwrites. `func (c *Conn) Reset() *gorm.DB { c.db = open(); return c.db }` still reads the caller's `c.db`. So a caller whose struct holds a chain is reported. Telling the two apart needs the reaching-store walk on a parameter's copy, and the shape is rare.
- **A struct result has no reads.** A helper whose returned struct holds a field of its parameter is not inferred ([Struct results](#inferred-immutable-return)).
- **Embedded and nested receivers (#376).** A promoted getter `o.Get()`, with `Get` declared on an embedded `H`, passes the fresh address `&o.H`. So does `o.H.Get()` or `get(&o.H)`. `paramReadPointee` reads it as `o` with the path `[H, db]`, as `fieldAddrPath` does for `o.db` written in place. Before, the stores were matched against `&o.H` itself, which no store goes through, and every one of these calls was trusted. A pointer read from a field, such as the embedded `*H` of `PO{&H{...}}` or `o.p` of `&T{p: &H{...}}`, also stands for each pointer stored into that field.
- **A store through another read of a pointer field is not seen.** `o.p.db = q; o.p.Get()` loads `o.p` twice. The store goes through one load, and the call passes the other. `o.p.db` written in place misses the same store, so the getter is as precise as the field read.
- **A pointer field the getter writes is not followed.** Take `r.ptr = &Inner{db: q.Where("x")}; return r.ptr.db`. The return loads `r.ptr` afresh, and no store matches that load, as in detection.

On the modules #308 measured (gorm's root and `tests`, new-api, gin-vue-admin), the diagnostics did not change. None of them calls a getter on a struct holding a chain, and their `ctx.Value(k).(*gorm.DB)` reads are written in place, where detection already read them as immutable. `TestAllocationGrowth` (`getterchain`) holds a chain of getters, each returning the one before it, to linear growth.

</details>

### immutable-param caller contract

When a function marked `//gormreuse:immutable-param` actually branches such a parameter, passing a mutable `*gorm.DB` at a call site is reported. The fix isolates the argument with `.Session(&gorm.Session{})` ([Contract and directive fixes](#contract-and-directive-fixes)). The caller can also be made `immutable-param` too, so the contract propagates.

| Call | Checked |
| --- | --- |
| A direct call, a function or method value, a generic instance | Yes |
| The same calls under `go` and `defer` (#192) | Yes. A `go`/`defer` statement goes through the same `handleSite` as a call ([One handler for every call site](#one-handler-for-every-call-site)) |
| A callee in another package | Yes, through its package's fact ([Directives across packages](#directives-across-packages)) |
| A call in a package-level initializer, as in `var n = count(DB.Where("x"))` (#346) | Yes. The initializer belongs to the synthetic `init`, which is not in `SrcFuncs`. `RunSSA` analyzes it after the other functions and reports only its contract violations. Its reuse is not checked |
| An interface method call, or a call through a func-typed parameter | No: there is no static callee (`contractInterface`) |

The check asks `handler.BreaksImmutableParam` of each argument: it is bound to a parameter, and its `*gorm.DB`, directly or in an interface, has a mutable root. The parameter-root fix asks the same before it declares a function `immutable-param` (#245). `computeNeedsImmutableParam` decides whether the function branches the parameter, with counterfactual analyses.

### immutable-param body rule

The callers of an `immutable-param` function pass an isolated value. So a statement that discards a non-finisher chain on the parameter changes a copy, and has no effect (#346). Examples are `tx.Where("t = 1")` and `tx.Where("t = 1").Limit(1)`. It is reported: `immutable-param declared but the result of tx.Where is discarded`. `reportImmutableParamDiscards` in `internal/immutable.go` finds them.

| Rule | Why |
| --- | --- |
| Each call of the chain is a `*gorm.DB` method that returns a `*gorm.DB`, and is not a finisher, a Session-family or a state-only method (`immutableParamDiscardedChain`) | A finisher runs a query. A discarded `tx.Session(...)` changes nothing either way |
| The whole chain is one expression statement (`immutableParamExprStmts`) | `q = tx.Where("a")` with `q` never read is dead code of another kind. A chain through a variable is not a discarded statement |
| A `pure` function is not checked | The pure check reports the same statement as pollution |
| The fix that isolates an argument to such a function is withheld (`GenerateIsolation`) | Without the directive the statement changes the caller's value, and the function's own queries see it. After `applyT(q.Session(&gorm.Session{}))` they would not, and the re-lint would be clean |

Limitation: a callee in another package exports no such fact, so the isolation fix of an argument to it is still offered.

### immutable-input body contract

If the function actually passes a mutable value to the callback, it is reported. `immutableInputFlow` walks the parameter's referrers with a worklist (#193):

<details><summary>How the callback is followed</summary>

| Use of a value holding the callback | Handling |
| --- | --- |
| Callee of a `Call`, `Go` or `Defer` | Its arguments are checked |
| Phi, load, field, element, map lookup, slice, `ChangeType`, `Extract`, range, `MakeInterface`, `TypeAssert` | The result is followed |
| Store into a local `Alloc` or `MakeSlice` (through fields and elements), `MapUpdate` into a `MakeMap`, `append` | The container is followed, so every later use of it is seen |
| Binding of a `MakeClosure` | The closure's free variable is followed in its body. The closure value itself may go anywhere, since its calls of the callback are checked |
| Argument of a call to an `immutable-input` function at its declared callback, or to gorm's `Transaction`/`FindInBatches` | Accepted: that callee makes the same promise |
| Comparison, `len`, `cap` | Ignored |
| Anything else: another call, return, store elsewhere, channel send, `select` | Reported as `callback cb escapes (...)` |

</details>

<details><summary>The callback's arguments, and which values are followed</summary>

The arguments the callback receives:

| Argument | Check |
| --- | --- |
| A `*gorm.DB` | `FindMutableRoot` |
| A pointer to a local `*gorm.DB` variable (`cb(&q)`) | Mutable when any value stored into `q` is |
| Any other argument that holds a `*gorm.DB` (a `**gorm.DB` parameter, a struct) | Reported as `cannot verify the *gorm.DB passed to callback` |

- Only values whose type can hold a function (an interface counts) are followed, so an `int` field of a struct that holds the callback is not.
- A boxed callback is followed rather than reported at `MakeInterface`, because an implicit conversion has no position. The call or return it then reaches is reported instead.

</details>

> [!IMPORTANT]
> Escapes are reported rather than given the benefit of the doubt. The directive makes the callback's own reuse unchecked, so a wrong promise hides real bugs.

<details><summary>Before #193</summary>

`ValidateImmutableInputs` checked only a `Call` whose callee was the parameter itself. So `go cb(...)`, `defer cb(...)`, a closure, `forward(cb, ...)`, `(*(&cb))(...)`, a struct field and a slice element all handed a mutable value to the callback unreported.

</details>

### Inferred immutable-param and immutable-input

Helpers that only ever receive fresh handles were the main false positives left in real code after #308 (#349). Two inferences in `internal/infer.go` remove them. Both run after the immutable callbacks are collected and before any pass reads the parameters, so every later pass sees them through `DirectiveFuncSet.Contains` and `ImmutableInputSet.Callbacks`.

| | `inferImmutableInputs` | `inferImmutableParams` |
| --- | --- | --- |
| Adds | `ImmutableInputSet.Infer` for one callback parameter | `DirectiveFuncSet.Infer` on the immutable-param set |
| Candidates | A function or method with an unexported name, or a closure, with no declared `immutable-input`. The callback has a `*gorm.DB` parameter, is not variadic, and the body uses it | A function or method with an unexported name and a `*gorm.DB` parameter, without the directive, that does not change a parameter in place (`inferChangesInPlace`) |
| Proof | `purity.ProvesImmutableInput`: the body check of the directive, without the function's own callbacks in the set. Each `*gorm.DB` the callback gets must be proved (`ProvesImmutable`) | `inferCallersPassImmutable`: for every call, every `*gorm.DB` argument is proved (`ProvesImmutable`) |
| Callers | Not needed: the promise is about the body | Every reference is the function of a call (instances included). A bound or thunk wrapper, a store or an argument turns it off. For a method, no interface call in the package has its name. The package initializer counts. At least one call |

<details><summary>The fixed points, decisions and limitations</summary>

**Least fixed points.** A larger set only proves more, so each worklist ends at the least fixed point, whatever the order of `funcs`.

- `inferImmutableInputs` proves the candidates with one tracer. A proof that fails only on a call to a candidate waits on it. Once that candidate is added, the candidates that call it (`dependents`) are proved again. A callback of an inferred function is immutable too. So when a pass added something, the immutable callbacks are collected again, as in `RunSSA`. That takes `CollectImmutableCallbacks`, `CollectImmutableInputCallbacks` and `DropReachableCallbacks`. Then the unproved candidates are proved again with a new tracer. The callbacks collected last are the ones every later pass uses.
- `inferImmutableParams` proves the candidates in rounds. A tracer remembers what it traced with the set it had, so each round has a new one. After a round, only the candidates that a newly inferred function calls, also from its closures, are proved again. A stale answer within a round is only ever "mutable", and the candidate is proved again in the next round.
- A function is never proved by assuming itself. So `rec(tx, n-1)` inside `rec`, or a wrapper that hands `cb` to itself, is not inferred, as for [inferred immutable-return](#inferred-immutable-return).

`TestAllocationGrowth` holds `inferparam` (a chain of forwarding helpers, written last first) and `inferinput` (a chain of forwarding wrappers) to linear growth. `inferinputnest`, where each wrapper calls its callback from the callback it hands to the next one, takes one collection per wrapper, and is held to quadratic growth.

Decisions:

- **Exported functions are never inferred.** Another package may call an exported `immutable-param` function, and its calls are not seen. An inferred function exports no fact, so another package treats it as unmarked.
- **Positive proof only (#371).** `tracer.RootTracer.ProvesImmutable` accepts a value only when it knows where every value it may hold comes from. A value that `FindMutableRoot` finds no root for is not enough: the tracer loses values it cannot follow, and detection reads such a value as immutable. Before #371 the proof was the caller-side check of the directive, so a channel receive, a map or slice element, `ctx.Value(k).(*gorm.DB)`, `*p` and a field read through the function's own parameter (`cb(s.db)`) all proved a call. The values it accepts:

  | Value | Proved when |
  | --- | --- |
  | `nil` | Always |
  | A call, or the `Extract` of its tuple | The tracer's immutable source (`isImmutableSource`): a Session-family builtin that does not return a `clone == 0` handle, `gorm.Open`, `Begin`, or an `immutable-return` function whose result reads no parameter. A getter with [reads](#parameter-reads) proves nothing |
  | A parameter | The `tx` of a Transaction or `immutable-input` callback (not distrusted), or the parameter of an inferred `immutable-param` function. A Scopes or Connection callback's parameter never |
  | A Phi, `ChangeType`, `MakeInterface`, `TypeAssert`, or a `FreeVar` bound by value | Every operand is proved |
  | A load of a local variable, also through a closure's `FreeVar` | The address goes only to loads, stores and closures, and every value stored into it, in the function or in a closure, before or after the load, is proved. So `f := func() { q = q.Where("x") }; f(); h(q)` is not |
  | A load of a package-level variable | Every value the package stores into it is proved, there is one store at least, and the package uses its address for nothing else (`ProvenGlobalsOf`, over every function and the initializer) |

- **A declared `immutable-param` parameter does not prove a call (#371).** `computeNeedsImmutableParam` checks the callers of a declared function only while the function branches the parameter. A function that hands it to a helper does not branch it, so its directive is redundant and its callers pass anything. Before #371 the helper was inferred through the declared parameter. Then the reuse in the helper went unreported, and so did a mid-chain value that a caller passed. Now the helper is reported, and the directive is reported redundant, as in 0.22.0: without it, the helper's report stays.
- **A change in place turns `immutable-param` off.** `tx.Where("t = 1"); tx.Find(&a)` relies on changing the caller's value in place. On an immutable value the `Where` does nothing, so the function would run without its filter. `inferChangesInPlace` reads every use of a parameter in the function and its closures (`inferParamUses`), through local variables, captured variables and closures called with it. It refuses a function that discards a chain without a finisher, hands the parameter to a function of the package that changes its own (a fixed point over the package), or to a function of another package that reaches gorm unless its fact makes that parameter finisher-only (#347). A dynamic or interface call, a store outside a local variable, a box and any other use the check does not follow refuse it too. A read of a field counts as a read, except a use of `Statement` other than a read of its fields. A write into the parameter's query through a direct path counts as a change too (#405): `tx.Statement.Clauses["X"] = c` updates the map the Statement holds, and `tx.Statement.Selects[0] = "id"` the slice. `inferParamUses` reads them from the pass's `stmtwrite.Cache`, as the [pure contract](#pure-contract) does. Any other write through the memory, such as through a local copy or a call, stays a read, as before #405. A scalar, a string or an interface field is a copy, so `log(tx.Statement.Table)` or `tx.Statement.Context.Value(k)` stays a read, as before #405. Before #405 the writes were inferred, and their reuse went unreported. Before #371 only the chains of the function itself counted, so `applyTenant(tx); tx.Find(&a)` and `func() { tx.Where("t") }()` were inferred, and a Session caller lost the filter.
- **Test variants.** The variant of a package without its test files cannot see a caller in a test file. So it may infer a function that the test variant does not. The diagnostics of the two variants are reported together, and the test variant sees every caller, so the reuse is still reported. The [parameter-root fix](#parameter-root-fix) keeps the stricter rule of `reportSeesWholePackage`, so the fixes of the two variants agree. `TestInferredParamVariants` checks each variant on `testdata/src/paramvariants`.
- **No caller check, no redundancy report.** `computeNeedsImmutableParam` and `reportRedundantImmutableParam` skip an inferred function: every call passes an immutable value, and there is no directive to report. The body check of `immutable-input` skips an inferred function too, since a report would say "declared".
- **A callback the body never uses is not inferred.** It would hand the callback nothing, and the inference would only change what its callers' callbacks mean.
- **A panic keeps what was proved.** Each inferred function was proved before the panic. After a panic in `inferImmutableInputs` the callbacks are collected again, so the functions inferred so far count.

Limitations:

- **One direction between the two.** `inferImmutableInputs` runs first. A callback called with the parameter of an inferred `immutable-param` function is not proved by it.
- **The proof follows less than detection.** A value in a struct field, a slice, a map or a channel, a field of a struct a call returned, and a package-level variable of another package prove nothing, even when they hold a fresh handle. So does a local variable whose address goes to a call. Their helpers are reported as before #349.
- **An exported package-level variable may be assigned in another package.** `ProvenGlobalsOf` sees only the package's own stores. A test in another package that assigns the variable a chain is not seen.
- **The in-place check is coarse.** A parameter passed to a func value (`fn(tx)`), or stored in a struct such as `&gorm.Statement{DB: tx}`, turns inference off, even when nothing changes it. A helper of the package that changes any of its parameters counts as changing the one it was passed.
- **The parameter-root fix is reachable less often.** Its directive case needs every caller to pass an isolated value. Such a function is inferred and not reported. A function that changes its parameter in place is not inferred. The fix does not declare it either (`paramDiscardsChain`). The directive would make the discarded chain do nothing, and the body check would report it (#346). It gets no fix, as the [discarded statements on a parameter](#parameter-root-fix) rule says.

Measured with the command (#349), before and after:

| Module | Before | After | Removed reports |
| --- | --- | --- | --- |
| gorm root | 69 | 65 | 4, in the callbacks of gorm's callMethod helper, which calls them with a `Session(&gorm.Session{NewDB: true})` result |
| gorm `tests` | 14 | 7 | 7, in the callbacks of a local transaction wrapper in a nested-transaction test |
| new-api | 31 | 19 | 7 on the parameter of a test helper. Every caller passes it `gorm.Open`'s result. 5 on helpers that get a `Transaction` `tx` or a package-level variable assigned `gorm.Open`'s result |
| gin-vue-admin | 32 | 29 | 3 in two import helpers of the export template service, whose only caller passes a `Transaction` `tx` |

Each removed report branched a `clone > 0` handle. No report was added.

#371 measured again with `gormreuse -test ./...`, on gorm v1.31.2 and on the heads of new-api and gin-vue-admin in October 2026:

| Module | Before #349 | 0.23.0 | #371 |
| --- | --- | --- | --- |
| gorm root | 69 | 65 | 65 |
| gorm `tests` | 14 | 7 | 7 |
| new-api | 168 | 98 | 107 |
| gin-vue-admin | 32 | 29 | 29 |

No report was added or removed against the reports before #349. The 9 new-api reports that came back are on helpers that get the package-level `DB` (assigned `gorm.Open`'s result) and then hand it to a func value or store it in a `gorm.Statement`, and on a test helper that gets a package-level variable of another package. Each still branches a `clone > 0` handle. The spot-checked removals that stay get a `Transaction` `tx`, a test helper's `gorm.Open` result, or the package-level `DB`.

</details>

### Directives across packages

`exportFacts` in `internal/fact.go` exports a `facts.Func` object fact for every exported function and method with a directive or a finisher-only parameter, after every pass has run (#195). A caller reads it through `facts.Table`, which `RunSSA` plugs into each consumer. A function of another package has no syntax in the SSA program, so a consumer asks the table only for a function without syntax.

<details><summary>The fact fields</summary>

| Field | Set when | Read by |
| --- | --- | --- |
| `Pure` | Marked `pure`, valid or not, as `DirectiveFuncSet.Contains` decides in the package | `DirectiveFuncSet.Import` on the pure set |
| `PureFailed` | The body definitively breaks the contract | `PureVerdicts.HasFailed`. So a pure function that passes its argument to it fails too (#187) |
| `Returned`, `ReturnsOnlyParams` | The parameters the result may be (#188), and whether nothing else is returned | `PureVerdicts.ReturnedArgs`, `RootTracer.pureReturnsOnlyParams` |
| `ImmutableReturn` | Marked `immutable-return` or inferred (#261), and not distrusted: the body keeps the contract without relying on a broken one (#314) | `DirectiveFuncSet.Import` on the immutable-return set |
| `BranchesImmutableParam` | In `needsImmutableParam` | The caller-side check (`handler.Context.NeedsImmutableParam`) |
| `ImmutableInput` | Marked `immutable-input(name)`, and not distrusted: `ValidateImmutableInputs` reports nothing, and the body does not rely on a broken contract (#314) | `ImmutableInputSet.Import`. The callback collectors and the body check then see the callee's callbacks |
| `FinisherOnly` | The function uses those `*gorm.DB` parameters only to run queries (`fix.Generator.HandoffFinisherOnlyParams`, #347). It needs no directive | `fix.Generator.HandoffFinisherOnlyParams`, so a caller's fix may give the argument a Session ([Helpers that only run queries](#helpers-that-only-run-queries)) |

</details>

<details><summary>Decisions, and the shortcuts for dependencies</summary>

Decisions:

- **A broken contract is trusted at home, not abroad.** In the declaring package a broken `immutable-return` or `immutable-input` is reported at the declaration. It is still trusted there, so the mistake is reported once. A caller in another package may belong to someone who never sees that report. So the fact leaves the directive out, and the callee is treated as unmarked. A broken `pure` is distrusted in both places (#66).
- **Distrust is transitive (#314).** A function that keeps its contract only because it trusts a broken one exports no fact either, like pure's failures (#187). See [Distrusted contracts](#distrusted-contracts). In the declaring package both are still trusted, so the broken contract is reported once, at the broken function (`BadConn` in the history below).
- **Exported functions only.** Another package can name only an exported function or method, so only those get a fact. This also keeps fact expectations in the test fixtures few. `analysistest` fails on a fact without an expectation, and an object fact is expected with `// want Name:"..."` on the declaration line.
- **No fact means no directive.** There is no disk fallback. Every driver that supports facts (`singlechecker`, `go vet`, golangci-lint, `analysistest`) runs the analyzer on the dependencies first, so a dependency without a fact has no directive function. A driver without facts would see none, which reports more, not less.
- **The #191 exemption is not exported.** It concerns the callee's own parameter, which is checked when the callee's package is analyzed. Exported functions stay out of it, because facts cannot tell the declaring package how other packages call it.
- **No fact for a generated file (#259).** The functions of a skipped file are not analyzed, so their directives are never validated. `exportFacts` skips them, so another package treats them as unmarked. Validating the skipped files only to decide their facts was rejected. A generated file is excluded from every pass, so its author would never see a broken contract reported, and an unmarked callee reports more, not less.
- **A directive in a generated file of the same package (#315).** The same rule holds at home: a directive that is not validated is not trusted. Generated files are not added to the directive sets. `DirectiveFuncSet.hasDirective` finds no directive on a function whose file was not added. So every kind and placement in a generated file counts as unmarked in its own package too. An unmarked callee reports more, not less.

**Cost.** With `FactTypes` set, the drivers also run the analyzer, and `buildssa`, on every dependency, including the standard library. Only facts come out of those runs. Two shortcuts keep them cheap, and neither changes a report:

| Shortcut | Why it is safe |
| --- | --- |
| `run` skips the directive scans of a file with no comment that contains `gormreuse:` | Every directive, and every comment `FindDirectiveProblems` reports, contains it |
| `run` builds SSA, by calling `buildssa.Analyzer.Run` itself, only for a package that reaches `gorm.io/gorm` through its imports (`typeutil.ReachesGorm`). `Analyzer` requires `ctrlflow` instead of `buildssa`: buildssa needs it, and it already runs on every dependency for its own facts. `RunSSA` gets a nil `ssaInfo` for any other package, and still reports unused directives and ignores | No value in such a package can hold a `*gorm.DB`, since its type would name `gorm.DB` and so record gorm among the imports |

</details>

<details><summary>Before #195, and the measurements</summary>

Before #195 the analyzer exported no facts. For a callee in another package, `DirectiveFuncSet` parsed the callee's source file from disk. It looked for the directive in the doc comment of a declaration with the same name. So:

- A same-line directive and a generic instance (`GenF[int]`, whose name is not `GenF`) were missed.
- A contract that the body broke was still trusted.
- The `immutable-param` caller check never ran.
- `immutable-input(name)` was ignored.
- A package whose source the driver does not load had no directives at all.

Before #259 a generated `immutable-return` function that returned `db.Where(...)` was exported as trusted. Before #315 a doc-comment `pure` or `immutable-return` in a generated file was read from `FuncDecl.Doc` and trusted, while a same-line `pure` and every `immutable-input` were not. Before #314, `//gormreuse:immutable-return func DeclaredViaBad(db) { return BadConn(db) }` exported the fact when `BadConn` broke its contract. So did an `immutable-input(cb)` function that handed `cb` to a broken one.

CPU time of the command on `e2e/internal`:

| Build | CPU |
| --- | --- |
| Both shortcuts | about 2.8 s |
| Only the second | 4.6 s |
| Neither | 7.5 s |
| v0.22.0, without facts | about 1.6 to 2.4 s |

Measured again before 0.23.0, with every later change, the command used about 1.8 s of CPU there, and v0.22.0 about 1.6 s.

</details>

### Distrusted contracts

`distrustContracts` in `internal/distrust.go` decides which `immutable-return` and `immutable-input` functions, declared or inferred, another package must not trust (#314). `exportFacts` leaves their directives out of the facts. The package itself keeps trusting them, as [Directives across packages](#directives-across-packages) says.

The running example is `//gormreuse:immutable-return func DeclaredViaBad(db) { return BadConn(db) }`, where `BadConn` breaks its contract.

A function is distrusted when its body breaks the contract. It is also distrusted when it is checked again with the distrusted functions distrusted, and the check fails:

| Function | Checked again with | Fails when |
| --- | --- | --- |
| Declared `immutable-return` | `ImmutableReturnValidator.Distrusting` | A returned root is a call to a distrusted function. The tracer from `RootTracer.Distrusting` does not trust one, so such a call is a root, and the validator counts it as provably mutable |
| Inferred `immutable-return` | `purity.InferImmutableReturns` on the function alone | A returned call's callee is distrusted, so the proof no longer holds |
| `immutable-input(name)` | `ValidateImmutableInputs` with `distrusted` and the distrusting tracer | It hands the callback the result of a distrusted function. Or it hands the callback to a distrusted immutable-input function (an escape). Or it hands the callback the parameter of a callback that a distrusted immutable-input function calls |

A callback's parameter stays exempt only while every function that calls it as a callback keeps its promise (`callback.CollectImmutableInputProviders`). One that is also called by gorm's `Transaction` still gets a mutable value from a distrusted function.

<details><summary>The worklist, decisions and limitations</summary>

**The worklist.** It works like `validatePureFuncs`. The functions whose body breaks a contract are the seeds. When a function becomes distrusted, `distrustDependents` checks these again:

- The functions that call it or refer to it, in their body or in a closure inside it.
- The functions that create or are a callback it calls.

The sets only grow, and a larger set only fails more checks, so this ends at the same sets in any order. `distrustGroups` splits the functions into the strongly connected components of the dependents graph, in topological order, and the worklist runs one group at a time (#350). So a function outside a cycle is checked again at most once, after every function it depends on is settled. Before #350 it was checked again once per distrusted function it depends on, with a cold tracer each time. A hub that calls each of a chain of `n` such functions took 0.22 s at `n = 800` (`distrusthub`). It now takes 0.03 s. A package where no contract breaks skips it. `TestAllocationGrowth` (`distrustreturn`, `distrustinput`) holds a chain of `n` directive functions whose last breaks its contract to linear growth. Each function is in a file of its own there, since a directive lookup scans the comments of its file (#301).

Decisions and limitations:

- **The declaring package keeps today's rule.** Distrusting `DeclaredViaBad` at home too would report each reuse of its result, on top of the report at `BadConn`. The issue and the README promise only that another package does not trust it.
- **Only a reliance that the check can see.** Take a declared `immutable-return` function that returns the result of an unmarked helper that is not inferred. It still gets the benefit of the doubt, since the validator does not follow named helpers. An inferred helper is a target itself. So a helper that returns a broken function's result is distrusted, and so is the function that returns the helper's result.
- **A function in a generated file is not a seed.** Its directive is not trusted at all (#315). A function that relies on it is checked against an unmarked callee.
- **`pure` is unchanged.** A pure function's fact already carries `PureFailed`, which is transitive (#187). PIR (`pure,immutable-return`) that returns a broken function's result exports `Pure` without `ImmutableReturn`.

</details>

## Suggested fixes

`fix.Generator` builds the fix of each reused root. Every fix must keep the code's behaviour, compile on its own, and resolve its diagnostics in one pass.

| Root or diagnostic | Fix | Section |
| --- | --- | --- |
| A local root assigned to one variable | Reassign the discarded statements, and add `.Session(&gorm.Session{})` where a root is still used twice | [Reassignment fix target](#reassignment-fix-target), [Where Sessions go](#where-sessions-go) |
| Any other call root | `.Session(&gorm.Session{})` after the root's call | [Where Sessions go](#where-sessions-go) |
| A parameter root | `//gormreuse:immutable-param`, or a Session at the top of the body | [Parameter-root fix](#parameter-root-fix) |
| A mutable argument to an `immutable-param` parameter | `.Session(&gorm.Session{})` after the argument | [Contract and directive fixes](#contract-and-directive-fixes) |
| An unused or redundant directive | Delete the name | [Contract and directive fixes](#contract-and-directive-fixes) |

> [!CAUTION]
> A Session at a root makes later in-place changes hit a copy. The fixed code then drops a condition silently, and the linter is clean. So no fix is offered when it would lose one of these effects:
>
> - A discarded statement it cannot reassign ([Discarded statements](#discarded-statements-under-a-session-fix))
> - A change made outside the variable ([In-place changes](#in-place-changes-outside-the-variable))
> - `Attrs`, `Assign` or `InstanceSet` state that a Session drops ([Statement state a Session drops](#statement-state-a-session-drops))
> - The error of a query that a later read sees ([Errors left on the root](#errors-left-on-the-root))

<details><summary>Other diagnostics without a fix</summary>

| Diagnostic | Section |
| --- | --- |
| An alias use, or a later-change report of an argument | [Session and Debug copies](#session-and-debug-copies) |
| A tuple root | [Results the tracer cannot see through](#results-the-tracer-cannot-see-through) |
| A `FieldRoot` | [Struct roots](#struct-roots) |
| A root whose diagnostic an ignore suppresses | [Deterministic reporting](#deterministic-reporting) |
| A diagnostic in a cgo file | [Skipped files and internal errors](#skipped-files-and-internal-errors) |
| A mutable argument to an `immutable-param` function that discards a chain on its parameter | [immutable-param body rule](#immutable-param-body-rule) |

</details>

### Reassignment fix target

The reassignment fix (`q.Where("a")` → `q = q.Where("a")`) rewrites one variable: the one that holds the reused root (#198). `findReassignTarget` in `internal/fix/reassign.go` finds it from the root's own definition: `q := <call>`, `q = <call>` or `var q = <call>`. A root that is not a call, such as a Phi, gets no reassignment. A statement is rewritten only when its own top-level call is the use, and its receiver is an identifier that `types.Info.Uses` resolves to that variable.

No statement is rewritten when one of the cases below holds. The root gets a Session instead, unless a discarded statement would lose its effect. Then the diagnostic has no fix ([Discarded statements](#discarded-statements-under-a-session-fix)).

| Case | Example | Why |
| --- | --- | --- |
| The root is not assigned to one local variable | `r.db.Where(...)`, a package-level `DB` | There is no local to rebind |
| A use does not mention the variable | `r := q; r.Where("a")`, a method value `where := q.Where` | Rebinding `q` leaves that branch on the old root |
| A closure captures the variable, unless it is called inline | `go func() { q.Where("a") }()`, `defer func() { q.Find(nil) }()`, `fn := func() { ... }` | A goroutine that writes `q` races with its parent. A deferred or stored closure would see the new value |

<details><summary>Inline closures, and tests</summary>

An inline closure `func() { ... }()` runs in program order, so writing a captured variable from it is safe. A closure passed to a call, such as `Transaction`, `t.Run` or `wg.Go`, may run later or on another goroutine, so it counts as a capture. A variable declared inside such a closure is local to it, and is rewritten as usual.

`TestGoldenFilesCompile` type-checks every `.golden`. `testdata/src/gormreuse/fix_target.go` and `testdata/src/converge/fix_target.go` hold the cases.

</details>

<details><summary>Before #198</summary>

The old resolver took the receiver of the outermost method call around the use. Inside `DB.Transaction(func(tx) { ... })` that rewrote the package-level `DB`, and in `base.Or(q)` it rebound `base`.

</details>

### Discarded statements under a Session fix

A discarded non-finisher statement such as `q.Where("a")` or `q.Where("a").Limit(10)` has an effect only because `q` is mutable (#199). It changes `q`'s Statement in place, and every later query on `q` sees it. A Session at the root makes `q` immutable, so the same statement then changes a throwaway copy. The fix must therefore never leave such a statement in place. `findReassignedChains` in `internal/fix/reassign.go` applies this rule:

| Statement | Fix |
| --- | --- |
| A chain of non-finisher calls on the target variable: `q.Where("a").Limit(10)` | Reassign it as a whole: `q = q.Where("a").Limit(10)`. A Session it needs goes after its last call |
| A chain that starts with a Session-family call: `q.Session(s).Where("a")` | None needed. It never changes `q` |
| A chain with a Session-family call before its last call: `q.Where("a").Session(s).Limit(1)` | **No fix.** Reassigning would add `Limit(1)` to `q` |
| A receiver that is not the target variable: an alias, a method value, a captured variable, a use #68 reorders | **No fix** |

<details><summary>Examples of each statement</summary>

<table>
<tr><th>Statement</th><th>Fix</th></tr>
<tr><td>

A chain of non-finisher calls on the target variable.

```go
q.Where("a").Limit(10)
```

</td><td>

Reassign it as a whole:

```go
q = q.Where("a").Limit(10)
```

A Session it needs goes after its last call. One after `Where` would be undone by `Limit`, which returns a new mutable value.

</td></tr>
<tr><td>

A chain that starts with a Session-family call.

```go
q.Session(s).Where("a")
```

</td><td>

None needed. It never changes `q`.

</td></tr>
<tr><td>

A chain with a Session-family call before its last call.

```go
q.Where("a").Session(s).Limit(1)
```

</td><td>

**No fix.** Only `q.Where("a")` changes `q`. Reassigning would add `Limit(1)` to `q`.

</td></tr>
<tr><td>

A receiver that is not the target variable. Examples:

- An alias `r := q`.
- A method value `where := q.Where`.
- A variable captured by a closure that is not called inline.
- A use that #68 orders at a closure's call site.

</td><td>

**No fix.** The diagnostic stays without a suggested fix.

</td></tr>
</table>

</details>

<details><summary>Two more rules, and the SQL tests</summary>

Two more rules make the reassignment see every statement:

- **Loop violations read their uses late.** A loop violation is added while its use is tracked (`handler/call.go`). The uses after it in the loop body are not recorded yet. `CollectViolations` fills `Violation.AllUses` after tracking has finished. It used to be a snapshot taken at the use, so the fix never saw a discarded statement after the loop's query.
- **A reassignment in a loop gets a Session.** The simulation orders uses by position. It cannot see the next iteration's use of a value reassigned in the loop body. `findRootsNeedingSession` gives such a virtual root a Session whenever its block is in a CFG cycle.

`UsageInfo` carries the method name and the call's own position for this. `TestFixPreservesQueries` in `e2e/internal/fixsql_test.go` runs `e2e/internal/fixsql/before` and `fixsql/after` against real GORM, and compares the SQL of each query. `TestGoldenFilesE2E` in the main module regenerates each file of `after` from its file in `before` (`after.go` from `before.go`) with the current fixes. So the comparison always checks the fixes gormreuse suggests now. `TestFixPreservesQueriesDetectsSessionOnly` checks that the comparison fails for the old Session-only fixes.

</details>

Limitations:

- A discarded statement whose effect no later query sees still blocks the fix. An example is a final `q.Where("after")` when a stored closure captures `q` (`whereOnlyStoredClosure` in `evil.go`). Proving that no later use exists would need alias and loop analysis.
- Other ways of keeping a chain's effect away from its root, such as `_ = q.Where("a")`, are not treated as discarded statements.

### Where Sessions go

After the reassignments, `simulateVirtualRoots` decides which root each use reads (#202). `findRootsNeedingSession` gives a Session to each root, original or created by a reassignment, that is still used twice.

| Rule | Detail |
| --- | --- |
| Conflict | Two uses of one root are a reuse when the earlier one can reach the later one (`usesConflict`). A reassignment does not conflict with the uses after it: on any path through it, they read the new value |
| Which reassignment a use reads | Each earlier reassignment that can reach it, back to the first whose block dominates it, and the original root when none does (`virtualRootReaches`) |
| Block order | The uses of one block are taken in the order the block runs them (`runOrderedUses`). They are not taken by position. go/ssa puts a `for` statement's post statement at the end of the body's block. So `for i := 0; i < 2; q.Where("a") { q.Find(nil); i++ }` reads the root in `q.Find` before the post statement reassigns it (#247) |
| Loops | A root used again in a loop that does not hold its definition, other than by a reassignment, gets a Session (`usedAgainInLoop`). That holds for a root created by a reassignment as for the original one (#311). So does a reassignment in a loop that does not define the root again: the next iteration reads it. Both ask `cfg.Repeats`, as the checker does, and so does `changeSeenAfterEscape` |
| A root that flows into a Phi | `sessionPositionsForRoot` always puts a Session on the root itself, and adds the other edges when the Phi is used twice |
| A chain of Phis (#343) | Each conditional filter adds a Phi that takes the one before. `mergedEdges` follows the Phis forward from the root, and back from each one used twice, down to values that are not Phis (`mergingPhis`, `phiMergedEdges`). A parameter found there gets its parameter fix (`mergedParamFixes`). A value found only by the walk back is followed only when it is merged or chained on and nothing else (`mergeOnly`). No Session is written when any merged value may carry Attrs, Assign or InstanceSet state (`mergedDropsState`, #344) |
| Already immutable | A reassignment whose chain already ends in an immutable call, by the tracer's `IsImmutableCall`, gets no second Session (`virtualRootEndsImmutable`) |

<details><summary>A loop example, the every-use check, and the one-pass cases</summary>

`q.Where("t"); for ... { q.Where("id = ?", id).Find(nil) }` reassigns `q = q.Where("t")`, and every iteration reads it. Before #311 the loop rule was asked of the original root only. The first pass then left each query adding its id to the ones before.

**Every use must be known.** These rules need every use of the root, in the function that defines it. `usesOrdered` checks that:

- The tracker records no use outside `AllUses` (`Violation.HiddenUses`): no deferred or spawned call, no read through a Session/Debug copy or a subquery, no loop-carried read.
- The reported position is one of `AllUses`. A use through a Phi is filed under one edge only, so a diagnostic for another edge can have no use of its own.
- Every use is in the defining function. A use in a closure, such as the body of a range-over-func loop, has no CFG path from the definition.

When the check fails, a root gets a Session when it has a use other than the one reassignment that reads it. That is what the fix did before. Removing a Session from a reassignment never drops an effect: the statement is still reassigned, so #199 holds. `testdata/src/gormreuse/fix_paths.go.golden` pins the output. `TestFixesConverge` re-lints that golden. `TestFixPathsPreserveQueries` in `e2e/internal/fix_paths_test.go` compares the SQL before and after the fixes.

**One pass (#247).** Three more cases needed a second pass, or hid a reuse:

| Case | Example | Change |
| --- | --- | --- |
| A variable that a closure captures has no Phi: SSA keeps it in memory, and the tracer resolves every read to its first store. The other stores were meant to get a Session from a synthetic Phi, but its result had no referrers, so they never did. The re-lint was then clean, because it reads only the first store, while the other path still reused its root | Two stores, `q = db.Where("a")` and `q = db.Where("b")`, read twice in a `go` closure. Only `"a"` got a Session | `mergedEdges` gives a Session to each other store that may run before two of the root's uses. A use in a closure counts as after every store. `TestFixCapturedMergeNeedsEveryStore` in `e2e/internal/fix_one_pass_test.go` checks the leak at run time |
| A reassignment inside a closure writes the captured variable, and detection does not follow a store in a closure (`closureModifiesCaptured`). The uses after it still read the root | `func() { q.Where("a") }(); q.Find(nil); q.Count(nil)`, and the body of a range-over-func loop | The root gets a Session in the same pass (`reassignsInClosure`) |
| Only the first diagnostic at a position was reported. A read-only argument that sees a change at the position of a reuse appeared only after the reuse's fix | `q = q.Where(r); q.Count(nil); r.Find(nil); q.Find(nil)` | A position has one reuse diagnostic and one of each other message (`reportKey` in `internal/report.go`) |

`testdata/src/converge/fix_multipass.go` and `testdata/src/gormreuse/fix_one_pass.go` hold the cases.

</details>

<details><summary>Chains of Phis (#343)</summary>

```go
q := DB.Model(&User{})
if a != "" { q = q.Where("a = ?", a) } // Phi1(Model, Where a)
if b != "" { q = q.Where("b = ?", b) } // Phi2(Phi1, Where b)
q.Count(&n)                            // reads Phi2
q.Limit(10).Find(&us)
```

Before #343, only the edges of the first Phi got a Session. That made `Where("b")` a new mutable root, so each filter took one more pass. The code after the first pass failed at run time: Count's `count(*)` leaked into Find (`sql: expected 5 destination arguments in Scan, not 1`). After one pass, 10 of the 38 fixable reports in new-api and 35 of the 46 in gin-vue-admin still had a fix to apply.

| Step | What it takes |
| --- | --- |
| Forward from the root | The root when it is a Phi, the Phis that take it, and the Phis that take one of those |
| Back from each one used twice | Its edges, and the edges of a Phi among them, down to values that are not Phis |

The walk back is needed because the reported root is not always the first value. With a loop between the filters, the root is the last filter's `Where`. The Model and the first filter reach it only through the loop-header Phi.

The fix's checks (`changedElsewhere`, the discarded statements of `findReassignedChains`) see the uses of the root and of the Phis forward from it. They do not see the uses of a value that only the walk back finds. `mergeOnly` accepts such a value only when each referrer is a Phi, or a `*gorm.DB` method with the value as its receiver. The method must run a query, return a copy, or return a value that the same rule accepts. Take this code:

```go
if a != "" { q = q.Where("a") }
applyTenant(q)
for … { q = q.Where(x) }
if b != "" { q = q.Where("b") }
```

A Session on the Model would make `applyTenant` add its filter to a copy. The walk stops at the Phi that `applyTenant` takes.

Rejected: one `q = q.Session(&gorm.Session{})` before the first use of the last Phi. The statement must come before every use after the merge, in source order. SSA's blocks do not give that order back, and the rest of the fix appends to calls instead of adding lines.

Fixtures: `testdata/src/converge/phi_chain.go` has 2, 3 and 5 filters on a local root, a parameter, and a root on a package-level handle, with loops mixed in. `testdata/src/gormreuse/fix_phi_chain.go` has the values where the walk back stops. `e2e/internal/phi_chain_test.go` runs `fixsql/before/phi_chain.go` against real GORM. `filterloops` is in the growth gate. After one `-fix` pass over new-api and gin-vue-admin, no reuse diagnostic with a fix remains, and both still build.

</details>

<details><summary>Uses of a Phi filed under another edge (#373)</summary>

The tracker files a use of a Phi under one of its edges. So the fix of another edge does not list it, and a Session on that edge could make it change a copy, or leave it reusing a value in the next pass:

| Case | Example | Change |
| --- | --- | --- |
| A discarded statement on the Phi | `for … { if c { q = q.Where("a") } else { q.Find(&us) }; q.Where("b") }`. The fix of the `Where("a")` gave it a Session, and `q.Where("b")` changed a copy | `reassignMergedChains` reassigns each discarded chain on the root, a Phi that takes it, or an edge of such a Phi (`mergedDiscardedChains`). One it cannot reassign withholds the fix |
| The same on a parameter's variable | The same loop with `q` a parameter. The parameter got its body Session from the `Where("a")`'s fix (`mergedParamFixes`) | The #312 rule holds for the reassignments of any root held by a parameter's variable (`paramOfVar`), not only for the parameter itself. A mutable `Where` returns the caller's value, so its statements change the caller's value too |
| A Phi used once by a use that repeats | `for … { q = q.Where("b") }; for i := range 2 { q.Where("i = ?", i).Find(&us) }` | `mergeUsedTwice` counts a use that `cfg.Repeats` runs again |
| A reassignment that takes the root's place on a Phi's edge | `if c { q.Where("a") } else { q.Find(&us) }; for … { q = q.Where("b") }; for … { q.Find(&us) }` | `mergedReassignmentsNeedingSession` gives a Session to each reassignment that reaches a Phi used twice, other than the one it reads (`mergeReassignmentReachesTwice`). `rootSessionSites` then isolates the Phi's other edges, as for the root |
| A use that reads the root through a back edge | `for … { q.Where("o"); for … { q.Where("x").Find(&us); if c { q = q.Where("r") } } }`. The reassignment of `q.Where("o")` dominates `q.Where("x")`, which still reads the `Where("r")` of the iteration before | `virtualOriginalReaches` adds the use to the root's uses when a path from the root's definition reaches it without passing a reassignment |

Fixtures: `testdata/src/converge/merged_one_pass.go` holds the local roots, each fixed in one pass. `testdata/src/gormreuse/fix_merged_param.go` holds the parameters, which get no fix. `TestFixMergedParamPreservesQueries` in `e2e/internal/merged_param_test.go` runs `fixsql/before/merged_param.go` against real GORM. `TestFixMergedParamDetectsOldFixes` and `TestFixMergedLaterLoopOnePass` check that the fixes suggested before lose a condition or leak one.

</details>

Limitations:

- The rules follow the CFG of one function, not the run-time order of deferred calls or closures. Those roots get the conservative Sessions above, some of them redundant.
- A reassignment in a `for` statement's post statement is ordered before the body, so its root needs a second pass (`onePassForPost`, see [Known Limitations](#known-limitations)).
- A Phi edge with no call of its own still gets no Session ([Fixes that compile](#fixes-that-compile)). A parameter edge gets its parameter fix (#343).
- The walk back through a Phi chain is conservative: a value that is also read by `fmt.Println`, or whose `Error` is read, stops it. Such a root may need another pass (#343).

<details><summary>The three defects #202 fixed</summary>

They made `-fix` add Sessions it did not need, miss ones it did, or need a second pass.

| Defect | Example | Change |
| --- | --- | --- |
| Every receiver use was recorded twice: by the receiver handler, and by the argument handler, which did not skip `Args[0]`. A reassigning call then counted as a use of the value it creates | `q.Where("a"); q.Where("b").Find(nil)` became `q = q.Where("a").Session(...)` | `handleArgs` skips the receiver of a gorm method (`Site.ArgParam`). Detection is unchanged: the copies had the same block and position |
| The simulation took uses in position order, so a use after an `if` without an `else` read only the reassignment in the `if` | `if c { q.Where("a") }; q.Where("b").Find(nil); q.Where("c").Find(nil)` left the root mutable on the path where `c` is false | `virtualRootReaches` |
| A root that flows into a Phi got Sessions only on the Phi's edges, and none when the Phi is used once | `q := db.Where("x"); q.Find(nil); q.Count(nil); if c { q = db.Where("z") }; q.Find(nil)` had no fix. With `q` captured by a closure, the synthetic Phi from its stores had the same result | `sessionPositionsForRoot` |

</details>

### Method values and fixes

A second branch made through a method value (`w := q.Where; w("a").Find(nil); w("b").Find(nil)`) gets `.Session(&gorm.Session{})` at `q`'s root (#178). A call through `w` names no variable to rebind, so it never gets a reassignment.

<details><summary>Discarded calls through a method value, and tests</summary>

- When one call is a discarded statement (`w("a")` or `w("a").Limit(1)`), it changes `q` in place, and a Session at the root would drop that change. `discardedChainToReassign` returns no receiver for it, so no fix is offered, as for any discarded chain that cannot be reassigned (#199).
- A root created by a call through a method value (`q := w("a")`) gets `.Session(&gorm.Session{})` appended.

`e2e/internal/fixsql` (`MethodValue`, `MethodValueDiscarded`) checks both against real GORM.

</details>

### Parameter-root fix

A parameter root has no call to append a Session to. `Generate` in `internal/fix/generator.go` chooses between the directive and a Session at the top of the body (#201). The checks are in `internal/fix/param.go`.

| Check | Fix |
| --- | --- |
| `canFixParam` fails: a Scopes/Preload callback, a closure with a `*gorm.DB` result, or a function declaration with a `*gorm.DB` result that `calledOnlyDirectly` rejects | None |
| `paramCallersIsolate` holds. The function is a declaration. The pass sees every file of the package (`reportSeesWholePackage`). `calledOnlyDirectly` holds. The function does not change a parameter in place (`ImmutableParamRule.ChangesInPlace`, the inference's `inferChangesInPlace`), and every call to it in the analysed functions passes values proved immutable (`ImmutableParamRule.ProvesArgs`, `tracer.RootTracer.ProvesImmutableArgs`) (#387). `handler.BreaksImmutableParam` holds for no argument of any call | `//gormreuse:immutable-param`. Such a function is often [inferred](#inferred-immutable-param-and-immutable-input) and not reported (#349). A use of the parameter that is a discarded chain (`paramDiscardsChain`) withholds the directive, since the immutable-param body check would report the chain (#346) |
| Otherwise | `tx = tx.Session(&gorm.Session{})` before the body's first statement (`paramSessionSite`), then the rest of the local-root fix. It is written by `sessionEdits`, with the same gorm name resolution and import as every other Session |

<details><summary>Callers, test variants, merged values, discarded statements, Scopes callbacks and closures</summary>

**The callers.**

- `paramCallersIsolate` checks every argument of each call with the caller-side check's own predicate, not only the argument of the reused parameter. Declaring the function covers all its `*gorm.DB` parameters. It scans the analysed functions (`runFuncs`) and the package `init`, so a caller in a package-level closure or initializer counts (#183, #245, #346).
- It also asks the rules of the [immutable-param inference](#inferred-immutable-param-and-immutable-input), which `run.go` hands it as `fix.ImmutableParamRule` (#387). Each argument must be proved immutable (`ProvesImmutableArgs`, which `inferCallersPassImmutable` asks too), and the function must not be in `inferChangesInPlace`, which `inferImmutableParams` computes once and returns for the fix. Before #387 the fix used only the caller-side check and `paramDiscardsChain`. It declared `func list(tx *gorm.DB) { applyTenant(tx); tx.Find(&us); tx.Count(&n) }` immutable-param, which promises an immutable `tx` on which `applyTenant`'s filter changes a copy. It declared a function whose caller passes `ctx.Value(k).(*gorm.DB)` too, which hides the reuse of a value that may be mid-chain. A function with no caller still gets the directive: no call breaks it. After a panic in the inference, every function is taken to change its parameter. `e2e/internal/in_place_param_test.go` runs `fixsql/before/in_place_param.go` against real GORM.
- `calledOnlyDirectly` needs an unexported function, not a method (a method can be called through an interface). Every reference must be the function of a call (`f(x)`, `f[T](x)`, `(f)(x)`).
- In the Session case the parameter is the reassignment target (`varToReassign` returns its variable). So a discarded `tx.Where("a")` becomes `tx = tx.Where("a")` and keeps its effect inside the function. The Session goes at the top of the body, so it also covers uses through an alias.

**Test variants.** A package with test files is analyzed as two variants: one without its `_test.go` files, and the test variant. `-fix` applies the fixes of both. The variant without the test files cannot see their callers. If each variant decided from the callers it sees, one could add `immutable-param` and the other the body Session, and the result reports the directive redundant. So take a package with test files. A function outside its test files always gets the body Session, in both variants (`reportSeesWholePackage`, the rule `reportIgnoreRemovable` uses for #248). A function in a test file is analyzed only by the test variant, which sees every caller, so it may get the directive. The cost is a body Session where the directive would also have worked. `TestParamFixWithTestFiles` and `testdata/src/paramtests` check it.

**Merged values (#311).** Neither fix makes a value that the parameter merges with immutable. In standard pagination, `if f.Name != "" { db = db.Where("name = ?", f.Name) }` is followed by `db.Count(&total)` and `db.Offset(10).Limit(10).Find(&users)`. The Phi of `db` and the `Where` is read twice, so Count's Select and Model leak into Find, which then fails to scan. The `Where` gets a Session, as an edge of a local root does (`mergedSessionPositions`, see `sessionPositionsForRoot`). With the body Session it is one more Session site. With the directive, the rest of the local-root fix runs for those Sessions, and the directive alone is the fallback when that fix offers none.

**Discarded statements on a parameter (#312).** A discarded statement on a parameter also changes the caller's value. A helper such as `func applyTenant(tx *gorm.DB) { tx.Where("tenant_id = ?", t) }` exists for exactly that. Reassigning it keeps the change inside the function only. It also makes the helper lint-clean, so nothing reports the change a caller's Session then drops (#243). `Generate` therefore offers no Session-case fix for a parameter with a discarded statement, with one exception. The function has a `*gorm.DB` result that can carry the change back to the caller, and every caller reads it.

- `everyCallUsesResult` checks each call in the analysed functions and the package `init` (#346). Its result, or a `*gorm.DB` element of its tuple, must have a referrer. A go or defer call has none.
- `canFixParam` already requires `calledOnlyDirectly` for such a function, and the pass must see every file of the package, as for `paramCallersIsolate`. So no call is missed, and both variants agree.
- A caller may read the result but go on using its argument, as in `r := applyTenant(q); q.Find(&a); r.Find(&b)`. That is not checked: only the result carries the change.
- The rule holds for the root of any value that the parameter's variable holds, such as a `Where` merged with the parameter in `if admin { q = q.Where("admin") }`. On a mutable parameter the `Where` returns the caller's value itself (#373).

`TestFixHelperResultPreservesQueries` in `e2e/internal/fix_helper_result_test.go` runs the cases of `fixsql/before/helper_result.go` against real GORM: a caller that discards the result and one that reads it. `TestFixHelperResultDetectsReassignment` checks that the fix suggested before drops the discarding caller's tenant filter.

**Scopes callbacks.** A function that returns a `*gorm.DB` and may be called by unknown code may be a Scopes callback. A Session result returned from one leaks gorm's transaction (go-gorm/gorm#7592), and the fix does not tell which results are Session results. It gets no fix.

**Closure parameters (#313).** A closure parameter gets the body Session under the same rules.

- A closure has no name, so `calledOnlyDirectly` cannot hold. One with a `*gorm.DB` result may be a Scopes callback: it gets no fix, as an exported function with one does.
- A closure never gets `immutable-param`. Placing a directive on a literal is left to the user. Its callers, such as the function a callback is passed to, are not calls the caller-side check can see.
- The body Session needs only the closure's own body. It fits a callback, an IIFE, a `go func(tx *gorm.DB) { ... }(q)`, a local closure and a package-level one alike.
- In a Transaction callback, the Session keeps the Statement's connection, which is the transaction (`TestClosureParamSessionKeepsTransaction`).

</details>

Limitations:

- The body Session also stops the function from changing the caller's Statement in place, as `tx.Where("a").Find(nil)` does on a mid-chain value. That change is the reuse being fixed.
- A parameter's body Session still drops the in-place changes of the function's queries, such as the errors in the caller's `q.Error` after `helper(q)`.

<details><summary>Phi edges, and tests</summary>

**Phi edges.** `edgeSessionPositions` skips an edge that is already immutable. It asks `RootTracer.IsImmutableCall`, the tracer's own rule. The edge's last call must be an immutable-returning builtin or an `immutable-return` function, and a Session that may set `Initialized` does not count. `TestSessionMidChainEdgeAccumulates` checks at runtime that such an edge is mutable.

Tests:

| Test or fixture | Checks |
| --- | --- |
| `TestParamSessionIsolatesBranches` in `e2e/internal/param_session_test.go` | The body Session at runtime |
| `testdata/src/converge/fix_param_root.go` | One pass converges |
| `testdata/src/gormreuse/fix_later_loop_merge.go` | The merged parameters and the reassignments read in a later loop. Its golden is re-linted by `TestFixesConverge` |
| `TestLaterLoopMergeStopsLeaks` in `e2e/internal/later_loop_merge_test.go` | Runs them against real GORM, and checks that the fixes of the first pass before #311 still leak |
| `testdata/src/gormreuse/fix_closure_param.go` | The closure parameters, re-linted by `TestFixesConverge`. `fix_param_root.go` holds the closures without a fix |
| `e2e/internal/closure_param_test.go` | The `ClosureParam` cases of `fixsql/before/closure_param.go` against real GORM |
| `testdata/src/gormreuse/inferred_immutable_param.go` | The functions that inference refuses get no directive fix either (#387): `ipListViaHelper` and `ipListViaFuncValue` get no fix, and `ipCtxValue`, `ipClosureStore`, `ipFromChainGlobal` and `ipFromDeclared` the body Session |
| `TestFixInPlaceParamPreservesQueries` in `e2e/internal/in_place_param_test.go` | Runs `fixsql/before/in_place_param.go` against real GORM: the tenant filter that a helper applies in place stays, and an unproved caller's body Session stops the leak |

</details>

<details><summary>Before #201, #311 and #313</summary>

`Generate` used to declare the function `//gormreuse:immutable-param` for every parameter root. That moved the duty to the callers, but:

- Callers in other packages are never checked (#195), so an exported function's reuse disappeared.
- A function used as a value, such as `var scopes = []func(*gorm.DB) *gorm.DB{scope}`, has callers no check sees, so a real Scopes-callback reuse disappeared.
- A caller in the package that passes a mid-chain value got the caller-side diagnostic, which had no fix, so `-fix` never reached zero. Since #248 that diagnostic has a fix of its own, which isolates the argument ([Contract and directive fixes](#contract-and-directive-fixes)). The parameter-root fix still uses the body Session for such a function, which fixes every caller at once.

Before #311 both parameter fixes needed a second pass for merged values. `mergedEdges` read only a root that is an instruction, and the directive was returned before the edges were asked.

`edgeSessionPositions` used to call an edge immutable when a Session-family call appeared anywhere in its chain. So `db.Session(&gorm.Session{}).Where("y")` and `db.WithContext(ctx).Where("y")` got no Session, while detection still reported them: the diagnostic stayed after every pass.

In gorm's own `tests/` module, 8 of the 9 diagnostics without a fix were closure parameters: callbacks of Transaction wrappers and of `Connection`. After #313 four of them get the body Session. The fixed tests compile, re-lint clean of them, and log the same SQL. The other four belong to one root, a callback parameter handed to another wrapper call and used after it, which [#243](#in-place-changes-outside-the-variable) still withholds.

</details>

### In-place changes outside the variable

The #199 rule covers a discarded statement written on the root's variable. Other code can change the root's Statement in place too, and a Session at the root makes it change a copy (#243). The tenant filter of `applyTenant(q, 7); q.Find(&users)` then disappears, and the re-lint is clean. `changedElsewhere` in `internal/fix/change.go` withholds the whole fix when:

| Case | Example | How it is found |
| --- | --- | --- |
| The root is handed to other code, and a later use may see the change | `applyTenant(q)`, `apply(q)` for a closure, `_ = withTenant(q)`, `qs := []*gorm.DB{q}`, `m["q"] = q`, `ch <- q` | `UsageInfo.Escapes`, set on the branch of a call argument, a slice or map store or a send (`handOn`), and by `q = helper(q)`. `changeSeenAfterEscape` decides whether a later use exists |
| The `Error`, `RowsAffected` or `Statement` field is read from the root, or from a chain on it that runs no query | `return q.Error`, `if err := q.Where("a").Error; ...`, `q.Where("a").Limit(1).Statement` | A `FieldAddr` in the root's function or its closures, whose operand `changeCarryingRoots` walks down a chain of non-finisher, non-Session-family `*gorm.DB` methods to the root |
| A go statement hands the root to a call | `go applyTenant(q)`, `go q.Where("a")` | An `*ssa.Go` whose argument traces to the root (`statementRootChanges`). The goroutine may change it while the uses after the statement run |
| A defer statement changes the root in place, and another deferred call may see it (#309) | `defer q.Find(nil); defer q.Where("a")`, `defer applyTenant(q); defer q.Find(nil)`, `for … { defer applyTenant(q) }` | An `*ssa.Defer` that `statementRootChanges` says changes the root, when another `*ssa.Defer` uses the root, or the statement repeats (`useRepeatsAfter`). Deferred calls run after every other use, so only a deferred call sees the change. Their run order is not followed, so any other one is taken to |
| The root is stored in a struct field that other code reaches (#310) | `s := &Svc{db: q}; s.apply()`, `applyP(params{db: q})`, `p.db = q; helper(&p)` | `holderChangesRoot`, over the stores that `holderStores` finds ([Struct stores](#struct-stores)) |

These scans are indexed once (#350):

| Index | Per |
| --- | --- |
| The state reads and the go and defer statements of a function, by root (`changeStatementsOf`) | Function |
| The calls of a file by Lparen (`callAtLparen`) | File |
| The variables that calls are assigned to (`reassignLHS`) | File |
| The variables that each function's closures capture (`closureCaptures`) | Function |

<details><summary>Before #350</summary>

Each was scanned again for every root. So `n` roots, each used in a deferred call, took 0.33 s at `n = 800` (`rootsdefer`). They now take 0.02 s.

</details>

#### Helpers that only run queries

An escape to a helper that only runs queries on the root changes it only by the pollution of those queries (#347). That is the reuse the fix removes. In application code every root left without a fix was of this kind: a `*gorm.DB` handed to a same-package helper and used again. `handoffKeepsChanges` in `internal/fix/handoff.go` lets `changedElsewhere` skip such an escape:

| Escape | Kept fix when |
| --- | --- |
| A call of a finisher-only helper | Each `*gorm.DB` argument goes to a parameter that `HandoffFinisherOnlyParams` lists |
| A recursive call of the parameter's own function | The root is that parameter, the argument goes to the same parameter, and the fix reassigns no discarded statement. The body Session rebinds the parameter in every call, so the callee changes only its own copy |

Both need a plain static call (no go or defer statement, method value or method expression) of a function that returns no `*gorm.DB`. No other argument may hold the root (`PointeeRoots`, `GormDBFieldValues`) or code (a function, an interface or a type parameter, in a field or element too).

`HandoffFinisherOnlyParams` summarises a function once per pass (`Generator.handoffs`). A parameter is listed when each of its uses starts a chain of `*gorm.DB` methods on the receiver. The chain must end in a finisher, or in `Session`, `WithContext` or `Debug`. These uses rule a parameter out:

| Rejected | Example |
| --- | --- |
| A discarded statement, also after a finisher | `tx.Where("a")`, `tx.Find(&x).Where("a")` |
| A state-field read other than the `Error` or `RowsAffected` of a finisher's result | `tx.Error`, `tx.Where("a").Error`, `tx.Find(&x).Statement` |
| Any use other than as the receiver of the next method | A store, a return, a Phi, a closure capture, an argument of another call |
| A method that takes a function, also boxed or in a variadic argument, or an interface whose value is unknown | `tx.Scopes(f).Find(&x)`, `tx.Transaction(fn)` |
| `AddError` and the other state-only methods | `tx.AddError(err)` changes the caller's `Error` |
| A `*gorm.DB` result | It could carry the parameter back |

The summary of an exported function travels as `facts.Func.FinisherOnly`, so a helper in another package counts too (`purelib/handoff.go` and `externalpure/handoff_use.go`). `testdata/src/gormreuse/fix_handoff.go` holds the cases with and without a fix. `TestFixHandoffPreservesQueries` in `e2e/internal/handoff_fix_test.go` runs `fixsql/before/handoff.go` against real GORM: the leaked conditions are gone, and every query keeps its own.

<details><summary>go statements, and what keeps the fix</summary>

**go statements (#312).** The receiver of a finisher or a Session-family call, as in `go q.Count(nil)`, is not handed over. The call is a use like any other. It runs on a copy once the root has a Session, as in `go func() { q.Count(nil) }()`. It is found by `Site.ArgParam`, since a static method call's arguments start with the receiver. A chain method's receiver is handed over: the goroutine changes the root in place, and a go statement cannot be reassigned.

**What keeps the fix.**

- `changeSeenAfterEscape` keeps the fix of an escape that nothing can observe. That is the last use of the root, in no loop, when every use is in order (`usesOrdered`). `q.Find(&a); render(q)` keeps its Session.
- An argument of a call that returns a `*gorm.DB`, other than a `*gorm.DB` method, always counts. `q = withTenant(q)` may return `q` itself, so the uses of its result see the change. A `*gorm.DB` method such as `base.Model(q)` returns a chain on its receiver.
- Read-only uses do not escape: a subquery (`RecordPureUse`), a `//gormreuse:pure` function, and the `fmt`/`log`/`testing` variadics.
- The field read on a finisher's result, as in `q.Where("a").Find(&x).Error`, keeps this check's fix. On a mutable root it also holds the errors of the queries before it, which [Errors left on the root](#errors-left-on-the-root) checks (#372). A read of the root's own field counts wherever it is, even before the first use. Telling the two apart needs ordering against the closures that may read it.
- A reassignment-only fix (`q = q.Where("a")` with no Session) would not change behaviour, since a mutable root's method returns the root itself. It is withheld too: the check runs before the fix is built, and a reassignment whose chain ends in a Session does change the root.

</details>

#### Struct stores

A store of the root into a struct field is not a use (#310). The call that is handed a local struct is also a branch of the root in detection (#341, [Pollution sources](#pollution-sources)). Detection reads the fields of a local struct as the value stored in them (#181), so `s.db.Find(&x)` is a use of `q`. The handler marks nothing at the store either, so `UsageInfo.Escapes` does not cover it. Without the check below, `s := &Svc{db: q}; s.apply(); q.Find(&a); q.Find(&b)` would get a Session at `q`. Then `apply` would add its filter to a copy.

<details><summary>Where other code reaches a stored root, and tests</summary>

`changedElsewhere` also checks each store of the root, or of a chain on it that runs no query (`changeCarryingRoots`). It checks a store into anything other than a slice, array or map element. A store into a local variable counts too: code that the variable's address is handed to reaches the root, as in `applyAll([]**gorm.DB{&q})` (#345). Each point where other code can reach the stored root is an escape for `changeSeenAfterEscape`:

| Memory | Where other code reaches it | Found by |
| --- | --- | --- |
| A local variable, struct or array (an `Alloc` reached through field and array element addresses) | Where it is handed on. That is a call that takes it, also as a receiver. It is also a copy of its value that goes anywhere but another local struct. It is also such an instruction in a closure that captures the struct or a variable holding its address. A read of a `*gorm.DB` field, a copy into another local struct and a Phi are followed, not escapes | `holderWalk`, once per struct |
| Other memory: a package-level variable, or a struct that a parameter or another pointer points to | At any call that may run code after the store (`holderCallChanges`, below). The first such call in the store's block is the escape. A call in a later block is taken as running at the end of the store's block. A local struct copied into such memory is reached the same way after the copy | `holderNextCall` over `holderFuncCalls`, once per function |

- What counts as a call that may run code (#345):

  | Instruction | Counts when |
  | --- | --- |
  | A `*gorm.DB` method | An argument other than the receiver can hold a function. `Connection`, `Transaction`, `FindInBatches` and `ToSQL` run theirs before they return. `Scopes` keeps its scopes for the finisher, so the `Scopes` call counts in its place, which is earlier |
  | A static call into a package that does not reach gorm | An argument can run code of a package that does (`holderArgRunsCode`). That is a function, an interface, a type parameter, or a value of a type with methods declared in such a package. It may be held directly or in a field, element or pointer. A value boxed in an interface is judged by its own type, and so is each value of a variadic argument built at the call. So `sort.Sort(byTenant(xs))` counts, and so does `json.Marshal(v)` when the type of `v` has methods. `fmt.Println(n)` does not |
  | A call into `sync` or `sync/atomic`, a channel send or receive, a `select`, `close` | Always. They let a goroutine that is already running go on, and it may reach the memory |
  | Any other call, a dynamic call, a `go` statement | Always |
  | A deferred call, another builtin | Never. A deferred call runs after every use |

- A call to a `//gormreuse:pure` function hands nothing on, as in `paDeref(&q)`.
- Code in a closure of the root's function is not ordered against the uses. An escape there, or a store there into memory that is not a local struct, is taken as seen.
- The stores, the walks and the calls are memoized in `Generator.holders`, so a function with n such stores stays linear (`structescape` in the growth gate).
- The rule withholds no fix that the existing golden fixtures had. Their struct stores go to one of these:
  - A struct the function only reads.
  - A package-level variable or receiver field with no call between the store and the uses (the builder idiom `r.db = r.db.Where(c)`).
  - A captured struct whose closure only reads the field.

Tests: `TestFixEscapePreservesQueries` in `e2e/internal/fix_escape_test.go` runs each case of the issue from `fixsql/before` and `fixsql/after` against real GORM. `TestFixEscapeDetectsSessionAtRoot` checks that the comparison fails for the fixes suggested before. `e2e/internal/fix_struct_escape_test.go` does the same for the struct cases of #310, including a parameter root whose body Session would drop the change. In `ErrorPropagation` a query runs only when `q.Error` holds the error of a failed query, so a lost error shows up as a missing query. `e2e/internal/fix_pointer_escape_test.go` does the same for #345. It hands on the address of `q`. It also stores `q` in a package-level struct before `Connection`, `Scopes` or `sort.Sort` runs code that reaches it. `testdata/src/gormreuse/fix_escape.go`, `fix_escape_struct.go` and `fix_escape_pointer.go` hold the fixtures with and without a fix.

</details>

Limitations:

- An escape before a later use withholds the fix even when the callee only reads the value. The exception is a callee that only runs queries on it ([Helpers that only run queries](#helpers-that-only-run-queries)). An example is a `*gorm.DB` method argument (`base.Model(q)`). Mark a read-only helper `//gormreuse:pure`, or pass it an isolated value.
- A parameter's body Session still drops the in-place changes of the function's queries ([Parameter-root fix](#parameter-root-fix)).
- A deferred call that takes the root, as in `defer applyTenant(q)`, keeps the fix when no other deferred call uses the root and it runs once. It runs after every use the fix orders, so only another deferred call could see its change. With another one, the fix is withheld even when that one runs first and cannot see it.
- A `*gorm.DB` method can also run code through the model's hooks (such as AfterFind), a driver.Valuer or a registered callback. These are not counted: gorm runs them on every query, and counting them would withhold the fix after any query.
- A goroutine that changes the memory without synchronizing with the function is a data race, and is not counted. Neither is a sleep nor a yield to the scheduler.
- A struct store is withheld more often than needed. A struct handed to a callee that only reads it (`log(params{db: q})`) counts as an escape. So does any call between a store into a package-level variable and a later use. A call in another block after such a store counts even when it runs after the last use.
- A struct reached through a pointer field of a local struct (`s.inner.db = q` with `inner *T`) is other memory, so every later call counts.

### Statement state a Session drops

The next call on a Session result clones the Statement, and gorm's `Statement.clone()` does not copy `attrs` (`Attrs`) or `assigns` (`Assign`). An `InstanceSet` entry is copied, but its key holds the old Statement's address, so `InstanceGet` on the clone misses it (#344). A Session after them changes what `FirstOrInit` and `FirstOrCreate` do: `q := db.Model(&User{}).Assign(User{Age: 77})`, reused with a Session, leaves `Age` at the row's value.

`RootTracer.SessionDropsState` (`internal/ssa/tracer/state.go`) says a value may carry such state in these cases:

- Its own chain, back to a Session-family call, calls one of the three.
- A chain on it calls one, and the value keeps the effect: a discarded `q.Assign(x)`, a merge back, or any use but a `*gorm.DB` receiver.
- It is a parameter that reaches `FirstOrInit` or `FirstOrCreate`.

For such a value, `Generate` withholds the fix whenever it would write a Session (root, reassignment, merged edge, parameter body). `GenerateIsolation` withholds the isolation. A fix with only `immutable-param` stays. The diagnostics end with "a Session would drop its Attrs, Assign or InstanceSet" in place of the Session advice.

<details><summary>Every field clone() leaves out, decisions, limitations and tests</summary>

| Field | Set by | Effect of the drop |
| --- | --- | --- |
| `attrs` | `Attrs` | `FirstOrInit`/`FirstOrCreate` no longer fill them when no row is found |
| `assigns` | `Assign` | They are no longer assigned, found row or not |
| `Settings` keys of `InstanceSet` | `InstanceSet` | Not found on the clone. `Set` keys are |
| `Vars` | `Raw`, `Exec` | Copied only with a non-empty `SQL`, which these always set |
| `BuildClauses`, `CurDestIndex` | a finisher's callbacks | None: each query sets them again |

`WithContext`, and a Session with `Context`, `PrepareStmt` or `SkipHooks`, clone at once and drop the same fields. An immutable value never carries the state, since its next call clones: `db.Assign(x).Session(s).Where("a")` keeps its fix, and so does a chain that sets the state per branch, `q.Assign(x).FirstOrInit(&u)`. The analyzer asks once per reported root, and each walk follows a chain once (`phiroots` in the [growth gate](#allocation-growth-gate)). The answer is kept for each value, since the fix asks it of each value merged with each root (`filtersloopuse`, [Fix cost](#fix-cost)).

Limitations:

- Only a parameter root is checked for its callers' state. A local root built from a parameter keeps its Session fix, as `q := tx.Where("a")` reused in `FirstOrInit` does. Checking it would withhold most `FirstOrInit` fixes in functions that take `db`.
- A helper's result (`q := withAssign(db)`), a chain through a method value or in a closure, and a struct field root are not followed. A caller's `InstanceSet` on a parameter is not checked.

`e2e/internal/uncloned_state_test.go` checks each field at run time against `statementCloned` and `statementUncloned`, and the runtime effects of `Attrs`, `Assign`, `InstanceSet` and the body Session. `TestGormStatementFieldsClassified` fails when gorm adds a Statement field neither list holds. `testdata/src/gormreuse/fix_uncloned_state.go` holds the fixtures.

</details>

### Errors left on the root

On a mutable root, a query's result is the root itself. A failed query leaves its error in the root's `Error`, gorm skips every later query on the root, and each later result reports the first error (#372). A Session gives each query its own copy, so a later query runs and reports its own outcome:

```go
q := db.Model(&Order{ID: id})
q.Update("name", "new") // fails
return q.Update("id", id).Error
```

Before the fix, the second `Update` is skipped and the function returns `name update failed`. After a Session at `q`, or `q = q.Session(...)` at the top of a body for a parameter, the second `Update` runs and the function returns `nil`. In a `Transaction` callback that turns a rollback into a commit. The re-lint is clean.

`finisherErrorChanges` in `internal/fix/change.go` withholds the fix when a query whose error nothing reads can run before a read that sees the error. `changeErrorsOfValue` follows each use's result:

| Result | Counts as |
| --- | --- |
| A finisher's result that is discarded, or whose only fields read are not `Error` (`n := q.Find(&x).RowsAffected`) | An unread error, at the query |
| A field read (`Error`, `RowsAffected`, `Statement`) | A read, at the field |
| A return from the function | A read, at the return |
| An argument of a call that is not a `*gorm.DB` method, whose result is used (`return helper(q)`) | A read, at the call. A call whose result is unused, such as `applyTenant(q, 7)`, or a `go` or `defer` statement, reads nothing |
| A store anywhere but a local variable, such as a struct field or a package-level variable | A read that may run anywhere |
| The argument of a `*gorm.DB` method (`q.Or(q.Where("a"))`, `db.Where("id IN (?)", sub)`) | Nothing |
| A Phi | Nothing. The Phi's uses are uses of the root |

It follows a result into the `*gorm.DB` methods called on it, out of a closure that is only called in place (`changeInlineResults`), into the loads of a local variable it is stored in (`changeVariableReads`), and into a closure that captures it. What a closure does is taken to run anywhere. So is a use that `AllUses` does not list (`Violation.Hidden`): a deferred or spawned call, a query on a `Session` copy (which takes the root's `Error` with it), a loop-carried read. A query that repeats in a loop sees its own error on the next iteration. Without a loop, a query's own result is not a read of an earlier error.

A query whose error is read, as in `if err := q.Count(&n).Error; err != nil { return err }`, keeps the fix: on every path that goes on, no error is left on the root. The immutable-param directive alone stays, since it changes nothing at run time. The result is memoized per root. The uses of one root share the values they walk, keyed with the walk's `query` and `far` state (`changeSeen`), and only the union of their points is read. So `n` uses that each reach the `n` loads of one captured variable walk each value once, not `n` times. The hidden uses share theirs apart, since they only say whether any point exists. The tracker builds `Violation.Hidden` once per root, as it does `AllUses` (#389; `closurereassigns`, `storedreassigns`, `loopclosurefilters` in the [growth gate](#allocation-growth-gate)).

Tests: `testdata/src/gormreuse/fix_error_propagation.go` holds the cases with and without a fix. `TestFixErrorPropagationPreservesQueries` in `e2e/internal/error_propagation_test.go` runs `fixsql/before/error_propagation.go` against real GORM, where a query runs only when the error is returned. `TestFixErrorPropagationDetectsSessionAtRoot` checks that the comparison fails for the fixes suggested before.

Limitations:

- An error that is read is taken to be handled, even when the code goes on after it, as in `if q.Find(&x).Error != nil { log(...) }`.
- The rule is conservative wherever the result goes out of sight. A builder that stores its chain in a field or a package-level variable (`r.db = r.db.Where(c)`) after a query whose error nothing reads gets no fix. Neither does a function that hands the root to a call whose result it returns, such as a recursive call.
- Standard pagination that does not check `Count`'s error (`q.Count(&n); err := q.Find(&us).Error`) gets no fix. Check `Count`'s error to get it back.
- The diagnostic does not advise a Session for such a root. `checker.reportViolation` generates the fixes before it reports, and asks `Generator.SessionChangesErrors`, the memo of this check. When it holds, `pollution.Violation.AdviseCheckingErrors` replaces the Session advice with `a Session would let a later query run past an earlier query's unchecked error, so check that error first`. A root whose fix an earlier check withholds, such as `changedElsewhere`, or that gets the directive alone, keeps the Session advice: `Generate` returns before it asks this check.

### Fixes that compile

Every suggested fix must compile, applied with all the others (`-fix`) or alone (an editor's quick fix) (#200). `internal/fix` checks types before it writes an edit:

| Edit | Rule | What went wrong before |
| --- | --- | --- |
| Reassignment `q = <statement>` | Each call in the chain must return `*gorm.DB` (`discardedChainToReassign`), and none may be a finisher (`typeutil.IsFinisher`) | `q = q.AddError(err)` assigned an `error`. `FindInBatches`, `CreateInBatches`, `UpdateColumn(s)`, `Commit`, `Rollback`, `SavePoint` and `RollbackTo` return `*gorm.DB` but run SQL or end a transaction, and were reassigned |
| `.Session(...)` after a root or a Phi edge | Only after the call whose `Lparen` is the value's position, and only when it returns `*gorm.DB` (`sessionEndAfterCall`) | The innermost call that contained the position got it. For a closure parameter edge that was the call the closure is passed to: `run(db, func(tx *gorm.DB) { ... }).Session(...)` |
| The `gorm` qualifier | `gormNameAt` resolves the name with `types.Scope.LookupParent` at each insertion point. It uses an import whose name resolves to `gorm.io/gorm` at all of them (a dot import if `Session` does). Otherwise it adds an import under the first of `gorm`, `gormio`, `gormio2`, ... that no identifier of the file uses and no scope of the file or package declares (`gormImportNameFree`). In package `gorm.io/gorm` itself it writes a bare `Session` | A blank import counted as an import (`undefined: gorm`). A parameter named `gorm` gave `gorm.Session is not a type`. A file importing another package as `gorm` got a second `gorm` (`gorm redeclared`). A name free only at the fix's own positions gave one file both `"gorm.io/gorm"` and `gormio "gorm.io/gorm"`, one per diagnostic (staticcheck ST1019, #378). Inside package gorm, the fix imported gorm itself (an import cycle, #378) |
| The import edit | Each diagnostic's fix carries every edit it needs. `internal/report.go` does not drop an edit that an earlier diagnostic already suggested | With two roots in a file without the import, only the first fix had it. Applying the second alone failed with `undefined: gorm` |

A Phi edge with no call is left without a Session, as an edge without a position always was. The fix is then partial, and the diagnostic can remain after it.

<details><summary>Tests</summary>

The `-fix` driver and `analysistest.RunWithSuggestedFixes` merge identical edits from several fixes. `goldentest.ApplyFixes` does the same.

| Test | Checks |
| --- | --- |
| `TestGoldenFilesCompile` | The fixtures type-check with all fixes applied |
| `TestEachFixCompiles` | Each diagnostic's fix, applied alone, type-checks in every file it edits |
| `TestEveryDiagnosticCarriesFix` | Each diagnostic in `fix_every_diagnostic.go` carries its root's fix (#246) |

The cases are in `testdata/src/gormreuse/fix_result_type.go`, `fix_session_target.go` and `fix_import_*.go`, and in `testdata/src/converge/fix_compile.go`.

</details>

### Contract and directive fixes

Five diagnostics get a mechanical fix (#248):

| Diagnostic | Fix | Where |
| --- | --- | --- |
| Mutable value passed to an `immutable-param` parameter | `.Session(&gorm.Session{})` after the argument | `Generator.GenerateIsolation` |
| Unused `pure`, `immutable-return`, `immutable-param` | Delete the name from the comment | `reportUnusedDirectiveFuncs` |
| Unused `immutable-input(name)` | Delete each unused `immutable-input(...)` part. An orphan comment loses all of them | `reportUnusedImmutableInputs`, from `ImmutableInputUnused.Parts` |
| Redundant `immutable-param` | Delete `immutable-param` from every comment `DirectiveFuncSet.CommentsOf` finds for the function | `reportRedundantImmutableParam` |
| Unused `ignore` | Delete `ignore` from the comment | `reportUnusedIgnores` |

<details><summary>Isolating an argument, deleting a directive, and which ignores are deleted</summary>

**Isolating an argument.** The tracker records the call and the parameter index (`callsite.Site.ArgParam`) on the violation (`AddContractViolation`).

- The index counts no receiver, and neither does `ast.CallExpr.Args` for `r.m(x)`, a method value or a method expression, so it indexes the AST arguments. Only a variadic parameter differs, and a `*gorm.DB` passed to one is packed into a slice that the check never sees.
- The fix checks that the argument is a `*gorm.DB` expression. It wraps an operand that is not a primary expression (`*p` becomes `(*p).Session(...)`). It writes the Session through `sessionEdits`, so it takes its gorm name and import from `gormNameAt` as every other Session does (#200).
- Several mutable arguments of one call give one diagnostic, because the reports share the position and the message (see `reportKey`). `checker.report` gives that diagnostic the fixes of every violation in its group with the same message.
- No fix isolates an argument to a function that discards a chain on its parameter ([immutable-param body rule](#immutable-param-body-rule)).
- The Session result is immutable, so one pass resolves the diagnostic. The caller's own reuse of the value is a separate diagnostic with its own fix.

**Deleting a directive.** `directive.Removals` holds every diagnostic whose fix deletes a directive part, and reports them in `Flush`, at the end of `RunSSA`. A diagnostic suppressed by an ignore is never recorded. The rewrite of a comment takes out the parts of every diagnostic recorded for it, not only its own. `//gormreuse:pure,immutable-return` on a function with no `*gorm.DB` has two diagnostics. Two different rewrites of the one comment would conflict under `-fix`, so their fixes are the same edit, which the drivers apply once. A fix is still complete alone, as #200 requires.

`removalEdit` rewrites a comment as follows:

| Comment after the deletion | Edit |
| --- | --- |
| Other names remain | Only the name list changes: `//gormreuse:ignore,pure // why` becomes `//gormreuse:pure // why` |
| No name, and a reason after `//` | The reason stays, from its `//`: `// why`. It may be a note or another tool's directive, such as `//nolint:errcheck` |
| No name, and a reason that starts with `gormreuse:` | **No fix.** `//gormreuse:ignore //gormreuse:pure` would become a pure directive |
| No name and no reason | The comment is deleted. A comment alone on its line goes with the line. A comment after code goes with the blanks before it |

The line test reads the file with `pass.ReadFile`. When it cannot read it, or the file's size differs from the parsed one, there is no fix.

**Which ignores are deleted.** An unused ignore covers no diagnostic of the pass by definition. `Ignores.Ignore` marks an entry used whenever it suppresses one, and a line entry and the function span of a doc-comment ignore share one entry. Two cases remain:

| Case | Rule |
| --- | --- |
| A function whose analysis panicked lost its diagnostics, so its ignores can look unused | After any `gormreuse: internal error` report, unused ignores keep their report but get no fix |
| An ignore used in one variant of a package and unused in the other | An unused ignore in a non-test file gets no fix when its directory holds a `_test.go` file (`reportIgnoreRemovable`). The variant without the test files does not report it when the package has test files of its own (`reportIgnoresLeftToTests`, #348) |

The test variant sees calls in `_test.go` files. For example, a direct call of a named Transaction callback makes its parameter mutable (#191) in that variant only. The non-test variant then reports the ignore unused, as it did before #248, and `-fix` would apply that variant's fix too. A pass sees only its own variant, so it cannot tell. The pass's own files are checked first, so the test variant withholds the fix too. Otherwise the directory is listed, and one that cannot be listed counts as having tests. An ignore in a `_test.go` file is analyzed only by the test variant, which sees the whole package, so it keeps its fix. `testdata/src/ignoretests` checks both, in both variants.

Before #348 the variant without the test files still reported such an ignore. Take an ignore that only the test variant needed. The `gormreuse` command runs both variants, so it could never run clean. `go vet` and golangci-lint run only the test variant, so they were clean. That variant now leaves the report to the test variant. It decides from `go/build`'s `TestGoFiles` of the directory: external test files (`package x_test`) make no test variant of `x`, so they do not count. The cost: run with `-test=false`, an unused ignore in such a file is never reported.

**Redundant immutable-param.** One comment before `a, b := func..., func...` applies to both closures. The comment is edited only when no closure it covers keeps the directive: one that needs it (`needsImmutableParam`), or one that is also `pure`. Otherwise the redundant closure's diagnostic has no fix. A function that forwards its parameter once to another immutable-param function is reported redundant (see `computeNeedsImmutableParam`). Deleting its directive then exposes the caller-side diagnostic at the forwarding call, which a second pass fixes.

`testdata/src/gormreuse/fix_directive_removal.go` and `fix_isolate_arg.go` pin the output. `testdata/src/converge/fix_directives.go` and `fix_isolate_arg.go` check that one pass converges. `internal/directive/removal_test.go` covers each comment shape.

</details>

### Fix cost

One `Generator` serves the whole pass. What a fix looks up in a file, a function or the package is indexed there on first use, not scanned again for each root or use (#350, #377):

| Index | Per | Used by |
| --- | --- | --- |
| The calls of a file by Lparen, and the Lparen of the first call that ends at each position (`fileCallsOf`) | File | `callAtLparen`, and `virtualRootEndsImmutable` for the end of a reassigned chain |
| The statements of a file in preorder, with their ranges and enclosing statements (`stmtIndexOf`) | File | `findStmtAtPos` |
| The SSA calls of a function by position | Function | `virtualRootEndsImmutable` |
| The functions that the package refers to other than by a call | Package | `calledOnlyDirectly` |
| The calls of the analysed functions and `init` by callee, in program order (`callsOf`) | Package | `paramCallersIsolate`, `everyCallUsesResult` |

`RootTracer.SessionDropsState` keeps its answer for each value. The fix asks it of each value merged with each root (`mergedDropsState`), and each walk follows the whole Phi chain.

`mergeReportFixes` keeps the edits it has seen in a map by range, so an edit that several roots need costs one lookup.

<details><summary>How findStmtAtPos uses its index, the cost before #377, and what is still cubic</summary>

**findStmtAtPos.** The innermost statement at a position is the narrowest statement whose range holds it. Of two with the same range, the outer one wins, as the scan before the index picked the first in preorder. The ranges of parsed syntax nest, so it is the last statement that starts at or before the position, or the first statement that encloses that one and holds the position. A binary search finds the first, and the walk up finds the second. If the starts are out of order, the index is scanned instead. An if statement's end is its else branch's end, found from the index: `IfStmt.End` walks down the else chain, so an else-if ladder of `n` cases cost `n²` per lookup.

**Before #377.** Each of these was a scan per root or per use. The CHANGELOG gives the times:

| Shape | Scan |
| --- | --- |
| `n` optional filters, then a use in a loop | `SessionDropsState` for each merged value of each root: cubic |
| An else-if ladder of `n` filters | `findStmtAtPos` over every statement for each use: cubic |
| `n` functions that reuse their parameter | `calledOnlyDirectly` over every identifier, and `paramCallersIsolate` over every instruction, for each parameter: quadratic |
| `n` functions with a discarded chain | `virtualRootEndsImmutable` over every call of the file for each virtual root: quadratic |

**Still cubic.** When `n` loops each hold an optional filter and a use, the use in loop `i` may see the roots of the `i` merges before it. The fix of each such root has a Session for each merge before it. So the diagnostic of loop `i` merges about `i²` edits into about `i` distinct ones, and the `n` diagnostics merge about `n³`. Each is a map lookup with no allocation. The allocations stay quadratic, and the growth gate holds the steps of `loopfilteruse`, `loopsfilters` and `goloopsfilters` to `growthCubic`. Interning the edits of each root fix once would make each merge an array lookup, which is a constant factor only.

</details>

## Scopes Session-family warning (temporary)

`internal/scopeswarn` works around go-gorm/gorm#7592 (#196). It reports a `Session`/`WithContext`/`Debug` call only when its result is what a `Scopes` callback returns, and the Scopes chain may write.

> [!WARNING]
> This rule is temporary. Re-check go-gorm/gorm#7592 on each GORM bump.

<details><summary>Why only a returned Session value leaks, and only for writes</summary>

**Why only the returned value** (checked in `e2e/internal/scopes_session_test.go`):

- gorm runs the create/update/delete callbacks on the value the last scope returned (`db = scope(db)` in `executeScopes`).
- `BeginTransaction` stores its flag with `InstanceSet`, which calls `getInstance`. On a `clone>0` value that clones the Statement, so `CommitOrRollbackTransaction` never finds the flag, and the transaction stays open.
- `Session`/`WithContext`/`Debug` return `clone>0`. Every other chain method returns a `clone==0` value from `getInstance`.

So `return q.Session(...).Where("x")` is safe, and `return other.Session(...)` leaks even though `other` is not the callback parameter. The issue suggested restricting the warning to values derived from the parameter. That was wrong: the rule keys off the return value instead.

**Only some finishers leak (#308)** (checked in `e2e/internal/scopes_finisher_test.go`). gorm registers `BeginTransaction` and `CommitOrRollbackTransaction` only for create, update and delete. The query, row and raw callbacks begin no transaction. So a callback that returns a Session result is harmless for `Find`, `Count`, `Pluck`, `Scan` or `Exec`.

</details>

A Scopes call is skipped when its chain provably only reads (`scopeswarn.ReadsOnly`). Every use of its result, and of each chain method after it, must be the receiver of one of these:

- A chain method: the chainable API, `Session`, `WithContext`, `Debug`.
- A finisher that only reads: `Find`, `First`, `Take`, `Last`, `FirstOrInit`, `Count`, `Pluck`, `Scan`, `Row`, `Rows`, `Exec`.

`CollectScopesOnlyCallbacks` reads only the other Scopes calls. A callback passed to one Scopes call that may write is still reported.

<details><summary>Examples</summary>

<table>
<tr><th>Chain from <code>db.Scopes(cb)</code></th><th>Reported</th></tr>
<tr><td>

```go
db.Scopes(cb).Where("x").Find(&x)
db.Scopes(cb).Count(&n)
q := db.Scopes(cb); q.First(&u)
```

</td><td>No</td></tr>
<tr><td>

```go
db.Scopes(cb).Delete(&u)
db.Scopes(cb).Update(...)
db.Scopes(cb).Create(...)
db.Scopes(cb).FirstOrCreate(...)
db.Scopes(cb).Save(...)
```

</td><td>Yes</td></tr>
<tr><td>

Returned, stored in a field, passed to a function or another method, or discarded.

</td><td>Yes: it may reach a write elsewhere</td></tr>
</table>

</details>

<details><summary>How the rule finds its input</summary>

| Input | How |
| --- | --- |
| Callbacks | `callback.CollectScopesOnlyCallbacks`, the Scopes half of the collector that reuse detection uses (`resolveMayFuncs`). It follows the varargs array, slice literals, `append`, Phi edges, local, captured and package-level variables, struct fields, and method values (both the `$bound` wrapper and the method). Package-level initializers live in the synthetic `init`, which is not in `SrcFuncs`, so the resolver scans it for stores to globals |
| Returned value | Followed through Phi edges, local and captured variables, `Extract`, and the returns of static callees (helpers, invoked closures). A callee's parameter maps back to the caller's argument, so `return passThrough(q.Debug())` is reported. Calls are followed to depth 16, and never into a function already on the stack |

</details>

Limitations:

- A dynamic callee (a func-typed parameter, an interface method) ends the trace, so a Session result returned through one is not reported.
- A callback that returns a root `*gorm.DB` (also `clone>0`, for example a package-level `DB`) leaks the same way. It is not reported, since the warning is about the three Session-family methods.
- The rule does not model the order of several scopes. Each callback that returns a Session-family result is reported, even when a later scope returns a `clone==0` value.
- The rule does not read `SkipDefaultTransaction`. With it, a write begins no transaction and does not leak, but the setting is in a config the analyzer does not see.
- A chain that leaves the function is taken to write, even when every caller only reads.
- A package-level closure is a known Scopes callback, and reuse detection analyzes its body like any closure ([Function variables](#function-variables)).

<details><summary>Before #308</summary>

The rule ignored the finisher, and reported gorm's own `tests/scopes_test.go:60`, whose chain ends in `Find`.

</details>

## Development

Develop on Go 1.27 or later. The fixtures use generic methods and promoted struct-literal keys, so the test harness needs a 1.27 toolchain to compile them. Both the main module and the E2E module require Go 1.27 in their `go.mod` files.

Run `go test ./...` for Go changes, and run `test_all.sh` (every check CI runs) before claiming the full gate passes. After changing detection or fixes, regenerate the fixtures with `GORMREUSE_UPDATE_GOLDEN=1 go test -run TestGoldenFiles .`.

<details><summary>All development commands</summary>

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
cd e2e/internal && go test ./...

# Regenerate the .golden and .diff fixtures after changing detection or fixes
GORMREUSE_UPDATE_GOLDEN=1 go test -run TestGoldenFiles .

# Run every check CI runs (declscope, both test suites, build, golangci-lint)
./test_all.sh
```

</details>

<details><summary>Code style and refactoring rules</summary>

**Code style.**

- Follow standard Go conventions.
- Use the `go/analysis` framework.
- Unexported types by default. Export only what is needed.

**Refactoring rules.** Follow these strictly:

| Change | Rule | How |
| --- | --- | --- |
| Main code | Tests must NEVER fail | Run `go test ./...` after each change. If tests fail, revert and try a different approach |
| Test code | Coverage must NEVER drop | Compare `go test -coverprofile=/tmp/before.out -coverpkg=./... .` and `go test -coverprofile=/tmp/after.out -coverpkg=./... .` with `go tool cover -func=/tmp/before.out` vs `go tool cover -func=/tmp/after.out` |

</details>

## Testing Strategy

- Use `analysistest` for all analyzer tests.
- Test fixtures use `// want` comments for expected diagnostics.
- Each fixture file is split into `===== SHOULD REPORT =====` (cases that should trigger warnings) and `===== SHOULD NOT REPORT =====` (negative cases).

The doc tests keep these notes honest:

<details><summary>The doc tests</summary>

| Test | Checks |
| --- | --- |
| `TestKnownLimitationsListed` | A fixture function that pins a known limitation carries `[LIMITATION]` in its doc comment or body, and is named in [Known Limitations](#known-limitations). It checks both directions, and checks the README's list against it. Remove the marker and both entries when a fix makes the case work |
| `TestImplementationNotesNamesExist` | Each code span of these notes names a file or a Go name that exists. A name counts only where code uses it, not in a comment. Code spans inside fenced blocks are not checked |
| `TestImplementationNotesArchitectureTree` | The [Architecture](#architecture) tree, both ways. Each entry and each Go name in its comments exists, and each directory it lists has all of its Go files listed |
| `TestReadmeVersionPinsAgree` | The README's version pins ([Releasing](#releasing)) |

</details>

> [!CAUTION]
> **Never name a fixture `*_test.go` unless you mean to.** `analysistest` loads a package's `*_test.go` files as part of both the normal package and the test variant. Every diagnostic in such a fixture is collected **twice**: this produced 343 duplicated diagnostics in PR #57. Use a plain `.go` name. The intentional exceptions are `testdata/src/filefilter/code_test.go`, which exists to test the analyzer's `-test` flag handling, and `testdata/src/ignoretests/code_test.go` and `needed_test.go`, which give their package test files.

### Suggested-fix fixtures

Each fixture `testdata/src/gormreuse/X.go` has two committed files that record its suggested fixes. So does each file of another fixture package that a fix edits, test files included. `goldenPackages` lists every package directory of `testdata/src` except the gorm stubs and `converge`, so a new package is covered without a change.

| File | Content | Checked by |
| --- | --- | --- |
| `X.go.golden` | The source with every suggested fix applied | `TestGoldenFiles`, `TestGoldenFilesCompile` (the packages must type-check with every `.golden` swapped in), and `RunWithSuggestedFixes` for `gormreuse`, `linedirective`, `noimport` and `aliasimport` |
| `X.go.diff` | A full-context unified diff from `X.go` to `X.go.golden`, for review | `TestGoldenFiles` |

A plain `go test` only compares these files. It never writes them, so CI fails when they are stale. To regenerate them after a change to detection or fixes:

```bash
GORMREUSE_UPDATE_GOLDEN=1 go test -run TestGoldenFiles .
```

This rewrites every `.golden` and `.diff` file, and deletes the ones whose source fixture is gone. Outside `gormreuse` it also deletes those of a file that no fix edits any more. `RunWithSuggestedFixes` does not catch that case: it ignores the golden file of a file without fixes. So a stale golden could keep a fix that is gone: two `linedirective` goldens kept a Session fix that #243 had withheld, until #316.

> [!IMPORTANT]
> Read each changed `.diff` before you commit it. It shows exactly what `-fix` now does to the fixture.

<details><summary>The compile and converge tests</summary>

The failure message of `TestGoldenFiles` prints the same command. Rendering a `.diff` needs `diff` on `PATH`, and a failing `diff` fails the test.

| Test | Checks |
| --- | --- |
| `TestEachFixCompiles` | Applies each diagnostic's fix on its own to the same packages and to `converge`, and type-checks each variant of the package (#200) |
| `TestFixesConverge` | Applies the fixes to each fixture in `testdata/src/converge`, then lints the fixed output again. It analyzes the package once, and re-lints all the fixed files as one package |

`TestFixesConverge` fails when the directory has no fixture, when a fixture's fixes change nothing, when the fixed output does not type-check, or when any diagnostic remains. So every fixture there must need a fix, and one pass of fixes must resolve all of it.

A fixture of `testdata/src/gormreuse` whose fixes must also converge is listed in `convergeGormreuseFixtures` instead of being copied into `converge`. Today these are `fix_paths.go`, `fix_isolate_arg.go`, `fix_later_loop_merge.go` and `fix_closure_param.go`. `TestFixesConverge` re-lints its committed `.golden`, which `TestGoldenFiles` keeps equal to the current fixes.

</details>

### Allocation-growth gate

`TestAllocationGrowth` in `growth_test.go` generates each case at two sizes and runs the analyzer on them (#302). It fails when the heap allocations of its `Run` grow faster than the case's limit. The packages are generated in a temporary `GOPATH`, so no large fixture is committed.

| Limit | Sizes | Ratio allowed | For |
| --- | --- | --- | --- |
| `growthLinear` | n, 2n | 2.5 | Many small functions, or one function whose work is linear |
| `growthNearLinear` | n, 2n | 3.0 | Shapes that grow a little faster than n (about 2.3x, 2.5x with the race detector), such as one large function |
| `growthQuadratic` | n, 2n | 4.5 | Shapes that are quadratic today |
| `growthCubic` | n, 2n | 9.0 | The steps of a shape whose code grows as n² (`inferexp2`), or whose diagnostics each merge a quadratic number of edits (`loopfilteruse`, [Fix cost](#fix-cost)) |
| `growthExponential` | n, n+4 | 2.0 | Shapes that used to double their work per statement (about 16x) |

The test counts the steps of the analyzer's searches too (#350). A step is one of these:

- A value traced
- A CFG block visited
- A referrer that `isAssignment` or the Scopes chain check reads
- A block that a store search walks back through
- A pair of uses compared
- A `mayAlias` search
- A syntax node or an instruction that a fix's index reads, and a statement that `findStmtAtPos` walks up through (#377)
- An edit that `mergeReportFixes` merges (#377)

`internal/effort` counts them, only once a test has called `effort.Count`. The steps are held to the case's limit, or to its `steps` limit when its searches repeat work that allocates nothing. Each such case says why.

<details><summary>How it measures, and the rejected time limit</summary>

How it measures:

- It counts `runtime.MemStats.Mallocs` around `Analyzer.Run` only, so loading and building SSA are not counted. The packages run one at a time (`checker.Options.Sequential`), and the test is not parallel, because the count is process-wide.
- It analyzes every package three times and keeps the smallest count. The analyzer does a few percent more or less work from run to run with map order, and the race detector makes `sync.Pool` drop items at random.

**Rejected: a wall-clock limit (#260's time limit).** Under CI's flags (`-race -covermode=atomic -coverpkg=./...`) the analyzer ran about 25 times slower. A limit was either flaky or too loose to catch anything. Allocation counts repeat, and the race detector and coverage change most ratios little.

</details>

Limitations:

- Work that allocates nothing is counted only where a search counts its steps. The race detector's `sync.Pool` drops make some of the rest visible. `switchuse` grows about 2.3x without the race detector and about 3x with it, so it has the quadratic limit.
- A ratio does not show a constant factor. `spawnedfields` is quadratic in its output, since each of its `n` reads may hold each of `n` roots, so a doubled constant passes its limit.
- A case whose issue is still open has `after: "#NNN"` and is skipped. Its fix removes the field. Every later performance fix adds its shape with a limit it meets.

### Testdata organization

<details><summary>The fixture packages</summary>

`TestAnalyzer` checks the `// want` comments of every package directory under `testdata/src` but those in its `wantSkipped` list, which gives the reason for each. So a new fixture package is checked without a change to the tests. The table lists the tests that check more than that.

| Directory under `testdata/src` | Test | Content |
| --- | --- | --- |
| `gormreuse/` | The golden tests | The main fixtures, one file per topic, named after it (`basic.go`, `evil.go`, `session_alias.go`, `subquery.go`, `fix_*.go`, ...). Each has a `.golden` and a `.diff` |
| `converge/` | `TestFixesConverge` | Code whose fixes must resolve every diagnostic in one pass |
| `determinism/` | `TestDeterministicDiagnostics` | Several roots reused at one position, and unused directives for the report order |
| `purelib/`, `externalpure/` | `TestAnalyzer` | Directives declared in one package and called from another |
| `filefilter/` | `TestAnalyzer` | The `-test` flag and generated files |
| `ignoretests/` | `TestIgnoreFixWithTestFiles`. `TestAnalyzer` skips it: a want comment cannot say which variant reports | Unused ignores in a package with a test file, and which of them get a fix |
| `paramtests/` | `TestParamFixWithTestFiles` | A parameter root in a package with a test file, and which fix it gets |
| `linedirective/`, `linedirectivedep/` | `TestLineDirective` | `//line` directives |
| `cgofile/` | `TestAnalyzer` | A cgo package. Skipped without a C compiler |
| `noimport/`, `aliasimport/` | `TestSuggestedFixesWithImport`, `TestSuggestedFixesWithAlias` | How a fix names `gorm` |
| `fakegorm/` | | A package that `gormreuse/fix_import_other.go` imports as `gorm` |
| `gorm.io/gorm/`, `github.com/stretchr/testify/` | | Stubs of the real packages |

Directives across packages are tested by `TestAnalyzer`. `testdata/src/purelib` declares them, and `testdata/src/externalpure` calls them. Both packages' diagnostics and purelib's facts are checked. `TestCrossPackageFactsE2E` in `cmd/gormreuse` runs the same kind of pair through the built command and through `go vet -vettool`.

</details>

### E2E tests

The `e2e/internal/` directory holds tests that verify actual GORM SQL behavior with sqlmock. It is a separate Go module (its own `go.mod`) to avoid dependency conflicts. CI tests and lints it on every PR (`cd e2e/internal && go test ./... && golangci-lint run ./...`). Dependabot covers it, so GORM bumps arrive as PRs. Re-check go-gorm/gorm#7592 on each (see the README).

> [!IMPORTANT]
> Every test asserts (`t.Errorf`). The suite guards the linter's core clone-semantics premise:
>
> - Mid-chain (`clone==0`) reuse accumulates conditions.
> - Root, `Session()`, and `Transaction`- and `FindInBatches`-callback (`clone>0`) handles fork a fresh Statement and are safe.
> - A `Connection`-callback handle is `clone==0` and accumulates.

<details><summary>What each E2E file pins, and the gorm stub checks</summary>

| File or test | Pins |
| --- | --- |
| `scopes_session_test.go` | Which Session-family calls inside a `Scopes` callback leak the transaction (go-gorm/gorm#7592): only a returned one. Also the exceptions in [Session and Debug copies](#session-and-debug-copies): a `Session()`/`Debug()` copy of a mid-chain value sees later changes to it, and `Session{Initialized: true}` returns a mid-chain value |
| `subquery_test.go` | A subquery, group condition or Joins condition argument is only read. A change to it before the outer query runs leaks into that query ([Subquery and condition arguments](#subquery-and-condition-arguments)) |
| `fixsql_test.go` | Suggested fixes keep every condition the code applied ([Discarded statements](#discarded-statements-under-a-session-fix)) |
| `TestFixsqlCasesCoverBefore` | Fails when an exported function of `fixsql/before` is in no case list. A topic's test file registers its cases with `registerFixsqlCases` |

`gorm_stub_test.go` checks the gorm stub in `testdata/src/gorm.io/gorm` against the real gorm, since Dependabot bumps only the real one:

| Test | Checks |
| --- | --- |
| `TestGormStubMatchesGorm` | Each method of `*gorm.DB`: its name, and for each parameter and result whether it is a `*gorm.DB`, a callback (with the same check inside) or another type. The analyzer finds callback parameters by their declared types (#257). A method that the stub leaves out must be in `gormStubOmitted`, with the reason |
| `TestGormStubFuncsMatchGorm` | Each package-level function of the stub (`Open`, `Expr`) against `gormRealFuncs`, in the same way |
| `TestGormStubSessionMatchesGorm` | Each field of the stub's `Session` exists in gorm's with the same type. Each field that `callsite` reads from a Session literal (`Initialized`, `NewDB`, `Context`) is in both |
| `TestGormReceiverUsesMatchGorm` | Each method that returns no `*gorm.DB` is in `typeutil.receiverUses` or in `gormNonChainBranches` (#307) |
| `TestGormStatementFieldsClassified` | Each field of `gorm.Statement` is in `statementCloned` or `statementUncloned`, and each method of the analyzer's `unclonedStateMethods` is in gorm and the stub (#344) |

When a gorm bump fails one of them, decide how the analyzer should treat the new or changed name. Then add it to the stub or to the list.

</details>

## Releasing

Releases are cut from GitHub Actions, never locally. Before you dispatch one, prepare it in a pull request:

1. In `CHANGELOG.md`, rename `## Unreleased (planned as X.Y.Z)` to `## X.Y.Z (YYYY-MM-DD)`. Check that it covers every behaviour change since the last tag: `git log --oneline vLAST..`.
2. Bump every version pin in `README.md` to the new version. The pins are the `mise use` line, the `mise.toml` example, the `@vX.Y.Z` caution, `VERSION=` in the tarball steps, and the `.custom-gcl.yml` example.
3. Merge it, and wait for CI on the merge commit.

`TestReadmeVersionPinsAgree` fails when any pin names another version than the newest `v*` tag. While a release is prepared, the newest dated `CHANGELOG.md` section names a newer version, and the pins must name that one. The test needs the tags, so CI's test job fetches the full history (`fetch-depth: 0`). In CI a missing tag fails the test. Elsewhere, such as in a shallow clone or a source tarball, the test checks only that the pins agree, and skips the comparison with the release.

Then dispatch the release:

| Workflow | Use |
| --- | --- |
| **Tag and Release** (`tag_and_release.yml`) | The normal path: creates the `vX.Y.Z` tag, then builds and publishes |
| `tag.yml` / `release.yml` | The two halves, dispatchable on their own when a tag already exists |

<details><summary>Workflow details</summary>

- `tag.yml` tags the commit the workflow was dispatched on. It refuses unless that commit is on `main` and has a successful CI run from a push to `main`. Dispatch it from `main` after CI on the merge commit has passed.
- Workflow actions are pinned by full commit SHA, with the release in a trailing comment. Dependabot updates both.
- `release.yml` runs goreleaser (`.goreleaser.yaml`) once for all six targets: darwin/linux/windows x amd64/arm64, `CGO_ENABLED=0`. It uploads the archives plus `checksums.txt` to the release.

</details>

> [!CAUTION]
> The release assets are what makes `mise use -g "github:mpyw/gormreuse"` work. A release with no assets breaks the documented install path. goreleaser is pinned to 2.17.1: 2.18.0 attests every build artifact, which fails the job.

<details><summary>Release notes and goreleaser checks</summary>

goreleaser groups the commit list by conventional-commit type (`changelog.groups`), and the release header links to `CHANGELOG.md`. Paste the version's `CHANGELOG.md` section into the release notes above that list. Validate config changes with `goreleaser check`, and dry-run with `goreleaser release --snapshot --clean --skip=publish`.

</details>

## Known Limitations

Each fixture function marked `[LIMITATION]` under `testdata/src` pins one of these cases. The README lists the same cases for users, in the same order. `TestKnownLimitationsListed` fails when:

<details><summary>What TestKnownLimitationsListed checks</summary>

- A marked function is not named in this section.
- A fixture named here (in a Fixtures column, or after "Fixtures:") is not a marked function.
- A marker comes after the last function of its file, so it marks none.
- The README's section lacks one of the four kinds below, or a kind has a table in one file only.
- A table has no rows, or a Markdown row has a different number of cells than its header (escape a `|` in a cell as `\|`).
- The rows of a table differ from the README's, or are in another order. Each README row starts with a `<!-- fixtures: a, b -->` comment that names the fixtures of this section's row.
- The README's summary table gives a kind another case count than its table has, or a count for a kind without a table (write `—`).

</details>

**Missed reuse (false negatives).** The fixture has no `want` at the missed use.

| Case | Fixtures | Cause |
| --- | --- | --- |
| A closure assigns a captured variable: `f := func() { q = db.Where(...) }; f()` | `closureModifiesCaptured` | The store in the closure is not a root the caller's uses resolve to |
| A `Session` copy used in `go`/`defer`, with the root changed after the statement | `siteDeferSessionCopyBranchAfterDefer` | The alias use is ordered by the statement's position, not by when the call runs ([Session and Debug copies](#session-and-debug-copies)) |
| `defer` inside a `go` closure, or `defer` → `go` → `defer`, with the other use after the statement | `deferInsideGoroutine`, `nestedDeferGoroutineDefer` | A deferred call nested in a spawned closure is not recorded as a use |
| A method value merged by a Phi: `if c { find = q.Find } else { find = q.Find }; find(nil)` | `methodValueConditional`, `methodValuePhiWithNil` | The bound-method receiver is not traced through the Phi |
| A closure passed once, outside a loop, to a function that calls it many times | `repeatsClosurePassedToRepeater` | Only the sites in the declaring function are counted ([Uses that run more than once](#uses-that-run-more-than-once)) |
| A closure stored in a slice or a local struct field, then called twice through it: `fs := []func(){f}; fs[0](); fs[0]()` | `repeatsClosureCalledThroughSlice`, `repeatsClosureCalledThroughField` | Only the uses of the closure value are sites. The store is one site, and a call through the container is a dynamic call |
| A local reassigned and used in a loop: `for … { q = q.Where(c); q.Find(nil) }` | `fsoLocalLoopCarried` | The loop-header Phi makes the reassignment an assignment. The loop-carried read is checked for fields and globals only (#182) |
| A derived variable that flows back: `if c { q2 = q.Where("a") }; if d { q = q2 }` | `derivedThenMergedBack` | `isAssignment` asks whether a merge *can* hand back the root, not whether it does on that path ([Which merges are reassignments](#which-merges-are-reassignments)) |
| A field rebuilt in a loop after it was copied: `p.db = q; for … { p.db = p.db.Where(c) }; q.Find(nil)` | `fsoLoopRebuildOfAliasedValue` | `isLoopFieldRebuild` makes the rebuild an assignment, so the first iteration's read of `q` is not a branch (#182) |
| A pure function that returns a `Session` copy of its argument | `paCopyHidden` | The copy is not in `PureVerdicts.Returned` ([Session and Debug copies](#session-and-debug-copies)) |
| A `*gorm.DB` global assigned in another function, a closure or the initializer | `pvInitializedAtPackageLevel`, `pvStoreInClosure` | A load sees only the stores in its own function ([Package-level variables](#package-level-variables)) |
| A struct field read through a parameter or a receiver: `s.db` twice, or `s.dbs[0]` twice | `fieldOfStructParam`, `crFieldContainer` | The field is not a mutable root ([Struct roots](#struct-roots)). Nor is a container read from it ([Reads from containers](#reads-from-containers)) |
| The `immutable-param` caller contract through an interface or a func-typed parameter | `contractInterface` | No static callee |
| A method expression of a method promoted through an embedded `*gorm.DB`: `W.Find(w, nil)` twice | `exprPromoted` | The thunk selects the field in its body, and the receiver `w` is not followed to it ([Method expressions](#method-expressions)) |
| The address of a struct field handed to a helper that changes it: `applyP(&s.db); s.db.Find(nil)` | `fixEscapePointerFieldAddr` | Only the address of a variable is a branch and a `PointeeRoot` ([Addresses of variables](#addresses-of-variables)). The fix is still withheld: the struct is handed on ([Struct stores](#struct-stores)) |
| A call through a receiver of a type parameter's type: `func twice[T interface{ Find(any, ...any) *gorm.DB }](q T) { q.Find(nil); q.Find(nil) }` | `irGenericReceiver` | The generic body has no `*gorm.DB`: `q` is a `T`, and the call is an interface call with no declared method ([Interface method calls](#interface-method-calls)) |
| A closure that a call returns, called twice: `load := loader(q); load("a"); load("b")`, or `g := mk(); g(); g()` | `cfReturnedClosureOfParam`, `cfReturnedClosureOfCapture` | A call through the returned func value has no callee. The closure's sites are its uses in the function that makes it, and a return is none ([Uses that run more than once](#uses-that-run-more-than-once)) |
| A pure function, or a helper inferred `immutable-param`, that writes the argument's query through anything but a direct path: `db.Statement.DB`, a `*gorm.Statement` parameter, a local variable that is assigned again, captured or has its address taken, a copy, a `Session` or `Debug` result, a holder parameter, an `append` result, a lookup, a conversion, a merge, or a call | `pswNotDirect`, `pswHolderParams`, `pswReassignedParam`, `pswInferNotDirect` | Only a direct path from the parameter surely reaches the argument ([Pure contract](#pure-contract)). The others may write memory it does not hold, and following them reported read-only code (#405 review) |
| A pure function that writes the argument's `Config`: `db.DryRun = true` | `pswConfig` | `Config` holds no clause, so the walk skips it. The write still changes how the caller's later queries run |

**Extra reports (false positives).**

| Case | Fixtures | Cause |
| --- | --- | --- |
| `a, b := pair(db)`, where `pair` returns two new chains, then `a.Find(nil); b.Find(nil)` | `opaqueTuplePairShared` | Every `*gorm.DB` of one tuple shares the call as its root (#176) |
| `G = db.Where("x"); if c { G = G.Where("a") }; G.Find(nil)` for a global or a field | `pvConditionalRebuildOfStore` | A store is not a merge point the way a local's Phi is |
| A captured pointer variable reassigned to a second returned struct: `p := mk(s); go func() { p.db.Find(nil) }(); p = mk(s); p.db.Count(nil)` | `cosReturnedReassigned` | A load of a reassigned pointer variable is resolved to every pointer stored in it. So the last read is matched to the first struct too ([Struct roots](#struct-roots)) |
| A recursive helper that returns only immutable values, branched twice | `notInferredRecursive` | Inference is a least fixed point, so a function that returns its own result is never inferred ([Inferred immutable-return](#inferred-immutable-return)) |
| A closure called on exclusive branches, once directly and once inside another closure | `rcsExclusiveAcrossClosures` | Sites in two functions are taken to both run ([Uses that run more than once](#uses-that-run-more-than-once)) |

**Wrong position.** Uses in a deferred closure are ordered by their source position, not by when they run. `defer func() { q.Count(nil) }(); q.Find(nil)` is reported at `q.Find`, which runs first. A deferred call such as `defer q.Count(nil)` is ordered after the function's other uses and reported at the `defer`. Fixtures: `ifInsideDefer`, `ifElseInsideDefer`, `nestedIfInsideDefer`, `tripleNestingIfForDefer`, `tripleNestingForIfDefer`, `deferCapturingLoopVar`, `nestedDeferClosures`, `tripleNestedDeferClosures`, `nestedDeferWithFor`, `ultimateIfForDeferChaos`.

**No fix.** A `go` or `defer` statement that changes the root in place where another use may see it gets no fix ([In-place changes outside the variable](#in-place-changes-outside-the-variable)). Fixtures: `godeferGoChainOnly`, `godeferDeferChainOnly`, `godeferDeferHelperOnly`, `godeferDeferHelperInLoop`. Neither does a root whose Statement may carry `Attrs`, `Assign` or `InstanceSet` state, which a Session would drop ([Statement state a Session drops](#statement-state-a-session-drops)). Fixtures: `ucsAssignRoot`, `UcsParamFirstOrCreate`.

<details><summary>Before #309: roots used only in go/defer</summary>

A root used only in `go`/`defer` statements also had no fix, as in `for … { go q.Find(nil) }` or `go q.Count(nil); go q.Find(nil)`. Its branch uses were not in `Violation.AllUses`. Now `Tracker.violationUses` gives such a root its branch uses, and the fix is a Session at the root, since they are not ordered.

</details>

Other limitations have no fixture marker. They are described with their design:

- A call through an exported, reassigned or address-taken `var f = func(...)` does not honour the closure's directives ([Function variables](#function-variables)).
- A getter of a field or a global is inferred `immutable-return` though another package, or code given the field's address, assigns it a chain ([Inferred immutable-return](#inferred-immutable-return)). A direct read of the field misses the same reuse. A store before the call, in the calling function, is seen ([Parameter reads](#parameter-reads)).
- The limits of the inferred directives are in [Inferred immutable-return](#inferred-immutable-return) and [Inferred immutable-param and immutable-input](#inferred-immutable-param-and-immutable-input).
- A callee in another package exports no fact for the [immutable-param body rule](#immutable-param-body-rule), so an argument to it still gets the isolation fix.
- The limits of `-fix` are in [Suggested fixes](#suggested-fixes), from [Reassignment fix target](#reassignment-fix-target) to [Contract and directive fixes](#contract-and-directive-fixes).

## Related Projects

- [goroutinectx](https://github.com/mpyw/goroutinectx) - Goroutine context propagation linter
- [zerologlintctx](https://github.com/mpyw/zerologlintctx) - Zerolog context propagation linter
- [ctxweaver](https://github.com/mpyw/ctxweaver) - Code generator for context-aware instrumentation
