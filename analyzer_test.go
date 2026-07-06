package gormreuse_test

import (
	"bytes"
	"os"
	"path/filepath"
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

func TestGenerateDiffFiles(t *testing.T) {
	testdata := analysistest.TestData()
	srcDir := filepath.Join(testdata, "src", "gormreuse")

	fixtures, err := goldentest.Fixtures(srcDir)
	if err != nil {
		t.Fatalf("Failed to list fixtures: %v", err)
	}

	for _, filename := range fixtures {
		filename := filename // capture range variable
		t.Run(filename, func(t *testing.T) {
			t.Parallel()
			srcPath := filepath.Join(srcDir, filename)

			original, fixed, err := goldentest.ApplyFixes(testdata, "gormreuse", srcPath, gormreuse.Analyzer)
			if err != nil {
				t.Fatalf("Failed to apply fixes for %s: %v", srcPath, err)
			}
			diffBytes, err := goldentest.GenerateDiff(filename, original, fixed)
			if err != nil {
				t.Fatalf("Failed to generate diff for %s: %v", srcPath, err)
			}

			if err := os.WriteFile(srcPath+".diff", diffBytes, 0o644); err != nil {
				t.Fatalf("Failed to write diff file %s.diff: %v", srcPath, err)
			}
			t.Logf("Generated diff file: %s.diff (%d bytes)", srcPath, len(diffBytes))
		})
	}
}

func TestDiffFilesUpToDate(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping diff file check in short mode")
	}

	testdata := analysistest.TestData()
	srcDir := filepath.Join(testdata, "src", "gormreuse")

	fixtures, err := goldentest.Fixtures(srcDir)
	if err != nil {
		t.Fatalf("Failed to list fixtures: %v", err)
	}

	for _, filename := range fixtures {
		srcPath := filepath.Join(srcDir, filename)
		diffPath := srcPath + ".diff"

		beforeContent, err := os.ReadFile(diffPath)
		if err != nil {
			t.Fatalf("Failed to read existing diff %s: %v", diffPath, err)
		}

		original, fixed, err := goldentest.ApplyFixes(testdata, "gormreuse", srcPath, gormreuse.Analyzer)
		if err != nil {
			t.Fatalf("Failed to apply fixes for %s: %v", srcPath, err)
		}
		diffBytes, err := goldentest.GenerateDiff(filename, original, fixed)
		if err != nil {
			t.Fatalf("Failed to generate diff for %s: %v", srcPath, err)
		}

		if !bytes.Equal(beforeContent, diffBytes) {
			t.Errorf("%s.diff is out of date.\nRun: go run ./testdata/cmd/gengolden && go test -run TestGenerateDiffFiles .\nThen commit the changes.", filename)
		}
	}
}
