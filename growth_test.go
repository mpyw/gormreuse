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
	"github.com/mpyw/gormreuse/internal/effort"
)

// Growth limits: how many times more heap allocations the analyzer may make
// on a case's larger package than on its smaller one. Doubling the size of a
// linear case about doubles the count, and a quadratic one about quadruples
// it; fixed costs per package pull both ratios down. growthNearLinear is for
// shapes that grow a little faster than n (about 2.3x, 2.5x with the race
// detector) and vary a few percent with map order from run to run; it still
// fails a quadratic shape. growthCubic is for the steps of a case whose code
// grows as n^2. growthExponential is for cases compared at n and
// n+4, where linear growth is under 1.3x and a shape that doubles its work
// per statement gives about 16x.
const (
	growthLinear      = 2.5
	growthNearLinear  = 3.0
	growthQuadratic   = 4.5
	growthCubic       = 9.0
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
	// steps is the limit for the steps of the analyzer's searches (see
	// effort), when it differs from limit: for a case whose searches repeat
	// work that allocates nothing.
	steps float64
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
// The allocation count misses work that allocates nothing, such as a linear
// search repeated in a loop. So the test also counts the steps of the
// analyzer's searches (see effort), which no detector or map order changes,
// and holds them to the same limit, or to the case's steps limit (#350).
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

	counts := growthMeasure(t, gopath, patterns)
	for _, c := range growthCases {
		t.Run(c.name, func(t *testing.T) {
			if c.after != "" {
				t.Skip("enforced after " + c.after)
			}
			pair := pairs[c.name]
			small, large := counts[pair[0].pkg], counts[pair[1].pkg]
			if small.allocs == 0 {
				t.Fatalf("%s: the analyzer did not run", pair[0].pkg)
			}
			stepLimit := c.limit
			if c.steps != 0 {
				stepLimit = c.steps
			}
			for _, m := range []struct {
				what         string
				small, large uint64
				limit        float64
			}{
				{"allocations", small.allocs, large.allocs, c.limit},
				{"search steps", small.steps, large.steps, stepLimit},
			} {
				if m.small == 0 && m.large == 0 {
					continue // a case with no search, such as one of comments only
				}
				ratio := float64(m.large) / float64(max(m.small, 1))
				t.Logf("n=%d: %d %s, n=%d: %d %s, ratio %.2f (limit %.1f)",
					pair[0].size, m.small, m.what, pair[1].size, m.large, m.what, ratio, m.limit)
				if ratio > m.limit {
					t.Errorf("%s grew %.2fx from n=%d to n=%d, want at most %.1fx",
						m.what, ratio, pair[0].size, pair[1].size, m.limit)
				}
			}
		})
	}
}

