package gormreuse_test

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/analysistest"
	"golang.org/x/tools/go/analysis/checker"
	"golang.org/x/tools/go/packages"

	"github.com/mpyw/gormreuse"
)

// Growth limits: how many times more heap allocations the analyzer may make
// on a case's larger package than on its smaller one. Doubling the size of a
// linear case about doubles the count, and a quadratic one about quadruples
// it; fixed costs per package pull both ratios down. growthNearLinear is for
// shapes that grow a little faster than n (about 2.3x, 2.5x with the race
// detector) and vary a few percent with map order from run to run; it still
// fails a quadratic shape. growthExponential is for cases compared at n and
// n+4, where linear growth is under 1.3x and a shape that doubles its work
// per statement gives about 16x.
const (
	growthLinear      = 2.5
	growthNearLinear  = 3.0
	growthQuadratic   = 4.5
	growthExponential = 2.0
)

// growthRepeats is how many times each package is analyzed; the smallest
// count is kept.
const growthRepeats = 3

// growthDefaultN is the smaller size of a case that sets no n.
const growthDefaultN = 100

// growthCase is one generated shape and the growth it is allowed.
type growthCase struct {
	name string
	// n is the smaller size (growthDefaultN when zero). The larger one is
	// 2n, or n+plus when plus is set.
	n, plus int
	limit   float64
	// after names the open issue whose fix brings the case under limit.
	// Until it is fixed the case is skipped; the fix removes this field.
	after string
	// shape returns the bodies of the package's files, without the package
	// clause and the gorm import.
	shape func(n int) []string
}

// TestAllocationGrowth analyzes each generated case at two sizes and fails
// when the analyzer's heap allocations grow faster than the case's limit
// allows (#302). It counts allocations, not time: the smallest count of
// growthRepeats runs repeats within a few percent on any machine, with or
// without the race detector and coverage, while wall time under them varied
// by a factor of 25 and made a time limit either flaky or blind.
//
// The count misses work that allocates nothing, such as a linear search
// repeated in a loop. A case whose slow path is of that kind needs a size at
// which the allocations show it too.
//
// Each performance fix adds its shape here. The cases come from the
// performance audit of #297-#301; the ones that are still too slow on main
// are skipped until their issue is fixed.
func TestAllocationGrowth(t *testing.T) {
	if testing.Short() {
		t.Skip("analyzes about 90 generated packages three times")
	}
	type sized struct {
		pkg  string
		size int
	}
	gopath := t.TempDir()
	if err := os.CopyFS(filepath.Join(gopath, "src", "gorm.io", "gorm"),
		os.DirFS(filepath.Join(analysistest.TestData(), "src", "gorm.io", "gorm"))); err != nil {
		t.Fatalf("copy gorm stub: %v", err)
	}
	pairs := map[string][2]sized{}
	var patterns []string
	for _, c := range growthCases {
		if c.after != "" {
			continue
		}
		n := c.n
		if n == 0 {
			n = growthDefaultN
		}
		large := 2 * n
		if c.plus != 0 {
			large = n + c.plus
		}
		var pair [2]sized
		for i, size := range []int{n, large} {
			pkg := fmt.Sprintf("%s%d", c.name, size)
			growthWritePackage(t, filepath.Join(gopath, "src", pkg), pkg, c.shape(size))
			pair[i] = sized{pkg, size}
			patterns = append(patterns, pkg)
		}
		pairs[c.name] = pair
	}

	allocs := growthMeasure(t, gopath, patterns)
	for _, c := range growthCases {
		t.Run(c.name, func(t *testing.T) {
			if c.after != "" {
				t.Skip("enforced after " + c.after)
			}
			pair := pairs[c.name]
			small, large := allocs[pair[0].pkg], allocs[pair[1].pkg]
			if small == 0 {
				t.Fatalf("%s: the analyzer did not run", pair[0].pkg)
			}
			ratio := float64(large) / float64(small)
			t.Logf("n=%d: %d allocations, n=%d: %d allocations, ratio %.2f (limit %.1f)",
				pair[0].size, small, pair[1].size, large, ratio, c.limit)
			if ratio > c.limit {
				t.Errorf("allocations grew %.2fx from n=%d to n=%d, want at most %.1fx",
					ratio, pair[0].size, pair[1].size, c.limit)
			}
		})
	}
}

// growthWritePackage writes one generated package.
func growthWritePackage(t *testing.T, dir, pkg string, bodies []string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for i, body := range bodies {
		src := "package " + pkg + "\n\nimport \"gorm.io/gorm\"\n\nvar _ = gorm.Open\n\n" + body
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%d.go", i)), []byte(src), 0o644); err != nil {
			t.Fatalf("write source: %v", err)
		}
	}
}

// growthMeasure loads the packages in one go and runs the analyzer on them
// one at a time, returning the heap allocations its Run made on each. Loading
// and building SSA happen outside Run, so they are not counted. The count is
// process-wide, so the packages run in sequence and the test is not parallel.
func growthMeasure(t *testing.T, gopath string, patterns []string) map[string]uint64 {
	t.Helper()
	pkgs, err := packages.Load(&packages.Config{
		Mode: packages.LoadAllSyntax,
		Dir:  gopath,
		Env:  append(os.Environ(), "GOPATH="+gopath, "GO111MODULE=off", "GOPROXY=off", "GOFLAGS="),
	}, patterns...)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if packages.PrintErrors(pkgs) > 0 {
		t.Fatal("the generated packages do not compile")
	}
	allocs := map[string]uint64{}
	counted := *gormreuse.Analyzer
	counted.Run = func(pass *analysis.Pass) (any, error) {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		result, err := gormreuse.Analyzer.Run(pass)
		runtime.ReadMemStats(&after)
		count := after.Mallocs - before.Mallocs
		if old, ok := allocs[pass.Pkg.Path()]; !ok || count < old {
			allocs[pass.Pkg.Path()] = count
		}
		return result, err
	}
	for range growthRepeats {
		if _, err := checker.Analyze([]*analysis.Analyzer{&counted}, pkgs, &checker.Options{Sequential: true}); err != nil {
			t.Fatalf("analyze: %v", err)
		}
	}
	return allocs
}

