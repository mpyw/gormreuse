package gormreuse_test

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/mpyw/gormreuse"
)

// performanceLimit is how long the analyzer may run on one generated package,
// not counting loading it or building its SSA. Each case took about 10 s
// before #260 and takes under 0.2 s after, so the limit leaves room for slow
// machines. The race detector slows the analyzer down about tenfold, so the
// limit is raised under it.
func performanceLimit() time.Duration {
	limit := 2 * time.Second
	if info, ok := debug.ReadBuildInfo(); ok && slices.Contains(info.Settings, debug.BuildSetting{Key: "-race", Value: "true"}) {
		limit *= 10
	}
	return limit
}

// TestPerformance analyzes generated functions that used to take time cubic or
// quadratic in their size (#260), and fails when one takes longer than
// performanceLimit. The reports must still be the expected ones.
func TestPerformance(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test")
	}
	const n = 400
	cases := map[string]string{
		// Many optional filters on one query: each `q = q.Where(...)` adds a
		// Phi edge that every later call traced again.
		"filters": generatedFilters(n),
		// The same, with a Session copy taken at each step, so that each
		// call also looks for the roots its copies alias.
		"session_filters": generatedSessionFilters(n),
		// A chain of pure functions, each returning what the next one
		// returns of its argument: validation took one round per function.
		"pure_chain": generatedPureChain(n),
	}
	testdata := analysistest.TestData()
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			gopath := t.TempDir()
			if err := os.CopyFS(filepath.Join(gopath, "src", "gorm.io", "gorm"),
				os.DirFS(filepath.Join(testdata, "src", "gorm.io", "gorm"))); err != nil {
				t.Fatalf("copy gorm stub: %v", err)
			}
			pkgDir := filepath.Join(gopath, "src", "perf")
			if err := os.MkdirAll(pkgDir, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if err := os.WriteFile(filepath.Join(pkgDir, "perf.go"), []byte(src), 0o644); err != nil {
				t.Fatalf("write source: %v", err)
			}
			// Time the analyzer's own Run on the generated package only:
			// loading it takes about as long and varies from run to run.
			var elapsed time.Duration
			timed := *gormreuse.Analyzer
			timed.Run = func(pass *analysis.Pass) (any, error) {
				start := time.Now()
				result, err := gormreuse.Analyzer.Run(pass)
				if pass.Pkg.Path() == "perf" {
					elapsed = time.Since(start)
				}
				return result, err
			}
			analysistest.Run(t, gopath, &timed, "perf")
			t.Logf("%s (n=%d): %v", name, n, elapsed)
			if limit := performanceLimit(); elapsed > limit {
				t.Errorf("%s (n=%d) took %v, want under %v", name, n, elapsed, limit)
			}
		})
	}
}

const performanceWant = " // want `\\*gorm\\.DB reused: second branch from mutable root`\n"

// generatedFilters returns a function that adds n optional filters to one
// query, then runs it twice.
func generatedFilters(n int) string {
	var b strings.Builder
	b.WriteString("package perf\n\nimport \"gorm.io/gorm\"\n\nfunc filters(db *gorm.DB, c []bool) {\n\tq := db.Model(nil)\n")
	for i := range n {
		fmt.Fprintf(&b, "\tif c[%d] {\n\t\tq = q.Where(\"c%d = ?\", %d)\n\t}\n", i, i, i)
	}
	b.WriteString("\tvar count int64\n\tq.Count(&count)\n\tq.Limit(10).Find(nil)" + performanceWant + "}\n")
	return b.String()
}

// generatedSessionFilters returns a function that adds n optional filters to
// one query, each through a Session copy of the query so far, then runs it
// twice.
func generatedSessionFilters(n int) string {
	var b strings.Builder
	b.WriteString("package perf\n\nimport \"gorm.io/gorm\"\n\nfunc sessionFilters(db *gorm.DB, c []bool) {\n\tq := db.Model(nil)\n")
	for i := range n {
		fmt.Fprintf(&b, "\tif c[%d] {\n\t\tq = q.Session(&gorm.Session{}).Where(\"c%d = ?\", %d)\n\t}\n", i, i, i)
	}
	b.WriteString("\tvar count int64\n\tq.Count(&count)\n\tq.Limit(10).Find(nil)" + performanceWant + "}\n")
	return b.String()
}

// generatedPureChain returns n pure functions, each returning what the next
// one returns of its argument, and a caller that branches the result of the
// first one twice.
func generatedPureChain(n int) string {
	var b strings.Builder
	b.WriteString("package perf\n\nimport \"gorm.io/gorm\"\n\n")
	for i := range n {
		next := "db"
		if i < n-1 {
			next = fmt.Sprintf("p%d(db)", i+1)
		}
		fmt.Fprintf(&b, "//gormreuse:pure\nfunc p%d(db *gorm.DB) *gorm.DB {\n\treturn %s\n}\n\n", i, next)
	}
	b.WriteString("func caller(db *gorm.DB) {\n\tq := p0(db.Where(\"x\"))\n\tq.Find(nil)\n\tq.Find(nil)" + performanceWant + "}\n")
	return b.String()
}
