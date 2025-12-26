// Package ssa provides SSA-based analysis for gormreuse linter.
//
// This package detects unsafe *gorm.DB instance reuse by analyzing
// SSA (Static Single Assignment) form of Go code. It tracks mutable
// *gorm.DB values through method chains and detects when the same
// mutable root is used in multiple code paths.
//
// # Analysis Pipeline
//
//	┌─────────────────────────────────────────────────────────┐
//	│                    Analyzer.Analyze()                    │
//	│  ┌─────────────┐  ┌─────────────┐  ┌─────────────────┐  │
//	│  │  PHASE 1    │→ │  PHASE 2    │→ │    PHASE 3      │  │
//	│  │  TRACKING   │  │  DETECTION  │  │   COLLECTION    │  │
//	│  │             │  │             │  │                 │  │
//	│  │ Record all  │  │ Check CFG   │  │ Return         │  │
//	│  │ gorm uses   │  │ reachability│  │ violations     │  │
//	│  └─────────────┘  └─────────────┘  └─────────────────┘  │
//	└─────────────────────────────────────────────────────────┘
//
// # Subpackages
//
//   - tracer/   : Traces SSA values to find mutable roots
//   - pollution/: Tracks pollution state and detects violations
//   - cfg/      : Control flow graph analysis (loops, reachability)
//   - handler/  : SSA instruction handlers (Call, Defer, Send, etc.)
//
// # Key Design Principles
//
//   - All gorm chain method calls are processed uniformly (no special "terminal" handling)
//   - Variable assignment creates a new mutable root (breaks pollution propagation)
//   - Two-phase detection: collect uses first, then check reachability
//   - Position-based ordering for cross-block violation detection
package ssa

import (
	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/directive"
	"github.com/mpyw/gormreuse/internal/ssa/cfg"
	"github.com/mpyw/gormreuse/internal/ssa/handler"
	"github.com/mpyw/gormreuse/internal/ssa/pollution"
	"github.com/mpyw/gormreuse/internal/ssa/tracer"
)

// Violation represents a detected reuse violation.
// This is a type alias for pollution.Violation.
type Violation = pollution.Violation

// Analyzer orchestrates SSA-based analysis for *gorm.DB reuse detection.
//
// It coordinates three main components:
//   - RootTracer: Traces SSA values to find their mutable origins
//   - cfg.Analyzer: Analyzes control flow for loop detection and reachability
//   - pollution.Tracker: Tracks usage sites and detects violations
//
// Example usage:
//
//	analyzer := ssa.NewAnalyzer(fn, pureFuncs)
//	violations := analyzer.Analyze()
//	for _, v := range violations {
//	    report(v.Pos, v.Message)
//	}
type Analyzer struct {
	fn          *ssa.Function      // Function being analyzed
	rootTracer  *tracer.RootTracer // Traces values to mutable roots
	cfgAnalyzer *cfg.Analyzer      // Control flow analysis
}

// NewAnalyzer creates a new Analyzer for the given function.
//
// Parameters:
//   - fn: The SSA function to analyze (can be nil, will return no violations)
//   - pureFuncs: Set of functions marked with //gormreuse:pure directive
//   - immutableReturnFuncs: Set of functions marked with //gormreuse:immutable-return directive
func NewAnalyzer(fn *ssa.Function, pureFuncs, immutableReturnFuncs *directive.DirectiveFuncSet) *Analyzer {
	return &Analyzer{
		fn:          fn,
		rootTracer:  tracer.New(pureFuncs, immutableReturnFuncs),
		cfgAnalyzer: cfg.New(),
	}
}

// Analyze performs the complete analysis and returns detected violations.
//
// The analysis proceeds in three phases:
//
//  1. TRACKING: Process all SSA instructions and record gorm usage sites.
//     Each gorm method call is recorded with its mutable root and position.
//
//  2. DETECTION: For each mutable root with multiple uses, check if an
//     earlier use can reach a later use via CFG analysis. If reachable,
//     the later use is a violation.
//
//  3. COLLECTION: Return all detected violations for reporting.
//
// Closures that capture *gorm.DB are processed recursively to detect
// violations across closure boundaries.
func (a *Analyzer) Analyze() []Violation {
	tracker := pollution.New(a.cfgAnalyzer)

	// PHASE 1: TRACKING
	// Process all instructions and record usages
	a.processFunction(a.fn, tracker, make(map[*ssa.Function]bool))

	// PHASE 2: DETECTION
	// Detect violations using CFG reachability
	tracker.DetectViolations()

	// PHASE 3: COLLECTION
	return tracker.CollectViolations()
}

// processFunction processes all instructions in a function and its closures.
//
// Processing order:
//  1. Detect loops in the function's CFG
//  2. First pass: Process all instructions except defers
//     - MakeClosure: Recursively process if it captures *gorm.DB
//     - Other instructions: Dispatch to appropriate handler
//  3. Second pass: Process defer statements
//     - Defers are processed last because they execute at function exit
//
// The visited map prevents infinite recursion for mutually recursive closures.
func (a *Analyzer) processFunction(fn *ssa.Function, tracker *pollution.Tracker, visited map[*ssa.Function]bool) {
	if fn == nil || fn.Blocks == nil {
		return
	}
	if visited[fn] {
		return
	}
	visited[fn] = true

	// Detect loops for special handling of loop-external roots
	loopInfo := a.cfgAnalyzer.DetectLoops(fn)

	// Create handler context shared across all instruction handlers
	ctx := &handler.Context{
		Tracker:    tracker,
		RootTracer: a.rootTracer,
		CFG:        a.cfgAnalyzer,
		LoopInfo:   loopInfo,
		CurrentFn:  fn,
	}

	// Collect defers and go statements for second pass
	// (they need all pollution recorded first before checking for violations)
	var defers []*ssa.Defer
	var goStmts []*ssa.Go

	// First pass: process regular instructions (record pollution)
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			// Recursively process closures that capture *gorm.DB
			if mc, ok := instr.(*ssa.MakeClosure); ok {
				if closureFn, ok := mc.Fn.(*ssa.Function); ok {
					if tracer.ClosureCapturesGormDB(mc) {
						a.processFunction(closureFn, tracker, visited)
					}
				}
				continue
			}

			// Collect defers for second pass
			if d, ok := instr.(*ssa.Defer); ok {
				defers = append(defers, d)
				continue
			}

			// Collect go statements for second pass
			// (SSA block order may differ from source order, so we need
			// all pollution recorded before checking go statements)
			if g, ok := instr.(*ssa.Go); ok {
				goStmts = append(goStmts, g)
				continue
			}

			// Dispatch to appropriate handler (Call, Send, Store, etc.)
			handler.Dispatch(instr, ctx)
		}
	}

	// Second pass: process go statements
	// (all pollution is now recorded, so we can check for violations)
	for _, g := range goStmts {
		handler.DispatchGo(g, ctx)
	}

	// Third pass: process defer statements
	// Defers use IsPollutedAnywhere since they execute at function exit
	for _, d := range defers {
		handler.DispatchDefer(d, ctx)
	}
}
