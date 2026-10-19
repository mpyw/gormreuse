package gormreuse_test

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/mpyw/gormreuse"
)

func TestAnalyzer(t *testing.T) {
	t.Parallel()
	testdata := analysistest.TestData()
	analysistest.Run(t, testdata, gormreuse.Analyzer, "gormreuse")
}

func TestFileFilter(t *testing.T) {
	t.Parallel()
	testdata := analysistest.TestData()
	// Tests that generated files are skipped
	analysistest.Run(t, testdata, gormreuse.Analyzer, "filefilter")
}

// TestIgnoreFixWithTestFiles checks that an unused ignore in a non-test file
// of a package with test files keeps its diagnostic but gets no fix, in both
// variants of the package, while one in a test file gets the fix (#248).
func TestIgnoreFixWithTestFiles(t *testing.T) {
	t.Parallel()
	results := analysistest.Run(t, analysistest.TestData(), gormreuse.Analyzer, "ignoretests")
	seen := map[string]int{}
	for _, result := range results {
		for _, diag := range result.Diagnostics {
			name := filepath.Base(result.Pass.Fset.Position(diag.Pos).Filename)
			seen[name]++
			if hasFix, want := len(diag.SuggestedFixes) > 0, name == "code_test.go"; hasFix != want {
				t.Errorf("%s: %s: has a fix: %v, want %v", name, diag.Message, hasFix, want)
			}
		}
	}
	if seen["code.go"] == 0 || seen["code_test.go"] == 0 {
		t.Errorf("diagnostics by file = %v, want some in code.go and code_test.go", seen)
	}
}

// TestLineDirective verifies that //line directives do not move ignore and
// pure directives, the generated-file skip, or the line keys of ignores
// (issues #151, #152, #153).
func TestLineDirective(t *testing.T) {
	t.Parallel()
	testdata := analysistest.TestData()
	analysistest.RunWithSuggestedFixes(t, testdata, gormreuse.Analyzer, "linedirective")
}

// TestCrossPackageDirectives checks directives declared in one package
// (purelib) and used from another (externalpure) through the facts that
// purelib's analysis exports (#195, #197).
func TestCrossPackageDirectives(t *testing.T) {
	t.Parallel()
	testdata := analysistest.TestData()
	analysistest.Run(t, testdata, gormreuse.Analyzer, "purelib", "externalpure")
}

// TestCgoFiles verifies that the files of a package that imports "C" are
// analyzed, and that a file marked generated is still skipped (#186). It needs
// cgo and a C compiler, and is skipped without them.
func TestCgoFiles(t *testing.T) {
	t.Parallel()
	out, err := exec.Command("go", "env", "CGO_ENABLED", "CC").Output()
	if err != nil {
		t.Skipf("go env failed: %v", err)
	}
	env := strings.Fields(string(out))
	if len(env) < 2 || env[0] != "1" {
		t.Skip("cgo is disabled")
	}
	if _, err := exec.LookPath(env[1]); err != nil {
		t.Skipf("no C compiler: %v", err)
	}
	testdata := analysistest.TestData()
	analysistest.Run(t, testdata, gormreuse.Analyzer, "cgofile")
}

func TestSuggestedFixes(t *testing.T) {
	t.Parallel()
	testdata := analysistest.TestData()
	analysistest.RunWithSuggestedFixes(t, testdata, gormreuse.Analyzer, "gormreuse")
}

func TestSuggestedFixesWithImport(t *testing.T) {
	t.Parallel()
	testdata := analysistest.TestData()
	analysistest.RunWithSuggestedFixes(t, testdata, gormreuse.Analyzer, "noimport")
}

// TestSuggestedFixesWithAlias verifies the Session fix uses the file's local
// name for gorm (g.Session) under an aliased import, so the output compiles
// (issue #71, defect 3).
func TestSuggestedFixesWithAlias(t *testing.T) {
	t.Parallel()
	testdata := analysistest.TestData()
	analysistest.RunWithSuggestedFixes(t, testdata, gormreuse.Analyzer, "aliasimport")
}