// growthTabs returns k tabs.
func growthTabs(k int) string { return strings.Repeat("\t", k) }

// growthCases are the generated shapes. Each shape's comment says what it
// stresses; n is the size it grows with.
var growthCases = []growthCase{
	// n optional filters on one query, then two uses (#260).
	{name: "filters", limit: growthQuadratic, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func f(db *gorm.DB, c []bool) {\n\tq := db.Model(nil)\n")
		for i := range n {
			fmt.Fprintf(&b, "\tif c[%d] {\n\t\tq = q.Where(\"c%d\")\n\t}\n", i, i)
		}
		b.WriteString("\tq.Count(nil)\n\tq.Find(nil)\n}\n")
		return []string{b.String()}
	}},
	// n unconditional reassignments of one query.
	{name: "chain", limit: growthNearLinear, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func f(db *gorm.DB) {\n\tq := db.Model(nil)\n")
		for i := range n {
			fmt.Fprintf(&b, "\tq = q.Where(\"c%d\")\n", i)
		}
		b.WriteString("\tq.Count(nil)\n\tq.Find(nil)\n}\n")
		return []string{b.String()}
	}},
	// n diagnostics on one root.
	{name: "uses", limit: growthNearLinear, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func f(db *gorm.DB) {\n\tq := db.Where(\"x\")\n")
		for i := range n {
			fmt.Fprintf(&b, "\tq.Where(\"c%d\").Find(nil)\n", i)
		}
		b.WriteString("}\n")
		return []string{b.String()}
	}},
	// n uses of one root in separate blocks.
	{name: "usesbr", limit: growthQuadratic, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func f(db *gorm.DB, c []bool) {\n\tq := db.Where(\"x\")\n")
		for i := range n {
			fmt.Fprintf(&b, "\tif c[%d] {\n\t\tq.Where(\"c%d\").Find(nil)\n\t}\n", i, i)
		}
		b.WriteString("}\n")
		return []string{b.String()}
	}},
	// A chain of n pure functions, declared caller first (#260).
	{name: "purechain", n: 50, limit: growthQuadratic, shape: func(n int) []string {
		var b strings.Builder
		for i := range n {
			next := "db"
			if i < n-1 {
				next = fmt.Sprintf("p%d(db)", i+1)
			}
			fmt.Fprintf(&b, "//gormreuse:pure\nfunc p%d(db *gorm.DB) *gorm.DB {\n\treturn %s\n}\n\n", i, next)
		}
		b.WriteString("func caller(db *gorm.DB) {\n\tq := p0(db.Where(\"x\"))\n\tq.Find(nil)\n\tq.Find(nil)\n}\n")
		return []string{b.String()}
	}},
	// Pure chain declared callee-first.
	{name: "purerev", n: 50, limit: growthQuadratic, shape: func(n int) []string {
		var b strings.Builder
		for i := n - 1; i >= 0; i-- {
			next := "db"
			if i < n-1 {
				next = fmt.Sprintf("p%d(db)", i+1)
			}
			fmt.Fprintf(&b, "//gormreuse:pure\nfunc p%d(db *gorm.DB) *gorm.DB {\n\treturn %s\n}\n\n", i, next)
		}
		b.WriteString("func caller(db *gorm.DB) {\n\tq := p0(db.Where(\"x\"))\n\tq.Find(nil)\n\tq.Find(nil)\n}\n")
		return []string{b.String()}
	}},
	// Pure chain whose last breaks the contract: failure propagates.
	{name: "purefail", n: 50, limit: growthQuadratic, shape: func(n int) []string {
		var b strings.Builder
		for i := range n {
			next := "db.Where(\"bad\")"
			if i < n-1 {
				next = fmt.Sprintf("p%d(db)", i+1)
			}
			fmt.Fprintf(&b, "//gormreuse:pure\nfunc p%d(db *gorm.DB) *gorm.DB {\n\treturn %s\n}\n\n", i, next)
		}
		return []string{b.String()}
	}},
	// A chain of n unannotated helpers, the last returning a Session copy.
	{name: "inferchain", limit: growthLinear, shape: func(n int) []string {
		var b strings.Builder
		for i := range n {
			next := "db.Session(&gorm.Session{})"
			if i < n-1 {
				next = fmt.Sprintf("i%d(db)", i+1)
			}
			fmt.Fprintf(&b, "func i%d(db *gorm.DB) *gorm.DB {\n\treturn %s\n}\n\n", i, next)
		}
		b.WriteString("func caller(db *gorm.DB) {\n\tq := i0(db)\n\tq.Find(nil)\n\tq.Find(nil)\n}\n")
		return []string{b.String()}
	}},
	// inferchain declared callee first.
	{name: "inferrev", limit: growthLinear, shape: func(n int) []string {
		var b strings.Builder
		for i := n - 1; i >= 0; i-- {
			next := "db.Session(&gorm.Session{})"
			if i < n-1 {
				next = fmt.Sprintf("i%d(db)", i+1)
			}
			fmt.Fprintf(&b, "func i%d(db *gorm.DB) *gorm.DB {\n\treturn %s\n}\n\n", i, next)
		}
		b.WriteString("func caller(db *gorm.DB) {\n\tq := i0(db)\n\tq.Find(nil)\n\tq.Find(nil)\n}\n")
		return []string{b.String()}
	}},
	// A chain of n immutable-return functions whose last breaks the
	// contract: distrust propagates to every one (#314). Each function has a
	// file of its own, since a directive lookup scans the comments of its
	// file (#301).
	{name: "distrustreturn", limit: growthLinear, shape: func(n int) []string {
		files := make([]string, n)
		for i := range n {
			next := "db.Where(\"bad\")"
			if i < n-1 {
				next = fmt.Sprintf("R%d(db)", i+1)
			}
			files[i] = fmt.Sprintf("//gormreuse:immutable-return\nfunc R%d(db *gorm.DB) *gorm.DB {\n\treturn %s\n}\n", i, next)
		}
		return files
	}},
	// A chain of n immutable-input functions, each handing its callback to
	// the next, whose last breaks the contract (#314).
	{name: "distrustinput", limit: growthLinear, shape: func(n int) []string {
		files := make([]string, n)
		for i := range n {
			body := "cb(db.Where(\"bad\"))"
			if i < n-1 {
				body = fmt.Sprintf("I%d(db, cb)", i+1)
			}
			files[i] = fmt.Sprintf("//gormreuse:immutable-input(cb)\nfunc I%d(db *gorm.DB, cb func(*gorm.DB)) {\n\t%s\n}\n", i, body)
		}
		return files
	}},
	// n closures, each using one captured root once.
	{name: "closures", limit: growthQuadratic, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func f(db *gorm.DB) {\n\tq := db.Where(\"x\")\n")
		for i := range n {
			fmt.Fprintf(&b, "\tf%d := func() { q.Where(\"c%d\").Find(nil) }\n\tf%d()\n", i, i, i)
		}
		b.WriteString("}\n")
		return []string{b.String()}
	}},
	// One closure called at n sites in distinct blocks.
	{name: "closurecalls", limit: growthQuadratic, after: "#299", shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func f(db *gorm.DB, c []bool) {\n\tq := db.Where(\"x\")\n\tg := func() { q.Find(nil) }\n")
		for i := range n {
			fmt.Fprintf(&b, "\tif c[%d] {\n\t\tg()\n\t}\n", i)
		}
		b.WriteString("}\n")
		return []string{b.String()}
	}},
	// Closure called at n sites, closure has n/10 uses.
	{name: "closurecallsuses", limit: growthQuadratic, after: "#299", shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func f(db *gorm.DB, c []bool) {\n\tq := db.Where(\"x\")\n\tg := func() {\n")
		for i := range n/10 + 1 {
			fmt.Fprintf(&b, "\t\tq.Where(\"u%d\").Find(nil)\n", i)
		}
		b.WriteString("\t}\n")
		for i := range n {
			fmt.Fprintf(&b, "\tif c[%d] {\n\t\tg()\n\t}\n", i)
		}
		b.WriteString("}\n")
		return []string{b.String()}
	}},
	// Closures nested n/4 deep, use at the bottom.
	{name: "nestclosures", limit: growthQuadratic, shape: func(n int) []string {
		var b strings.Builder
		d := n / 4
		b.WriteString("func f(db *gorm.DB) {\n\tq := db.Where(\"x\")\n")
		for i := range d {
			fmt.Fprintf(&b, "%sfunc() {\n", growthTabs(i+1))
		}
		fmt.Fprintf(&b, "%sq.Find(nil)\n%sq.Find(nil)\n", growthTabs(d+1), growthTabs(d+1))
		for i := d - 1; i >= 0; i-- {
			fmt.Fprintf(&b, "%s}()\n", growthTabs(i+1))
		}
		b.WriteString("}\n")
		return []string{b.String()}
	}},
	// n loops, each starting a goroutine that uses one root.
	{name: "goloops", limit: growthQuadratic, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func f(db *gorm.DB, xs []int) {\n\tq := db.Where(\"x\")\n")
		for i := range n {
			fmt.Fprintf(&b, "\tfor range xs {\n\t\tgo func() { q.Where(\"c%d\").Find(nil) }()\n\t}\n", i)
		}
		b.WriteString("}\n")
		return []string{b.String()}
	}},
	// n defer and go statements using one root.
	{name: "godefer", limit: growthLinear, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func f(db *gorm.DB) {\n\tq := db.Where(\"x\")\n")
		for i := range n {
			if i%2 == 0 {
				fmt.Fprintf(&b, "\tdefer q.Where(\"c%d\").Find(nil)\n", i)
			} else {
				fmt.Fprintf(&b, "\tgo q.Where(\"c%d\").Find(nil)\n", i)
			}
		}
		b.WriteString("}\n")
		return []string{b.String()}
	}},
	// n/4 nested optional filters.
	{name: "phinest", limit: growthQuadratic, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func f(db *gorm.DB, c []bool) {\n\tq := db.Model(nil)\n")
		d := n / 4
		for i := range d {
			fmt.Fprintf(&b, "%sif c[%d] {\n%s\tq = q.Where(\"c%d\")\n", growthTabs(i+1), i, growthTabs(i+1), i)
		}
		for i := d - 1; i >= 0; i-- {
			fmt.Fprintf(&b, "%s}\n", growthTabs(i+1))
		}
		b.WriteString("\tq.Count(nil)\n\tq.Find(nil)\n}\n")
		return []string{b.String()}
	}},
	// n/4 nested loops around one use.
	{name: "loopnest", limit: growthQuadratic, shape: func(n int) []string {
		var b strings.Builder
		d := n / 4
		b.WriteString("func f(db *gorm.DB, n int) {\n\tq := db.Model(nil)\n")
		for i := range d {
			fmt.Fprintf(&b, "%sfor i%d := 0; i%d < n; i%d++ {\n", growthTabs(i+1), i, i, i)
		}
		fmt.Fprintf(&b, "%sq.Find(nil)\n", growthTabs(d+1))
		for i := d - 1; i >= 0; i-- {
			fmt.Fprintf(&b, "%s}\n", growthTabs(i+1))
		}
		b.WriteString("}\n")
		return []string{b.String()}
	}},
	// n sequential loops rebuilding one query, then two uses.
	{name: "loops", limit: growthLinear, after: "#300", shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func f(db *gorm.DB, xs []string) {\n\tq := db.Model(nil)\n")
		for range n {
			b.WriteString("\tfor _, x := range xs {\n\t\tq = q.Where(x)\n\t}\n")
		}
		b.WriteString("\tq.Count(nil)\n\tq.Find(nil)\n}\n")
		return []string{b.String()}
	}},
	// n sequential loops, each uses a fixed root (n diagnostics, n loops).
	{name: "loopsuse", limit: growthQuadratic, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func f(db *gorm.DB, xs []string) {\n\tq := db.Model(nil)\n")
		for i := range n {
			fmt.Fprintf(&b, "\tfor range xs {\n\t\tq.Where(\"c%d\").Find(nil)\n\t}\n", i)
		}
		b.WriteString("}\n")
		return []string{b.String()}
	}},
	// filters, each step through a Session copy (#260).
	{name: "sessfilters", limit: growthQuadratic, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func f(db *gorm.DB, c []bool) {\n\tq := db.Model(nil)\n")
		for i := range n {
			fmt.Fprintf(&b, "\tif c[%d] {\n\t\tq = q.Session(&gorm.Session{}).Where(\"c%d\")\n\t}\n", i, i)
		}
		b.WriteString("\tq.Count(nil)\n\tq.Find(nil)\n}\n")
		return []string{b.String()}
	}},
	// filters on a struct field.
	{name: "fields", limit: growthQuadratic, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("type S struct{ db *gorm.DB }\n\nfunc f(db *gorm.DB, c []bool) {\n\ts := &S{db: db.Model(nil)}\n")
		for i := range n {
			fmt.Fprintf(&b, "\tif c[%d] {\n\t\ts.db = s.db.Where(\"c%d\")\n\t}\n", i, i)
		}
		b.WriteString("\ts.db.Count(nil)\n\ts.db.Find(nil)\n}\n")
		return []string{b.String()}
	}},
	// Struct with n fields, each a root reused.
	{name: "structfields", limit: growthNearLinear, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("type S struct {\n")
		for i := range n {
			fmt.Fprintf(&b, "\tf%d *gorm.DB\n", i)
		}
		b.WriteString("}\n\nfunc f(db *gorm.DB) {\n\ts := &S{}\n")
		for i := range n {
			fmt.Fprintf(&b, "\ts.f%d = db.Where(\"c%d\")\n\ts.f%d.Find(nil)\n\ts.f%d.Find(nil)\n", i, i, i, i)
		}
		b.WriteString("}\n")
		return []string{b.String()}
	}},
	// A closure using a captured query that n optional filters reassign, called after each.
	{name: "capturedre", limit: growthQuadratic, after: "#299", shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func f(db *gorm.DB, c []bool) {\n\tq := db.Model(nil)\n\tg := func() { q.Find(nil) }\n")
		for i := range n {
			fmt.Fprintf(&b, "\tif c[%d] {\n\t\tq = q.Where(\"c%d\")\n\t}\n\tg()\n", i, i)
		}
		b.WriteString("}\n")
		return []string{b.String()}
	}},
	// Captured var: n stores of fresh roots, n reads in the parent.
	{name: "storereads", limit: growthQuadratic, after: "#299", shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func f(db *gorm.DB, c []bool) {\n\tq := db.Model(nil)\n\t_ = func() { _ = q }\n")
		for i := range n {
			fmt.Fprintf(&b, "\tif c[%d] {\n\t\tq = db.Where(\"c%d\")\n\t}\n\tq.Find(nil)\n", i, i)
		}
		b.WriteString("}\n")
		return []string{b.String()}
	}},
	// n roots captured by one closure that runs twice.
	{name: "capturemany", limit: growthNearLinear, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func f(db *gorm.DB) {\n")
		for i := range n {
			fmt.Fprintf(&b, "\tq%d := db.Where(\"c%d\")\n", i, i)
		}
		b.WriteString("\tg := func() {\n")
		for i := range n {
			fmt.Fprintf(&b, "\t\tq%d.Find(nil)\n", i)
		}
		b.WriteString("\t}\n\tg()\n\tg()\n}\n")
		return []string{b.String()}
	}},
	// n independent roots, each reused (n diagnostics, n fixes).
	{name: "manyroots", limit: growthNearLinear, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func f(db *gorm.DB) {\n")
		for i := range n {
			fmt.Fprintf(&b, "\tq%d := db.Where(\"c%d\")\n\tq%d.Find(nil)\n\tq%d.Find(nil)\n", i, i, i, i)
		}
		b.WriteString("}\n")
		return []string{b.String()}
	}},
	// n independent roots in branches.
	{name: "manyrootsbr", limit: growthQuadratic, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func f(db *gorm.DB, c []bool) {\n")
		for i := range n {
			fmt.Fprintf(&b, "\tif c[%d] {\n\t\tq := db.Where(\"c%d\")\n\t\tq.Find(nil)\n\t\tq.Find(nil)\n\t}\n", i, i)
		}
		b.WriteString("}\n")
		return []string{b.String()}
	}},
	// Big function, mostly non-gorm, few roots.
	{name: "bigstraight", limit: growthLinear, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func f(db *gorm.DB, c []bool, x int) int {\n\tq := db.Where(\"x\")\n")
		for i := range n * 4 {
			fmt.Fprintf(&b, "\tif c[%d] {\n\t\tx += %d\n\t}\n", i, i)
		}
		b.WriteString("\tq.Find(nil)\n\tq.Find(nil)\n\treturn x\n}\n")
		return []string{b.String()}
	}},
	// A switch with n cases, each reassigning one query.
	{name: "switchcase", limit: growthNearLinear, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func f(db *gorm.DB, k int) {\n\tq := db.Model(nil)\n\tswitch k {\n")
		for i := range n {
			fmt.Fprintf(&b, "\tcase %d:\n\t\tq = q.Where(\"c%d\")\n", i, i)
		}
		b.WriteString("\t}\n\tq.Count(nil)\n\tq.Find(nil)\n}\n")
		return []string{b.String()}
	}},
	// A switch with n cases, each using one root twice. It grows about 2.3x
	// without the race detector, but about 3x with it: some quadratic work
	// allocates only when sync.Pool drops items.
	{name: "switchuse", limit: growthQuadratic, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func f(db *gorm.DB, k int) {\n\tq := db.Model(nil)\n\tswitch k {\n")
		for i := range n {
			fmt.Fprintf(&b, "\tcase %d:\n\t\tq.Where(\"c%d\").Find(nil)\n\t\tq.Find(nil)\n", i, i)
		}
		b.WriteString("\t}\n}\n")
		return []string{b.String()}
	}},
	// n globals, each assigned and reused in its own function.
	{name: "globals", limit: growthLinear, shape: func(n int) []string {
		var b strings.Builder
		for i := range n {
			fmt.Fprintf(&b, "var g%d *gorm.DB\n\nfunc s%d(db *gorm.DB) { g%d = db.Where(\"c%d\") }\n\nfunc u%d() { g%d.Find(nil); g%d.Find(nil) }\n\n", i, i, i, i, i, i, i)
		}
		return []string{b.String()}
	}},
	// One global stored in n functions, read twice in n functions.
	{name: "oneglobal", limit: growthLinear, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("var g *gorm.DB\n\n")
		for i := range n {
			fmt.Fprintf(&b, "func s%d(db *gorm.DB) { g = db.Where(\"c%d\") }\n\nfunc u%d() { g.Find(nil); g.Find(nil) }\n\n", i, i, i)
		}
		return []string{b.String()}
	}},
	// n functions each with a reuse silenced by ignore, plus an unused ignore.
	{name: "ignores", limit: growthLinear, after: "#301", shape: func(n int) []string {
		var b strings.Builder
		for i := range n {
			fmt.Fprintf(&b, "func f%d(db *gorm.DB) {\n\tq := db.Where(\"x\")\n\tq.Find(nil)\n\tq.Find(nil) //gormreuse:ignore\n\t//gormreuse:ignore\n\t_ = 1\n}\n\n", i)
		}
		return []string{b.String()}
	}},
	// n ignored functions.
	{name: "ignorefn", limit: growthLinear, after: "#301", shape: func(n int) []string {
		var b strings.Builder
		for i := range n {
			fmt.Fprintf(&b, "//gormreuse:ignore\nfunc f%d(db *gorm.DB) {\n\tq := db.Where(\"x\")\n\tq.Find(nil)\n\tq.Find(nil)\n}\n\n", i)
		}
		return []string{b.String()}
	}},
	// One function with n ignored diagnostics.
	{name: "ignoresone", limit: growthNearLinear, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func f(db *gorm.DB) {\n\tq := db.Where(\"x\")\n")
		for i := range n {
			fmt.Fprintf(&b, "\tq.Where(\"c%d\").Find(nil) //gormreuse:ignore\n", i)
		}
		b.WriteString("}\n")
		return []string{b.String()}
	}},
	// n independent pure + immutable-return functions with directives.
	{name: "directives", limit: growthLinear, after: "#301", shape: func(n int) []string {
		var b strings.Builder
		for i := range n {
			fmt.Fprintf(&b, "//gormreuse:pure\nfunc p%d(db *gorm.DB) *gorm.DB {\n\treturn db\n}\n\n//gormreuse:immutable-return\nfunc r%d(db *gorm.DB) *gorm.DB {\n\treturn db.Session(&gorm.Session{})\n}\n\n", i, i)
		}
		return []string{b.String()}
	}},
	// Many ordinary comments and n functions (placement scans).
	{name: "comments", limit: growthLinear, shape: func(n int) []string {
		var b strings.Builder
		for i := range n {
			fmt.Fprintf(&b, "// f%d does things.\n// More text.\nfunc f%d(db *gorm.DB) *gorm.DB {\n\t// inside\n\treturn db // trailing\n}\n\n", i, i)
		}
		return []string{b.String()}
	}},
	// n unused/redundant directives.
	{name: "unuseddir", limit: growthLinear, after: "#301", shape: func(n int) []string {
		var b strings.Builder
		for i := range n {
			fmt.Fprintf(&b, "//gormreuse:pure\nfunc p%d(x int) int {\n\treturn x\n}\n\n", i)
		}
		return []string{b.String()}
	}},
	// n named scope callbacks.
	{name: "scopes", limit: growthNearLinear, shape: func(n int) []string {
		var b strings.Builder
		for i := range n {
			fmt.Fprintf(&b, "func s%d(db *gorm.DB) *gorm.DB { return db.Where(\"c%d\") }\n\n", i, i)
		}
		b.WriteString("func f(db *gorm.DB) {\n\tq := db.Model(nil)\n")
		for i := range n {
			fmt.Fprintf(&b, "\tq = q.Scopes(s%d)\n", i)
		}
		b.WriteString("\tq.Find(nil)\n}\n")
		return []string{b.String()}
	}},
	// n Transaction callbacks.
	{name: "txcallbacks", limit: growthQuadratic, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func f(db *gorm.DB) {\n\tq := db.Where(\"x\")\n")
		for i := range n {
			fmt.Fprintf(&b, "\t_ = db.Transaction(func(tx *gorm.DB) error {\n\t\treturn q.Where(\"c%d\").Find(nil).Error\n\t})\n", i)
		}
		b.WriteString("}\n")
		return []string{b.String()}
	}},
	// n small functions each with one reuse (scale baseline).
	{name: "funcs", limit: growthLinear, shape: func(n int) []string {
		var b strings.Builder
		for i := range n {
			fmt.Fprintf(&b, "func f%d(db *gorm.DB) {\n\tq := db.Where(\"x\")\n\tq.Find(nil)\n\tq.Find(nil)\n}\n\n", i)
		}
		return []string{b.String()}
	}},
	// n unannotated helpers called by n callers (inference, returned-arg).
	{name: "helpers", limit: growthLinear, shape: func(n int) []string {
		var b strings.Builder
		for i := range n {
			fmt.Fprintf(&b, "func h%d(db *gorm.DB) *gorm.DB { return db.Where(\"c%d\") }\n\nfunc u%d(db *gorm.DB) { q := h%d(db); q.Find(nil); q.Find(nil) }\n\n", i, i, i, i)
		}
		return []string{b.String()}
	}},
	// One package, n files.
	{name: "files", limit: growthLinear, shape: func(n int) []string {
		var files []string
		for i := range n {
			files = append(files, fmt.Sprintf("//gormreuse:pure\nfunc p%d(db *gorm.DB) *gorm.DB { return db }\n\nfunc f%d(db *gorm.DB) {\n\tq := p%d(db.Where(\"x\"))\n\tq.Find(nil)\n\tq.Find(nil)\n\tq.Find(nil) //gormreuse:ignore\n}\n", i, i, i))
		}
		return files
	}},
	// n variables each conditionally reassigned, all merged in a loop.
	{name: "phiwide", limit: growthQuadratic, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func f(db *gorm.DB, c []bool, xs []int) {\n")
		for i := range n / 4 {
			fmt.Fprintf(&b, "\tq%d := db.Model(nil)\n", i)
		}
		b.WriteString("\tfor range xs {\n")
		for i := range n / 4 {
			fmt.Fprintf(&b, "\t\tif c[%d] {\n\t\t\tq%d = q%d.Where(\"c\")\n\t\t}\n", i, i, (i+1)%(n/4))
		}
		b.WriteString("\t}\n")
		for i := range n / 4 {
			fmt.Fprintf(&b, "\tq%d.Find(nil)\n\tq%d.Find(nil)\n", i, i)
		}
		b.WriteString("}\n")
		return []string{b.String()}
	}},
	// Phi edges [p(phi), phi]: pure call on then-branch, else branch present.
	{name: "purephi", n: 16, plus: 4, limit: growthExponential, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("//gormreuse:pure\nfunc p(db *gorm.DB) *gorm.DB { return db }\n\nfunc side() {}\n\nfunc f(db *gorm.DB, c []bool) {\n\tq := db.Where(\"x\")\n")
		for i := range n {
			fmt.Fprintf(&b, "\tif c[%d] {\n\t\tq = p(q)\n\t} else {\n\t\tside()\n\t}\n", i)
		}
		b.WriteString("\tq.Find(nil)\n\tq.Find(nil)\n}\n")
		return []string{b.String()}
	}},
	// purephi with an immutable base.
	{name: "purephiimm", n: 16, plus: 4, limit: growthExponential, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("//gormreuse:pure\nfunc p(db *gorm.DB) *gorm.DB { return db }\n\nfunc side() {}\n\nfunc f(db *gorm.DB, c []bool) {\n\tq := db.Session(&gorm.Session{})\n")
		for i := range n {
			fmt.Fprintf(&b, "\tif c[%d] {\n\t\tq = p(q)\n\t} else {\n\t\tside()\n\t}\n", i)
		}
		b.WriteString("\tq.Find(nil)\n\tq.Find(nil)\n}\n")
		return []string{b.String()}
	}},
	// purephi without the else branch: edge order [phi, call]. Since #297
	// it grows like filters.
	{name: "purephisafe", limit: growthQuadratic, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("//gormreuse:pure\nfunc p(db *gorm.DB) *gorm.DB { return db }\n\nfunc f(db *gorm.DB, c []bool) {\n\tq := db.Where(\"x\")\n")
		for i := range n {
			fmt.Fprintf(&b, "\tif c[%d] {\n\t\tq = p(q)\n\t}\n", i)
		}
		b.WriteString("\tq.Find(nil)\n\tq.Find(nil)\n}\n")
		return []string{b.String()}
	}},
	// n exclusive uses followed by a long tail of blocks.
	{name: "excltail", limit: growthQuadratic, after: "#299", shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func side() {}\n\nfunc f(db *gorm.DB, k int, c []bool) {\n\tq := db.Where(\"x\")\n\tswitch k {\n")
		for i := range n {
			fmt.Fprintf(&b, "\tcase %d:\n\t\tq.Find(nil)\n", i)
		}
		b.WriteString("\t}\n")
		for i := range n {
			fmt.Fprintf(&b, "\tif c[%d] {\n\t\tside()\n\t}\n", i)
		}
		b.WriteString("}\n")
		return []string{b.String()}
	}},
	// Merged roots each with a polluting (non-pure) use.
	{name: "mergedpollute", limit: growthQuadratic, after: "#299", shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func logDB(db *gorm.DB) {}\n\nfunc f(db *gorm.DB, c []bool) {\n\tq := db.Model(nil)\n")
		for i := range n {
			fmt.Fprintf(&b, "\tif c[%d] {\n\t\tq = q.Where(\"c%d\")\n\t\tlogDB(q)\n\t}\n", i, i)
		}
		b.WriteString("\tq.Find(nil)\n}\n")
		return []string{b.String()}
	}},
	// Closures nested n deep, each called in 8 exclusive cases.
	{name: "nestexcl", n: 4, limit: growthNearLinear, shape: func(n int) []string {
		var b strings.Builder
		d, k := n, 8
		b.WriteString("func f(db *gorm.DB, x int) {\n\tq := db.Where(\"x\")\n")
		var rec func(level int, ind string)
		rec = func(level int, ind string) {
			if level == d {
				fmt.Fprintf(&b, "%sq.Find(nil)\n", ind)
				return
			}
			fmt.Fprintf(&b, "%sg%d := func() {\n", ind, level)
			rec(level+1, ind+"\t")
			fmt.Fprintf(&b, "%s}\n%sswitch x {\n", ind, ind)
			for i := range k {
				fmt.Fprintf(&b, "%scase %d:\n%s\tg%d()\n", ind, i, ind, level)
			}
			fmt.Fprintf(&b, "%s}\n", ind)
		}
		rec(0, "\t")
		b.WriteString("}\n")
		return []string{b.String()}
	}},
	// f switches over c0..ck, and each ci returns c(i+1): immutable-return
	// inference was exponential in k (#298).
	{name: "inferexp", n: 16, plus: 4, limit: growthExponential, shape: func(n int) []string {
		var b strings.Builder
		k := n
		b.WriteString("func f(db *gorm.DB, i int) *gorm.DB {\n\tswitch i {\n")
		for i := range k {
			fmt.Fprintf(&b, "\tcase %d:\n\t\treturn c%d(db)\n", i, i)
		}
		fmt.Fprintf(&b, "\t}\n\treturn c%d(db)\n}\n\n", k)
		for i := range k {
			fmt.Fprintf(&b, "func c%d(db *gorm.DB) *gorm.DB { return c%d(db) }\n\n", i, i+1)
		}
		fmt.Fprintf(&b, "func c%d(db *gorm.DB) *gorm.DB { return db.Session(&gorm.Session{}) }\n", k)
		return []string{b.String()}
	}},
	// Each ci switches over every later cj. The analyzer's count does not show
	// #298 on this shape; TestInferImmutableReturnsGrowth checks inference
	// alone on it.
	{name: "inferexp2", n: 16, limit: growthQuadratic, shape: func(n int) []string {
		var b strings.Builder
		k := n
		for i := range k {
			fmt.Fprintf(&b, "func c%d(db *gorm.DB, x int) *gorm.DB {\n\tswitch x {\n", i)
			for j := i + 1; j <= k; j++ {
				fmt.Fprintf(&b, "\tcase %d:\n\t\treturn c%d(db, x)\n", j, j)
			}
			fmt.Fprintf(&b, "\t}\n\treturn c%d(db, x)\n}\n\n", k)
		}
		fmt.Fprintf(&b, "func c%d(db *gorm.DB, x int) *gorm.DB { return db.Session(&gorm.Session{}) }\n", k)
		return []string{b.String()}
	}},
	// Scopes callback returning through a helper chain with 2 call sites per level.
	{name: "scopesdeep", n: 16, limit: growthLinear, shape: func(n int) []string {
		var b strings.Builder
		d := n
		b.WriteString("func scope(db *gorm.DB) *gorm.DB { return h0(db) }\n\n")
		for i := range d {
			fmt.Fprintf(&b, "func h%d(db *gorm.DB) *gorm.DB {\n\tif db.Error != nil {\n\t\treturn h%d(db)\n\t}\n\treturn h%d(db.Where(\"x\"))\n}\n\n", i, i+1, i+1)
		}
		fmt.Fprintf(&b, "func h%d(db *gorm.DB) *gorm.DB { return db.Session(&gorm.Session{}) }\n\nfunc f(db *gorm.DB) { db.Scopes(scope).Find(nil) }\n", d)
		return []string{b.String()}
	}},
	// Helpers + one ignore directive in the file (arms Contains scans).
	{name: "helpersign", limit: growthLinear, after: "#301", shape: func(n int) []string {
		var b strings.Builder
		for i := range n {
			fmt.Fprintf(&b, "// h%d doc.\nfunc h%d(db *gorm.DB) *gorm.DB { return db.Where(\"c%d\") }\n\n", i, i, i)
		}
		b.WriteString("func caller(db *gorm.DB) {\n")
		for i := range n {
			fmt.Fprintf(&b, "\th%d(db).Find(nil)\n", i)
		}
		b.WriteString("\t//gormreuse:ignore\n\t_ = 1\n}\n")
		return []string{b.String()}
	}},
	// Control for helpersign.
	{name: "helpersnoign", limit: growthLinear, shape: func(n int) []string {
		var b strings.Builder
		for i := range n {
			fmt.Fprintf(&b, "// h%d doc.\nfunc h%d(db *gorm.DB) *gorm.DB { return db.Where(\"c%d\") }\n\n", i, i, i)
		}
		b.WriteString("func caller(db *gorm.DB) {\n")
		for i := range n {
			fmt.Fprintf(&b, "\th%d(db).Find(nil)\n", i)
		}
		b.WriteString("\t_ = 1\n}\n")
		return []string{b.String()}
	}},
	// n closures with //gormreuse:pure above, each called twice.
	{name: "closurepure", limit: growthLinear, after: "#301", shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func f(db *gorm.DB) {\n")
		for i := range n {
			fmt.Fprintf(&b, "\t//gormreuse:pure\n\tp%d := func(db *gorm.DB) *gorm.DB { return db }\n\t_ = p%d(db)\n\t_ = p%d(db)\n", i, i, i)
		}
		b.WriteString("}\n")
		return []string{b.String()}
	}},
	// n unexported funcs reusing their param, each called once.
	{name: "paramfix", limit: growthLinear, shape: func(n int) []string {
		var b strings.Builder
		for i := range n {
			fmt.Fprintf(&b, "func h%d(db *gorm.DB) {\n\tdb.Find(nil)\n\tdb.Find(nil)\n}\n\n", i)
		}
		b.WriteString("func caller(db *gorm.DB) {\n")
		for i := range n {
			fmt.Fprintf(&b, "\th%d(db.Session(&gorm.Session{}))\n", i)
		}
		b.WriteString("}\n")
		return []string{b.String()}
	}},
	// Discarded conditional chains (virtual roots).
	{name: "discarded", limit: growthQuadratic, after: "#299", shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func f(db *gorm.DB, c []bool) {\n\tq := db.Model(nil)\n")
		for i := range n {
			fmt.Fprintf(&b, "\tif c[%d] {\n\t\tq.Where(\"c%d\")\n\t}\n", i, i)
		}
		b.WriteString("\tq.Find(nil)\n\tq.Find(nil)\n}\n")
		return []string{b.String()}
	}},
	// Switch with n exclusive uses, last case reuses, long tail.
	{name: "conflict", limit: growthQuadratic, after: "#299", shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func f(db *gorm.DB, k int, c []bool, x int) int {\n\tq := db.Where(\"x\")\n\tswitch k {\n")
		for i := range n {
			fmt.Fprintf(&b, "\tcase %d:\n\t\tq.Find(nil)\n", i)
		}
		b.WriteString("\tdefault:\n\t\tq.Find(nil)\n\t\tq.Find(nil)\n\t}\n")
		for i := range n {
			fmt.Fprintf(&b, "\tif c[%d] {\n\t\tx++\n\t}\n", i)
		}
		b.WriteString("\treturn x\n}\n")
		return []string{b.String()}
	}},
	// n violations in one function plus one unrelated ignore line.
	{name: "ignoreline", limit: growthNearLinear, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func f(db *gorm.DB) {\n\tq := db.Where(\"x\")\n\t_ = 1 //gormreuse:ignore\n")
		for i := range n {
			fmt.Fprintf(&b, "\tq.Where(\"c%d\").Find(nil)\n", i)
		}
		b.WriteString("}\n")
		return []string{b.String()}
	}},
	// n unused ignores on clean code.
	{name: "unusedign", limit: growthQuadratic, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func f(db *gorm.DB) {\n")
		for i := range n {
			fmt.Fprintf(&b, "\tdb.Where(\"c%d\").Find(nil) //gormreuse:ignore\n", i)
		}
		b.WriteString("}\n")
		return []string{b.String()}
	}},
	// Pure function with n sequential loops reassigning via a pure callee.
	// Since #297 it grows like filters.
	{name: "pureloops", limit: growthQuadratic, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("//gormreuse:pure\nfunc id(db *gorm.DB) *gorm.DB { return db }\n\n//gormreuse:pure\nfunc p(db *gorm.DB, xs []int) *gorm.DB {\n")
		for range n {
			b.WriteString("\tfor range xs {\n\t\tdb = id(db)\n\t}\n")
		}
		b.WriteString("\treturn db\n}\n")
		return []string{b.String()}
	}},
	// n roots each passed to a non-pure function then reused (change/escape checks).
	{name: "manyrootsescape", limit: growthNearLinear, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func use(db *gorm.DB) {}\n\nfunc f(db *gorm.DB) {\n")
		for i := range n {
			fmt.Fprintf(&b, "\tq%d := db.Where(\"c%d\")\n\tuse(q%d)\n\tq%d.Find(nil)\n", i, i, i, i)
		}
		b.WriteString("}\n")
		return []string{b.String()}
	}},
}