// growthCount is what growthMeasure counts for one package.
type growthCount struct {
	allocs, steps uint64
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
// one at a time, returning the heap allocations and the search steps its Run
// made on each. Loading and building SSA happen outside Run, so they are not
// counted. The counts are process-wide, so the packages run in sequence and
// the test is not parallel. Each count is the smallest of the runs: the
// steps vary a little with map order too.
func growthMeasure(t *testing.T, gopath string, patterns []string) map[string]growthCount {
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
	counts := map[string]growthCount{}
	counted := *gormreuse.Analyzer
	counted.Run = func(pass *analysis.Pass) (any, error) {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		stepsBefore := effort.Count()
		result, err := gormreuse.Analyzer.Run(pass)
		steps := effort.Count() - stepsBefore
		runtime.ReadMemStats(&after)
		allocs := after.Mallocs - before.Mallocs
		old, ok := counts[pass.Pkg.Path()]
		if !ok || allocs < old.allocs {
			old.allocs = allocs
		}
		if !ok || steps < old.steps {
			old.steps = steps
		}
		counts[pass.Pkg.Path()] = old
		return result, err
	}
	for range growthRepeats {
		if _, err := checker.Analyze([]*analysis.Analyzer{&counted}, pkgs, &checker.Options{Sequential: true}); err != nil {
			t.Fatalf("analyze: %v", err)
		}
	}
	return counts
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
	// n optional filters, each merge used twice: n reused roots, and each
	// one's chains run on through the merges after it (#344).
	// Its steps are quadratic: Each merge's search walks back through the merges
	// before it, which allocates nothing (#350).
	{name: "phiroots", limit: growthNearLinear, steps: growthQuadratic, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func f(db *gorm.DB, c []bool) {\n\tq := db.Model(nil)\n")
		for i := range n {
			fmt.Fprintf(&b, "\tif c[%d] {\n\t\tq = q.Where(\"c%d\")\n\t}\n\tq.Count(nil)\n\tq.Find(nil)\n", i, i)
		}
		b.WriteString("}\n")
		return []string{b.String()}
	}},
	// n/2 optional filters on a parameter, each followed by a loop of
	// filters, then two uses. The fix follows the Phis back from the last
	// filter through every loop-header Phi (#343).
	// Its steps are quadratic: Each Phi's search walks back through the Phis
	// before it, which allocates nothing (#350).
	{name: "filterloops", limit: growthNearLinear, steps: growthQuadratic, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func f(db *gorm.DB, c []bool, xs []int) {\n")
		for i := range n / 2 {
			fmt.Fprintf(&b, "\tif c[%d] {\n\t\tdb = db.Where(\"c%d\")\n\t}\n\tfor range xs {\n\t\tdb = db.Where(\"x%d\")\n\t}\n", i, i, i)
		}
		b.WriteString("\tdb.Count(nil)\n\tdb.Find(nil)\n}\n")
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
	{name: "closurecalls", limit: growthQuadratic, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func f(db *gorm.DB, c []bool) {\n\tq := db.Where(\"x\")\n\tg := func() { q.Find(nil) }\n")
		for i := range n {
			fmt.Fprintf(&b, "\tif c[%d] {\n\t\tg()\n\t}\n", i)
		}
		b.WriteString("}\n")
		return []string{b.String()}
	}},
	// Closure called at n sites, closure has n/10 uses. What the loads of one
	// closure see, and what reaches each block, are kept once (#325).
	// Its steps are quadratic: The stores each call site of the closure sees are
	// searched back from its own block, through the blocks of the sites before
	// it (#350).
	{name: "closurecallsuses", limit: growthLinear, steps: growthQuadratic, shape: func(n int) []string {
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
	// A closure made on each iteration, called on n exclusive cases (#306):
	// no site reaches another without making the closure again, so every
	// site is labelled.
	// Its steps are quadratic: Whether a site runs again is asked of every other
	// site, which allocates nothing (#350).
	{name: "loopclosurecases", limit: growthNearLinear, steps: growthQuadratic, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func f(db *gorm.DB, ids []int) {\n\tfor _, id := range ids {\n\t\tq := db.Where(\"x\")\n\t\tg := func() { q.Find(nil) }\n\t\tswitch id {\n")
		for i := range n {
			fmt.Fprintf(&b, "\t\tcase %d:\n\t\t\tg()\n", i)
		}
		b.WriteString("\t\t}\n\t}\n}\n")
		return []string{b.String()}
	}},
	// A chain of n closures, each calling the one before it through a
	// captured variable, the last one called twice (#305).
	{name: "closurechain", limit: growthLinear, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func f(db *gorm.DB) {\n\tq := db.Where(\"x\")\n\tf0 := func() { q.Find(nil) }\n")
		for i := 1; i < n; i++ {
			fmt.Fprintf(&b, "\tf%d := func() { f%d() }\n", i, i-1)
		}
		fmt.Fprintf(&b, "\tf%d()\n\tf%d()\n}\n", n-1, n-1)
		return []string{b.String()}
	}},
	// One closure called once in each of n closures, each called once on an
	// exclusive case (#305).
	{name: "closurecallers", limit: growthLinear, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func f(db *gorm.DB, k int) {\n\tq := db.Where(\"x\")\n\tg := func() { q.Find(nil) }\n\tswitch k {\n")
		for i := range n {
			fmt.Fprintf(&b, "\tcase %d:\n\t\tfunc() { g() }()\n", i)
		}
		b.WriteString("\t}\n}\n")
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
	// n sequential loops rebuilding one query, then two uses. Each loop's
	// receiver may hold the root of every loop before it, since a loop may
	// run no time, so the root lists are quadratic in total: the steps grow
	// so. They are found once per merge, so the allocations grow about
	// 2.4x at these sizes, and 2.7x to 3x at n=400 to 1600 (#350).
	{name: "loops", limit: growthNearLinear, steps: growthQuadratic, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func f(db *gorm.DB, xs []string) {\n\tq := db.Model(nil)\n")
		for range n {
			b.WriteString("\tfor _, x := range xs {\n\t\tq = q.Where(x)\n\t}\n")
		}
		b.WriteString("\tq.Count(nil)\n\tq.Find(nil)\n}\n")
		return []string{b.String()}
	}},
	// n sequential range-over-func loops rebuilding one query, then two uses.
	// The stores of each body are seen where its loop is (#341), so the read
	// in each body may hold the root of every body before it. A body sees only
	// the stores that reach its loop, not those of the bodies after it, and the
	// stores of a body are searched once. The shape was as superlinear before
	// #341, from the Repeats of the n bodies.
	{name: "rangeloops", limit: growthQuadratic, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func f(db *gorm.DB, seq func(func(string) bool)) {\n\tq := db.Model(nil)\n")
		for range n {
			b.WriteString("\tfor x := range seq {\n\t\tq = q.Where(x)\n\t}\n")
		}
		b.WriteString("\tq.Count(nil)\n\tq.Find(nil)\n}\n")
		return []string{b.String()}
	}},
	// n struct hand-offs of one root, as arguments and as receivers (#341).
	{name: "handoffs", limit: growthNearLinear, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("type S struct{ db *gorm.DB }\n\nfunc (s *S) m() {}\n\nfunc use(S) {}\n\n")
		b.WriteString("func f(db *gorm.DB) {\n\tq := db.Model(nil)\n\tr := &S{db: q}\n")
		for range n {
			b.WriteString("\tuse(S{db: q})\n\tr.m()\n")
		}
		b.WriteString("}\n")
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
	{name: "capturedre", limit: growthQuadratic, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func f(db *gorm.DB, c []bool) {\n\tq := db.Model(nil)\n\tg := func() { q.Find(nil) }\n")
		for i := range n {
			fmt.Fprintf(&b, "\tif c[%d] {\n\t\tq = q.Where(\"c%d\")\n\t}\n\tg()\n", i, i)
		}
		b.WriteString("}\n")
		return []string{b.String()}
	}},
	// Captured var: n stores of fresh roots, n reads in the parent.
	{name: "storereads", limit: growthQuadratic, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func f(db *gorm.DB, c []bool) {\n\tq := db.Model(nil)\n\t_ = func() { _ = q }\n")
		for i := range n {
			fmt.Fprintf(&b, "\tif c[%d] {\n\t\tq = db.Where(\"c%d\")\n\t}\n\tq.Find(nil)\n", i, i)
		}
		b.WriteString("}\n")
		return []string{b.String()}
	}},
	// Captured var: n stores of immutable values, each followed by a read.
	// A read tries the stores that reach it until one has a root (#340), so
	// each one tries them all, as FindAllMutableRoots does in storereads.
	{name: "storeimmut", limit: growthQuadratic, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func f(db *gorm.DB, c []bool) {\n\tq := db.Session(nil)\n\t_ = func() { _ = q }\n")
		for i := range n {
			fmt.Fprintf(&b, "\tif c[%d] {\n\t\tq = db.Session(nil)\n\t}\n\tq.Find(nil)\n", i)
		}
		b.WriteString("\tq = q.Where(\"x\")\n\tq.Find(nil)\n\tq.Find(nil)\n}\n")
		return []string{b.String()}
	}},
	// The same through a field (#340).
	{name: "fieldimmut", limit: growthQuadratic, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("type S struct{ db *gorm.DB }\n\nfunc f(db *gorm.DB, c []bool) {\n\ts := &S{db: db.Session(nil)}\n")
		for i := range n {
			fmt.Fprintf(&b, "\tif c[%d] {\n\t\ts.db = db.Session(nil)\n\t}\n\ts.db.Find(nil)\n", i)
		}
		b.WriteString("\ts.db = s.db.Where(\"x\")\n\ts.db.Find(nil)\n\ts.db.Find(nil)\n}\n")
		return []string{b.String()}
	}},
	// n optional filters on a captured query, each followed by a goroutine
	// that reads it: every read sees the stores before and after it (#303), so
	// it has up to n roots, as in capturedre.
	{name: "spawnedre", limit: growthQuadratic, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func f(db *gorm.DB, c []bool) {\n\tq := db.Model(nil)\n")
		for i := range n {
			fmt.Fprintf(&b, "\tif c[%d] {\n\t\tq = q.Where(\"c%d\")\n\t}\n\tgo func() { q.Find(nil) }()\n", i, i)
		}
		b.WriteString("}\n")
		return []string{b.String()}
	}},
	// The same through a field of a captured pointer (#303).
	{name: "spawnedfields", limit: growthQuadratic, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("type S struct{ db *gorm.DB }\n\nfunc f(db *gorm.DB, c []bool) {\n\ts := &S{db: db.Model(nil)}\n")
		for i := range n {
			fmt.Fprintf(&b, "\tif c[%d] {\n\t\ts.db = s.db.Where(\"c%d\")\n\t}\n\tgo func() { s.db.Find(nil) }()\n", i, i)
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
	{name: "ignores", limit: growthLinear, shape: func(n int) []string {
		var b strings.Builder
		for i := range n {
			fmt.Fprintf(&b, "func f%d(db *gorm.DB) {\n\tq := db.Where(\"x\")\n\tq.Find(nil)\n\tq.Find(nil) //gormreuse:ignore\n\t//gormreuse:ignore\n\t_ = 1\n}\n\n", i)
		}
		return []string{b.String()}
	}},
	// n ignored functions.
	{name: "ignorefn", limit: growthLinear, shape: func(n int) []string {
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
	{name: "directives", limit: growthLinear, shape: func(n int) []string {
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
	{name: "unuseddir", limit: growthLinear, shape: func(n int) []string {
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
	// n getters of a field, each result reused (#308: inferred from the
	// field read).
	{name: "getters", limit: growthLinear, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("type repo struct{ db *gorm.DB }\n\n")
		for i := range n {
			fmt.Fprintf(&b, "func (r *repo) g%d() *gorm.DB { return r.db }\n\nfunc u%d(r *repo) { q := r.g%d(); q.Find(nil); q.Find(nil) }\n\n", i, i, i)
		}
		return []string{b.String()}
	}},
	// A chain of n getters, each returning the one before it on the same
	// receiver, and n callers that each read one of them on a struct they
	// build (#342: the reads compose, and each call selects the field of its
	// own receiver).
	{name: "getterchain", limit: growthLinear, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("type repo struct{ db *gorm.DB }\n\nfunc mk(db *gorm.DB) *gorm.DB { return db.Where(\"x\") }\n\nfunc (r *repo) g0() *gorm.DB { return r.db }\n\n")
		for i := 1; i < n; i++ {
			fmt.Fprintf(&b, "func (r *repo) g%d() *gorm.DB { return r.g%d() }\n\n", i, i-1)
		}
		for i := range n {
			fmt.Fprintf(&b, "func u%d(db *gorm.DB) { r := &repo{db: mk(db)}; q := r.g%d(); q.Find(nil); q.Find(nil) }\n\n", i, i)
		}
		return []string{b.String()}
	}},
	// A chain of n closures, each returning the call of the one before it,
	// the first one a captured immutable value, the last one's result
	// reused (#308: closureReturnsImmutable and inference).
	{name: "closuregetters", limit: growthLinear, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func f(db *gorm.DB) {\n\tconn := db.Session(&gorm.Session{})\n\tg0 := func() *gorm.DB { return conn }\n")
		for i := 1; i < n; i++ {
			fmt.Fprintf(&b, "\tg%d := func() *gorm.DB { return g%d() }\n", i, i-1)
		}
		fmt.Fprintf(&b, "\td := g%d()\n\td.Find(nil)\n\td.Find(nil)\n}\n", n-1)
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
	{name: "excltail", limit: growthQuadratic, shape: func(n int) []string {
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
	{name: "mergedpollute", limit: growthQuadratic, shape: func(n int) []string {
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
	// The code grows as n^2, and each of its n functions of n blocks asks
	// which blocks one reaches from each of them, so the steps grow as n^3
	// (#350).
	{name: "inferexp2", n: 16, limit: growthQuadratic, steps: growthCubic, shape: func(n int) []string {
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
	{name: "helpersign", limit: growthLinear, shape: func(n int) []string {
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
	{name: "closurepure", limit: growthLinear, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func f(db *gorm.DB) {\n")
		for i := range n {
			fmt.Fprintf(&b, "\t//gormreuse:pure\n\tp%d := func(db *gorm.DB) *gorm.DB { return db }\n\t_ = p%d(db)\n\t_ = p%d(db)\n", i, i, i)
		}
		b.WriteString("}\n")
		return []string{b.String()}
	}},
	// n closures with //gormreuse:pure after the opening brace, each called twice.
	{name: "closurepuresame", limit: growthLinear, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func f(db *gorm.DB) {\n")
		for i := range n {
			fmt.Fprintf(&b, "\tp%d := func(db *gorm.DB) *gorm.DB { //gormreuse:pure\n\t\treturn db\n\t}\n\t_ = p%d(db)\n\t_ = p%d(db)\n", i, i, i)
		}
		b.WriteString("}\n")
		return []string{b.String()}
	}},
	// n package-level closures with //gormreuse:pure above, each called twice.
	{name: "closurepurepkg", limit: growthLinear, shape: func(n int) []string {
		var b strings.Builder
		for i := range n {
			fmt.Fprintf(&b, "//gormreuse:pure\nvar p%d = func(db *gorm.DB) *gorm.DB { return db }\n\nfunc f%d(db *gorm.DB) {\n\t_ = p%d(db)\n\t_ = p%d(db)\n}\n\n", i, i, i, i)
		}
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
	// h0 gets a Session, and each hi forwards its parameter to h(i+1), also
	// from a closure: immutable-param inference proves one per round (#349).
	// The functions are written last first, so the first round proves h0 only.
	{name: "inferparam", limit: growthLinear, shape: func(n int) []string {
		var b strings.Builder
		fmt.Fprintf(&b, "func h%d(db *gorm.DB) {\n\tdb.Find(nil)\n\tdb.Find(nil)\n}\n\n", n)
		for i := n - 1; i >= 0; i-- {
			fmt.Fprintf(&b, "func h%d(db *gorm.DB) {\n\th%d(db)\n\tfunc() { h%d(db) }()\n}\n\n", i, i+1, i+1)
		}
		b.WriteString("func caller(db *gorm.DB) { h0(db.Session(&gorm.Session{})) }\n")
		return []string{b.String()}
	}},
	// Each wi forwards its callback to w(i+1), and the last to Transaction:
	// immutable-input inference proves one per step (#349).
	{name: "inferinput", limit: growthLinear, shape: func(n int) []string {
		var b strings.Builder
		fmt.Fprintf(&b, "func w%d(db *gorm.DB, fn func(*gorm.DB) error) error { return db.Transaction(fn) }\n\n", n)
		for i := n - 1; i >= 0; i-- {
			fmt.Fprintf(&b, "func w%d(db *gorm.DB, fn func(*gorm.DB) error) error { return w%d(db, fn) }\n\n", i, i+1)
		}
		b.WriteString("func caller(db *gorm.DB) error {\n\treturn w0(db, func(tx *gorm.DB) error {\n\t\ttx.Find(nil)\n\t\treturn tx.Find(nil).Error\n\t})\n}\n")
		return []string{b.String()}
	}},
	// Each wi calls its callback with the tx of a closure it hands to w(i+1).
	// That tx is immutable only once w(i+1) is inferred and the callbacks are
	// collected again, so each wi takes a collection of its own: quadratic
	// (#349).
	{name: "inferinputnest", limit: growthQuadratic, shape: func(n int) []string {
		var b strings.Builder
		fmt.Fprintf(&b, "func w%d(db *gorm.DB, fn func(*gorm.DB)) error {\n\treturn db.Transaction(func(tx *gorm.DB) error { fn(tx); return nil })\n}\n\n", n)
		for i := n - 1; i >= 0; i-- {
			fmt.Fprintf(&b, "func w%d(db *gorm.DB, fn func(*gorm.DB)) error {\n\treturn w%d(db, func(tx *gorm.DB) { fn(tx) })\n}\n\n", i, i+1)
		}
		b.WriteString("func caller(db *gorm.DB) error {\n\treturn w0(db, func(tx *gorm.DB) {\n\t\ttx.Find(nil)\n\t\ttx.Find(nil)\n\t})\n}\n")
		return []string{b.String()}
	}},
	// Discarded conditional chains (virtual roots).
	{name: "discarded", limit: growthQuadratic, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func f(db *gorm.DB, c []bool) {\n\tq := db.Model(nil)\n")
		for i := range n {
			fmt.Fprintf(&b, "\tif c[%d] {\n\t\tq.Where(\"c%d\")\n\t}\n", i, i)
		}
		b.WriteString("\tq.Find(nil)\n\tq.Find(nil)\n}\n")
		return []string{b.String()}
	}},
	// Switch with n exclusive uses, last case reuses, long tail.
	{name: "conflict", limit: growthQuadratic, shape: func(n int) []string {
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
	{name: "unusedign", limit: growthNearLinear, shape: func(n int) []string {
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
	// n roots each stored into a local struct that a call takes, then reused,
	// and n into the fields of a package-level struct with a call after each
	// (struct stores as escapes, #310).
	{name: "structescape", limit: growthNearLinear, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("type H struct{ db *gorm.DB }\n\nfunc use(h *H) {}\n\nfunc touch() {}\n\ntype G struct {\n")
		for i := range n {
			fmt.Fprintf(&b, "\tf%d *gorm.DB\n", i)
		}
		b.WriteString("}\n\nvar g G\n\nfunc f(db *gorm.DB) {\n")
		for i := range n {
			fmt.Fprintf(&b, "\tq%d := db.Where(\"c%d\")\n\tuse(&H{db: q%d})\n\tq%d.Find(nil)\n\tq%d.Find(nil)\n", i, i, i, i, i)
			fmt.Fprintf(&b, "\tr%d := db.Where(\"d%d\")\n\tg.f%d = r%d\n\ttouch()\n\tr%d.Find(nil)\n\tr%d.Find(nil)\n", i, i, i, i, i, i)
		}
		b.WriteString("}\n")
		return []string{b.String()}
	}},
	// n variables whose address a helper takes, and n that a helper may
	// rebind (#345).
	{name: "pointerescape", limit: growthLinear, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func apply(p **gorm.DB) {}\n\nfunc scan(p **gorm.DB, db *gorm.DB) {}\n\nfunc f(db *gorm.DB) {\n")
		for i := range n {
			fmt.Fprintf(&b, "\tq%d := db.Where(\"c%d\")\n\tapply(&q%d)\n\tq%d.Find(nil)\n\tq%d.Find(nil)\n", i, i, i, i, i)
			fmt.Fprintf(&b, "\tvar r%d *gorm.DB\n\tscan(&r%d, db)\n\tr%d.Find(nil)\n\tr%d.Find(nil)\n", i, i, i, i)
		}
		b.WriteString("}\n")
		return []string{b.String()}
	}},
	// n roots stored into the fields of a package-level struct, each followed
	// by a call that takes a function, which may reach the struct (#345).
	{name: "callbackescape", limit: growthLinear, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func each(f func()) {}\n\ntype G struct {\n")
		for i := range n {
			fmt.Fprintf(&b, "\tf%d *gorm.DB\n", i)
		}
		b.WriteString("}\n\nvar g G\n\nfunc f(db *gorm.DB) {\n")
		for i := range n {
			fmt.Fprintf(&b, "\ts%d := db.Where(\"d%d\")\n\tg.f%d = s%d\n\teach(func() {})\n\ts%d.Find(nil)\n\ts%d.Find(nil)\n", i, i, i, i, i, i)
		}
		b.WriteString("}\n")
		return []string{b.String()}
	}},
	// n roots, each used once and in a deferred call: the go and defer
	// statements and the state reads of the function are indexed by root
	// once, not scanned once per root (#350).
	{name: "rootsdefer", limit: growthLinear, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func f(db *gorm.DB) {\n")
		for i := range n {
			fmt.Fprintf(&b, "\tq%d := db.Where(\"c%d\")\n\tdefer q%d.Count(nil)\n\tq%d.Find(nil)\n", i, i, i, i)
		}
		b.WriteString("}\n")
		return []string{b.String()}
	}},
	// A chain of n immutable-return functions whose last breaks the
	// contract, and a hub that calls each of them on its own case without
	// returning its result. Distrust reaches the chain one function at a
	// time, and the hub is checked again once, after all of them (#350). Its
	// steps are quadratic: the hub's parameter is used on n exclusive cases,
	// and each use is checked against the others, as in usesbr.
	{name: "distrusthub", limit: growthLinear, steps: growthQuadratic, shape: func(n int) []string {
		files := make([]string, n+1)
		var hub strings.Builder
		hub.WriteString("//gormreuse:immutable-return\nfunc H(db *gorm.DB, k int) *gorm.DB {\n\tswitch k {\n")
		for i := range n {
			next := "db.Where(\"bad\")"
			if i < n-1 {
				next = fmt.Sprintf("R%d(db)", i+1)
			}
			files[i] = fmt.Sprintf("//gormreuse:immutable-return\nfunc R%d(db *gorm.DB) *gorm.DB {\n\treturn %s\n}\n", i, next)
			fmt.Fprintf(&hub, "\tcase %d:\n\t\t_ = R%d(db)\n\t\treturn db.Session(&gorm.Session{})\n", i, i)
		}
		hub.WriteString("\t}\n\treturn nil\n}\n")
		files[n] = hub.String()
		return files
	}},
	// One map literal of n mutable entries, each read twice: the referrers of
	// the map are read once for all its keys (#374).
	{name: "mapreads", limit: growthNearLinear, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func f(db *gorm.DB) {\n\tbase := db.Session(&gorm.Session{})\n\tm := map[string]*gorm.DB{\n")
		for i := range n {
			fmt.Fprintf(&b, "\t\t\"k%d\": base.Where(\"c%d\"),\n", i, i)
		}
		b.WriteString("\t}\n")
		for i := range n {
			fmt.Fprintf(&b, "\tm[\"k%d\"].Find(nil)\n\tm[\"k%d\"].Count(nil)\n", i, i)
		}
		b.WriteString("}\n")
		return []string{b.String()}
	}},
	// One []any literal of n values next to a mutable root: a store of a
	// value that is no function literal never walks the container (#397).
	{name: "anyliteral", limit: growthNearLinear, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func use([]any) {}\n\nfunc f(db *gorm.DB) {\n\tq := db.Where(\"x\")\n\txs := []any{\n")
		for i := range n {
			fmt.Fprintf(&b, "\t\t%d,\n", i)
		}
		b.WriteString("\t}\n\tuse(xs)\n\tq.Find(nil)\n}\n")
		return []string{b.String()}
	}},
	// One local slice of n function literals that return a captured root,
	// each called in a loop: the container is walked once for all its stores
	// (#397).
	{name: "literalslice", limit: growthNearLinear, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func f(db *gorm.DB) {\n\tq := db.Where(\"x\")\n\tfs := []func() *gorm.DB{\n")
		for range n {
			b.WriteString("\t\tfunc() *gorm.DB { return q },\n")
		}
		b.WriteString("\t}\n\tfor _, f := range fs {\n\t\tf()\n\t}\n\tq.Find(nil)\n}\n")
		return []string{b.String()}
	}},
	// A chain of n literals, each handing the one before it on: whether a
	// literal returns a captured *gorm.DB is decided once per literal
	// (closureReturnsCapturedGormDB, #397).
	{name: "literalchain", n: 400, limit: growthLinear, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func run(func() *gorm.DB) {}\n\nfunc f(db *gorm.DB) {\n\tq := db.Where(\"x\")\n\tf0 := func() *gorm.DB { return q }\n")
		for i := 1; i <= n; i++ {
			fmt.Fprintf(&b, "\tf%d := func() *gorm.DB { run(f%d); return nil }\n", i, i-1)
		}
		fmt.Fprintf(&b, "\trun(f%d)\n}\n", n)
		return []string{b.String()}
	}},
	// n loops that each append a query of their own to a slice, then a loop
	// that reads it: each append runs again, and the values the slice may
	// hold are found once (#374). Its steps are quadratic, but no more than
	// with q%d.Find(nil) in place of each append, which counts the same
	// steps within 1%: they come from the n loops.
	{name: "appendloops", limit: growthNearLinear, steps: growthQuadratic, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func f(db *gorm.DB, xs []int) {\n\tbase := db.Session(&gorm.Session{})\n\tvar qs []*gorm.DB\n")
		for i := range n {
			fmt.Fprintf(&b, "\tq%d := base.Where(\"c%d\")\n\tfor range xs {\n\t\tqs = append(qs, q%d)\n\t}\n", i, i, i)
		}
		b.WriteString("\tfor _, e := range qs {\n\t\te.Find(nil)\n\t\te.Count(nil)\n\t}\n}\n")
		return []string{b.String()}
	}},
	// n getters of one field, which n methods assign: whether the package
	// assigns the field a mutable value is decided once (#350).
	{name: "getterfield", limit: growthLinear, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("type repo struct{ db *gorm.DB }\n\n")
		for i := range n {
			fmt.Fprintf(&b, "func (r *repo) g%d() *gorm.DB { return r.db }\n\nfunc (r *repo) s%d(db *gorm.DB) { r.db = db.Session(&gorm.Session{}) }\n\n", i, i)
		}
		return []string{b.String()}
	}},
	// n optional filters, then a use in a loop: build the filters, then page
	// through the results. The fix checks each merged edge for state a
	// Session drops, and each check walked the whole Phi chain, once per
	// root (#377).
	{name: "filtersloopuse", n: 25, limit: growthQuadratic, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func f(db *gorm.DB, xs []int, c []bool) {\n\tq := db.Model(nil)\n")
		for i := range n {
			fmt.Fprintf(&b, "\tif c[%d] {\n\t\tq = q.Where(\"z\")\n\t}\n", i)
		}
		b.WriteString("\tfor range xs {\n\t\tq.Find(nil)\n\t}\n}\n")
		return []string{b.String()}
	}},
	// n loops, each with an optional filter and a use inside (#377).
	// Its steps are cubic: the use in loop i may see the roots of the i
	// merges before it, the fix of each such root has a Session for each of
	// the merges before it, and the diagnostic merges those fixes, an edit
	// at a time, with no allocation for an edit seen before. The rest of
	// the steps are quadratic.
	{name: "loopfilteruse", n: 25, limit: growthQuadratic, steps: growthCubic, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func f(db *gorm.DB, xs []int, c []bool) {\n\tq := db.Model(nil)\n")
		for i := range n {
			fmt.Fprintf(&b, "\tfor range xs {\n\t\tif c[%d] {\n\t\t\tq = q.Where(\"z\")\n\t\t}\n\t\tq.Count(nil)\n\t}\n", i)
		}
		b.WriteString("}\n")
		return []string{b.String()}
	}},
	// n loops using one query, each followed by an optional filter (#377).
	// Its steps are cubic, as loopfilteruse's are.
	{name: "loopsfilters", n: 25, limit: growthQuadratic, steps: growthCubic, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func f(db *gorm.DB, xs []int, c []bool) {\n\tq := db.Model(nil)\n")
		for i := range n {
			fmt.Fprintf(&b, "\tfor range xs {\n\t\tq.Where(\"c%d\").Find(nil)\n\t}\n\tif c[%d] {\n\t\tq = q.Where(\"z\")\n\t}\n", i, i)
		}
		b.WriteString("}\n")
		return []string{b.String()}
	}},
	// The same, with the uses in go statements (#377).
	{name: "goloopsfilters", n: 25, limit: growthQuadratic, steps: growthCubic, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func f(db *gorm.DB, xs []int, c []bool) {\n\tq := db.Model(nil)\n")
		for i := range n {
			fmt.Fprintf(&b, "\tfor range xs {\n\t\tgo q.Where(\"c%d\").Find(nil)\n\t}\n\tif c[%d] {\n\t\tq = q.Where(\"z\")\n\t}\n", i, i)
		}
		b.WriteString("}\n")
		return []string{b.String()}
	}},
	// An else-if ladder of n filters, then two uses. IfStmt.End recurses
	// down the else chain, so checking every statement of the file for the
	// statement of each use was cubic, and allocated little (#377).
	{name: "elseifs", limit: growthNearLinear, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func f(db *gorm.DB, k int) {\n\tq := db.Model(nil)\n\tif k == -1 {\n\t}")
		for i := range n {
			fmt.Fprintf(&b, " else if k == %d {\n\t\tq = q.Where(\"c%d\")\n\t}", i, i)
		}
		b.WriteString("\n\tq.Count(nil)\n\tq.Find(nil)\n}\n")
		return []string{b.String()}
	}},
	// n functions with an optional filter, a discarded chain and two uses.
	// The statement of each use, and the call that ends each reassigned
	// chain, are looked up in indexes of the file, not by walking it (#377).
	{name: "filterfuncs", limit: growthLinear, shape: func(n int) []string {
		var b strings.Builder
		for i := range n {
			fmt.Fprintf(&b, "func f%d(db *gorm.DB, c bool) {\n\tq := db.Model(nil)\n\tif c {\n\t\tq = q.Where(\"a\")\n\t}\n\tq.Where(\"b\")\n\tq.Count(nil)\n\tq.Find(nil)\n}\n\n", i)
		}
		return []string{b.String()}
	}},
	// n functions that reuse their parameter, each called once with a
	// mutable value. Whether each is called only directly, and its calls,
	// are looked up in indexes of the package, not by walking it once per
	// parameter (#377).
	{name: "paramcallers", limit: growthLinear, shape: func(n int) []string {
		var b strings.Builder
		for i := range n {
			fmt.Fprintf(&b, "func h%d(db *gorm.DB) {\n\tdb.Find(nil)\n\tdb.Find(nil)\n}\n\nfunc c%d(db *gorm.DB) { h%d(db.Where(\"x\")) }\n\n", i, i, i)
		}
		return []string{b.String()}
	}},
	// n functions that reuse their parameter, one per file (#377).
	{name: "paramfiles", limit: growthLinear, shape: func(n int) []string {
		files := make([]string, n)
		for i := range n {
			files[i] = fmt.Sprintf("func f%d(db *gorm.DB) {\n\tdb.Find(nil)\n\tdb.Count(nil)\n}\n", i)
		}
		return files
	}},
	// n loops whose closure uses one query, each followed by an optional
	// filter (#389). The diagnostics share the root's hidden uses, and the
	// error check walks each value once for all uses. The hidden uses cost
	// bytes more than allocations, so this gate is looser than the 28 GB
	// they once took at n=800.
	{name: "loopclosurefilters", limit: growthQuadratic, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func f(db *gorm.DB, xs []int, c []bool) {\n\tq := db.Model(nil)\n")
		for i := range n {
			fmt.Fprintf(&b, "\tfor range xs {\n\t\tfunc() { q.Where(\"c%d\").Find(nil) }()\n\t}\n\tif c[%d] {\n\t\tq = q.Where(\"z\")\n\t}\n", i, i)
		}
		b.WriteString("}\n")
		return []string{b.String()}
	}},
	// n closures called in place, each reassigning one captured query, then
	// two uses (#389). Each use's error check reaches the n loads of q.
	{name: "closurereassigns", limit: growthLinear, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func f(db *gorm.DB) {\n\tq := db.Model(nil)\n")
		for i := range n {
			fmt.Fprintf(&b, "\tfunc() { q = q.Where(\"c%d\") }()\n", i)
		}
		b.WriteString("\tq.Count(nil)\n\tq.Find(nil)\n}\n")
		return []string{b.String()}
	}},
	// The same with stored closures, each called later (#389).
	{name: "storedreassigns", limit: growthLinear, shape: func(n int) []string {
		var b strings.Builder
		b.WriteString("func f(db *gorm.DB) {\n\tq := db.Model(nil)\n")
		for i := range n {
			fmt.Fprintf(&b, "\tw%d := func() { q = q.Where(\"c%d\") }\n", i, i)
		}
		for i := range n {
			fmt.Fprintf(&b, "\tw%d()\n\tq.Find(nil)\n", i)
		}
		b.WriteString("}\n")
		return []string{b.String()}
	}},
}
