// Package internal provides SSA-based analysis for GORM *gorm.DB reuse detection.
package internal

import (
	"go/ast"
	"go/token"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/directive"
	ssapkg "github.com/mpyw/gormreuse/internal/ssa"
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

// =============================================================================
// Re-exports from ssa package (for tests and backward compatibility)
// =============================================================================

// Violation is an alias for ssapkg.Violation.
type Violation = ssapkg.Violation

// CFGAnalyzer is an alias for ssapkg.CFGAnalyzer.
type CFGAnalyzer = ssapkg.CFGAnalyzer

// LoopInfo is an alias for ssapkg.LoopInfo.
type LoopInfo = ssapkg.LoopInfo

// PollutionTracker is an alias for ssapkg.PollutionTracker.
type PollutionTracker = ssapkg.PollutionTracker

// RootTracer is an alias for ssapkg.RootTracer.
type RootTracer = ssapkg.RootTracer

// SSATracer is an alias for ssapkg.SSATracer.
type SSATracer = ssapkg.SSATracer

// HandlerContext is an alias for ssapkg.HandlerContext.
type HandlerContext = ssapkg.HandlerContext

// InstructionHandler is an alias for ssapkg.InstructionHandler.
type InstructionHandler = ssapkg.InstructionHandler

// CallHandler is an alias for ssapkg.CallHandler.
type CallHandler = ssapkg.CallHandler

// DeferHandler is an alias for ssapkg.DeferHandler.
type DeferHandler = ssapkg.DeferHandler

// NewCFGAnalyzer creates a new CFGAnalyzer.
func NewCFGAnalyzer() *CFGAnalyzer {
	return ssapkg.NewCFGAnalyzer()
}

// NewPollutionTracker creates a new PollutionTracker.
func NewPollutionTracker(cfgAnalyzer *CFGAnalyzer, fn *ssa.Function) *PollutionTracker {
	return ssapkg.NewPollutionTracker(cfgAnalyzer, fn)
}

// NewRootTracer creates a new RootTracer.
func NewRootTracer(pureFuncs *PureFuncSet) *RootTracer {
	return ssapkg.NewRootTracer(pureFuncs)
}

// NewSSATracer creates a new SSATracer.
func NewSSATracer() *SSATracer {
	return ssapkg.NewSSATracer()
}

// DefaultHandlers returns the default set of instruction handlers.
func DefaultHandlers() []InstructionHandler {
	return ssapkg.DefaultHandlers()
}

// ClosureCapturesGormDB checks if a MakeClosure captures any *gorm.DB values.
func ClosureCapturesGormDB(mc *ssa.MakeClosure) bool {
	return ssapkg.ClosureCapturesGormDB(mc)
}

// IsNilConst checks if a value is a nil constant.
func IsNilConst(v ssa.Value) bool {
	return ssapkg.IsNilConst(v)
}

// NewAnalyzer creates a new SSA Analyzer (for test compatibility).
func NewAnalyzer(fn *ssa.Function, pureFuncs *PureFuncSet) *ssaAnalyzer {
	return newSSAAnalyzer(fn, pureFuncs)
}
