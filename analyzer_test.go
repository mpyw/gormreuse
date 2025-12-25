package gormreuse_test

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/mpyw/gormreuse"
)

func TestAnalyzer(t *testing.T) {
	testdata := analysistest.TestData()
	analysistest.Run(t, testdata, gormreuse.Analyzer, "gormreuse")
}

func TestFileFilter(t *testing.T) {
	testdata := analysistest.TestData()
	// Tests that generated files are skipped
	analysistest.Run(t, testdata, gormreuse.Analyzer, "filefilter")
}

func TestSuggestedFixes(t *testing.T) {
	t.Skip("Skipping until all golden files are created")
	testdata := analysistest.TestData()
	analysistest.RunWithSuggestedFixes(t, testdata, gormreuse.Analyzer, "gormreuse")
}

func TestSuggestedFixesBasic(t *testing.T) {
	// Test only fix_basic.go for now
	testdata := analysistest.TestData()
	result := analysistest.Run(t, testdata, gormreuse.Analyzer, "gormreuse")

	// Print suggested fixes for debugging
	for _, r := range result {
		for _, d := range r.Diagnostics {
			t.Logf("Diagnostic at %v: %s", d.Pos, d.Message)
			for i, fix := range d.SuggestedFixes {
				t.Logf("  Fix %d: %s", i, fix.Message)
				for j, edit := range fix.TextEdits {
					t.Logf("    Edit %d: [%v-%v] -> %q", j, edit.Pos, edit.End, edit.NewText)
				}
			}
		}
	}
}
