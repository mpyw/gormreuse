# Changelog

These notes say what changes for users in each release, and what to do about it. The full commit list of each release is on [GitHub Releases](https://github.com/mpyw/gormreuse/releases). Releases up to 0.22.0 have their notes there only.

## Unreleased (planned as 0.23.0)

gormreuse now follows a `*gorm.DB` through far more code. It also reads directives across packages, and infers `immutable-return` for most connection helpers. It no longer counts methods such as `AddError` as a branch. `-fix` offers fewer fixes, but each one compiles and keeps every condition.

> [!IMPORTANT]
> **Expect new reports on code that passed with 0.22.0.** Most of them are real reuse. Run `gormreuse ./...` before you bump the version in CI. Then use the tables below to resolve each kind. Also check your exclude rules: [some messages changed](#changed-messages).

| Section | What is in it |
| --- | --- |
| [Highlights](#highlights) | The main changes in one table |
| [New diagnostics](#new-diagnostics) | Reports that 0.22.0 did not give, and what to do about each |
| [Removed diagnostics](#removed-diagnostics) | Reports that 0.22.0 gave and 0.23.0 does not |
| [Changed messages](#changed-messages) | Messages that an exclude rule may match |
| [Directives](#directives) | How the directives changed |
| [`-fix`](#-fix) | How the fixes changed |
| [Performance](#performance) | Wall times |
| [Integration](#integration) | The Go API and golangci-lint |

### Highlights

| Change | Effect |
| --- | --- |
| More code is followed | Loops, closures that run more than once, `go`/`defer`, method values, method expressions, struct fields, package-level variables, local function variables and helper results |
| Fewer false reports | `AddError`, `DB`, `Get`, `InstanceGet` and `Callback` are not branches. The Scopes warning needs a chain that may write |
| Directives across packages | A directive on an exported function applies in the packages that call it. It travels as an analysis fact, so a broken contract is not trusted there |
| Inferred `immutable-return` | A helper whose body returns only `Session`, `WithContext`, `Debug`, `Begin`, `gorm.Open` or `nil` needs no directive. So does a getter of a field or a package-level variable, and a closure that returns an immutable value |
| Safer `-fix` | Every fix compiles, alone or with the others. A fix never drops a condition. When it cannot keep every condition, there is no fix |

<details>
<summary>Results on gorm's own repository</summary>

On gorm's own repository (v1.31.2), 0.23.0 reports fewer problems:

| Module | 0.22.0 | 0.23.0 | Removed | Added |
| --- | --- | --- | --- | --- |
| Root | 151 | 66 | 90 | 5 |
| `tests` | 15 | 14 | 2 | 1 |

- Most removed reports in the root module had a state-only call such as `AddError` as a branch (#307).
- Four of the five added ones are a `*gorm.DB` parameter used in a loop. The fifth is a report that moved within its line.
- In `tests`, the added report is a `Connection` callback.
- The removed ones in `tests` are two Scopes warnings: a `WithContext` that the callback does not return, and a `Session` whose chain ends in `Find`.

</details>

### New diagnostics

Each row is a report that 0.22.0 did not give.

> [!TIP]
> Run `gormreuse -fix -diff ./...` to see the fixes for most of these. Some reports have no fix on purpose. See [Automatic Fixes](README.md#automatic-fixes).

#### Reuse

<table>
<tr><th>Now reported</th><th>Example</th><th>What to do</th></tr>
<tr><td>

A `Connection` callback's `tx` branched twice (#173)

</td><td>

```go
db.Connection(func(tx *gorm.DB) error {
    tx.Exec(a)
    return tx.Raw(b).Scan(&x).Error
})
```

</td><td>

gorm passes a mid-chain handle there. Start the callback with `tx = tx.Session(&gorm.Session{})`

</td></tr>
<tr><td>

A `Session`/`Debug` copy used after its root changed (#174)

</td><td>

```go
c := q.Session(&gorm.Session{})
q.Where("a").Find(&x)
c.Count(&n)
```

</td><td>

End the root's chain with `.Session(&gorm.Session{})` before you copy it

</td></tr>
<tr><td>

`Session(&gorm.Session{Initialized: true})` branched twice (#174)

</td><td>

```go
s := db.Session(&gorm.Session{Initialized: true})
s.Find(&a)
s.Find(&b)
```

</td><td>

Its result is a mid-chain handle. Use `Session(&gorm.Session{})`

</td></tr>
<tr><td>

A chain kept in another variable (#175)

</td><td>

```go
if c { q2 = q.Where("a") }
q.Count(&n)
q2.Find(&x)
```

</td><td>

`q2` shares `q`'s Statement. Isolate `q` with a Session first

</td></tr>
<tr><td>

The result of a helper, a func value or an interface method (#176)

</td><td>

```go
db, err := NewDB()
db.Find(&a)
db.Find(&b)
```

</td><td>

Most helpers are now [inferred](README.md#inferred-immutable-return) safe. Otherwise mark the helper `//gormreuse:immutable-return`. For an interface method, write `repo.DB().Session(&gorm.Session{})`

</td></tr>
<tr><td>

A `pure` function's result that may be its argument (#188, #229)

</td><td>

```go
//gormreuse:pure
func id(db *gorm.DB) *gorm.DB { return db }

r := id(q)
r.Find(nil)
q.Find(nil)
```

</td><td>

`r` is `q`. Branch only one of them, or isolate `q` with a Session

</td></tr>
<tr><td>

A subquery changed before the outer query runs (#185)

</td><td>

```go
x := db.Where("id IN (?)", sub)
sub.Where("a").Find(&y)
x.Find(&z)
```

</td><td>

The message is `mutable root passed as a query argument is read when the query runs`. End the subquery's chain with a Session

</td></tr>
<tr><td>

One use that runs more than once (#177, #249, #252, #305)

</td><td>

```go
for x := range seq { q.Find(&x) }

f := func() { q.Find(&x) }
f(); f()

for … { g.Go(func() error { … }) }

run := func() { for … { f() } }
run()
```

</td><td>

Define the root inside the loop or closure, or end its chain with a Session

</td></tr>
<tr><td>

A `go`, `defer` or helper call in a loop (#254)

</td><td>

```go
for … { go q.Find(nil) }
for … { helper(q) }
```

</td><td>

Same as above

</td></tr>
<tr><td>

A method value used under `go` or `defer` (#178, #254)

</td><td>

```go
f := q.Find
defer f(nil)
q.Count(&n)
```

</td><td>

As for the direct call

</td></tr>
<tr><td>

A value that may hold one of several roots (#252)

</td><td>

```go
q := b
if c { q = a }
q.Find(&x)
a.Find(&y)
```

</td><td>

Isolate the root that is used again

</td></tr>
<tr><td>

A field of a struct that a helper returns (#181, #250)

</td><td>

```go
s := newApp(db)
s.DB.Find(&a)
s.DB.Find(&b)
```

</td><td>

Mark the helper `//gormreuse:immutable-return`, or put the Session where the helper builds the struct

</td></tr>
<tr><td>

A field that a spawned closure reads, stored after the closure (#303)

</td><td>

```go
go func() { d.db.Find(nil) }()
d.db = q
q.Find(nil)
```

</td><td>

Store an isolated value

</td></tr>
<tr><td>

A package-level variable stored and reused in one function (#183)

</td><td>

```go
G = db.Where("t")
G.Find(&a)
G.Find(&b)
```

</td><td>

Store an isolated value

</td></tr>
<tr><td>

A named callback that is also called directly or exported (#191)

</td><td>

`db.Transaction(txBody)` and `txBody(q)`

</td><td>

Its parameter is mutable again. Mark the function `//gormreuse:immutable-param`, or stop calling it directly

</td></tr>
</table>

#### Directive contracts

<table>
<tr><th>Now reported</th><th>Example</th><th>What to do</th></tr>
<tr><td>

A `pure` function that pollutes its argument in more ways (#187, #308)

</td><td>

- `go db.Find(nil)`
- `same(db).Find(nil)`
- `h.db = db`
- a method value: `f := db.Where`
- a closure: `get := func() *gorm.DB { return db }; func() { get().Where("x") }()`

</td><td>

Fix the function, or remove `pure`

</td></tr>
<tr><td>

A `pure` function that passes its argument to a `pure` function that breaks its contract (#187)

</td><td>

```go
//gormreuse:pure
func broken(db *gorm.DB) { db.Where("x") }

//gormreuse:pure
func caller(db *gorm.DB) { broken(db) }
```

</td><td>

The message is `pure function passes *gorm.DB argument to broken, which violates its pure contract`. Fix `broken` first

</td></tr>
<tr><td>

An `immutable-return` function that returns a mutable value through more shapes (#194)

</td><td>

```go
// An IIFE
return func() *gorm.DB { return conn.Where("x") }()

// A local closure
f := func() *gorm.DB { return conn.Where("x") }
return f()

// An unexported global assigned only mutable chains
var gq = conn.Where("pkg")
return gq

// A deferred write to a named result
r = conn.Session(&gorm.Session{})
defer func() { r = r.Where("x") }()
return
```

</td><td>

Fix the function, or remove the directive

</td></tr>
<tr><td>

An `immutable-return` function that returns a package-level func var's chain (#315)

</td><td>

```go
var get = func() *gorm.DB { return conn.Where("x") }
```

then `return get()`

</td><td>

Fix the function, or remove the directive

</td></tr>
<tr><td>

An `immutable-input` callback that escapes (#193)

</td><td>

`forward(cb, db)`

</td><td>

The message is `callback cb escapes (...)`. Pass the callback only to another `immutable-input` function or to `Transaction`

</td></tr>
<tr><td>

An `immutable-input` callback called with the result of a func value

</td><td>

```go
//gormreuse:immutable-input(cb)
func run(cb func(*gorm.DB) error, get func() *gorm.DB) error {
    return cb(get())
}
```

</td><td>

`get()` may be mutable. Pass `get().Session(&gorm.Session{})`

</td></tr>
<tr><td>

An `immutable-input` directive attached to no function or closure (#193)

</td><td>

```go
//gormreuse:immutable-input(cb)
db.Find(nil)
```

</td><td>

The message is `unused gormreuse:immutable-input directive: not attached to a function or closure`. Move it to the function's doc comment, or delete it

</td></tr>
<tr><td>

`immutable-param` caller contract in more calls (#192, #195, #258, #304)

</td><td>

- `go apply(q)`
- `defer apply(q)`
- `pkg.Apply(q)`
- `Repo.apply(r, q)`
- `f := apply; func() { f(q) }()`

</td><td>

`-fix` appends `.Session(&gorm.Session{})` to the argument

</td></tr>
</table>

#### Ignores, files and errors

| Now reported | Example | What to do |
| --- | --- | --- |
| A trailing ignore no longer covers the next line (#189) | `q.Find(nil) //gormreuse:ignore` followed by `q.Count(nil)` | Put the ignore on the line it suppresses |
| A function-level ignore that suppresses nothing (#190) | `//gormreuse:ignore` on a clean function | Reported unused. Delete it |
| Files that import `"C"` (#186) | Any reuse in a cgo file | They are analyzed now. `-fix` does not edit them |
| A panic inside the analyzer (#186, #315) | `gormreuse: internal error analyzing <func>: ...`, or `gormreuse: internal error in <stage> of package <path>: ...` at the package clause | Please [open an issue](https://github.com/mpyw/gormreuse/issues) with the code. A function whose directive check panicked is not trusted |

### Removed diagnostics

Each row is a report that 0.22.0 gave and 0.23.0 does not.

<table>
<tr><th>No longer reported</th><th>Example</th></tr>
<tr><td>

A subquery passed to several queries (#185)

</td><td>

```go
db.Where("id IN (?)", sub).Find(&a)
db.Where("id NOT IN (?)", sub).Find(&b)
```

</td></tr>
<tr><td>

A subquery in a `FirstOrInit`/`FirstOrCreate` condition, or in a wrapper, passed to several queries (#253)

</td><td>

```go
db.FirstOrInit(&u, "id IN (?)", sub)
db.FirstOrCreate(&u, "id IN (?)", sub)
```

The wrappers are `gorm.Expr("?", sub)`, `sql.Named("s", sub)`, `map[string]any{"s": sub}` and `clause.Expr{Vars: []any{sub}}`.

</td></tr>
<tr><td>

A Scopes callback that uses `Session`, `WithContext` or `Debug` without returning it (#196)

</td><td>

```go
db.Scopes(func(q *gorm.DB) *gorm.DB {
    q.Session(s).Find(&x)
    return q
})
```

</td></tr>
<tr><td>

A Scopes callback that returns a `Session` result, when every chain from the `Scopes` call ends in a read such as `Find` or `Count` (#308)

</td><td>

```go
db.Scopes(func(q *gorm.DB) *gorm.DB { return q.Session(s) }).Find(&x)
```

</td></tr>
<tr><td>

The result of a getter of a field or a package-level variable, branched twice (#308)

</td><td>

```go
d := r.DB()
d.Find(&a)
d.Count(&n)
```

</td></tr>
<tr><td>

The result of a closure that returns an immutable captured value (#308)

</td><td>

```go
get := func() *gorm.DB { return conn }
d := get()
d.Find(&a)
d.Count(&n)
```

</td></tr>
<tr><td>

Uses on paths that cannot both run, through a closure (#179)

</td><td>

```go
if c { f() } else { q.Count(&n) }
```

</td></tr>
<tr><td>

A captured variable rebound before the closure runs (#251)

</td><td>

```go
db = db.WithContext(ctx)
db.Find(&a)
func() { db.Count(&n) }()
```

</td></tr>
<tr><td>

The same, when the closures are spawned, deferred or passed on (#303)

</td><td>

```go
db = db.WithContext(ctx)
g.Go(func() error { return db.First(&u).Error })
g.Go(…)
```

</td></tr>
<tr><td>

A closure that builds a new chain on each call (#251)

</td><td>

`base := func() *gorm.DB { return db.Model(&User{}) }` on an immutable `db`, called twice

</td></tr>
<tr><td>

The builder idiom on a field (#182)

</td><td>

```go
r.db = r.db.Where("t")
r.db.Find(&x)
```

</td></tr>
<tr><td>

Two calls through a method value of an immutable value (#178)

</td><td>

```go
where := db.Session(s).Where
where("a").Find(&x)
where("b").Find(&y)
```

</td></tr>
<tr><td>

A Transaction or FindInBatches callback under `go`/`defer`, or held in a variable or field (#256, #257)

</td><td>

`go db.Transaction(cb)`, `db.Transaction(s.cb)`

</td></tr>
<tr><td>

A `pure` directive through a method value (#180)

</td><td>

```go
f := r.helper
f(q)
q.Find(&x)
```

</td></tr>
<tr><td>

A `pure` directive through a local function variable that a closure captures (#304)

</td><td>

```go
keep := helper
func() { keep(q) }()
q.Find(&x)
```

</td></tr>
<tr><td>

A `*gorm.DB` formatted under `go`/`defer` (#309)

</td><td>

```go
defer log.Printf("%v", q)
q.Find(&x)
```

</td></tr>
<tr><td>

`AddError`, `DB`, `Get`, `InstanceGet` and `Callback` counted as a branch (#307). `ToSQL` now only reads its receiver

</td><td>

```go
sqlDB, _ := db.DB()
db.AutoMigrate(&User{})
```

```go
q.Where("a").Find(&x)
q.AddError(err)
```

</td></tr>
</table>

> [!WARNING]
> **A `//gormreuse:ignore` written for a removed report is now unused**, and is reported as such. The Scopes warning (#196, #308) and the state-only methods (#307) are the most common cases. `-fix` deletes it. In a non-test file of a package with `_test.go` files there is no fix, because the test variant may still need it. Delete those by hand.

### Changed messages

An exclude rule that matches a message must match the new text. A rule that matches only `*gorm.DB reused` keeps working.

| Diagnostic | 0.22.0 | 0.23.0 |
| --- | --- | --- |
| Scopes warning (#196) | `Session() in Scopes callback causes transaction leak (GORM bug)` | `Session() result returned from Scopes callback causes transaction leak (GORM bug)` |
| Scopes warning for `WithContext` and `Debug` | `WithContext() in Scopes callback causes ...` | `WithContext() result returned from Scopes callback causes ...` |
| A variadic `immutable-input` callback (#259) | `unused gormreuse:immutable-input directive: parameter "cbs" is not a function type` | `... parameter "cbs" is variadic; variadic callbacks are not supported` |
| Reuse | Sometimes `(root at …)` or `(first branch at …)` alone | Always `(root at …, first branch at …)` |

### Directives

| Change | Detail |
| --- | --- |
| Inferred `immutable-return` (#261, #308) | A function without the directive is treated as `immutable-return` when its body proves it. See [Inferred immutable-return](README.md#inferred-immutable-return). A declared directive on such a function is not reported unused |
| Directives across packages (#195, #314) | `pure`, `immutable-return`, `immutable-param` and `immutable-input(name)` on an exported function apply in other packages. A same-line directive and a generic instance work too. A contract that the body breaks is trusted only in its own package. So is a contract that holds only because the body relies on a broken one |
| Facts need the dependencies | The `gormreuse` command, `go vet` and golangci-lint analyze each dependency first |
| Generated files (#259, #315) | A directive in a generated file is not checked, so it is not trusted, in its own package too. The function exports no fact |
| Method expressions (#258) | Every directive applies to `Repo.helper(r, q)` and `(*Repo).helper` |
| Package-level func vars (#183, #257) | Every directive applies to a call through `var f = func(...)` when `f` is unexported, assigned only in its declaration, and its address is never taken |
| Local function variables (#304) | Every directive applies to a call through a local variable that is assigned once, also from a closure that captures it: `f := helper; func() { f(q) }()` |
| `immutable-input` placement (#193) | It works where `pure` does: a doc comment, after `{` on the same line, or the line before a closure |
| A directive after a closure's `{` (#315) | It marks only that closure. Before, it also marked the function that holds the closure |
| One ignore scope (#190) | A line, function or file ignore covers every kind of diagnostic in the same way. A function-level ignore also covers its closures |

### `-fix`

`-fix` now offers fewer fixes. Each one is safe to apply.

| Change | Detail |
| --- | --- |
| Every fix compiles (#200) | Alone, as an editor's quick fix, or with every other fix. The `gorm` import is added under a free name when needed |
| No fix that drops a condition (#199, #243, #309, #310, #312) | A discarded statement such as `q.Where("a")` must be reassigned. When it cannot be, or when other code changes the root in place, there is no fix. See the examples below |
| Roots used only in `go`/`defer` (#309, #312) | `go q.Count(&n); go q.Find(&x)` and `for … { go q.Find(&x) }` get a Session at the root. The message names the first branch |
| The right variable (#198) | A reassignment rebinds the variable that holds the reused root, never an outer receiver |
| Parameter roots (#201, #245, #313) | An exported function, a method, a function value or a closure without a `*gorm.DB` result gets `tx = tx.Session(&gorm.Session{})` at the top of the body. A `Transaction` wrapper's callback is such a closure. `immutable-param` is added only when every caller of a function declaration is checked |
| Sessions by control flow (#202, #247, #306, #311) | Sessions go where the paths need them. That includes a reassignment read in a later loop, a value merged with a parameter, and every root that a merge may hold. One pass of fixes resolves the diagnostics |
| New fixes (#248) | Isolate an argument passed to an `immutable-param` parameter. Delete an unused or redundant directive, or an unused ignore |
| Every diagnostic carries its root's fix (#246, #312) | A quick fix works on each diagnostic. A root with an ignored diagnostic gets no fix, so the fix cannot make the ignore unused |
| Same output on every run (#184, #273) | `-json` lists diagnostics in position order |

<details>
<summary>Code that other code changes in place, so it gets no fix</summary>

| Code | Example |
| --- | --- |
| A method of a struct that holds the root | `s := &Svc{db: q}; s.apply()` |
| A deferred change that another deferred call sees | `defer q.Find(&x); defer q.Where("a")` |
| A helper that changes its parameter, when a caller discards the result | `tenant(q); q.Find(&x)` |

</details>

### Performance

The analysis does much more, but takes about as long as 0.22.0. These are wall times with the command, the median of five runs on one machine:

| Code | 0.22.0 | 0.23.0 |
| --- | --- | --- |
| gorm's root module | 1.1 s | 0.9 s |
| gorm's `tests` module | 1.4 s | 1.3 s |
| 400 conditional reassignments in one function | 0.8 s | 0.9 s |
| A chain of 400 `pure` functions | 0.8 s | 0.8 s |
| 800 closures with `//gormreuse:pure` in one function (#301) | 81 s | 0.8 s |

With facts, the drivers also run gormreuse on every dependency. gormreuse builds SSA only for the packages that reach `gorm.io/gorm`, so this adds little. A test fails when the analyzer's allocations on a generated shape, such as the worst cases above, grow faster than the shape allows (#260, #302).

### Integration

| Change | Detail |
| --- | --- |
| `Analyzer.Requires` | It lists `ctrlflow.Analyzer` and `inspect.Analyzer`. In 0.22.0 it listed `buildssa.Analyzer` and `inspect.Analyzer`. gormreuse now builds SSA itself, only for a package that reaches `gorm.io/gorm` |
| `Analyzer.FactTypes` | New. The analyzer exports facts, so a driver that runs it must support facts. The `gormreuse` command, `go vet` and golangci-lint do |
| golangci-lint | It shows only the first issue on each line by default. gormreuse can report more than one on a line. Set `issues.uniq-by-line: false` to see them all. See the [golangci-lint setup](README.md#golangci-lint) |
