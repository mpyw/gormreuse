# gormreuse

[![Go Reference](https://pkg.go.dev/badge/github.com/mpyw/gormreuse.svg)](https://pkg.go.dev/github.com/mpyw/gormreuse)
[![CI](https://github.com/mpyw/gormreuse/actions/workflows/ci.yml/badge.svg)](https://github.com/mpyw/gormreuse/actions/workflows/ci.yml)
[![Codecov](https://codecov.io/gh/mpyw/gormreuse/graph/badge.svg)](https://codecov.io/gh/mpyw/gormreuse)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

A Go linter that detects unsafe [`*gorm.DB`](https://pkg.go.dev/gorm.io/gorm#DB) instance reuse after chain methods.

## Background

[GORM](https://pkg.go.dev/gorm.io/gorm)'s Traditional API chain methods ([`Where`](https://pkg.go.dev/gorm.io/gorm#DB.Where), [`Order`](https://pkg.go.dev/gorm.io/gorm#DB.Order), etc.) modify internal state. Reuse the same [`*gorm.DB`](https://pkg.go.dev/gorm.io/gorm#DB) instance after a chain method, and the query conditions can pile up.

<table>
<tr><th>Bug: <code>users.go</code></th><th>Fix: <code>gormreuse -fix</code> changes line 9</th></tr>
<tr><td>

```go
package users

import "gorm.io/gorm"

type User struct{ ID int }

func List(db *gorm.DB) {
	var users, admins []User
	q := db.Where("active = ?", true)
	q.Find(&users)  // SELECT * FROM users WHERE active = true
	q.Find(&admins) // Bug: conditions accumulate unexpectedly
}
```

</td><td>

```go
package users

import "gorm.io/gorm"

type User struct{ ID int }

func List(db *gorm.DB) {
	var users, admins []User
	q := db.Where("active = ?", true).Session(&gorm.Session{})
	q.Find(&users)  // OK: q is immutable
	q.Find(&admins) // OK: a fresh chain from q
}
```

</td></tr>
</table>

```console
$ gormreuse ./...
users.go:11:8: *gorm.DB reused: second branch from mutable root (root at users.go:9, first branch at users.go:10); make the root immutable with .Session(&gorm.Session{})
```

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
q.Count(&n) // reported: second branch from q
```

</td></tr>
<tr><td>

**[One use that runs more than once](#examples)**

</td><td>

```go
q := db.Where("x")
for _, id := range ids {
    // reported: runs on every iteration
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
c.Count(&n) // reported: c sees the change to q
```

</td></tr>
<tr><td>

**[A subquery changed before the outer query runs](#subqueries-and-condition-arguments)**

</td><td>

```go
sub := db.Model(&User{}).Select("id")
x := db.Where("id IN (?)", sub)
sub.Where("a").Find(&y)
x.Find(&z) // reported: the subquery has Where("a")
```

</td></tr>
<tr><td>

**[A directive whose contract the code breaks](#directives)**

</td><td>

```go
//gormreuse:pure
func scope(db *gorm.DB) {
    db.Where("x") // reported: pollutes the argument
}
```

</td></tr>
<tr><td>

**[A `Session` result returned from a `Scopes` callback whose chain may write (GORM bug)](#temporary-rule-sessionwithcontextdebug-returned-from-scopes-callbacks)**

</td><td>

```go
db.Scopes(func(q *gorm.DB) *gorm.DB {
    // reported: leaks the transaction
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

### Contents

| Section | Read it to |
| --- | --- |
| [Installation & Usage](#installation--usage) | Install gormreuse, or run it in golangci-lint |
| [Flags](#flags) | See the command's flags and exit status |
| [Detection Model](#detection-model-mutable-branching) | Learn which code counts as reuse, with examples |
| [Temporary rule for `Scopes`](#temporary-rule-sessionwithcontextdebug-returned-from-scopes-callbacks) | Understand the transaction-leak warning |
| [Directives](#directives) | State what a helper guarantees, or suppress a report |
| [Automatic Fixes](#automatic-fixes) | See what `-fix` changes, and when it offers no fix |
| [Known Limitations](#known-limitations) | See the cases that are missed or reported by mistake |

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
> To prevent supply chain attacks, pin to a specific version tag instead of `@latest` in CI/CD pipelines (e.g., `@v0.24.1`).

<a id="mise"></a>
<details>
<summary><a href="https://mise.jdx.dev/"><img src="https://mise.jdx.dev/logo.svg" height="20" alt=""></a> Using <b>mise</b> (macOS/Linux/Windows), recommended</summary>

mise's `github` backend installs gormreuse directly from GitHub Releases. It needs no extra registry. It needs no Go toolchain either, because the binaries are prebuilt. Run it in the project root:

```bash
mise use "github:mpyw/gormreuse@0.24.1"
gormreuse ./...
```

It pins the version in the project's `mise.toml`. So every checkout and CI run the same one:

```toml
[tools]
"github:mpyw/gormreuse" = "0.24.1"
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

> [!NOTE]
> gormreuse's `go.mod` says `go 1.27.0`. So `go get -tool` raises the `go` line of your `go.mod` to `1.27.0` when it is lower. Use another install method if your module must stay on an older Go.

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
export VERSION=0.24.1
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

On Windows, download `gormreuse_${VERSION}_windows_${ARCH}.zip`. Extract `gormreuse.exe` somewhere on your `PATH`.

</details>

<a id="golangci-lint"></a>
<details>
<summary><a href="https://golangci-lint.run/"><img src="https://cdn.simpleicons.org/go" height="20" alt=""></a> Running inside golangci-lint (custom build)</summary>

gormreuse is not bundled with golangci-lint. But it ships a [module plugin](https://golangci-lint.run/plugins/module-plugins/). So you can build a golangci-lint binary that holds it.

| Step | File or command |
| --- | --- |
| 1. Name the plugin | `.custom-gcl.yml` |
| 2. Turn it on | `.golangci.yml` |
| 3. Build and run | `golangci-lint custom`, then `./custom-gcl run` |

1\. Write `.custom-gcl.yml`:

```yaml
version: v2.15.0  # the golangci-lint release to build
plugins:
  - module: github.com/mpyw/gormreuse
    import: github.com/mpyw/gormreuse/plugin
    version: v0.24.1
```

2\. Turn it on in `.golangci.yml`. gormreuse has no settings, and any key under `settings` is an error:

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

3\. Build and run it:

```bash
golangci-lint custom  # writes ./custom-gcl
./custom-gcl run ./...
```

Use golangci-lint's own options in place of the driver flags below:

| Driver flag | golangci-lint option |
| --- | --- |
| `-test=false` | `run.tests: false` |
| `-fix` | `--fix` |

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

| Run | Exit status |
| --- | --- |
| A diagnostic is reported | `3` |
| No diagnostic is reported | `0` |
| `-json` is set | `0` |
| `-fix`, or `-fix -diff` | `0` |

> [!NOTE]
> With `-fix`, and with `-fix -diff`, the command prints no diagnostics and exits with `0`. This holds even when some reports have no fix. This is how the [analysis driver](https://pkg.go.dev/golang.org/x/tools/go/analysis/singlechecker) works. Run the command again without `-fix` to see the reports that are left.

<details>
<summary>Which files are analyzed, and internal errors</summary>

| Files | Analyzed | Directives | `-fix` edits them |
| --- | --- | --- | --- |
| `*_test.go` | Unless `-test=false` | Checked | Yes |
| Generated (contains `// Code generated ... DO NOT EDIT.`) | Never. They cannot be opted in | Not checked, so not trusted. At any call, in the same package or another, a function in a generated file counts as unmarked. This holds whatever directive it carries | No |
| Files that import `"C"` | Yes. The header that cgo adds to its output does not count | Checked | No. Their diagnostics carry no fix. The offsets would be in cgo's output, not in your file |

| Where gormreuse fails | Report |
| --- | --- |
| Inside a function | `gormreuse: internal error analyzing <func>: ...` at that function. gormreuse does not trust the directives that the function's failed check was proving |
| In work that reads the whole package | `gormreuse: internal error in <stage> of package <path>: ...` at the package clause |

Please [open an issue](https://github.com/mpyw/gormreuse/issues) with the code. Set `GORMREUSE_DEBUG_PANIC=1` to get the stack trace instead.

</details>

## Detection Model: Mutable Branching

gormreuse reports when a **mutable [`*gorm.DB`](https://pkg.go.dev/gorm.io/gorm#DB) branches into multiple code paths**:

| # | Rule |
| --- | --- |
| 1 | **Immutable-returning methods** ([`Session`](https://pkg.go.dev/gorm.io/gorm#DB.Session), [`WithContext`](https://pkg.go.dev/gorm.io/gorm#DB.WithContext), [`Debug`](https://pkg.go.dev/gorm.io/gorm#DB.Debug), etc.) return an **immutable** instance. It can branch freely. |
| 2 | **State-only methods** ([`AddError`](https://pkg.go.dev/gorm.io/gorm#DB.AddError), [`DB`](https://pkg.go.dev/gorm.io/gorm#DB.DB), [`Get`](https://pkg.go.dev/gorm.io/gorm#DB.Get), [`InstanceGet`](https://pkg.go.dev/gorm.io/gorm#DB.InstanceGet), [`Callback`](https://pkg.go.dev/gorm.io/gorm#DB.Callback), [`Commit`](https://pkg.go.dev/gorm.io/gorm#DB.Commit), [`Rollback`](https://pkg.go.dev/gorm.io/gorm#DB.Rollback)) touch no clause. A call of one does not count at all. |
| 3 | **All other methods** on a mutable instance create a **branch** that consumes the instance. |
| 4 | A **second branch** from the same mutable root is a **violation**. |
| 5 | A **`Session`/`Debug` copy** of a mutable instance still reads that instance's `Statement`. Using the copy **after the instance changed** is a **violation** (see [Session and Debug copies](#session-and-debug-copies)). |
| 6 | A mutable instance passed as a **subquery or condition argument** (`db.Where("id IN (?)", sub)`) is **read**, not branched. Running the outer query **after the argument changed** is a **violation** (see [Subqueries and condition arguments](#subqueries-and-condition-arguments)). |

### Method Classification

> [!IMPORTANT]
> This linter analyzes the **Traditional API** ([`*gorm.DB`](https://pkg.go.dev/gorm.io/gorm#DB)) only. The Generics API ([`gorm.G[T]`](https://pkg.go.dev/gorm.io/gorm#G), available since v1.30.0) returns different types ([`Interface[T]`](https://pkg.go.dev/gorm.io/gorm#Interface), [`ChainInterface[T]`](https://pkg.go.dev/gorm.io/gorm#ChainInterface)). It is excluded from analysis automatically.

| Category | Methods | Effect on the receiver |
| --- | --- | --- |
| Immutable-returning | `Session`, `WithContext`, `Debug`, [`Open`](https://pkg.go.dev/gorm.io/gorm#Open), [`Begin`](https://pkg.go.dev/gorm.io/gorm#DB.Begin) | Return a new immutable instance (`clone > 0`). They read the receiver but do not branch it. `Session` and `Debug` have exceptions: see [Session and Debug copies](#session-and-debug-copies) |
| Transaction | [`Transaction`](https://pkg.go.dev/gorm.io/gorm#DB.Transaction) | Returns an `error`. Like `Begin`, it reads the receiver but does not branch it. Its callback gets a fresh `tx` (see [Parameters are mutable](#parameters-are-mutable)) |
| ToSQL | [`ToSQL`](https://pkg.go.dev/gorm.io/gorm#DB.ToSQL) | Returns a `string`. It builds SQL from a copy of the receiver's Statement. So it reads the receiver but does not branch it. A branch before it is reported |
| State-only | `AddError`, `DB`, `Get`, `InstanceGet`, `Callback`, `Commit`, `Rollback` | Touch no clause. So they neither read nor branch the receiver. See the notes below |
| All others | [`Where`](https://pkg.go.dev/gorm.io/gorm#DB.Where), [`Find`](https://pkg.go.dev/gorm.io/gorm#DB.Find), [`Count`](https://pkg.go.dev/gorm.io/gorm#DB.Count), [`Order`](https://pkg.go.dev/gorm.io/gorm#DB.Order), etc. | Create a branch from the receiver |

Notes on the state-only methods:

- `AddError` sets `db.Error`. A later chain of the same value inherits it and stops at it. That is what `AddError` is for, so it is not reported.
- `Commit` and `Rollback` end the transaction and pass its error to `AddError`.
- They return the receiver itself. So a chain on their result is a branch of the receiver.

<table>
<tr><th>Code</th><th>Result</th></tr>
<tr><td>

```go
q := db.Where("x")

// OK: Transaction reads q
_ = q.Transaction(func(tx *gorm.DB) error {
    return tx.Find(&users).Error
})

// OK: first branch from q
q.Find(&users)
```

</td><td>Not reported: <code>Transaction</code> reads <code>q</code></td></tr>
<tr><td>

```go
q := db.Where("x")

// OK: ToSQL reads q
_ = q.ToSQL(func(tx *gorm.DB) *gorm.DB {
    return tx.Find(&users)
})

// OK: first branch from q
q.Find(&users)
```

</td><td>Not reported: <code>ToSQL</code> reads <code>q</code></td></tr>
<tr><td>

```go
q := db.Where("x")

// first branch from q
q.Find(&users)

// reported: reads q after the branch
_ = q.ToSQL(func(tx *gorm.DB) *gorm.DB {
    return tx.Find(&admins)
})
```

</td><td><b>Reported</b>: a branch before <code>ToSQL</code></td></tr>
<tr><td>

```go
q := db.Where("x")

// OK: touches no clause
q.AddError(errors.New("bad input"))

// OK: first branch, stops at the error
q.Find(&users)
```

</td><td>Not reported: <code>AddError</code> does not count</td></tr>
<tr><td>

```go
q := db.Begin().Model(&User{})

// first branch from q
q.Create(&user)

// OK: Commit does not count
q.Commit()
```

</td><td>Not reported</td></tr>
<tr><td>

```go
q := db.Begin().Model(&User{})

// first branch from q
q.Create(&user)

// reported: Commit returns q itself
q.Commit().Find(&users)
```

</td><td><b>Reported</b>: a chain on the result of <code>Commit</code> is a second branch of <code>q</code></td></tr>
</table>

### Automatic Pollution Sources

gormreuse cannot follow a `*gorm.DB` everywhere. These count as a branch:

- an interface method call (`repo.Query(db)`)
- a channel send
- a slice or map store
- a gorm call on a value taken back out of an interface by a type assertion
- a call to a function that is not `//gormreuse:pure`

A subquery or condition argument is a [read](#subqueries-and-condition-arguments), not a branch.

<details>
<summary>Every pollution source, and how struct fields are followed</summary>

In each example, `q` is mutable. The marked line is reported because the line before it already branched `q`.

<table>
<tr><th>Operation</th><th>Example</th><th>Why</th></tr>
<tr><td>Interface method call</td><td>

```go
repo.Query(q)

// reported
q.Find(&users)
```

</td><td>

It can't be analyzed statically.

</td></tr>
<tr><td>Interface method call on an interface that holds <code>q</code> itself</td><td>

```go
var qi Querier = q
qi.Where("a")

// reported
q.Find(&users)
```

</td><td>

It is `q`'s own method call.

</td></tr>
<tr><td>Channel send</td><td>

```go
ch <- q

// reported
q.Find(&users)
```

</td><td>

It may be received and used elsewhere.

</td></tr>
<tr><td>Slice/Map storage, <code>append</code></td><td>

```go
qs = append(qs, q)

// reported
q.Find(&users)
```

</td><td>

It may be accessed elsewhere. A slice literal `[]*gorm.DB{q}` and a map store count too.

</td></tr>
<tr><td>A method value handed on</td><td>

```go
firstPage(q.Find)

// reported
q.Find(&users)
```

</td><td>

It may be called elsewhere. A struct field such as `job{find: q.Find}` counts too.

</td></tr>
<tr><td>Type assertion out of an interface</td><td>

```go
v := interface{}(q)
if g, ok := v.(*gorm.DB); ok {
    g.Find(&users)
}

// reported
q.Find(&users)
```

</td><td>

The conversion alone is not a branch. The gorm call on the asserted value <code>g</code> is.

</td></tr>
<tr><td>Non-pure function call</td><td>

```go
helper(q)

// reported
q.Find(&users)
```

</td><td>

Unless the function is marked `//gormreuse:pure`.

</td></tr>
<tr><td>The address of the variable, passed to a non-pure function</td><td>

```go
applyP(&q)

// reported
q.Find(&users)
```

</td><td>

The function may change `*p` in place. Or it may store another chain in it. A later read of `q` sees both.

</td></tr>
<tr><td>A struct that holds it, handed to a call</td><td>

```go
useDeps(deps{DB: q})

// reported
q.Find(&users)
```

```go
r := &repo{db: q}
r.active()

// reported
q.Find(&users)
```

</td><td>

The callee can reach it through the struct. This does not apply to:

- a pure function
- an inferred getter
- a `*gorm.DB` method
- a method of `gorm.Statement`
- a package that does not import gorm

</td></tr>
<tr><td>Struct field access</td><td>

```go
h := holder{db: db.Where("x")}
h.db.Find(&users)

// reported
h.db.Find(&users)
```

</td><td>

It traces back to the stored value.

</td></tr>
</table>

A struct field is followed in these forms:

<table>
<tr><th>Form</th><th>Example</th><th>Rule</th></tr>
<tr><td>A field of a struct literal or variable</td><td>

See "Struct field access" above.

</td><td>

Includes nested fields and fields reached through embedding.

</td></tr>
<tr><td>Methods promoted from an embedded <code>*gorm.DB</code></td><td>

```go
e := E{db.Where("x")}
e.Where("a")

// reported
e.Find(&users)
```

</td><td>

Followed like the field itself.

</td></tr>
<tr><td>A field of a struct that a function returned, by value or by pointer</td><td>

```go
s := mk(db)
s.DB.Where("a")

// reported
s.DB.Find(&users)
```

</td><td>

- Each field of each call is its own mutable root, like a function that returns `*gorm.DB`.
- A call inside a loop makes a new root on each iteration.
- A store to the field through the returned pointer replaces it, as for a local struct.
- No fix is suggested for it, because the `Session` belongs inside the function.
- A helper that builds the struct from safe handles, such as a test setup function, is [inferred](#inferred-immutable-return) `immutable-return`.
- Mark one the analyzer cannot prove with `//gormreuse:immutable-return`.

</td></tr>
<tr><td>A struct captured by a closure</td><td>

```go
h := &holder{db: db.Where("x")}
f := func() { h.db.Find(&users) }
f()

// reported
h.db.Find(&users)
```

</td><td>

Also through a pointer.

</td></tr>
</table>

More rules:

- A send, a store and `append` are branches like a call that takes the value.
- One in a loop is reported, as `for … { helper(q) }` is.
- One that may hand on any of several values branches each of them.
- A field read through a parameter or a receiver, by value or by pointer, is not a mutable root. Only the stores to it in the same function are followed.
- A getter that returns such a field is read the same way. It is the field of the receiver at each call (see [Inferred immutable-return](#inferred-immutable-return)).
- Simple struct literal storage (`_ = &S{db: q}`) without actual field usage does NOT pollute.
- A struct handed to a call is followed when the call gets the struct, its address or a copy. That includes a struct in an interface, and one used as a method receiver.
- A struct sent on a channel, stored in a slice or a map, or packed into a variadic argument is not followed.
- A field read sees only the stores that can run before it. So the builder idiom below is not reported:

<table>
<tr><th>Code</th><th>Result</th></tr>
<tr><td>

```go
func (r *repo) list() {
    r.db = r.db.Where("active")

    // OK: one use of r.db
    r.db.Find(&users)
}
```

</td><td>Not reported</td></tr>
<tr><td>

```go
func (r *repo) list() {
    r.db = r.db.Where("active")
    r.db.Find(&users)

    // reported: second use of r.db
    r.db.Count(&n)
}
```

</td><td><b>Reported</b></td></tr>
</table>

</details>

### Examples

In these examples, `db` is the value that `gorm.Open` returned. It is immutable. A `*gorm.DB` parameter is mutable (see [Parameters are mutable](#parameters-are-mutable)). So some of the safe examples are reported when `db` is a parameter.

| Pattern | Reported? |
| --- | --- |
| Two branches from a mutable value, finisher or not | Yes |
| Branches from a value whose chain **ends** in `Session` | No |
| A `Session` in the middle of the chain | Yes |
| One use in a loop, or in a closure that runs more than once | Yes |
| A variable reassigned before its next use | No |
| A chain kept in a different variable, then the original used again | Yes |

> [!IMPORTANT]
> **Chaining without reassignment is a violation!** Each statement using the same variable creates a separate branch. To fix it, reassign the result or use method chaining in a single expression.

<table>
<tr><th>Unsafe</th><th>Safe</th></tr>
<tr><td>

```go
q := db.Where("base")

// OK: first branch
q.Where("a")

// reported: second branch
q.Where("b")

// reported: third branch
q.Find(&users)
```

</td><td>

```go
// Option 1: Reassign each step
q := db.Where("base")
q = q.Where("a")
q = q.Where("b")

// OK: first branch from final q
q.Find(&users)
```

```go
// Option 2: Single chained expression
// OK: single chain
db.Where("base").
    Where("a").
    Where("b").
    Find(&users)
```

</td></tr>
</table>

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

// reported: second branch from q
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

// reported: second branch from q
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

// reported: second branch from q
where("b").Find(&users2)
```

</td><td><b>Yes</b></td><td>

A method value of a mutable value branches it on each call.

</td></tr>
<tr><td>

```go
q := db.Where("x")
find := q.Find

// reported: runs after q.Count
defer find(&users)
q.Count(&count)
```

</td><td><b>Yes</b></td><td>

`go` and `defer` are uses too, also through a method value. A deferred call runs when the function returns, after every other use.

</td></tr>
<tr><td>

```go
q := db.Where("x")
if condition {
    // OK: only one of the two runs
    defer q.Find(&users)
} else {
    defer q.Count(&count)
}
```

</td><td>No</td><td>

A deferred call conflicts only with uses that may run in the same call of the function. Uses on exclusive branches do not.

</td></tr>
<tr><td>

```go
q := b
if condition {
    q = a
}

// OK: first branch from a or b
q.Find(&users)

// reported: second branch from a when condition holds
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

// reported: second branch
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

// reported: second branch from q
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
    // reported: reused on every iteration
    q.Where("id = ?", v).Find(&users)
}
```

</td><td><b>Yes</b></td><td>

A loop body runs more than once. So one use inside it is reuse.

Range-over-func loops (Go 1.23+) count as loops too. As in a plain loop, a use followed by `break` or `return` runs once, also from a nested loop with `break outer`. A chain that the body assigns is seen after the loop.

</td></tr>
<tr><td>

```go
q := db.Where("x")

// reported: f runs twice
f := func() { q.Find(&users) }
f()
f()
for _, id := range ids {
    g.Go(func() error {
        // reported: one closure per iteration
        return q.Where("id = ?", id).Find(&users).Error
    })
}

// reported: run calls it in a loop
update := func(id int) { q.Where("id = ?", id).Find(&users) }
run := func() {
    for _, id := range ids {
        update(id)
    }
}
run()
```

</td><td><b>Yes</b></td><td>

So does a closure that runs more than once. That is a closure:

- called at two sites
- called, spawned, deferred or passed to another function inside a loop

A call inside another closure counts where that closure runs.

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
for _, it := range items {
    q := s.Model(&User{}).Where("id = ?", it.ID)
    if it.Skip {
        // OK: one query per iteration
        q.Delete(&User{})
        continue
    }
    q.Update("name", "x")
}
```

</td><td>No</td><td>

A root built in the loop body is a new root on each iteration. Uses on exclusive branches never share one.

Still reported:

- uses that both run in one iteration
- a root built before the loop

This holds when each use names the variable itself. A use through another variable, or inside a closure, may see the root of an earlier iteration. So it is checked as before.

</td></tr>
<tr><td>

```go
q := db.Model(&User{})
if org != nil {
    q = q.Where("org = ?", *org)
}
for _, id := range ids {
    // reported: the fix adds a Session to both roots
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

A variable reassignment creates a **new mutable root**. So the variable can be used fresh. gormreuse uses [SSA (Static Single Assignment)](https://pkg.go.dev/golang.org/x/tools/go/ssa) form. In it, each assignment creates a distinct value. So the new value has no relationship to the previous one.

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
q := db.Where("base")
if condition {
    // extends the same variable
    q = q.Where("alt")
}

// OK: not a second branch of q
q.Find(&users)
```

</td><td>No</td><td>

A reassignment that extends the same variable is not a second branch of `q`.

</td></tr>
<tr><td>

```go
q := db.Where("x")
var q2 *gorm.DB
if condition {
    // first branch from q
    q2 = q.Where("a")
}

// reported: second branch from q
q.Count(&count)
```

</td><td><b>Yes</b></td><td>

A chain assigned to a **different** variable is a branch. This holds under a condition, and when a closure captures it.

</td></tr>
</table>

Each safe example above branches `db` more than once. That is safe only because `db` is immutable. If `db` were a `*gorm.DB` parameter, the second branch of `db` would be reported:

```go
func list(db *gorm.DB) {
    q := db.Where("x")
    q.Find(&users)

    // reported: second branch of the parameter db
    q = db.Where("y")
    q.Find(&admins)
}
```

</details>

### Session and Debug copies

On a mutable value, [`Session(&gorm.Session{})`](https://pkg.go.dev/gorm.io/gorm#DB.Session) and [`Debug()`](https://pkg.go.dev/gorm.io/gorm#DB.Debug) return a copy that **points at the same `Statement`**.

- Using the copy does not change the original.
- A change to the original after the copy was taken shows up in every later use of the copy.
- A change by reassignment (`q = q.Where(...)`) counts too. It changes a mutable `Statement` in place.

To fix it, end the root's chain with `.Session(&gorm.Session{})` before you copy it. Or take the copy with an option that clones the `Statement`.

<table>
<tr><th>Unsafe</th><th>Safe</th></tr>
<tr><td>

```go
// q is mutable
q := db.Model(&User{}).Where("tenant = 1")

// countQ shares q's Statement
countQ := q.Session(&gorm.Session{})

// OK: q has not changed yet
countQ.Count(&before)

// first branch: changes q in place
q.Where("active = 1").Find(&users)

// reported: runs WHERE tenant = 1 AND active = 1
countQ.Count(&n)
```

</td><td>

```go
// q is immutable
q := db.Model(&User{}).Where("tenant = 1").
    Session(&gorm.Session{})

countQ := q.Session(&gorm.Session{})
countQ.Count(&before)
q.Where("active = 1").Find(&users)

// OK: q did not change
countQ.Count(&n)
```

```go
q := db.Model(&User{}).Where("tenant = 1")

// WithContext clones the Statement
countQ := q.WithContext(ctx)
countQ.Count(&before)
q.Where("active = 1").Find(&users)

// OK: countQ has its own Statement
countQ.Count(&n)
```

</td></tr>
</table>

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

GORM builds a subquery from a copy of the argument's `Statement`. So passing a `*gorm.DB` as an argument never changes it. gormreuse treats such an argument as a **read** of its root.

- You can pass one subquery to several queries.
- GORM reads it when the **outer query runs**.
- A change to the argument before that, by a branch or by reassignment, is reported.

To fix it, end the argument's chain with `.Session(&gorm.Session{})` before you pass it.

<table>
<tr><th>Unsafe</th><th>Safe</th></tr>
<tr><td>

```go
sub := db.Model(&User{}).Select("id")
x := db.Where("id IN (?)", sub)

// changes sub in place
sub.Where("age > 18").Find(&adults)

// reported: the subquery has age > 18
x.Find(&users)
```

</td><td>

```go
sub := db.Model(&User{}).Select("id").
    Session(&gorm.Session{})
x := db.Where("id IN (?)", sub)

// a new chain: sub does not change
sub.Where("age > 18").Find(&adults)

// OK
x.Find(&users)
```

</td></tr>
</table>

Here GORM reads `sub` at `x.Find`.

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

// reported: the subquery has age > 18
db.Where("id IN (?)", sub).Find(&users)
```

</td><td><b>Yes</b></td><td>

`sub` changed before the outer query ran.

</td></tr>
<tr><td>

```go
sub := db.Model(&User{}).Select("id")

// first branch: Model is not read-only
db.Model(sub).Find(&users)

// reported: second branch of sub
sub.Find(&ids)
```

</td><td><b>Yes</b></td><td>

A `*gorm.DB` passed outside the read-only parameters is still a branch.

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

<table>
<tr><th>Wrapper</th><th>Example</th></tr>
<tr><td><code>gorm.Expr</code></td><td>

```go
db.Where("id IN (?)", gorm.Expr("?", sub))
```

</td></tr>
<tr><td><code>sql.Named</code></td><td>

```go
db.Where("id IN (@s)", sql.Named("s", sub))
```

</td></tr>
<tr><td>A map literal</td><td>

```go
db.Where("id IN (@s)", map[string]any{"s": sub})
```

</td></tr>
<tr><td>A <code>clause.Expr</code> literal, or a pointer to one</td><td>

```go
db.Where(clause.Expr{SQL: "id IN (?)", Vars: []any{sub}})
```

</td></tr>
</table>

The wrapper may be kept in a variable, and wrappers may nest. But each use of the wrapper must be such an argument. A wrapper may also be used another way, such as `keep(e)`, `m["s"]` or `e.Vars`. Then it may hand the subquery to other code. So the subquery is a branch again.

<table>
<tr><th>Code</th><th>Reported?</th></tr>
<tr><td>

```go
sub := db.Model(&User{}).Select("id")
e := gorm.Expr("?", sub)

// OK: each use of e is a read-only argument
db.Where("id IN (?)", e).Find(&users)
db.Where("id NOT IN (?)", e).Find(&others)

// OK: first branch of sub
sub.Find(&ids)
```

</td><td>No</td></tr>
<tr><td>

```go
sub := db.Model(&User{}).Select("id")
e := gorm.Expr("?", sub)
db.Where("id IN (?)", e).Find(&users)

// first branch: keep may hand sub on
keep(e)

// reported: second branch of sub
sub.Find(&ids)
```

</td><td><b>Yes</b></td></tr>
</table>

</details>

### Scopes / Preload callbacks

The [`*gorm.DB`](https://pkg.go.dev/gorm.io/gorm#DB) passed to a [`Scopes`](https://pkg.go.dev/gorm.io/gorm#DB.Scopes) / [`Preload`](https://pkg.go.dev/gorm.io/gorm#DB.Preload) callback is a **mid-chain (mutable) value**. So reusing it inside the callback is a violation, as for any other mutable value:

```go
db.Scopes(func(tx *gorm.DB) *gorm.DB {
    // OK: first branch from tx
    tx.Where("a").Find(&users)

    // reported: second branch from tx
    return tx.Where("b")
})
```

### Parameters are mutable

A [`*gorm.DB`](https://pkg.go.dev/gorm.io/gorm#DB) **parameter** is a mutable root. A caller may pass a mid-chain value. So branching a parameter twice interferes, just like branching a local.

<table>
<tr><th>Unsafe</th><th>Safe</th></tr>
<tr><td>

```go
func helper(tx *gorm.DB) *gorm.DB {
    // OK: first branch from tx
    tx.Where("a").Find(&users)

    // reported: second branch from tx
    return tx.Where("b")
}
```

</td><td>

```go
//gormreuse:immutable-param
func helper(tx *gorm.DB) *gorm.DB {
    tx.Where("a").Find(&users)

    // OK: the caller passes an isolated value
    return tx.Where("b")
}

func caller(db *gorm.DB) {
    helper(db.Session(&gorm.Session{}))
}
```

</td></tr>
</table>

| Parameter | Treated as |
| --- | --- |
| A parameter of any function | Mutable |
| A parameter of a function marked [`//gormreuse:immutable-param`](#gormreuseimmutable-param) | Immutable. The caller is then responsible for passing an isolated value |
| A parameter of an unexported helper whose every caller already passes an isolated value | Immutable without a directive. It is [inferred](#inferred-immutable-param) |
| The `tx` of a `Transaction` or `FindInBatches` callback | Immutable. The callback gets a fresh `tx` |
| The `tx` of a `Connection` callback | Mutable |

<details>
<summary>Which values are safe to reuse (GORM's <code>clone</code> field)</summary>

GORM's internal `clone` field decides whether reuse is safe:

- A handle with `clone > 0` forks a **fresh** `Statement` on the next chain call, so it is safe to reuse.
- A mid-chain handle (`clone == 0`) shares its `Statement`.
- A parameter's `clone` value cannot be known statically, so it is treated as mutable.

<table>
<tr><th>Source</th><th>Example</th><th><code>clone</code></th><th>Reusable?</th></tr>
<tr><td>

[`gorm.Open`](https://pkg.go.dev/gorm.io/gorm#Open), `Session`, `WithContext`, `Debug`

</td><td>

```go
db, _ := gorm.Open(dialector)
db.Where("a").Find(&users)
db.Where("b").Find(&users) // OK
```

```go
s := db.Where("x").Session(&gorm.Session{})
s.Where("a").Find(&users)
s.Where("b").Find(&users) // OK
```

</td><td><code>&gt; 0</code></td><td>

✅ forks a fresh Statement (but see [Session and Debug copies](#session-and-debug-copies))

</td></tr>
<tr><td>

`Session(&gorm.Session{Initialized: true})`

</td><td>

```go
s := db.Session(&gorm.Session{Initialized: true})
s.Where("a").Find(&users)
s.Where("b").Find(&users) // reported
```

</td><td><code>== 0</code></td><td>❌ shares Statement</td></tr>
<tr><td>

`Begin` result, `Transaction` callback `tx`

</td><td>

```go
tx := db.Begin()
tx.Where("a").Find(&users)
tx.Where("b").Find(&users) // OK
```

</td><td><code>&gt; 0</code></td><td>✅ fresh transaction handle</td></tr>
<tr><td>

[`FindInBatches`](https://pkg.go.dev/gorm.io/gorm#DB.FindInBatches) callback `tx`

</td><td>

```go
db.FindInBatches(&users, 100, func(tx *gorm.DB, batch int) error {
    tx.Where("a").Find(&admins)
    return tx.Where("b").Find(&admins).Error // OK
})
```

</td><td><code>&gt; 0</code></td><td>✅ fresh handle per batch</td></tr>
<tr><td>

Mid-chain (`Where`, `Order`, …)

</td><td>

```go
q := db.Where("x")
q.Where("a").Find(&users)
q.Where("b").Find(&users) // reported
```

</td><td><code>== 0</code></td><td>❌ shares Statement</td></tr>
<tr><td>

[`Connection`](https://pkg.go.dev/gorm.io/gorm#DB.Connection) callback `tx`

</td><td>

```go
db.Connection(func(tx *gorm.DB) error {
    tx.Where("a").Find(&users)
    return tx.Where("b").Find(&users).Error // reported
})
```

</td><td><code>== 0</code></td><td>❌ shares Statement</td></tr>
<tr><td>

A `*gorm.DB` **parameter**

</td><td>

```go
func param(db *gorm.DB) {
    db.Where("a").Find(&users)
    db.Where("b").Find(&users) // reported
}
```

</td><td>unknown</td><td>❌ treated as mutable</td></tr>
<tr><td>

A value read from a slice, an array, a map or a channel (`m["k"]`, `qs[0]`, `range qs`, `<-ch`), or through a `**gorm.DB` parameter (`*pq`)

</td><td>

```go
qs := []*gorm.DB{db.Where("x")}
qs[0].Find(&users)
qs[0].Count(&count) // reported
```

```go
func ptr(pq **gorm.DB) {
    (*pq).Where("a").Find(&users)
    (*pq).Where("b").Find(&users) // reported
}
```

</td><td>unknown</td><td>

❌ treated as mutable when the container may hold a mutable value:

- one is stored, sent or appended, or
- the container is a parameter or a call result, or
- the container is handed to a function that may reach gorm.

Two reads of one element (`qs[0]` twice) share a root. A container in a struct field or a package-level variable is not followed.

</td></tr>
<tr><td>

The result of a helper, a func-typed value (`f(db)`) or an interface method (`repo.DB()`), alone or in a tuple such as `(*gorm.DB, error)`

</td><td>

```go
q := newQuery(db)
q.Find(&users)
q.Count(&count) // reported
```

</td><td>unknown</td><td>

❌ treated as mutable, unless the helper is [`//gormreuse:immutable-return`](#gormreuseimmutable-return), declared or [inferred](#inferred-immutable-return)

</td></tr>
</table>

A repository interface method may return a fresh handle. Its result is still reported when it is branched twice. No directive applies to an interface method. Isolate the result with `Session`:

<table>
<tr><th>Unsafe</th><th>Safe</th></tr>
<tr><td>

```go
q := repo.DB()
q.Where("a").Find(&users)
q.Where("b").Find(&users) // reported
```

</td><td>

```go
q := repo.DB().Session(&gorm.Session{})
q.Where("a").Find(&users)
q.Where("b").Find(&users) // OK
```

</td></tr>
</table>

</details>

<details>
<summary>Transaction, FindInBatches and Connection callbacks in detail</summary>

- Callbacks of `Transaction` and `FindInBatches` are exempt. Their `tx` is a fresh handle.
- A `Connection` callback is **not** exempt. gorm passes it a `clone == 0` handle. So branching its `tx` twice is reported.
- Declare your own such helpers with [`//gormreuse:immutable-input(name)`](#gormreuseimmutable-inputname).

<table>
<tr><th>Code</th><th>Result</th></tr>
<tr><td>

```go
go db.Transaction(func(tx *gorm.DB) error {
    tx.Where("a").Find(&users)
    return tx.Where("b").Find(&users).Error // OK
})
```

</td><td>Exempt, also when the call runs under <code>go</code> or <code>defer</code></td></tr>
<tr><td>

```go
t := db.Transaction
t(func(tx *gorm.DB) error {
    tx.Where("a").Find(&users)
    return tx.Where("b").Find(&users).Error // OK
})
```

</td><td>Exempt. A call of <code>Transaction</code> or <code>FindInBatches</code> through a method value exempts its callback too</td></tr>
</table>

A **named** function or method is exempt only when every use of it in the package passes it as such a callback. Otherwise its parameter is treated as mutable like any other parameter.

<table>
<tr><th>Code</th><th>Result</th></tr>
<tr><td>

```go
func body(tx *gorm.DB) error {
    tx.Where("a").Find(&users)
    return tx.Where("b").Find(&users).Error // OK
}

db.Transaction(body)
```

</td><td>Exempt. Its only use is as a callback</td></tr>
<tr><td>

```go
func body(tx *gorm.DB) error {
    tx.Where("a").Find(&users)
    return tx.Where("b").Find(&users).Error // reported
}

db.Transaction(body)
body(db.Where("x"))
```

</td><td>Mutable. It is also called directly</td></tr>
<tr><td>

```go
func Body(tx *gorm.DB) error {
    tx.Where("a").Find(&users)
    return tx.Where("b").Find(&users).Error // reported
}

db.Transaction(Body)
```

</td><td>Mutable. It is exported, so another package could call it</td></tr>
<tr><td>

```go
//gormreuse:immutable-param
func Body(tx *gorm.DB) error {
    tx.Where("a").Find(&users)
    return tx.Where("b").Find(&users).Error // OK
}

db.Transaction(Body)
```

</td><td>Exempt. Each direct caller, in its package or in another one, must pass an isolated value</td></tr>
</table>

- A named function also loses the exemption when it is stored, returned, or passed as an ordinary argument.
- The packages that import an exported function are analyzed after its own package. So its own package cannot see their calls. To keep it exempt, mark it [`//gormreuse:immutable-param`](#gormreuseimmutable-param).

The callback may also be read from a variable or a field that holds one function:

<table>
<tr><th>Holder</th><th>Example</th><th>Condition</th></tr>
<tr><td>Package-level variable</td><td>

```go
var txBody = func(tx *gorm.DB) error {...}

db.Transaction(txBody)
```

</td><td>Unexported, assigned only in its declaration, and its address is never taken</td></tr>
<tr><td>Struct field</td><td>

```go
s := &svc{cb: func(tx *gorm.DB) error {...}}

db.Transaction(s.cb)
```

</td><td>Unexported, assigned in one place in the package, its address is never taken, and its struct is never converted to another type</td></tr>
</table>

- The function stays exempt only while every read of the variable or field passes it as such a callback.
- A read that calls it (`txBody(q)`) or passes it on as an ordinary argument makes its parameter mutable again.
- The same holds for an `immutable-input` callback.

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
q := db.Where("x")
defer func() { q.Find(&users) }()
if condition {
    // reported: the deferred closure also runs
    q.Count(&count)
}
```

</td><td><b>Yes</b></td><td>

A deferred closure may run at any time. So its use conflicts with every other use of `q`.

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

Some closures may run at any point after they are created: deferred, run with `go`, stored, or passed on (errgroup's `Go`, a `Transaction` callback).

- Such a closure reads the value the variable held when it was created, or any value stored later.
- A range-over-func body runs only during its loop. So it reads only the values that can reach the loop.
- The same applies to a field read through a captured pointer:

  ```go
  d := &deps{db: db}
  d.db = d.db.WithContext(ctx)
  ```

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

// reported: base runs twice
base := func() *gorm.DB { return q.Where("y") }
base().Find(&users)

// reported: second branch
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

<table>
<tr><th>Code</th><th>Result</th></tr>
<tr><td>

```go
var Global *gorm.DB

func F(db *gorm.DB) {
    Global = db.Session(&gorm.Session{}).Where("tenant")

    // OK: first branch
    Global.Where("a").Find(&users)

    // reported: second branch from the stored chain
    Global.Where("b").Find(&users)
}
```

</td><td>Reported. <code>F</code> stores a mutable chain in <code>Global</code></td></tr>
<tr><td>

```go
var q = Global.Where("x")

func G() {
    q.Find(&users)
    q.Count(&count) // not reported
}
```

</td><td>Not reported. <code>G</code> only reads <code>q</code></td></tr>
</table>

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

## Temporary rule: `Session`/`WithContext`/`Debug` returned from `Scopes` callbacks

> [!WARNING]
> This is a **temporary** rule working around an upstream GORM bug.

A [`Scopes`](https://pkg.go.dev/gorm.io/gorm#DB.Scopes) callback may **return** the result of [`Session`](https://pkg.go.dev/gorm.io/gorm#DB.Session), [`WithContext`](https://pkg.go.dev/gorm.io/gorm#DB.WithContext), or [`Debug`](https://pkg.go.dev/gorm.io/gorm#DB.Debug). Then the transaction of a create, update or delete leaks. GORM begins it and never commits or rolls it back. See [go-gorm/gorm#7592](https://github.com/go-gorm/gorm/issues/7592). The fix attempt [#7593](https://github.com/go-gorm/gorm/pull/7593) was closed without merging.

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

<table>
<tr><th>Inside a <code>Scopes</code> callback</th><th>Leaks</th><th>Reported</th></tr>
<tr><td>

```go
return q.Session(&gorm.Session{})
```

</td><td>Yes</td><td>Yes</td></tr>
<tr><td>

```go
// any *gorm.DB
return other.Session(&gorm.Session{})
```

</td><td>Yes</td><td>Yes</td></tr>
<tr><td>

```go
s := q.Session(&gorm.Session{})
s.Where("a").Find(&x)
return s
```

</td><td>Yes</td><td>Yes</td></tr>
<tr><td>

```go
return q.Session(&gorm.Session{}).Where("x")
```

</td><td>No</td><td>No</td></tr>
<tr><td>

```go
other.Session(&gorm.Session{}).Find(&x)
return q
```

</td><td>No</td><td>No</td></tr>
</table>

`e2e/internal/scopes_session_test.go` checks each row against real GORM.

Only a create, update or delete begins the transaction that leaks. A `Scopes` call whose chain ends in a finisher that only reads is not reported:

<table>
<tr><th>Chain</th><th>Leaks</th><th>Reported</th></tr>
<tr><td>

```go
db.Scopes(cb).Where("x").Find(&x)
```

The same with `First`, `Take`, `Last`, `FirstOrInit`, `Count`, `Pluck`, `Scan`, `Row`, `Rows` or `Exec`

</td><td>No</td><td>No</td></tr>
<tr><td>

```go
db.Scopes(cb).Delete(&u)
```

The same with `Create`, `Save`, `Update`, `Updates`, `FirstOrCreate`, …

</td><td>Yes</td><td>Yes</td></tr>
<tr><td>

```go
return db.Scopes(cb)
```

Any chain that is returned, stored, passed on or discarded

</td><td>It may reach a write</td><td>Yes</td></tr>
</table>

- A callback is reported when one `Scopes` call that passes it may write.
- `e2e/internal/scopes_finisher_test.go` checks the finishers against real GORM.
- With `SkipDefaultTransaction`, GORM begins no transaction at all. But the rule does not read the config.

The rule finds the callback the same way reuse detection does:

- The callback may be a function literal, a named function, or a method value.
- It may also be a closure in a variable or a package-level variable, or in a slice built with `append`.
- The rule follows the returned value through variables, branches, and the returns of helpers and closures the callback calls.
- It reports the `Session`/`WithContext`/`Debug` call itself.

[`Preload`](https://pkg.go.dev/gorm.io/gorm#DB.Preload) callbacks are **unaffected** (Preload builds a fresh DB). So the same code inside a Preload callback is not flagged.

</details>

> [!NOTE]
> **Removal condition:** this rule is deleted once the upstream fix ships in a supported GORM release. go-gorm/gorm#7592 is checked again on each GORM version bump.

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

A directive on an exported function or method also applies in the packages that call it. The analysis of the declaring package checks the function's body. It passes the result on as an [analysis fact](https://pkg.go.dev/golang.org/x/tools/go/analysis#hdr-Modular_analysis_with_Facts). So the packages that call it do not read its source. This works with the `gormreuse` command, `go vet` and golangci-lint, which all analyze the dependencies first.

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

<table>
<tr><th>Where you write it</th><th>Example</th></tr>
<tr><td>

**Line**, on its own line.

Covers that line and the next one. That is every line of the statement directly below.

</td><td>

```go
func ownLine(db *gorm.DB) {
    q := db.Where("x")
    q.Find(nil)
    //gormreuse:ignore // intentional reuse for pagination
    q.Count(nil) // not reported
}
```

</td></tr>
<tr><td>

**Line**, after code on the same line.

Covers that line only. That is every line of the statement it is on.

</td><td>

```go
func sameLine(db *gorm.DB) {
    q := db.Where("x")
    q.Find(nil)
    q.Count(nil) //gormreuse:ignore // not reported
    q.Delete(&User{}) // reported
}
```

</td></tr>
<tr><td>

**Line**, above a multi-line statement.

Covers every line of the statement.

</td><td>

```go
func multiLine(db *gorm.DB) {
    q := db.Where("x")
    q.Find(nil)
    //gormreuse:ignore
    q.
        Where("y").
        Find(nil) // not reported
}
```

</td></tr>
<tr><td>

**Line**, above `if`, `for`, `switch` or `select`.

Covers only the header up to `{`.

</td><td>

```go
func ifHeader(db *gorm.DB) {
    q := db.Where("x")
    q.Find(nil)
    //gormreuse:ignore
    if q.Count(nil).Error == nil { // not reported
        q.Delete(&User{}) // reported
    }
}
```

</td></tr>
<tr><td>

**Line**, inside a closure.

Covers the closure's statement only.

</td><td>

```go
func closure(db *gorm.DB) {
    q := db.Where("x")
    q.Find(nil)
    func() {
        //gormreuse:ignore
        q.Count(nil) // not reported
        q.Delete(&User{}) // reported
    }()
}
```

</td></tr>
<tr><td>

**Function**, in the function's doc comment.

Covers the whole declaration, its doc comment, and every closure inside it.

</td><td>

```go
//gormreuse:ignore
func legacyCode(db *gorm.DB) {
    q := db.Where("x")
    q.Find(nil)
    q.Count(nil) // not reported
}
```

</td></tr>
<tr><td>

**File**, before the package declaration.

Covers the whole file.

</td><td>

```go
//gormreuse:ignore

package mypackage

// All violations in this file are suppressed
```

</td></tr>
</table>

- An ignore only hides reports. gormreuse still checks the function. A `//gormreuse:pure` function that leaks its argument is still not trusted at its call sites, even when the leak report is ignored.
- An unused ignore is reported. See [Unused and redundant directives](#unused-and-redundant-directives).

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

Without a directive, a call to your own function is treated like this. Here `helper` is an unmarked `func helper(db *gorm.DB) *gorm.DB { return db.Where("x") }`:

<table>
<tr><th>Call</th><th>Effect on the <code>*gorm.DB</code> argument <code>q</code></th></tr>
<tr><td>

```go
func caller(db *gorm.DB) {
    q := db.Where("a")
    helper(q)   // a branch of q
    q.Find(nil) // reported
}
```

</td><td>

`helper(q)` is a branch of `q`. The same holds when the result is not a `*gorm.DB`.

</td></tr>
<tr><td>

```go
func caller(db *gorm.DB, cond bool) {
    q := db.Where("a")
    if cond {
        q = helper(q) // like q = q.Where(...)
    }
    q.Find(nil) // OK
    q.Count(nil) // reported: q is still a mutable root
}
```

</td><td>

`q = helper(q)` assigns the result back to `q`. It works like `q = q.Where(...)`, also under a condition or in a loop. The result is a new mutable root.

</td></tr>
<tr><td>

```go
func caller(db *gorm.DB) {
    q := db.Where("a")
    r := helper(q) // a branch of q
    r.Find(nil)
    r.Count(nil) // reported: r is a new mutable root
    q.Find(nil)  // reported
}
```

</td><td>

`r := helper(q)` is a branch of `q`. `r` is a new mutable root, unless `helper` is [inferred immutable-return](#inferred-immutable-return).

</td></tr>
</table>

gormreuse validates that functions marked `//gormreuse:pure` actually satisfy the pure contract. Each line marked `// reported` below gets a report:

<table>
<tr><th>Rule</th><th>Example</th></tr>
<tr><td>

**Must NOT** call polluting methods ([`Where`](https://pkg.go.dev/gorm.io/gorm#DB.Where), [`Find`](https://pkg.go.dev/gorm.io/gorm#DB.Find), etc.) on [`*gorm.DB`](https://pkg.go.dev/gorm.io/gorm#DB) **arguments**.

This includes `go`, `defer`, a method value, a method expression and a closure the function creates.

It also includes an argument reached through a pointer, slice, variadic or struct parameter. So does an argument returned from another pure function that returns its argument, like `same` (see below). So does an argument boxed in an interface, then asserted back or called through it.

Message: `pure function pollutes *gorm.DB argument by calling Where`

</td><td>

```go
type wherer interface{ Where(any, ...any) *gorm.DB }

type holder struct{ db *gorm.DB }

//gormreuse:pure
func same(db *gorm.DB) *gorm.DB { return db }

//gormreuse:pure
func bad(db *gorm.DB, pp **gorm.DB, dbs []*gorm.DB, h holder) {
    db.Where("x")             // reported
    go db.Find(nil)           // reported
    defer db.Find(nil)        // reported
    f := db.Find              // reported
    (*gorm.DB).Find(db, nil)  // reported
    func() { db.Find(nil) }() // reported
    (*pp).Find(nil)           // reported
    dbs[0].Find(nil)          // reported
    h.db.Find(nil)            // reported
    same(db).Find(nil)        // reported
    var x any = db
    x.(*gorm.DB).Where("x")   // reported
    var i wherer = db
    i.Where("x")              // reported
    _ = f
}
```

</td></tr>
<tr><td>

**Must NOT** let an argument escape.

This includes a channel send, a slice/array or map store, a store to a package-level variable, and a store through a pointer.

It also includes a return of the argument inside another value: a struct, a pointer, an interface, or a closure that returns it. Return it as a `*gorm.DB` instead, as `same` below does.

Message: `pure function leaks *gorm.DB argument via channel send`

</td><td>

```go
type S struct{ DB *gorm.DB }

type holder struct{ db *gorm.DB }

var global *gorm.DB

//gormreuse:pure
func leak(db *gorm.DB, ch chan *gorm.DB, s []*gorm.DB, m map[int]*gorm.DB, h *holder) {
    ch <- db     // reported
    s[0] = db    // reported
    m[0] = db    // reported
    global = db  // reported
    h.db = db    // reported
}

//gormreuse:pure
func wrap1(db *gorm.DB) S { return S{DB: db} } // reported

//gormreuse:pure
func wrap2(db *gorm.DB) **gorm.DB { return &db } // reported

//gormreuse:pure
func wrap3(db *gorm.DB) func() *gorm.DB {
    return func() *gorm.DB { return db } // reported
}
```

</td></tr>
<tr><td>

**Must NOT** write to an argument directly.

A write into its `Statement`, or over the `*gorm.DB` itself, changes its query in place, as `Where` does. Only a direct path from a `*gorm.DB` parameter counts:

- `*db = v` and `db.Statement = v`;
- a field of the `Statement`, as in `db.Statement.Distinct = true`;
- a map or a slice the `Statement` holds: `db.Statement.Clauses[k] = v`, `db.Statement.Selects[i] = v`, and `delete`, `clear` or `copy` into one;
- the same through the parameter in a closure, a `go` or a `defer` statement in the body, also through a copy `db := db` in an inner block before any branch, when nothing assigns that copy again.

A write through anything else is not followed, since it may write memory the argument does not hold: `db.Statement.DB`, which is the parent of a `Session` copy that shares the `Statement`, a `*gorm.Statement` parameter, which may be a fresh `Statement`, a local variable that is assigned again, captured or has its address taken, a copy such as `c := *db.Statement`, a `Session` or `Debug` result, a parameter that holds a `*gorm.DB`, an `append` result, a lookup, a conversion, a merge, or a call such as `sort.Strings(db.Statement.Selects)` (see [Known Limitations](#known-limitations)).

These may be written:

- The argument's `Error` and `RowsAffected`. They hold the outcome of a query, as `AddError` does.
- Its `Config`. It holds no clause. A write to it, such as `db.DryRun = true`, still changes how the caller's later queries run, and is not reported (see [Known Limitations](#known-limitations)).

Message: `pure function pollutes *gorm.DB argument by writing to it directly`

</td><td>

```go
//gormreuse:pure
func mark(db *gorm.DB) {
    db.Statement.Distinct = true          // reported
    db.Statement.Clauses["X"] = c         // reported
    delete(db.Statement.Clauses, "WHERE") // reported
    *db = gorm.DB{}                       // reported
    db.Error = nil                        // OK
    copied := *db.Statement
    copied.Distinct = true                // OK: a local copy
    copied.Clauses["X"] = c               // not reported: through a copy
    sort.Strings(db.Statement.Selects)    // not reported: a call
}
```

</td></tr>
<tr><td>

**Must NOT** pass an argument to a function that is not pure.

This includes a pure function that fails this contract itself. It also includes a method called through an interface that may hold the argument but is not the boxed argument itself.

Message: `pure function passes *gorm.DB argument to non-pure function plain`

</td><td>

```go
type wherer interface{ Where(any, ...any) *gorm.DB }

func plain(db *gorm.DB) {}

//gormreuse:pure
func broken(db *gorm.DB) { db.Where("x") } // reported

//gormreuse:pure
func pass(db *gorm.DB) {
    plain(db)  // reported
    broken(db) // reported
    var x any = db
    x.(wherer).Where("x") // reported
}
```

</td></tr>
<tr><td>

**MAY** call polluting methods on **immutable values**.

**MAY** pass an argument to output functions: `fmt`, `log` and `testing`.

**MAY** use its receiver. A method's receiver is not an argument.

</td><td>

```go
type repo struct{ db *gorm.DB }

//gormreuse:pure
func ok(db *gorm.DB) *gorm.DB {
    fmt.Println(db) // OK
    return db.Session(&gorm.Session{}).Where("x") // OK
}

//gormreuse:pure
func (r *repo) ok(db *gorm.DB) {
    r.db.Where("x") // OK: the receiver is not an argument
}
```

</td></tr>
</table>

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

// reported: second branch from mutable q
q.Count(&count)
```

A pure function MAY also return its argument. Its result is then the same value as the argument. So a branch from the result is a branch from the argument:

```go
//gormreuse:pure
func same(db *gorm.DB) *gorm.DB { return db }

q := db.Where("x")

// OK: first branch from q
same(q).Find(&users)

// reported: second branch from q
q.Count(&count)
```

A result that comes only from immutable values, like `withTenant`'s `db.Session(...).Where(...)`, is a new chain. It does not share the argument. This holds for a pure function in another package too.

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

<table>
<tr><th>Correct directive</th><th>False directive</th></tr>
<tr><td>

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

</td><td>

```go
//gormreuse:immutable-return
func GetDB() *gorm.DB { // reported
    // trailing Where re-forks a fresh clone==0 Statement
    return globalDB.Session(&gorm.Session{}).Where("x")
}
```

Message: `immutable-return declared but function returns mutable *gorm.DB`

</td></tr>
</table>

Without the check, gormreuse would trust a false directive. It would silently allow unsafe reuse of the return value at every call site.

Only provably mutable returns are reported:

<table>
<tr><th>Returned value</th><th>Example</th></tr>
<tr><td>

**Reported:** a value whose root is a gorm chain method like `Where`, or `Session(&gorm.Session{Initialized: true})`

</td><td>

```go
//gormreuse:immutable-return
func a(db *gorm.DB) *gorm.DB { // reported
    return db.Where("x")
}

//gormreuse:immutable-return
func b(db *gorm.DB) *gorm.DB { // reported
    return db.Session(&gorm.Session{Initialized: true})
}
```

</td></tr>
<tr><td>

**Reported:** the same, returned through an IIFE, a local closure, or a deferred closure that writes a named result

</td><td>

```go
//gormreuse:immutable-return
func a(db *gorm.DB) *gorm.DB { // reported
    return func() *gorm.DB { return db.Where("x") }()
}

//gormreuse:immutable-return
func b(db *gorm.DB) *gorm.DB { // reported
    get := func() *gorm.DB { return db.Where("x") }
    return get()
}

//gormreuse:immutable-return
func c(db *gorm.DB) (q *gorm.DB) { // reported
    defer func() { q = db.Where("x") }()
    return nil
}
```

</td></tr>
<tr><td>

**Reported:** an unexported package-level variable whose every assignment in the package is mutable

</td><td>

```go
var scoped *gorm.DB

func setup(db *gorm.DB) { scoped = db.Where("x") }

//gormreuse:immutable-return
func get() *gorm.DB { // reported
    return scoped
}
```

</td></tr>
<tr><td>

**Not reported** (benefit of the doubt): a bare parameter, a value from an unmarked named helper, or a `Session` config gormreuse can't read

</td><td>

```go
func helper(db *gorm.DB) *gorm.DB { return db.Where("x") }

var cfg = &gorm.Session{}

//gormreuse:immutable-return
func a(db *gorm.DB) *gorm.DB { // not reported
    return db
}

//gormreuse:immutable-return
func b(db *gorm.DB) *gorm.DB { // not reported
    return helper(db)
}

//gormreuse:immutable-return
func c(db *gorm.DB) *gorm.DB { // not reported
    return db.Session(cfg)
}
```

</td></tr>
<tr><td>

**Not reported** (benefit of the doubt): an exported or address-taken package-level variable

</td><td>

```go
var Scoped *gorm.DB
var scoped *gorm.DB

func setA(db *gorm.DB) { Scoped = db.Where("x") }
func setB(db *gorm.DB) { scoped = db.Where("x") }
func ptr() **gorm.DB   { return &scoped }

//gormreuse:immutable-return
func a() *gorm.DB { // not reported
    return Scoped
}

//gormreuse:immutable-return
func b() *gorm.DB { // not reported
    return scoped
}
```

</td></tr>
</table>

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

Each `*gorm.DB` result, in a `(*gorm.DB, error)` result too, must be one of the inferred values below on every path. In each example, the caller branches the result twice. That second branch is reported only when the helper is not inferred.

<table>
<tr><th>Helper and caller</th><th>Inferred?</th></tr>
<tr><td>

```go
func active(db *gorm.DB) *gorm.DB {
    return db.Where("active").Session(&gorm.Session{})
}

func caller(db *gorm.DB) {
    q := active(db)
    q.Find(nil)
    q.Count(nil) // OK
}
```

</td><td>

**Inferred.** The **last** method is [`Session`](https://pkg.go.dev/gorm.io/gorm#DB.Session), [`WithContext`](https://pkg.go.dev/gorm.io/gorm#DB.WithContext) or [`Debug`](https://pkg.go.dev/gorm.io/gorm#DB.Debug).

</td></tr>
<tr><td>

```go
func begin(db *gorm.DB) *gorm.DB {
    return db.Begin()
}

func caller(db *gorm.DB) {
    tx := begin(db)
    tx.Find(nil)
    tx.Count(nil) // OK
}
```

</td><td>

**Inferred.** A [`Begin`](https://pkg.go.dev/gorm.io/gorm#DB.Begin) result.

</td></tr>
<tr><td>

```go
func open(d gorm.Dialector) (*gorm.DB, error) {
    db, err := gorm.Open(d)
    if err != nil {
        return nil, err
    }
    return db, nil
}

func caller(d gorm.Dialector) {
    db, _ := open(d)
    db.Find(nil)
    db.Count(nil) // OK
}
```

</td><td>

**Inferred.** A [`gorm.Open`](https://pkg.go.dev/gorm.io/gorm#Open) result, or `nil`.

</td></tr>
<tr><td>

```go
type repo struct{ db *gorm.DB }

func (r *repo) conn(ctx context.Context) *gorm.DB {
    return r.db.WithContext(ctx)
}

func (r *repo) users(ctx context.Context) *gorm.DB {
    return r.conn(ctx)
}

func caller(r *repo, ctx context.Context) {
    q := r.users(ctx)
    q.Find(nil)
    q.Count(nil) // OK
}
```

</td><td>

**Inferred.** The result of another `immutable-return` function, declared or inferred, in this package or another.

</td></tr>
<tr><td>

```go
func caller(db *gorm.DB) {
    conn := db.Session(&gorm.Session{})
    get := func() *gorm.DB { return conn }

    q := get()
    q.Find(nil)
    q.Count(nil) // OK
}
```

</td><td>

**Inferred.** A local variable, such as one a closure captures. Every assignment to it is in the function and holds one of these values.

</td></tr>
<tr><td>

```go
type txKey struct{}

func txFrom(ctx context.Context) *gorm.DB {
    if tx, ok := ctx.Value(txKey{}).(*gorm.DB); ok {
        return tx
    }
    return nil
}

func caller(ctx context.Context) {
    tx := txFrom(ctx)
    tx.Find(nil)
    tx.Count(nil) // OK
}
```

</td><td>

**Inferred.** A type assertion of a value that holds no chain, as reuse detection reads it.

</td></tr>
<tr><td>

```go
func same(db *gorm.DB) *gorm.DB {
    return db
}

func caller(db *gorm.DB) {
    q := same(db)
    q.Find(nil)
    q.Count(nil) // reported
}
```

</td><td>

**Not inferred.** A `*gorm.DB` parameter.

</td></tr>
<tr><td>

```go
type holder struct{ db *gorm.DB }

func build(db *gorm.DB) *gorm.DB {
    h := holder{db: db.Session(&gorm.Session{})}
    return h.db
}

func caller(db *gorm.DB) {
    q := build(db)
    q.Find(nil)
    q.Count(nil) // reported
}
```

</td><td>

**Not inferred.** A field of a struct built in the function.

</td></tr>
<tr><td>

```go
func pick(db *gorm.DB) *gorm.DB {
    var q *gorm.DB
    func() { q = db.Session(&gorm.Session{}) }()
    return q
}

func caller(db *gorm.DB) {
    q := pick(db)
    q.Find(nil)
    q.Count(nil) // reported
}
```

</td><td>

**Not inferred.** A variable assigned in a closure. A deferred closure counts too.

</td></tr>
<tr><td>

```go
func call(get func() *gorm.DB) *gorm.DB {
    return get()
}

func caller(get func() *gorm.DB) {
    q := call(get)
    q.Find(nil)
    q.Count(nil) // reported
}
```

</td><td>

**Not inferred.** A func-typed value.

</td></tr>
<tr><td>

```go
func wrap(db *gorm.DB) *gorm.DB {
    return scoped(db)
}

func scoped(db *gorm.DB) *gorm.DB {
    return db.Where("x")
}

func caller(db *gorm.DB) {
    q := wrap(db)
    q.Find(nil)
    q.Count(nil) // reported
}
```

</td><td>

**Not inferred.** A call to an unmarked helper.

</td></tr>
<tr><td>

```go
func active(db *gorm.DB) *gorm.DB {
    return db.Session(&gorm.Session{}).Where("active")
}

func caller(db *gorm.DB) {
    q := active(db)
    q.Find(nil)
    q.Count(nil) // reported
}
```

</td><td>

**Not inferred.** A chain after `Session`.

</td></tr>
<tr><td>

```go
func conn(db *gorm.DB) *gorm.DB {
    return db.Session(&gorm.Session{Initialized: true})
}

func caller(db *gorm.DB) {
    q := conn(db)
    q.Find(nil)
    q.Count(nil) // reported
}
```

</td><td>

**Not inferred.** A `Session` config with `Initialized: true`. A config that is not a `&gorm.Session{...}` literal, such as a variable, also turns inference off.

</td></tr>
<tr><td>

```go
func conn(db *gorm.DB, n int) *gorm.DB {
    if n > 0 {
        return conn(db, n-1)
    }
    return db.Session(&gorm.Session{})
}

func caller(db *gorm.DB) {
    q := conn(db, 3)
    q.Find(nil)
    q.Count(nil) // reported
}
```

</td><td>

**Not inferred.** A function that returns its own result is never inferred. Add the directive.

</td></tr>
</table>

A field read through a parameter or the receiver, or a package-level variable, is read as reuse detection reads it where it is used. So a getter is treated like the field it returns, **at each call**. The call reads the field of its own receiver.

Some code in the package may assign the field or package-level variable a chain that gormreuse can prove. Then its getter is not inferred at all. A caller may call the getter after that assignment. Then every call of the getter in the package returns a mutable root. The report names the `d := r.DB()` line as the root.

<table>
<tr><th>Getter and caller</th><th>Inferred?</th></tr>
<tr><td>

```go
type repo struct{ db *gorm.DB }

func (r *repo) DB() *gorm.DB { return r.db }

func caller(r *repo) {
    d := r.DB()
    d.Find(nil)
    d.Count(nil) // OK, as for r.db.Find and r.db.Count
}
```

</td><td>

**Inferred.** No function stores a chain in `db`. With `r` a parameter, nothing is reported.

</td></tr>
<tr><td>

```go
type repo struct{ db *gorm.DB }

func (r *repo) DB() *gorm.DB { return r.db }

type outer struct{ repo }

func caller(o *outer) {
    d := o.DB() // reads o.db
    d.Find(nil)
    d.Count(nil) // OK
}
```

</td><td>

**Inferred.** A getter promoted from an embedded struct reads the field the same way. So do `o.inner.DB()` and `get(&o.repo)`.

</td></tr>
<tr><td>

```go
type repo struct{ db *gorm.DB }

func (r *repo) DB() *gorm.DB { return r.db }

type outer struct{ p *repo }

func helper(db *gorm.DB) *gorm.DB { return db.Where("x") }

func caller(db *gorm.DB) {
    q := helper(db)
    o := outer{p: &repo{db: q}}
    d := o.p.DB()
    d.Find(nil)
    d.Count(nil) // reported
}
```

</td><td>

**Inferred.** A getter reached through a pointer field that holds `&repo{db: q}` reads `q` too. Here `q` is the result of a helper, so the getter stays inferred.

</td></tr>
<tr><td>

```go
type repo struct{ db *gorm.DB }

func (r *repo) DB() *gorm.DB { return r.db }

type outer struct{ p *repo }

func helper(db *gorm.DB) *gorm.DB { return db.Where("x") }

func caller(o *outer, db *gorm.DB) {
    q := helper(db)
    o.p.db = q
    d := o.p.DB()
    d.Find(nil)
    d.Count(nil) // not reported: the store through o.p is not seen
}
```

</td><td>

**Inferred, but the store is not seen.** A store through another read of a pointer field is not seen. Neither is `o.p.db` written in place. This holds while the getter is inferred, such as when `q` is the result of a helper.

</td></tr>
<tr><td>

```go
type repo struct{ db *gorm.DB }

func (r *repo) DB() *gorm.DB { return r.db }

func (r *repo) scope() { r.db = r.db.Where("x") }

func caller(r *repo) {
    d := r.DB()
    d.Find(nil)
    d.Count(nil) // reported
}
```

</td><td>

**Not inferred.** The package stores a chain like `db.Where("x")` in the field.

</td></tr>
<tr><td>

```go
type repo struct{ db *gorm.DB }

func (r *repo) DB() *gorm.DB { return r.db }

func newRepo(db *gorm.DB) *repo {
    q := db.Where("x")
    return &repo{db: q}
}

func caller(r *repo) {
    d := r.DB()
    d.Find(nil)
    d.Count(nil) // reported
}
```

</td><td>

**Not inferred.** `&repo{db: q}`, with `q := db.Where("x")`. This holds in every function of the package, also where `r` is a parameter.

</td></tr>
</table>

A helper that returns a struct is inferred the same way when it builds the struct. Every `*gorm.DB` field it fills in must hold one of the inferred values above.

<table>
<tr><th>Helper and caller</th><th>Inferred?</th></tr>
<tr><td>

```go
type testEnv struct{ DB *gorm.DB }

func newTestEnv(db *gorm.DB) *testEnv {
    return &testEnv{DB: db.Session(&gorm.Session{})}
}

func caller(db *gorm.DB) {
    env := newTestEnv(db)
    env.DB.Create(&User{})
    env.DB.Find(&[]User{}) // OK
}
```

</td><td>

**Inferred.** The helper builds the struct and fills `DB` with an immutable value.

</td></tr>
<tr><td>

```go
type testEnv struct{ DB *gorm.DB }

func fill(env *testEnv, db *gorm.DB) { env.DB = db.Session(&gorm.Session{}) }

func newTestEnv(db *gorm.DB) *testEnv {
    env := &testEnv{}
    fill(env, db)
    return env
}

func caller(db *gorm.DB) {
    env := newTestEnv(db)
    env.DB.Create(&User{})
    env.DB.Find(&[]User{}) // reported
}
```

</td><td>

**Not inferred.** The helper passes the struct to another function. A struct the helper did not build also turns inference off.

</td></tr>
</table>

| Also note | Detail |
| --- | --- |
| Exported | An exported inferred function exports the same fact as a declared one, so other packages trust it too. A getter's fact names the field it reads. So a call in another package reads the field of its own receiver too |
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

    // reported: mutable *gorm.DB passed to applyFilters,
    // which has a gormreuse:immutable-param directive
    applyFilters(q)
}
```

Fix it by passing an isolated value (`root.Session(&gorm.Session{})`). Or make the caller `//gormreuse:immutable-param` too, so the contract propagates. See [Contract and Directive Fixes](#contract-and-directive-fixes) for the fix.

| Call | Caller contract checked? |
| --- | --- |
| Direct calls, function and method values, method expressions and generic instances | Yes |
| The same calls under `go` and `defer` | Yes |
| A function in another package | Yes |
| A call in a package-level initializer, such as `var n = count(DB.Where("x"))` | Yes |
| A call through an interface or a func-typed parameter | No. The callee is unknown |

**Tip:** combine with `//gormreuse:pure` when a helper both leaves its argument unpolluted and expects an isolated value. If the `pure` check fails, the caller check still applies.

A statement that **discards a chain** on the parameter is reported. The parameter is isolated, so the call changes a copy:

```go
//gormreuse:immutable-param
func applyTenant(tx *gorm.DB) {
    // reported: immutable-param declared but the result of tx.Where is discarded
    tx.Where("tenant_id = ?", 1)
    tx.Find(&users)
}
```

Without the directive, this statement adds the condition to the caller's value, and the function's own `Find` sees it. So a mutable argument to such a function gets no fix: a Session would drop the condition. Assign the result (`tx = tx.Where(...)`), or remove the directive.

`immutable-param` does not exempt the parameter of a `Scopes`, `Preload` or `Connection` callback. gorm passes these callbacks a `clone == 0` handle. So their reuse is still reported.

A **redundant** `immutable-param` is reported. See [Unused and redundant directives](#unused-and-redundant-directives).

</details>

#### Inferred immutable-param

An unexported function without the directive is treated as `immutable-param` when every call passes it immutable values. So a helper that only ever gets fresh handles needs no directive:

```go
func (s *Service) importBySQL(tx *gorm.DB, sql string) error {
    if err := tx.Exec(sql).Error; err != nil {
        return err
    }

    // OK: the only caller passes a Transaction callback's tx
    return tx.Exec(sql).Error
}

func (s *Service) Import(db *gorm.DB, sql string) error {
    return db.Transaction(func(tx *gorm.DB) error {
        return s.importBySQL(tx, sql)
    })
}
```

<details>
<summary>What can be inferred, and what turns inference off</summary>

Every `*gorm.DB` argument of every call must be one of the inferred values below. In each example, the helper branches its parameter twice. That second branch is reported only when the helper is not inferred.

<table>
<tr><th>Helper and caller</th><th>Inferred?</th></tr>
<tr><td>

```go
func count(tx *gorm.DB) {
    tx.Find(nil)
    tx.Count(nil) // OK
}

func caller(db *gorm.DB) {
    count(db.Session(&gorm.Session{}))
}
```

</td><td>

**Inferred.** A `Session`-family, `Begin` or `gorm.Open` result.

</td></tr>
<tr><td>

```go
func count(tx *gorm.DB) {
    tx.Find(nil)
    tx.Count(nil) // OK
}

func conn(db *gorm.DB) *gorm.DB {
    return db.WithContext(context.Background())
}

func caller(db *gorm.DB) {
    count(conn(db))
}
```

</td><td>

**Inferred.** The result of an `immutable-return` function.

</td></tr>
<tr><td>

```go
func count(tx *gorm.DB) {
    tx.Find(nil)
    tx.Count(nil) // OK
}

func caller(db *gorm.DB) error {
    return db.Transaction(func(tx *gorm.DB) error {
        count(tx)
        return nil
    })
}
```

</td><td>

**Inferred.** The `tx` of a `Transaction` or `FindInBatches` callback.

</td></tr>
<tr><td>

```go
func count(tx *gorm.DB) {
    tx.Find(nil)
    tx.Count(nil) // OK
}

//gormreuse:immutable-input(cb)
func withFresh(db *gorm.DB, cb func(*gorm.DB)) {
    cb(db.Session(&gorm.Session{}))
}

func caller(db *gorm.DB) {
    withFresh(db, func(tx *gorm.DB) { count(tx) })
}
```

</td><td>

**Inferred.** The `tx` of a callback that an `immutable-input` function calls, declared or [inferred](#inferred-immutable-input).

</td></tr>
<tr><td>

```go
func count(tx *gorm.DB) {
    tx.Find(nil)
    tx.Count(nil) // OK
}

func outer(tx *gorm.DB) {
    count(tx)
}

func caller(db *gorm.DB) {
    outer(db.Session(&gorm.Session{}))
}
```

</td><td>

**Inferred.** The parameter of another inferred function.

</td></tr>
<tr><td>

```go
func count(tx *gorm.DB) {
    tx.Find(nil)
    tx.Count(nil) // OK
}

func caller(db *gorm.DB) {
    q := db.Session(&gorm.Session{})
    count(q)
}
```

</td><td>

**Inferred.** A local variable, when every value stored into it is one of these.

</td></tr>
<tr><td>

```go
func count(tx *gorm.DB) {
    tx.Find(nil)
    tx.Count(nil) // OK
}

var conn, _ = gorm.Open(nil)

func caller() {
    count(conn)
}
```

</td><td>

**Inferred.** A package-level variable of the package, when every value the package stores into it is one of these.

</td></tr>
<tr><td>

```go
func count(tx *gorm.DB) {
    tx.Find(nil)
    tx.Count(nil) // reported
}

//gormreuse:immutable-param
func outer(tx *gorm.DB) {
    tx.Find(nil)
    count(tx)
}

func caller(db *gorm.DB) {
    outer(db.Session(&gorm.Session{}))
}
```

</td><td>

**Not inferred.** The parameter of a declared `immutable-param` function does not count.

</td></tr>
<tr><td>

```go
func count(tx *gorm.DB) {
    tx.Find(nil)
    tx.Count(nil) // reported
}

func caller(ch chan *gorm.DB) {
    count(<-ch)
}
```

</td><td>

**Not inferred.** gormreuse must prove the value. A value it cannot follow is not enough, even when reuse detection reads it as safe.

Other examples are a map or slice element, `ctx.Value(k).(*gorm.DB)`, `*p`, a field read through a parameter, and a variable that a closure reassigns a chain.

</td></tr>
<tr><td>

```go
func count(tx *gorm.DB) {
    tx.Find(nil)
    tx.Count(nil) // reported
}

func caller(db *gorm.DB) {
    count(db.Where("x"))
}
```

</td><td>

**Not inferred.** One caller passes a mid-chain value.

</td></tr>
<tr><td>

```go
func count(tx *gorm.DB) {
    tx.Find(nil)
    tx.Count(nil) // reported
}
```

</td><td>

**Not inferred.** No call at all.

</td></tr>
<tr><td>

```go
func Count(tx *gorm.DB) {
    tx.Find(nil)
    tx.Count(nil) // reported
}

func caller(db *gorm.DB) {
    Count(db.Session(&gorm.Session{}))
}
```

</td><td>

**Not inferred.** The name is exported, so another package may call it.

</td></tr>
<tr><td>

```go
func count(tx *gorm.DB) {
    tx.Find(nil)
    tx.Count(nil) // reported
}

var hooks = []func(*gorm.DB){count}

func caller(db *gorm.DB) {
    count(db.Session(&gorm.Session{}))
}
```

</td><td>

**Not inferred.** It is used as a function value. A method value and a method expression also turn inference off. Calls under `go` and `defer`, and calls in package-level initializers, count as direct calls.

</td></tr>
<tr><td>

```go
type repo struct{}

func (repo) count(tx *gorm.DB) {
    tx.Find(nil)
    tx.Count(nil) // reported
}

type counter interface{ count(*gorm.DB) }

func caller(r repo, c counter, db *gorm.DB) {
    r.count(db.Session(&gorm.Session{}))
    c.count(db.Session(&gorm.Session{}))
}
```

</td><td>

**Not inferred.** A method is called through an interface. That is, an interface call in the package has its name.

</td></tr>
<tr><td>

```go
func count(tx *gorm.DB, n int) {
    tx.Find(nil)
    if n > 0 {
        count(tx, n-1) // reported
    }
}

func caller(db *gorm.DB) {
    count(db.Session(&gorm.Session{}), 3)
}
```

</td><td>

**Not inferred.** A call to itself with its own parameter. Recursion is not inferred.

</td></tr>
<tr><td>

```go
func count(tx *gorm.DB) {
    tx.Where("a")
    tx.Find(nil) // reported
}

func caller(db *gorm.DB) {
    count(db.Session(&gorm.Session{}))
}
```

</td><td>

**Not inferred.** A discarded chain statement on the parameter. On a mid-chain value it changes the caller's value in place. On an immutable one it does nothing.

The same holds in a closure, as in `func() { tx.Where("a") }()`. It also holds in a helper the function passes the parameter to, as in `applyTenant(tx)`.

A write into the parameter's `Statement` changes it in place too. That includes a write into a map or a slice the `Statement` holds, as in `tx.Statement.Clauses["X"] = c` or `delete(tx.Statement.Clauses, "WHERE")`, through a direct path from the parameter, as for the [`pure`](#gormreusepure) contract. A write through anything else, such as a local copy, a `Session` result or a call, is not followed, as before. Reads of scalar and interface fields, as in `log(tx.Statement.Table)` or `tx.Statement.Context.Value(k)`, stay reads.

</td></tr>
<tr><td>

```go
func count(tx *gorm.DB, hook func(*gorm.DB)) {
    hook(tx)
    tx.Find(nil) // reported
}

func caller(db *gorm.DB) {
    count(db.Session(&gorm.Session{}), func(*gorm.DB) {})
}
```

</td><td>

**Not inferred.** A use of the parameter that gormreuse cannot follow. This is a call of a func value, an interface method or a function of another package that may change it. A store into a struct also counts.

</td></tr>
</table>

A declared `immutable-param` also turns inference off. Its caller check and redundancy report stay as they are.

| Also note | Detail |
| --- | --- |
| Test files | A package with test files is analyzed twice. The run without them cannot see a caller in a test file, so it may infer the function. The run with them sees that caller. It reports the reuse when the caller passes a mid-chain value |
| No caller check | Every caller passes an immutable value. So an inferred function adds no caller-side report and exports no fact |
| Generated files | A function in a generated file is never inferred |

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

    // reported: immutable-input(cb) declared but mutable *gorm.DB passed to callback
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

#### Inferred immutable-input

An unexported function or a closure without the directive is treated as `immutable-input(name)` for each callback parameter whose body check passes. So a `Transaction` wrapper needs no directive:

```go
func withTx(db *gorm.DB, fn func(tx *gorm.DB) error) error {
    return db.Transaction(fn)
}

func useIt(db *gorm.DB) error {
    return withTx(db, func(tx *gorm.DB) error {
        tx.Create(&user)

        // OK: withTx is inferred immutable-input(fn)
        return tx.First(&user).Error
    })
}
```

<details>
<summary>What can be inferred, and what turns inference off</summary>

The check is the [body check](#gormreuseimmutable-inputname) of the directive. In each example, the callback branches its `tx` twice. That second branch is reported only when the function is not inferred.

<table>
<tr><th>Function and caller</th><th>Inferred?</th></tr>
<tr><td>

```go
func withTx(db *gorm.DB, fn func(*gorm.DB) error) error {
    return db.Transaction(fn)
}

func caller(db *gorm.DB) {
    _ = withTx(db, func(tx *gorm.DB) error {
        tx.Find(nil)
        return tx.Count(nil).Error // OK
    })
}
```

</td><td>

**Inferred.** It hands the callback to `Transaction` or `FindInBatches`.

</td></tr>
<tr><td>

```go
func withTx(db *gorm.DB, fn func(*gorm.DB) error) error {
    return db.Transaction(fn)
}

func withRetry(db *gorm.DB, fn func(*gorm.DB) error) error {
    return withTx(db, fn)
}

func caller(db *gorm.DB) {
    _ = withRetry(db, func(tx *gorm.DB) error {
        tx.Find(nil)
        return tx.Count(nil).Error // OK
    })
}
```

</td><td>

**Inferred.** It hands the callback to another `immutable-input` function, declared or inferred.

</td></tr>
<tr><td>

```go
func withFresh(db *gorm.DB, fn func(*gorm.DB) error) error {
    return fn(db.Session(&gorm.Session{}))
}

func caller(db *gorm.DB) {
    _ = withFresh(db, func(tx *gorm.DB) error {
        tx.Find(nil)
        return tx.Count(nil).Error // OK
    })
}
```

</td><td>

**Inferred.** It calls the callback with an immutable value.

</td></tr>
<tr><td>

```go
func eachBatch(db *gorm.DB, fn func(*gorm.DB) error) error {
    return db.FindInBatches(&[]User{}, 100, func(tx *gorm.DB, _ int) error {
        return fn(tx)
    }).Error
}

func caller(db *gorm.DB) {
    _ = eachBatch(db, func(tx *gorm.DB) error {
        tx.Find(nil)
        return tx.Count(nil).Error // OK
    })
}
```

</td><td>

**Inferred.** It calls the callback with the `tx` of such a callback.

</td></tr>
<tr><td>

```go
func caller(db *gorm.DB) {
    transaction := func(db *gorm.DB, fn func(*gorm.DB) error) error {
        return db.Transaction(fn)
    }
    _ = transaction(db, func(tx *gorm.DB) error {
        tx.Find(nil)
        return tx.Count(nil).Error // OK
    })
}
```

</td><td>

**Inferred.** A closure in a local variable counts.

</td></tr>
<tr><td>

```go
func WithTx(db *gorm.DB, fn func(*gorm.DB) error) error {
    return db.Transaction(fn)
}

func caller(db *gorm.DB) {
    _ = WithTx(db, func(tx *gorm.DB) error {
        tx.Find(nil)
        return tx.Count(nil).Error // reported
    })
}
```

</td><td>

**Not inferred.** A function or method with an exported name.

</td></tr>
<tr><td>

```go
func withTx(db *gorm.DB, fn func(*gorm.DB) error) error {
    return nil
}

func caller(db *gorm.DB) {
    _ = withTx(db, func(tx *gorm.DB) error {
        tx.Find(nil)
        return tx.Count(nil).Error // reported
    })
}
```

</td><td>

**Not inferred.** A callback the body never uses. A variadic callback also turns inference off.

</td></tr>
<tr><td>

```go
func withActive(db *gorm.DB, fn func(*gorm.DB) error) error {
    return fn(db.Where("active"))
}

func caller(db *gorm.DB) {
    _ = withActive(db, func(tx *gorm.DB) error {
        tx.Find(nil)
        return tx.Count(nil).Error // reported
    })
}
```

</td><td>

**Not inferred.** A mid-chain value handed to the callback.

</td></tr>
<tr><td>

```go
func forward(fn func(*gorm.DB) error) {}

func withTx(db *gorm.DB, fn func(*gorm.DB) error) error {
    forward(fn)
    return db.Transaction(fn)
}

func caller(db *gorm.DB) {
    _ = withTx(db, func(tx *gorm.DB) error {
        tx.Find(nil)
        return tx.Count(nil).Error // reported
    })
}
```

</td><td>

**Not inferred.** A flow the body check cannot follow.

</td></tr>
<tr><td>

```go
type svc struct{ db *gorm.DB }

func (s *svc) run(fn func(*gorm.DB) error) error {
    return fn(s.db)
}

func caller(s *svc) {
    _ = s.run(func(tx *gorm.DB) error {
        tx.Find(nil)
        return tx.Count(nil).Error // reported
    })
}
```

</td><td>

**Not inferred.** A value handed to the callback that gormreuse cannot prove immutable, such as `cb(s.db)` with `s` a parameter, `cb(*p)` or `cb(dbs[0])`. The values that count are the ones [inferred immutable-param](#inferred-immutable-param) accepts.

</td></tr>
<tr><td>

```go
func withTx(db *gorm.DB, fn func(*gorm.DB) error) error {
    if db == nil {
        return withTx(db, fn)
    }
    return db.Transaction(fn)
}

func caller(db *gorm.DB) {
    _ = withTx(db, func(tx *gorm.DB) error {
        tx.Find(nil)
        return tx.Count(nil).Error // reported
    })
}
```

</td><td>

**Not inferred.** A call to itself with its own callback. Recursion is not inferred.

</td></tr>
<tr><td>

```go
//gormreuse:immutable-input(fn)
func withTx(db *gorm.DB, fn, after func(*gorm.DB) error) error {
    return db.Transaction(func(tx *gorm.DB) error {
        if err := fn(tx); err != nil {
            return err
        }
        return after(tx)
    })
}

func caller(db *gorm.DB) {
    _ = withTx(db, func(*gorm.DB) error { return nil }, func(tx *gorm.DB) error {
        tx.Find(nil)
        return tx.Count(nil).Error // reported: after is not inferred
    })
}
```

</td><td>

**Not inferred.** A declared `immutable-input` on any parameter of the function. Here `fn` is declared, so `after` is not inferred.

</td></tr>
</table>

A function inferred this way is checked like a declared one at its calls. Its callbacks' `tx` parameters are immutable, unless something else also calls them.

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
     var users []User
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
 func Caller(db *gorm.DB) {
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

A discarded chain statement on the root's local variable becomes a reassignment. That creates a new mutable root at each step:

<table>
<tr><th>Before</th><th>After <code>-fix</code></th></tr>
<tr><td>

```go
q := db.Where("base")
q.Where("a")           // changes q in place
q.Where("b").Find(nil) // reported
```

</td><td>

```go
q := db.Where("base")
q = q.Where("a")
q.Where("b").Find(nil) // first branch from the new q
```

</td></tr>
<tr><td>

```go
q := db.Where("base")
q.Where("a").Limit(10)
q.Where("b").Find(nil) // reported
q.Where("c").Find(nil) // reported
```

</td><td>

```go
q := db.Where("base")
q = q.Where("a").Limit(10).Session(&gorm.Session{})
q.Where("b").Find(nil)
q.Where("c").Find(nil)
```

The whole chain is reassigned. Its Session goes after its last call.

</td></tr>
<tr><td>

```go
q := db.Where("base")
for _, id := range ids {
    q.Where("id > ?", id) // reported
    q.Find(nil)           // reported
}
```

</td><td>

```go
q := db.Where("base")
for _, id := range ids {
    q = q.Where("id > ?", id).Session(&gorm.Session{})
    q.Find(nil)
}
```

The next iteration uses `q` again, so the reassignment gets a Session.

</td></tr>
</table>

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

A root that still has two or more branches on one path gets `.Session(&gorm.Session{})` at the end of its chain. Each branch from it is then an independent chain. The [example](#automatic-fixes) above shows the basic case.

<details>
<summary>Where a Session goes</summary>

Counting starts after the reassignments. A reassigned value used once gets no Session. Neither does a chain that already ends in a Session. See the combined example below.

<table>
<tr><th>Applied to</th><th>Before</th><th>After <code>-fix</code></th></tr>
<tr><td>

A value used in a loop that does not create it again. The loop counts as a second branch

</td><td>

```go
q := db.Model(nil)
q.Where("t")
for _, id := range ids {
    q.Where("id = ?", id).Find(nil)
}
```

</td><td>

```go
q := db.Model(nil)
q = q.Where("t").Session(&gorm.Session{})
for _, id := range ids {
    q.Where("id = ?", id).Find(nil)
}
```

</td></tr>
<tr><td>

Each value whose branches are on different paths. The paths come from the control flow

</td><td>

```go
q := db.Where("base")
if c {
    q.Where("a")
}
q.Where("b").Find(nil)
q.Where("c").Find(nil)
```

</td><td>

```go
q := db.Where("base").Session(&gorm.Session{})
if c {
    q = q.Where("a").Session(&gorm.Session{})
}
q.Where("b").Find(nil)
q.Where("c").Find(nil)
```

</td></tr>
<tr><td>

Every root or reassigned value with a later use, when the fix cannot see every use of the root in order. Examples are a `defer`/`go` use, a use inside a closure, and a read through a `Session` copy

</td><td>

```go
q := db.Where("base")
q.Where("a")
q.Where("b").Find(nil)
defer q.Count(nil)
```

</td><td>

```go
q := db.Where("base")
q = q.Where("a").Session(&gorm.Session{})
q.Where("b").Find(nil)
defer q.Count(nil)
```

</td></tr>
<tr><td>

Each incoming edge of a Phi node (a conditional merge) that is still mutable. An edge counts as immutable by the same rule as detection: its **last** call must return an immutable value. `db.Session(&gorm.Session{}).Where("y")` and `db.WithContext(ctx).Where("y")` are mutable, so they get a Session too

</td><td>

```go
q := db.WithContext(ctx).Where("x")
if c {
    q = db.WithContext(ctx).Where("y")
}
q.Find(nil)
q.Count(nil)
```

</td><td>

```go
q := db.WithContext(ctx).Where("x").Session(&gorm.Session{})
if c {
    q = db.WithContext(ctx).Where("y").Session(&gorm.Session{})
}
q.Find(nil)
q.Count(nil)
```

</td></tr>
<tr><td>

Every filter of a chain of conditional filters, in one pass, also across a loop. A parameter among them gets its parameter fix. A value handed to other code, or with a discarded statement on it, stops the walk back

</td><td>

```go
q := db.Model(&User{})
if a != "" {
    q = q.Where("a = ?", a)
}
if b != "" {
    q = q.Where("b = ?", b)
}
q.Count(&n)
q.Find(&us)
```

</td><td>

```go
q := db.Model(&User{}).Session(&gorm.Session{})
if a != "" {
    q = q.Where("a = ?", a).Session(&gorm.Session{})
}
if b != "" {
    q = q.Where("b = ?", b).Session(&gorm.Session{})
}
q.Count(&n)
q.Find(&us)
```

</td></tr>
<tr><td>

Only a call that returns `*gorm.DB`. An edge that has no such call gets none. Examples are a closure parameter (`q = tx` here) and one result of `q, err := open()`

</td><td>

```go
db.Transaction(func(tx *gorm.DB) error {
    q := db.Where("x")
    if c {
        q = tx
    }
    // ... two queries on q
})
```

</td><td>

```go
db.Transaction(func(tx *gorm.DB) error {
    tx = tx.Session(&gorm.Session{})
    q := db.Where("x").Session(&gorm.Session{})
    if c {
        q = tx
    }
    // ... two queries on q
})
```

</td></tr>
</table>

</details>

<details>
<summary>How the fix writes <code>Session</code> and its import</summary>

`Session` is written with a name that refers to `gorm.io/gorm` where it is inserted:

<table>
<tr><th>The file has</th><th>The fix</th></tr>
<tr><td>

A name for `gorm.io/gorm`

</td><td>

Uses it:

```diff
 import orm "gorm.io/gorm"

 func f(db *orm.DB) {
-    q := db.Where("base")
+    q := db.Where("base").Session(&orm.Session{})
```

</td></tr>
<tr><td>

No such name

</td><td>

Adds the import `"gorm.io/gorm"`:

```diff
 package imp2

+import "gorm.io/gorm"
+
 func f() {
-    q := open().Where("base")
+    q := open().Where("base").Session(&gorm.Session{})
```

</td></tr>
<tr><td>

No such name, and `gorm` is taken anywhere in the file

</td><td>

Adds `gormio "gorm.io/gorm"`:

```diff
 package imp3

+import gormio "gorm.io/gorm"
+
 func f(gorm string) {
-    q := open().Where(gorm)
+    q := open().Where(gorm).Session(&gormio.Session{})
```

A parameter or local variable named `gorm` takes the name, in any function of the file. So does another package imported as `gorm`. Every fix in the file adds the same import.

</td></tr>
<tr><td>

Only a blank import (`_ "gorm.io/gorm"`)

</td><td>

It gives no name, so the fix adds an import as above:

```diff
 import _ "gorm.io/gorm"
+import "gorm.io/gorm"
```

</td></tr>
<tr><td>

Is in package `gorm.io/gorm` itself

</td><td>

Writes `&Session{}` and adds no import

</td></tr>
</table>

- Each diagnostic's fix carries its own import edit. So an editor can apply one quick fix alone.
- Every diagnostic of a root carries the same fix.
- A root with a diagnostic that `//gormreuse:ignore` suppresses gets **no fix**. The ignore says that reuse is meant. The fix would change it too, so the next run would report the ignore as unused. This holds when the ignore is in another function, such as a closure stored in a variable. See [the example](#when-there-is-no-fix).

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

A discarded statement such as `q.Where("a")` changes `q` in place. Later queries see `"a"`. A Session at the root alone would make it change a copy, and silently drop `"a"`. So when the fix cannot keep such a change, the diagnostic has **no fix**. Fix it by hand.

Each example below is reported, and `-fix` leaves it unchanged:

<table>
<tr><th>Code</th><th>Why there is no fix</th></tr>
<tr><td>

```go
q := db.Where("base")
r := q
r.Where("a")
q.Where("b").Find(nil)
```

</td><td>

A discarded statement cannot be reassigned. Here the receiver is an alias.

</td></tr>
<tr><td>

```go
q := db.Where("base")
where := q.Where
where("a")
q.Where("b").Find(nil)
```

</td><td>

A discarded statement cannot be reassigned. Here it is a method value.

</td></tr>
<tr><td>

```go
q := db.Where("base")
add := func() { q.Where("a") }
add()
q.Where("b").Find(nil)
q.Where("c").Find(nil)
```

</td><td>

A discarded statement cannot be reassigned. Here it is on a variable captured by a closure.

</td></tr>
<tr><td>

```go
q := db.Where("base")
q.Where("a").Session(s).Limit(1)
q.Where("b").Find(nil)
q.Where("c").Find(nil)
```

</td><td>

A discarded statement cannot be reassigned. Here the chain has a Session-family call before its last call.

</td></tr>
<tr><td>

```go
q := db.Where("base")
q.Find(nil)
applyTenant(q)
q.Count(nil)
```

</td><td>

`q` is handed to other code before a later use of `q`, or in a loop. Here it is an argument of a function that is not `//gormreuse:pure`. These count too:

- `apply(q)` for a closure `apply`
- `_ = withTenant(q)`
- `base.Model(q)`

Some helpers do not count. See [the hand-offs that keep the fix](#hand-offs-that-keep-the-fix).

</td></tr>
<tr><td>

```go
q := db.Where("base")
q.Find(nil)
applyP(&q)
q.Count(nil)
```

</td><td>

`q` is handed to other code through its address. That holds also when the address is stored first (`applyAll([]**gorm.DB{&q})`). A store into a slice, map or channel counts too.

</td></tr>
<tr><td>

```go
q := db.Where("base")
q.Find(nil)
q.Count(nil)
q = withTenant(q)
return q
```

</td><td>

A function other than a `*gorm.DB` method that returns a `*gorm.DB` may return `q` itself. So its argument counts even as the last use.

</td></tr>
<tr><td>

```go
q := db.Where("base")
go q.Where("a")
q.Find(nil)
q.Count(nil)
```

</td><td>

An argument of a `go` statement counts. So does the receiver of a chain method it runs.

</td></tr>
<tr><td>

```go
q := db.Where("base")
defer q.Find(&x)
defer q.Where("a")
q.Count(nil)
```

</td><td>

A `defer` statement counts the same way when another `defer` uses `q`. Here `Where` runs first, and `Find` reads its condition. A `defer` inside a loop counts too.

</td></tr>
<tr><td>

```go
q := db.Where("base")
q.Find(nil)
s := &Svc{db: q}
s.apply()
q.Count(nil)
```

</td><td>

`q` is stored in a struct field that other code can reach before a later use of `q`. A local struct is reached where it is handed on. Here that is `s.apply()`. `applyP(params{db: q})` counts too, and so does a closure that does so.

Reading the field in the same function (`s.db.Find(&x)`) is a use of `q`, and does not count.

</td></tr>
<tr><td>

```go
var cur *gorm.DB

func F(db *gorm.DB, xs []int) {
    q := db.Where("base")
    q.Find(nil)
    cur = q
    sort.Sort(byTenant(xs))
    q.Count(nil)
}
```

</td><td>

A package-level variable, or a struct that a parameter points to, is reached by any call after the store that may run other code. Here a method of `byTenant` may read `cur`. See the list below.

</td></tr>
<tr><td>

```go
q := db.Where("base")
q.Find(nil)
if err := q.Where("a").Error; err != nil {
    return err
}
return q.Count(nil).Error
```

</td><td>

The `Error`, `RowsAffected` or `Statement` field is read from `q`, or from a chain on `q` that runs no query. `return q.Error` counts too.

Each query adds its error to `q.Error`. So that chain changes `q` the same way a discarded statement does. Reading the field of a query's own result (`q.Where("a").Find(&x).Error`) is fine.

</td></tr>
<tr><td>

```go
q := db.Model(&Order{ID: id})
q.Update("name", "new") // fails
return q.Update("id", id).Error
```

</td><td>

A query on `q` whose error nothing reads, followed by code that sees a later result (#372).

On a mutable `q`, a failed query leaves its error on `q`. gorm skips the later `Update`, and the function returns the first error. With a Session, the later `Update` runs and the function returns `nil`. In a `Transaction` callback, that turns a rollback into a commit.

The details below say what counts as a later result, and how to get the fix back.

</td></tr>
<tr><td>

```go
q := db.Model(&User{}).Assign(u)
q.FirstOrInit(&a)
q.FirstOrCreate(&b)
```

</td><td>

The root's `Statement` may carry `Attrs`, `Assign` or `InstanceSet` state. A Session drops that state, so `FirstOrInit` and `FirstOrCreate` would change their result. Build each branch from a fresh chain instead.

</td></tr>
<tr><td>

```go
q := db.Where("base")
q.Where("a").Find(nil)
q.Where("b").Find(nil) //gormreuse:ignore
q.Where("c").Find(nil)
```

</td><td>

A diagnostic of the root is suppressed by an ignore. The ignore says that reuse is meant. See [Strategy 2](#strategy-2-session-making-roots-immutable).

</td></tr>
<tr><td>

A parameter root that the fix cannot isolate

</td><td>

See [Parameter Roots](#parameter-roots).

</td></tr>
</table>

<details>
<summary>Which calls reach a stored package-level variable or pointed-to struct</summary>

After the store, these calls count:

- A `*gorm.DB` method counts only when it takes a function, which it may run before it returns: `Connection`, `Transaction`, `FindInBatches`, `ToSQL`, and the scopes of `Scopes`.
- A call into a package that does not import gorm counts only when an argument can run code of one that does. That is a function, an interface, or a value whose type has methods in such a package. `sort.Sort(byTenant(xs))` counts, and so does `json.Marshal(v)` when the type of `v` has methods. `fmt.Sprint(n)` and `strings.ToUpper(s)` do not.
- A channel operation, `close`, and a call into `sync` or `sync/atomic` count. They let a goroutine that is already running go on.

</details>

<details>
<summary>The unchecked error case</summary>

The later code sees the error when it does any of these with the chain:

- reads `Error` or `RowsAffected`
- returns it
- passes it to a call whose result is used
- stores it in a field

The message says so. It ends with `a Session would let a later query run past an earlier query's unchecked error, so check that error first`.

Check the first query's error, and the fix comes back:

```diff
 func F(db *gorm.DB, id int) error {
-    q := db.Model(&Order{ID: id})
+    q := db.Model(&Order{ID: id}).Session(&gorm.Session{})
     if err := q.Update("name", "new").Error; err != nil {
         return err
     }
     return q.Update("id", id).Error
 }
```

</details>

#### Hand-offs that keep the fix

<table>
<tr><th>Before</th><th>After <code>-fix</code></th></tr>
<tr><td>

```go
func F(db, other *gorm.DB) {
    var n int64
    q := db.Where("base")
    go q.Count(&n)
    other.Where("id IN (?)", q).Find(nil)
    deleteByID(q, 7)
    q.Find(nil)
}
```

</td><td>

```go
func F(db, other *gorm.DB) {
    var n int64
    q := db.Where("base").Session(&gorm.Session{})
    go q.Count(&n)
    other.Where("id IN (?)", q).Find(nil)
    deleteByID(q, 7)
    q.Find(nil)
}
```

</td></tr>
<tr><td>

```go
func CreateMenus(tx *gorm.DB, ms []Menu) error {
    for _, m := range ms {
        if err := tx.Create(&m).Error; err != nil {
            return err
        }
        if err := CreateMenus(tx, m.Children); err != nil {
            return err
        }
    }
    return nil
}
```

</td><td>

```go
func CreateMenus(tx *gorm.DB, ms []Menu) error {
    tx = tx.Session(&gorm.Session{})
    for _, m := range ms {
        if err := tx.Create(&m).Error; err != nil {
            return err
        }
        if err := CreateMenus(tx, m.Children); err != nil {
            return err
        }
    }
    return nil
}
```

</td></tr>
</table>

| Hand-off | Why the fix is safe |
| --- | --- |
| The receiver of a query under `go` (`go q.Count(&n)`) | With the Session, the query runs on a copy |
| A subquery (`db.Where("id IN (?)", q)`) | It only reads `q` |
| An argument of a helper that only runs queries on it (`deleteByID(q, 7)`) | Its only change to `q` is the reuse that the fix removes. The conditions are below |
| A parameter passed to a recursive call of its own function (`createMenus(tx, m.Children)` inside `createMenus`) | The body Session rebinds the parameter in every call. No discarded statement may be reassigned |

<details>
<summary>When a helper only runs queries on a parameter</summary>

This helper qualifies:

```go
func deleteByID(db *gorm.DB, id int) error {
    return db.Where("id = ?", id).Delete(&User{}).Error
}
```

All of these must hold:

- Each use of the parameter starts a chain that ends in a finisher, or in `Session`, `WithContext` or `Debug`.
- It reads no `Error`, `RowsAffected` or `Statement` of the parameter. It may read the `Error` or `RowsAffected` of a query's result, as in `return db.Where("id = ?", id).Delete(&User{}).Error`.
- It does not store the parameter, capture it in a closure, or hand it to another call.
- It returns no `*gorm.DB`. It passes no function to a method of the chain, such as `Scopes` or `Transaction`.
- It calls no state-only method, such as `AddError`, on the parameter.
- The call is a plain call by name, not a `go` or `defer` statement. No other argument holds `q` or a function.
- A helper in another package counts when the analysis of its own package found the same.

</details>

### Parameter Roots

A reused `*gorm.DB` parameter has no definition to append a Session to. Most functions get `tx = tx.Session(&gorm.Session{})` at the top of the body. An unexported helper whose every caller passes an isolated value is [inferred `immutable-param`](#inferred-immutable-param), so its reuse is not reported at all.

<table>
<tr><th>Before</th><th>After <code>-fix</code></th></tr>
<tr><td>

```go
func List(tx *gorm.DB) {
    var users []User
    tx.Where("a").Find(&users)
    tx.Where("b").Find(&users) // reported
}
```

</td><td>

```go
func List(tx *gorm.DB) {
    tx = tx.Session(&gorm.Session{})
    var users []User
    tx.Where("a").Find(&users)
    tx.Where("b").Find(&users)
}
```

Exported, so the body gets a Session.

</td></tr>
<tr><td>

```go
func list(tx *gorm.DB) {
    var users []User
    tx.Where("a").Find(&users)
    tx.Where("b").Find(&users) // reported
}
```

</td><td>

```go
//gormreuse:immutable-param
func list(tx *gorm.DB) {
    var users []User
    tx.Where("a").Find(&users)
    tx.Where("b").Find(&users)
}
```

Unexported, and no caller passes a mutable value. The package has no test files.

</td></tr>
<tr><td>

```go
func Paginate(tx *gorm.DB) *gorm.DB {
    var n int64
    tx.Count(&n)
    return tx.Limit(10) // reported
}
```

</td><td>

**No fix.** It is exported and returns a `*gorm.DB`, so it may be a Scopes callback.

</td></tr>
<tr><td>

```go
func ApplyTenant(tx *gorm.DB) {
    tx.Where("tenant_id = ?", 1)
    tx.Find(nil)  // reported
    tx.Count(nil) // reported
}
```

</td><td>

**No fix.** The discarded `Where` also changes the caller's value.

</td></tr>
</table>

<details>
<summary>Which function gets which fix, and why</summary>

<table>
<tr><th>Function</th><th>Fix</th></tr>
<tr><td>

Every one of these holds:

- unexported, and not a method
- only called directly in the package. Callers in package-level closures (`var f = func(...)`) count
- every caller passes a value that gormreuse proves immutable for every `*gorm.DB` parameter, as [inference](#inferred-immutable-param) asks. A value it cannot follow, such as `ctx.Value(k).(*gorm.DB)`, proves nothing
- it does not change a parameter in place, itself or through a helper, a closure or a func value, as inference asks too
- when the package has test files, declared in a test file

</td><td>

Add [`//gormreuse:immutable-param`](#gormreuseimmutable-param). The caller-side check keeps the callers isolated.

Such a function is usually [inferred `immutable-param`](#inferred-immutable-param) and not reported. One that changes its parameter in place, such as with `tx.Where("a")` or `applyTenant(tx)`, is not inferred. It does not get the directive either (#387). Under the directive the change would do nothing. For a discarded statement, the [body check](#gormreuseimmutable-param) would report it. See the last row.

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

**No fix**. The statement also changes the caller's value. Callers such as `applyTenant(q); q.Find(&users)` rely on that. With `tx = tx.Where("a")`, the change would stay inside the function.

The same holds when the statement runs on a value that the parameter's variable holds, as after `if admin { tx = tx.Where("admin") }`. On a mutable `tx`, the `Where` returns the caller's value itself (#373).

A `*gorm.DB` result carries it back only to a caller that reads it. So every call in the package must read the result, and a `go` or `defer` call does not. When the package has test files, the function must be declared in a test file.

</td></tr>
</table>

Why the rules are this strict:

- `immutable-param` on an exported function would hide the reuse: callers in other packages are never checked.
- On a function with a mid-chain caller, it would move the report to that caller. On one whose caller passes a value gormreuse cannot follow, it would hide the reuse.
- On a function that changes its parameter in place, it would promise an immutable value on which the change does nothing (#387).
- A closure never gets the directive. Write it yourself where it fits.
- A package with test files is analyzed twice: without the test files and with them. The run without them cannot see the callers in the test files. So both runs use the body Session for a function outside the test files, and their fixes agree.

Neither the directive nor the body Session covers a value that the parameter merges with. That value also gets a Session, as an incoming edge of a local root does. So the fix takes one pass. Pagination code is a common case:

```diff
 func Page(db *gorm.DB, f Filter) {
+    db = db.Session(&gorm.Session{})
     var total int64
     var users []User
     if f.Name != "" {
-        db = db.Where("name = ?", f.Name)
+        db = db.Where("name = ?", f.Name).Session(&gorm.Session{})
     }
     db.Count(&total)
     db.Offset(10).Limit(10).Find(&users)
 }
```

</details>

### Contract and Directive Fixes

A mutable argument to an [`immutable-param`](#gormreuseimmutable-param) parameter gets `.Session(&gorm.Session{})`. An unused or redundant directive name, or an unused ignore, is deleted from its comment.

<table>
<tr><th>Before</th><th>After <code>-fix</code></th></tr>
<tr><td>

```go
//gormreuse:immutable-param
func apply(q *gorm.DB) {
    q.Where("a").Find(nil)
    q.Where("b").Find(nil)
}

func Caller(db *gorm.DB, p **gorm.DB) {
    q := db.Where("x")
    apply(q)  // reported
    apply(*p) // reported
}
```

</td><td>

```go
//gormreuse:immutable-param
func apply(q *gorm.DB) {
    q.Where("a").Find(nil)
    q.Where("b").Find(nil)
}

func Caller(db *gorm.DB, p **gorm.DB) {
    q := db.Where("x")
    apply(q.Session(&gorm.Session{}))
    apply((*p).Session(&gorm.Session{}))
}
```

</td></tr>
<tr><td>

```go
//gormreuse:pure // why
func double(n int) int { return n * 2 }

//gormreuse:immutable-param
func count(tx *gorm.DB) {
    tx.Count(nil)
}
```

</td><td>

```go
// why
func double(n int) int { return n * 2 }

func count(tx *gorm.DB) {
    tx.Count(nil)
}
```

`pure` is unused: `double` has no `*gorm.DB`. `immutable-param` is redundant: no parameter is reused.

</td></tr>
</table>

<details>
<summary>Details of these fixes</summary>

| Diagnostic | Fix |
| --- | --- |
| Mutable `*gorm.DB` passed to an `immutable-param` parameter | Append `.Session(&gorm.Session{})` to the argument. An argument such as `*p` is wrapped first: `(*p).Session(...)`. When a call passes several mutable arguments, the fix isolates all of them. `Session` is named and imported as in [Strategy 2](#strategy-2-session-making-roots-immutable). No fix when the function [discards a chain on its parameter](#gormreuseimmutable-param): the Session would drop that condition |
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
| In a file other than a `_test.go` file, when the package has `_test.go` files | The package is analyzed twice, with and without its tests. The ignore may be needed only with them. For example, a test may call a Transaction callback directly. Only the run with the tests reports the ignore unused, so `gormreuse ./...`, `go vet` and golangci-lint agree. Delete such an ignore by hand |

</details>

## Known Limitations

gormreuse reports a reuse only when it can follow the value. These cases are known. Each one has a test fixture, and [the implementation notes](./design/implementation.md#known-limitations) give the reasons.

| Kind | Cases | Meaning |
| --- | --- | --- |
| Missed reuse (false negatives) | 19 | Reuse that is not reported |
| Extra reports (false positives) | 5 | Safe code that is reported |
| Wrong position | — | A report on the wrong line |
| No fix | — | A report without a suggested fix |

<details>
<summary>Missed reuse (false negatives)</summary>

**Missed reuse (false negatives):**

<table>
<tr><th>Code</th><th>Why it is missed</th></tr>
<tr><td><!-- fixtures: closureModifiesCaptured -->

A closure assigns a captured variable:

```go
var q *gorm.DB
f := func() { q = db.Where("x") }
f()
q.Find(nil)
q.Count(nil) // not reported
```

</td><td>The assignment inside the closure is not followed</td></tr>
<tr><td><!-- fixtures: siteDeferSessionCopyBranchAfterDefer -->

```go
c := q.Session(&gorm.Session{})
defer c.Count(nil) // not reported
q.Where("a").Find(nil)
```

</td><td>The use of the copy is ordered at the <code>defer</code> statement, before the change</td></tr>
<tr><td><!-- fixtures: deferInsideGoroutine, nestedDeferGoroutineDefer -->

`defer` inside a `go` closure, or `defer` → `go` → `defer`, with the other use after the statement:

```go
go func() {
    defer q.Find(nil)
}()
q.Count(nil) // not reported
```

</td><td>These nested calls are not followed</td></tr>
<tr><td><!-- fixtures: methodValueConditional, methodValuePhiWithNil -->

A method value that a branch picks:

```go
if c {
    find = q.Find
} else {
    find = q.Find
}
find(nil)
q.Count(nil) // not reported
```

</td><td>A method value merged from branches is not traced to <code>q</code></td></tr>
<tr><td><!-- fixtures: repeatsClosurePassedToRepeater -->

A closure passed once to a function that calls it many times:

```go
slices.SortFunc(s, func(a, b T) int {
    q.Find(nil) // not reported
    return a.ID - b.ID
})
```

</td><td>Only the call sites in your code are counted. A closure passed to another function outside a loop is taken to run once</td></tr>
<tr><td><!-- fixtures: repeatsClosureCalledThroughSlice, repeatsClosureCalledThroughField -->

A closure called twice through a slice or a struct field:

```go
f := func() { q.Find(nil) }
fs := []func(){f}
fs[0]()
fs[0]() // not reported
```

</td><td>A call through the container is not a call site of the closure</td></tr>
<tr><td><!-- fixtures: fsoLocalLoopCarried -->

A variable reassigned and used in a loop:

```go
for _, c := range conds {
    q = q.Where(c)
    q.Find(nil) // not reported
}
```

</td><td>The reassignment is not a branch, and the next iteration's <code>Where</code> is not checked against <code>Find</code></td></tr>
<tr><td><!-- fixtures: derivedThenMergedBack -->

```go
if c { q2 = q.Where("a") }
if d { q = q2 }
q.Count(nil) // not reported
```

</td><td><code>q2</code> can flow back into <code>q</code>, so <code>Where("a")</code> is treated as an assignment of <code>q</code></td></tr>
<tr><td><!-- fixtures: fsoLoopRebuildOfAliasedValue -->

A field rebuilt in a loop after it was copied:

```go
p.db = q
for _, c := range conds {
    p.db = p.db.Where(c)
}
q.Find(nil) // not reported
```

</td><td>In a loop, the rebuild is an assignment, so its first read of <code>q</code> is not a branch</td></tr>
<tr><td><!-- fixtures: paCopyHidden -->

A pure function that returns `db.Session(...)` of its argument:

```go
//gormreuse:pure
func copyOf(db *gorm.DB) *gorm.DB { return db.Session(&gorm.Session{}) }

c := copyOf(q)
q.Where("y").Find(nil)
c.Count(nil) // not reported
```

</td><td>The copy is not the argument, so the shared <code>Statement</code> is hidden from the caller</td></tr>
<tr><td><!-- fixtures: pvInitializedAtPackageLevel, pvStoreInClosure -->

A package-level `*gorm.DB` that another function, a closure or the initializer assigned:

```go
var G = DB.Where("x") // the initializer assigns G
var H *gorm.DB

func f(db *gorm.DB) {
    func() { H = db.Where("x") }() // a closure assigns H
    G.Find(nil)
    G.Count(nil) // not reported
    H.Find(nil)
    H.Count(nil) // not reported
}
```

</td><td>Only the stores in the same function are followed. A global that the function only reads is immutable</td></tr>
<tr><td><!-- fixtures: fieldOfStructParam, crFieldContainer -->

A struct field read through a parameter or a receiver, a getter of it, or an element of a slice or a map in such a field:

```go
func (r *Repo) list() {
    r.db.Where("a").Find(nil)
    r.db.Where("b").Find(nil) // not reported
    r.DB().Find(nil)
    r.DB().Count(nil) // not reported
    r.dbs[0].Find(nil)
    r.dbs[0].Count(nil) // not reported
}
```

</td><td>The field is not a mutable root. Only the stores to it in the same function are followed, and for a getter, also the stores in its package</td></tr>
<tr><td><!-- fixtures: contractInterface -->

`immutable-param` called through an interface or a func-typed parameter:

```go
var a Applier = impl{} // impl.Apply has //gormreuse:immutable-param
a.Apply(db.Where("x"))      // not reported
impl{}.Apply(db.Where("x")) // reported
```

</td><td>The callee is unknown, so the caller contract is not checked</td></tr>
<tr><td><!-- fixtures: exprPromoted -->

A method expression of a method promoted through an embedded `*gorm.DB`:

```go
type W struct{ *gorm.DB }

w := W{db.Where("x")}
W.Find(w, nil)
W.Find(w, nil) // not reported
```

</td><td>The receiver <code>w</code> is not followed to its embedded field</td></tr>
<tr><td><!-- fixtures: fixEscapePointerFieldAddr -->

The address of a struct field handed to a helper that changes it:

```go
func applyP(p **gorm.DB) { (*p).Where("tenant = 1") }

s := svc{db: db.Where("x")}
applyP(&s.db)
s.db.Find(nil) // not reported

q := db.Where("x")
applyP(&q)
q.Find(nil) // reported
```

</td><td>Only the address of a variable, as in <code>applyP(&amp;q)</code>, counts as a branch</td></tr>
<tr><td><!-- fixtures: irGenericReceiver -->

A call through a receiver of a type parameter's type:

```go
func twice[T interface{ Find(any, ...any) *gorm.DB }](q T) {
    q.Find(nil)
    q.Find(nil) // not reported
}
```

</td><td>In the generic body <code>q</code> is a <code>T</code>, not a <code>*gorm.DB</code></td></tr>
<tr><td><!-- fixtures: cfReturnedClosureOfParam, cfReturnedClosureOfCapture -->

A closure that a call returns, called twice:

```go
func loader(q *gorm.DB) func(name string) {
    return func(name string) { q.Where("name = ?", name).Find(nil) }
}

load := loader(q)
load("a")
load("b") // not reported
```

</td><td>A call through the returned func value has no known callee</td></tr>
<tr><td><!-- fixtures: pswNotDirect, pswHolderParams, pswReassignedParam, pswInferNotDirect -->

A pure function, or a helper inferred `immutable-param`, that writes the argument's query through anything but a direct path: a `*gorm.Statement` parameter, a local variable that is assigned again, captured or has its address taken, a copy, a `Session` or `Debug` result, a holder parameter, an `append` result, or a call:

```go
//gormreuse:pure
func mark(db *gorm.DB, w clause.Clause) {
    st := db.Statement
    func() { st.Clauses["X"] = w }()                      // not reported: a captured variable
    db.Session(&gorm.Session{}).Statement.Distinct = true // not reported: a Session result
    sort.Strings(db.Statement.Selects)                    // not reported: a call
}
```

</td><td>Only a direct path from the parameter surely reaches the argument. The others may write memory it does not hold</td></tr>
<tr><td><!-- fixtures: pswConfig -->

A pure function that writes the argument's `Config`:

```go
//gormreuse:pure
func dry(db *gorm.DB) {
    db.DryRun = true // not reported
}
```

</td><td><code>Config</code> holds no clause, but the write changes how the caller's later queries run</td></tr>
</table>

</details>

<details>
<summary>Extra reports (false positives)</summary>

**Extra reports (false positives):**

<table>
<tr><th>Code</th><th>Why it is reported</th></tr>
<tr><td><!-- fixtures: opaqueTuplePairShared -->

`pair` returns two new chains:

```go
func pair(db *gorm.DB) (*gorm.DB, *gorm.DB) {
    return db.Where("a"), db.Where("b")
}

a, b := pair(db.Session(&gorm.Session{}))
a.Find(nil)
b.Find(nil) // reported
```

</td><td>Both results of one call share the call as their root</td></tr>
<tr><td><!-- fixtures: pvConditionalRebuildOfStore -->

For a global or a field `G`:

```go
G = db.Where("x")
if c { G = G.Where("a") }
G.Find(nil) // reported
```

</td><td>A store is not a merge point, so <code>Where("a")</code> counts as a branch of the stored root</td></tr>
<tr><td><!-- fixtures: cosReturnedReassigned -->

`mk` returns a new struct:

```go
p := mk(s)
go func() { p.db.Find(nil) }()
p = mk(s)
p.db.Count(nil) // reported
```

</td><td>A pointer variable that a closure captures and that is reassigned may point to either struct. So the last read is matched to the first one too</td></tr>
<tr><td><!-- fixtures: notInferredRecursive -->

A recursive helper whose every path ends in `Session(...)`, branched twice:

```go
func build(db *gorm.DB, n int) *gorm.DB {
    if n > 0 {
        return build(db, n-1)
    }
    return db.Session(&gorm.Session{})
}

q := build(db, 3)
q.Find(nil)
q.Find(nil) // reported
```

</td><td>

A function that returns its own result is never [inferred](#inferred-immutable-return) `immutable-return`. Add the directive

</td></tr>
<tr><td><!-- fixtures: rcsExclusiveAcrossClosures -->

A closure called on exclusive branches, once directly and once inside another closure:

```go
f := func() { q.Find(nil) } // reported
run := func() { f() }
if c {
    f()
} else {
    run()
}
```

</td><td>Calls in two functions are taken to both run</td></tr>
</table>

</details>

<details>
<summary>Wrong position, and no fix</summary>

**Wrong position:** a use inside a deferred closure runs at function exit. gormreuse orders it by its source position. So the report lands on `q.Find`, which runs first:

```go
q := db.Where("x")
defer func() { q.Count(nil) }()
q.Find(nil) // reported
```

**No fix:** these reports get no suggested fix.

- A `go` or `defer` statement that changes the root in place, where another use may see the change. That is `go q.Where("a")` or `go applyTenant(q)`. For `defer q.Where("a")` or `defer applyTenant(q)`, it applies with another `defer` that uses `q`, or in a loop.

  ```go
  q := db.Where("x")
  go q.Where("a") // reported, no fix
  q.Find(nil)
  ```

  ```go
  q := db.Where("x")
  defer q.Find(nil)
  defer applyTenant(q) // reported, no fix
  ```

  A Session at the root would make the call change a copy. Isolate the root by hand where the change is not needed.

- A root whose `Statement` may carry `Attrs`, `Assign` or `InstanceSet` state. A Session drops that state, so `FirstOrInit` and `FirstOrCreate` would change their result. Build each branch from a fresh chain instead.

  ```go
  q := db.Model(&User{}).Assign(u)
  q.Where("a").FirstOrCreate(u)
  q.Where("b").FirstOrCreate(u) // reported, no fix
  ```

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
