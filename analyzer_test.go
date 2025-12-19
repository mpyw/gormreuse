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

func TestFileFilterDefault(t *testing.T) {
	testdata := analysistest.TestData()
	// Default: -test=true, so test files are analyzed
	analysistest.Run(t, testdata, gormreuse.Analyzer, "filefilter")
}

func TestFileFilterSkipTests(t *testing.T) {
	testdata := analysistest.TestData()

	// Set -test=false to skip test files
	if err := gormreuse.Analyzer.Flags.Set("test", "false"); err != nil {
		t.Fatal(err)
	}

	defer func() {
		_ = gormreuse.Analyzer.Flags.Set("test", "true")
	}()

	analysistest.Run(t, testdata, gormreuse.Analyzer, "filefilterskip")
}
