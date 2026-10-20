package gormreuse_test

import (
	"fmt"
	"go/ast"
	"go/token"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/mpyw/gormreuse"
	"github.com/mpyw/gormreuse/internal/goldentest"
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
// of a package with test files is reported only by the test variant, without a
// fix, while one in a test file gets the fix (#248). The variant without the
// test files leaves it to the test variant, so the ignore in needed.go, which
// only the test variant needs, is reported by neither, and both variants run
// clean once the unused ignores are gone (#348). A want comment cannot say
// which variant reports, so the test runs without them.
func TestIgnoreFixWithTestFiles(t *testing.T) {
	t.Parallel()
	results := analysistest.Run(goldentest.NoopT{}, analysistest.TestData(), gormreuse.Analyzer, "ignoretests")
	got := map[string][]string{} // By variant: "test" or "base"
	for _, result := range results {
		if result.Err != nil {
			t.Fatalf("analyze: %v", result.Err)
		}
		variant := "base"
		for _, f := range result.Pass.Files {
			if strings.HasSuffix(result.Pass.Fset.Position(f.Pos()).Filename, "_test.go") {
				variant = "test"
			}
		}
		for _, diag := range result.Diagnostics {
			at := result.Pass.Fset.Position(diag.Pos)
			got[variant] = append(got[variant], fmt.Sprintf("%s:%d: %s (fix: %v)", filepath.Base(at.Filename), at.Line, diag.Message, len(diag.SuggestedFixes) > 0))
		}
	}
	want := map[string][]string{
		"test": {
			"code.go:11: unused gormreuse:ignore directive (fix: false)",
			"code_test.go:9: unused gormreuse:ignore directive (fix: true)",
		},
	}
	for _, variant := range []string{"base", "test"} {
		if !slices.Equal(got[variant], want[variant]) {
			t.Errorf("%s variant: diagnostics = %q, want %q", variant, got[variant], want[variant])
		}
	}
}

// TestParamFixWithTestFiles checks that a parameter root declared outside the
// test files of a package with test files gets the body Session in both
// variants of the package, so their fixes agree, while one declared in a test
// file may get immutable-param (#245). A discarded statement on a parameter
// whose function returns a *gorm.DB gets no fix outside the test files: the
// variant without them cannot see whether every caller reads the result
// (#312).
func TestParamFixWithTestFiles(t *testing.T) {
	t.Parallel()
	results := analysistest.Run(t, analysistest.TestData(), gormreuse.Analyzer, "paramtests")
	seen := map[string]int{}
	for _, result := range results {
		for _, diag := range result.Diagnostics {
			name := filepath.Base(result.Pass.Fset.Position(diag.Pos).Filename)
			seen[name]++
			var text strings.Builder
			for _, fix := range diag.SuggestedFixes {
				for _, edit := range fix.TextEdits {
					text.Write(edit.NewText)
				}
			}
			got, want := text.String(), "tx = tx.Session(&gorm.Session{})"
			switch {
			case name == "code_test.go":
				want = "//gormreuse:immutable-param"
			case inParamTestsTenant(result.Pass.Files, diag.Pos):
				if got != "" {
					t.Errorf("%s: %s: fix inserts %q, want no fix", name, diag.Message, got)
				}
				continue
			}
			if !strings.Contains(got, want) {
				t.Errorf("%s: %s: fix inserts %q, want %q", name, diag.Message, got, want)
			}
		}
	}
	// The variant without the test files and the test variant both report
	// code.go.
	if seen["code.go"] != 4 || seen["code_test.go"] != 1 {
		t.Errorf("diagnostics by file = %v, want 4 in code.go and 1 in code_test.go", seen)
	}
}

// inParamTestsTenant reports whether pos is in the declaration of
// paramTestsTenant, whose diagnostic has no fix (see TestParamFixWithTestFiles).
func inParamTestsTenant(files []*ast.File, pos token.Pos) bool {
	for _, f := range files {
		for _, decl := range f.Decls {
			if fd, ok := decl.(*ast.FuncDecl); ok && fd.Name.Name == "paramTestsTenant" && fd.Pos() <= pos && pos < fd.End() {
				return true
			}
		}
	}
	return false
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
	if reason := cgoUnavailable(); reason != "" {
		t.Skip(reason)
	}
	testdata := analysistest.TestData()
	analysistest.Run(t, testdata, gormreuse.Analyzer, "cgofile")
}

// cgoUnavailable returns why the cgofile fixtures cannot be loaded, or "" when
// cgo is enabled and its C compiler is on PATH.
//
//declscope:shared // golden_test.go's goldenPackages leaves cgofile out with it
func cgoUnavailable() string {
	out, err := exec.Command("go", "env", "CGO_ENABLED", "CC").Output()
	if err != nil {
		return fmt.Sprintf("go env failed: %v", err)
	}
	env := strings.Fields(string(out))
	if len(env) < 2 || env[0] != "1" {
		return "cgo is disabled"
	}
	if _, err := exec.LookPath(env[1]); err != nil {
		return fmt.Sprintf("no C compiler: %v", err)
	}
	return ""
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
