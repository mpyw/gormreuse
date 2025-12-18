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
	"go/token"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/buildssa"

	"github.com/mpyw/gormreuse/internal"
)

// Analyzer is the main analyzer for gormreuse.
var Analyzer = &analysis.Analyzer{
	Name:     "gormreuse",
	Doc:      "detects unsafe *gorm.DB instance reuse after chain methods",
	Requires: []*analysis.Analyzer{buildssa.Analyzer},
	Run:      run,
}

func run(pass *analysis.Pass) (any, error) {
	ssaInfo := pass.ResultOf[buildssa.Analyzer].(*buildssa.SSA)

	// Build ignore maps for each file
	ignoreMaps := make(map[string]internal.IgnoreMap)
	funcIgnores := make(map[string]map[token.Pos]struct{})
	pureFuncs := make(map[string]struct{})

	pkgPath := pass.Pkg.Path()
	for _, file := range pass.Files {
		filename := pass.Fset.Position(file.Pos()).Filename
		ignoreMaps[filename] = internal.BuildIgnoreMap(pass.Fset, file)
		funcIgnores[filename] = internal.BuildFunctionIgnoreSet(pass.Fset, file)

		// Build pure function set for this file
		for name := range internal.BuildPureFunctionSet(pass.Fset, file, pkgPath) {
			pureFuncs[name] = struct{}{}
		}
	}

	// Run SSA-based analysis
	internal.RunSSA(pass, ssaInfo, ignoreMaps, funcIgnores, pureFuncs)

	return nil, nil
}
