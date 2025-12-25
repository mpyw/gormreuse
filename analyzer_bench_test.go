package gormreuse_test

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/mpyw/gormreuse"
)

// BenchmarkAnalyzer benchmarks the analyzer on test fixtures.
func BenchmarkAnalyzer(b *testing.B) {
	testdata := analysistest.TestData()

	b.Run("Basic", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			analysistest.Run(b, testdata, gormreuse.Analyzer, "gormreuse")
		}
	})
}

// BenchmarkAnalyzerSmall benchmarks on minimal code.
func BenchmarkAnalyzerSmall(b *testing.B) {
	testdata := analysistest.TestData()

	b.Run("SingleFile", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			analysistest.Run(b, testdata, gormreuse.Analyzer, "gormreuse")
		}
	})
}
