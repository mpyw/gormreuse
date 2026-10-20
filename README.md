# gormreuse

[![Go Reference](https://pkg.go.dev/badge/github.com/mpyw/gormreuse.svg)](https://pkg.go.dev/github.com/mpyw/gormreuse)
[![CI](https://github.com/mpyw/gormreuse/actions/workflows/ci.yml/badge.svg)](https://github.com/mpyw/gormreuse/actions/workflows/ci.yml)
[![Codecov](https://codecov.io/gh/mpyw/gormreuse/graph/badge.svg)](https://codecov.io/gh/mpyw/gormreuse)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

A Go linter that detects unsafe [`*gorm.DB`](https://pkg.go.dev/gorm.io/gorm#DB) instance reuse after chain methods.

## Background

[GORM](https://pkg.go.dev/gorm.io/gorm)'s Traditional API chain methods ([`Where`](https://pkg.go.dev/gorm.io/gorm#DB.Where), [`Order`](https://pkg.go.dev/gorm.io/gorm#DB.Order), etc.) modify internal state. Reuse the same [`*gorm.DB`](https://pkg.go.dev/gorm.io/gorm#DB) instance after a chain method, and the query conditions can pile up.

<table>
<tr><th>Bug</th><th>Fix</th></tr>
<tr><td>

```go
q := db.Where("active = ?", true)

// SELECT * FROM users WHERE active = true
q.Find(&users)

// Bug: conditions accumulate unexpectedly
q.Find(&admins)
```

</td><td>

```go
q := db.
    Where("active = ?", true).
    Session(&gorm.Session{})

// OK: q is immutable
q.Find(&users)

// OK: a fresh chain from q
q.Find(&admins)
```

</td></tr>
</table>

[GORM's guide on Method Chaining](https://gorm.io/docs/method_chaining.html#Reusability-and-Safety) documents this problem. GORM's Generics API (v1.30.0+) is a safer alternative. But many production codebases still use the Traditional API, and will keep it for years. gormreuse catches these bugs in such code.

### What it reports

<table>
<tr><th>Report</th><th>Example</th></tr>
<tr><td>

**[A second branch from a mutable `*gorm.DB`](#detection-model-mutable-branching)**

</td><td>

```go
q := db.Where("x")
q.Find(&a)

// VIOLATION: second branch from q
q.Count(&n)
```

</td></tr>
<tr><td>

**[One use that runs more than once](#examples)**

</td><td>

```go
q := db.Where("x")
for _, id := range ids {
    // VIOLATION: runs on every iteration
    q.Where("id = ?", id).Find(&u)
}
```

</td></tr>
<tr><td>

**[A `Session`/`Debug` copy used after its root changed](#session-and-debug-copies)**

</td><td>

```go
q := db.Where("x")
c := q.Session(&gorm.Session{})
q.Where("a").Find(&x)

// VIOLATION: c sees the change to q
c.Count(&n)
```

</td></tr>
<tr><td>

**[A subquery changed before the outer query runs](#subqueries-and-condition-arguments)**

</td><td>

```go
sub := db.Model(&User{}).Select("id")
x := db.Where("id IN (?)", sub)
sub.Where("a").Find(&y)

// VIOLATION: the subquery has a
x.Find(&z)
```

</td></tr>
<tr><td>

**[A directive whose contract the code breaks](#directives)**

</td><td>

```go
//gormreuse:pure
func scope(db *gorm.DB) {
    // VIOLATION: pollutes the argument
    db.Where("x")
}
```

</td></tr>
<tr><td>

**[A `Session` result returned from a `Scopes` callback whose chain may write (GORM bug)](#temporary-rule-sessionwithcontextdebug-returned-from-scopes-callbacks)**

</td><td>

```go
db.Scopes(func(q *gorm.DB) *gorm.DB {
    // WARNING: leaks the transaction
    return q.Session(&gorm.Session{})
}).Delete(&User{})
```

</td></tr>
</table>

### Quick start

```bash
gormreuse ./...             # report
gormreuse -fix -diff ./...  # preview the fixes
gormreuse -fix ./...        # apply them
gormreuse -test=false ./... # skip *_test.go files
```

Some reports have no fix on purpose. See [Automatic Fixes](#automatic-fixes).

## Installation & Usage

> [!NOTE]
> This README describes the `main` branch. It may document changes that are not released yet. [CHANGELOG.md](./CHANGELOG.md) lists them.

| Method | Needs a Go toolchain | Pins the version in |
| --- | --- | --- |
| [mise](#mise) **(recommended)** | No. The binaries are prebuilt | `mise.toml` |
| [`go tool`](#go-tool) | Yes, Go 1.27 or later | `go.mod` |
| [`go install`](#go-install) | Yes, Go 1.27 or later | The command line |
| [`go vet`](#go-vet) | Yes, Go 1.27 or later | The command line |
| [`go run`](#go-run) | Yes, Go 1.27 or later | The command line |
| [Release archive](#release-archive) | No | The file you download |
| [golangci-lint custom build](#golangci-lint) | Yes, to build the binary | `.custom-gcl.yml` |

> [!IMPORTANT]
> Building gormreuse from source requires Go 1.27 or later.

> [!CAUTION]
> To prevent supply chain attacks, pin to a specific version tag instead of `@latest` in CI/CD pipelines (e.g., `@v0.22.0`).

<a id="mise"></a>
<details>
<summary><a href="https://mise.jdx.dev/"><img src="https://mise.jdx.dev/logo.svg" height="20" alt=""></a> Using <b>mise</b> (macOS/Linux/Windows), recommended</summary>

mise's `github` backend installs gormreuse directly from GitHub Releases. It needs no extra registry. It needs no Go toolchain either, because the binaries are prebuilt:

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

</details>

<a id="go-tool"></a>
<details>
<summary>Using <a href="https://pkg.go.dev/cmd/go#hdr-Run_specified_go_tool"><code>go tool</code></a></summary>

```bash
# Add to go.mod as a tool dependency
go get -tool github.com/mpyw/gormreuse/cmd/gormreuse@latest

# Run via go tool
go tool gormreuse ./...
```

**Note:** gormreuse's `go.mod` says `go 1.27.0`. So `go get -tool` raises the `go` line of your `go.mod` to `1.27.0` when it is lower. Use another install method if your module must stay on an older Go.

</details>

<a id="go-install"></a>
<details>
<summary>Using <a href="https://pkg.go.dev/cmd/go#hdr-Compile_and_install_packages_and_dependencies"><code>go install</code></a></summary>

```bash
go install github.com/mpyw/gormreuse/cmd/gormreuse@latest
gormreuse ./...
```

</details>

<a id="go-vet"></a>
<details>
<summary>Using <a href="https://pkg.go.dev/cmd/go#hdr-Report_likely_mistakes_in_packages"><code>go vet</code></a></summary>

gormreuse has no custom flags, so `go vet` can run it:

```bash
go install github.com/mpyw/gormreuse/cmd/gormreuse@latest
go vet -vettool=$(which gormreuse) ./...
```

</details>

<a id="go-run"></a>
<details>
<summary>Using <a href="https://pkg.go.dev/cmd/go#hdr-Compile_and_run_Go_program"><code>go run</code></a></summary>

```bash
go run github.com/mpyw/gormreuse/cmd/gormreuse@latest ./...
```

</details>

<a id="release-archive"></a>
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

<a id="golangci-lint"></a>
<details>
<summary><a href="https://golangci-lint.run/"><img src="https://cdn.simpleicons.org/go" height="20" alt=""></a> Running inside golangci-lint (custom build)</summary>

gormreuse is not bundled with golangci-lint. But it ships a [module plugin](https://golangci-lint.run/plugins/module-plugins/), so you can build a golangci-lint binary that holds it. Write `.custom-gcl.yml`:

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
issues:
  uniq-by-line: false # show every gormreuse report on a line
```

By default, golangci-lint shows only the first issue on each line. gormreuse can report more than one on a line, as in `q.Find(nil); q.Count(nil); q.First(nil)`. `issues.uniq-by-line: false` shows them all.

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
| `-json` | `false` | Print the diagnostics as JSON, sorted by file and position |
| `-c=N` | `-1` | Print each reported line with `N` lines of context |
| `-V=full` | | Print the version and exit (`go vet` uses this) |
| `-flags` | | Print the flags as JSON and exit |

The command exits with status `3` when it reports a diagnostic. It exits with `0` when it reports none, or when `-json` is set.

<details>
<summary>Which files are analyzed, and internal errors</summary>

| Files | Analyzed | Directives | `-fix` edits them |
| --- | --- | --- | --- |
| `*_test.go` | Unless `-test=false` | Checked | Yes |
| Generated (contains `// Code generated ... DO NOT EDIT.`) | Never. They cannot be opted in | Not checked, so not trusted. At any call, in the same package or another, a function in a generated file counts as unmarked, whatever directive it carries | No |
| Files that import `"C"` | Yes. The header that cgo adds to its output does not count | Checked | No. The driver treats cgo's output as generated |

| Where gormreuse fails | Report |
| --- | --- |
| Inside a function | `gormreuse: internal error analyzing <func>: ...` at that function. gormreuse does not trust the directives that the function's failed check was proving |
| In work that reads the whole package | `gormreuse: internal error in <stage> of package <path>: ...` at the package clause |

Please [open an issue](https://github.com/mpyw/gormreuse/issues) with the code. Set `GORMREUSE_DEBUG_PANIC=1` to get the stack trace instead.

</details>

## Automatic Fixes

`-fix` repairs most reuse with two strategies. Both can apply to one root. Each change below is `gormreuse -fix -diff` output:

<table>
<tr><th>Strategy</th><th>Change</th></tr>
<tr><td>

**[Reassignment](#strategy-1-reassignment-creating-new-roots)**

For a chain statement whose result is discarded

</td><td>

```diff
 q := db.Where("base")
-q.Where("a")
+q = q.Where("a")
 q.Where("b").Find(nil)
```

</td></tr>
<tr><td>

**[Session](#strategy-2-session-making-roots-immutable)**

For a root that still branches more than once

</td><td>

```diff
-q := db.Where("base")
+q := db.Where("base").Session(&gorm.Session{})
 q.Where("a").Find(nil)
 q.Where("b").Find(nil)
```

</td></tr>
<tr><td>

**[Parameter roots](#parameter-roots)**

For a reused `*gorm.DB` parameter. Some helpers get `//gormreuse:immutable-param` instead

</td><td>

```diff
 func List(tx *gorm.DB) {
+    tx = tx.Session(&gorm.Session{})
     tx.Where("a").Find(&users)
     tx.Where("b").Find(&users)
 }
```

</td></tr>
<tr><td>

**[Contract and directive fixes](#contract-and-directive-fixes)**

For directive diagnostics. It isolates an argument, or deletes an unused directive name

</td><td>

```diff
 func caller(db *gorm.DB) {
     q := db.Where("x")
-    apply(q)
+    apply(q.Session(&gorm.Session{}))
 }
```

</td></tr>
</table>

> [!IMPORTANT]
> A fix never drops a condition. When it cannot keep every change the code makes, the diagnostic has **no fix**. See [When there is no fix](#when-there-is-no-fix).

### Strategy 1: Reassignment (Creating New Roots)

A discarded chain statement on the local variable that holds the root becomes a reassignment. That creates a new mutable root at each step. In the [example](#automatic-fixes), `q.Where("a")` is the first branch, and it changes `q` in place. After the fix, `q.Where("b")` is the first branch from the new root.

<details>
<summary>When a reassignment is applied</summary>

| Applied when | Detail |
| --- | --- |
| The method is a non-finisher | A chain builder such as `Where`, `Order` or `Limit` whose result is a `*gorm.DB`. Calls that return something else are not reassigned: `AddError`, `DB()`, `Association`, `Migrator`, `AutoMigrate`. Neither are calls that run SQL or end a transaction: `FindInBatches`, `CreateInBatches`, `UpdateColumn(s)`, `Commit`, `Rollback`, `SavePoint`, `RollbackTo` |
| The result is not assigned to a variable | The expression is used as a statement. A chain such as `q.Where("a").Limit(10)` is reassigned as a whole. A Session it needs goes after its last call |
| The receiver is the local variable that holds the reused root | These are not rewritten: aliases (`r := q`), calls that take the root as an argument (`base.Model(q)`), and package-level variables. Neither are variables captured by a closure that is not called inline (`go func() { ... }()`, `defer func() { ... }()`). The fix relies on a Session at the root instead, unless that would drop a discarded statement or another in-place change (see [When there is no fix](#when-there-is-no-fix)) |
| Inside a loop | The reassignment gets a Session when the next iteration uses it again, that is, when the loop does not define the root again |

</details>

### Strategy 2: Session (Making Roots Immutable)

A root that still has two or more branches on one path gets `.Session(&gorm.Session{})` at the end of its chain. In the [example](#automatic-fixes), the root becomes immutable. So each branch from it is an independent chain.

<details>
<summary>Where a Session goes</summary>

| Applied to | Detail |
| --- | --- |
| A root or a reassigned value with 2+ branches on one path | This holds after the reassignments. A use in a loop that does not create the value again counts too. So `q.Where("t"); for _, id := range ids { q.Where("id = ?", id).Find(nil) }` becomes `q = q.Where("t").Session(&gorm.Session{})`. The paths come from the control flow. So in `if c { q.Where("a") }; q.Where("b").Find(nil); q.Where("c").Find(nil)`, both the root and `q = q.Where("a")` get a Session. A reassigned value used once gets none. Neither does a chain that already ends in a Session |
| Every root or reassigned value with a later use | When the fix cannot see every use of the root in order. Examples are a `defer`/`go` use, a use inside a closure, and a read through a `Session` copy |
| Each incoming edge of a Phi node (a conditional merge) that is still mutable | An edge counts as immutable by the same rule as detection: its **last** call must return an immutable value. `db.Session(&gorm.Session{}).Where("y")` and `db.WithContext(ctx).Where("y")` are mutable, so they get a Session too |
| Only a call that returns `*gorm.DB` | An edge that has no such call gets none. Examples are a closure parameter (`if c { q = tx }`) and one result of `q, err := open()` |

</details>

<details>
<summary>How the fix writes <code>Session</code> and its import</summary>

`Session` is written with a name that refers to `gorm.io/gorm` where it is inserted:

| The file has | The fix |
| --- | --- |
| A name for `gorm.io/gorm` | Uses it |
| No such name | Adds the import `"gorm.io/gorm"` |
| No such name, and `gorm` is taken there | Adds `gormio "gorm.io/gorm"`. A parameter or local variable named `gorm` takes the name. So does another package imported as `gorm` |
| Only a blank import (`_ "gorm.io/gorm"`) | It gives no name, so the fix adds an import as above |

- Each diagnostic's fix carries its own import edit. So an editor can apply one quick fix alone.
- Every diagnostic of a root carries the same fix.
- A root with a diagnostic that `//gormreuse:ignore` suppresses gets **no fix**. The ignore says that reuse is meant. The fix would change it too, so the next run would report the ignore as unused.

</details>

<details>
<summary>Combined example: both strategies on one root</summary>

The `b`, `c` and `d` lines are reported:

```diff
 q := db.Where("base")
-q.Where("a")
+q = q.Where("a").Session(&gorm.Session{})
 q.Where("b").Find(nil)
-q.Where("c")
+q = q.Where("c")
 q.Where("d").Find(nil)
```

| Line | Fix | Why |
| --- | --- | --- |
| `a` | Reassignment and Session | `b` and `c` both use the new root |
| `c` | Reassignment only | Only `d` uses the new root |

When several roots are reused at the same place, such as `use(q, r)`:

- That place gets one diagnostic. Its message names the root that appears first in the source.
- Its fix covers every root reused there.
- A different problem at the same place gets its own diagnostic, such as a [condition argument](#subqueries-and-condition-arguments) read after it changed.
- The output is the same on every run.

</details>

### When there is no fix

A discarded statement such as `q.Where("a")` changes `q` in place, and later queries see `"a"`. A Session at the root alone would make it change a copy, and silently drop `"a"`. So when the fix cannot keep such a change, the diagnostic has **no fix**. Fix it by hand.

<details>
<summary>Every case without a fix</summary>

<table>
<tr><th>No fix when</th><th>Examples</th></tr>
<tr><td>

A discarded statement cannot be reassigned

</td><td>

- an alias receiver
- a method value (`where := q.Where`)
- a variable captured by a closure
- a chain with a Session-family call before its last call (`q.Where("a").Session(s).Limit(1)`)

</td></tr>
<tr><td>

`q` is handed to other code before a later use of `q`, or in a loop

</td><td>

- An argument of a function that is not `//gormreuse:pure`: `applyTenant(q)`, `apply := func(d *gorm.DB) { ... }; apply(q)`, `_ = withTenant(q)`, `base.Model(q)`.
- A store into a slice, map or channel.
- A function other than a `*gorm.DB` method that returns a `*gorm.DB` may return `q` itself. So its argument counts even as the last use (`q = withTenant(q)`).
- An argument of a `go` statement, and the receiver of a chain method it runs (`go q.Where("a")`).
- The same for a `defer` statement, when another `defer` uses `q` or the statement is in a loop. `defer q.Find(&x); defer q.Where("a")` runs `Where` first, and `Find` reads its condition.

These do not count:

- The receiver of a query (`go q.Count(&n)`). With the Session, the query runs on a copy.
- A subquery (`db.Where("id IN (?)", q)`). It only reads `q`.

</td></tr>
<tr><td>

`q` is stored in a struct field that other code can reach before a later use of `q` (#310)

</td><td>

- A local struct is reached where it is handed on: `s := &Svc{db: q}; s.apply()`, `applyP(params{db: q})`, or a closure that does so.
- A package-level variable, or a struct that a parameter points to, is reached by any call after the store, other than a `*gorm.DB` method.

Reading the field in the same function (`s.db.Find(&x)`) is a use of `q`, and does not count.

</td></tr>
<tr><td>

The `Error`, `RowsAffected` or `Statement` field is read from `q`, or from a chain on `q` that runs no query

</td><td>

- `return q.Error`
- `if err := q.Where("a").Error; ...`

Each query adds its error to `q.Error`, and that chain changes `q` the same way a discarded statement does. Reading the field of a query's own result (`q.Where("a").Find(&x).Error`) is fine.

</td></tr>
<tr><td>

A parameter root that the fix cannot isolate

</td><td>

See [Parameter Roots](#parameter-roots).

</td></tr>
<tr><td>

A root with a diagnostic that an ignore suppresses

</td><td>

The ignore says that reuse is meant. See [Strategy 2](#strategy-2-session-making-roots-immutable).

</td></tr>
</table>

</details>

### Parameter Roots

A reused `*gorm.DB` parameter has no definition to append a Session to. Most functions get `tx = tx.Session(&gorm.Session{})` at the top of the body. An unexported helper whose every caller gormreuse can check gets [`//gormreuse:immutable-param`](#gormreuseimmutable-param) instead.

<details>
<summary>Which function gets which fix, and why</summary>

<table>
<tr><th>Function</th><th>Fix</th></tr>
<tr><td>

Every one of these holds:

- unexported, and not a method
- only called directly in the package. Callers in package-level closures (`var f = func(...)`) count
- every caller passes an immutable value for every `*gorm.DB` parameter
- when the package has test files, declared in a test file

</td><td>

Add [`//gormreuse:immutable-param`](#gormreuseimmutable-param). The caller-side check keeps the callers isolated.

</td></tr>
<tr><td>

Any other function, such as:

- an exported function, a method, or one used as a function value
- one with a caller that passes a mid-chain value
- one outside the test files of a package that has test files
- a closure with no `*gorm.DB` result: a callback of a `Transaction` wrapper, a closure called in place or with `go`, or one in a local variable

</td><td>

Rebind the parameter at the top of the body: `tx = tx.Session(&gorm.Session{})`. Discarded statements on it are reassigned as for a local root.

</td></tr>
<tr><td>

- a Scopes/Preload callback
- a function that returns a `*gorm.DB` and is not only called directly (it may be a Scopes callback)
- a closure that returns a `*gorm.DB`

</td><td>

**No fix**

</td></tr>
<tr><td>

A function with a discarded statement on the parameter (`tx.Where("a")`), and either:

- no `*gorm.DB` result
- or a caller that discards the result

</td><td>

**No fix**. The statement also changes the caller's value, and callers such as `applyTenant(q); q.Find(&users)` rely on that. With `tx = tx.Where("a")` the change would stay inside the function.

A `*gorm.DB` result carries it back only to a caller that reads it. So every call in the package must read the result, and a `go` or `defer` call does not. When the package has test files, the function must be declared in a test file.

</td></tr>
</table>

Why the rules are this strict:

- `immutable-param` on an exported function would hide the reuse: callers in other packages are never checked.
- On a function with a mid-chain caller, it would move the report to that caller.
- A closure never gets the directive. Write it yourself where it fits.
- A package with test files is analyzed twice: without the test files and with them. The run without them cannot see the callers in the test files. So both runs use the body Session for a function outside the test files, and their fixes agree.

Neither the directive nor the body Session covers a value that the parameter merges with. In pagination code, `if f.Name != "" { db = db.Where("name = ?", f.Name) }` is followed by `db.Count(&total)` and `db.Offset(10).Limit(10).Find(&users)`. That `Where` also gets a Session, as an incoming edge of a local root does, so the fix takes one pass.

</details>

### Contract and Directive Fixes

A mutable argument to an [`immutable-param`](#gormreuseimmutable-param) parameter gets `.Session(&gorm.Session{})`: `apply(q)` becomes `apply(q.Session(&gorm.Session{}))`. An unused or redundant directive name, or an unused ignore, is deleted from its comment.

<details>
<summary>Details of these fixes</summary>

| Diagnostic | Fix |
| --- | --- |
| Mutable `*gorm.DB` passed to an `immutable-param` parameter | Append `.Session(&gorm.Session{})` to the argument. An argument such as `*p` is wrapped first: `(*p).Session(...)`. When a call passes several mutable arguments, the fix isolates all of them. `Session` is named and imported as in [Strategy 2](#strategy-2-session-making-roots-immutable) |
| Unused `pure`, `immutable-return`, `immutable-param` or `immutable-input(name)` | Delete that name from the comment |
| Redundant `immutable-param` | Delete `immutable-param` from the function's comment. A comment before `a, b := func..., func...` covers both closures. So it gets no fix while another closure still needs it |
| Unused `ignore` | Delete `ignore` from the comment |

A fix deletes only the name it is about. The comment keeps its other names and its reason. A directive on its own line is deleted with its line. Here every `ignore` is unused:

```diff
-//gormreuse:ignore,pure // why
+//gormreuse:pure // why
 func peek(q *gorm.DB) { _ = q }
 
-//gormreuse:ignore // why
+// why
 func a(db *gorm.DB) {
     q := db.Where("x")
-    q.Find(nil) //gormreuse:ignore //nolint:errcheck
-    //gormreuse:ignore
+    q.Find(nil) //nolint:errcheck
     peek(q)
 }
 
 func b(db *gorm.DB) {
     q := db.Where("x")
-    q.Find(nil) //gormreuse:ignore
+    q.Find(nil)
 }
```

There is no fix when the reason would become a directive itself, as in `//gormreuse:ignore //gormreuse:pure`.

An unused ignore keeps its warning but gets no fix in two cases:

| Case | Why |
| --- | --- |
| After a `gormreuse: internal error` report | The analysis may have lost the diagnostic the ignore covers |
| In a file other than a `_test.go` file, when the package has `_test.go` files | The package is analyzed twice, with and without its tests. The ignore may be needed only with them. For example, a test may call a Transaction callback directly. Delete such an ignore by hand |

</details>

## Detection Model: Mutable Branching

gormreuse reports when a **mutable [`*gorm.DB`](https://pkg.go.dev/gorm.io/gorm#DB) branches into multiple code paths**:

1. **Immutable-returning methods** ([`Session`](https://pkg.go.dev/gorm.io/gorm#DB.Session), [`WithContext`](https://pkg.go.dev/gorm.io/gorm#DB.WithContext), [`Debug`](https://pkg.go.dev/gorm.io/gorm#DB.Debug), etc.) return an **immutable** instance that can branch freely.
2. **State-only methods** ([`AddError`](https://pkg.go.dev/gorm.io/gorm#DB.AddError), [`DB`](https://pkg.go.dev/gorm.io/gorm#DB.DB), [`Get`](https://pkg.go.dev/gorm.io/gorm#DB.Get), [`InstanceGet`](https://pkg.go.dev/gorm.io/gorm#DB.InstanceGet), [`Callback`](https://pkg.go.dev/gorm.io/gorm#DB.Callback)) touch no clause. A call of one does not count at all.
3. **All other methods** on a mutable instance create a **branch** that consumes the instance.
4. A **second branch** from the same mutable root is a **violation**.
5. A **`Session`/`Debug` copy** of a mutable instance still reads that instance's `Statement`. So using the copy **after the instance changed** is a **violation** (see [Session and Debug copies](#session-and-debug-copies)).
6. A mutable instance passed as a **subquery or condition argument** (`db.Where("id IN (?)", sub)`) is **read**, not branched. Running the outer query **after the argument changed** is a **violation** (see [Subqueries and condition arguments](#subqueries-and-condition-arguments)).

### Method Classification

> [!IMPORTANT]
> This linter analyzes the **Traditional API** ([`*gorm.DB`](https://pkg.go.dev/gorm.io/gorm#DB)) only. The Generics API ([`gorm.G[T]`](https://pkg.go.dev/gorm.io/gorm#G), available since v1.30.0) returns different types ([`Interface[T]`](https://pkg.go.dev/gorm.io/gorm#Interface), [`ChainInterface[T]`](https://pkg.go.dev/gorm.io/gorm#ChainInterface)). It is excluded from analysis automatically.

| Category | Methods | Effect on the receiver |
| --- | --- | --- |
| Immutable-returning | `Session`, `WithContext`, `Debug`, [`Open`](https://pkg.go.dev/gorm.io/gorm#Open), [`Begin`](https://pkg.go.dev/gorm.io/gorm#DB.Begin) | Return a new immutable instance (`clone > 0`). They read the receiver but do not branch it. `Session` and `Debug` have exceptions: see [Session and Debug copies](#session-and-debug-copies) |
| Transaction | [`Transaction`](https://pkg.go.dev/gorm.io/gorm#DB.Transaction) | Returns an `error`. Like `Begin`, it reads the receiver but does not branch it. Its callback gets a fresh `tx` (see [Parameters are mutable](#parameters-are-mutable)) |
| ToSQL | [`ToSQL`](https://pkg.go.dev/gorm.io/gorm#DB.ToSQL) | Returns a `string`. It builds SQL from a copy of the receiver's Statement, so it reads the receiver but does not branch it. A branch before it is reported |
| State-only | `AddError`, `DB`, `Get`, `InstanceGet`, `Callback` | Touch no clause, so they neither read nor branch the receiver. `AddError` sets `db.Error`, which a later chain of the same value inherits and stops at. That is what `AddError` is for, so it is not reported |
| All others | [`Where`](https://pkg.go.dev/gorm.io/gorm#DB.Where), [`Find`](https://pkg.go.dev/gorm.io/gorm#DB.Find), [`Count`](https://pkg.go.dev/gorm.io/gorm#DB.Count), [`Order`](https://pkg.go.dev/gorm.io/gorm#DB.Order), etc. | Create a branch from the receiver |

### Automatic Pollution Sources

gormreuse cannot follow a `*gorm.DB` everywhere. These count as a branch: an interface method call (`repo.Query(db)`), a channel send, a slice or map store, and an interface conversion. So does a call to a function that is not `//gormreuse:pure`. A subquery or condition argument is a [read](#subqueries-and-condition-arguments), not a branch.

<details>
<summary>Every pollution source, and how struct fields are followed</summary>

| Operation | Example | Why |
| --- | --- | --- |
| Interface method call | `repo.Query(db)` | It can't be analyzed statically |
| Channel send | `ch <- db` | It may be received and used elsewhere |
| Slice/Map storage | `[]*gorm.DB{db}` | It may be accessed elsewhere |
| Interface conversion | `interface{}(db)` | It may be extracted via a type assertion |
| Non-pure function call | `helper(db)` | Unless the function is marked `//gormreuse:pure` |
| Struct field access | `h.db.Find(nil)` | It traces back to the stored value |

A struct field is followed in these forms:

| Form | Example or rule |
| --- | --- |
| A field of a struct literal or variable | Includes nested fields and fields reached through embedding |
| Methods promoted from an embedded `*gorm.DB` | `e := E{db.Where("x")}; e.Where("a")` |
| A field of a struct that a function returned, by value or by pointer | `s := mk(db); s.DB.Where("a")`. Each field of each call is its own mutable root, like a function that returns `*gorm.DB`. A call inside a loop makes a new root on each iteration. A store to the field through the returned pointer replaces it, as for a local struct. No fix is suggested for it, because the `Session` belongs inside the function. A helper that builds the struct from safe handles, such as a test setup function, is [inferred](#inferred-immutable-return) `immutable-return`. Mark one the analyzer cannot prove with `//gormreuse:immutable-return` |
| A struct captured by a closure | Also through a pointer |

- A field read through a parameter or a receiver, by value or by pointer, is not a mutable root. Only the stores to it in the same function are followed. A getter that returns such a field is read the same way, as the field of the receiver at each call (see [Inferred immutable-return](#inferred-immutable-return)).
- Simple struct literal storage (`_ = &S{db: q}`) without actual field usage does NOT pollute.
- A field read sees only the stores that can run before it. So the builder idiom `r.db = r.db.Where(...)` in a pointer-receiver method, followed by one use of `r.db`, is not reported. A second use of `r.db` is.

</details>

### Examples

In these examples, `db` is the value that `gorm.Open` returned, which is immutable. A `*gorm.DB` parameter is mutable (see [Parameters are mutable](#parameters-are-mutable)). So some of the safe examples are reported when `db` is a parameter.

| Pattern | Reported? |
| --- | --- |
| Two branches from a mutable value, finisher or not | Yes |
| Branches from a value whose chain **ends** in `Session` | No |
| A `Session` in the middle of the chain | Yes |
| One use in a loop, or in a closure that runs more than once | Yes |
| A variable reassigned before its next use | No |
| A chain kept in a different variable, then the original used again | Yes |

> [!IMPORTANT]
> **Chaining without reassignment is a violation!** Each statement using the same variable creates a separate branch:
> ```go
> q := db.Where("base")
>
> // OK: first branch
> q.Where("a")
>
> // VIOLATION: second branch
> q.Where("b")
>
> // VIOLATION: third branch
> q.Find(&users)
> ```
> **Solution**: Reassign the result or use method chaining in a single expression:
> ```go
> // Option 1: Reassign each step
> q := db.Where("base")
> q = q.Where("a")
> q = q.Where("b")
>
> // OK: first branch from final q
> q.Find(&users)
>
> // Option 2: Single chained expression
> // OK: single chain
> db.Where("base").
>     Where("a").
>     Where("b").
>     Find(&users)
> ```

<details>
<summary>Examples: branching</summary>

<table>
<tr><th>Code</th><th>Reported?</th><th>Why</th></tr>
<tr><td>

```go
// A Session at the end makes q immutable, so it may branch many times
q := db.
    Where("active = ?", true).
    Session(&gorm.Session{})

// OK: first branch from q
q.Count(&count)

// OK: q is immutable, can branch freely
q.Find(&users)
```

</td><td>No</td><td>

Each branch from an immutable value creates an independent mutable chain.

</td></tr>
<tr><td>

```go
q := db.Where("base").Session(&gorm.Session{})

// OK: branch 1 (independent chain)
q.Where("a").Find(&users1)

// OK: branch 2 (independent chain)
q.Where("b").Find(&users2)
```

</td><td>No</td><td>

Same as above.

</td></tr>
<tr><td>

```go
where := db.
    Where("base").
    Session(&gorm.Session{}).
    Where

// OK: same as q.Where("a")
where("a").Find(&users1)

// OK: same as q.Where("b")
where("b").Find(&users2)
```

</td><td>No</td><td>

A call through a method value is the same as the direct call.

</td></tr>
<tr><td>

```go
// q is mutable
q := db.Where("active = ?", true)

// OK: first branch from q
q.Find(&users)

// VIOLATION: second branch from q
q.Count(&count)
```

</td><td><b>Yes</b></td><td>

The second branch from a mutable value is a violation.

</td></tr>
<tr><td>

```go
q := db.Where("x")

// OK: first branch from q
q.Where("a")

// VIOLATION: second branch from q
q.Where("b")
```

</td><td><b>Yes</b></td><td>

Not only a "finisher": any method creates a branch.

</td></tr>
<tr><td>

```go
q := db.Where("x")
where := q.Where

// OK: first branch from q
where("a").Find(&users1)

// VIOLATION: second branch from q
where("b").Find(&users2)
```

</td><td><b>Yes</b></td><td>

A method value of a mutable value branches it on each call.

</td></tr>
<tr><td>

```go
q := db.Where("x")
find := q.Find

// VIOLATION: runs after q.Count
defer find(&users)
q.Count(&count)
```

</td><td><b>Yes</b></td><td>

`go` and `defer` are uses too, also through a method value. A deferred call runs when the function returns, after every other use.

</td></tr>
<tr><td>

```go
q := b
if condition {
    q = a
}

// OK: first branch from a or b
q.Find(&users)

// VIOLATION: second branch from a when condition holds
a.Find(&admins)
```

</td><td><b>Yes</b></td><td>

A variable that may hold one of two roots branches both.

</td></tr>
<tr><td>

```go
// q is mutable!
q := db.Session(&gorm.Session{}).Where("x")

// OK: first branch
q.Find(&users)

// VIOLATION: second branch
q.Count(&count)
```

</td><td><b>Yes</b></td><td>

A Session in the middle doesn't help. The result is still mutable.

</td></tr>
<tr><td>

```go
q := db.Where("x")

// OK: first branch
q.Find(&users)

// VIOLATION: second branch from q
q.Session(&gorm.Session{}).Count(&c)
```

</td><td><b>Yes</b></td><td>

An immutable-returning method on a polluted value is a branch too.

</td></tr>
</table>

</details>

<details>
<summary>Examples: loops and closures</summary>

<table>
<tr><th>Code</th><th>Reported?</th><th>Why</th></tr>
<tr><td>

```go
q := db.Where("x")
for v := range slices.Values(ids) {
    // VIOLATION: reused on every iteration
    q.Where("id = ?", v).Find(&users)
}
```

</td><td><b>Yes</b></td><td>

A loop body runs more than once, so one use inside it is reuse. Range-over-func loops (Go 1.23+) count as loops too.

</td></tr>
<tr><td>

```go
q := db.Where("x")

// VIOLATION: f runs twice
f := func() { q.Find(&users) }
f()
f()
for _, id := range ids {
    g.Go(func() error {
        // VIOLATION: one closure per iteration
        return q.Where("id = ?", id).Find(&users).Error
    })
}

// VIOLATION: run calls it in a loop
update := func(id int) { q.Where("id = ?", id).Find(&users) }
run := func() {
    for _, id := range ids {
        update(id)
    }
}
run()
```

</td><td><b>Yes</b></td><td>

So does a closure that runs more than once. That is one called at two sites, or one called, spawned, deferred or passed to another function inside a loop. A call inside another closure counts where that closure runs.

</td></tr>
<tr><td>

```go
for _, it := range items {
    q := db.Where("id = ?", it.ID)
    save := func() error { return q.Updates(it).Error }
    if it.New {
        // OK: each closure runs once
        err = save()
    } else {
        err = save()
    }
}
```

</td><td>No</td><td>

A closure made in the loop body is a new closure on each iteration.

</td></tr>
<tr><td>

```go
q := db.Model(&User{})
if org != nil {
    q = q.Where("org = ?", *org)
}
for _, id := range ids {
    // VIOLATION: the fix adds a Session to both roots
    q.Where("id = ?", id).Find(&users)
}
```

</td><td><b>Yes</b></td><td>

A use in a loop reuses every root that the variable may hold.

</td></tr>
</table>

</details>

<details>
<summary>Examples: reassignment</summary>

A variable reassignment creates a **new mutable root**. So the variable can be used fresh. gormreuse uses [SSA (Static Single Assignment)](https://pkg.go.dev/golang.org/x/tools/go/ssa) form, where each assignment creates a distinct value. So the new value has no relationship to the previous one.

<table>
<tr><th>Code</th><th>Reported?</th><th>Why</th></tr>
<tr><td>

```go
// immutable: safe to branch more than once
db, err := gorm.Open(dialector)

q := db.Where("x")

// OK: first branch from original q
q.Find(&users)

// reassignment creates NEW mutable root
q = db.Where("y")

// OK: first branch from new q
q.Find(&admins)

// another reassignment
q = db.Where("z")

// OK: first branch from newest q
q.Count(&count)
```

</td><td>No</td><td>

Each reassignment is a new root.

</td></tr>
<tr><td>

```go
for _, filter := range filters {
    // new mutable root each iteration
    q := db.Where(filter)

    // OK: first branch in this iteration
    q.Find(&results)
}
```

</td><td>No</td><td>

Reassignment in loops is also safe.

</td></tr>
<tr><td>

```go
q := db.Where("base")
if condition {
    // reassignment on this path
    q = db.Where("alt")
}

// OK: first branch from whichever root
q.Find(&users)
```

</td><td>No</td><td>

A conditional reassignment.

</td></tr>
<tr><td>

```go
q = db.Where("base")
if condition {
    where := q.Where

    // reassignment of q
    q = where("alt")
}

// OK
q.Find(&users)
```

</td><td>No</td><td>

The same through a method value.

</td></tr>
<tr><td>

```go
q := db.Where("x")
var q2 *gorm.DB
if condition {
    // first branch from q
    q2 = q.Where("a")
}

// VIOLATION: second branch from q
q.Count(&count)
```

</td><td><b>Yes</b></td><td>

A chain assigned to a **different** variable is a branch. This holds under a condition, and when a closure captures it.

</td></tr>
</table>

Each safe example above branches `db` more than once. That is safe only because `db` is immutable. If `db` were a `*gorm.DB` parameter, the second branch of `db` would be reported. A reassignment that extends the same variable, as in `if condition { q = q.Where("alt") }`, is not a second branch of `q`.

</details>

### Session and Debug copies

On a mutable value, [`Session(&gorm.Session{})`](https://pkg.go.dev/gorm.io/gorm#DB.Session) and [`Debug()`](https://pkg.go.dev/gorm.io/gorm#DB.Debug) return a copy that **points at the same `Statement`**. Using the copy does not change the original. But a change to the original after the copy was taken shows up in every later use of the copy. A change by reassignment (`q = q.Where(...)`) counts too, since it changes a mutable `Statement` in place:

```go
// q is mutable
q := db.Model(&User{}).Where("tenant = 1")

// countQ shares q's Statement
countQ := q.Session(&gorm.Session{})

// OK: q has not changed yet
countQ.Count(&before)

// first branch: changes q in place
q.Where("active = 1").Find(&users)

// VIOLATION: runs WHERE tenant = 1 AND active = 1
countQ.Count(&n)
```

To fix it, end the root's chain with `.Session(&gorm.Session{})` before you copy it. Or take the copy with an option that clones the `Statement`.

<details>
<summary>Which calls share the Statement</summary>

| Call on a mutable value | Shares the `Statement`? | Result |
| --- | --- | --- |
| `Session(&gorm.Session{})`, `Debug()` | Yes | Immutable |
| `Session` with only other options (`DryRun`, `QueryFields`, `Logger`, …) | Yes | Immutable |
| [`WithContext(ctx)`](https://pkg.go.dev/gorm.io/gorm#DB.WithContext), `Session` with `Context`, `PrepareStmt`, `SkipHooks` or `NewDB` | No | Immutable |
| `Session(&gorm.Session{Initialized: true})` | No (it takes a snapshot) | **Mutable** (`clone == 0`), even on `db` |
| `Session(cfg)` where `cfg` is not a `&gorm.Session{...}` literal | Assumed yes | Assumed **mutable** |

gormreuse reads the `&gorm.Session{...}` literal at the call:

- An option set to a non-constant value, or set in only some branches, is unknown. gormreuse assumes the worst case.
- A `Context` set to anything other than `nil` counts as set, as for `WithContext(ctx)`.

</details>

### Subqueries and condition arguments

GORM builds a subquery from a copy of the argument's `Statement`. So passing a `*gorm.DB` as an argument never changes it. gormreuse treats such an argument as a **read** of its root. You can pass one subquery to several queries. But GORM reads it when the **outer query runs**, here at `x.Find`. A change to the argument before that, by a branch or by reassignment, is reported:

```go
sub := db.Model(&User{}).Select("id")
x := db.Where("id IN (?)", sub)

// changes sub in place
sub.Where("age > 18").Find(&adults)

// VIOLATION: the subquery has age > 18
x.Find(&users)
```

To fix it, end the argument's chain with `.Session(&gorm.Session{})` before you pass it.

<details>
<summary>More examples, the read-only parameters, and wrappers</summary>

<table>
<tr><th>Code</th><th>Reported?</th><th>Why</th></tr>
<tr><td>

```go
// sub is mutable
sub := db.Model(&User{}).Select("id")

// OK: reads sub
db.Where("id IN (?)", sub).Find(&users)

// OK: reads sub again
db.Where("id NOT IN (?)", sub).Find(&others)

// OK: first branch of sub
sub.Find(&ids)
```

</td><td>No</td><td>

You can pass one subquery to several queries.

</td></tr>
<tr><td>

```go
sub := db.Model(&User{}).Select("id")

// first branch: changes sub in place
sub.Where("age > 18").Find(&adults)

// VIOLATION: the subquery has age > 18
db.Where("id IN (?)", sub).Find(&users)
```

</td><td><b>Yes</b></td><td>

`sub` changed before the outer query ran.

</td></tr>
</table>

The second kind of violation:

- is reported once per call that takes the argument
- is reported at the first use of the outer chain after the change
- carries no suggested fix

A read holds for these methods and parameters only. A `*gorm.DB` passed anywhere else, such as `Model(sub)` or a `Find` destination, is still a branch.

| Methods | Read-only parameters |
| --- | --- |
| `Where`, `Or`, `Not`, `Having`, `Joins`, `InnerJoins`, `Table`, `Select`, `Distinct`, `Raw`, `Exec` | Every parameter, including group conditions (`db.Where(sub)`) and `Joins("Company", cond)` |
| `Find`, `First`, `Last`, `Take`, `Delete`, `FirstOrInit`, `FirstOrCreate` | The conditions after the first parameter |
| `Update`, `UpdateColumn` | The value |

A subquery inside one of these wrappers, passed to a read-only parameter, is a read too:

| Wrapper | Example |
| --- | --- |
| `gorm.Expr` | `db.Where("id IN (?)", gorm.Expr("?", sub))` |
| `sql.Named` | `db.Where("id IN (@s)", sql.Named("s", sub))` |
| A map literal | `db.Where("id IN (@s)", map[string]any{"s": sub})` |
| A `clause.Expr` literal, or a pointer to one | `db.Where(clause.Expr{SQL: "id IN (?)", Vars: []any{sub}})` |

The wrapper may be kept in a variable, and wrappers may nest. But each use of the wrapper must be such an argument. A wrapper may also be used another way, such as `keep(e)`, `m["s"]` or `e.Vars`. Then it may hand the subquery to other code, so the subquery is a branch again.

</details>

### Scopes / Preload callbacks

The [`*gorm.DB`](https://pkg.go.dev/gorm.io/gorm#DB) passed to a [`Scopes`](https://pkg.go.dev/gorm.io/gorm#DB.Scopes) / [`Preload`](https://pkg.go.dev/gorm.io/gorm#DB.Preload) callback is a **mid-chain (mutable) value**. So reusing it inside the callback is a violation, as for any other mutable value:

```go
db.Scopes(func(tx *gorm.DB) *gorm.DB {
    // OK: first branch from tx
    tx.Where("a").Find(&users)

    // VIOLATION: second branch from tx
    return tx.Where("b")
})
```

### Parameters are mutable

A [`*gorm.DB`](https://pkg.go.dev/gorm.io/gorm#DB) **parameter** is a mutable root. A caller may pass a mid-chain value, so branching a parameter interferes exactly as branching a local would:

```go
func helper(tx *gorm.DB) *gorm.DB {
    // OK: first branch from tx
    tx.Where("a").Find(&users)

    // VIOLATION: second branch from tx
    return tx.Where("b")
}
```

To opt a helper out, annotate it with [`//gormreuse:immutable-param`](#gormreuseimmutable-param). The caller is then responsible for passing an isolated value. The callbacks of `Transaction` and `FindInBatches` get a fresh `tx`, so they are exempt. A `Connection` callback is not.

<details>
<summary>Which values are safe to reuse (GORM's <code>clone</code> field)</summary>

GORM's internal `clone` field decides whether reuse is safe:

- A handle with `clone > 0` forks a **fresh** `Statement` on the next chain call, so it is safe to reuse.
- A mid-chain handle (`clone == 0`) shares its `Statement`.
- A parameter's `clone` value cannot be known statically, so it is treated as mutable.

| Source | `clone` | Reusable? |
| --- | --- | --- |
| [`gorm.Open`](https://pkg.go.dev/gorm.io/gorm#Open), `Session`, `WithContext`, `Debug` | `> 0` | ✅ forks a fresh Statement (but see [Session and Debug copies](#session-and-debug-copies)) |
| `Session(&gorm.Session{Initialized: true})` | `== 0` | ❌ shares Statement |
| `Begin` result, `Transaction` callback `tx` | `> 0` | ✅ fresh transaction handle |
| [`FindInBatches`](https://pkg.go.dev/gorm.io/gorm#DB.FindInBatches) callback `tx` | `> 0` | ✅ fresh handle per batch |
| Mid-chain (`Where`, `Order`, …) | `== 0` | ❌ shares Statement |
| [`Connection`](https://pkg.go.dev/gorm.io/gorm#DB.Connection) callback `tx` | `== 0` | ❌ shares Statement |
| A `*gorm.DB` **parameter** | unknown | ❌ treated as mutable |
| The result of a helper, a func-typed value (`f(db)`) or an interface method (`repo.DB()`), alone or in a tuple such as `(*gorm.DB, error)` | unknown | ❌ treated as mutable, unless the helper is [`//gormreuse:immutable-return`](#gormreuseimmutable-return), declared or [inferred](#inferred-immutable-return) |

A repository interface whose method returns a fresh handle is reported when its result is branched twice. No directive applies to an interface method. Isolate the result with `repo.DB().Session(&gorm.Session{})`.

</details>

<details>
<summary>Transaction, FindInBatches and Connection callbacks in detail</summary>

Callbacks of `Transaction` and `FindInBatches` are exempt, also when the call runs under `go` or `defer`. An example is `tx` in `db.Transaction(func(tx *gorm.DB) error {...})`. Their `tx` is a fresh handle. A `Connection` callback is **not** exempt. gorm passes it a `clone == 0` handle, so branching its `tx` twice is reported. Declare your own such helpers with [`//gormreuse:immutable-input(name)`](#gormreuseimmutable-inputname).

A **named** function or method is exempt only when every use of it in the package passes it as such a callback. Otherwise its parameter is treated as mutable like any other parameter. That is the case when it is also called directly, stored, returned, passed as an ordinary argument, or exported (another package could call it). The packages that import an exported function are analyzed after its own package, so its own package cannot see their calls. To keep it exempt, mark it [`//gormreuse:immutable-param`](#gormreuseimmutable-param). Then each direct caller, in its package or in another one, must pass an isolated value.

The callback may also be read from a variable or a field that holds one function:

| Holder | Example | Condition |
| --- | --- | --- |
| Package-level variable | `var txBody = func(tx *gorm.DB) error {...}`, then `db.Transaction(txBody)` | Unexported, assigned only in its declaration, and its address is never taken |
| Struct field | `s := &svc{cb: func(tx *gorm.DB) error {...}}`, then `db.Transaction(s.cb)` | Unexported, assigned in one place in the package, its address is never taken, and its struct is never converted to another type |

- The function stays exempt only while every read of the variable or field passes it as such a callback.
- A read that calls it (`txBody(q)`) or passes it on as an ordinary argument makes its parameter mutable again.
- The same holds for an `immutable-input` callback.
- A call of `Transaction` or `FindInBatches` through a method value (`tx := db.Transaction; tx(cb)`) exempts its callback too.

</details>

### Closures

A use inside a closure counts where the closure is called.

| How the closure runs | Its uses |
| --- | --- |
| One plain call site, or called in place (`func() { ... }()`) | Uses on paths that cannot both run are not reported |
| Called from more than one place, deferred, run with `go`, passed to a function, or stored | It may run at any time. A use inside it conflicts with every other use of the same root |

<details>
<summary>Examples: captured variables and factories</summary>

<table>
<tr><th>Code</th><th>Reported?</th><th>Why</th></tr>
<tr><td>

```go
q := db.Where("x")
f := func() { q.Find(&users) }
if condition {
    // OK: first branch from q
    f()
} else {
    // OK: f() never runs on this path
    q.Count(&count)
}
```

</td><td>No</td><td>

The two uses are on paths that cannot both run.

</td></tr>
<tr><td>

```go
db = db.WithContext(ctx)
db.Find(&users)

// OK: reads the WithContext result
func() { db.Count(&count) }()
```

</td><td>No</td><td>

A closure reads a captured variable as it is at the closure's calls. So rebinding a parameter before the closure runs makes it safe to use there.

</td></tr>
<tr><td>

```go
db = db.WithContext(ctx)
for _, id := range ids {
    // OK: reads the WithContext result
    g.Go(func() error { return db.First(&User{}, id).Error })
}
```

</td><td>No</td><td>

Some closures may run at any point after they are created. That is a closure that is deferred, run with `go`, stored, passed on (errgroup's `Go`, a `Transaction` callback) or used as a range-over-func body. It reads the value the variable held when the closure was created, or any value stored later. The same applies to a field read through a captured pointer, such as `d := &deps{db: db}; d.db = d.db.WithContext(ctx)`.

</td></tr>
<tr><td>

```go
db = db.Session(&gorm.Session{})
base := func() *gorm.DB { return db.Model(&User{}) }

// OK
base().Where("c").Find(&users)

// OK: a new chain
base().Where("d").Find(&users)
```

</td><td>No</td><td>

A closure that builds a new chain from an immutable value on each call is a factory. Each call is its own root, whether it is called twice or in a loop.

</td></tr>
<tr><td>

```go
q := db.Where("x")

// VIOLATION: base runs twice
base := func() *gorm.DB { return q.Where("y") }
base().Find(&users)

// VIOLATION: second branch
base().Find(&admins)
```

</td><td><b>Yes</b></td><td>

The chain starts from a captured mutable root `q`. So every call branches `q`, and the second call is still reported.

</td></tr>
</table>

</details>

### Package-level variables

A package-level `*gorm.DB` follows the stores to it in the same function. A global that the function only reads is treated as immutable, because it usually holds a `gorm.Open` result.

<details>
<summary>Examples, package-level closures, and local function variables</summary>

```go
var Global *gorm.DB

func F(db *gorm.DB) {
    Global = db.Session(&gorm.Session{}).Where("tenant")

    // OK: first branch
    Global.Where("a").Find(&users)

    // VIOLATION: second branch from the stored chain
    Global.Where("b").Find(&users)
}
```

| Case | Rule |
| --- | --- |
| A global that the function only reads | Immutable. So a mutable chain assigned in another function, or in the variable's initializer (`var q = db.Where("x")`), is not reported when it is reused |
| The body of a package-level closure (`var f = func(db *gorm.DB) { ... }`) | Checked like any other closure. The directives on it are validated |
| A call through such an `f` | Honours those directives when `f` is unexported, assigned only in its declaration, and its address is never taken |
| A local variable that holds a function | Read the same way, also inside a closure that captures it, such as a closure that calls it or a recursive closure. A call through it honours the directives of the function it holds. The variable must be assigned exactly once, outside any closure, and its address must never be taken |

```go
//gormreuse:pure
peek := func(q *gorm.DB) { ... }
q := db.Where("x")
peek(q)

// OK: peek is pure
q.Find(&users)

// the capture does not change that
func() { peek(nil) }()
```

</details>

## Directives

A directive tells gormreuse what a function guarantees. gormreuse checks each guarantee:

| Directive | Write it on | Says | gormreuse checks |
| --- | --- | --- | --- |
| [`ignore`](#gormreuseignore) | A line, a function or a file | Suppress the diagnostics there | That it suppresses something |
| [`pure`](#gormreusepure) | A function or closure | It does not pollute its `*gorm.DB` arguments | The body |
| [`immutable-return`](#gormreuseimmutable-return) | A function or closure | It returns an immutable `*gorm.DB` | The body |
| [`immutable-param`](#gormreuseimmutable-param) | A function or closure | Its `*gorm.DB` parameters are immutable | Every caller passes an isolated value |
| [`immutable-input(name)`](#gormreuseimmutable-inputname) | A function or closure | It passes an immutable `*gorm.DB` to callback `name` | The body |

Write `//gormreuse:name[,name...]` as a line comment: lowercase names, and no space anywhere in it. A reason may follow after `//`, as in `//gormreuse:ignore // reason here`.

<details>
<summary>Syntax rules, and directives across packages</summary>

- Only `//gormreuse:name[,name...]` is a directive. There is no space anywhere in it, including after a comma.
- `//` is the only reason separator.
- A directive on a generic function or method applies to every instance of it (`helper[int](q)`).
- A directive on a method also applies when the method is called through a method value (`f := r.helper; f(q)`) or a method expression (`Repo.helper(r, q)`).
- **Warning:** any other comment that starts with `gormreuse:` has no effect, and is reported with how to write it. One unknown name in a list drops the whole comment: `//gormreuse:pure,bogus` does not mark the function pure.

A directive on an exported function or method also applies in the packages that call it. The analysis of the declaring package checks the function's body. It passes the result on as an [analysis fact](https://pkg.go.dev/golang.org/x/tools/go/analysis#hdr-Modular_analysis_with_Facts), so the packages that call it do not read its source. This works with the `gormreuse` command, `go vet` and golangci-lint, which all analyze the dependencies first.

| Directive in the declaring package | In a package that calls the function |
| --- | --- |
| `pure` | Trusted, unless the body pollutes its argument. Which arguments the result may be is known too |
| `immutable-return` | Trusted, unless the body returns a mutable value |
| `immutable-param` | The caller must pass an isolated value, when the function branches the parameter |
| `immutable-input(name)` | The callback gets an immutable value, unless the body hands it a mutable one |

- A contract that the body breaks is reported in the declaring package. Its callers in that package still trust it, so the mistake is reported once.
- A caller in another package does not trust it, since the author of that package may never see the report.
- Another package also does not trust an `immutable-return` or `immutable-input(name)` function that keeps its contract only by relying on a broken one. That is one that returns its result, hands it to the callback, or hands the callback to it. This holds through any number of such functions.

</details>

### `//gormreuse:ignore`

Suppress warnings on a line, in a function, or in a file. It covers every kind of diagnostic: reuse violations, `pure` / `immutable-*` contract violations, redundant or unused directives, and the Scopes `Session()` warning.

```go
q := db.Where("active = ?", true)
q.Find(&users)
//gormreuse:ignore // intentional reuse for pagination
q.Count(&count)  // Suppressed
```

<details>
<summary>What each ignore covers, with examples</summary>

| Ignore | Write it | Covers |
| --- | --- | --- |
| Line | On its own line | That line and the next one. That is every line of the statement directly below |
| Line | After code on the same line | That line only. That is every line of the statement it is on |
| Function | In the function's doc comment | The whole declaration, its doc comment, and every closure inside it |
| File | Before the package declaration | The whole file |

- For `if`, `for`, `switch` and `select`, a line ignore covers only the header up to `{`.
- An ignore inside a closure covers the closure's statement only.
- An ignore only hides reports. gormreuse still checks the function. A `//gormreuse:pure` function that leaks its argument is still not trusted at its call sites, even when the leak report is ignored.
- An unused ignore is reported. See [Unused and redundant directives](#unused-and-redundant-directives).

A comment after code covers that line only:

```go
q.Count(&count) //gormreuse:ignore // Suppressed

// Still reported
q.Delete(&User{})
```

A function:

```go
//gormreuse:ignore
func legacyCode(db *gorm.DB) {
    // All violations in this function are suppressed
}
```

A file (place it before the package declaration):

```go
//gormreuse:ignore

package mypackage

// All violations in this file are suppressed
```

A multi-line statement is covered from any of its lines:

```go
//gormreuse:ignore
q.
    Where("y").
    Find(nil) // Suppressed
```

</details>

### `//gormreuse:pure`

Mark a function or closure as not polluting its [`*gorm.DB`](https://pkg.go.dev/gorm.io/gorm#DB) argument. Passing an argument to it is then not a branch. Without the directive, `helper(q)` branches `q`.

> [!WARNING]
> **Body contract.** A pure function must not call a chain method on an argument (`db.Where("x")`). It must not let an argument escape, or pass it to a function that is not pure. It may return a mutable value. So `q := withTenant(db, 1)` is still mutable.

```go
//gormreuse:pure
func withTenant(db *gorm.DB, tenantID int) *gorm.DB {
    return db.
        Session(&gorm.Session{}).
        Where("tenant_id = ?", tenantID)
}
```

<details>
<summary>Placement, calls without the directive, and the full contract</summary>

The directive also works on closures, on the next line or the same line:

```go
//gormreuse:pure
helper := func(q *gorm.DB) { _ = q }

helper2 := func(q *gorm.DB) { //gormreuse:pure
    _ = q
}
```

A same-line directive belongs to the innermost function whose `{` comes before it on the line. In `func outer(db *gorm.DB) { run(func(q *gorm.DB) { //gormreuse:pure`, only the closure is pure.

Without a directive, a call to your own function is treated like this:

| Call | Effect on the `*gorm.DB` argument `q` | Result |
| --- | --- | --- |
| `helper(q)`, result discarded or not a `*gorm.DB` | A branch of `q` | |
| `q = helper(q)`, result assigned back to `q` | Like `q = q.Where(...)`, also under a condition or in a loop | A new mutable root |
| `r := helper(q)` | A branch of `q` | A new mutable root, unless `helper` is [inferred immutable-return](#inferred-immutable-return) |

gormreuse validates that functions marked `//gormreuse:pure` actually satisfy the pure contract:

```go
//gormreuse:pure
func badPure(db *gorm.DB) {
    // ERROR: pure function pollutes *gorm.DB argument by calling Where
    db.Where("x")
}
```

| A pure function | Rule | Includes |
| --- | --- | --- |
| Must NOT call polluting methods ([`Where`](https://pkg.go.dev/gorm.io/gorm#DB.Where), [`Find`](https://pkg.go.dev/gorm.io/gorm#DB.Find), etc.) on [`*gorm.DB`](https://pkg.go.dev/gorm.io/gorm#DB) **arguments** | Reported | `go db.Find(nil)`, `defer db.Find(nil)`, a method value (`f := db.Find`), a method expression (`(*gorm.DB).Find(db, nil)`), and a closure the function creates (`func() { db.Find(nil) }()`). An argument reached through a pointer, slice, variadic or struct parameter (`(*pp).Find`, `dbs[0].Find`, `h.db.Find`). An argument returned from another pure function that returns its argument (`same(db).Find`, see below) |
| Must NOT let an argument escape | Reported | A channel send, a slice/array or map store, a store to a package-level variable, or a store through a pointer (`h.db = db`) |
| Must NOT pass an argument to a function that is not pure | Reported | Also a pure function that fails this contract itself |
| MAY call polluting methods on **immutable values** | Allowed | `db.Session(&gorm.Session{}).Where(...)` |
| MAY pass an argument to output functions | Allowed | `fmt`, `log` and `testing` |
| MAY use its receiver | Allowed | A method's receiver is not an argument, so `r.db.Where(...)` inside a pure method is fine |

Some pure functions are not trusted as pure at their call sites, so reuse after the call is reported too:

- A function that pollutes or leaks its argument.
- A function that passes its argument to a failing pure function.

Passing the argument to a function that is not marked pure is reported inside the pure function only.

A pure function MAY return a mutable [`*gorm.DB`](https://pkg.go.dev/gorm.io/gorm#DB). Callers must treat the return value as potentially mutable:

```go
// q is mutable!
q := withTenant(db, 1)

// OK: first branch
q.Find(&users)

// VIOLATION: second branch from mutable q
q.Count(&count)
```

A pure function MAY also return its argument. Its result is then the same value as the argument, so a branch from the result is a branch from the argument:

```go
//gormreuse:pure
func same(db *gorm.DB) *gorm.DB { return db }

q := db.Where("x")

// OK: first branch from q
same(q).Find(&users)

// VIOLATION: second branch from q
q.Count(&count)
```

A result that comes only from immutable values, like `withTenant`'s `db.Session(...).Where(...)`, is a new chain and does not share the argument. This holds for a pure function in another package too.

</details>

### `//gormreuse:immutable-return`

Mark a function as returning an **immutable** [`*gorm.DB`](https://pkg.go.dev/gorm.io/gorm#DB) (like builtin [`Session`](https://pkg.go.dev/gorm.io/gorm#DB.Session), [`WithContext`](https://pkg.go.dev/gorm.io/gorm#DB.WithContext)). Callers can then reuse its result freely. It also covers a `*gorm.DB` in a multi-value result, such as `func Open() (*gorm.DB, error)`. The body contract applies to that result. Most helpers need no directive: see [Inferred immutable-return](#inferred-immutable-return).

> [!WARNING]
> **Body contract.** A function that returns a provably *mutable* value is reported at the declaration. Put `Session` at the **end** of the chain (`globalDB.Where("x").Session(&gorm.Session{})`), not before a `Where`.

```go
//gormreuse:immutable-return
func GetDB() *gorm.DB {
    return globalDB.Session(&gorm.Session{})
}
```

<details>
<summary>A full example, and what the body check reports</summary>

```go
//gormreuse:immutable-return
func GetDB() *gorm.DB {
    return globalDB.Session(&gorm.Session{})
}

func useIt() {
    db := GetDB()
    db.Where("x").Find(&users)

    // OK: GetDB returns immutable
    db.Where("y").Find(&admins)
}
```

Without the check, gormreuse would trust a false directive, and silently allow unsafe reuse of the return value at every call site:

```go
//gormreuse:immutable-return // VIOLATION: returns mutable *gorm.DB
func GetDB() *gorm.DB {
    // trailing Where re-forks a fresh clone==0 Statement
    return globalDB.Session(&gorm.Session{}).Where("x")
}
```

Only provably mutable returns are reported:

| Returned value | Reported? |
| --- | --- |
| A value whose root is a gorm chain method like `Where`, or `Session(&gorm.Session{Initialized: true})` | Yes |
| The same, returned through an IIFE, a local closure, or a deferred closure that writes a named result | Yes |
| An unexported package-level variable whose every assignment in the package is mutable | Yes |
| A bare parameter, a value from an unmarked named helper, or a `Session` config gormreuse can't read | No (benefit of the doubt) |
| An exported or address-taken package-level variable | No (benefit of the doubt) |

</details>

#### Inferred immutable-return

A function or closure without the directive is treated as `immutable-return` when its body proves that every [`*gorm.DB`](https://pkg.go.dev/gorm.io/gorm#DB) it returns is immutable. So most connection helpers need no directive:

```go
func (r *repo) conn(ctx context.Context) *gorm.DB {
    // inferred: callers may branch the result twice
    return r.db.WithContext(ctx)
}
```

<details>
<summary>What can be inferred, and what turns inference off</summary>

```go
func (r *repo) conn(ctx context.Context) *gorm.DB {
    return r.db.WithContext(ctx)
}

func OpenTestConnection(cfg *gorm.Config) (db *gorm.DB, err error) {
    db, err = gorm.Open(sqlite.Open("test.db"), cfg)
    if err != nil {
        return nil, err
    }
    return db, nil
}

func useIt(r *repo, ctx context.Context) {
    q := r.conn(ctx)
    q.Where("a").Find(&users)

    // OK: conn is inferred immutable-return
    q.Where("b").Find(&admins)
}
```

Each `*gorm.DB` result, in a `(*gorm.DB, error)` result too, must be one of these on every path:

| Returned value | Example |
| --- | --- |
| A call whose **last** method is [`Session`](https://pkg.go.dev/gorm.io/gorm#DB.Session), [`WithContext`](https://pkg.go.dev/gorm.io/gorm#DB.WithContext) or [`Debug`](https://pkg.go.dev/gorm.io/gorm#DB.Debug) | `db.Where("x").Session(&gorm.Session{})` |
| A [`Begin`](https://pkg.go.dev/gorm.io/gorm#DB.Begin) or [`gorm.Open`](https://pkg.go.dev/gorm.io/gorm#Open) result | `db.Begin()` |
| `nil` | `return nil, err` |
| The result of another `immutable-return` function, declared or inferred, in this package or another | `return r.conn(ctx)` |
| A field read through a parameter or the receiver, or a package-level variable, as reuse detection reads them where they are used | `return r.db` |
| A local variable, such as one a closure captures, whose every assignment is in the function and holds one of these values | `get := func() *gorm.DB { return conn }` |
| A type assertion of a value that holds no chain, as reuse detection reads it | `if tx, ok := ctx.Value(txKey).(*gorm.DB); ok { return tx }` |

So a getter is treated like the field it returns, **at each call**: the call reads the field of its own receiver. `d := r.DB(); d.Find(&a); d.Count(&n)` is not reported when `r` is a parameter, as `r.db.Find(&a); r.db.Count(&n)` is not. It is reported when `r := &repo{db: q.Where("x")}`, as `r.db` would be. A getter of a field or package-level variable is not inferred at all when code in its package assigns the variable a chain, such as `r.db = r.db.Where("x")`. A caller may call the getter after that.

A helper that returns a struct is inferred the same way when it builds the struct and every `*gorm.DB` field it fills in holds one of these values. Then `env := newTestEnv(); env.DB.Create(&u); env.DB.Find(&us)` is not reported. A struct the helper passes to another function, or did not build, turns inference off.

Anything else turns inference off for the function:

- a `*gorm.DB` parameter, a field of a struct built in the function, or a variable assigned in a closure, a deferred one included
- a func-typed value, or a call to an unmarked helper
- a chain after `Session` (`Session(...).Where(...)`)
- a `Session` config that is not a `&gorm.Session{...}` literal with `Initialized` unset or false

| Also note | Detail |
| --- | --- |
| Exported | An exported inferred function exports the same fact as a declared one, so other packages trust it too. A getter's fact names the field it reads, so a call in another package reads the field of its own receiver too |
| Recursion | A function that returns its own result is never inferred. Add the directive |
| Generated files | A function in a generated file is never inferred, since its body is not analyzed |
| Relying on a broken contract | A declared `immutable-return` function may break its contract. A function inferred only because it returns that function's result is trusted in its own package, like that function. Another package does not trust it |
| A declared directive on an inferred function | It is not reported as unused. It also documents the intent, and keeps the guarantee checked if the body changes |

</details>

### `//gormreuse:immutable-param`

Opt a function's [`*gorm.DB`](https://pkg.go.dev/gorm.io/gorm#DB) **parameters** out of the default mutable treatment. They are treated as immutable inside the function. The caller becomes responsible for passing an isolated value.

> [!WARNING]
> **Caller-side contract.** When the function actually branches such a parameter, passing a *mutable* `*gorm.DB` at a call site is reported. The suggested fix appends `.Session(&gorm.Session{})` to the argument.

```go
//gormreuse:immutable-param
func applyFilters(tx *gorm.DB) {
    // OK: tx is treated as immutable here
    tx.Where("a").Find(&users)

    // OK
    tx.Where("b").Find(&more)
}
```

<details>
<summary>The caller check in detail</summary>

The callee's internal branching would interfere, because the value is not isolated:

```go
func caller(root *gorm.DB) {
    // mutable
    q := root.Session(&gorm.Session{}).Where("x")

    // VIOLATION: mutable *gorm.DB passed to immutable-param parameter
    applyFilters(q)
}
```

Fix it by passing an isolated value (`root.Session(&gorm.Session{})`). Or make the caller `//gormreuse:immutable-param` too, so the contract propagates. See [Contract and Directive Fixes](#contract-and-directive-fixes) for the fix.

| Call | Caller contract checked? |
| --- | --- |
| Direct calls, function and method values, method expressions and generic instances | Yes |
| The same calls under `go` and `defer` | Yes |
| A function in another package | Yes |
| A call through an interface or a func-typed parameter | No. The callee is unknown |

**Tip:** combine with `//gormreuse:pure` when a helper both leaves its argument unpolluted and expects an isolated value.

A **redundant** `immutable-param` is reported. See [Unused and redundant directives](#unused-and-redundant-directives).

</details>

### `//gormreuse:immutable-input(name)`

Declare that the function passes an **immutable** [`*gorm.DB`](https://pkg.go.dev/gorm.io/gorm#DB) to its callback parameter `name`. It is a user-defined equivalent of gorm's [`Transaction`](https://pkg.go.dev/gorm.io/gorm#DB.Transaction)/[`FindInBatches`](https://pkg.go.dev/gorm.io/gorm#DB.FindInBatches). The named callback's `*gorm.DB` parameter is then treated as immutable, so reuse inside the callback is allowed.

> [!WARNING]
> **Body contract.** A function that hands the callback a mutable value is reported. So is a callback that escapes where gormreuse cannot follow it.

```go
//gormreuse:immutable-input(cb)
func withFreshDB(cb func(*gorm.DB) error, db *gorm.DB) error {
    // hands the callback an isolated value
    return cb(db.Session(&gorm.Session{}))
}
```

<details>
<summary>A caller, placement, the body check, and unused cases</summary>

```go
func useIt(db *gorm.DB) {
    _ = withFreshDB(func(q *gorm.DB) error {
        q.Find(&users)

        // OK: q is declared immutable input
        q.Count(&count)
        return nil
    }, db)
}
```

Write it where `pure` goes: in the function's doc comment, after its opening brace on the same line, or on the line before a closure:

```go
func withFresh(cb func(*gorm.DB) error, db *gorm.DB) error { //gormreuse:immutable-input(cb)
    return cb(db.Session(&gorm.Session{}))
}

//gormreuse:immutable-input(cb)
withTx := func(cb func(*gorm.DB) error) error { return cb(fresh) }
```

A mutable value handed to the callback is reported:

```go
//gormreuse:immutable-input(cb)
func bad(cb func(*gorm.DB) error, db *gorm.DB) error {
    q := db.Where("x")

    // VIOLATION: immutable-input(cb) declared but mutable *gorm.DB passed to callback
    return cb(q)
}
```

The check follows the callback wherever the function keeps it:

- `go cb(...)` and `defer cb(...)`
- a closure that captures it
- a local variable or its address
- a struct field, a slice, an array, a map, and `append`
- an interface it is boxed in

A `func(**gorm.DB)` callback called with `&q` is checked against every value stored in `q`.

Handing `cb` to another `immutable-input` function at its declared callback keeps the promise. So does handing it to gorm's `Transaction`/`FindInBatches`. That function may be called directly, through a method value, or through a package-level variable that holds it (`var withFresh = func(...)`). Any other flow cannot be checked, so it is reported:

| Flow | Message |
| --- | --- |
| Passed to a function, method or function value | `callback cb escapes (passed to forward); cannot verify the *gorm.DB it receives` |
| Returned, sent on a channel, stored in a package-level variable or through a pointer | `callback cb escapes (returned)` and so on |
| Called with a value that holds a `*gorm.DB` the checker cannot trace (a `**gorm.DB` parameter, a struct) | `cannot verify the *gorm.DB passed to callback` |

The directive is reported **unused** in these cases:

| Case | Note |
| --- | --- |
| `name` isn't a parameter, or isn't a function type | |
| The callback has no [`*gorm.DB`](https://pkg.go.dev/gorm.io/gorm#DB) parameter | |
| The directive is attached to no function or closure | |
| A variadic callback parameter (`cbs ...func(*gorm.DB)`) | Not supported. The message says `variadic callbacks are not supported`. The callbacks are checked like ordinary ones |

gorm's built-in `Transaction` and `FindInBatches` already pass a fresh handle to their callbacks. So they need no directive (see [Parameters are mutable](#parameters-are-mutable)). [`Connection`](https://pkg.go.dev/gorm.io/gorm#DB.Connection) does not: its callback gets a `clone == 0` handle. Do not put `//gormreuse:immutable-input` on a wrapper that forwards a Connection `tx` unchanged. Its body contract would be false.

</details>

### `//gormreuse:pure,immutable-return`

The recommended pattern for DB connection helpers. It combines both guarantees.

> [!WARNING]
> **Body contract.** Both contracts apply, so each one needs its own `Session`. The first one keeps `db` unpolluted, for `pure`. The last one isolates the result, for `immutable-return`.

```go
//gormreuse:pure,immutable-return
func GetTenantDB(db *gorm.DB, tenantID int) *gorm.DB {
    return db.
        // pure: Where runs on a copy, not on db
        Session(&gorm.Session{}).
        Where("tenant_id = ?", tenantID).
        // immutable-return: the result is isolated
        Session(&gorm.Session{})
}
```

<details>
<summary>A caller</summary>

```go
func useIt(db *gorm.DB) {
    tenant1 := GetTenantDB(db, 1)
    tenant2 := GetTenantDB(db, 2)

    // OK
    tenant1.Where("x").Find(&users)

    // OK
    tenant2.Where("y").Find(&admins)

    // OK: immutable-return
    tenant1.Count(&count)

    // OK: pure (db not polluted)
    db.Find(&all)
}
```

</details>

### Unused and redundant directives

A directive that does nothing is reported as a warning. Its fix deletes the unused name (see [Contract and Directive Fixes](#contract-and-directive-fixes)).

<details>
<summary>When each directive counts as unused</summary>

| Directive | Reported when |
| --- | --- |
| `pure`, `immutable-return`, `immutable-param` | Unused: the signature doesn't fit, e.g. `immutable-param` on a function with no [`*gorm.DB`](https://pkg.go.dev/gorm.io/gorm#DB) parameter |
| `immutable-param` | Redundant: the signature is valid, but its parameter is never reused, so it suppresses nothing |
| A combination such as `pure,immutable-return` | Never, while either part is used |
| `immutable-input(name)` | See [its unused cases](#gormreuseimmutable-inputname) |
| Line-level and function-level `ignore` | It suppresses nothing. A function-level ignore is unused when the function has nothing to suppress. This finds stale ignore comments |
| File-level `ignore` | Never |

No ignore suppresses an unused-ignore warning or a `gormreuse: internal error` report.

</details>

## Temporary rule: `Session`/`WithContext`/`Debug` returned from `Scopes` callbacks

> [!WARNING]
> This is a **temporary** rule working around an upstream GORM bug.

A [`Scopes`](https://pkg.go.dev/gorm.io/gorm#DB.Scopes) callback that **returns** the result of [`Session`](https://pkg.go.dev/gorm.io/gorm#DB.Session), [`WithContext`](https://pkg.go.dev/gorm.io/gorm#DB.WithContext), or [`Debug`](https://pkg.go.dev/gorm.io/gorm#DB.Debug) leaks the transaction of a create, update or delete. GORM begins it and never commits or rolls it back. See [go-gorm/gorm#7592](https://github.com/go-gorm/gorm/issues/7592). The fix attempt [#7593](https://github.com/go-gorm/gorm/pull/7593) was closed without merging.

```go
db.Scopes(func(q *gorm.DB) *gorm.DB {
    // WARNING: Session() result returned from Scopes callback causes transaction leak
    return q.Session(&gorm.Session{})
})
```

Only the returned value matters, and only a `Scopes` call whose chain may write is reported.

<details>
<summary>Why, and which callbacks and chains are reported</summary>

GORM runs the query on the value the callback returns. These three methods return a value that makes GORM store its "transaction started" flag on a copy of the statement. So the commit step never finds it. Every other chain method returns a value without this problem. So only the returned value matters:

| Inside a `Scopes` callback | Leaks | Reported |
| --- | --- | --- |
| `return q.Session(&gorm.Session{})` | Yes | Yes |
| `return other.Session(&gorm.Session{})` (any `*gorm.DB`) | Yes | Yes |
| `s := q.Session(...); s.Where("a").Find(&x); return s` | Yes | Yes |
| `return q.Session(&gorm.Session{}).Where("x")` | No | No |
| `other.Session(&gorm.Session{}).Find(&x); return q` | No | No |

`e2e/internal/scopes_session_test.go` checks each row against real GORM.

Only a create, update or delete begins the transaction that leaks. A `Scopes` call whose chain ends in a finisher that only reads is not reported:

| Chain | Leaks | Reported |
| --- | --- | --- |
| `db.Scopes(cb).Where("x").Find(&x)`, and the same with `First`, `Take`, `Last`, `FirstOrInit`, `Count`, `Pluck`, `Scan`, `Row`, `Rows` or `Exec` | No | No |
| `db.Scopes(cb).Delete(&u)`, `Create`, `Save`, `Update`, `Updates`, `FirstOrCreate`, … | Yes | Yes |
| A chain that is returned, stored, passed on or discarded | It may reach a write | Yes |

A callback is reported when one `Scopes` call that passes it may write. `e2e/internal/scopes_finisher_test.go` checks the finishers against real GORM. With `SkipDefaultTransaction`, GORM begins no transaction at all, but the rule does not read the config.

The rule finds the callback the same way reuse detection does:

- The callback may be a function literal, a named function, or a method value.
- It may also be a closure in a variable or a package-level variable, or in a slice built with `append`.
- The rule follows the returned value through variables, branches, and the returns of helpers and closures the callback calls.
- It reports the `Session`/`WithContext`/`Debug` call itself.

[`Preload`](https://pkg.go.dev/gorm.io/gorm#DB.Preload) callbacks are **unaffected** (Preload builds a fresh DB). So the same code inside a Preload callback is not flagged.

</details>

**Removal condition**: once the upstream fix ships in a supported GORM release, this rule is deleted. Removing the `internal/scopeswarn` package and the `Scopes warnings` block of `RunSSA` deletes it, as the package documents. Re-check go-gorm/gorm#7592 on each GORM version bump.

## Known Limitations

gormreuse reports a reuse only when it can follow the value. These cases are known. Each one has a test fixture, and [the implementation notes](./design/implementation.md#known-limitations) give the reasons.

| Kind | Cases | Meaning |
| --- | --- | --- |
| Missed reuse (false negatives) | 14 | Reuse that is not reported |
| Extra reports (false positives) | 5 | Safe code that is reported |
| Wrong position | — | A report on the wrong line |
| No fix | — | A report without a suggested fix |

<details>
<summary>Missed reuse (false negatives)</summary>

**Missed reuse (false negatives):**

<table>
<tr><th>Code</th><th>Why it is missed</th></tr>
<tr><td>

A closure assigns a captured variable:

```go
f := func() { q = db.Where("x") }
f()
```

</td><td>The assignment inside the closure is not followed</td></tr>
<tr><td>

```go
c := q.Session(&gorm.Session{})
defer c.Count(nil)
q.Where("a").Find(nil)
```

</td><td>The use of the copy is ordered at the <code>defer</code> statement, before the change</td></tr>
<tr><td>

`defer` inside a `go` closure, or `defer` → `go` → `defer`, with the other use after the statement

</td><td>These nested calls are not followed</td></tr>
<tr><td>

A method value that a branch picks:

```go
if c {
    find = q.Find
} else {
    find = q.Find
}
```

</td><td>A method value merged from branches is not traced to <code>q</code></td></tr>
<tr><td>

A closure passed once to a function that calls it many times:

```go
slices.SortFunc(s, func(a, b T) int {
    q.Find(nil)
    ...
})
```

</td><td>Only the call sites in your code are counted. A closure passed to another function outside a loop is taken to run once</td></tr>
<tr><td>

A closure called twice through a slice or a struct field:

```go
fs := []func(){f}
fs[0]()
fs[0]()
```

</td><td>A call through the container is not a call site of the closure</td></tr>
<tr><td>

A variable reassigned and used in a loop:

```go
for … {
    q = q.Where(c)
    q.Find(nil)
}
```

</td><td>The reassignment is not a branch, and the next iteration's <code>Where</code> is not checked against <code>Find</code></td></tr>
<tr><td>

```go
if c { q2 = q.Where("a") }
if d { q = q2 }
q.Count(nil)
```

</td><td><code>q2</code> can flow back into <code>q</code>, so <code>Where("a")</code> is treated as an assignment of <code>q</code></td></tr>
<tr><td>

A field rebuilt in a loop after it was copied:

```go
p.db = q
for … { p.db = p.db.Where(c) }
q.Find(nil)
```

</td><td>In a loop, the rebuild is an assignment, so its first read of <code>q</code> is not a branch</td></tr>
<tr><td>

A pure function that returns `db.Session(...)` of its argument

</td><td>The copy is not the argument, so the shared <code>Statement</code> is hidden from the caller</td></tr>
<tr><td>

A package-level `*gorm.DB` that another function, a closure or the initializer assigned

</td><td>Only the stores in the same function are followed. A global that the function only reads is immutable</td></tr>
<tr><td>

A struct field read through a parameter or a receiver (`s.db`, `r.db`), or a getter of it (`r.DB()`)

</td><td>The field is not a mutable root. Only the stores to it in the same function are followed, and for a getter, also the stores in its package</td></tr>
<tr><td>

`immutable-param` called through an interface or a func-typed parameter

</td><td>The callee is unknown, so the caller contract is not checked</td></tr>
<tr><td>

A method expression of a method promoted through an embedded `*gorm.DB`: `W.Find(w, nil)`

</td><td>The receiver <code>w</code> is not followed to its embedded field</td></tr>
</table>

</details>

<details>
<summary>Extra reports (false positives)</summary>

**Extra reports (false positives):**

<table>
<tr><th>Code</th><th>Why it is reported</th></tr>
<tr><td>

`pair` returns two new chains:

```go
a, b := pair(db)
a.Find(nil)
b.Find(nil)
```

</td><td>Both results of one call share the call as their root</td></tr>
<tr><td>

For a global or a field `G`:

```go
G = db.Where("x")
if c { G = G.Where("a") }
G.Find(nil)
```

</td><td>A store is not a merge point, so <code>Where("a")</code> counts as a branch of the stored root</td></tr>
<tr><td>

`mk` returns a new struct:

```go
p := mk(s)
go func() { p.db.Find(nil) }()
p = mk(s)
p.db.Count(nil)
```

</td><td>A pointer variable that a closure captures and that is reassigned may point to either struct. So the last read is matched to the first one too</td></tr>
<tr><td>

A recursive helper whose every path ends in `Session(...)`, branched twice

</td><td>

A function that returns its own result is never [inferred](#inferred-immutable-return) `immutable-return`. Add the directive

</td></tr>
<tr><td>

A closure called on exclusive branches, once directly and once inside another closure, where `run` calls `f`:

```go
if c { f() } else { run() }
```

</td><td>Calls in two functions are taken to both run</td></tr>
</table>

</details>

<details>
<summary>Wrong position, and no fix</summary>

**Wrong position:** a use inside a deferred closure runs at function exit. gormreuse orders it by its source position. So `defer func() { q.Count(nil) }(); q.Find(nil)` is reported at `q.Find`, which runs first.

**No fix:** a `go` or `defer` statement that changes the root in place, where another use may see the change, gets no suggested fix. That is `go q.Where("a")` or `go applyTenant(q)`, and `defer q.Where("a")` or `defer applyTenant(q)` with another `defer` that uses `q`, or in a loop. A Session at the root would make the call change a copy. Isolate the root by hand where the change is not needed. A root whose `Statement` may carry `Attrs`, `Assign` or `InstanceSet` state, such as `db.Model(&User{}).Assign(u)`, gets no Session either: a Session drops that state, so `FirstOrInit` and `FirstOrCreate` would change their result. Build each branch from a fresh chain instead.

</details>

## Documentation

| Document | Contents |
| --- | --- |
| [CHANGELOG.md](./CHANGELOG.md) | What changes in each release, and what to do about new diagnostics |
| [AGENTS.md](./AGENTS.md) | Repository instructions for contributors and coding agents |
| [design/implementation.md](./design/implementation.md) | How the analysis works, design decisions, and limitations |

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
