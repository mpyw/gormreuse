package gormreuse_test

import (
	"os/exec"
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
