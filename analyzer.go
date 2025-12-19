// Package gormreuse provides a static analysis tool for detecting unsafe
// *gorm.DB instance reuse in Go code.
//
// GORM's chain methods (Where, Order, etc.) modify internal state. Reusing
// the same *gorm.DB instance after chain methods can cause query conditions
// to accumulate unexpectedly.
//
// This analyzer detects such patterns and suggests using Session() or
// WithContext() to create safe, reusable instances.
package gormreuse

import (
	"go/ast"
	"go/token"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/buildssa"

	"github.com/mpyw/gormreuse/internal"
)

// File filtering flags.
var analyzeTests bool

func init() {
	Analyzer.Flags.BoolVar(&analyzeTests, "test", true, "analyze test files (*_test.go)")
}

// Analyzer is the main analyzer for gormreuse.
var Analyzer = &analysis.Analyzer{
	Name:     "gormreuse",
	Doc:      "detects unsafe *gorm.DB instance reuse after chain methods",
	Requires: []*analysis.Analyzer{buildssa.Analyzer},
	Run:      run,
}

func run(pass *analysis.Pass) (any, error) {
	ssaInfo := pass.ResultOf[buildssa.Analyzer].(*buildssa.SSA)

	// Build set of files to skip
	skipFiles := buildSkipFiles(pass)

	// Build ignore maps for each file (excluding skipped files)
	ignoreMaps := make(map[string]internal.IgnoreMap)
	funcIgnores := make(map[string]map[token.Pos]struct{})
	pureFuncs := make(map[string]struct{})

	pkgPath := pass.Pkg.Path()
	for _, file := range pass.Files {
		filename := pass.Fset.Position(file.Pos()).Filename
		if skipFiles[filename] {
			continue
		}
		ignoreMaps[filename] = internal.BuildIgnoreMap(pass.Fset, file)
		funcIgnores[filename] = internal.BuildFunctionIgnoreSet(pass.Fset, file)

		// Build pure function set for this file
		for name := range internal.BuildPureFunctionSet(pass.Fset, file, pkgPath) {
			pureFuncs[name] = struct{}{}
		}
	}

	// Run SSA-based analysis
	internal.RunSSA(pass, ssaInfo, ignoreMaps, funcIgnores, pureFuncs, skipFiles)

	return nil, nil
}

// buildSkipFiles creates a set of filenames to skip based on flags.
// Generated files are always skipped.
// Test files are skipped when analyzeTests is false.
func buildSkipFiles(pass *analysis.Pass) map[string]bool {
	skipFiles := make(map[string]bool)

	for _, file := range pass.Files {
		filename := pass.Fset.Position(file.Pos()).Filename

		// Always skip generated files
		if ast.IsGenerated(file) {
			skipFiles[filename] = true
			continue
		}

		// Skip test files if -test=false
		if !analyzeTests && strings.HasSuffix(filename, "_test.go") {
			skipFiles[filename] = true
		}
	}

	return skipFiles
}
