package gormreuse_test

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/mpyw/gormreuse"
	"github.com/mpyw/gormreuse/internal/goldentest"
)

// wantComment strips analysistest `// want ...` directives from fixed source so
// the re-lint pass asserts on real diagnostics, not the originals.
var wantComment = regexp.MustCompile(`(?m)[ \t]*// want .*$`)

// convergeGormreuseFixtures are the fixtures of testdata/src/gormreuse whose
// fixes must also resolve every diagnostic in one pass. TestFixesConverge
// re-lints their committed .golden, which TestGoldenFiles keeps equal to the
// current fixes, so the fixture is not copied into testdata/src/converge
// (#264).
var convergeGormreuseFixtures = []string{"fix_paths.go", "fix_isolate_arg.go", "fix_later_loop_merge.go", "fix_closure_param.go"}

// TestFixesConverge is the convergence harness for #71: it applies ONE pass of
// the suggested fixes to every fixture in testdata/src/converge, then RE-LINTS
// the fixed output in a fresh GOPATH and asserts the analyzer reports nothing,
// so a fix that needs a second -fix pass fails it (#202). This is the check
// RunWithSuggestedFixes never does (it only diffs one pass against a golden),
// which is why wrong/non-convergent fixes could hide in CI. The
// convergeGormreuseFixtures are re-linted with them.
//
// So that it cannot pass vacuously (#203), it also requires at least one
// fixture, requires each fixture's fixes to change it, and requires the fixed
// output to type-check (analysistest.Run only logs type errors).
//
// The package is analyzed once and the fixed files are re-linted together, as
// one package, like TestGoldenFiles (#195, #264).
func TestFixesConverge(t *testing.T) {
	t.Parallel()
	testdata := analysistest.TestData()
	srcDir := filepath.Join(testdata, "src", "converge")

	fixtures, err := goldentest.Fixtures(srcDir)
	if err != nil {
		t.Fatalf("list fixtures: %v", err)
	}
	if len(fixtures) == 0 {
		t.Fatalf("no fixtures in %s", srcDir)
	}
	run, err := goldentest.Analyze(testdata, "converge", gormreuse.Analyzer)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}

	// Re-lint the fixed output in an isolated GOPATH containing the gorm stub
	// and the fixed files. analysistest.Run fails on any diagnostic (there are
	// no `// want` comments left), so a non-convergent or wrong fix — one that
	// still triggers a violation — is caught here.
	gopath := t.TempDir()
	if err := os.CopyFS(filepath.Join(gopath, "src", "gorm.io", "gorm"),
		os.DirFS(filepath.Join(testdata, "src", "gorm.io", "gorm"))); err != nil {
		t.Fatalf("copy gorm stub: %v", err)
	}
	pkgDir := filepath.Join(gopath, "src", "converge")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFixed := func(name string, original, fixed []byte, pkgClause string) {
		if bytes.Equal(original, fixed) {
			t.Errorf("the suggested fixes did not change %s; every converge fixture must need a fix", name)
		}
		fixed = bytes.Replace(fixed, []byte(pkgClause), []byte("package converge"), 1)
		fixed = wantComment.ReplaceAll(fixed, nil)
		if err := os.WriteFile(filepath.Join(pkgDir, name), fixed, 0o644); err != nil {
			t.Fatalf("write fixed: %v", err)
		}
	}

	for _, name := range fixtures {
		original, fixed, err := run.ApplyFixes(filepath.Join(srcDir, name))
		if err != nil {
			t.Fatalf("apply fixes to %s: %v", name, err)
		}
		writeFixed(name, original, fixed, "package converge")
	}
	for _, name := range convergeGormreuseFixtures {
		src := filepath.Join(testdata, "src", "gormreuse", name)
		original, err := os.ReadFile(src)
		if err != nil {
			t.Fatalf("read fixture: %v", err)
		}
		golden, err := os.ReadFile(src + ".golden")
		if err != nil {
			t.Fatalf("read golden: %v", err)
		}
		writeFixed("gormreuse_"+name, original, golden, "package internal")
	}

	for _, result := range analysistest.Run(t, gopath, gormreuse.Analyzer, "converge") {
		for _, err := range result.Action.Package.Errors {
			t.Errorf("fixed output does not compile: %v", err)
		}
	}
}
