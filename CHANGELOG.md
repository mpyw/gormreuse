# Changelog

These notes say what changes for users in each release, and what to do about it. The full commit list of each release is on [GitHub Releases](https://github.com/mpyw/gormreuse/releases). Releases up to 0.22.0 have their notes there only.

## Unreleased (planned as 0.23.0)

gormreuse now follows a `*gorm.DB` through far more code. It also reads directives across packages, and infers `immutable-return` for most connection helpers. `-fix` offers fewer fixes, but each one compiles and keeps every condition.

> [!IMPORTANT]
> **Expect new reports on code that passed with 0.22.0.** Most of them are real reuse. Run `gormreuse ./...` before you bump the version in CI. Then use the tables below to resolve each kind.

### Highlights

| Change | Effect |
| --- | --- |
| More code is followed | Loops, closures that run more than once, `go`/`defer`, method values, method expressions, struct fields, package-level variables and helper results |
| Directives across packages | A directive on an exported function applies in the packages that call it. It travels as an analysis fact, so a broken contract is not trusted there |
| Inferred `immutable-return` | A helper whose body returns only `Session`, `WithContext`, `Debug`, `Begin`, `gorm.Open` or `nil` needs no directive |
| Safer `-fix` | Every fix compiles, alone or with the others. A fix never drops a condition. When it cannot keep every condition, there is no fix |

On gorm's own repository (v1.31.2), the totals barely move. Some reports go and others come:

| Module | 0.22.0 | 0.23.0 | Removed | Added |
| --- | --- | --- | --- | --- |
| Root | 151 | 150 | 6 | 5 |
| `tests` | 15 | 15 | 1 | 1 |

Most added reports in the root module are a `*gorm.DB` parameter branched in a loop. In `tests`, the added report is a `Connection` callback, and the removed one is a Scopes warning on a `WithContext` that the callback does not return.

### New diagnostics

| Now reported | Example | What to do |
| --- | --- | --- |
| A `Connection` callback's `tx` branched twice (#173) | `db.Connection(func(tx *gorm.DB) error { tx.Exec(a); return tx.Raw(b).Scan(&x).Error })` | gorm passes a mid-chain handle there. Start the callback with `tx = tx.Session(&gorm.Session{})` |
| A `Session`/`Debug` copy used after its root changed (#174) | `c := q.Session(&gorm.Session{}); q.Where("a").Find(&x); c.Count(&n)` | End the root's chain with `.Session(&gorm.Session{})` before you copy it |
| `Session(&gorm.Session{Initialized: true})` branched twice (#174) | `s := db.Session(&gorm.Session{Initialized: true}); s.Find(&a); s.Find(&b)` | Its result is a mid-chain handle. Use `Session(&gorm.Session{})` |
| A chain kept in another variable (#175) | `if c { q2 = q.Where("a") }; q.Count(&n)` | `q2` shares `q`'s Statement. Isolate `q` with a Session first |
| The result of a helper, a func value or an interface method (#176) | `db, err := NewDB(); db.Find(&a); db.Find(&b)` | Most helpers are now [inferred](README.md#inferred-immutable-return) safe. Otherwise mark the helper `//gormreuse:immutable-return`. For an interface method, write `repo.DB().Session(&gorm.Session{})` |
| A subquery changed before the outer query runs (#185) | `x := db.Where("id IN (?)", sub); sub.Where("a").Find(&y); x.Find(&z)` | The message is `mutable root passed as a query argument is read when the query runs`. End the subquery's chain with a Session |
| One use that runs more than once (#177, #249, #252) | `for … { q.Find(&x) }`, `f := func() { q.Find(&x) }; f(); f()`, `for … { g.Go(func() error { … }) }` | Define the root inside the loop or closure, or end its chain with a Session |
| A `go`, `defer` or helper call in a loop (#254) | `for … { go q.Find(nil) }`, `for … { helper(q) }` | Same as above. A `go`/`defer` in a loop gets no fix |
| A value that may hold one of several roots (#252) | `q := b; if c { q = a }; q.Find(&x); a.Find(&y)` | Isolate the root that is used again |
| A field of a struct that a helper returns (#181, #250) | `s := newApp(db); s.DB.Find(&a); s.DB.Find(&b)` | Mark the helper `//gormreuse:immutable-return`, or put the Session where the helper builds the struct |
| A package-level variable stored and reused in one function (#183) | `G = db.Where("t"); G.Find(&a); G.Find(&b)` | Store an isolated value |
| Reuse through a method value or a method expression (#178, #258) | `find := q.Find; find(&a); find(&b)` | As for the direct call |
| A named callback that is also called directly or exported (#191) | `db.Transaction(txBody)` and `txBody(q)` | Its parameter is mutable again. Mark the function `//gormreuse:immutable-param`, or stop calling it directly |
| A `pure` function that pollutes its argument in more ways (#187) | `go db.Find(nil)`, `same(db).Find(nil)`, `h.db = db` | Fix the function, or remove `pure` |
| An `immutable-input` callback that escapes (#193) | `forward(cb, db)` | The message is `callback cb escapes (...)`. Pass the callback only to another `immutable-input` function or to `Transaction` |
| A variadic `immutable-input` callback (#259) | `//gormreuse:immutable-input(cbs)` on `cbs ...func(*gorm.DB)` | Reported unused. Delete the directive |
| `immutable-param` caller contract in more calls (#192, #195, #258) | `go apply(q)`, `defer apply(q)`, `pkg.Apply(q)`, `Repo.apply(r, q)` | `-fix` appends `.Session(&gorm.Session{})` to the argument |
| A trailing ignore no longer covers the next line (#189) | `q.Find(nil) //gormreuse:ignore` followed by `q.Count(nil)` | Put the ignore on the line it suppresses |
| A function-level ignore that suppresses nothing (#190) | `//gormreuse:ignore` on a clean function | Reported unused. Delete it |
| Files that import `"C"` (#186) | Any reuse in a cgo file | They are analyzed now. `-fix` does not edit them |
| A panic inside the analyzer (#186) | `gormreuse: internal error analyzing <func>: ...` | Please [open an issue](https://github.com/mpyw/gormreuse/issues) with the code |

> [!TIP]
> Run `gormreuse -fix -diff ./...` to see the fixes for most of these. Some reports have no fix on purpose. See [Automatic Fixes](README.md#automatic-fixes).

### Removed diagnostics

| No longer reported | Example |
| --- | --- |
| A subquery passed to several queries (#185) | `db.Where("id IN (?)", sub).Find(&a); db.Where("id NOT IN (?)", sub).Find(&b)` |
| A Scopes callback that uses `Session`, `WithContext` or `Debug` without returning it (#196) | `db.Scopes(func(q *gorm.DB) *gorm.DB { q.Session(s).Find(&x); return q })` |
| Uses on paths that cannot both run, through a closure (#179) | `if c { f() } else { q.Count(&n) }` |
| A captured parameter rebound before the closure runs (#251) | `db = db.WithContext(ctx); func() { db.Find(&x) }()` |
| A closure that builds a new chain on each call (#251) | `base := func() *gorm.DB { return db.Model(&User{}) }`, called twice |
| The builder idiom on a field (#182) | `r.db = r.db.Where("t"); r.db.Find(&x)` |
| Two calls through a method value of an immutable value (#178) | `where := db.Session(s).Where; where("a").Find(&x); where("b").Find(&y)` |
| A Transaction or FindInBatches callback under `go`/`defer`, or held in a variable or field (#256, #257) | `go db.Transaction(cb)`, `db.Transaction(s.cb)` |
| Directives on generic instances and method values (#180) | `helper[int](q)`, `f := r.helper; f(q)` |

> [!WARNING]
> **A `//gormreuse:ignore` written for a removed report is now unused**, and is reported as such. The Scopes warning (#196) is the most common case. `-fix` deletes it. In a non-test file of a package with `_test.go` files there is no fix, because the test variant may still need it. Delete those by hand.

### Directives

| Change | Detail |
| --- | --- |
| Inferred `immutable-return` (#261) | A function without the directive is treated as `immutable-return` when its body proves it. See [Inferred immutable-return](README.md#inferred-immutable-return). A declared directive on such a function is not reported unused |
| Directives across packages (#195) | `pure`, `immutable-return`, `immutable-param` and `immutable-input(name)` on an exported function apply in other packages. A same-line directive and a generic instance work too. A contract the body breaks is trusted only in its own package |
| Facts need the dependencies | The `gormreuse` command, `go vet` and golangci-lint analyze each dependency first. A function in a generated file exports no fact, so other packages treat it as unmarked (#259) |
| Method expressions (#258) | Every directive applies to `Repo.helper(r, q)` and `(*Repo).helper` |
| Package-level func vars (#183, #257) | Every directive applies to a call through `var f = func(...)` when `f` is unexported, assigned only in its declaration, and its address is never taken |
| `immutable-input` placement (#193) | It works where `pure` does: a doc comment, after `{` on the same line, or the line before a closure |
| One ignore scope (#190) | A line, function or file ignore covers every kind of diagnostic in the same way. A function-level ignore also covers its closures |

### `-fix`

`-fix` now offers fewer fixes. Each one is safe to apply.

| Change | Detail |
| --- | --- |
| Every fix compiles (#200) | Alone, as an editor's quick fix, or with every other fix. The `gorm` import is added under a free name when needed |
| No fix that drops a condition (#199, #243) | A discarded statement such as `q.Where("a")` must be reassigned. When it cannot be, or when other code changes the root in place, there is no fix |
| The right variable (#198) | A reassignment rebinds the variable that holds the reused root, never an outer receiver |
| Parameter roots (#201, #245) | An exported function, a method or a function value gets `tx = tx.Session(&gorm.Session{})` at the top of the body. `immutable-param` is added only when every caller is checked |
| Sessions by control flow (#202, #247, #311) | Sessions go where the paths need them, also for a reassignment read in a later loop and for a value merged with a parameter. One pass of fixes resolves the diagnostics |
| New fixes (#248) | Isolate an argument passed to an `immutable-param` parameter. Delete an unused or redundant directive, or an unused ignore |
| Every diagnostic carries its root's fix (#246) | A quick fix works on each diagnostic, also when another one is ignored |
| Same output on every run (#184, #273) | `-json` lists diagnostics in position order |

### Performance

The analysis does much more, but takes about as long as 0.22.0. These are wall times with the command, on one machine:

| Code | 0.22.0 | 0.23.0 |
| --- | --- | --- |
| gorm's root module | 0.9 s | 0.8 s |
| gorm's `tests` module | 1.2 s | 1.1 s |
| 400 conditional reassignments in one function, and a chain of 400 `pure` functions | 1.0 s | 1.2 s |
| 800 closures with `//gormreuse:pure` in one function (#301) | 82 s | 0.9 s |

With facts, the drivers also run gormreuse on every dependency. gormreuse builds SSA only for the packages that reach `gorm.io/gorm`, so this adds little. A test fails when the analyzer's allocations on a generated shape, such as the worst cases above, grow faster than the shape allows (#260, #302).
