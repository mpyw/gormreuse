// Package internal provides SSA-based analysis for GORM *gorm.DB reuse detection.
package internal

import (
	"go/ast"
	"go/token"

	"github.com/mpyw/gormreuse/internal/directive"
)

// =============================================================================
// Re-exports from directive package
// =============================================================================

// IgnoreMap is an alias for directive.IgnoreMap.
type IgnoreMap = directive.IgnoreMap

// PureFuncSet is an alias for directive.PureFuncSet.
type PureFuncSet = directive.PureFuncSet

// PureFuncKey is an alias for directive.PureFuncKey.
type PureFuncKey = directive.PureFuncKey

// NewPureFuncSet creates a new PureFuncSet.
func NewPureFuncSet(fset *token.FileSet) *PureFuncSet {
	return directive.NewPureFuncSet(fset)
}

// BuildIgnoreMap builds an ignore map for a file.
func BuildIgnoreMap(fset *token.FileSet, file *ast.File) IgnoreMap {
	return directive.BuildIgnoreMap(fset, file)
}

// BuildFunctionIgnoreSet builds a set of functions that have ignore directives.
func BuildFunctionIgnoreSet(fset *token.FileSet, file *ast.File) map[token.Pos]struct{} {
	return directive.BuildFunctionIgnoreSet(fset, file)
}

// BuildPureFunctionSet builds a set of pure function keys from a file.
func BuildPureFunctionSet(fset *token.FileSet, file *ast.File, pkgPath string) map[PureFuncKey]struct{} {
	return directive.BuildPureFunctionSet(fset, file, pkgPath)
}
