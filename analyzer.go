// Package gormreuse provides a static analysis tool for detecting unsafe
// *gorm.DB instance reuse in Go code.
//
// # Problem
//
// GORM's chain methods (Where, Order, etc.) create shallow clones that share
// internal state. When a mutable *gorm.DB branches into multiple code paths,
// the branches interfere with each other:
//
//	q := db.Where("x")
//	q.Where("a").Find(&r1)  // First branch - OK
//	q.Where("b").Find(&r2)  // Second branch - Bug! Conditions accumulate
//
// # Solution
//
// Use Session() to create an immutable instance. Place it at the point where
// you want to "freeze" the query - subsequent chain methods will create new
// independent chains:
//
//	q := db.Where("x").Session(&gorm.Session{})
//	q.Where("a").Find(&r1)  // Branch 1: WHERE x AND a
//	q.Where("b").Find(&r2)  // Branch 2: WHERE x AND b ← Correct!
//
// # This Analyzer
//
// Detects when a mutable *gorm.DB is used to create multiple branches.
// The first branch is OK; second and subsequent branches are violations.
//
// # Directives
//
// Suppress false positives with:
//
//	//gormreuse:ignore           - Suppress for next line or same line
//	//gormreuse:pure             - Mark function as not polluting *gorm.DB args
//	//gormreuse:immutable-return - Mark function as returning immutable *gorm.DB
package gormreuse

import (
	"go/ast"
	"go/token"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/buildssa"

	"github.com/mpyw/gormreuse/internal"
	"github.com/mpyw/gormreuse/internal/directive"
)

// Analyzer is the main analyzer for gormreuse.
//
// It requires the buildssa analyzer to build SSA form of the code,
// then performs reuse detection via pollution tracking.
//
// Usage with go vet:
//
//	go vet -vettool=$(which gormreuse) ./...
//
// Usage programmatically:
//
//	analysis.Run([]*analysis.Analyzer{gormreuse.Analyzer}, pkgs)
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
	ignoreMaps := make(map[string]directive.IgnoreMap)
	funcIgnores := make(map[string]map[token.Pos]directive.FunctionIgnoreEntry)
	pureFuncs := directive.NewPureFuncSet(pass.Fset, pass.TypesInfo)
	immutableReturnFuncs := directive.NewImmutableReturnFuncSet(pass.Fset, pass.TypesInfo)

	pkgPath := pass.Pkg.Path()
	for _, file := range pass.Files {
		filename := pass.Fset.Position(file.Pos()).Filename
		if skipFiles[filename] {
			continue
		}
		ignoreMaps[filename] = directive.BuildIgnoreMap(pass.Fset, file)
		funcIgnores[filename] = directive.BuildFunctionIgnoreSet(pass.Fset, file)

		// Add original file to sets (for position-correct directive detection)
		pureFuncs.AddFile(file)
		immutableReturnFuncs.AddFile(file)

		// Build pure function set for this file
		for key := range directive.BuildPureFunctionSet(file, pkgPath) {
			pureFuncs.Add(key)
		}
		// Build immutable-return function set for this file
		for key := range directive.BuildImmutableReturnFunctionSet(file, pkgPath) {
			immutableReturnFuncs.Add(key)
		}
	}

	// Run SSA-based analysis
	internal.RunSSA(pass, ssaInfo, ignoreMaps, funcIgnores, pureFuncs, immutableReturnFuncs, skipFiles)

	return nil, nil
}

// buildSkipFiles creates a set of filenames to skip.
// Generated files are always skipped.
// Test files can be skipped via the driver's built-in -test flag.
func buildSkipFiles(pass *analysis.Pass) map[string]bool {
	skipFiles := make(map[string]bool)

	for _, file := range pass.Files {
		filename := pass.Fset.Position(file.Pos()).Filename

		// Always skip generated files
		if ast.IsGenerated(file) {
			skipFiles[filename] = true
		}
	}

	return skipFiles
}
