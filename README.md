# gormreuse

[![Go Reference](https://pkg.go.dev/badge/github.com/mpyw/gormreuse.svg)](https://pkg.go.dev/github.com/mpyw/gormreuse)
[![CI](https://github.com/mpyw/gormreuse/actions/workflows/ci.yml/badge.svg)](https://github.com/mpyw/gormreuse/actions/workflows/ci.yml)
[![Codecov](https://codecov.io/gh/mpyw/gormreuse/graph/badge.svg)](https://codecov.io/gh/mpyw/gormreuse)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

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

### <a href="https://mise.jdx.dev/"><img src="https://mise.jdx.dev/logo.svg" height="28" alt=""></a> Using [mise](https://mise.jdx.dev/) (macOS/Linux/Windows)

**Recommended.** mise's `github` backend installs gormreuse directly from GitHub Releases. It needs no extra registry. It needs no Go toolchain either, because the binaries are prebuilt:

```bash
mise use "github:mpyw/gormreuse@0.22.0"
gormreuse ./...
```

Run it in the project root. It pins the version in the project's `mise.toml`, so every checkout and CI run the same one:

```toml
[tools]
"github:mpyw/gormreuse" = "0.22.0"
```

Add `-g` to install it for every project on your machine instead.

> [!IMPORTANT]
> Building gormreuse from source requires Go 1.27 or later.

### Using [`go tool`](https://pkg.go.dev/cmd/go#hdr-Run_specified_go_tool)

```bash
# Add to go.mod as a tool dependency
go get -tool github.com/mpyw/gormreuse/cmd/gormreuse@latest

# Run via go tool
go tool gormreuse ./...
```

> [!NOTE]
> gormreuse's `go.mod` says `go 1.27.0`. So `go get -tool` raises the `go` line of your `go.mod` to `1.27.0` when it is lower. Use another install method if your module must stay on an older Go.

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

### Using [`go run`](https://pkg.go.dev/cmd/go#hdr-Compile_and_run_Go_program)

```bash
go run github.com/mpyw/gormreuse/cmd/gormreuse@latest ./...
```

> [!CAUTION]
> To prevent supply chain attacks, pin to a specific version tag instead of `@latest` in CI/CD pipelines (e.g., `@v0.22.0`).

<details>
<summary><a href="https://curl.se/"><img src="https://cdn.simpleicons.org/curl" height="20" alt=""></a> Downloading the tarball directly (macOS/Linux/Windows)</summary>

No package manager? Grab the archive for your platform from [GitHub Releases](https://github.com/mpyw/gormreuse/releases):

```bash
export VERSION=0.22.0
export OS=linux    # or darwin
export ARCH=amd64  # or arm64
export BASE_URL="https://github.com/mpyw/gormreuse/releases/download/v${VERSION}"

# Download the archive and the release's checksum list
curl -LO "${BASE_URL}/gormreuse_${VERSION}_${OS}_${ARCH}.tar.gz"
curl -LO "${BASE_URL}/checksums.txt"

# Verify before installing. checksums.txt lists every platform's archive,
# so skip the ones you did not download.
sha256sum --ignore-missing -c checksums.txt               # Linux
# shasum -a 256 --ignore-missing -c checksums.txt         # macOS

tar xzf "gormreuse_${VERSION}_${OS}_${ARCH}.tar.gz"
sudo mv gormreuse /usr/local/bin/
```

On Windows, download `gormreuse_${VERSION}_windows_${ARCH}.zip` and extract `gormreuse.exe` somewhere on your `PATH`.

</details>

<details>
<summary><a href="https://golangci-lint.run/"><img src="https://cdn.simpleicons.org/go" height="20" alt=""></a> Running inside golangci-lint (custom build)</summary>

gormreuse is not bundled with golangci-lint, but it ships a [module plugin](https://golangci-lint.run/plugins/module-plugins/), so you can build a golangci-lint binary that holds it. Write `.custom-gcl.yml`:

```yaml
version: v2.13.1  # the golangci-lint release to build
plugins:
  - module: github.com/mpyw/gormreuse
    import: github.com/mpyw/gormreuse/plugin
    version: v0.22.0
```

Turn it on in `.golangci.yml`. gormreuse has no settings, and any key under `settings` is an error:

```yaml
version: "2"
linters:
  enable:
    - gormreuse
  settings:
    custom:
      gormreuse:
        type: module
        description: Detects unsafe *gorm.DB reuse.
```

Then build and run it:

```bash
golangci-lint custom  # writes ./custom-gcl
./custom-gcl run ./...
```

Use golangci-lint's own options in place of the driver flags below: `run.tests: false` for `-test=false`, and `--fix` for `-fix`.

</details>

## Flags

gormreuse has no flags of its own. These come from the analysis driver:

| Flag | Default | Description |
|------|---------|-------------|
| `-test` | `true` | Analyze test files (`*_test.go`) |
| `-fix` | `false` | Apply the suggested fixes to the files |
| `-diff` | `false` | With `-fix`, print the fixes as a unified diff instead of writing them |
| `-json` | `false` | Print the diagnostics as JSON |
| `-c=N` | `-1` | Print each reported line with `N` lines of context |
| `-V=full` | | Print the version and exit (`go vet` uses this) |
| `-flags` | | Print the flags as JSON and exit |

The command exits with status 3 when it reports a diagnostic, and with 0 when it reports none. With `-json` it exits with 0 either way.

Generated files (containing `// Code generated ... DO NOT EDIT.`) are always excluded and cannot be opted in. Files that import `"C"` are analyzed: the header that cgo adds to its output does not count. `-fix` does not edit them, because the driver treats cgo's output as generated.

If gormreuse fails inside a function, it reports `gormreuse: internal error analyzing <func>: ...` at that function. Please [open an issue](https://github.com/mpyw/gormreuse/issues) with the code. Set `GORMREUSE_DEBUG_PANIC=1` to get the stack trace instead.

### Examples

```bash
# Exclude test files from analysis
gormreuse -test=false ./...

# Preview the fixes without writing them
gormreuse -fix -diff ./...

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
q.Where("a")           // first branch (non-finisher): changes q in place
q.Where("b").Find(nil) // VIOLATION: second branch

// After -fix
q := db.Where("base")
q = q.Where("a")       // ← Reassignment creates new root
q.Where("b").Find(nil) // OK: first branch from new root
```

**When applied:**
- Non-finisher methods (chain builders like `Where`, `Order`, `Limit`) whose result is a `*gorm.DB`. Calls that return something else are not reassigned: `AddError`, `DB()`, `Association`, `Migrator`, `AutoMigrate`. Neither are calls that run SQL or end a transaction: `FindInBatches`, `CreateInBatches`, `UpdateColumn(s)`, `Commit`, `Rollback`, `SavePoint`, `RollbackTo`
- Result is not assigned to a variable
- Expression used as a statement. A chain such as `q.Where("a").Limit(10)` is reassigned as a whole, and a Session it needs goes after its last call
- The receiver is the local variable that holds the reused root. These are not rewritten: aliases (`r := q`), calls that take the root as an argument (`base.Model(q)`), and package-level variables. Neither are variables captured by a closure that is not called inline (`go func() { ... }()`, `defer func() { ... }()`). The fix relies on a Session at the root instead, unless that would drop a discarded statement or another in-place change (see below)
- A reassignment inside a loop always gets a Session, since the next iteration uses it again

> [!IMPORTANT]
> A discarded statement such as `q.Where("a")` changes `q` in place, and later queries see `"a"`. A Session at the root alone would make it change a copy, so the fixed code would silently drop `"a"`. So when such a statement cannot be reassigned, the diagnostic has **no fix**. Fix these by hand:
>
> - an alias receiver
> - a method value (`where := q.Where`)
> - a variable captured by a closure
> - a chain with a Session-family call before its last call (`q.Where("a").Session(s).Limit(1)`)
>
> Other code can change `q` in place too, and a Session at the root would drop that change as well. So the diagnostic also has **no fix** when:
>
> - `q` is handed to other code before a later use of `q`, or in a loop: an argument of a function that is not `//gormreuse:pure` (`applyTenant(q)`, `apply := func(d *gorm.DB) { ... }; apply(q)`, `_ = withTenant(q)`, `base.Model(q)`), or a store into a slice, map or channel. A function other than a `*gorm.DB` method that returns a `*gorm.DB` may return `q` itself, so its argument counts even as the last use (`q = withTenant(q)`), and so does an argument of a `go` statement. A subquery (`db.Where("id IN (?)", q)`) only reads `q`, and does not count
> - the `Error`, `RowsAffected` or `Statement` field is read from `q` (`return q.Error`), or from a chain on `q` that runs no query (`if err := q.Where("a").Error; ...`). Each query adds its error to `q.Error`, and that chain changes `q` the same way a discarded statement does. Reading the field of a query's own result (`q.Where("a").Find(&x).Error`) is fine

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
- A root, or a value that a reassignment creates, still has 2+ branches on one path after the reassignments. The paths come from the control flow, so in `if c { q.Where("a") }; q.Where("b").Find(nil); q.Where("c").Find(nil)` both the root and `q = q.Where("a")` get a Session. A reassigned value used once gets none, and neither does a chain that already ends in a Session
- Sometimes the fix cannot see every use of the root in order. Examples are a `defer`/`go` use, a use inside a closure, and a read through a `Session` copy. Then every root or reassigned value with a later use gets a Session
- Special handling for Phi nodes (conditional branches): adds Session to each incoming edge that is still mutable. An edge counts as immutable by the same rule as detection: its **last** call must return an immutable value. `db.Session(&gorm.Session{}).Where("y")` and `db.WithContext(ctx).Where("y")` are mutable, so they get a Session too
- Session goes only after a call that returns `*gorm.DB`. An edge that has no such call gets none. Examples are a closure parameter (`if c { q = tx }`) and one result of `q, err := open()`

`Session` is written with a name that refers to `gorm.io/gorm` where it is inserted. When the file has no such name, the fix adds an import: `"gorm.io/gorm"`, or `gormio "gorm.io/gorm"` when `gorm` is taken there, such as by a parameter or local variable named `gorm`, or by another package imported as `gorm`. A blank import (`_ "gorm.io/gorm"`) gives no name. Each diagnostic's fix carries its own import edit, so an editor can apply one quick fix alone.

### Parameter Roots

A reused `*gorm.DB` parameter has no definition to append a Session to. The fix depends on whether gormreuse can check every caller:

| Function | Fix |
| --- | --- |
| Unexported, not a method, only called directly in the package, and every caller passes an immutable value | Add [`//gormreuse:immutable-param`](#gormreuseimmutable-param). The caller-side check keeps the callers isolated |
| Any other function: exported, a method, used as a function value, or with a caller that passes a mid-chain value | Rebind the parameter at the top of the body: `tx = tx.Session(&gorm.Session{})`. Discarded statements on it are reassigned as for a local root |
| A Scopes/Preload callback, a function that returns a `*gorm.DB` and is not only called directly (it may be a Scopes callback), or a closure | **No fix** |
| A function with a discarded statement on the parameter (`tx.Where("a")`) and no `*gorm.DB` result | **No fix**. The statement also changes the caller's value, and callers such as `applyTenant(q); q.Find(&users)` rely on that. With `tx = tx.Where("a")` the change would stay inside the function |

`immutable-param` on an exported function would hide the reuse: callers in other packages are never checked. On a function with a mid-chain caller, it would move the report to that caller, where it has no fix.

### Combined Example

Both strategies work together for complex cases:

```go
// Before
q := db.Where("base")
q.Where("a")           // first branch
q.Where("b").Find(nil) // VIOLATION
q.Where("c")           // VIOLATION
q.Where("d").Find(nil) // VIOLATION

// After -fix (both strategies applied)
q := db.Where("base")
q = q.Where("a").Session(&gorm.Session{})  // Reassignment + Session: used by b and c
q.Where("b").Find(nil)
q = q.Where("c")                            // Reassignment: used once, by d
q.Where("d").Find(nil)
```

> [!TIP]
> Run `gormreuse -fix -diff ./...` to review the fixes, then `gormreuse -fix ./...` to apply them. Some diagnostics have no fix (see the IMPORTANT note above and [Parameter Roots](#parameter-roots)). Fix those by hand.

When several roots are reused at the same place, such as `use(q, r)`, that place gets one diagnostic. Its message names the root that appears first in the source, and its fix covers every root reused there. The output is the same on every run.

## Detection Model: Mutable Branching

This linter detects when a **mutable [`*gorm.DB`](https://pkg.go.dev/gorm.io/gorm#DB) branches into multiple code paths**. The core concept:

1. **Immutable-returning methods** ([`Session`](https://pkg.go.dev/gorm.io/gorm#DB.Session), [`WithContext`](https://pkg.go.dev/gorm.io/gorm#DB.WithContext), [`Debug`](https://pkg.go.dev/gorm.io/gorm#DB.Debug), etc.) return an **immutable** instance that can branch freely
2. **All other methods** on a mutable instance create a **branch** that consumes the instance
3. **Second branch** from the same mutable root is a **violation**
4. A **`Session`/`Debug` copy** of a mutable instance still reads that instance's `Statement`. So using the copy **after the instance changed** is a **violation** (see [Session and Debug copies](#session-and-debug-copies))
5. A mutable instance passed as a **subquery or condition argument** (`db.Where("id IN (?)", sub)`) is **read**, not branched. Running the outer query **after the argument changed** is a **violation** (see [Subqueries and condition arguments](#subqueries-and-condition-arguments))

### Method Classification

> [!IMPORTANT]
> This linter analyzes the **Traditional API** ([`*gorm.DB`](https://pkg.go.dev/gorm.io/gorm#DB)) only. The Generics API ([`gorm.G[T]`](https://pkg.go.dev/gorm.io/gorm#G), available since v1.30.0) returns different types ([`Interface[T]`](https://pkg.go.dev/gorm.io/gorm#Interface), [`ChainInterface[T]`](https://pkg.go.dev/gorm.io/gorm#ChainInterface)) and is automatically excluded from analysis.

| Category                    | Methods                  | Description                    |
| --------------------------- | ------------------------ | ------------------------------ |
| Immutable-Returning Methods | [`Session`](https://pkg.go.dev/gorm.io/gorm#DB.Session), [`WithContext`](https://pkg.go.dev/gorm.io/gorm#DB.WithContext), [`Debug`](https://pkg.go.dev/gorm.io/gorm#DB.Debug), [`Open`](https://pkg.go.dev/gorm.io/gorm#Open), [`Begin`](https://pkg.go.dev/gorm.io/gorm#DB.Begin) | Return a new immutable instance (`clone > 0`). They read the receiver but do not branch it. `Session` and `Debug` have exceptions: see [Session and Debug copies](#session-and-debug-copies) |
| Transaction                 | [`Transaction`](https://pkg.go.dev/gorm.io/gorm#DB.Transaction) | Returns an `error`. Like `Begin`, it reads the receiver but does not branch it. Its callback gets a fresh `tx` (see [Parameters are mutable](#parameters-are-mutable)) |
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

A `*gorm.DB` passed as a subquery or condition argument of a query method is a read, not one of these. See [Subqueries and condition arguments](#subqueries-and-condition-arguments).

A struct field is followed in these forms:

- A field of a struct literal or variable, including nested fields and fields reached through embedding.
- Methods promoted from an embedded `*gorm.DB`: `e := E{db.Where("x")}; e.Where("a")`.
- A field of a struct that a function returned, by value or by pointer: `s := mk(db); s.DB.Where("a")`. Each field of each call is its own mutable root, like a function that returns `*gorm.DB`. A call inside a loop makes a new root on each iteration. A store to the field through the returned pointer replaces it, as for a local struct. No fix is suggested for it, because the `Session` belongs inside the function. Mark a helper that returns a struct of safe handles, such as a test setup function, with `//gormreuse:immutable-return`.
- A struct captured by a closure, also through a pointer.

A field read through a parameter or a receiver, by value or by pointer, is not a mutable root. Only the stores to it in the same function are followed.

Note: Simple struct literal storage (`_ = &S{db: q}`) without actual field usage does NOT pollute.

A field read sees only the stores that can run before it. So the builder idiom `r.db = r.db.Where(...)` in a pointer-receiver method, followed by one use of `r.db`, is not reported. A second use of `r.db` is.

### Examples

In these examples, `db` is the value that `gorm.Open` returned, which is immutable. A `*gorm.DB` parameter is mutable (see [Parameters are mutable](#parameters-are-mutable)). So some of the safe examples are reported when `db` is a parameter.

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

// A call through a method value is the same as the direct call
where := db.Where("base").Session(&gorm.Session{}).Where
where("a").Find(&users1)    // OK - same as q.Where("a")
where("b").Find(&users2)    // OK - same as q.Where("b")
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

// A method value of a mutable value branches it on each call
q := db.Where("x")
where := q.Where
where("a").Find(&users1) // first branch from q - OK
where("b").Find(&users2) // VIOLATION: second branch from q

// Session in middle doesn't help - result is still mutable
q := db.Session(&gorm.Session{}).Where("x")  // q is mutable!
q.Find(&users)   // first branch - OK
q.Count(&count)  // VIOLATION: second branch

// Using immutable-returning method on polluted value is also a violation
q := db.Where("x")
q.Find(&users)                       // first branch - OK
q.Session(&gorm.Session{}).Count(&c) // VIOLATION: second branch from q

// A loop body runs more than once, so one use inside it is reuse.
// Range-over-func loops (Go 1.23+) count as loops too.
q := db.Where("x")
for v := range slices.Values(ids) {
    q.Where("id = ?", v).Find(&users) // VIOLATION: reused on every iteration
}
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
db, err := gorm.Open(dialector) // immutable: safe to branch more than once

q := db.Where("x")
q.Find(&users)        // first branch from original q - OK

q = db.Where("y")     // reassignment creates NEW mutable root
q.Find(&admins)       // first branch from new q - OK

q = db.Where("z")     // another reassignment
q.Count(&count)       // first branch from newest q - OK
```

The linter uses [SSA (Static Single Assignment)](https://pkg.go.dev/golang.org/x/tools/go/ssa) form, where each assignment creates a distinct value. So the new value has no relationship to the previous one.

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

Each example branches `db` more than once, which is safe only because `db` is immutable. If `db` were a `*gorm.DB` parameter, the second branch of `db` would be reported. A reassignment that extends the same variable, as in `if condition { q = q.Where("alt") }`, is not a second branch of `q`.

A chain assigned to a **different** variable is a branch, even under a condition or when a closure captures it:

```go
q := db.Where("x")
var q2 *gorm.DB
if condition {
    q2 = q.Where("a")      // first branch from q
}
q.Count(&count)            // VIOLATION: second branch from q
```

#### Session and Debug copies

On a mutable value, [`Session(&gorm.Session{})`](https://pkg.go.dev/gorm.io/gorm#DB.Session) and [`Debug()`](https://pkg.go.dev/gorm.io/gorm#DB.Debug) return a copy that **points at the same `Statement`**. Using the copy does not change the original. But a change to the original after the copy was taken shows up in every later use of the copy:

```go
q := db.Model(&User{}).Where("tenant = 1") // q is mutable
countQ := q.Session(&gorm.Session{})       // countQ shares q's Statement
countQ.Count(&before)                      // OK - q has not changed yet
q.Where("active = 1").Find(&users)         // first branch - changes q in place
countQ.Count(&n)                           // VIOLATION: runs WHERE tenant = 1 AND active = 1
```

A change by reassignment (`q = q.Where(...)`) counts too, since it changes a mutable `Statement` in place. To fix it, end the root's chain with `.Session(&gorm.Session{})` before you copy it, or take the copy with an option that clones the `Statement`.

| Call on a mutable value | Shares the `Statement`? | Result |
| --- | --- | --- |
| `Session(&gorm.Session{})`, `Debug()` | Yes | Immutable |
| `Session` with only other options (`DryRun`, `QueryFields`, `Logger`, …) | Yes | Immutable |
| [`WithContext(ctx)`](https://pkg.go.dev/gorm.io/gorm#DB.WithContext), `Session` with `Context`, `PrepareStmt`, `SkipHooks` or `NewDB` | No | Immutable |
| `Session(&gorm.Session{Initialized: true})` | No (it takes a snapshot) | **Mutable** (`clone == 0`), even on `db` |
| `Session(cfg)` where `cfg` is not a `&gorm.Session{...}` literal | Assumed yes | Assumed **mutable** |

The linter reads the `&gorm.Session{...}` literal at the call. It treats an option set to a non-constant value, or set in only some branches, as unknown and assumes the worst case. A `Context` set to anything other than `nil` counts as set, as for `WithContext(ctx)`.

#### Subqueries and condition arguments

GORM builds a subquery from a copy of the argument's `Statement`, so passing a `*gorm.DB` as an argument never changes it. The linter treats such an argument as a **read** of its root. You can pass one subquery to several queries:

```go
sub := db.Model(&User{}).Select("id") // sub is mutable
db.Where("id IN (?)", sub).Find(&users)     // OK - reads sub
db.Where("id NOT IN (?)", sub).Find(&others) // OK - reads sub again
sub.Find(&ids)                               // OK - first branch of sub
```

This holds for these methods and parameters. A `*gorm.DB` passed anywhere else, such as `Model(sub)` or a `Find` destination, is still a branch.

| Methods | Read-only parameters |
| --- | --- |
| `Where`, `Or`, `Not`, `Having`, `Joins`, `InnerJoins`, `Table`, `Select`, `Distinct`, `Raw`, `Exec` | Every parameter, including group conditions (`db.Where(sub)`) and `Joins("Company", cond)` |
| `Find`, `First`, `Last`, `Take`, `Delete` | The conditions after the first parameter |
| `Update`, `UpdateColumn` | The value |

GORM reads the argument when the **outer query runs**, not when you pass it. So a change to the argument before that, by a branch or by reassignment, shows up in the outer query:

```go
sub := db.Model(&User{}).Select("id")
sub.Where("age > 18").Find(&adults)    // first branch - changes sub in place
db.Where("id IN (?)", sub).Find(&users) // VIOLATION: the subquery has age > 18

sub := db.Model(&User{}).Select("id")
x := db.Where("id IN (?)", sub)
sub.Where("age > 18").Find(&adults)    // changes sub in place
x.Find(&users)                          // VIOLATION: the subquery has age > 18
```

To fix it, end the argument's chain with `.Session(&gorm.Session{})` before you pass it. The second kind of violation is reported once per call that takes the argument. It is reported at the first use of the outer chain after the change, and carries no suggested fix.

#### Scopes / Preload callbacks

The [`*gorm.DB`](https://pkg.go.dev/gorm.io/gorm#DB) passed to a [`Scopes`](https://pkg.go.dev/gorm.io/gorm#DB.Scopes) / [`Preload`](https://pkg.go.dev/gorm.io/gorm#DB.Preload) callback is a **mid-chain (mutable) value**. So reusing it inside the callback is a violation, as for any other mutable value:

```go
db.Scopes(func(tx *gorm.DB) *gorm.DB {
    tx.Where("a").Find(&users) // first branch from tx - OK
    return tx.Where("b")       // VIOLATION: second branch from tx
})
```

#### Parameters are mutable

A [`*gorm.DB`](https://pkg.go.dev/gorm.io/gorm#DB) **parameter** is a mutable root: a caller may pass a mid-chain value, so branching a parameter interferes exactly as branching a local would.

```go
func helper(tx *gorm.DB) *gorm.DB {
    tx.Where("a").Find(&users) // first branch from tx - OK
    return tx.Where("b")       // VIOLATION: second branch from tx
}
```

To opt a helper out, annotate it with [`//gormreuse:immutable-param`](#gormreuseimmutable-param). The caller is then responsible for passing an isolated value.

GORM's internal `clone` field decides whether reuse is safe. A handle with `clone > 0` forks a **fresh** `Statement` on the next chain call, so it is safe to reuse. A mid-chain handle (`clone == 0`) shares its `Statement`. A parameter's `clone` value cannot be known statically, so it is treated as mutable.

| Source | `clone` | Reusable? |
| --- | --- | --- |
| [`gorm.Open`](https://pkg.go.dev/gorm.io/gorm#Open), [`Session`](https://pkg.go.dev/gorm.io/gorm#DB.Session), [`WithContext`](https://pkg.go.dev/gorm.io/gorm#DB.WithContext), [`Debug`](https://pkg.go.dev/gorm.io/gorm#DB.Debug) | `> 0` | ✅ forks a fresh Statement (but see [Session and Debug copies](#session-and-debug-copies)) |
| `Session(&gorm.Session{Initialized: true})` | `== 0` | ❌ shares Statement |
| [`Begin`](https://pkg.go.dev/gorm.io/gorm#DB.Begin) result, [`Transaction`](https://pkg.go.dev/gorm.io/gorm#DB.Transaction) callback `tx` | `> 0` | ✅ fresh transaction handle |
| [`FindInBatches`](https://pkg.go.dev/gorm.io/gorm#DB.FindInBatches) callback `tx` | `> 0` | ✅ fresh handle per batch |
| Mid-chain ([`Where`](https://pkg.go.dev/gorm.io/gorm#DB.Where), [`Order`](https://pkg.go.dev/gorm.io/gorm#DB.Order), …) | `== 0` | ❌ shares Statement |
| [`Connection`](https://pkg.go.dev/gorm.io/gorm#DB.Connection) callback `tx` | `== 0` | ❌ shares Statement |
| A [`*gorm.DB`](https://pkg.go.dev/gorm.io/gorm#DB) **parameter** | unknown | ❌ treated as mutable |
| The result of a helper, a func-typed value (`f(db)`) or an interface method (`repo.DB()`), alone or in a tuple such as `(*gorm.DB, error)` | unknown | ❌ treated as mutable, unless the helper is [`//gormreuse:immutable-return`](#gormreuseimmutable-return) |

A repository interface whose method returns a fresh handle is reported when its result is branched twice. No directive applies to an interface method. Isolate the result with `repo.DB().Session(&gorm.Session{})`.

Transaction and FindInBatches callbacks (e.g. `tx` in `db.Transaction(func(tx *gorm.DB) error {...})`) are exempt, also when the call runs under `go` or `defer`. Their `tx` is a fresh handle. A Connection callback is **not** exempt: gorm passes it a `clone == 0` handle, so branching its `tx` twice is reported. Declare your own such helpers with [`//gormreuse:immutable-input(name)`](#gormreuseimmutable-inputname).

> [!NOTE]
> A **named** function or method is exempt only when every use of it in the package passes it as such a callback. Otherwise its parameter is treated as mutable like any other parameter. That is the case when it is also called directly, stored, returned, passed as an ordinary argument, or exported (another package could call it). The packages that import an exported function are analyzed after its own package, so its own package cannot see their calls. To keep it exempt, mark it [`//gormreuse:immutable-param`](#gormreuseimmutable-param): then each direct caller, in its package or in another one, must pass an isolated value.

#### Closures

A use inside a closure counts where the closure is called. When the closure has one plain call site, or is called in place (`func() { ... }()`), uses on paths that cannot both run are not reported:

```go
q := db.Where("x")
f := func() { q.Find(&users) }
if condition {
    f()               // first branch from q - OK
} else {
    q.Count(&count)   // OK - f() never runs on this path
}
```

A closure called from more than one place, deferred, run with `go`, passed to a function, or stored, may run at any time. A use inside it conflicts with every other use of the same root.

#### Package-level variables

A package-level `*gorm.DB` follows the stores to it in the same function:

```go
var Global *gorm.DB

func F(db *gorm.DB) {
    Global = db.Session(&gorm.Session{}).Where("tenant")
    Global.Where("a").Find(&users) // first branch - OK
    Global.Where("b").Find(&users) // VIOLATION: second branch from the stored chain
}
```

A global that the function only reads is treated as immutable, because it usually holds a `gorm.Open` result. So a mutable chain assigned in another function, or in the variable's initializer (`var q = db.Where("x")`), is not reported when it is reused.

The body of a package-level closure (`var f = func(db *gorm.DB) { ... }`) is checked like any other closure, and the directives on it are validated. A call through `f` honours those directives when `f` is unexported, assigned only in its declaration, and its address is never taken.

## Directives

Only `//gormreuse:name[,name...]` is a directive: a line comment, lowercase names, and no space anywhere in it, including after a comma. A reason may follow after `//`, as in `//gormreuse:ignore // reason here`. `//` is the only reason separator.

The names are `ignore`, `pure`, `immutable-return`, `immutable-param` and `immutable-input(name)`.

A directive on a generic function or method applies to every instance of it (`helper[int](q)`). A directive on a method also applies when the method is called through a method value (`f := r.helper; f(q)`).

A directive on an exported function or method also applies in the packages that call it. The analysis of the declaring package checks the function's body. It passes the result on as an [analysis fact](https://pkg.go.dev/golang.org/x/tools/go/analysis#hdr-Modular_analysis_with_Facts), so the packages that call it do not read its source. This works with the `gormreuse` command, `go vet` and golangci-lint, which all analyze the dependencies first.

| Directive in the declaring package | In a package that calls the function |
| --- | --- |
| `pure` | Trusted, unless the body pollutes its argument. Which arguments the result may be is known too |
| `immutable-return` | Trusted, unless the body returns a mutable value |
| `immutable-param` | The caller must pass an isolated value, when the function branches the parameter |
| `immutable-input(name)` | The callback gets an immutable value, unless the body hands it a mutable one |

A contract that the body breaks is reported in the declaring package. Its callers in that package still trust it, so the mistake is reported once. A caller in another package does not trust it, since the author of that package may never see the report.

> [!WARNING]
> Any other comment that starts with `gormreuse:` has no effect, and is reported with how to write it. One unknown name in a list drops the whole comment: `//gormreuse:pure,bogus` does not mark the function pure.

### `//gormreuse:ignore`

Suppress warnings for a specific line:

```go
q := db.Where("active = ?", true)
q.Find(&users)
//gormreuse:ignore // intentional reuse for pagination
q.Count(&count)  // Suppressed
```

A comment on its own line covers that line and the next one. A comment after code on the same line covers that line only:

```go
q.Count(&count) //gormreuse:ignore // Suppressed
q.Delete(&User{})                  // Still reported
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

The same scope applies to every diagnostic gormreuse reports: reuse violations, `pure` / `immutable-*` contract violations, redundant or unused directives, and the Scopes `Session()` warning.

| Ignore | Covers |
| --- | --- |
| Line | Every line of the statement it is on or directly above. For `if`, `for`, `switch` and `select`, only the header up to `{`. An ignore inside a closure covers the closure's statement only |
| Function | The whole declaration, its doc comment, and every closure inside it |
| File | The whole file |

A multi-line statement is covered from any of its lines:

```go
//gormreuse:ignore
q.
    Where("y").
    Find(nil) // Suppressed
```

> [!NOTE]
> An ignore only hides reports. gormreuse still checks the function. A `//gormreuse:pure` function that leaks its argument is still not trusted at its call sites, even when the leak report is ignored.

> [!WARNING]
> Unused `//gormreuse:ignore` directives are reported as warnings for line-level and function-level ignores. A function-level ignore is unused when the function has nothing to suppress. This helps keep the codebase clean by identifying stale ignore comments. File-level ignores do not trigger unused warnings. No ignore suppresses an unused-ignore warning or a `gormreuse: internal error` report.

### `//gormreuse:pure`

Mark a function or closure as not polluting its [`*gorm.DB`](https://pkg.go.dev/gorm.io/gorm#DB) argument:

```go
//gormreuse:pure
func withTenant(db *gorm.DB, tenantID int) *gorm.DB {
    return db.Session(&gorm.Session{}).Where("tenant_id = ?", tenantID)
}

// Also works on closures (next-line or same-line pattern):
//gormreuse:pure
helper := func(q *gorm.DB) { _ = q }

helper2 := func(q *gorm.DB) { //gormreuse:pure
    _ = q
}
```

> [!TIP]
> Without a directive, a call to your own function is treated like this:
>
> | Call | Effect on the `*gorm.DB` argument `q` | Result |
> | --- | --- | --- |
> | `helper(q)`, result discarded or not a `*gorm.DB` | A branch of `q` | |
> | `q = helper(q)`, result assigned back to `q` | Like `q = q.Where(...)`, also under a condition or in a loop | A new mutable root |
> | `r := helper(q)` | A branch of `q` | A new mutable root |
>
> Add `//gormreuse:pure` to a helper that does not change its argument, so that passing it does not branch it.

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
> - NOT call polluting methods ([`Where`](https://pkg.go.dev/gorm.io/gorm#DB.Where), [`Find`](https://pkg.go.dev/gorm.io/gorm#DB.Find), etc.) on [`*gorm.DB`](https://pkg.go.dev/gorm.io/gorm#DB) **arguments**. This includes `go db.Find(nil)`, `defer db.Find(nil)`, a method value (`f := db.Find`), and a closure the function creates (`func() { db.Find(nil) }()`). It also includes an argument reached through a pointer, slice, variadic or struct parameter (`(*pp).Find`, `dbs[0].Find`, `h.db.Find`). So does an argument returned from another pure function that returns its argument (`same(db).Find`, see the note below)
> - NOT let an argument escape: channel send, slice/array or map store, a store to a package-level variable, or a store through a pointer (`h.db = db`)
> - NOT pass an argument to a function that is not pure, or to a pure function that fails this contract itself
> - MAY call polluting methods on **immutable values** (e.g., `db.Session(&gorm.Session{}).Where(...)` is OK)
> - MAY pass an argument to `fmt`, `log` and `testing` output functions
>
> Some pure functions are not trusted as pure at their call sites, so reuse after the call is reported too. These are functions that pollute or leak their argument, or pass it to a failing pure function. Passing the argument to a function that is not marked pure is reported inside the pure function only. A method's receiver is not an argument, so `r.db.Where(...)` inside a pure method is allowed.
>
> **Note**: Pure functions MAY return mutable [`*gorm.DB`](https://pkg.go.dev/gorm.io/gorm#DB). Callers must treat the return value as potentially mutable:
> ```go
> q := withTenant(db, 1)  // q is mutable!
> q.Find(&users)          // first branch - OK
> q.Count(&count)         // VIOLATION - second branch from mutable q
> ```
>
> A pure function MAY also return its argument. Its result is then the same value as the argument, so a branch from the result is a branch from the argument:
> ```go
> //gormreuse:pure
> func same(db *gorm.DB) *gorm.DB { return db }
>
> q := db.Where("x")
> same(q).Find(&users)    // first branch from q - OK
> q.Count(&count)         // VIOLATION - second branch from q
> ```
> A result that comes only from immutable values, like `withTenant`'s `db.Session(...).Where(...)`, is a new chain and does not share the argument. This is known only for a pure function in the same package.

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
>
> It also covers a [`*gorm.DB`](https://pkg.go.dev/gorm.io/gorm#DB) in a multi-value result, such as `func Open() (*gorm.DB, error)`. The body contract applies to that result.

> [!WARNING]
> **Body contract.** If the function actually returns a *mutable* value, the directive is reported at the declaration. Otherwise the linter would trust it, and silently allow unsafe reuse of the return value at every call site:
> ```go
> //gormreuse:immutable-return // VIOLATION: returns mutable *gorm.DB
> func GetDB() *gorm.DB {
>     return globalDB.Session(&gorm.Session{}).Where("x") // trailing Where re-forks a fresh clone==0 Statement
> }
> ```
> Put [`Session`](https://pkg.go.dev/gorm.io/gorm#DB.Session) at the **end** of the chain (`globalDB.Where("x").Session(&gorm.Session{})`) so the returned value is isolated. Only provably-mutable returns are reported: a value whose root is a gorm chain method like `Where`, or `Session(&gorm.Session{Initialized: true})`. The check follows IIFEs, local closures, and values that a deferred closure writes to a named result. It also follows unexported package-level variables whose every assignment in the package is mutable. These get the benefit of the doubt: a bare parameter, a value from an unmarked named helper, and a `Session` config the linter can't read. So does an exported or address-taken package-level variable.

### `//gormreuse:immutable-param`

Opt a function's [`*gorm.DB`](https://pkg.go.dev/gorm.io/gorm#DB) **parameters** out of the default mutable treatment. They are treated as immutable inside the function, so the caller becomes responsible for passing an isolated value:

```go
//gormreuse:immutable-param
func applyFilters(tx *gorm.DB) {
    tx.Where("a").Find(&users) // OK - tx is treated as immutable here
    tx.Where("b").Find(&more)  // OK
}
```

> [!WARNING]
> **Caller-side contract.** When the function actually branches such a parameter, passing a *mutable* [`*gorm.DB`](https://pkg.go.dev/gorm.io/gorm#DB) at a call site is reported. The callee's internal branching would interfere, because the value is not isolated:
> ```go
> func caller(root *gorm.DB) {
>     q := root.Session(&gorm.Session{}).Where("x") // mutable
>     applyFilters(q) // VIOLATION: mutable *gorm.DB passed to immutable-param parameter
> }
> ```
> Fix by passing an isolated value (`root.Session(&gorm.Session{})`), or make the caller `//gormreuse:immutable-param` too so the contract propagates.
>
> The check covers direct calls, function and method values, generic instances, and the same calls under `go` and `defer`, also when the function is in another package. It cannot cover a call through an interface or a func-typed parameter, where the callee is unknown.

> [!TIP]
> Combine with `//gormreuse:pure` when a helper both leaves its argument unpolluted and expects an isolated value. A **redundant** `//gormreuse:immutable-param` is reported so you can drop it. That is one whose parameter is never reused, so it suppresses nothing.

### `//gormreuse:immutable-input(name)`

Declare that the function passes an **immutable** [`*gorm.DB`](https://pkg.go.dev/gorm.io/gorm#DB) to its callback parameter `name`. It is a user-defined equivalent of gorm's [`Transaction`](https://pkg.go.dev/gorm.io/gorm#DB.Transaction)/[`FindInBatches`](https://pkg.go.dev/gorm.io/gorm#DB.FindInBatches). The named callback's `*gorm.DB` parameter is then treated as immutable, so reuse inside the callback is allowed:

```go
//gormreuse:immutable-input(cb)
func withFreshDB(cb func(*gorm.DB) error, db *gorm.DB) error {
    return cb(db.Session(&gorm.Session{})) // hands the callback an isolated value
}

func useIt(db *gorm.DB) {
    _ = withFreshDB(func(q *gorm.DB) error {
        q.Find(&users)
        q.Count(&count) // OK — q is declared immutable input
        return nil
    }, db)
}
```

> [!WARNING]
> **Body contract.** If the function actually hands the callback a mutable value, it is reported:
> ```go
> //gormreuse:immutable-input(cb)
> func bad(cb func(*gorm.DB) error, db *gorm.DB) error {
>     q := db.Where("x")
>     return cb(q) // VIOLATION: immutable-input(cb) declared but mutable *gorm.DB passed to callback
> }
> ```
>
> The check follows the callback wherever the function keeps it:
>
> - `go cb(...)` and `defer cb(...)`
> - a closure that captures it
> - a local variable or its address
> - a struct field, a slice, an array, a map, and `append`
> - an interface it is boxed in
>
> A `func(**gorm.DB)` callback called with `&q` is checked against every value stored in `q`.
>
> Handing `cb` to another `immutable-input` function at its declared callback, or to gorm's `Transaction`/`FindInBatches`, keeps the promise. Any other flow cannot be checked, so it is reported:
>
> | Flow | Message |
> | --- | --- |
> | Passed to a function, method or function value | `callback cb escapes (passed to forward); cannot verify the *gorm.DB it receives` |
> | Returned, sent on a channel, stored in a package-level variable or through a pointer | `callback cb escapes (returned)` and so on |
> | Called with a value that holds a `*gorm.DB` the checker cannot trace (a `**gorm.DB` parameter, a struct) | `cannot verify the *gorm.DB passed to callback` |
>
> The directive is reported **unused** when `name` isn't a parameter, isn't a function type, or the callback has no [`*gorm.DB`](https://pkg.go.dev/gorm.io/gorm#DB) parameter. It is also reported when it is attached to no function or closure.

Write it where `pure` goes: in the function's doc comment, after its opening brace on the same line, or on the line before a closure:

```go
func withFresh(cb func(*gorm.DB) error, db *gorm.DB) error { //gormreuse:immutable-input(cb)
    return cb(db.Session(&gorm.Session{}))
}

//gormreuse:immutable-input(cb)
withTx := func(cb func(*gorm.DB) error) error { return cb(fresh) }
```

> [!NOTE]
> gorm's built-in [`Transaction`](https://pkg.go.dev/gorm.io/gorm#DB.Transaction) and [`FindInBatches`](https://pkg.go.dev/gorm.io/gorm#DB.FindInBatches) already pass a fresh handle to their callbacks. So reuse inside those callbacks needs no directive. [`Connection`](https://pkg.go.dev/gorm.io/gorm#DB.Connection) does not: its callback gets a `clone == 0` handle, so reuse inside it is reported. Do not put `//gormreuse:immutable-input` on a wrapper that forwards a Connection `tx` unchanged. Its body contract would be false.

### `//gormreuse:pure,immutable-return`

The recommended pattern for DB connection helpers. It combines both guarantees, so each one needs its own `Session`:

```go
//gormreuse:pure,immutable-return
func GetTenantDB(db *gorm.DB, tenantID int) *gorm.DB {
    return db.Session(&gorm.Session{}).       // pure: Where runs on a copy, not on db
        Where("tenant_id = ?", tenantID).
        Session(&gorm.Session{})              // immutable-return: the result is isolated
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

> [!WARNING]
> Unused `//gormreuse:pure`, `//gormreuse:immutable-return`, and `//gormreuse:immutable-param` directives are reported as warnings. A directive is unused when the signature doesn't fit, e.g. `immutable-param` on a function with no [`*gorm.DB`](https://pkg.go.dev/gorm.io/gorm#DB) parameter. A **redundant** `//gormreuse:immutable-param` (signature-valid but its parameter is never reused) is reported too. For combined directives like `//gormreuse:pure,immutable-return`, if either part is used, no unused warning is reported.

## Temporary rule: `Session`/`WithContext`/`Debug` returned from `Scopes` callbacks

> [!WARNING]
> This is a **temporary** rule working around an upstream GORM bug.

A [`Scopes`](https://pkg.go.dev/gorm.io/gorm#DB.Scopes) callback that **returns** the result of [`Session`](https://pkg.go.dev/gorm.io/gorm#DB.Session), [`WithContext`](https://pkg.go.dev/gorm.io/gorm#DB.WithContext), or [`Debug`](https://pkg.go.dev/gorm.io/gorm#DB.Debug) leaks the transaction of a create, update or delete: GORM begins it and never commits or rolls it back ([go-gorm/gorm#7592](https://github.com/go-gorm/gorm/issues/7592); the fix attempt [#7593](https://github.com/go-gorm/gorm/pull/7593) was closed without merging).

```go
db.Scopes(func(q *gorm.DB) *gorm.DB {
    return q.Session(&gorm.Session{}) // WARNING: Session() result returned from Scopes callback causes transaction leak
})
```

GORM runs the query on the value the callback returns. These three methods return a value that makes GORM store its "transaction started" flag on a copy of the statement, so the commit step never finds it. Every other chain method returns a value without this problem. So only the returned value matters:

| Inside a `Scopes` callback | Leaks | Reported |
| --- | --- | --- |
| `return q.Session(&gorm.Session{})` | Yes | Yes |
| `return other.Session(&gorm.Session{})` (any `*gorm.DB`) | Yes | Yes |
| `s := q.Session(...); s.Where("a").Find(&x); return s` | Yes | Yes |
| `return q.Session(&gorm.Session{}).Where("x")` | No | No |
| `other.Session(&gorm.Session{}).Find(&x); return q` | No | No |

`e2e/internal/scopes_session_test.go` checks each row against real GORM.

The rule finds the callback the same way reuse detection does. The callback may be a function literal, a named function, or a method value. It may also be a closure in a variable or a package-level variable, or in a slice built with `append`. The rule follows the returned value through variables, branches, and the returns of helpers and closures the callback calls. It reports the `Session`/`WithContext`/`Debug` call itself.

[`Preload`](https://pkg.go.dev/gorm.io/gorm#DB.Preload) callbacks are **unaffected** (Preload builds a fresh DB), so the same code inside a Preload callback is not flagged.

**Removal condition**: once the upstream fix ships in a supported GORM release, this rule is deleted. Removing the `internal/scopeswarn` package and its single call site in `RunSSA` deletes it, as the package documents. Re-check go-gorm/gorm#7592 on each GORM version bump.

## Known Limitations

gormreuse reports a reuse only when it can follow the value. These cases are known. Each one has a test fixture.

**Missed reuse (false negatives):**

| Code | Why it is missed |
| --- | --- |
| A closure assigns a captured variable: `f := func() { q = db.Where("x") }; f()` | The assignment inside the closure is not followed |
| `defer` inside a `go` closure, or `defer` → `go` → `defer` | These nested calls are not followed |
| A method value that a branch picks: `if c { find = q.Find } else { find = q.Find }` | A method value merged from branches is not traced to `q` |
| Closures stored in a slice and called in a loop | Each call branches `q` again, but the calls are not counted |
| `if c { q2 = q.Where("a") }; if d { q = q2 }; q.Count(nil)` | `q2` can flow back into `q`, so `Where("a")` is treated as an assignment of `q` |
| A field rebuilt in a loop after it was copied: `p.db = q; for … { p.db = p.db.Where(c) }; q.Find(nil)` | In a loop, the rebuild is an assignment, so its first read of `q` is not a branch |
| A pure function that returns `db.Session(...)` of its argument | The copy is not the argument, so the shared `Statement` is hidden from the caller |
| A package-level `*gorm.DB` that another function, a closure or the initializer assigned | Only the stores in the same function are followed. A global that the function only reads is immutable |
| A struct field read through a parameter or a receiver (`r.db`) | The field is not a mutable root. Only the stores to it in the same function are followed |
| `immutable-param` called through an interface or a func-typed parameter | The callee is unknown, so the caller contract is not checked |

**Extra reports (false positives):**

| Code | Why it is reported |
| --- | --- |
| `a, b := pair(db)`, then `a.Find(nil); b.Find(nil)` | Both results of one call share the call as their root |
| `p := &q; *p = db.Where("y")`, then `q.Find(nil)` | The new value stored through the pointer is still traced to the old root |
| `G = db.Where("x"); if c { G = G.Where("a") }; G.Find(nil)` for a global or a field `G` | A store is not a merge point, so `Where("a")` counts as a branch of the stored root |

**Wrong position:** a use inside a deferred closure runs at function exit. gormreuse orders it by its source position. So `defer func() { q.Count(nil) }(); q.Find(nil)` is reported at `q.Find`, which runs first.

See [Known Limitations](./design/implementation.md#known-limitations) in the implementation notes for the fixtures and the reasons.

## Documentation

- [AGENTS.md](./AGENTS.md) - Repository instructions for contributors and coding agents
- [design/implementation.md](./design/implementation.md) - How the analysis works, design decisions, and limitations

## Development

Develop on Go 1.27 or later. The main module and the E2E module (`e2e/internal`) both require it.

| Command | What it does |
| --- | --- |
| `go test ./...` | Runs the analyzer tests in the main module |
| `cd e2e/internal && go test ./...` | Runs the E2E tests. They check GORM's behavior against real GORM with sqlmock. This is a separate module |
| `GORMREUSE_UPDATE_GOLDEN=1 go test -run TestGoldenFiles .` | Rewrites the `.golden` and `.diff` fixtures after a change to detection or fixes. A plain `go test` only compares them |
| `./test_all.sh` | Runs every check CI runs: declscope, the tests of both modules, a build, and golangci-lint on both modules. Install the tools with `mise install` |
| `go build -o bin/gormreuse ./cmd/gormreuse` | Builds the command |

## License

MIT License
