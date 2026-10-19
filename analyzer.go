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
//	//gormreuse:ignore                // Suppress for a statement, function or file
//	//gormreuse:pure                  // Mark function as not polluting *gorm.DB args
//	//gormreuse:immutable-return      // Mark function as returning immutable *gorm.DB
//	//gormreuse:immutable-param       // Mark *gorm.DB parameters as immutable
//	//gormreuse:immutable-input(name) // Mark a callback as receiving an immutable *gorm.DB
//
// Only //gormreuse:name[,name...] (lowercase, no spaces, line comment) is a
// directive. A reason goes after "//". Any other comment that starts with
// "gormreuse:", or one with a name gormreuse does not read, is reported and
// has no effect.
package gormreuse

import (
	"go/ast"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/buildssa"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"

	"github.com/mpyw/gormreuse/internal"
	"github.com/mpyw/gormreuse/internal/directive"
)

// Analyzer is the main analyzer for gormreuse.
//
// It requires the buildssa analyzer to build SSA form of the code,
// then performs reuse detection via pollution tracking. It also requires the
// inspect analyzer, which buildssa already depends on, so the directive scans
// share one AST traversal instead of walking each file again.
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
	Requires: []*analysis.Analyzer{buildssa.Analyzer, inspect.Analyzer},
	Run:      run,
}

func run(pass *analysis.Pass) (any, error) {
	ssaInfo := pass.ResultOf[buildssa.Analyzer].(*buildssa.SSA)
	insp := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)

	// Build set of files to skip
	skipFiles := buildSkipFiles(pass)

	// Collect ignore directives for each file (excluding skipped files)
	ignores := directive.NewIgnores(pass.Fset)
	pureFuncs := directive.NewPureFuncSet(pass.Fset, pass.TypesInfo)
	immutableReturnFuncs := directive.NewImmutableReturnFuncSet(pass.Fset, pass.TypesInfo)
	immutableParamFuncs := directive.NewImmutableParamFuncSet(pass.Fset, pass.TypesInfo)
	immutableInputSet := directive.NewImmutableInputSet(pass.Fset, pass.TypesInfo)

	pkgPath := pass.Pkg.Path()
	// The inspector's root children are pass.Files, in the same order.
	for cur := range insp.Root().Children() {
		file := cur.Node().(*ast.File)
		filename := pass.Fset.PositionFor(file.Pos(), false).Filename
		if skipFiles[filename] {
			continue
		}
		ignores.AddFile(cur)
		for _, p := range directive.FindDirectiveProblems(file) {
			if !ignores.Ignore(p.Pos) {
				pass.Reportf(p.Pos, "%s", p.Message)
			}
		}

		// Add original file to sets (for position-correct directive detection)
		pureFuncs.AddFile(cur)
		immutableReturnFuncs.AddFile(cur)
		immutableParamFuncs.AddFile(cur)

		// Build pure function set for this file
		for key := range directive.BuildPureFuncSet(cur, pkgPath) {
			pureFuncs.Add(key)
		}
		// Build immutable-return function set for this file
		for key := range directive.BuildImmutableReturnFuncSet(cur, pkgPath) {
			immutableReturnFuncs.Add(key)
		}
		// Build immutable-param function set for this file
		for key := range directive.BuildImmutableParamFuncSet(cur, pkgPath) {
			immutableParamFuncs.Add(key)
		}
		// Build immutable-input(name) callback declarations for this file
		immutableInputSet.AddFile(cur, pkgPath)
	}

	// Run SSA-based analysis
	internal.RunSSA(pass, ssaInfo, ignores, pureFuncs, immutableReturnFuncs, immutableParamFuncs, immutableInputSet, skipFiles)

	return nil, nil
}

// buildSkipFiles creates a set of filenames to skip.
// Generated files are always skipped.
// Test files can be skipped via the driver's built-in -test flag.
func buildSkipFiles(pass *analysis.Pass) map[string]bool {
	skipFiles := make(map[string]bool)

	for _, file := range pass.Files {
		filename := pass.Fset.PositionFor(file.Pos(), false).Filename

		// Always skip generated files
		if isGeneratedFile(file) {
			skipFiles[filename] = true
		}
	}

	return skipFiles
}

// cgoHeader is the first comment of every file that cmd/cgo writes.
const cgoHeader = "// Code generated by cmd/cgo; DO NOT EDIT."

// isGeneratedFile reports whether file is generated code (#186).
//
// In a package that imports "C", the driver gives the analyzer cmd/cgo's
// output instead of the user's files. cgo rewrites each user file: it puts
// cgoHeader first, then a //line directive back to the user's file, then the
// user's file with its comments. ast.IsGenerated sees cgoHeader, so it would
// skip all of the user's code. For such a file, cgoHeader is ignored and the
// user's own comments decide. A user file marked "// Code generated ... DO NOT
// EDIT." is still skipped. The files that cgo writes for itself, such as
// _cgo_gotypes.go, have no //line directive between cgoHeader and the package
// clause, so they are still skipped.
func isGeneratedFile(file *ast.File) bool {
	if len(file.Comments) >= 2 &&
		len(file.Comments[0].List) == 1 && file.Comments[0].List[0].Text == cgoHeader &&
		strings.HasPrefix(file.Comments[1].List[0].Text, "//line ") && file.Comments[1].Pos() < file.Package {
		userFile := *file
		userFile.Comments = file.Comments[1:]
		return ast.IsGenerated(&userFile)
	}
	return ast.IsGenerated(file)
}
