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

// TestFixesConverge is the convergence harness for #71: for each curated
// fully-fixable fixture, it applies the suggested fixes, then RE-LINTS the fixed
// output in a fresh GOPATH and asserts the analyzer reports nothing. This is the
// check RunWithSuggestedFixes never does (it only diffs one pass against a
// golden), which is why wrong/non-convergent fixes could hide in CI.
//
// So that it cannot pass vacuously (#203), it also requires at least one
// fixture, requires each fixture's fixes to change it, and requires the fixed
// output to type-check (analysistest.Run only logs type errors).
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

	for _, name := range fixtures {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			src := filepath.Join(srcDir, name)
			original, fixed, err := goldentest.ApplyFixes(testdata, "converge", src, gormreuse.Analyzer)
			if err != nil {
				t.Fatalf("apply fixes: %v", err)
			}
			if bytes.Equal(original, fixed) {
				t.Fatalf("the suggested fixes did not change %s; every converge fixture must need a fix", name)
			}
			fixed = wantComment.ReplaceAll(fixed, nil)

			// Re-lint the fixed output in an isolated GOPATH containing the gorm
			// stub and the fixed file. analysistest.Run fails on any diagnostic
			// (there are no `// want` comments left), so a non-convergent or
			// wrong fix — one that still triggers a violation — is caught here.
			gopath := t.TempDir()
			if err := os.CopyFS(filepath.Join(gopath, "src", "gorm.io", "gorm"),
				os.DirFS(filepath.Join(testdata, "src", "gorm.io", "gorm"))); err != nil {
				t.Fatalf("copy gorm stub: %v", err)
			}
			pkgDir := filepath.Join(gopath, "src", "converge")
			if err := os.MkdirAll(pkgDir, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if err := os.WriteFile(filepath.Join(pkgDir, name), fixed, 0o644); err != nil {
				t.Fatalf("write fixed: %v", err)
			}

			for _, result := range analysistest.Run(t, gopath, gormreuse.Analyzer, "converge") {
				for _, err := range result.Action.Package.Errors {
					t.Errorf("fixed output does not compile: %v", err)
				}
			}
		})
	}
}
