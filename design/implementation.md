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

1. **Immutable-returning methods** ([`Session`](https://pkg.go.dev/gorm.io/gorm#DB.Session), [`WithContext`](https://pkg.go.dev/gorm.io/gorm#DB.WithContext), [`Debug`](https://pkg.go.dev/gorm.io/gorm#DB.Debug), [`Open`](https://pkg.go.dev/gorm.io/gorm#Open), [`Begin`](https://pkg.go.dev/gorm.io/gorm#DB.Begin)) return a new `clone > 0` instance (`clone == 1` or `2`; the exceptions are below). A call to one is a pure use of the receiver's root, not a branch. `typeutil.IsImmutableReturningBuiltin` also lists [`Transaction`](https://pkg.go.dev/gorm.io/gorm#DB.Transaction). It returns `error`, so only the pure use applies: it runs its callback on a `Begin` result. Its callback's fresh `tx` is a separate exemption (see [Directives](#directives))
2. **State-only methods** (`AddError`, `DB`, `Get`, `InstanceGet`, `Callback`) touch no clause, so a call of one is not a use at all. `ToSQL` reads the receiver like `Session`. See [Methods that do not branch](#methods-that-do-not-branch-307)
3. **All other methods** on a mutable instance create a **branch** that consumes the instance
4. **Second branch** from the same mutable root is a **violation**
5. A **use of a `Session`/`Debug` copy** after its mutable root changed is a **violation** (see below)
6. A `*gorm.DB` **argument** of a query method is a **read** of its root, and a use of the outer chain after the argument's root changed is a **violation** (see [Subquery and condition arguments](#subquery-and-condition-arguments-185))

#### Session and Debug copies (#174)

The name-based "immutable-returning" class is not exact. In GORM v1.31, `Session` builds a new `*DB` with `Statement: db.Statement` and clones the Statement only when `Context`, `PrepareStmt` or `SkipHooks` is set. `Debug` is `getInstance()` followed by `Session` with only a `Logger`, and `getInstance()` returns the receiver itself when it is `clone == 0`. The E2E tests in `e2e/internal` (`TestCopyOfMidChain_*`, `TestSessionInitialized_*`) check each claim below.

| Call on a `clone == 0` receiver | Effect | Model |
| --- | --- | --- |
| `Session(&gorm.Session{})`, `Debug()`, `Session` with only other options | Copy points at the receiver's Statement. Each use of the copy forks its own Statement (`clone == 2`), so it never changes the receiver. But it reads the receiver's Statement as it is at that moment. | Pure use of the receiver's root at the call. Each later use of the copy is an **alias use** of that root. |
| `WithContext`, `Session` with `Context`, `PrepareStmt`, `SkipHooks` | Statement cloned at the call | Pure use only (unchanged) |
| `Session` with `NewDB` | `clone == 1`: each use starts a new Statement | Pure use only (unchanged) |
| `Session` with `Initialized` | Returns `tx.getInstance()`: a `clone == 0` handle with its own copy | Pure use of the receiver; the result is a **new mutable root** |

`callsite.Site.SessionEffect` decides this from the `&gorm.Session{...}` literal. In SSA the literal is an `Alloc` with one `FieldAddr`+`Store` per set field. Any other use of the `Alloc`, a store in another block than the `Alloc`, or a non-constant bool makes the field unknown. An unknown config is assumed to both share and return a mutable value. A `Context` set to any non-constant value counts as set, matching the existing trust in `WithContext(ctx)`.

`RootTracer.FindAliasedRoots` finds the copies a value reaches through `FindAllMutableRoots` (so Phi, variables and closure captures work) and returns each copy's receiver root, following copies of copies. The call handler records an alias use for a gorm method receiver, a bound-method receiver, and a `*gorm.DB` argument of a non-gorm function. `DetectViolations` reports an alias use when a polluting **or assignment** use of the root lies between the copy and the use and reaches the use. Assignment uses count here because `q = q.Where(...)` changes a `clone == 0` Statement in place. A change before the copy is not counted: it already makes the `Session`/`Debug` call itself a second branch, so counting it again would duplicate that report.

**Rejected: count the copy as a branch of the receiver.** The issue suggested this. It would also report `q.Session(&gorm.Session{}).Count(); q.Session(&gorm.Session{}).Find()` and "copy used, then the original changed", which are safe at runtime (`TestSessionBeforeFinisher`, `TestCopyOfMidChain_UseDoesNotChangeOriginal`). The alias-use model reports only the order that leaks.

**Cost (#260).** Every gorm call asks `FindAliasedRoots` about its receiver, and each step down the chain asks `FindAllMutableRoots` again. With `n` conditional reassignments `if c { q = q.Where(...) }` that was cubic in `n` (about 10 s at `n = 400`). Memos on the `RootTracer` keep it near quadratic, and none of them changes a report:

- `FindAllMutableRoots` keeps its result per value and `LoopInfo`. The result depends on nothing else once the pure verdicts are final, which is before any `RootTracer` is made. A `LoopInfo` with no loop header keys as `nil` (`rootsLoopInfo`, #300): the tracer reads a `LoopInfo` only through `IsLoopHeader`, and with every answer false each reader does what it does for `nil`. So every function without loops, such as most closures, shares one entry per value instead of tracing a parent's value once per closure. A `LoopInfo` with headers keeps its own entry, since a trace from `v` can reach that function's blocks, for example through a closure that `v` calls.
- `mayAlias` keeps, per value and `copies` flag, whether `collectAliasedRoots` can add a root from there. A search that cannot is skipped. Every call it would visit adds nothing either, so what is added, and in what order, stays the same.
- `mayAlias` computes the least fixed point (#300). The searches form a graph, and a loop makes a cycle in it. Every search on a cycle reaches every other one, so they all get the same answer. `mayAliasVisit` finds the cycles with Tarjan's algorithm and answers each one when it is complete, so a cycle that adds nothing is answered false and skipped too. Before #300, a search that reached itself was assumed to add a root, so a search inside a loop was never skipped. `n` sequential loops that rebuild one query then took time cubic in `n` (5.6 s at `n = 800`).
- **One tracer per pass (#325).** `run.go` makes one `RootTracer` for the reuse analysis of every function, and `computeNeedsImmutableParam` makes one for its counterfactual analyses. Each closure is also a function of the package and is analysed on its own, and that analysis traces the values of the function that declares it again. With a tracer per function, `n` spawned closures that each read a query reassigned `n` times traced the parent's `n` loads, each with up to `n` roots, once per closure: cubic, 85 s at `n = 800`. The memos depend only on the values asked about, and the sets the tracer holds are final before it is made, so sharing them changes no report.
- `HasMutableRoot` answers `isAssignment`'s membership test. It keeps a list of 16 or more roots as a set. After `n` such loops the receiver has `n` roots, and `isAssignment` asks about each of them, so a linear search was cubic too. The allocation gate cannot see this, since the search allocates nothing, so `TestHasMutableRoot` checks that a long list is kept as a set.

The `loops` shape also copied its roots once per Phi in `traceAllPhiEdges`, and `pollution.sortedRoots` built the names of its Phi roots once per comparison. `cfg.Analyzer.reachSet` resliced its queue's head off, so the queue was reallocated again and again for every source block. All three are now done once, which keeps the shape linear in allocations. It is still quadratic in time (0.8 s at `n = 1600`): the receiver of each of the `n` calls has up to `n` roots, and each Phi keeps its own list.

**Rejected: one shared `visited` set for the whole search.** It visits each value once, but through a loop it visits roots in a different order, and the order of alias uses picks the message when two reports share a position.

`TestAllocationGrowth` in `growth_test.go` holds these shapes (`filters`, `sessfilters`, `purechain`) to quadratic growth, and `loops` to linear growth. See [Allocation-growth gate](#allocation-growth-gate-302).

Alias violations carry no suggested fix. The reuse-fix strategies rewrite the root's branches, and the right fix (`.Session(&gorm.Session{})` at the end of the root's chain, or an isolating copy) depends on intent.

Limitations:

- A `go`/`defer` use of a copy is an alias use at the statement's position. A change after the statement is missed, although a deferred or spawned call can run after it (`siteDeferSessionCopyBranchAfterDefer`).
- A copy returned from a function, or stored in a struct field and read in another function, is not followed. A `//gormreuse:pure` function that returns a copy of its argument hides the sharing from its caller. The copy is not the argument itself, so it is not in `PureVerdicts.Returned` (see [Returned arguments](#returned-arguments-188)). Treating it as the argument would count each use of the copy as a branch, which the rejected approach above shows is wrong.
- The fix generator's Phi-edge check treats `Session(&gorm.Session{Initialized: true})` as already immutable. The `//gormreuse:immutable-return` body check reports it (see [Immutable-return body contract](#immutable-return-body-contract-194)).

#### Subquery and condition arguments (#185)

A `*gorm.DB` passed as an argument of a query method used to count as a branch, through the varargs slice store or the interface conversion. So `db.Where("id IN (?)", sub)` twice was reported. GORM v1.31 never changes such an argument. `e2e/internal/subquery_test.go` checks each claim below.

| Shape | When GORM reads the argument | How |
| --- | --- | --- |
| Subquery bound to `?` (`Where`, `Or`, `Not`, `Having`, `Table`, `Select`, `Joins` with SQL, `Find` conditions, `Update` value) | When the outer query builds its SQL: at its finisher | `Statement.AddVar` runs `v.Session(&Session{DryRun: true}).getInstance()`, a cloned Statement, or copies its `Statement.SQL` for a `Raw` argument |
| `Raw`, `Exec` | At the call | The SQL is built at once |
| Group condition `Where(sub)`, `Or(sub)`, `Not(sub)` | At the call | `BuildCondition` copies the `WHERE` expressions. It also runs `sub`'s pending `Scopes`. Running them on a `clone == 0` value changes it in place, but a second run finds them already applied, so the result is the same |
| `Joins("Company", cond)` | At the call | `joins` copies `Selects`, `Omits` and the `WHERE` clause |

`pollutionsource.readOnlyArgMethods` lists the methods and the first parameter that only reads. Every later parameter reads too. `Find`/`First`/`Last`/`Take`/`Delete`/`FirstOrInit`/`FirstOrCreate` take a destination or model first, which GORM writes to or parses, so that parameter is not in the list. `FirstOrInit` and `FirstOrCreate` pass their conditions to `Find` (#253). `Model`, `Order`, `Clauses`, `Assign` and `Attrs` are not in the list. A call through a method value (`where := db.Where; where("id IN (?)", sub)`) is read as the method call it stands for: `collectAliasedRoots` resolves it with `callsite.Site`, and `pollutionsource.CallReadOnlyArgs` does the same for the handler and `Leak` (#178).

The model, as the issue proposed: an argument is a read at some point between the call and the end of the outer chain.

- **At the call** it is a pure use of its root (`siteUse.read`), like a `Session` call. A branch or a finisher of the root that reaches the call is reported as a second branch. A finisher counts too: it leaves `Dest`, `Model` (set from `Dest` when empty) and the parsed schema on a `clone == 0` Statement, so the read can differ. The issue listed `s.Count(&n)` before a subquery of `s` as a false positive; it stays reported.
- **Later**, every use of the outer chain reads it again. `FindAliasedRoots` returns the argument's root for each use of a value whose chain holds such a call, with the call as `Copy` and `Argument` set. `DetectViolations` then treats it as a `Session`/`Debug` alias use: a polluting or assignment use of the root after the call and before the use, that reaches the use, is reported. It walks every gorm call of the chain, through `Session`/`WithContext` too, since a cloned Statement still holds the argument. A `Session`/`Debug` copy below another chain call is not an alias there: that call forked its own Statement. A subquery of a subquery is followed.
- **The varargs array** is not a leak when its every slice goes only to read-only parameters (`pollutionsource.isReadOnlyArray`). `Leak` applies this to the main handler and to the `pure` validator alike. A `[]any{sub}` literal spread into `Where` is read; one that is also passed elsewhere still leaks.
- **Wrappers** (#253). A subquery inside `gorm.Expr(sql, sub)`, `sql.Named(name, sub)`, a local map literal, or a local `clause.Expr` literal (or a pointer to one) is read like a bare one. GORM binds it with `Statement.AddVar` when the outer query builds its SQL (`e2e/internal/subquery_wrapper_test.go`). `isReadOnlyValue` accepts a wrapper only when it is used, and its every use is a read-only argument or another such wrapper; stores into the wrapper itself are allowed. Then `Leak` skips the store into it, the handler and the `pure` validator skip the `sql.Named` call (`IsReadOnlyWrapperCall`), and `ReadOnlyGormArgs` returns the subquery inside (`wrappedGormDBs`), so the "changed before the outer query runs" check applies too. `pollutionsource.wrapperFuncs` lists the wrapper functions. Any other use of a wrapper, such as a field read, a map lookup or a call of another function, keeps it a leak. Only these wrappers are known: a user function that wraps its argument the same way is still a call of a function that is not `pure`.

Decisions and limitations:

- **Eager reads are treated as lazy.** `Raw`, `Exec`, group conditions and `Joins("Company", cond)` read at the call, so a change after the call does not reach them. They are still reported, as for a subquery. Telling them apart would need per-method timing for little gain, and the report is a false positive only for this order.
- **One report per argument.** Each call of `x.Order("id").Find(nil)` is a use of `x`. A changed argument is reported once per call that took it, at the earliest use that sees the change. `Session`/`Debug` alias uses keep one report per use.
- **No suggested fix** for the later-change report, as for alias uses. A `.Session(&gorm.Session{})` at the end of the argument's chain fixes both reports.
- **Not followed:** an outer chain returned from a function, or stored in a field and used elsewhere, as for `Session`/`Debug` copies. An outer chain used in `go`/`defer` is an alias use at the statement's position, as for a copy. A `//gormreuse:pure` function may now take a `*gorm.DB` argument and pass it to `Where`, and its caller does not see that the result holds it.

#### Methods that do not branch (#307)

Every gorm method used to branch its receiver unless it returns an immutable copy. So `sqlDB, _ := db.DB(); db.AutoMigrate(...)` on a parameter was reported, and so was each `db.AddError(...)` after a branch in a GORM callback. In the gorm corpus (the root module of `go-gorm/gorm` at commit b3d3bf2), 83 of 149 reports were of this kind. `typeutil.receiverUses` is the one table of methods whose call does not branch its receiver. GORM v1.31.2 does the following for each of them on a `clone == 0` receiver. `e2e/internal/state_methods_test.go` checks each claim with sqlmock.

| Method | What GORM does | Model |
| --- | --- | --- |
| `Session`, `WithContext`, `Debug`, `Open`, `Begin`, `Transaction` | See above | Read |
| `ToSQL` | Runs its callback on `db.Session(&Session{DryRun: true}).getInstance()`, a cloned Statement. The receiver is unchanged, but the SQL holds the receiver's clauses | Read: a branch before it is reported |
| `AddError` | Sets `db.Error` (and `Statement.Result.Error`). Touches no clause | State only |
| `DB` | Returns the `*sql.DB` behind the connection pool of the Statement or the `Config` | State only |
| `Get`, `InstanceGet` | Read `Statement.Settings` | State only |
| `Callback` | Returns the callback manager of the `Config` | State only |

A read is `siteUse.read`, as for `Session`: a branch of the root that reaches the call is reported. A state-only call is skipped by `handleReceiver` before anything is recorded. `IsImmutableReturningBuiltin` covers both, so the `pure` validator, the immutable-return check and the fixes treat them as not branching too.

Decisions:

- **`AddError` is state only, although a later chain inherits the error.** All chain calls of a `clone == 0` value return that same `*gorm.DB`, so `q.AddError(err); q.Find(&users)` runs no SQL and returns `err` (`TestStateMethods_AddErrorIsInherited`). Stopping the chain is what `AddError` is for, and GORM's own callbacks use it that way. This linter reports clauses that one chain leaves for another. An error is outside that model, just as a failed `Find` leaves its error on the value. With the error cleared, the later query is the same (`TestStateMethods_TouchNoClause`).
- **`Get` and `InstanceGet` after a branch are not reported.** A branch can store a setting with `Set` or `InstanceSet`, and a later `Get` sees it. No query is built from that read, so it is treated as a field read like `db.Statement`, which is not a use either.
- **`Migrator` and `AutoMigrate` stay branches.** `Migrator` runs the receiver's pending scopes in place, and the `Migrator` it returns holds a `Session` copy that shares the receiver's Statement. A condition added to the receiver after the call leaks into the queries the `Migrator` runs (`TestStateMethods_MigratorSharesStatement`). The copy cannot be followed through the `Migrator` interface. `Association`, `Connection`, `Row`, `Rows`, `ScanRows`, `SetupJoinTable` and `Use` stay branches too. `gormNonChainBranches` in `e2e/internal/gorm_stub_test.go` gives the reason for each.
- **Methods promoted from `*gorm.Config` are not in the table.** `Name`, `Explain`, `Apply` and the other dialector methods are calls on the embedded `Config` in SSA, not on the `*gorm.DB`, so they were never uses.

`TestGormReceiverUsesMatchGorm` keeps the table in line with GORM. Every method declared on the real `*gorm.DB` that returns no `*gorm.DB` must be in `receiverUses` or in `gormNonChainBranches`. Every name in either must still be a method of it.

### Directives

- `//gormreuse:ignore` - Suppress warnings for the same line, and for the next line when the comment is the first token on its line. A trailing comment after code covers its own line only (#189). `buildIgnoreMap` (run by `Ignores.AddFile`) decides this from the smallest column at which an AST node starts or ends on the comment's line.

  `directive.Ignores` applies one scope rule to every diagnostic (#190). `RunSSA` sends each report through `Ignores.Ignore(pos)`, and `run` sends directive problems through it too. Lookups go from the most specific scope to the widest, and mark the first match used:

  | Scope | Rule |
  | --- | --- |
  | Line | The diagnostic's own line, or any line of the innermost statement that holds it. Blocks and labeled statements are skipped. For `if`, `for`, `range`, `switch`, `select` and case clauses only the header (up to `{` or `:`) counts, so a header ignore does not reach the body. An ignore comment inside a closure nested in the statement belongs to the closure, not to the outer statement |
  | Function | A `func` declaration whose doc comment holds the ignore: from the doc comment to the closing brace, closures included. A closure has no function-level ignore of its own |
  | File | The whole file |

  An ignore only suppresses reports. Before #190 a function-level ignore skipped the function in every pass. That hid contract violations while the directive stayed trusted (a leaking `pure` function was trusted at call sites), never covered closures, and marked the ignore used even when nothing was suppressed. Now every pass runs, so `PureVerdicts.Failed` and `needsImmutableParam` see ignored functions, and an ignore is used only when it suppressed a report. Unused-ignore reports go straight to `pass.Reportf`: an ignore never silences another ignore's staleness. So do `panicGuard` internal-error reports, which can now come from an ignored function too, since it is analyzed.
- `//gormreuse:pure` - Mark function/method/closure as not polluting its `*gorm.DB` argument
- `//gormreuse:immutable-return` - Mark function/method/closure as returning immutable `*gorm.DB` (like Session/WithContext). **Body contract**: when the function actually returns a provably-mutable value — one whose root is a gorm chain-method call, e.g. `db.Where(...)` or `Session().Where(...)` (the trailing chain re-forks a fresh `clone==0` Statement), or `Session(&gorm.Session{Initialized: true})` — the directive is reported at the declaration, since the linter would otherwise trust it and silently allow unsafe reuse of the return value at call sites. Closures, deferred writes to named results and some package-level variables are followed; see [Immutable-return body contract](#immutable-return-body-contract-194). Roots the tracer treats as mutable only conservatively (a bare `*gorm.DB` parameter, or a call into an unmarked named function) are given the benefit of the doubt and not reported.
- `//gormreuse:immutable-param` - Opt a function's `*gorm.DB` parameters out of the mutable-by-default treatment of parameters: they are treated as immutable inside the function (the caller is responsible for passing an isolated value). **Caller-side contract**: when the function actually branches such a parameter, passing a mutable `*gorm.DB` at a call site is reported (isolate with `.Session(&gorm.Session{})` first, or make the caller `immutable-param` too so the contract propagates). The check runs wherever the callee is known statically: a direct call, a function or method value, a generic instance, and the same calls under `go` and `defer` (#192). A `go`/`defer` statement goes through the same `handleSite` as a call (see [One handler for every call site](#one-handler-for-every-call-site-254-252)). It asks `handler.BreaksImmutableParam` of each argument: it is bound to a parameter, and its `*gorm.DB`, directly or in an interface, has a mutable root. The parameter-root fix asks the same before it declares a function `immutable-param` (#245). An interface method call or a call through a func-typed parameter has no static callee, so its contract cannot be checked. A callee in another package is checked through its package's fact (see [Directives across packages](#directives-across-packages-195)).
- `//gormreuse:immutable-input(name)` - Declare that the function passes an **immutable** `*gorm.DB` to its callback parameter `name` (a user-defined equivalent of gorm's `Transaction`/`FindInBatches`). The named callback's `*gorm.DB` parameter is then treated as immutable, so reuse inside the callback is allowed. **Body contract**: if the function actually passes a mutable value to the callback, it is reported. Reported unused when `name` isn't a parameter, is variadic, isn't a function type, or the callback has no `*gorm.DB` parameter.

  **Variadic callbacks are not supported (#259).** For `func many(db *gorm.DB, cbs ...func(*gorm.DB))`, each element of `cbs` would be a callback. Support needs both sides: the body check would follow every element of the slice, and the callback collectors, which read one callback per argument (`CollectedCallbackArgs`), would resolve every function the caller packs into the variadic slice. Neither does today, so the directive is reported unused with `parameter "cbs" is variadic; variadic callbacks are not supported`, instead of the misleading `is not a function type` it got before. The callbacks keep a mutable parameter, which reports more, not less.

  **Following the callback (#193).** Before #193 `ValidateImmutableInputs` checked only a `Call` whose callee was the parameter itself, so `go cb(...)`, `defer cb(...)`, a closure, `forward(cb, ...)`, `(*(&cb))(...)`, a struct field and a slice element all handed a mutable value to the callback unreported. Now `immutableInputFlow` walks the parameter's referrers with a worklist:

  | Use of a value holding the callback | Handling |
  | --- | --- |
  | Callee of a `Call`, `Go` or `Defer` | Its arguments are checked |
  | Phi, load, field, element, map lookup, slice, `ChangeType`, `Extract`, range, `MakeInterface`, `TypeAssert` | The result is followed |
  | Store into a local `Alloc` or `MakeSlice` (through fields and elements), `MapUpdate` into a `MakeMap`, `append` | The container is followed, so every later use of it is seen |
  | Binding of a `MakeClosure` | The closure's free variable is followed in its body. The closure value itself may go anywhere, since its calls of the callback are checked |
  | Argument of a call to an `immutable-input` function at its declared callback, or to gorm's `Transaction`/`FindInBatches` | Accepted: that callee makes the same promise |
  | Comparison, `len`, `cap` | Ignored |
  | Anything else: another call, return, store elsewhere, channel send, `select` | Reported as `callback cb escapes (...)` |

  Only values whose type can hold a function (an interface counts) are followed, so an `int` field of a struct that holds the callback is not. A boxed callback is followed rather than reported at `MakeInterface`, because an implicit conversion has no position; the call or return it then reaches is reported instead. An argument that is a `*gorm.DB` goes to `FindMutableRoot`. A pointer to a local `*gorm.DB` variable (`cb(&q)`) is mutable when any value stored into `q` is. Any other argument that holds a `*gorm.DB` (a `**gorm.DB` parameter, a struct) is reported as `cannot verify the *gorm.DB passed to callback`. Escapes are reported rather than given the benefit of the doubt, because the directive makes the callback's own reuse unchecked: a wrong promise hides real bugs.

  **Placement.** The directive used to be read only from a `FuncDecl` doc comment; on a closure or after the opening brace it had no effect and was not reported. `ImmutableInputSet` now places it with a `DirectiveFuncSet` that has no signature check, so it accepts the same placements as `pure` (doc comment, same line after `{`, next line before a closure, every closure of a multi-assignment). Closure directives are keyed by the `FuncLit` position, and `Callbacks` looks a closure up through `fn.Syntax()`. A comment attached to no function is reported as `not attached to a function or closure`.

Also: gorm's built-in `Transaction` (via `Begin`) and `FindInBatches` (via `Session(&Session{NewDB: true})`) pass a fresh (`clone>0`) handle to their callbacks, so reuse inside those callbacks is always allowed. `Connection` is deliberately **not** in this set: it calls `fc(db.getInstance())`, which is a `clone==0` handle, so its callback's `tx` is an ordinary mutable parameter and reuse inside it is reported (#173). `e2e/internal` guards all three premises against real gorm.

**Callbacks under `go` and `defer` (#256).** The callee hands the callback the same value however the callee itself is invoked. So the callback collectors in `callback/collect.go` (`CollectImmutableCallbacks`, `CollectImmutableInputCallbacks` and the Scopes/Preload collectors) read the call of every `CallInstruction`, not only `*ssa.Call`, through `callsite.EachSite`. Before #256, `go db.Transaction(func(tx *gorm.DB) error {...})` and `defer withFresh(db, cb)` lost the exemption, and a callback of a deferred `Scopes` call lost the Session warning. `DropReachableCallbacks`, the immutable-param caller check and the immutable-input body check already read every `CallInstruction`.

**Named callbacks (#191).** The callback sets are keyed by the declared function, not by the call that passes it. A function's `*gorm.DB` parameter receives a fresh value only when every caller is a callback position, so `callback.DropReachableCallbacks` runs after both collectors and removes a function from the set when:

| Case | Why |
| --- | --- |
| Any reference that is not an argument in a callback position: a direct call (also under `go`/`defer`, and recursion), a method expression `T.m`, a store, a `Phi`, a capture, a return, an ordinary argument | That reference can hand the parameter a mid-chain value |
| The function or method is exported | Another package can call it, and the analyzer cannot see that call. Facts do not change this: they flow from a package to the packages that import it, so the declaring package is analyzed before any caller in another package (#195) |
| A method whose name matches an interface call, interface method value or interface method expression in the package | Dynamic dispatch can reach it |

A reference counts as a callback position when it is the Transaction/FindInBatches callback argument or an `immutable-input(name)` argument. `callback.CollectedCallbackArgs` lists these positions for a call, and the collectors use the same function. A closure (`MakeClosure`, including a bound method value `r.m`) is followed: it counts only when all its uses are callback positions. Any other wrapper, such as an interface conversion, counts as a non-callback reference, because both callback positions are func-typed. The scan covers the source functions, their nested literals and the package `init` (package-level variable initializers). A function that is dropped falls back to the rules for an ordinary parameter, so it can opt in with `immutable-param`, whose caller-side check then covers the direct calls. A capture of a local variable holding the function (`cb := f; go func() { _ = db.Transaction(cb) }()`) is treated as a non-callback reference, which may report a callback that is only ever passed to Transaction. That direction is the conservative one.

**Callbacks held in a variable or a field (#257).** Before #257 the collectors and `DropReachableCallbacks` read each call through its static callee alone, and `resolveExactFunc` resolved only a function, a closure and an interface conversion. So `db.Transaction(txBody)` with `var txBody = func(tx *gorm.DB) error {...}`, `db.Transaction(s.cb)` with a struct field, a call of an `immutable-input` closure held by a package-level variable, and `tx := db.Transaction; tx(cb)` all reported reuse inside the callback, and forwarding an `immutable-input` callback to such a variable reported `callback cb escapes (passed to a function value)`. Now every reader takes the full `Site` with `FuncVars`, and `callback/holder.go` finds the holders of the package:

| Holder | Condition | A read is |
| --- | --- | --- |
| Package-level variable | In `callsite.FuncVars`: unexported, stored once, its address only loaded and stored | `*g` |
| Struct field (keyed by its `*types.Var`, generic instances by their origin) | Unexported, exactly one `Store` to it in the source functions and the package `init`, every `FieldAddr` of it only loaded or stored, and no `ChangeType`/`Convert` of its struct (or a pointer to it) anywhere in the package | `*(&x.f)`, or `x.f` on a struct in a register |

The single store may run many times, but every value it stores is the same function or a closure of it, so every read of the holder is that function or nil. `resolveExactFunc` follows a read of a holder to the stored value. `DropReachableCallbacks` accepts a store into a holder as a callback reference when every read of the holder is itself used only as a callback argument (`holders.feedsOnlyCallbacks`, through the same `refReachesOnlyCallback`). A read that calls the function, passes it to an ordinary parameter or stores it elsewhere drops it, as a direct reference does. A cycle of holders feeding each other is dropped.

The Transaction/FindInBatches callback positions now come from the declared parameter types (a parameter of a function type with a `*gorm.DB` parameter), not from resolving each argument. Resolving decided the position from what the value is, so a function boxed in FindInBatches's `dest interface{}` counted, and a callback read from a holder did not.

Decisions and limitations:

- **Exactness rests on `DropReachableCallbacks`, not on the resolver.** The exemption only claims that the function's parameter receives fresh values. That holds when every reference reaches only callback positions, whatever the resolver found. A wrong resolution can only exempt a function whose every reference is a callback position, or miss one, which reports more.
- **Field holders are per field, not per object.** Two `svc` values share the field's holder. That is why one store in the whole package is required: with two stores, a read may hold either function.
- **Not followed:** a local struct field stored twice, a field assigned by reflection or through `unsafe` arithmetic beyond a plain `unsafe.Pointer` conversion of the struct, an exported field or variable, and a variable or field that holds a method value (`var f = r.m`), which `FuncVars` does not resolve. Each keeps its callback's parameter mutable.

Only `//gormreuse:name[,name...]` (lowercase, no spaces, line comment) is a directive. `readDirective` in `directive.go` reads every comment once, in this order:

1. Cut the comment at the first `//` after the leading `//`. The rest is a reason: `//gormreuse:ignore // reason`, `//gormreuse:ignore //reason` and `//gormreuse:ignore//reason` are all a bare ignore. `//` is the only reason separator; ` - reason` was never accepted.
2. Read what is left with `ast.ParseDirective`, with no normalization. A comment whose body starts with `gormreuse:` but is not a gormreuse directive is reported as `malformed gormreuse directive: write it as //gormreuse:name`. So is an empty part in the list (`pure,`, `pure, immutable-return`).
3. Check every part of the comma list. A part that is not `ignore`, `pure`, `immutable-return`, `immutable-param` or `immutable-input(name)` is reported as `unknown directive gormreuse:<part> (want ...)`. A malformed `immutable-input(...)` part, such as `immutable-input(cb` or `immutable-input()`, is reported as `malformed gormreuse:<part> directive: write it as immutable-input(name)`.
4. Any text left after the name is reported as `gormreuse:<name> takes no argument; write a reason after //`.

A comment with a problem has no effect at all, even when only one part of its list is wrong (#156). Before #156, `hasDirective` looked for one known name in the list, so `//gormreuse:pure,imutable-return` still marked the function pure and nothing reported the typo. Keeping the valid parts was rejected: the user meant the whole list, and a half-applied list hides the typo.

Problems are reported by `FindDirectiveProblems` through `pass.Reportf`, at the comment. Generated files are skipped with the rest of the analysis.

Directives can be combined with commas: `//gormreuse:pure,immutable-return`

**Generic instances and method values (#180).** A call can resolve to a stand-in for the function the directive is written on: an instance of a generic function or method (`genBranch[int]`), a bound method wrapper (`f := r.m`, named `m$bound`), or a method expression thunk (`T.m(r, x)`, named `m$thunk`, #258). The directive sets and the maps keyed by `*ssa.Function` (`PureVerdicts.Failed`, `needsImmutableParam`, the Scopes and immutable callback sets) are keyed by the declared function. `origin.Declared` maps a stand-in to it: an instance to its `Origin()`, a bound wrapper or a thunk to the declared method (`Prog.FuncValue`). Every lookup goes through it: `DirectiveFuncSet.Contains`, `ImmutableInputSet.Callbacks`, the `PureVerdicts.HasFailed` check in `IsPureFunction`, the immutable-param caller check, and the callback collectors (`callback/resolve.go`: `resolveMayFuncs` for Scopes/Preload, `resolveExactFunc` for Transaction/FindInBatches and immutable-input). Code that reads the arguments of a call reads them from `callsite.Site`, which aligns them to the declared method: a bound wrapper has no receiver argument, and a thunk takes the receiver as its first one. An interface method value or expression has no declared body, so `origin.Declared` leaves it unchanged and it carries no directive.

All internal positions (file keys, ignore lines, directive placement, the generated-file skip, fix offsets) use `PositionFor(pos, false)`, so `//line` directives cannot move them (#151, #152, #153). This includes the line and column that indent the parameter Session fix: a `//line` without a column sets every adjusted column to 0 (#244). Only the `root at file:line` text in messages stays adjusted, to match the position the driver prints. `TestNoAdjustedPositions` in `internal/position_guard_test.go` fails on any other `Position` call, or `PositionFor` without `false`, in non-test code under `internal/`. Its allowlist names the message function `Tracker.loc`.

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

A same-line directive belongs to the innermost function whose `{` is before it on the line: a closure nested in a closure, or a closure in a declaration's body (#315). `func outer(db *gorm.DB) { run(func(q *gorm.DB) { //gormreuse:pure` marks only the closure. Before #315 the declaration path (`findDirectiveAfterFuncDeclBrace`) lacked the nested-closure check, so it also made `outer` pure, and `immutable-input`, which shares the placement, attached to both. The placement index keeps the closures' braces of each line for that check.

**Lookup cost (#301).** Every call site asks the directive sets about its callee, so a lookup must not scan the file:

| Lookup | How |
| --- | --- |
| Which function a directive is on | `placement.addFile` reads the comments of a file once and keeps the groups that hold the directive. Only a file with such a group is walked, once, to place every closure's directive. A closure is matched only against the groups on its brace line or on the line above it or its statement, in file order, so the first match is the same as a scan of all comments |
| `DirectiveFuncSet.Contains` | Remembered per declared function, misses included. `Add` and `Import` clear it. An inferred function is checked first, so `Infer` needs no clearing |
| Innermost statement of a report, for a line ignore | The file's statements and closures are kept in source order with the index of the innermost one that holds each. A lookup is a binary search and a walk up the holders. Function-level ignore spans are searched the same way |

Before, a lookup scanned every comment of the file and placed a closure's directive by walking its statement and the whole file again, and an ignore lookup walked the file from its root. n closures with a directive in one function took 108 s at n = 800. The cases `ignores`, `ignorefn`, `directives`, `unuseddir`, `helpersign`, `closurepure`, `closurepuresame` and `closurepurepkg` in the [allocation-growth gate](#allocation-growth-gate-302) cover these.

> **Important**: Passing `q` to a user-defined function without `//gormreuse:pure` is a branch of `q`, like a gorm method call. When the function returns a `*gorm.DB`, its call is also handled like a gorm method call in a merge: `if c { q = helper(q) }` is an assignment (`handleArgs` calls `isAssignment`, see [Which merges are reassignments](#which-merges-are-reassignments-175)). Its result is a new mutable root, unless the helper is [inferred](#inferred-immutable-return-261) `immutable-return`. Use `//gormreuse:pure` for functions that don't pollute their arguments, and `//gormreuse:immutable-return` for functions whose result can be reused (like DB connection helpers).

> **Note**: Unused directives are reported as warnings:
> - Unused `//gormreuse:ignore` - line/function-level ignores that suppress no diagnostic
> - Unused `//gormreuse:pure` - directives that don't match any function
> - Unused `//gormreuse:immutable-return` - directives that don't match any function
> - Unused `//gormreuse:immutable-param` - directives that don't match any function (no `*gorm.DB` parameter)
> - **Redundant** `//gormreuse:immutable-param` - directive is signature-valid but has no effect: the parameter is never reused, so even treated as mutable it would produce no violation to suppress. Reported at the function declaration. Skipped when combined with `//gormreuse:pure` (a valid pure function cannot branch its parameter, so immutable-param is redundant there by construction). This is callee-side only: callers are not checked for it.
> - Unused `//gormreuse:immutable-input(name)` - `name` is not a function-typed parameter with a `*gorm.DB` parameter, is variadic, or the comment is attached to no function
> - For combined directives (`//gormreuse:pure,immutable-return`), if either part is used, no unused warning is reported

## Architecture

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
├── growth_test.go               # TestAllocationGrowth: allocation growth on generated shapes (#302)
├── cmd/gormreuse/
│   ├── main.go                  # CLI entry point (singlechecker)
│   └── *_test.go                # Build the command and run it (go vet, //line, facts, inspector paths)
├── plugin/plugin.go             # golangci-lint module plugin (no settings; shares Analyzer)
│
├── internal/
│   ├── run.go                   # RunSSA: every package-level pass, in order
│   ├── checker.go               # checker: one function's tracking, detection and reports
│   ├── report.go                # The order of reports, unused directives and ignores, and merging fixes
│   ├── pure.go                  # validatePureFuncs: the pure functions' contracts, to a fixed point
│   ├── immutable.go             # Inferring and checking immutable-return, and the need for immutable-param
│   ├── fact.go                  # exportFacts: the facts importing packages read
│   ├── distrust.go              # distrustContracts: contracts that rely on a broken one stay out of the facts (#314)
│   ├── guard.go                 # panicGuard: one function's panic does not abort the package
│   ├── directive/               # Comment directives
│   │   ├── directive.go         # readDirective, FindDirectiveProblems
│   │   ├── ignore.go            # //gormreuse:ignore scopes (Ignores), unused tracking
│   │   ├── funcset.go           # DirectiveFuncSet: pure / immutable-return / immutable-param sets, Infer
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
│   │   ├── change.go            # In-place changes outside the variable (#243)
│   │   ├── holder.go            # Struct stores of a root that other code reaches: escapes for #243 (#310)
│   │   ├── closure.go           # Closures: captured variables, reassignments in a closure
│   │   ├── gorm.go              # gorm.io/gorm name and import, finishers, state fields
│   │   ├── call.go              # Calls in the syntax: the call at an Lparen, direct and inline calls
│   │   └── stmt.go              # The innermost statement at a position
│   ├── goldentest/goldentest.go # Shared golden machinery (ApplyFixes, UpdateCommand)
│   ├── scopeswarn/scopeswarn.go # TEMPORARY Session-returned-from-Scopes warning (GORM bug #7592)
│   ├── typeutil/gorm.go         # IsGormDB, IsImmutableReturningBuiltin, and whether a type holds a *gorm.DB
│   └── ssa/
│       ├── analyzer.go          # Per-function Analyzer: tracking, then detection
│       ├── origin/origin.go     # Declared: the declared function behind a generic instance, a method value or a method expression
│       ├── callsite/            # What a call, go or defer instruction runs
│       │   ├── site.go          # Site, SiteOf, EachSite: callee, receiver, arguments
│       │   ├── session.go       # SessionEffect: what a Session/Debug call shares (#174)
│       │   ├── funcvar.go       # FuncVars: package-level and local variables that hold one function
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
│       │   ├── freevar.go       # traceFreeVar: roots through a closure's captured variables
│       │   ├── phi.go           # Phi edges, loop variable swaps through Phis
│       │   ├── pure.go          # PureVerdicts, IsPureFunction: pure calls and the arguments they return
│       │   ├── alias.go         # FindAliasedRoots: Session/Debug copies and read-only arguments a value reads
│       │   └── closure.go       # ClosureCapturesGormDB, IIFE returns, stored closure results, closure factories (#251)
│       ├── pollution/tracker.go # Tracker: records uses, DetectViolations
│       ├── pollutionsource/     # Which uses leak a *gorm.DB; read-only query arguments
│       ├── cfg/                 # Control flow of one function
│       │   ├── analyzer.go      # Analyzer: the CFG cache of a pass (#299); CanReach: block reachability
│       │   ├── loop.go          # LoopInfo, DetectLoops: natural loops, range-over-func bodies (#177)
│       │   └── repeat.go        # Repeats.RepeatsAfter: may a use run again for one run of its root's definition (#249)
│       ├── handler/call.go      # handleSite for calls, go and defer; Send, Store, MapUpdate, MakeInterface
│       └── purity/              # Body contracts of the directives
│           ├── validator.go     # pure (Validator, ReturnedParams)
│           ├── immutable_return.go  # immutable-return (ImmutableReturnValidator), InferImmutableReturns (#261)
│           ├── immutable_input.go   # immutable-input(name) (ValidateImmutableInputs)
│           └── violation.go     # Violation
│
├── testdata/src/                # See Testdata Organization
└── e2e/internal/                # Runtime checks against real GORM with sqlmock (separate module)
```

The `plugin` package registers `Analyzer` with golangci-lint's module plugin system. `Analyzer` reads no flags, so the plugin hands it over as it is and rejects any settings. It lives in the main module: `plugin-module-register` adds nothing beyond `golang.org/x/tools`, and a nested module would need its own tag per release.

### Analysis Pipeline

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
│    2. Collect Scopes and immutable callbacks (callback package)         │
│    3. Check immutable-input and immutable-return bodies                 │
│    4. Scopes Session warning (scopeswarn)                               │
│    5. For each function: detect violations (ssa.Analyzer), add fixes    │
│    6. Redundant and unused directives, export facts, unused ignores     │
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
- The checker reports each position once. When several roots are reused at one position (`use2(q, r)`), the diagnostic takes the message of the first root in that order, which is the root that appears first in the source. It carries the fixes of every root there, merged into one `SuggestedFix`, because drivers apply only the first fix of a diagnostic. Before #184, the first violation in map order won the position and the fixes of the other roots were dropped, so `-fix` needed several passes.
- Every diagnostic of a root carries the root's fix, not only the first one (#246). Before, the root was marked fixed on its first violation, even when an ignore directive suppressed that diagnostic or another function had already reported the position, so the fix was lost. An editor also offered the quick fix only on the first diagnostic. Each fix is complete on its own (#200), so the copies are identical, and the drivers apply a shared edit once. The checker generates each root's fix once and reuses it. A root with a diagnostic that an ignore suppresses gets no fix at all (#312). The ignore says that reuse is meant, and the fix changes it too: it isolates the root, or reassigns the ignored statement, so the next run reports the ignore as unused. The checker asks `Ignores.Covers`, which marks nothing used, for every violation of the function before it builds a fix. Deleting the ignore in the same fix was rejected: the fix must also know that the ignore covers no other diagnostic, of any kind or variant, and that it resolves every diagnostic of the root, before the pass has reported them all. `testdata/src/gormreuse/fix_ignored_root.go` holds the cases, and `testdata/src/converge/fix_escape.go` checks that the fix of another root leaves the ignore used.
- `RunSSA` and the directive checks do not report in position order: the unused-directive reports come from map ranges. `internal.ReportInOrder` holds back every diagnostic of the pass and reports them sorted by file name, offset, and message (#273). The text output is sorted by the driver anyway, but `-json` lists diagnostics in report order. File names are compared, not `token.Pos`, because the loader parses files concurrently and their order in the file set can vary.

`TestDeterministicDiagnostics` runs the analyzer 50 times over `testdata/src/determinism` and requires identical output, reported in position order. `testdata/src/determinism/unused.go` holds unused directives of several kinds for the order check. Range over a map only where the order cannot reach a diagnostic, or sort first.

### Design Patterns

1. **Composition over Inheritance**: `Analyzer` composes `RootTracer`, `cfg.Analyzer`, `pollution.Tracker`
2. **Type Switch Dispatch**: `handler.Dispatch()` routes SSA instructions to handlers via type switch
3. **Two-Phase Detection**: First collect all uses, then detect violations via CFG reachability
4. **Separation of Concerns**:
   - `tracer/`: WHERE is the mutable root? (value tracing)
   - `pollution/`: WHAT uses exist and are they violations? (state tracking)
   - `cfg/`: CAN use A reach use B? MAY a use run again? (control flow)
   - `handler/`: HOW to process each instruction type? (dispatch)

### Key Design Decisions

1. **SSA-based analysis**: Uses `go/ssa` to track `*gorm.DB` values through method chains
2. **Pollute model**: Tracks pollution state of mutable values (branching = pollution)
3. **Branch detection**: Detects when mutable `*gorm.DB` is used in multiple code paths
4. **Mutable root finding**: Traces back to find the origin of each chain
5. **Conservative approach**: Prefer false positives over false negatives (reduces false-negatives)
6. **IIFE return tracing**: Traces through immediately invoked function expressions to find mutable roots
7. **Method value tracking**: `callsite.SiteOf` reads a call through a method value (`find := q.Find; find(nil)`) as the method call it stands for

### Skipped files and internal errors

Code is left out of the analysis in two ways only, and neither is silent (#186).

- **Generated files.** `isGeneratedFile` in `analyzer.go` decides this per file. A package that imports `"C"` reaches the analyzer as cmd/cgo's output, not as the user's files. Each output file starts with `// Code generated by cmd/cgo; DO NOT EDIT.`, then a `//line` directive back to the user's file, then the user's file with its comments. `ast.IsGenerated` alone would skip every user file of a cgo package. So when that header is followed by a `//line` directive before the package clause, the header is ignored and the user's own comments decide. A user file marked `// Code generated ... DO NOT EDIT.` is still skipped. cgo's own files, such as `_cgo_gotypes.go`, have no such `//line` directive and are still skipped. All positions still come from `PositionFor(pos, false)`, so the file keys are the cgo output files, consistently. `testdata/src/cgofile` covers this with a real cgo build, and is skipped without a C compiler.
- **A panic while analyzing a function.** `panicGuard` in `internal/guard.go` recovers it, so one function cannot abort the run for the package. It reports `gormreuse: internal error analyzing <func>: <panic>; please report this at ...` at the function, once per function, through `pass.Reportf`. No ignore directive suppresses it. Before #186 the panic was dropped with every diagnostic of the function. `GORMREUSE_DEBUG_PANIC=1` still re-panics with the function name, for a stack trace.
- **A panic leaves the conservative result (#315).** `panicGuard.run` reports whether the work finished. A function whose `pure`, `immutable-return` or `immutable-input` check panicked is treated as breaking the contract, so it is not trusted and exports no fact for it. One whose `immutable-param` counterfactual panicked counts as needing the directive, so no fix deletes it. The work that reads the whole package runs under `panicGuard.runPackage`, which reports `gormreuse: internal error in <stage> of package <path>: ...` at the package clause of the first file. After a panic there, nothing is inferred immutable-return, no callback is exempt as immutable, and every `immutable-return` and `immutable-input` contract is left out of the facts. A panic while collecting the Scopes callbacks leaves none, so their parameters are not always mutable; that one reports less, and the internal error says so. `TestPanicInjection` in `internal/guard_injection_test.go` injects a panic into each stage.

The `-fix` drivers do not apply fixes in cgo files: they skip every fix that edits a file `ast.IsGenerated` reports, and the cgo output carries cgo's header. The diagnostics are still reported.

### Pollution Sources (Safe Side)

The linter marks `*gorm.DB` as polluted in these scenarios:

- **Interface method calls**: Assumed to pollute (can't statically analyze)
- **Channel send**: `ch <- db` marks db as polluted
- **Slice/Array storage**: `[]*gorm.DB{db}` marks db as polluted
- **Map storage**: `map[string]*gorm.DB{"k": db}` marks db as polluted
- **Interface conversion**: `interface{}(db)` marks db as polluted (type assertion may extract), except a read-only argument of a query method (#185)
- **Function arguments**: passing `*gorm.DB` to a function that is not trusted as pure is a branch. A `*gorm.DB` result assigned back in a merge is an assignment, as for a gorm method
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
  - The loop-carried check covers fields and globals only. The local form
    `for … { q = q.Where(c); q.Find(nil) }` is not reported
    (`fsoLocalLoopCarried`, see Known Limitations).
  - Outside a loop the rebuild is not an assignment: `r.db = q; r.db =
    r.db.Where("y"); q.Find(nil)` is reported. Inside a loop, as with locals,
    the value read on the first iteration is not counted as branched.

Bound Method Tracking:
  find := q.Find    <- MakeClosure with receiver q in Bindings[0]
  find(nil)         <- SSA: Call with Value=*ssa.MakeClosure, Fn.Name()="Find$bound"
                   <- Receiver extracted from Bindings[0] for pollution tracking

  w := db.Session(&gorm.Session{}).Where
  w("a").Find(nil)  <- w("a") is its own root, like a direct .Where("a") (#178)
  w("b").Find(nil)  <- OK: the receiver is immutable, so each call forks

Reassignment Behavior:
  q := db.Where("x")   <- SSA: q_1 = call db.Where("x")
  q.Find(nil)          <- First branch from q_1 - OK
  q = db.Where("y")    <- SSA: q_2 = call db.Where("y") (NEW SSA value)
  q.Find(nil)          <- First branch from q_2 - OK (no relation to q_1)

  Each assignment creates a new SSA value, so reassignment = new mutable root.

User-Defined Function Reassignment:
  q := db.Where("x")
  q = buildQuery(q, "y")   <- first branch of q_1; q_2 is a new mutable root
  q = buildQuery(q, "z")   <- first branch of q_2
  q.Find(nil)              <- First branch from latest q - OK

  buildQuery(q, "w")       <- Result discarded: a branch of q
  q.Find(nil)              <- VIOLATION: second branch

  r := buildQuery(q, "v")  <- a branch of q, as for r := q.Where("v")

  When the function returns *gorm.DB and isAssignment finds a merge that
  hands q's root back, handleArgs uses RecordAssignment, as
  handleReceiver does for a gorm method.
```

### Results the tracer cannot see through (#176)

`traceCall` makes a call its own mutable root when its result holds a `*gorm.DB` and the callee is not known to return an immutable value. Before #176 it returned `nil` (immutable) in two cases, against the conservative principle:

| Call | Root | Immutable when |
| --- | --- | --- |
| `q := helper(db)`, user function without `immutable-return` | the call | its body proves an immutable result ([Inferred immutable-return](#inferred-immutable-return-261)) |
| `q, err := helper(db)`, tuple result | the call, reached through the `Extract`; for a trusted pure callee, the returned argument's root (#188) | the callee is `immutable-return`, declared or inferred, or is pure and returns only immutable arguments |
| `q := f(db)`, `f` a func-typed parameter, field or merged func value | the call | never |
| `q := r.Query()`, interface method | the call | never |

Decisions:

- **A trusted pure function's tuple result is an alias of its argument** when its body may return that argument, as for a single result ([Returned arguments](#returned-arguments-188)).
- **`immutable-return` applies to a tuple result.** The signature check already accepted any result list with a `*gorm.DB`, and `ImmutableReturnValidator.Validate` already checks every `*gorm.DB` operand of each `return`. So `//gormreuse:immutable-return` on `func open() (*gorm.DB, error)` makes its `*gorm.DB` immutable at call sites, and returning `db.Where(...)` from it is reported at the declaration.
- **The tuple call is the root**, not the `Extract`. An `Extract` has no source position, so the message could not say `root at file:line`. This matches what `traceCall` already did for a closure with a stored tuple result.
- **A dynamic or interface callee is mutable.** Nothing says what it returns. A repository interface whose method returns a fresh handle, such as `r.DB()`, is now reported when its result is branched twice. Write `r.DB().Session(&gorm.Session{})`, or `//gormreuse:ignore`. No directive applies to an interface method, so `immutable-return` on the implementation does not help a call through the interface.
- **`q, err = helper(q)` is a reassignment**, like `q = helper(q)`. `handleArgs` accepts a tuple result, and `isAssignment` follows the `*gorm.DB` `Extract` to the merge.
- **No suggested fix for a tuple root.** `-fix` appends `Session()` to the root's call, and `helper(db).Session(...)` does not compile when `helper` returns two values. `Generate` returns no fix when the root is not a `*gorm.DB`.

Limitation: every `*gorm.DB` element of one tuple shares the call as its root. `a, b := pair(db)` followed by `a.Find(nil); b.Find(nil)` is reported as a second branch. This errs on the side of reporting. `testdata/src/gormreuse/opaque_results.go` marks it `[LIMITATION]`.

### Calls through a method value (#178)

The tracer resolves a call `w(x)` through a method value `w := r.m` as the call `r.m(x)`. `callsite.SiteOf` reads it as a `SiteBound` site: `Declared` is the declared method (`origin.Declared` of the `m$bound` wrapper), `Recv` the receiver (`MakeClosure.Bindings[0]`), and `Args` the other arguments. `traceCall`, `traceAll`, `isImmutableSource`, `pureReturnedArgs` and the Session/Debug copy checks (`aliasSharesStatement`, `Site.SessionEffect`, `collectAliasedRoots`) all read the call through it. The call handler (for a plain call, `go` and `defer`), the pure validator and `pollutionsource.CallReadOnlyArgs` read it the same way. Since #257 so does every other reader: the immutable-input body check, the immutable-return body check, the callback collectors and `DropReachableCallbacks`, the Scopes warning, the read-only stdlib check, the closure-result check and the fix generator. So:

| Code | Result |
| --- | --- |
| `w := q.Where; w("a")` | branch of `q`'s root (call handler), and a new mutable root |
| `w := q.Where; q = w("a")` under a condition or in a loop | an assignment of `q`, as for `q = q.Where("a")` (#254) |
| `f := q.Find; defer f(nil)` | a deferred branch of `q`'s root, as for `defer q.Find(nil)` (#254) |
| `w := db.Session(&gorm.Session{}).Where` | each call is a fresh root; no reuse |
| `s := q.Session; c := s(&gorm.Session{})` | `c` is a copy that shares `q`'s Statement, as with `q.Session(...)` |
| `s(&gorm.Session{Initialized: true})` | mutable root |
| `f := r.build; f()` | a call of the user method `build`, like `r.build()` |
| `f := o.open; q, err := f()` | the call is a mutable root, as for a tuple result of `o.open()` (#176) |
| `f := h.same; f(q)`, `same` pure and returning its argument | an alias of `q`'s root, as for `h.same(q)` (#188) |

**Rejected: trace into the wrapper as an IIFE.** This was the old behavior. The wrapper's body is one `r.m(x)` call, so every call through `w` traced to that one call. They shared one root: two calls through a method value of an immutable receiver were a false second branch, and on a mutable receiver the chained use was reported again, with no root position, since the wrapper call has none.

An interface method value has no declared method, so `origin.Declared` keeps the wrapper. Its signature has no receiver, so a call through it is handled like any other call whose result holds a `*gorm.DB`: a mutable root, as for an interface call `r.Query()` (#176).

A method value's wrapper is never analyzed as a closure of its caller, even when its receiver holds a `*gorm.DB` and so the `MakeClosure` captures one (`processFunction`, `isMethodValue`). Analyzing it read the wrapper's parameter as a mutable parameter and reported its forwarding call to an `immutable-param` method at no position (#255). As a last guard, `Analyzer.Analyze` moves a violation without a position to its call, then to the analyzed function (`positionedViolation`). A diagnostic at no position cannot be suppressed, and golangci-lint drops it.

Suggested fixes: a second branch made through a method value (`w := q.Where; w("a").Find(nil); w("b").Find(nil)`) gets `.Session(&gorm.Session{})` at `q`'s root. A call through `w` names no variable to rebind, so it never gets a reassignment. When one is a discarded statement (`w("a")` or `w("a").Limit(1)`), it changes `q` in place, and a Session at the root would drop that change. `discardedChainToReassign` returns no receiver for it, so no fix is offered, as for any discarded chain that cannot be reassigned (#199). `e2e/internal/fixsql` (`MethodValue`, `MethodValueDiscarded`) checks both against real GORM. A root created by a call through a method value (`q := w("a")`) gets `.Session(&gorm.Session{})` appended.

### Calls through a method expression (#258)

A method expression `T.m(r, x)`, or `(*T).m`, is a call of a synthetic `m$thunk` function whose first parameter is the receiver. `callsite.SiteOf` reads it as a `SiteThunk` site: `Declared` is the declared method (`origin.Declared`), `Recv` the first argument and `Args` the rest, as for the static call `r.m(x)`. A package-level variable that holds a method expression (`var find = (*Repo).find`) is read the same way (#257). So every directive of `m` applies (pure, immutable-return, immutable-param, immutable-input, the returned arguments of a pure method), also for a method of another package, and `(*gorm.DB).Find(q, nil)` is a gorm method call on `q`. The immutable-param isolation fix counts the receiver argument of a method expression (`receiverArgs`). `callback.DropReachableCallbacks` reads a thunk through `origin.Declared` too.

The thunk of a method promoted through an embedded field (`type W struct{ *gorm.DB }; W.Find(w, nil)`) selects the field inside its body. `Recv` is then `w`, not `w.DB`, and the tracer does not follow it to the field, so reuse through it is missed (`exprPromoted`). An interface method expression has no declared method, so it is a call of an unknown function.

### One handler for every call site (#254, #252)

A call, a `go` statement and a `defer` statement are each one `callsite.Site`, and `handler.handleSite` handles all three. It does the same thing whether the site names its function directly, through a method value or through a method expression:

- The receiver of a `*gorm.DB` method is a branch of its root. A method that returns an immutable copy (`Session`, `Debug`, ...) only reads it. A plain call whose result reassigns the variable is an assignment (`isAssignment`).
- An argument passed to a function that is not `pure` is a branch, also when it is boxed in an interface (`pollutionsource.UnwrapGormDB`) and when the callee is unknown (`go fn(q)` with `fn` a parameter). A read-only argument of a `*gorm.DB` method is a read (#185). The `immutable-param` caller contract is checked for each argument.
- A Session/Debug copy, or a chain that holds a subquery, records an alias use.
- A branch on every iteration of a loop that the root is defined outside of is reported at once (`cfg.Repeats`). Before #254 only the receiver of a plain call was checked. Now `for … { go q.Find(nil) }`, `for … { defer q.Find(nil) }` and `for … { helper(q) }` are reported too, as `helper(q); helper(q)` already was. In gorm's own code this adds two reports, both a `*gorm.DB` parameter passed to a callee in a loop (`plugin.Initialize(db)` in `Config.AfterInitialize`, `preloadEntryPoint(db, ...)` recursing per relation).

The mode (`Site.Mode`) decides only two things. How an earlier change is found: a plain call leaves it to `DetectViolations`, which orders uses by position; a `go` statement checks the changes that reach its block (`IsPollutedAt`), and a `defer` checks every change (`IsPollutedAnywhere`). And what a branch records: a plain call records a use; a `go` or `defer` records a branch use, which a later `go` or `defer` sees and position-ordered detection skips. `go` and `defer` statements are handled after the other instructions of their function, so every plain use is recorded by then. A root with no plain branch, as in `go q.Count(nil); go q.Find(nil)`, names as its first branch the earliest branch use recorded before the reported one (#309).

Before #254, the method-value path always recorded a branch, so `if c { w := q.Where; q = w("a") }; q.Find(nil)` was reported. The `go`/`defer` path read a method value as its `m$bound` wrapper, which has no receiver, so `f := q.Find; defer f(nil); q.Count(nil)` was missed. It also had no `pure` exemption, did not unwrap boxed arguments, ignored read-only arguments and calls with no static callee, and recorded no alias uses. `pollutionsource.isReadOnlyUse` now accepts a `go`/`defer` statement too, so the varargs of `defer db.Find(&rows, "id IN (?)", sub)` are not a leak. Since #309 so does `isReadOnlyVariadicArg`, so `defer log.Printf("%v", q)` is not a leak either, for the handler and the pure validator alike.

**A value that may hold several roots (#252).** After `q := b; if c { q = a }`, `q` resolves to `b` (`FindMutableRoot`), and `FindAllMutableRoots` returns `a` too. Before #252 only `b` got the use; `a` was only checked for an earlier change. So `q.Find(nil); a.Find(nil)` was missed. Now the other roots get a *may branch* (`Tracker.RecordMayBranch`). `DetectViolations` checks a may branch against the root's own uses, both ways: a use of `a` after it, and a polluting use of `a` before it. The second also catches `boundMethodWithPhiOnePollutedReverse`, where the polluting branch is in a block that is handled after the merge. A `go`/`defer` checks its first root against may branches too.

Decisions:

- **Two may branches are never checked against each other.** Two values that each may hold a root can be exclusive. In `chaosVariableSwapLoop`, `q1` and `q2` swap in a loop, and each may hold either original root, but never the same one. Reporting such pairs would be a false positive for every swap.
- **Rejected: record the use for every root.** It reported the swap above, and every pair of values that share a root only through correlated merges.
- **A may branch is recorded only for a root defined before the site in this run** (`siteUse.definedBefore`): its block dominates the site's, or it is outside every loop, or in another function. In a loop body, a value read before a root's definition holds the root of the previous iteration. A may branch of it would pair with this iteration's uses (`chaosUltimateStressTest` in `nested_chaos.go`).
- A root with may branches has hidden uses for the fix (`Violation.HiddenUses`): the fix cannot order them, so it adds a Session to every root with a later use.

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

### Uses that run more than once (#249, #252)

A use that may run more than once for one run of its root's definition reuses the root, even with one use in the source. `cfg.Repeats.RepeatsAfter(use, def)` is the one query for it. The handler asks it for every root of a branch, of a call, a `go` or a `defer` (#306). The suggested fixes ask it too (`usedAgainInLoop`, `changeSeenAfterEscape`, the Session of a reassignment in a loop), so a fix and the diagnostic agree. `RunsAgain(block)` asks only whether a block can run after itself, for a loop-carried read (`mergedStoreReadTwice`).

`RepeatsAfter` is true when:

| Case | Example |
| --- | --- |
| The use is in a loop that does not hold `def` | `q := …; for … { q.Find(nil) }`. Also a root of an outer loop used in an inner loop, and a root of one loop used in a later loop |
| The use is in a closure that runs at two sites that may both run for one run of its `MakeClosure` (one can reach the other) | `f := func() { q.Find(nil) }; f(); f()`, `g.Go(f); g.Go(f)` |
| A site where the closure runs is in a loop of the declaring function that does not hold `def`, at any depth | `for … { f() }`, `for … { func() { … }() }`, `for … { go func() { … }() }`, `for … { g.Go(func() error { … }) }`, `for … { funcs = append(funcs, func() { … }) }` |
| A site is in another closure that runs more than once, or in a loop of it (#305) | `run := func() { for … { update(id) } }; run()`, `db.Transaction(func(tx *gorm.DB) error { update(1); return update(2) })`, `go func() { for … { update(id) } }()` |

A site is any use of the closure value: a call, a `go` or `defer` statement, an argument, a store. `callsite.ClosureRuns` finds them. A comparison with nil (`if f != nil { f() }`) does not run the closure, so it is not a site (#309). A Phi of the closure, and a local variable that holds it, are followed to their uses, so `held = f` in a loop with `held()` after it is one site after the loop. The variable is followed into the closures that capture it, at any depth, so a call of `update` inside `run` is a site in `run`, and asks `run`'s own `Repeats`. Before #305 the capturing `MakeClosure` was one site in the declaring function, so `update` called in a loop of `run`, or twice in a `Transaction` callback, an IIFE or a goroutine, was counted once. A slice element or a struct field is not followed: the store into it is one site, and a call through it is a dynamic call (`repeatsClosureCalledThroughSlice`). Calls on two exclusive branches (`if c { f() } else { f() }`) are not two runs.

`ClosureRuns` also gives the checker and the tracer a closure's plain calls (`callsite.ClosureCalls`): the single call that orders and places a closure's uses ([Closure uses](#closure-uses-68-179)), and the calls at which a captured variable is read (#251). Before, each of the three walked the referrers in its own way, and a call through a captured variable was a site for one and not for the others.

Decisions:

- **Nested loops (#252).** `DetectLoops` keeps one block set per loop header. `markLoopBlocks` used one set for every loop as its visited set, so the walk of an outer loop stopped at an inner header marked before it, and the outer loop's blocks before the inner loop were not in a loop. `IsInLoopWithout(use, def)` asks for a loop that holds the use but not the definition. Before, any loop counted, and a definition in any loop was taken to run as often as the use. So `for … { q := …; for … { q.Find(nil) } }` and a use in a later loop were missed.
- **The blocks before a `for cond` loop (#252).** Back-edges are found by block index, and go/ssa puts the header of `for cond { … }` after its body. The edge from the header to the body is then taken as the back-edge, with the body as its header, which does not dominate the edge's source. The walk from it went on through the blocks before the loop, up to the entry. So a root defined at the entry, such as a parameter, counted as defined in a loop, and a use before the loop in a closure counted as repeating. The walk now keeps to the blocks that its header reaches.
- **A definition in another function.** A definition in a function that declares the use's function, at any depth, is asked at the site where the closure runs. A definition in a closure that the use's function runs, such as a root returned by an IIFE, is moved to the block where that closure runs when it runs at one site, or where it is declared otherwise. Before, a root an IIFE returned in a loop was reported on every iteration. A definition in an unrelated function, and a global, are outside every loop and closure.
- **A closure analysed on its own** (each closure is also a function of the package) asks only its own loops. The analysis of the function that declares it asks the sites, so one reuse is not reported twice.
- **Sites in two functions may both run (#305).** A closure called once in the declaring function and once inside another closure, or inside two closures, is taken to run twice, even when the calls are on exclusive branches (`rcsExclusiveAcrossClosures`). Ordering the two would mean lifting each site to the sites of the closure that holds it. A site in the closure itself, or in one nested in it, is a recursive call, so the closure runs again. Two closures that call each other each ask for the other's sites. A question asked again while it is being answered counts as a repeat, since they recurse.
- **A closure made on each iteration (#306).** `for … { save := func() { … }; if c { save() } else { save() } }` makes a new closure on each iteration, and each runs once. Two sites in the declaring function may both run only on a path that does not run the `MakeClosure` again. The path through the back-edge does, so the two calls are exclusive. This holds only when every site uses the `MakeClosure` itself. A Phi or a variable may still hold the closure of an earlier iteration, as `if c { last = save }; last()` does, so then any path counts, and one site that can reach itself runs again (`rcsPerIterationKept`). Before #309 the `last != nil` of that fixture counted as a second site, and a single site was not asked. Before #306 the back-edge made every pair of sites in a loop reach each other, and the case was reported.
- **Every root of a merge (#306).** After `q := db.Model(nil); if c { q = q.Where("a") }`, a use of `q` in a loop reuses both roots. Before, only the first root (`FindMutableRoot`) was asked, so the first run reported only `Model`, its fix added a Session there, and a second run reported `Where`. Now each root that the merge may hold is asked, except a root the use reassigns and an immutable call such as a `Session` result, which `FindAllMutableRoots` keeps (`rcsMergedSessionInLoop`). The violations share the position, so one diagnostic carries a fix for every root. The tracker files the use under the other roots as a may branch, which `AllUses` did not list, so their fix had no use. `Tracker.CollectViolations` now lists the may branch at the violation's position.
- **A closure passed to another function runs once** unless the site is in a loop or there are two sites. `slices.SortFunc(s, func(a, b T) int { q.Find(nil); … })` is missed. Counting every argument as a repeat would report each `db.Transaction(func(tx *gorm.DB) error { … })`.
- Where a reuse is reported follows the closure's position rules ([Closure uses](#closure-uses-68-179)): at the body, or at the one later call site. `f := func() { q.Find(nil) }; for … { f() }` is reported at `f()`.

**One CFG cache per pass (#299).** `run.go` makes one `cfg.Analyzer` and hands it to the checker (the pollution tracker and the handler context of every function) and to `fix.Generator`. For each function it keeps:

| Cached | Filled | Before #299 |
| --- | --- | --- |
| The blocks each block reaches, as a bitset indexed by `b.Index` | On the first `CanReach` from that block | A breadth-first search on every `CanReach` |
| `LoopInfo` (`DetectLoops`) | Once | Once per function analysed, and once per use in the fixes |
| `Repeats` on its own (`Repeats`), as a closure of each parent `Repeats` (`Closure`), and the closure's sites | Once | `RepeatsOf` rebuilt the chain, with `DetectLoops`, on every use the fixes asked about |
| Whether a closure's sites repeat after a definition (`sitesRepeatAfter`) | Once per definition block | Asked again at every depth, so `k` sites per level of `d` nested closures took `k^d` steps |

`Closure` stops comparing sites once two may both run (`multi` answers every question then). Whether two sites in one function may both run takes one pass over the function: each block is labelled with at most two of the sites whose paths reach it (`repeatRunReachesAnother`). So a closure called on `n` exclusive branches takes time linear in the function, not `n` reachability questions. The sites of a closure are found from its referrers, not by scanning the declaring function, so a chain of `n` closures that call each other is not quadratic either (`loopclosurecases`, `closurechain` and `closurecallers` in the growth gate). And `mergedStoreReadTwice` stops at the second use after a store. The results do not change: the goldens, and `-json` and `-fix -diff` over `e2e/internal`, the testdata and gorm's source, are byte-identical. A block that is not `fn.Blocks[b.Index]`, such as one built by hand, is still searched. The tracer keeps its own `cfg.Analyzer` for `closureRunsRepeatedly`. An Analyzer is not safe for concurrent use, and a pass runs on one goroutine.

In gorm's own source (the root module), the loop fixes add six reports (154 to 160). Each is a `*gorm.DB` parameter branched in a loop: in `Scan` (three), `CreateInBatches` (the parameter of its batch closure), `SaveAfterAssociations` and `SetupUpdateReflectValue`. Before, the parameter counted as defined in a loop, because a `for cond` loop in the function marked the entry block, or the use's block was left out of an outer loop. None comes from the closure rules. The `tests` module is unchanged (34).

### Range-over-func Loops

SSA compiles the body of `for v := range seq` (Go 1.23+) into a synthetic closure that the iterator calls once per element. go/ssa marks it with `Synthetic == "range-over-func yield"`. The closure has no back-edge, so `DetectLoops` treats every block of it as in a loop (#177). A root defined outside the body and used inside it is then reported, as in a plain `for range`. A root defined inside the body is in a loop block, so it is fresh on each iteration.

The body reaches outer variables through `FreeVar`s, so `q = q.Where(v)` inside it is a `Store` to a `FreeVar`, not to an `Alloc`. `isAssignment` treats a `FreeVar` as a merge point like an `Alloc` (see the previous section). Its values are those of the captured variable, so the store is an assignment only when that variable can hold `root`. That is the in-place update of a plain loop. A store into another variable, such as `last = q.Where(v)`, still counts as a use of `q`.

A closure nested inside the body (an IIFE, a `defer`) runs once per run of the body, so it repeats as the body does ([Uses that run more than once](#uses-that-run-more-than-once-249-252)).

### Closure uses (#68, #179)

**Ordering (#68).** A closure may be called at a *single* later call site (`f := func() { q.Find(nil) }; …; f()`). Then the uses inside it are ordered by that **call-site position**, not by the body's source position. So a closure defined early and called late is reported at the call site, and the earlier direct use is the first branch (`storedClosureReordered` in `evil.go`). Only this single-call case moves. IIFEs (called in place), deferred or spawned closures, and closures called from several sites keep their body positions.

**Reachability (#179).** The same single invocation, and also an IIFE, gives the closure an invocation site: the block of its one plain call (`Tracker.RecordInvocationSite`). When two uses are in different functions, `isReachable` lifts each use through these sites to the innermost function that holds both, and asks the CFG whether the first site reaches the second. So `if c { f() } else { q.Count(nil) }`, `if c { f(); return }; q.Count(nil)` and one IIFE per branch are not reported. Positions still decide the order. If the two sites are the same block, or no common function is reached, the pair counts as reachable, as before #179. A closure with no single plain call (more than one call, `defer`, `go`, passed as an argument, stored) has no site, so its uses stay conservative. A nested closure without its own site does not inherit its parent's site either: it can escape the parent and run after it.

**Reassigned captured variables (#251).** A variable that a closure captures and that is reassigned lives in an `Alloc`, with no Phi. Before #251 every read of it resolved to its first store, often the parameter it was initialized with, so `db = db.WithContext(ctx); db.Find(nil); func() { db.Count(nil) }()` reported two branches of the parameter, and `-fix` added a redundant Session. Now `allocReachingStoreValues` matches stores with the CFG-ordered search of #182 (`observedFieldStores` with an empty path):

| Read | Stores it sees |
| --- | --- |
| In the function that declares the variable | The stores that reach the read |
| In a closure declared there whose only uses are plain calls (`f()`, an IIFE) | The stores that reach any of those calls |
| In any other closure: deferred, spawned, stored, passed on (errgroup's `Go`, a `Transaction` callback), a range-over-func body, or nested in another closure (#303) | The stores that reach the `MakeClosure` in the declaring function (the outermost one, for a nested closure), plus every store that can run after it (`fieldStoresAfter`) |

**Why the MakeClosure (#303).** A closure cannot run before it is created, and once created it may run at any later point, also after the function returns. So the value it reads is the one held at its creation or one stored later. Before #303 such a closure saw every store, so `db = db.WithContext(ctx)` followed by `g.Go(func() error { return db.First(&u, id).Error })` in a loop, or by `for _, u := range slices.All(us) { db.Model(&u).Update(...) }`, resolved the read to the parameter and reported it. A store after the `MakeClosure`, or later in a loop that creates it, is still seen (`cosStoreAfterSpawn`, `cosStoreInLoop`). The rule is not tighter for a deferred closure (the stores that reach the function's exits), since a deferred closure can also be stored or passed on before it runs.

**Fields through a captured pointer (#303).** A field read through a pointer variable that a closure captures, such as `d := &deps{db: db}`, follows the same rule: `fieldStoredValuesAt` passes the load to `observedFieldStores` also when the address is defined in an enclosing function. Before #303 it passed a nil load, so every store was seen. A captured pointer variable lives in an `Alloc`, so `d.db = ...` stores through a fresh load of it, not through the pointer the variable holds. `storeTarget` matches these stores to that pointer when it is the only value ever stored into the variable. Before #303 they were missed, in the declaring function too: `d.db = d.db.WithContext(ctx)` did not replace the parameter, and a store of a mutable value after the closure was not seen (`cosFieldStoreAfterSpawn`). With more than one store in the variable, a load of it may hold another pointer, so a store through it is not matched, as before.

**Cost (#325).** The tracer keeps these searches in a `storeMemo`, per address, field path and function:

| Kept | Per | Before #325 |
| --- | --- | --- |
| The stores that reach the start of a block (`fieldStoresInto`) | Block | A backward walk for every read. Only the part of the read's block before it is still scanned per read |
| What the loads in a closure see (`observedFieldStores`) | Closure | Every load walked back from every call site of the closure: a closure called at `n` sites with `n/10` loads took 9 s at `n = 800` |

The results are the same as the walks', so no report changes. `FieldStoredValues`, which the callback resolver calls with a nil load, keeps nothing (a nil `storeMemo`).

The single-root trace takes the first store it sees, in block order. A store made inside a closure is still not seen (`closureModifiesCaptured`). This also fixes `p := &q; *p = db.Where("y")`, which is a direct store to `q`'s `Alloc` (`reassignPointerDeref`). The fix's `mergedEdges` still gives a Session to a store that may run before two reads of the variable, so a store whose own reads need none can get a redundant one.

**Closure factories (#251).** A closure that returns a chain, such as `base := func() *gorm.DB { return db.Model(&User{}) }`, is traced through its returns like an IIFE, so every call of it shared the root inside its body. Calling it twice, or once in a loop, reported the second query, though each call builds a new `Statement`. `closureCallOwnsRoots` makes the call its own root when both hold:

- The closure may run more than once for one run of its `MakeClosure`, as `cfg.Repeats` counts it (#249): at two sites that may both run, or at a site in a loop that does not create it (`closureRunsRepeatedly`, which asks `RepeatsOf` of the tracer's own `cfg.Analyzer`). A closure called once in a loop is already safe without this rule, since `RepeatsAfter` places a root inside it at its one call site; two calls are not.
- Each root is a gorm method call in the closure whose result is only returned, and every `*gorm.DB` operand of the chain it ends is immutable or another such call in the closure. A chain from a captured mutable root, a closure parameter or a call of another function is shared by every run, so it keeps its root and stays reported (`cfFactoryCapturedRoot`, `cfFactoryParam`, `cfFactoryHelper`). So does a returned value that the body also uses (`cfFactoryBodyUse`).

The rule does not count the runs itself, so the checker, the fixes and the tracer agree on when a closure runs again.

### Struct roots (#181)

Three struct shapes lost the root before #181, so reuse through them was missed.

| Shape | SSA | Handling |
| --- | --- | --- |
| Promoted methods of an embedded `*gorm.DB`: `e := E{q}; e.Where("a")` | `t4 = *t0; t5 = t4.DB`: a `Field` of a struct in a register, not a `FieldAddr` | `fieldValueStoredValues` turns the `Field` chain over the load into (loaded address, field path) and matches stores as for a `FieldAddr` chain. Over a Phi, each edge is resolved the same way |
| A field of a returned struct: `s := mk(db); s.DB.Where("a")` | `*t0 = mk(db)`, or `mk(db).DB` directly | The field is a `FieldRoot{call, path}` (below) |
| A field of a returned pointer (#250): `p := mkPtr(db); p.DB.Where("a")` | `t1 = &t0.DB; *t1`, a `FieldAddr` chain on the call | `reachingFieldStores` matches stores through `t0` as for a local. When some path from the call reaches the load with no store, the field is also the `FieldRoot{call, path}` |
| A struct captured by a closure: `f := func() { h.q.Where("b") }` | `&h.q` on a `FreeVar`; with `h := &T{...}`, `&(*h).q` on a fresh load of the captured pointer variable | `ClosureCapturesGormDB` looks through struct fields (`closureHoldsGormDB`), so the closure is analyzed. `fieldBaseAddrs` maps a `FreeVar` to its binding and a load of a pointer variable to each pointer stored in it, and the stores are searched in the function that defines that address |

**`FieldRoot`.** A call returning `*gorm.DB` is a mutable root. A call returning a struct that holds one has no SSA value for "field `DB` of this call", and every read is a new instruction. So the tracer makes a `FieldRoot` that implements `ssa.Value` and interns one per (call, field path) in `RootTracer.fieldRoots`. Reads of the same field of the same call share it; another field or another call is a different root. Its position is the call's. A call to a builtin or a `//gormreuse:immutable-return` function yields no root.

**One rule for value and pointer results (#250).** Before #250 a field of a struct returned by value was a root and the same field reached through a returned pointer was not. Both are now roots, for these reasons:

- The rule then matches "a call returning `*gorm.DB` is a mutable root". A helper such as `newTestApp(t)` that returns `*gorm.DB`, a struct or a pointer to a struct is treated the same. In each case `//gormreuse:immutable-return` on the helper marks its handles as safe.
- A returned pointer is not like a parameter. The caller usually gets a struct the callee has just built, so a mutable chain in it is as likely as in a returned value. The parameter trade-off stays because a parameter is most often the application's shared handle.
- Making the value case match the parameter case instead would drop the #181 detection and its E2E evidence.

The pointer walk stops at the call (`reachingFieldStores`): a store above it on a loop path wrote the struct of an earlier iteration. Only a `FieldAddr` chain directly on the call is followed. A pointer field inside the result (`p.inner.DB` with `inner *T`) is not.

**Defining block (#250).** The loop check reports a use in a loop of a root defined outside it. It used to treat every value that is not an instruction as outside, so a `FieldRoot` from a call inside the loop was reported on every iteration. `tracer.RootBlock` now gives each root kind its defining block, and `cfg.Repeats.RepeatsAfter` takes that block with the use's block. A root defined in another function is placed as [Uses that run more than once](#uses-that-run-more-than-once-249-252) describes. The suggested fixes ask the same query.

| Root kind | Defining block |
| --- | --- |
| A call, a Phi (instructions) | Its own block |
| `FieldRoot` | The block of its call (`FieldRoot.Block`) |
| `*ssa.Parameter` | The entry block of its function. That block is in a loop only for a range-over-func body, whose parameters are the values of the current iteration, so `for q := range seq { q.Find(nil) }` is no longer reported |
| Anything else | None, so outside. The tracer returns no other kind: it follows a `FreeVar` to its binding and a global to its stores, and a constant is no root |

A root defined in another function, such as an enclosing function's root used in a closure, has a block that is not in the closure's loops, so it is outside, as before.

Decisions and limitations:

- No fix is suggested for a `FieldRoot`. `Session()` cannot be appended to a call that returns a struct. It belongs where the function builds the struct.
- A field read through a parameter or a receiver is still not a mutable root, whether the struct is passed by value or by pointer. That trade-off is unchanged. Since #308 a getter that returns such a field is inferred `immutable-return`, so `d := r.DB()` is read like `r.db` ([Inferred immutable-return](#inferred-immutable-return-261)).
- A write to a returned struct inside another function (`p.Reset()`, `reset(&s)`) is not seen. A later read still sees the call's field, so it can be reported though the field was replaced. A closure capturing such a parameter sees only the stores in the enclosing function, as the function itself does.
- A captured pointer variable is resolved by its stores in program order: the single-root trace takes the first pointer stored into it. After `h = &T{q: b}`, a use of `h.q` can still be matched to the first value, and a store through `h` is not matched to either pointer. A captured `*gorm.DB` variable is now read by the stores that reach the read instead ([Reassigned captured variables](#closure-uses-68-179)). With one pointer ever stored, the fields are read the same way (#303).
- A struct value that a Phi merges (`e := E{a}; if c { e = E{b} }`) yields the field of every edge. The single-root trace takes the first edge, so the suggested fix makes only that edge immutable. A struct from a channel receive, or a map or slice element, is not followed.
- A `Field` read over a load uses the CFG-ordered store matching of #182 (`fieldStores`), so `e.DB = other` between two promoted calls is not a second branch. A read inside a closure has no CFG path to the stores in the enclosing function, so it matches all of them, and the single-root trace takes the first. This keeps reuse reported when the closure may run before a later store.

### Package-level variables (#183)

A package-level variable reaches the analysis in two ways: as a `*gorm.DB` that a function loads and stores, and as a `var f = func(...)` closure.

**Loads of a `*gorm.DB` global.** A load `*G` is matched to the stores to `G` in the load's own function that reach it, with the same CFG walk as a field (`fieldStores`, #182). `globalStoredValues` does this, and `isLoopFieldRebuild` treats `for … { G = G.Where(c) }` like the field form.

| Code | The load sees | Result |
| --- | --- | --- |
| `G = db.Session(s).Where("t"); G.Where("a").Find(nil); G.Where("b").Find(nil)` | the store | second branch reported |
| `G = G.Where("t"); G.Find(nil); G.Count(nil)` | nothing on the right, the store after it | second branch reported |
| `G.Where("a").Find(nil); G.Where("b").Find(nil)`, no store in the function | nothing | immutable |

The last row is a deliberate trade-off. A global that a function only reads usually holds a `gorm.Open` result, which is safe to branch. Treating every read of a global as mutable would report each second use of a shared handle. So the function's own stores decide, and a read that no store reaches is immutable. The `//gormreuse:immutable-return` body check reads globals the other way: it reports a returned global only when every assignment in the package is provably mutable ([Immutable-return body contract](#immutable-return-body-contract-194)). Both directions give the benefit of the doubt when the value is unknown.

Limitations:

- Stores in other functions are not seen. That includes the package initializer (`var gq = g.Where("pkg")`), a closure the function calls, and the enclosing function of a closure that loads the global.
- A store to a global is not a merge point. `G = db.Where("x"); if c { G = G.Where("a") }; G.Find(nil)` is reported, because `Where("a")` counts as a branch of the stored root. A struct field behaves the same.

**Package-level closures.** buildssa puts only declared functions and their literals in `SrcFuncs`. The closure of `var f = func(...)` belongs to the synthetic package `init`, so before #183 its body was never analyzed, a `//gormreuse:pure` on it was never validated, and a call through `f` carried none of its directives. Now:

- `runFuncs` adds the anonymous functions of `init`, nested ones included, to every pass. Their bodies get reuse detection and directive validation like any closure.
- A diagnostic names the closure by its variable, not `init$1` (#259). Every message that names a called function goes through `callsite.FuncVars.FuncName`, which also names a method value or method expression wrapper (`m$bound`, `m$thunk`) by its method, and a generic instance by its generic function. A closure held by a variable that is not resolved (see below) keeps its SSA name.
- `callsite.CollectFuncVars` maps a variable to its closure, and `callsite.SiteOf` reads a call through it as a `SiteFuncVar` site. Every reader of a call uses it, so every directive applies at the call site: `pure`, `immutable-return`, the `immutable-param` caller contract, and since #257 `immutable-input` (its callbacks are collected, and forwarding a callback to the variable keeps the promise). A Transaction callback or an `immutable-input` callback can also be held by such a variable (see "Callbacks held in a variable or a field (#257)" under [Directives](#directives)).

A variable is resolved only when the package stores to it once, the stored value is a function, its address is used only to load and store it, and it is unexported. Otherwise the call may run another function: a second assignment, a write through a pointer, or an assignment in another package. An unresolved call stays a dynamic call: it pollutes its arguments, and its result is a mutable root (#176).

### Local function variables (#304)

A local variable that holds a function and is never captured is no variable in SSA: `f := func(...) {...}; f(x)` calls the `MakeClosure` (or the `*ssa.Function`) directly. Once a closure captures `f`, such as a closure that calls it, a recursive closure, or an IIFE that reads it, `f` lives in an `Alloc`, the closure reads it through a `FreeVar`, and each call loads it first. Before #304 `SiteOf` resolved only a static callee or a load of a package-level variable, so every call through a captured `f` was a `SiteDynamic`: its directives were dropped, the `immutable-param` caller contract was skipped, an inferred immutable-return was lost, and an `immutable-input` callback (such as the recursive `transaction` wrapper in gorm's own transaction tests) was not collected.

`FuncVars.LocalValue` now resolves a load of a local `Alloc`, directly or through a chain of `FreeVar`s, to the value stored into it, and `SiteOf` reads the call as the call of that value: a `SiteClosure` for a function literal, a `SiteBound` for a method value, a `SiteStatic` or `SiteThunk` for a function. A stored load of another such variable, or of a package-level one, is followed. So the call is read exactly as the same call without the capture. The immutable-return body check (through the `Site` callee since #315) and the closure-factory rule (`closureCallOwnsRoots`) ask the same function, so there is one sole-store implementation (`funcVarSoleStore`), which replaced the body check's own copy.

A variable is resolved only when:

| Condition | Why |
| --- | --- |
| It has exactly one `Store`, in the function that declares it | A second store may give it another function. A store in a closure may run at any point |
| Its address is only loaded, stored to, or bound to a closure, at any depth of nesting | Any other use, such as `&f` passed to a call, may write it |

A read before the store holds nil, and calling it panics, so every call that runs, runs the stored value. A store in a loop stores a new closure of the same function on each iteration.

`FuncVars` remembers the result for each `Alloc`, so a variable called at n sites is scanned once. The zero `FuncVars` (`SiteOfWithoutFuncVars`) resolves neither package-level nor local variables. A closure called at n sites asks for its directive n times. Each answer walked the enclosing statement until #301, which [places every directive of a file once](#directives).

Limitations:

- A variable that a branch assigns (`if c { f = g }`) or that a closure assigns stays dynamic, as before.
- A closure that is stored in a variable read every store of a captured, reassigned variable until #303. It now reads the stores that reach its `MakeClosure` or run after it, so after `db = db.Session(&gorm.Session{})`, a factory `base := func() *gorm.DB { return db.Model(nil) }` that an IIFE also calls is a factory again (`cfvFactoryReassigned`).

### Closures that return an immutable value (#308)

A call of a function literal is traced into its returns (`traceClosureReturns`). It gave a nil root both when no return had a root and when the returns had several roots, and `traceCall` then made the call a mutable root in both cases. So `get := func() *gorm.DB { return conn }`, with `conn` immutable, made `d := get(); d.Find(nil); d.Count(nil)` a report.

`closureReturnsImmutable` now tells the two apart. It searches each return of the closure with `FindAllMutableRoots`, and the closure returns no mutable value when it has a `*gorm.DB` return and none of them has a root. Then `traceCall` and `traceAll` give the call no root.

| Closure | Call |
| --- | --- |
| Returns a captured value with no root, such as a `Transaction` callback's `tx` | No root |
| Returns a captured value with roots, or a parameter | Its own root, or the returned root for an IIFE chain, as before |
| Returns one of several roots | Its own root, as before |
| Has no return, or only panics | Its own root, as before |

Decisions:

- **A search of its own.** The single-root trace follows only the first store into a variable, so a nil from it does not prove that there is no root (`iwClosureCapturedReassigned`). `traceAll` shares its `visited` set with the rest of the search, and a value it has visited traces to nothing there, whatever its roots. So each return is searched afresh, and the answer is kept per closure.
- **Recursion.** A closure being checked counts as returning a mutable value, so a recursive closure ends and its calls stay roots, as before.
- **Inference does the rest.** Most such closures are also [inferred](#inferred-immutable-return-261) `immutable-return` since #308, which covers calls the tracer reads through `returnsImmutable`. The search here also covers values that no body proves immutable, such as an exempt callback parameter.

`TestAllocationGrowth` (`closuregetters`) holds a chain of `n` closures, each returning the call of the one before it, to linear growth.

## Pure Contract Validation

`purity.Validator` checks the body of every `//gormreuse:pure` function. It follows "parameter-derived" values and reports when one is polluted or escapes.

| Step | What it does |
| --- | --- |
| Seed | Every parameter whose type holds a `*gorm.DB` without going through an interface: `*gorm.DB`, `**gorm.DB`, `[]*gorm.DB` (variadic too), maps, channels, arrays, and structs with such a field. A method's receiver is not seeded. |
| Propagate | Through Phi, Extract, call results, loads (`*p`), channel receives, `FieldAddr`/`Field`/`IndexAddr`/`Index`/`Lookup`/`Slice`, conversions, range iteration, and stores into a local `Alloc` (`p := &db`, `h := holder{db: db}`). It repeats until nothing changes, so loop Phis and later stores are seen. |
| Call results | The result of a gorm chain method on a derived receiver, or of a call that is not trusted as pure with a derived argument, is derived. So is the result of a closure that returns a derived value it captures (`func() *gorm.DB { return db }()`), and of a trusted pure function when one of the parameters it may return (`ReturnedParams`) gets a derived argument. Session, WithContext, Debug and `immutable-return` functions return a fresh handle, so their results are not derived. |
| Closures | A `MakeClosure` whose bindings are derived seeds the closure's free variables and validates the closure body as part of the pure function (IIFE, `go func(){...}()`, stored closures). The `MakeClosure` is derived itself (#308). Stored in a variable that another closure captures, it makes that variable derived, so a call of it there is seen: `get := func() *gorm.DB { return db }; func() { get().Where("x") }()` is reported. A call through the variable is read as the call of the closure it holds (`FuncVars.LocalValue`, [Local function variables](#local-function-variables-304)). |
| Check | `Call`, `Go` and `Defer` share one check. `*gorm.DB` method values (`Find$bound`) are checked where they are created. Stores outside local variables and `pollutionsource.Leak` cover escapes. |

A violation is **definitive** (`Violation.Definitive`) when the argument is polluted by a gorm chain method, escapes, or is passed to a pure function that already failed. Only definitive violations put the function in `PureVerdicts.Failed`, which makes the tracer stop trusting it at call sites. Passing the argument to a function that is not marked pure stays conservative and does not revoke trust (`nestedClosureOuterPureViolation`, PV117).

Failure is transitive (#187). `validatePureFuncs` in `internal/pure.go` validates the pure functions from a worklist until `PureVerdicts` stops changing, then reports from a final round. When a verdict changes, the pure functions that call or refer to that function (`pureCallers`, closures inside them included) are validated again. The failed set and each returned set only grow, and larger ones only add violations and returned parameters, so this ends, at the same verdicts in any order. A chain of `n` pure functions, each returning the next one's result, takes about `2n` validations. Repeating whole rounds took `n` rounds of `n` (#260).

Decisions and limitations:

- **The receiver is not seeded.** It usually holds the application's root handle, and the directive's signature check also ignores receivers (`OrmWrapper.GetDB` is reported unused). Seeding it would report every `r.db.Where(...)` in a pure method.
- **Interfaces are not followed.** A `*gorm.DB` boxed in an `interface{}` is caught only when it is passed or stored directly (`pollutionsource.UnwrapGormDB`). Types that hold one only through an interface are not seeded.
- **Builtins.** `len(dbs)` and `cap(dbs)` cannot pollute and are not reported. `append` and `copy` count every derived argument. Other builtins count only a `*gorm.DB` argument itself.
- **Captured variables.** A closure that assigns a derived value to a variable captured from the pure function (`func() { x = db }()`) is reported as a store through a pointer. The free variable is not traced back to the enclosing `Alloc`.

### Returned arguments (#188)

A pure function does not pollute its argument, but it may return it: `func p(db *gorm.DB) *gorm.DB { return db }`. The result is then the same `*gorm.DB`, so `r := p(q); r.Find(nil); q.Count(nil)` branches `q` twice. Each trusted pure function therefore records the parameters its result may be in `PureVerdicts.Returned`.

| Step | What it does |
| --- | --- |
| Summary | `Validator.ReturnedParams` seeds one parameter at a time, propagates as above, and reports the parameter when a `Return` carries a derived value. Seeding one at a time keeps `func(a, b *gorm.DB) *gorm.DB { return a }` from aliasing `b`. |
| Transitivity | The result of a trusted pure call is derived when it may return a derived argument, so a pure wrapper of `p` returns its argument too. `validatePureFuncs` repeats until the summaries stop growing. |
| Caller | `RootTracer.pureReturnedArgs` traces the call result to the arguments it may be. The first one with a mutable root is the root; `traceAll` returns the roots of all of them. The summary is keyed by the declared function, so a generic instance and a method value (`same := r.Same; same(q)`, whose arguments lack the receiver) use it too (`PureVerdicts.ReturnedArgs`). |
| Immutable argument | When no returned argument has a mutable root, the result is immutable only when every root of the callee's returns is one of its own parameters (`pureReturnsOnlyParams`). `p(db.Session(s))` is then immutable. `if c { return db.Session(s).Where("x") }; return db` can also return a fresh chain, so its call stays its own root. |
| Inside a pure function | Branching such a result branches the argument, so `p(db).Find(nil)` inside a pure function is a definitive violation, and that function is no longer trusted at its call sites. |

Decisions and limitations:

- **The argument wins when the result may be either.** A result that may be the argument or a fresh chain is treated as the argument. A branch from it is then counted against the argument, which can report a reuse on the fresh path. Treating it as fresh would miss the reuse on the other path.
- **A Session-isolated result is not an alias.** `db.Session(&gorm.Session{}).Where(...)` gets its own Statement copy at the `Where` (`TestPureSessionResultIsolated` in `e2e/internal`), so the documented `withTenant` pattern keeps a fresh root, as the README says.
- **A tuple result follows the same rule (#176).** `Validator.ReturnedParams` already looks at every operand of each `Return`, so `return db, nil` records `db`. `pureReturnedArgs` accepts a tuple result, and `pureReturnsOnlyParams` checks every `*gorm.DB` operand, in any position. The `*gorm.DB` elements of one tuple share the call as their root (see [Results the tracer cannot see through](#results-the-tracer-cannot-see-through-176)), so they are all traced to the returned arguments.
- **Across packages.** `ReturnedParams` needs the callee's body, so the declaring package exports the summary and `pureReturnsOnlyParams` in its fact (#195, `pureReturnedArg` in `testdata/src/externalpure/use.go`).
- **The receiver.** The receiver is not seeded, so a pure method that returns `r.db` is not an alias of anything.
- **Cost (#297).** The arguments are traced in the same `visited` set as the rest of the search. Each one used to get a copy of it, so the other edge of a Phi walked the same values again. With `n` statements `if c { q = p(q) } else { side() }` that doubled the work at each one: 20 s and 2.5 GB at `n = 24`. In `trace`, sharing changes no answer: an argument whose search ends without a root leaves only values that have no root from any edge. In `traceAll`, `allRootsSearch.rooted` records the values that the search has finished, and whether each found a root. A pure call whose argument is finished with a root adds nothing, because those roots are in the result already, and it is not a root of its own. Before, such a call (`if c { q = p(q) }`, where the search reaches `q` first) was added as an extra root that nothing branches. An argument that is still being traced (a loop) or has no root makes the call its own root, as before. `TestAllocationGrowth` (`purephi`, `purephiimm`) checks that the allocations grow at most twofold from `n = 16` to `n = 20`. The shapes without the `else` (`purephisafe`) and with loops (`pureloops`) still grow like `filters`, about 3x per doubling.

## Immutable-return body contract (#194)

`purity.ImmutableReturnValidator` checks the body of every `//gormreuse:immutable-return` function. It reports the function when some `*gorm.DB` it returns is **provably** mutable. Anything it cannot prove gets the benefit of the doubt, because the directive exists to assert what the analyzer cannot see.

| Returned value | Provably mutable when |
| --- | --- |
| A gorm chain-method call (`Where`, `Order`, …) | Always |
| `Session(cfg)` | `cfg` is a `&gorm.Session{...}` literal that sets `Initialized: true` (`callsite.Site.SessionSetsInitialized`). It returns `tx.getInstance()`, a `clone == 0` handle (#174). An unreadable config is not reported |
| A Phi | Any edge is |
| A call to a closure: an IIFE, a `MakeClosure`, a captured local assigned exactly once (`f := func() ...; return f()`), or a package-level variable that always holds one (`var f = func() ...`, #315) | One of the closure's returns is, checked the same way. A closure marked `immutable-return` is trusted here and checked on its own. The closure is the call's `callsite.Site.Callee`, so the check resolves function variables as every other pass does. Before #315 it read the raw call value and missed a package-level variable |
| A load of a named result that a deferred closure stores to | Any value the closure stores is. When a deferred closure stores on every path through it and its `defer` dominates the return (or the return is in the recover block), the values stored before it are hidden and not checked |
| A load of a package-level variable | It is unexported, its address is used only to load and store it, it is assigned at least once in the package (the `init` function included), and **every** assigned value is |
| Anything else the tracer returns (a parameter, a named user function's result) | Never |

Decisions and limitations:

- **Globals: check the package, not the benefit of the doubt.** The issue allowed either. An unexported variable can only be assigned in its package, so when every store there is mutable, every read is. One immutable store gives the benefit of the doubt, since a read may run after it (`var db = x.Where(...)`, then `func init() { db = db.Session(...) }`). An exported variable may be assigned by another package, and a variable whose address escapes may be assigned through a pointer, so neither is reported.
- **Named functions are not followed.** A call into an unmarked named helper stays the benefit of the doubt, as the README says. Only closures, whose body belongs to the function, are followed.
- **Deferred writes are direct stores only.** A store made by a closure that the deferred closure calls, or by a non-deferred closure (`f := func() { r = r.Where("x") }; f(); return`), is not seen.
- **A captured closure variable** is followed only when it is assigned once and no closure assigns it.

## Directives across packages (#195)

Before #195 the analyzer exported no facts. For a callee in another package, `DirectiveFuncSet` parsed the callee's source file from disk and looked for the directive in the doc comment of a declaration with the same name. So a same-line directive and a generic instance (`GenF[int]`, whose name is not `GenF`) were missed, a contract that the body broke was still trusted, the `immutable-param` caller check never ran, `immutable-input(name)` was ignored, and a package whose source the driver does not load had no directives at all.

Now `exportFacts` in `internal/fact.go` exports a `facts.Func` object fact for every exported function and method with a directive, after every pass has run. A caller reads it through `facts.Table`, which `RunSSA` plugs into each consumer. A function of another package has no syntax in the SSA program, so a consumer asks the table only for a function without syntax.

| Field | Set when | Read by |
| --- | --- | --- |
| `Pure` | Marked `pure`, valid or not, as `DirectiveFuncSet.Contains` decides in the package | `DirectiveFuncSet.Import` on the pure set |
| `PureFailed` | The body definitively breaks the contract | `PureVerdicts.HasFailed`. So a pure function that passes its argument to it fails too (#187) |
| `Returned`, `ReturnsOnlyParams` | The parameters the result may be (#188), and whether nothing else is returned | `PureVerdicts.ReturnedArgs`, `RootTracer.pureReturnsOnlyParams` |
| `ImmutableReturn` | Marked `immutable-return` or inferred (#261), and not distrusted: the body keeps the contract without relying on a broken one (#314) | `DirectiveFuncSet.Import` on the immutable-return set |
| `BranchesImmutableParam` | In `needsImmutableParam` | The caller-side check (`handler.Context.NeedsImmutableParam`) |
| `ImmutableInput` | Marked `immutable-input(name)`, and not distrusted: `ValidateImmutableInputs` reports nothing, and the body does not rely on a broken contract (#314) | `ImmutableInputSet.Import`. The callback collectors and the body check then see the callee's callbacks |

Decisions:

- **A broken contract is trusted at home, not abroad.** In the declaring package a broken `immutable-return` or `immutable-input` is reported at the declaration and still trusted, so the mistake is reported once. A caller in another package may belong to someone who never sees that report, so the fact leaves the directive out and the callee is treated as unmarked. A broken `pure` is distrusted in both places, as before (#66).
- **Distrust is transitive (#314).** A function that keeps its contract only because it trusts a broken one exports no fact either, like pure's failures (#187). Before #314, `//gormreuse:immutable-return func DeclaredViaBad(db) { return BadConn(db) }` exported the fact when `BadConn` broke its contract, and so did an `immutable-input(cb)` function that handed `cb` to a broken one. See [Distrusted contracts](#distrusted-contracts-314). In the declaring package both are still trusted, so the broken contract is reported once, at `BadConn`.
- **Exported functions only.** Another package can name only an exported function or method, so only those get a fact. This also keeps fact expectations in the test fixtures few: `analysistest` fails on a fact without an expectation, and an object fact is expected with `// want Name:"..."` on the declaration line.
- **No fact means no directive.** The disk fallback is gone. Every driver that supports facts (`singlechecker`, `go vet`, golangci-lint, `analysistest`) runs the analyzer on the dependencies first, so a dependency without a fact has no directive function. A driver without facts would see none, which reports more, not less.
- **The #191 exemption is not exported.** It concerns the callee's own parameter, which is checked when the callee's package is analyzed. Exported functions stay out of it, because facts cannot tell the declaring package how other packages call it.
- **No fact for a generated file (#259).** The functions of a skipped file are not analyzed, so their directives are never validated. `exportFacts` skips them, so another package treats them as unmarked. Before #259 a generated `immutable-return` function that returned `db.Where(...)` was exported as trusted. Validating the skipped files only to decide their facts was rejected: a generated file is excluded from every pass, so its author would never see a broken contract reported, and an unmarked callee reports more, not less.
- **A directive in a generated file of the same package (#315).** The same rule holds at home: a directive that is not validated is not trusted. Generated files are not added to the directive sets, and `DirectiveFuncSet.hasDirective` finds no directive on a function whose file was not added, so every kind and placement in a generated file counts as unmarked in its own package too. Before #315 a doc-comment `pure` or `immutable-return` there was read from `FuncDecl.Doc` and trusted, while a same-line `pure` and every `immutable-input` were not. An unmarked callee reports more, not less.

Cost: with `FactTypes` set, the drivers also run the analyzer, and `buildssa`, on every dependency, including the standard library. Only facts come out of those runs. Two shortcuts keep them cheap, and neither changes a report:

| Shortcut | Why it is safe |
| --- | --- |
| `run` skips the directive scans of a file with no comment that contains `gormreuse:` | Every directive, and every comment `FindDirectiveProblems` reports, contains it |
| `run` builds SSA, by calling `buildssa.Analyzer.Run` itself, only for a package that reaches `gorm.io/gorm` through its imports (`typeutil.ReachesGorm`). `Analyzer` requires `ctrlflow`, which buildssa needs and which already runs on every dependency for its own facts, instead of `buildssa`. `RunSSA` gets a nil `ssaInfo` for any other package and still reports unused directives and ignores | No value in such a package can hold a `*gorm.DB`, since its type would name `gorm.DB` and so record gorm among the imports |

On `e2e/internal`, the command used about 2.8 s of CPU with both, 4.6 s with only the second, and 7.5 s with neither. v0.22.0, without facts, used about 1.6 to 2.4 s. Measured again before 0.23.0, with every later change, the command used about 1.8 s of CPU there, and v0.22.0 about 1.6 s.

## Inferred immutable-return (#261)

After #227 a helper's result is a mutable root, so on gorm's own `tests` module most new reports came from `db, err := OpenTestConnection(...)`, and in services from helpers such as `r.conn(ctx)` returning `r.db.WithContext(ctx)`. Each needed a manual `//gormreuse:immutable-return`. Now `inferImmutableReturns` in `internal/immutable.go` adds such functions to the immutable-return set with `DirectiveFuncSet.Infer`, right after the facts are imported and before any pass reads the set. Every consumer (the tracer, the pure validator, the callback and fix code, `exportFacts`) then sees them through `Contains`.

`purity.InferImmutableReturns` proves a function when every `*gorm.DB` operand of every `Return` is, through Phi edges:

| Value | Proved when |
| --- | --- |
| A `Call`, or an `Extract` of a tuple `Call` such as `gorm.Open`'s | `RootTracer.IsImmutableCall` accepts it with a tracer that has no immutable-return set: its last call is `Session` (with a config literal whose `Initialized` is known false), `WithContext`, `Debug`, `Begin` or `gorm.Open`. Or the declared callee is trusted (declared, or imported from a fact) or already inferred |
| The nil constant | Always |
| A load of a local variable assigned once, such as one a closure captures (#308) | Its stored value is proved (`FuncVars.LocalValue`) |
| A read of a field through a parameter or a receiver, at any depth of fields and pointer fields, also when a closure captures the parameter, or a read of a package-level variable (#308) | Every store in the function that reaches the read is proved, through the roots `FindAllMutableRoots` finds, and no store in the package assigns the variable a provably mutable value (`ImmutableReturnValidator.AssignedMutable`). With no store, the value is immutable, as reuse detection reads it |
| Anything else: a parameter, a field of a local struct, a variable assigned more than once (a named result a deferred closure writes is one), a dynamic or interface call | Never |

A function needs at least one such result, so a function that only panics is not inferred. Candidates are the analyzed functions and closures with a body and a `*gorm.DB` result, not in a skipped file, and without the directive.

Decisions:

- **Positive proof, not the benefit of the doubt.** `ImmutableReturnValidator` reports only what is provably mutable, because a declared directive is the user's assertion. Inference has no assertion behind it, so it accepts only what is provably immutable. Between the two, a function stays unmarked.
- **A least fixed point.** A worklist proves each candidate. A candidate that fails only on calls to other candidates waits on them, and is proved again once the last of them is added, so each candidate is proved at most twice. Proving it again whenever any one of them was added, and queuing it once per failed proof, grew exponentially with a caller that switches over a chain `c0 → c1 → … → ck` (17.7 s at k = 32, #298). `TestAllocationGrowth` (`inferexp`) holds the allocations near linear, and `TestInferImmutableReturnsGrowth` does the same for inference alone on a dense chain, where each `ci` switches over every later `cj`. The set only grows, and a larger set only proves more, so the result is the least fixed point, whatever the order. A function is never added on the strength of itself, which is why recursion is not inferred: `if n > 0 { return rec(n-1) }; return db.Session(...)` is immutable, but proving it needs a greatest fixed point, and an assumed-true cycle is harder to trust. `TestInferImmutableReturns` checks every rotation of the candidates, forwards and backwards.
- **A broken declared contract: trusted at home, not abroad.** A declared `immutable-return` whose body breaks the contract is trusted in its package (see [Directives across packages](#directives-across-packages-195)). A function that returns its result is inferred there too, so the mistake is still reported once. `distrustContracts` proves the inferred functions again with the broken ones distrusted, and the ones that no longer hold export no fact, like the broken function itself (see [Distrusted contracts](#distrusted-contracts-314)). The broken contracts are known only after the pure pass, which inference must precede, so this is a second fixed point and not a single one.
- **A declared directive stays used.** An inferable function with the directive is not a candidate, keeps its body check, and its directive is not reported unused or redundant. It documents the intent, and a later change to the body is reported at the declaration instead of turning into reports at the call sites.
- **`Validate` skips inferred functions.** Their bodies were proved, and a report would say "declared" about a function without the directive.
- **No `-fix` to insert the directive.** The issue suggested it as an alternative to implicit inference. Inference is the same check the analyzer would make when validating an inserted directive, so inserting it adds a comment and no information.
- **What detection reads as immutable, inference accepts (#308).** Reuse detection treats a field read through a parameter or a receiver, and a global that the function only reads, as immutable ([Struct roots](#struct-roots-181), [Package-level variables](#package-level-variables-183)). Before #308, inference refused both, so `func (r *Repo) DB() *gorm.DB { return r.db }` made `d := r.DB(); d.Find(nil); d.Count(nil)` a report while `r.db.Find(nil); r.db.Count(nil)` was not one. A wrapper should not change the answer, so inference now accepts the reads that detection accepts. The rule is still a positive proof for everything else: a parameter, a chain and an unknown call stay unproved.
- **A getter of a variable the package assigns a chain to is not inferred.** A caller may run `r.db = r.db.Where("x")` before `r.DB()`. Detection sees such a store when it is in the reading function, but a getter is trusted at every call site. So inference refuses a read of a field or global that any function of the package, its initializer included, assigns a provably mutable value (`AssignedMutable`). That is the body check's rule for a global, made strict: the body check needs every assignment to be mutable to report, inference refuses on one. A global whose address escapes is refused too. A store in another package, or through a field address passed to a function, is not seen, as for reads in detection.
- **A captured variable assigned once.** `conn := db.Session(&gorm.Session{}); get := func() *gorm.DB { return conn }` lives in an `Alloc` that the closure reads through a `FreeVar`. `LocalValue` resolves it to the stored value, with the conditions of [Local function variables](#local-function-variables-304). A variable assigned twice, or in a closure, stays unproved even when the tracer finds no root, since the tracer's single-root trace reads only the first store.

Measured with the command on gorm's repository (v1.31.2):

| Module | Before | After | Removed reports |
| --- | --- | --- | --- |
| Root | 160 | 150 | 10 in `Migrator.AutoMigrate`, all on the results of `GetQueryAndExecTx`, which returns two `Session` results |
| `tests` | 34 | 15 | 19, all on the result of `OpenTestConnection`, which returns `gorm.Open`'s result or nil |

Each removed report branched a `clone > 0` handle, so none was a real reuse. No report was added.

#308 measured the same repository, and two real services (new-api and gin-vue-admin), before and after:

| Module | Before | After | Removed reports |
| --- | --- | --- | --- |
| gorm root | 66 | 66 | none |
| gorm `tests` | 15 | 14 | the Scopes warning at `scopes_test.go:60`, whose chain ends in `Find` ([Scopes Session-family warning](#scopes-session-family-warning-temporary-196)) |
| new-api | 34 | 31 | 3 test setup helpers that return `gorm.Open`'s result through a variable that a `t.Cleanup` closure captures |
| gin-vue-admin | 32 | 32 | none |

Each removed report branched a `clone > 0` handle. No report was added. The counts are diagnostic lines of `gormreuse ./...`, so they differ from the table above.

## Distrusted contracts (#314)

`distrustContracts` in `internal/distrust.go` decides which `immutable-return` and `immutable-input` functions, declared or inferred, another package must not trust. `exportFacts` leaves their directives out of the facts. The package itself keeps trusting them, as [Directives across packages](#directives-across-packages-195) says.

A function is distrusted when its body breaks the contract, or when it is checked again with the distrusted functions distrusted and the check fails:

| Function | Checked again with | Fails when |
| --- | --- | --- |
| Declared `immutable-return` | `ImmutableReturnValidator.Distrusting` | A returned root is a call to a distrusted function. The tracer from `RootTracer.Distrusting` does not trust one, so such a call is a root, and the validator counts it as provably mutable |
| Inferred `immutable-return` | `purity.InferImmutableReturns` on the function alone | A returned call's callee is distrusted, so the proof no longer holds |
| `immutable-input(name)` | `ValidateImmutableInputs` with `distrusted` and the distrusting tracer | It hands the callback the result of a distrusted function, hands the callback to a distrusted immutable-input function (an escape), or hands it the parameter of a callback that a distrusted immutable-input function calls |

A callback's parameter stays exempt only while every function that calls it as a callback keeps its promise (`callback.CollectImmutableInputProviders`). One that is also called by gorm's `Transaction` still gets a mutable value from a distrusted function.

It is a worklist, like `validatePureFuncs`. The functions whose body breaks a contract are the seeds. When a function becomes distrusted, the functions that call it or refer to it, in their body or in a closure inside it, and the functions that create or are a callback it calls, are checked again (`distrustDependents`). The sets only grow, and a larger set only fails more checks, so this ends at the same sets in any order. Each function is checked again at most once per function it depends on that becomes distrusted. A package where no contract breaks skips it. `TestAllocationGrowth` (`distrustreturn`, `distrustinput`) holds a chain of `n` directive functions whose last breaks its contract to linear growth. Each function is in a file of its own there, since a directive lookup scans the comments of its file (#301).

Decisions and limitations:

- **The declaring package keeps today's rule.** Distrusting `DeclaredViaBad` at home too would report each reuse of its result, on top of the report at `BadConn`. The issue and the README promise only that another package does not trust it.
- **Only a reliance that the check can see.** A declared `immutable-return` function that returns the result of an unmarked helper that is not inferred still gets the benefit of the doubt, since the validator does not follow named helpers. An inferred helper is a target itself, so a helper that returns a broken function's result is distrusted, and so is the function that returns the helper's result.
- **A function in a generated file is not a seed.** Its directive is not trusted at all (#315), so a function that relies on it is checked against an unmarked callee.
- **`pure` is unchanged.** A pure function's fact already carries `PureFailed`, which is transitive (#187). PIR (`pure,immutable-return`) that returns a broken function's result exports `Pure` without `ImmutableReturn`.

## Reassignment Fix Target

The reassignment fix (`q.Where("a")` → `q = q.Where("a")`) rewrites one variable: the one that holds the reused root (#198). `findReassignTarget` in `internal/fix/reassign.go` finds it from the root's own definition (`q := <call>`, `q = <call>` or `var q = <call>`; a root that is not a call, such as a Phi, gets no reassignment). A statement is rewritten only when its own top-level call is the use and its receiver is an identifier that `types.Info.Uses` resolves to that variable.

No statement is rewritten when one of the cases below holds. The root gets a Session instead, unless a discarded statement would lose its effect; then the diagnostic has no fix (see [Discarded Statements Under a Session Fix](#discarded-statements-under-a-session-fix-199)):

| Case | Example | Why |
| --- | --- | --- |
| The root is not assigned to one local variable | `r.db.Where(...)`, a package-level `DB` | There is no local to rebind |
| A use does not mention the variable | `r := q; r.Where("a")`, a method value `where := q.Where` | Rebinding `q` leaves that branch on the old root |
| A closure captures the variable, unless it is called inline | `go func() { q.Where("a") }()`, `defer func() { q.Find(nil) }()`, `fn := func() { ... }` | A goroutine that writes `q` races with its parent. A deferred or stored closure would see the new value |

An inline closure `func() { ... }()` runs in program order, so writing a captured variable from it is safe. A closure passed to a call, such as `Transaction`, `t.Run` or `wg.Go`, may run later or on another goroutine, so it counts as a capture. A variable declared inside such a closure is local to it and is rewritten as usual.

The old resolver took the receiver of the outermost method call around the use. Inside `DB.Transaction(func(tx) { ... })` that rewrote the package-level `DB`, and in `base.Or(q)` it rebound `base`. `TestGoldenFilesCompile` now type-checks every `.golden`, and `testdata/src/gormreuse/fix_target.go` and `testdata/src/converge/fix_target.go` hold the cases.

## Parameter-Root Fix (#201)

A parameter root has no call to append a Session to. `Generate` used to declare the function `//gormreuse:immutable-param` for every parameter root. That moved the duty to the callers, but:

- callers in other packages are never checked (#195), so an exported function's reuse disappeared;
- a function used as a value, such as `var scopes = []func(*gorm.DB) *gorm.DB{scope}`, has callers no check sees, so a real Scopes-callback reuse disappeared;
- a caller in the package that passes a mid-chain value got the caller-side diagnostic, which had no fix, so `-fix` never reached zero. Since #248 that diagnostic has a fix of its own, which isolates the argument (see [Contract and Directive Fixes](#contract-and-directive-fixes-248)). The parameter-root fix still uses the body Session for such a function, which fixes every caller at once.

`Generate` in `internal/fix/generator.go` now chooses (the checks are in `internal/fix/param.go`):

| Check | Fix |
| --- | --- |
| `canFixParam` fails: a Scopes/Preload callback, a closure with a `*gorm.DB` result, or a function declaration with a `*gorm.DB` result that `calledOnlyDirectly` rejects | None |
| `paramCallersIsolate`: the function is a declaration, the pass sees every file of the package (`reportSeesWholePackage`), `calledOnlyDirectly`, and `handler.BreaksImmutableParam` holds for no argument of any call to the function in the analysed functions | `//gormreuse:immutable-param` |
| Otherwise | `tx = tx.Session(&gorm.Session{})` before the body's first statement (`paramSessionSite`, written by `sessionEdits` with the same gorm name resolution and import as every other Session), then the rest of the local-root fix |

`paramCallersIsolate` checks every argument of each call with the caller-side check's own predicate, not only the argument of the reused parameter: declaring the function covers all its `*gorm.DB` parameters. It scans the analysed functions (`runFuncs`), so a caller in a package-level closure counts (#183, #245).

A package with test files is analyzed as two variants, one without its `_test.go` files and the test variant, and `-fix` applies the fixes of both. The variant without the test files cannot see their callers. If each variant decided from the callers it sees, one could add `immutable-param` and the other the body Session, and the result reports the directive redundant. So a function outside the test files of a package with test files always gets the body Session, in both variants (`reportSeesWholePackage`, the rule `reportIgnoreRemovable` uses for #248). A function in a test file is analyzed only by the test variant, which sees every caller, so it may get the directive. The cost is a body Session where the directive would also have worked. `TestParamFixWithTestFiles` and `testdata/src/paramtests` check it.

Neither fix makes a value that the parameter merges with immutable. In standard pagination, `if f.Name != "" { db = db.Where("name = ?", f.Name) }` is followed by `db.Count(&total)` and `db.Offset(10).Limit(10).Find(&users)`. The Phi of `db` and the `Where` is read twice, so Count's Select and Model leak into Find, which then fails to scan. The `Where` gets a Session, as an edge of a local root does (`mergedSessionPositions`, see `sessionPositionsForRoot`). With the body Session it is one more Session site. With the directive, the rest of the local-root fix runs for those Sessions, and the directive alone is the fallback when that fix offers none. Both used to need a second pass (#311): `mergedEdges` read only a root that is an instruction, and the directive was returned before the edges were asked.

`calledOnlyDirectly` needs an unexported function, not a method (a method can be called through an interface), whose every reference is the function of a call (`f(x)`, `f[T](x)`, `(f)(x)`). In the Session case the parameter is the reassignment target (`varToReassign` returns its variable), so a discarded `tx.Where("a")` becomes `tx = tx.Where("a")` and keeps its effect inside the function. The Session goes at the top of the body, so it also covers uses through an alias.

A discarded statement on a parameter also changes the caller's value, and a helper such as `func applyTenant(tx *gorm.DB) { tx.Where("tenant_id = ?", t) }` exists for exactly that. Reassigning it keeps the change inside the function only, and makes the helper lint-clean, so nothing reports the change a caller's Session then drops (#243). `Generate` therefore offers no Session-case fix for a parameter with a discarded statement, unless the function has a `*gorm.DB` result that can carry the change back to the caller, and every caller reads it (#312). `everyCallUsesResult` checks each call in the analysed functions: its result, or a `*gorm.DB` element of its tuple, must have a referrer, and a go or defer call has none. `canFixParam` already requires `calledOnlyDirectly` for such a function, and the pass must see every file of the package, as for `paramCallersIsolate`, so no call is missed and both variants agree. A caller that reads the result but goes on using its argument, as in `r := applyTenant(q); q.Find(&a); r.Find(&b)`, is not checked: only the result carries the change. `TestFixHelperResultPreservesQueries` in `e2e/internal/fix_helper_result_test.go` runs the cases of `fixsql/before/helper_result.go`, a caller that discards the result and one that reads it, against real GORM, and `TestFixHelperResultDetectsReassignment` checks that the fix suggested before drops the discarding caller's tenant filter.

A function that returns a `*gorm.DB` and may be called by unknown code may be a Scopes callback. A Session result returned from one leaks gorm's transaction (go-gorm/gorm#7592), and the fix does not tell which results are Session results. It gets no fix.

A closure parameter gets the body Session under the same rules (#313). In gorm's own `tests/` module, 8 of the 9 diagnostics without a fix were closure parameters: callbacks of Transaction wrappers and of `Connection`. Four of them now get the body Session, and the fixed tests compile, re-lint clean of them and log the same SQL. The other four belong to one root, a callback parameter handed to another wrapper call and used after it, which [#243](#in-place-changes-outside-the-variable-243) still withholds. A closure has no name, so `calledOnlyDirectly` cannot hold, and one with a `*gorm.DB` result may be a Scopes callback: it gets no fix, as an exported function with one does. A closure never gets `immutable-param`. Placing a directive on a literal is left to the user, and its callers, such as the function a callback is passed to, are not calls the caller-side check can see. The body Session needs only the closure's own body, so it fits a callback, an IIFE, a `go func(tx *gorm.DB) { ... }(q)`, a local closure and a package-level one alike. In a Transaction callback, the Session keeps the Statement's connection, which is the transaction (`TestClosureParamSessionKeepsTransaction`).

Limitations:

- The body Session also stops the function from changing the caller's Statement in place, as `tx.Where("a").Find(nil)` does on a mid-chain value. That change is the reuse being fixed.

`TestParamSessionIsolatesBranches` in `e2e/internal/param_session_test.go` checks the body Session at runtime, and `testdata/src/converge/fix_param_root.go` checks that one pass converges. `testdata/src/gormreuse/fix_later_loop_merge.go` holds the merged parameters and the reassignments read in a later loop, and its golden is re-linted by `TestFixesConverge`. `TestLaterLoopMergeStopsLeaks` in `e2e/internal/later_loop_merge_test.go` runs them against real GORM and checks that the fixes of the first pass before #311 still leak. `testdata/src/gormreuse/fix_closure_param.go` holds the closure parameters, re-linted by `TestFixesConverge`, and `fix_param_root.go` the closures without a fix. `e2e/internal/closure_param_test.go` runs the `ClosureParam` cases of `fixsql/before/closure_param.go` against real GORM.

### Phi edges

`edgeSessionPositions` skips an edge that is already immutable. It used to call an edge immutable when a Session-family call appeared anywhere in its chain, so `db.Session(&gorm.Session{}).Where("y")` and `db.WithContext(ctx).Where("y")` got no Session, while detection still reported them: the diagnostic stayed after every pass. It now asks `RootTracer.IsImmutableCall`, the tracer's own rule: the edge's last call must be an immutable-returning builtin or an `immutable-return` function, and a Session that may set `Initialized` does not count. `TestSessionMidChainEdgeAccumulates` checks at runtime that such an edge is mutable.

## Discarded Statements Under a Session Fix (#199)

A discarded non-finisher statement such as `q.Where("a")` or `q.Where("a").Limit(10)` has an effect only because `q` is mutable: it changes `q`'s Statement in place, and every later query on `q` sees it. A Session at the root makes `q` immutable, so the same statement then changes a throwaway copy. The fixed code drops `"a"` silently, and the linter is clean. The fix must therefore never leave such a statement in place. `findReassignedChains` in `internal/fix/reassign.go` applies this rule:

| Statement | Fix |
| --- | --- |
| A chain of non-finisher calls on the target variable, such as `q.Where("a").Limit(10)` | Reassign it as a whole: `q = q.Where("a").Limit(10)`. A Session it needs goes after its last call. One after `Where` would be undone by `Limit`, which returns a new mutable value |
| A chain that starts with a Session-family call, such as `q.Session(s).Where("a")` | None needed. It never changes `q` |
| A chain with a Session-family call before its last call, such as `q.Where("a").Session(s).Limit(1)` | **No fix.** Only `q.Where("a")` changes `q`. Reassigning would add `Limit(1)` to `q` |
| A receiver that is not the target variable: an alias `r := q`, a method value `where := q.Where`, a variable captured by a closure that is not called inline, or a use that #68 orders at a closure's call site | **No fix.** The diagnostic stays without a suggested fix |

Two more changes make the reassignment see every statement:

- **Loop violations read their uses late.** A loop violation is added while its use is tracked (`handler/call.go`), before the uses after it in the loop body are recorded. `Violation.AllUses` used to be a snapshot taken then, so the fix never saw a discarded statement after the loop's query. `CollectViolations` now fills `AllUses` after tracking has finished.
- **A reassignment in a loop gets a Session.** The simulation orders uses by position, so it cannot see the next iteration's use of a value reassigned in the loop body. `findRootsNeedingSession` gives such a virtual root a Session whenever its block is in a CFG cycle.

`UsageInfo` carries the method name and the call's own position for this. `TestFixPreservesQueries` in `e2e/internal/fixsql_test.go` runs `e2e/internal/fixsql/before` and `fixsql/after` against real GORM and compares the SQL of each query. `TestGoldenFilesE2E` in the main module regenerates each file of `after` from its file in `before` (`after.go` from `before.go`) with the current fixes, so the comparison always checks the fixes gormreuse suggests now. `TestFixPreservesQueriesDetectsSessionOnly` checks that the comparison fails for the old Session-only fixes.

Limitations:

- A discarded statement whose effect no later query sees still blocks the fix, such as a final `q.Where("after")` when a stored closure captures `q` (`whereOnlyStoredClosure` in `evil.go`). Proving that no later use exists would need alias and loop analysis.
- Other ways of keeping a chain's effect away from its root, such as `_ = q.Where("a")`, are not treated as discarded statements.

## In-Place Changes Outside the Variable (#243)

The #199 rule covers a discarded statement written on the root's variable. Other code can change the root's Statement in place too, and a Session at the root makes it change a copy. The tenant filter of `applyTenant(q, 7); q.Find(&users)` then disappears, and the re-lint is clean. `changedElsewhere` in `internal/fix/change.go` withholds the whole fix when:

| Case | Example | How it is found |
| --- | --- | --- |
| The root is handed to other code, and a later use may see the change | `applyTenant(q)`, `apply(q)` for a closure, `_ = withTenant(q)`, `qs := []*gorm.DB{q}`, `m["q"] = q`, `ch <- q` | `UsageInfo.Escapes`, set by `Tracker.MarkPolluted` (a call argument, slice or map store, send) and by `q = helper(q)`. `changeSeenAfterEscape` decides whether a later use exists |
| The `Error`, `RowsAffected` or `Statement` field is read from the root, or from a chain on it that runs no query | `return q.Error`, `if err := q.Where("a").Error; ...`, `q.Where("a").Limit(1).Statement` | A `FieldAddr` in the root's function or its closures, whose operand `carriesRootChanges` walks down a chain of non-finisher, non-Session-family `*gorm.DB` methods to the root |
| A go statement hands the root to a call | `go applyTenant(q)`, `go q.Where("a")` | An `*ssa.Go` whose argument traces to the root (`statementRootChange`). The goroutine may change it while the uses after the statement run. The receiver of a finisher or a Session-family call, as in `go q.Count(nil)`, is not handed over: the call is a use like any other, and runs on a copy once the root has a Session, as in `go func() { q.Count(nil) }()` (#312). It is found by `Site.ArgParam`, since a static method call's arguments start with the receiver. A chain method's receiver is: the goroutine changes the root in place, and a go statement cannot be reassigned |
| A defer statement changes the root in place, and another deferred call may see it (#309) | `defer q.Find(nil); defer q.Where("a")`, `defer applyTenant(q); defer q.Find(nil)`, `for … { defer applyTenant(q) }` | An `*ssa.Defer` that `statementRootChange` says changes the root, when another `*ssa.Defer` uses the root, or the statement repeats (`useRepeatsAfter`). Deferred calls run after every other use, so only a deferred call sees the change. Their run order is not followed, so any other one is taken to |
| The root is stored in a struct field that other code reaches (#310) | `s := &Svc{db: q}; s.apply()`, `applyP(params{db: q})`, `p.db = q; helper(&p)` | `holderChangesRoot`, over the stores that `holderStores` finds (below) |

`changeSeenAfterEscape` keeps the fix of an escape that nothing can observe: the last use of the root, in no loop, when every use is in order (`usesOrdered`). `q.Find(&a); render(q)` keeps its Session. An argument of a call that returns a `*gorm.DB`, other than a `*gorm.DB` method, always counts: `q = withTenant(q)` may return `q` itself, so the uses of its result see the change. A `*gorm.DB` method such as `base.Model(q)` returns a chain on its receiver. Read-only uses do not escape: a subquery (`RecordPureUse`), a `//gormreuse:pure` function, and the `fmt`/`log`/`testing` variadics.

The field read on a finisher's result, as in `q.Where("a").Find(&x).Error`, is that query's own outcome, and keeps the fix. A read of the root's own field counts wherever it is, even before the first use: telling the two apart needs ordering against the closures that may read it.

A reassignment-only fix (`q = q.Where("a")` with no Session) would not change behaviour, since a mutable root's method returns the root itself. It is withheld too: the check runs before the fix is built, and a reassignment whose chain ends in a Session does change the root.

**Struct stores (#310).** A store of the root into a struct field is not a use: detection reads the fields of a local struct as the value stored in them (#181), so `s.db.Find(&x)` is a use of `q`. The handler marks nothing at the store either, so `UsageInfo.Escapes` never covered it, and `s := &Svc{db: q}; s.apply(); q.Find(&a); q.Find(&b)` got a Session at `q` that made `apply` add its filter to a copy. `changedElsewhere` now also checks each store of the root, or of a chain on it that runs no query (`changeCarryingRoots`), into anything other than a variable or a slice, array or map element. Each point where other code can reach the stored root is an escape for `changeSeenAfterEscape`:

| Memory | Where other code reaches it | Found by |
| --- | --- | --- |
| A local struct or array (an `Alloc` reached through field and array element addresses) | Where it is handed on: a call that takes it, also as a receiver, a copy of its value that goes anywhere but another local struct, or such an instruction in a closure that captures the struct or a variable holding its address. A read of a `*gorm.DB` field, a copy into another local struct and a Phi are followed, not escapes | `holderWalk`, once per struct |
| Other memory: a package-level variable, or a struct that a parameter or another pointer points to | At any call that may run after the store. The first such call in the store's block is the escape; a call in a later block is taken as running at the end of the store's block. A local struct copied into such memory is reached the same way after the copy | `holderNextCall` over `holderFuncCalls`, once per function |

A `*gorm.DB` method, a builtin and a deferred call do not count as such a call, nor does a static call into a package that does not reach gorm with no function argument (`fmt.Println(n)`). Code in a closure of the root's function is not ordered against the uses, so an escape there, or a store there into memory that is not a local struct, is taken as seen. The stores, the walks and the calls are memoized in `Generator.holders`, so a function with n such stores stays linear (`structescape` in the growth gate).

The rule withholds no fix that the existing golden fixtures had: their struct stores go to a struct the function only reads, to a package-level variable or receiver field with no call between the store and the uses (the builder idiom `r.db = r.db.Where(c)`), or to a captured struct whose closure only reads the field.

`TestFixEscapePreservesQueries` in `e2e/internal/fix_escape_test.go` runs each case of the issue from `fixsql/before` and `fixsql/after` against real GORM. `TestFixEscapeDetectsSessionAtRoot` checks that the comparison fails for the fixes suggested before. `e2e/internal/fix_struct_escape_test.go` does the same for the struct cases of #310, including a parameter root whose body Session would drop the change. In `ErrorPropagation` a query runs only when `q.Error` holds the error of a failed query, so a lost error shows up as a missing query. `testdata/src/gormreuse/fix_escape.go` and `fix_escape_struct.go` hold the fixtures with and without a fix.

Limitations:

- An escape before a later use withholds the fix even when the callee only reads the value, such as `voidFunc(q)` with an empty body, or a `*gorm.DB` method argument (`base.Model(q)`). Mark a read-only helper `//gormreuse:pure`, or pass it an isolated value.
- A parameter's body Session still drops the in-place changes of the function's queries, such as the errors in the caller's `q.Error` after `helper(q)`. See [Parameter-Root Fix](#parameter-root-fix-201).
- A deferred call that takes the root, as in `defer applyTenant(q)`, keeps the fix when no other deferred call uses the root and it runs once. It runs after every use the fix orders, so only another deferred call could see its change. With another one, the fix is withheld even when that one runs first and cannot see it.
- A struct store is withheld more often than needed: a struct handed to a callee that only reads it (`log(params{db: q})`), and any call between a store into a package-level variable and a later use, count as escapes. A call in another block after such a store counts even when it runs after the last use.
- A struct reached through a pointer field of a local struct (`s.inner.db = q` with `inner *T`) is other memory, so every later call counts.

## Where Sessions Go (#202)

After the reassignments, `simulateVirtualRoots` decides which root each use reads, and `findRootsNeedingSession` gives a Session to each root, original or created by a reassignment, that is still used twice. Three defects made `-fix` add Sessions it did not need, miss ones it did, or need a second pass:

| Defect | Example | Change |
| --- | --- | --- |
| Every receiver use was recorded twice: by the receiver handler and by the argument handler, which did not skip `Args[0]`. A reassigning call then counted as a use of the value it creates | `q.Where("a"); q.Where("b").Find(nil)` became `q = q.Where("a").Session(...)` | `handleArgs` skips the receiver of a gorm method (`Site.ArgParam`). Detection is unchanged: the copies had the same block and position |
| The simulation took uses in position order, so a use after an `if` without an `else` read only the reassignment in the `if` | `if c { q.Where("a") }; q.Where("b").Find(nil); q.Where("c").Find(nil)` left the root mutable on the path where `c` is false | A use reads each earlier reassignment that can reach it, back to the first whose block dominates it, and the original root when none does (`virtualRootReaches`) |
| A root that flows into a Phi got Sessions only on the Phi's edges, and none when the Phi is used once | `q := db.Where("x"); q.Find(nil); q.Count(nil); if c { q = db.Where("z") }; q.Find(nil)` had no fix. With `q` captured by a closure, the synthetic Phi from its stores had the same result | `sessionPositionsForRoot` always puts a Session on the root itself, and adds the other edges when the Phi is used twice |

`findRootsNeedingSession` counts two uses of one root as a reuse when the earlier one can reach the later one (`usesConflict`). A reassignment does not conflict with the uses after it: on any path through it, they read the new value. A root used again in a loop that does not hold its definition, other than by a reassignment, gets a Session (`usedAgainInLoop`). That holds for a root created by a reassignment as for the original one: `q.Where("t"); for ... { q.Where("id = ?", id).Find(nil) }` reassigns `q = q.Where("t")`, and every iteration reads it. It used to be asked of the original root only, so the first pass left each query adding its id to the ones before (#311). So does a reassignment in a loop that does not define the root again: the next iteration reads it. Both ask `cfg.Repeats`, as the checker does, and so does `changeSeenAfterEscape`. The simulation takes the uses of one block in the order the block runs them (`runOrderedUses`), not by position: go/ssa puts a `for` statement's post statement at the end of the body's block, so `for i := 0; i < 2; q.Where("a") { q.Find(nil); i++ }` reads the root in `q.Find` before the post statement reassigns it (#247). A reassignment whose chain already ends in an immutable call, by the tracer's `IsImmutableCall`, gets no second Session (`virtualRootEndsImmutable`).

These rules need every use of the root, in the function that defines it. `usesOrdered` checks that:

- the tracker records no use outside `AllUses` (`Violation.HiddenUses`): no deferred or spawned call, no read through a Session/Debug copy or a subquery, no loop-carried read;
- the reported position is one of `AllUses`. A use through a Phi is filed under one edge only, so a diagnostic for another edge can have no use of its own;
- every use is in the defining function. A use in a closure, such as the body of a range-over-func loop, has no CFG path from the definition.

When the check fails, a root gets a Session when it has a use other than the one reassignment that reads it, which is what the fix did before. Removing a Session from a reassignment never drops an effect: the statement is still reassigned, so #199 holds. `testdata/src/gormreuse/fix_paths.go.golden` pins the output, `TestFixesConverge` re-lints that golden, and `TestFixPathsPreserveQueries` in `e2e/internal/fix_paths_test.go` compares the SQL before and after the fixes.

Three more cases needed a second pass, or hid a reuse (#247):

| Case | Example | Change |
| --- | --- | --- |
| A variable that a closure captures has no Phi: SSA keeps it in memory, and the tracer resolves every read to its first store. The other stores were meant to get a Session from a synthetic Phi, but its result had no referrers, so they never did. The re-lint was then clean, because it reads only the first store, while the other path still reused its root | `var q *gorm.DB; if c { q = db.Where("a") } else { q = db.Where("b") }; go func() { q.Find(nil); q.Count(nil) }()` got a Session on `"a"` only | `mergedEdges` gives a Session to each other store that may run before two of the root's uses. A use in a closure counts as after every store. `TestFixCapturedMergeNeedsEveryStore` in `e2e/internal/fix_one_pass_test.go` checks the leak at run time |
| A reassignment inside a closure writes the captured variable, and detection does not follow a store in a closure (`closureModifiesCaptured`). The uses after it still read the root | `func() { q.Where("a") }(); q.Find(nil); q.Count(nil)`, and the body of a range-over-func loop | The root gets a Session in the same pass (`reassignsInClosure`) |
| Only the first diagnostic at a position was reported. A read-only argument that sees a change at the position of a reuse appeared only after the reuse's fix | `q = q.Where(r); q.Count(nil); r.Find(nil); q.Find(nil)` | A position has one reuse diagnostic and one of each other message (`reportKey` in `internal/report.go`) |

`testdata/src/converge/fix_multipass.go` and `testdata/src/gormreuse/fix_one_pass.go` hold the cases.

Limitations:

- The rules follow the CFG of one function, not the run-time order of deferred calls or closures. Those roots get the conservative Sessions above, some of them redundant.
- A reassignment in a `for` statement's post statement is ordered before the body, so its root needs a second pass (`onePassForPost`, see [Known Limitations](#known-limitations)).
- A Phi edge with no call of its own still gets no Session (see [Fixes That Compile](#fixes-that-compile-200)).

## Fixes That Compile (#200)

Every suggested fix must compile, applied with all the others (`-fix`) or alone (an editor's quick fix). `internal/fix` checks types before it writes an edit:

| Edit | Rule | What went wrong before |
| --- | --- | --- |
| Reassignment `q = <statement>` | Each call in the chain must return `*gorm.DB` (`discardedChainToReassign`), and none may be a finisher (`isGormFinisher`) | `q = q.AddError(err)` assigned an `error`. `FindInBatches`, `CreateInBatches`, `UpdateColumn(s)`, `Commit`, `Rollback`, `SavePoint` and `RollbackTo` return `*gorm.DB` but run SQL or end a transaction, and were reassigned |
| `.Session(...)` after a root or a Phi edge | Only after the call whose `Lparen` is the value's position, and only when it returns `*gorm.DB` (`sessionEndAfterCall`) | The innermost call that contained the position got it. For a closure parameter edge that was the call the closure is passed to: `run(db, func(tx *gorm.DB) { ... }).Session(...)` |
| The `gorm` qualifier | `gormNameAt` resolves the name with `types.Scope.LookupParent` at each insertion point. It uses an import whose name resolves to `gorm.io/gorm` at all of them (a dot import if `Session` does). Otherwise it adds an import under the first free name of `gorm`, `gormio`, `gormio2`, ... | A blank import counted as an import (`undefined: gorm`). A parameter named `gorm` gave `gorm.Session is not a type`. A file importing another package as `gorm` got a second `gorm` (`gorm redeclared`) |
| The import edit | Each diagnostic's fix carries every edit it needs. `internal/report.go` no longer drops an edit that an earlier diagnostic already suggested | With two roots in a file without the import, only the first fix had it. Applying the second alone failed with `undefined: gorm` |

A Phi edge with no call is left without a Session, as an edge without a position always was. The fix is then partial, and the diagnostic can remain after it.

The `-fix` driver and `analysistest.RunWithSuggestedFixes` merge identical edits from several fixes. `goldentest.ApplyFixes` does the same. `TestGoldenFilesCompile` type-checks the fixtures with all fixes applied, and `TestEachFixCompiles` type-checks each diagnostic's fix applied alone, in every file it edits. `TestEveryDiagnosticCarriesFix` requires each diagnostic in `fix_every_diagnostic.go` to carry its root's fix (#246). The cases are in `testdata/src/gormreuse/fix_result_type.go`, `fix_session_target.go` and `fix_import_*.go`, and in `testdata/src/converge/fix_compile.go`.

## Contract and Directive Fixes (#248)

Four diagnostics used to have no fix, although the fix is mechanical:

| Diagnostic | Fix | Where |
| --- | --- | --- |
| Mutable value passed to an `immutable-param` parameter | `.Session(&gorm.Session{})` after the argument | `Generator.GenerateIsolation` |
| Unused `pure`, `immutable-return`, `immutable-param` | Delete the name from the comment | `reportUnusedDirectiveFuncs` |
| Unused `immutable-input(name)` | Delete each unused `immutable-input(...)` part. An orphan comment loses all of them | `reportUnusedImmutableInputs`, from `ImmutableInputUnused.Parts` |
| Redundant `immutable-param` | Delete `immutable-param` from every comment `DirectiveFuncSet.CommentsOf` finds for the function | `reportRedundantImmutableParam` |
| Unused `ignore` | Delete `ignore` from the comment | `reportUnusedIgnores` |

**Isolating an argument.** The tracker records the call and the parameter index (`callsite.Site.ArgParam`) on the violation (`AddContractViolation`). The index counts no receiver, and neither does `ast.CallExpr.Args` for `r.m(x)`, a method value or a method expression, so it indexes the AST arguments. Only a variadic parameter differs, and a `*gorm.DB` passed to one is packed into a slice that the check never sees. The fix checks that the argument is a `*gorm.DB` expression, wraps an operand that is not a primary expression (`*p` becomes `(*p).Session(...)`), and writes the Session through `sessionEdits`, so it takes its gorm name and import from `gormNameAt` as every other Session does (#200). Several mutable arguments of one call give one diagnostic, because the reports share the position and the message (see `reportKey`). `checkFunction` therefore gives that diagnostic the fixes of every violation in its group with the same message. The Session result is immutable, so one pass resolves the diagnostic. The caller's own reuse of the value is a separate diagnostic with its own fix.

**Deleting a directive.** `directive.Removals` holds every diagnostic whose fix deletes a directive part and reports them in `Flush`, at the end of `RunSSA`. A diagnostic suppressed by an ignore is never recorded. The rewrite of a comment takes out the parts of every diagnostic recorded for it, not only its own. `//gormreuse:pure,immutable-return` on a function with no `*gorm.DB` has two diagnostics, and two different rewrites of the one comment would conflict under `-fix`. Their fixes are now the same edit, which the drivers apply once. A fix is still complete alone, as #200 requires.

`removalEdit` rewrites a comment as follows:

| Comment after the deletion | Edit |
| --- | --- |
| Other names remain | Only the name list changes: `//gormreuse:ignore,pure // why` becomes `//gormreuse:pure // why` |
| No name, and a reason after `//` | The reason stays, from its `//`: `// why`. It may be a note or another tool's directive, such as `//nolint:errcheck` |
| No name, and a reason that starts with `gormreuse:` | **No fix.** `//gormreuse:ignore //gormreuse:pure` would become a pure directive |
| No name and no reason | The comment is deleted. A comment alone on its line goes with the line. A comment after code goes with the blanks before it |

The line test reads the file with `pass.ReadFile`. When it cannot read it, or the file's size differs from the parsed one, there is no fix.

**Which ignores are deleted.** An unused ignore covers no diagnostic of the pass by definition: `Ignores.Ignore` marks an entry used whenever it suppresses one, and a line entry and the function span of a doc-comment ignore share one entry. Two cases remain:

- A function whose analysis panicked lost its diagnostics, so its ignores can look unused. After any `gormreuse: internal error` report, unused ignores keep their report but get no fix.
- An ignore can be used in one variant of a package and unused in the other. The test variant sees calls in `_test.go` files: for example, a direct call of a named Transaction callback makes its parameter mutable (#191) in that variant only. The non-test variant then reports the ignore unused, as it did before #248, and `-fix` would apply that variant's fix too. A pass sees only its own variant, so it cannot tell. So an unused ignore in a non-test file gets no fix when its directory holds a `_test.go` file (`reportIgnoreRemovable`). The pass's own files are checked first, so the test variant withholds the fix too; otherwise the directory is listed, and one that cannot be listed counts as having tests. An ignore in a `_test.go` file is analyzed only by the test variant, which sees the whole package, so it keeps its fix. `testdata/src/ignoretests` checks both, in both variants.

**Redundant immutable-param.** One comment before `a, b := func..., func...` applies to both closures. The comment is edited only when no closure it covers keeps the directive: one that needs it (`needsImmutableParam`) or is also `pure`. Otherwise the redundant closure's diagnostic has no fix. A function that forwards its parameter once to another immutable-param function is reported redundant (see `computeNeedsImmutableParam`). Deleting its directive then exposes the caller-side diagnostic at the forwarding call, which a second pass fixes.

`testdata/src/gormreuse/fix_directive_removal.go` and `fix_isolate_arg.go` pin the output. `testdata/src/converge/fix_directives.go` and `fix_isolate_arg.go` check that one pass converges. `internal/directive/removal_test.go` covers each comment shape.

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
cd e2e/internal && go test ./...

# Regenerate the .golden and .diff fixtures after changing detection or fixes
GORMREUSE_UPDATE_GOLDEN=1 go test -run TestGoldenFiles .

# Run every check CI runs (declscope, both test suites, build, golangci-lint)
./test_all.sh
```

## Releasing

Releases are cut from GitHub Actions, never locally. Before you dispatch one, prepare it in a pull request:

1. In `CHANGELOG.md`, rename `## Unreleased (planned as X.Y.Z)` to `## X.Y.Z (YYYY-MM-DD)`. Check that it covers every behaviour change since the last tag: `git log --oneline vLAST..`.
2. Bump every version pin in `README.md` to the new version. They are the `mise use` line, the `mise.toml` example, the `@vX.Y.Z` caution, `VERSION=` in the tarball steps, and the `.custom-gcl.yml` example. `TestReadmeVersionPinsAgree` fails when any of them names another version than the newest `v*` tag. While a release is prepared, the newest dated `CHANGELOG.md` section names a newer version, and the pins must name that one. The test needs the tags, so CI's test job fetches the full history (`fetch-depth: 0`). In CI a missing tag fails the test. Elsewhere, such as in a shallow clone or a source tarball, the test checks only that the pins agree, and skips the comparison with the release.
3. Merge it, and wait for CI on the merge commit.

Then dispatch the release:

- **Tag and Release** (`tag_and_release.yml`) — the normal path: creates the `vX.Y.Z` tag, then builds and publishes.
- `tag.yml` / `release.yml` are the two halves, dispatchable on their own when a tag already exists.

`tag.yml` tags the commit the workflow was dispatched on, and refuses unless that commit is on `main` and has a successful CI run from a push to `main`. Dispatch it from `main` after CI on the merge commit has passed. Workflow actions are pinned by full commit SHA, with the release in a trailing comment; dependabot updates both.

`release.yml` runs goreleaser (`.goreleaser.yaml`) once for all six targets — darwin/linux/windows x amd64/arm64, `CGO_ENABLED=0` — and uploads the archives plus `checksums.txt` to the release. Those assets are what makes `mise use -g "github:mpyw/gormreuse"` work, so a release with no assets breaks the documented install path. goreleaser is pinned to 2.17.1: 2.18.0 attests every build artifact, which fails the job.

goreleaser groups the commit list by conventional-commit type (`changelog.groups`), and the release header links to `CHANGELOG.md`. Paste the version's `CHANGELOG.md` section into the release notes above that list. Validate config changes with `goreleaser check`, and dry-run with `goreleaser release --snapshot --clean --skip=publish`.

## Testing Strategy

- Use `analysistest` for all analyzer tests
- Test fixtures use `// want` comments for expected diagnostics
- Test structure:
  - `===== SHOULD REPORT =====` - Cases that should trigger warnings
  - `===== SHOULD NOT REPORT =====` - Negative cases
- A fixture function that pins a known limitation carries `[LIMITATION]` in its doc comment or body, and is named in [Known Limitations](#known-limitations). `TestKnownLimitationsListed` checks this in both directions, and checks the README's list against it. Remove the marker and both entries when a fix makes the case work.
- `TestImplementationNotesNamesExist` checks that each code span of these notes names a file or a Go name that exists. A name counts only where code uses it, not in a comment. `TestImplementationNotesArchitectureTree` checks the [Architecture](#architecture) tree both ways: each entry and each Go name in its comments exists, and each directory it lists has all of its Go files listed.

### Suggested-fix fixtures

Each fixture `testdata/src/gormreuse/X.go` has two committed files that record its suggested fixes. So does each file of another fixture package that a fix edits, test files included. `goldenPackages` lists every package directory of `testdata/src` except the gorm stubs and `converge`, so a new package is covered without a change:

| File | Content | Checked by |
| --- | --- | --- |
| `X.go.golden` | The source with every suggested fix applied | `TestGoldenFiles`, `TestGoldenFilesCompile` (the packages must type-check with every `.golden` swapped in), and `RunWithSuggestedFixes` for `gormreuse`, `linedirective`, `noimport` and `aliasimport` |
| `X.go.diff` | A full-context unified diff from `X.go` to `X.go.golden`, for review | `TestGoldenFiles` |

A plain `go test` only compares these files. It never writes them, so CI fails when they are stale. To regenerate them after a change to detection or fixes:

```bash
GORMREUSE_UPDATE_GOLDEN=1 go test -run TestGoldenFiles .
```

This rewrites every `.golden` and `.diff` file and deletes the ones whose source fixture is gone. Outside `gormreuse` it also deletes those of a file that no fix edits any more. `RunWithSuggestedFixes` does not catch that case: it ignores the golden file of a file without fixes. Before #316, two `linedirective` goldens kept a Session fix that #243 had withheld. Read each changed `.diff` before you commit it: it shows exactly what `-fix` now does to the fixture. The failure message of `TestGoldenFiles` prints the same command. Rendering a `.diff` needs `diff` on `PATH`, and a failing `diff` fails the test.

`TestEachFixCompiles` applies each diagnostic's fix on its own to the same packages and to `converge`, and type-checks each variant of the package (#200).

`TestFixesConverge` applies the fixes to each fixture in `testdata/src/converge`, then lints the fixed output again. It fails when the directory has no fixture, when a fixture's fixes change nothing, when the fixed output does not type-check, or when any diagnostic remains. So every fixture there must need a fix, and one pass of fixes must resolve all of it. It analyzes the package once and re-lints all the fixed files as one package.

A fixture of `testdata/src/gormreuse` whose fixes must also converge is listed in `convergeGormreuseFixtures` (today `fix_paths.go`, `fix_isolate_arg.go`, `fix_later_loop_merge.go` and `fix_closure_param.go`) instead of being copied into `converge`. `TestFixesConverge` re-lints its committed `.golden`, which `TestGoldenFiles` keeps equal to the current fixes.

> **Trap: never name a fixture `*_test.go` unless you mean to.** `analysistest`
> loads a package's `*_test.go` files as part of both the normal package and the
> test variant, so every diagnostic in such a fixture is collected **twice**
> (this produced 343 duplicated diagnostics in PR #57). Use a plain `.go` name.
> The intentional exceptions are `testdata/src/filefilter/code_test.go`, which
> exists specifically to test the analyzer's `-test` flag handling, and
> `testdata/src/ignoretests/code_test.go`, which gives its package test files.

### Allocation-growth gate (#302)

`TestAllocationGrowth` in `growth_test.go` generates each case at two sizes, runs the analyzer on them, and fails when the heap allocations of its `Run` grow faster than the case's limit. The packages are generated in a temporary `GOPATH`, so no large fixture is committed.

| Limit | Sizes | Ratio allowed | For |
| --- | --- | --- | --- |
| `growthLinear` | n, 2n | 2.5 | Many small functions, or one function whose work is linear |
| `growthNearLinear` | n, 2n | 3.0 | Shapes that grow a little faster than n (about 2.3x, 2.5x with the race detector), such as one large function |
| `growthQuadratic` | n, 2n | 4.5 | Shapes that are quadratic today |
| `growthExponential` | n, n+4 | 2.0 | Shapes that used to double their work per statement (about 16x) |

How it measures:

- It counts `runtime.MemStats.Mallocs` around `Analyzer.Run` only, so loading and building SSA are not counted. The packages run one at a time (`checker.Options.Sequential`) and the test is not parallel, because the count is process-wide.
- It analyzes every package three times and keeps the smallest count. The analyzer does a few percent more or less work from run to run with map order, and the race detector makes `sync.Pool` drop items at random.

**Rejected: a wall-clock limit (#260's time limit).** Under CI's flags (`-race -covermode=atomic -coverpkg=./...`) the analyzer ran about 25 times slower, so a limit was either flaky or too loose to catch anything. Allocation counts repeat, and the race detector and coverage change most ratios little.

Limitations:

- Work that allocates nothing, such as a linear search repeated in a loop, is not counted. The race detector's `sync.Pool` drops make some of it visible (`switchuse` grows about 2.3x without the race detector and about 3x with it, so it has the quadratic limit).
- A case whose issue is still open has `after: "#NNN"` and is skipped. Its fix removes the field, and every later performance fix adds its shape with a limit it meets.

### Testdata Organization

| Directory under `testdata/src` | Test | Content |
| --- | --- | --- |
| `gormreuse/` | `TestAnalyzer`, the golden tests | The main fixtures, one file per topic, named after it (`basic.go`, `evil.go`, `session_alias.go`, `subquery.go`, `fix_*.go`, ...). Each has a `.golden` and a `.diff` |
| `converge/` | `TestFixesConverge` | Code whose fixes must resolve every diagnostic in one pass |
| `determinism/` | `TestDeterministicDiagnostics` | Several roots reused at one position, and unused directives for the report order |
| `purelib/`, `externalpure/` | `TestCrossPackageDirectives` | Directives declared in one package and called from another |
| `filefilter/` | `TestFileFilter` | The `-test` flag and generated files |
| `ignoretests/` | `TestIgnoreFixWithTestFiles` | Unused ignores in a package with a test file, and which of them get a fix |
| `paramtests/` | `TestParamFixWithTestFiles` | A parameter root in a package with a test file, and which fix it gets |
| `linedirective/`, `linedirectivedep/` | `TestLineDirective` | `//line` directives |
| `cgofile/` | `TestCgoFiles` | A cgo package. Skipped without a C compiler |
| `noimport/`, `aliasimport/` | `TestSuggestedFixesWithImport`, `TestSuggestedFixesWithAlias` | How a fix names `gorm` |
| `fakegorm/` | | A package that `gormreuse/fix_import_other.go` imports as `gorm` |
| `gorm.io/gorm/`, `github.com/stretchr/testify/` | | Stubs of the real packages |

Directives across packages are tested by `TestCrossPackageDirectives`: `testdata/src/purelib` declares them and `testdata/src/externalpure` calls them. Both packages' diagnostics and purelib's facts are checked. `TestCrossPackageFactsE2E` in `cmd/gormreuse` runs the same kind of pair through the built command and through `go vet -vettool`.

### E2E Tests

The `e2e/internal/` directory contains tests that verify actual GORM SQL behavior using sqlmock. This is a separate Go module (its own `go.mod`) to avoid dependency conflicts, and CI tests and lints it on every PR (`cd e2e/internal && go test ./... && golangci-lint run ./...`). Dependabot covers it, so GORM bumps arrive as PRs; re-check go-gorm/gorm#7592 on each (see the README). Every test asserts (`t.Errorf`) — the suite guards the linter's core clone-semantics premise: mid-chain (`clone==0`) reuse accumulates conditions; root / `Session()` / `Transaction`- and `FindInBatches`-callback (`clone>0`) handles fork a fresh Statement and are safe; a `Connection`-callback handle is `clone==0` and accumulates. `scopes_session_test.go` pins which Session-family calls inside a `Scopes` callback leak the transaction (go-gorm/gorm#7592): only a returned one. It also pins the exceptions in [Session and Debug copies](#session-and-debug-copies-174): a `Session()`/`Debug()` copy of a mid-chain value sees later changes to it, and `Session{Initialized: true}` returns a mid-chain value. `subquery_test.go` pins that a subquery, group condition or Joins condition argument is only read, and that a change to it before the outer query runs leaks into that query (see [Subquery and condition arguments](#subquery-and-condition-arguments-185)). `fixsql_test.go` checks that suggested fixes keep every condition the code applied (see [Discarded Statements Under a Session Fix](#discarded-statements-under-a-session-fix-199)). `TestFixsqlCasesCoverBefore` fails when an exported function of `fixsql/before` is in no case list. A topic's test file registers its cases with `registerFixsqlCases`.

`gorm_stub_test.go` checks the gorm stub in `testdata/src/gorm.io/gorm` against the real gorm, since Dependabot bumps only the real one:

| Test | Checks |
| --- | --- |
| `TestGormStubMatchesGorm` | Each method of `*gorm.DB`: its name, and for each parameter and result whether it is a `*gorm.DB`, a callback (with the same check inside) or another type. The analyzer finds callback parameters by their declared types (#257). A method that the stub leaves out must be in `gormStubOmitted`, with the reason |
| `TestGormStubFuncsMatchGorm` | Each package-level function of the stub (`Open`, `Expr`) against `gormRealFuncs`, in the same way |
| `TestGormStubSessionMatchesGorm` | Each field of the stub's `Session` exists in gorm's with the same type. Each field that `callsite` reads from a Session literal (`Initialized`, `NewDB`, `Context`) is in both |
| `TestGormReceiverUsesMatchGorm` | Each method that returns no `*gorm.DB` is in `typeutil.receiverUses` or in `gormNonChainBranches` (#307) |

When a gorm bump fails one of them, decide how the analyzer should treat the new or changed name, then add it to the stub or to the list.

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

Each fixture function marked `[LIMITATION]` under `testdata/src` pins one of these cases. The README lists the same cases for users, in the same order. `TestKnownLimitationsListed` fails when:

- a marked function is not named in this section;
- a fixture named here (in a Fixtures column, or after "Fixtures:") is not a marked function;
- a marker comes after the last function of its file, so it marks none;
- the README's section lacks one of the four kinds below, or one of its tables has a different number of rows.

**Missed reuse (false negatives).** The fixture has no `want` at the missed use.

| Case | Fixtures | Cause |
| --- | --- | --- |
| A closure assigns a captured variable: `f := func() { q = db.Where(...) }; f()` | `closureModifiesCaptured` | The store in the closure is not a root the caller's uses resolve to |
| A `Session` copy used in `go`/`defer`, with the root changed after the statement | `siteDeferSessionCopyBranchAfterDefer` | The alias use is ordered by the statement's position, not by when the call runs ([Session and Debug copies](#session-and-debug-copies-174)) |
| `defer` inside a `go` closure, or `defer` → `go` → `defer`, with the other use after the statement | `deferInsideGoroutine`, `nestedDeferGoroutineDefer` | A deferred call nested in a spawned closure is not recorded as a use |
| A method value merged by a Phi: `if c { find = q.Find } else { find = q.Find }; find(nil)` | `methodValueConditional`, `methodValuePhiWithNil` | The bound-method receiver is not traced through the Phi |
| A closure passed once, outside a loop, to a function that calls it many times | `repeatsClosurePassedToRepeater` | Only the sites in the declaring function are counted ([Uses that run more than once](#uses-that-run-more-than-once-249-252)) |
| A closure stored in a slice or a local struct field, then called twice through it: `fs := []func(){f}; fs[0](); fs[0]()` | `repeatsClosureCalledThroughSlice`, `repeatsClosureCalledThroughField` | Only the uses of the closure value are sites. The store is one site, and a call through the container is a dynamic call |
| A local reassigned and used in a loop: `for … { q = q.Where(c); q.Find(nil) }` | `fsoLocalLoopCarried` | The loop-header Phi makes the reassignment an assignment. The loop-carried read is checked for fields and globals only (#182) |
| A derived variable that flows back: `if c { q2 = q.Where("a") }; if d { q = q2 }` | `derivedThenMergedBack` | `isAssignment` asks whether a merge *can* hand back the root, not whether it does on that path ([Which merges are reassignments](#which-merges-are-reassignments-175)) |
| A field rebuilt in a loop after it was copied: `p.db = q; for … { p.db = p.db.Where(c) }; q.Find(nil)` | `fsoLoopRebuildOfAliasedValue` | `isLoopFieldRebuild` makes the rebuild an assignment, so the first iteration's read of `q` is not a branch (#182) |
| A pure function that returns a `Session` copy of its argument | `paCopyHidden` | The copy is not in `PureVerdicts.Returned` ([Session and Debug copies](#session-and-debug-copies-174)) |
| A `*gorm.DB` global assigned in another function, a closure or the initializer | `pvInitializedAtPackageLevel`, `pvStoreInClosure` | A load sees only the stores in its own function ([Package-level variables](#package-level-variables-183)) |
| A struct field read through a parameter or a receiver: `s.db` twice | `fieldOfStructParam` | The field is not a mutable root ([Struct roots](#struct-roots-181)) |
| The `immutable-param` caller contract through an interface or a func-typed parameter | `contractInterface` | No static callee |
| A method expression of a method promoted through an embedded `*gorm.DB`: `W.Find(w, nil)` twice | `exprPromoted` | The thunk selects the field in its body, and the receiver `w` is not followed to it ([Calls through a method expression](#calls-through-a-method-expression-258)) |

**Extra reports (false positives).**

| Case | Fixtures | Cause |
| --- | --- | --- |
| `a, b := pair(db)`, where `pair` returns two new chains, then `a.Find(nil); b.Find(nil)` | `opaqueTuplePairShared` | Every `*gorm.DB` of one tuple shares the call as its root (#176) |
| `G = db.Where("x"); if c { G = G.Where("a") }; G.Find(nil)` for a global or a field | `pvConditionalRebuildOfStore` | A store is not a merge point the way a local's Phi is |
| A captured pointer variable reassigned to a second returned struct: `p := mk(s); go func() { p.db.Find(nil) }(); p = mk(s); p.db.Count(nil)` | `cosReturnedReassigned` | A load of a reassigned pointer variable is resolved to every pointer stored in it, so the last read is matched to the first struct too ([Struct roots](#struct-roots-181)) |
| A recursive helper that returns only immutable values, branched twice | `notInferredRecursive` | Inference is a least fixed point, so a function that returns its own result is never inferred ([Inferred immutable-return](#inferred-immutable-return-261)) |
| A closure called on exclusive branches, once directly and once inside another closure | `rcsExclusiveAcrossClosures` | Sites in two functions are taken to both run ([Uses that run more than once](#uses-that-run-more-than-once-249-252)) |

**Wrong position.** Uses in a deferred closure are ordered by their source position, not by when they run. `defer func() { q.Count(nil) }(); q.Find(nil)` is reported at `q.Find`, which runs first. A deferred call such as `defer q.Count(nil)` is ordered after the function's other uses and reported at the `defer`. Fixtures: `ifInsideDefer`, `ifElseInsideDefer`, `nestedIfInsideDefer`, `tripleNestingIfForDefer`, `tripleNestingForIfDefer`, `deferCapturingLoopVar`, `nestedDeferClosures`, `tripleNestedDeferClosures`, `nestedDeferWithFor`, `ultimateIfForDeferChaos`.

**No fix.** A `go` or `defer` statement that changes the root in place where another use may see it gets no fix ([In-Place Changes Outside the Variable](#in-place-changes-outside-the-variable-243)). Fixtures: `godeferGoChainOnly`, `godeferDeferChainOnly`, `godeferDeferHelperOnly`, `godeferDeferHelperInLoop`. Before #309, a root used only in `go`/`defer` statements, as in `for … { go q.Find(nil) }` or `go q.Count(nil); go q.Find(nil)`, also had none: its branch uses were not in `Violation.AllUses`. Now `Tracker.violationUses` gives such a root its branch uses, and the fix is a Session at the root, since they are not ordered.

Other limitations have no fixture marker. They are described with their design:

- A call through an exported, reassigned or address-taken `var f = func(...)` does not honour the closure's directives ([Package-level variables](#package-level-variables-183)).
- A getter of a field or a global is inferred `immutable-return` though another package, or code given the field's address, assigns it a chain ([Inferred immutable-return](#inferred-immutable-return-261)). A direct read of the field misses the same reuse.
- The limits of `-fix` are in the fix sections, from [Reassignment Fix Target](#reassignment-fix-target) to [Fixes That Compile](#fixes-that-compile-200).

## Scopes Session-family warning (temporary, #196)

`internal/scopeswarn` works around go-gorm/gorm#7592. It reports a `Session`/`WithContext`/`Debug` call only when its result is what a `Scopes` callback returns.

Why only the returned value (checked in `e2e/internal/scopes_session_test.go`):

- gorm runs the create/update/delete callbacks on the value the last scope returned (`db = scope(db)` in `executeScopes`).
- `BeginTransaction` stores its flag with `InstanceSet`, which calls `getInstance`. On a `clone>0` value that clones the Statement, so `CommitOrRollbackTransaction` never finds the flag and the transaction stays open.
- `Session`/`WithContext`/`Debug` return `clone>0`. Every other chain method returns a `clone==0` value from `getInstance`.

So `return q.Session(...).Where("x")` is safe, and `return other.Session(...)` leaks even though `other` is not the callback parameter. The issue's suggestion to restrict the warning to values derived from the parameter was wrong; the rule keys off the return value instead.

Only some finishers leak (#308, checked in `e2e/internal/scopes_finisher_test.go`). gorm registers `BeginTransaction` and `CommitOrRollbackTransaction` only for create, update and delete. The query, row and raw callbacks begin no transaction, so a callback that returns a Session result is harmless for `Find`, `Count`, `Pluck`, `Scan` or `Exec`. Before #308 the rule ignored the finisher and reported gorm's own `tests/scopes_test.go:60`, whose chain ends in `Find`.

So a Scopes call is skipped when its chain provably only reads (`scopeswarn.ReadsOnly`): every use of its result, and of each chain method after it, is the receiver of a chain method (the chainable API, `Session`, `WithContext`, `Debug`) or of a finisher that only reads (`Find`, `First`, `Take`, `Last`, `FirstOrInit`, `Count`, `Pluck`, `Scan`, `Row`, `Rows`, `Exec`). `CollectScopesOnlyCallbacks` reads only the other Scopes calls. A callback passed to one Scopes call that may write is still reported.

| Chain from `db.Scopes(cb)` | Reported |
| --- | --- |
| `.Where("x").Find(&x)`, `.Count(&n)`, `q := db.Scopes(cb); q.First(&u)` | No |
| `.Delete(&u)`, `.Update(...)`, `.Create(...)`, `.FirstOrCreate(...)`, `.Save(...)` | Yes |
| Returned, stored in a field, passed to a function or another method, or discarded | Yes: it may reach a write elsewhere |

How the rule finds its input:

- **Callbacks**: `callback.CollectScopesOnlyCallbacks`, the Scopes half of the collector that reuse detection uses (`resolveMayFuncs`). It follows the varargs array, slice literals, `append`, Phi edges, local, captured and package-level variables, struct fields, and method values (both the `$bound` wrapper and the method). Package-level initializers live in the synthetic `init`, which is not in `SrcFuncs`, so the resolver scans it for stores to globals.
- **Returned value**: followed through Phi edges, local and captured variables, `Extract`, and the returns of static callees (helpers, invoked closures). A callee's parameter maps back to the caller's argument, so `return passThrough(q.Debug())` is reported. Calls are followed to depth 16 and never into a function already on the stack.

Limitations:

- A dynamic callee (a func-typed parameter, an interface method) ends the trace, so a Session result returned through one is not reported.
- A callback that returns a root `*gorm.DB` (also `clone>0`, for example a package-level `DB`) leaks the same way. It is not reported, since the warning is about the three Session-family methods.
- The rule does not model the order of several scopes; each callback that returns a Session-family result is reported, even when a later scope returns a `clone==0` value.
- The rule does not read `SkipDefaultTransaction`. With it, a write begins no transaction and does not leak, but the setting is in a config the analyzer does not see.
- A chain that leaves the function is taken to write, even when every caller only reads.
- A package-level closure is a known Scopes callback, and reuse detection analyzes its body like any closure ([Package-level variables](#package-level-variables-183)).

## Related Projects

- [goroutinectx](https://github.com/mpyw/goroutinectx) - Goroutine context propagation linter
- [zerologlintctx](https://github.com/mpyw/zerologlintctx) - Zerolog context propagation linter
- [ctxweaver](https://github.com/mpyw/ctxweaver) - Code generator for context-aware instrumentation
