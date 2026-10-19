package gormreuse_test

import (
	"cmp"
	"fmt"
	"go/token"
	"os"
	"strings"
	"testing"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/analysistest"
	"golang.org/x/tools/go/analysis/checker"
	"golang.org/x/tools/go/packages"

	"github.com/mpyw/gormreuse"
)

// determinismRuns is how many times TestDeterministicDiagnostics re-runs the
// analyzer. Go randomizes the order of every map range, and the fixture has
// several positions shared by 2-3 roots, so a dependency on map order shows up
// within a few runs.
const determinismRuns = 50

// TestDeterministicDiagnostics runs the analyzer many times over a fixture where
// several roots are reused at the same position, and requires every run to
// produce the same diagnostics: positions, messages, and suggested fixes (#184),
// in position order (#273).
// Output that changes between runs breaks golden tests, golangci-lint caching,
// and -fix reproducibility.
func TestDeterministicDiagnostics(t *testing.T) {
	t.Parallel()
	testdata := analysistest.TestData()

	// analysistest checks the `// want` comments, which pin which root wins a
	// shared position.
	analysistest.Run(t, testdata, gormreuse.Analyzer, "determinism")

	// Load the package once, then re-run only the analysis: each run builds SSA
	// and runs the analyzer from scratch.
	pkgs, err := packages.Load(&packages.Config{
		Mode: packages.LoadAllSyntax,
		Dir:  testdata,
		Env:  append(os.Environ(), "GOPATH="+testdata, "GO111MODULE=off", "GOWORK=off"),
	}, "determinism")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if packages.PrintErrors(pkgs) > 0 {
		t.Fatal("the fixture has load errors")
	}

	want := analyzeOnce(t, pkgs)
	if !strings.Contains(want, "fix:") {
		t.Fatalf("no suggested fixes; the fixture no longer exercises fix selection:\n%s", want)
	}
	for i := range determinismRuns {
		if got := analyzeOnce(t, pkgs); got != want {
			t.Fatalf("run %d differs from the first run\n--- first ---\n%s\n--- run %d ---\n%s", i+2, want, i+2, got)
		}
	}
}

// analyzeOnce runs the analyzer over pkgs and renders every diagnostic with its
// suggested fixes in report order, using file:line:column positions. The -json
// output lists diagnostics in report order, so it fails unless they are
// reported by file, then offset (#273).
func analyzeOnce(t *testing.T, pkgs []*packages.Package) string {
	t.Helper()
	graph, err := checker.Analyze([]*analysis.Analyzer{gormreuse.Analyzer}, pkgs, nil)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}

	var b strings.Builder
	for _, act := range graph.Roots {
		if act.Err != nil {
			t.Fatalf("analyze %s: %v", act.Package.PkgPath, act.Err)
		}
		fset := act.Package.Fset
		var prev token.Position
		for _, d := range act.Diagnostics {
			at := fset.Position(d.Pos)
			if cmp.Or(cmp.Compare(at.Filename, prev.Filename), cmp.Compare(at.Offset, prev.Offset)) < 0 {
				t.Fatalf("%s is reported after %s; diagnostics must be reported in position order", at, prev)
			}
			prev = at
			fmt.Fprintf(&b, "%s: %s\n", fset.Position(d.Pos), d.Message)
			for _, f := range d.SuggestedFixes {
				fmt.Fprintf(&b, "  fix: %s\n", f.Message)
				for _, e := range f.TextEdits {
					fmt.Fprintf(&b, "    %s-%s %q\n", fset.Position(e.Pos), fset.Position(e.End), e.NewText)
				}
			}
		}
	}
	return b.String()
}
