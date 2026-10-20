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

	"github.com/mpyw/gormreuse"
)

// TestPurePhiAllocationGrowth checks that tracing roots through a pure call
// on one edge of each Phi is not exponential (#297). Each `if c { q = p(q) }
// else { side() }` used to double the work, so n+4 statements took 16 times
// the allocations of n. The gate of #302 replaces this test.
func TestPurePhiAllocationGrowth(t *testing.T) {
	if testing.Short() {
		t.Skip("allocation-growth test")
	}
	small := purephiMallocs(t, 16)
	large := purephiMallocs(t, 20)
	t.Logf("mallocs: n=16 %d, n=20 %d (%.2fx)", small, large, float64(large)/float64(small))
	if float64(large) > 2*float64(small) {
		t.Errorf("mallocs grew %.2fx from n=16 to n=20, want at most 2x", float64(large)/float64(small))
	}
}

// purephiMallocs returns the heap allocations of the analyzer's Run on a
// package of n pure-call Phis.
func purephiMallocs(t *testing.T, n int) uint64 {
	t.Helper()
	gopath := t.TempDir()
	if err := os.CopyFS(filepath.Join(gopath, "src", "gorm.io", "gorm"),
		os.DirFS(filepath.Join(analysistest.TestData(), "src", "gorm.io", "gorm"))); err != nil {
		t.Fatalf("copy gorm stub: %v", err)
	}
	pkgDir := filepath.Join(gopath, "src", "purephi")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "purephi.go"), []byte(purephiSource(n)), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	var mallocs uint64
	counted := *gormreuse.Analyzer
	counted.Run = func(pass *analysis.Pass) (any, error) {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		result, err := gormreuse.Analyzer.Run(pass)
		runtime.ReadMemStats(&after)
		if pass.Pkg.Path() == "purephi" {
			mallocs = after.Mallocs - before.Mallocs
		}
		return result, err
	}
	analysistest.Run(t, gopath, &counted, "purephi")
	return mallocs
}

// purephiSource returns a function that passes a query through a pure
// function on one branch of each of n ifs, then runs it twice.
func purephiSource(n int) string {
	var b strings.Builder
	b.WriteString("package purephi\n\nimport \"gorm.io/gorm\"\n\n//gormreuse:pure\nfunc p(db *gorm.DB) *gorm.DB { return db }\n\nfunc side() {}\n\nfunc f(db *gorm.DB, c []bool) {\n\tq := db.Where(\"x\")\n")
	for i := range n {
		fmt.Fprintf(&b, "\tif c[%d] {\n\t\tq = p(q)\n\t} else {\n\t\tside()\n\t}\n", i)
	}
	b.WriteString("\tq.Find(nil)\n\tq.Find(nil) // want `\\*gorm\\.DB reused: second branch from mutable root`\n}\n")
	return b.String()
}
