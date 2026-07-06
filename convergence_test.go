package gormreuse_test

import (
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
func TestFixesConverge(t *testing.T) {
	t.Parallel()
	testdata := analysistest.TestData()
	srcDir := filepath.Join(testdata, "src", "converge")

	fixtures, err := goldentest.Fixtures(srcDir)
	if err != nil {
		t.Fatalf("list fixtures: %v", err)
	}

	for _, name := range fixtures {
		name := name
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			src := filepath.Join(srcDir, name)
			_, fixed, err := goldentest.ApplyFixes(testdata, "converge", src, gormreuse.Analyzer)
			if err != nil {
				t.Fatalf("apply fixes: %v", err)
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

			analysistest.Run(t, gopath, gormreuse.Analyzer, "converge")
		})
	}
}
