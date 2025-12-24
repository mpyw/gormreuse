// Package ssa provides SSA-based analysis for gormreuse linter.
//
// # Key Design Principles
//
// 1. No isTerminal concept - all gorm calls are processed uniformly
// 2. Variable assignment creates new mutable root (cleaner ownership model)
// 3. Entry point detection via recv == root comparison
//
// # Package Structure
//
//   - handler/  : Instruction handlers (CallHandler, etc.)
//   - tracer/   : SSA value tracing (RootTracer)
//   - pollution/: Pollution state tracking (Tracker)
//   - cfg/      : Control flow analysis (Analyzer, LoopInfo)
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
type Violation = pollution.Violation

// Analyzer orchestrates SSA-based analysis for *gorm.DB reuse detection.
type Analyzer struct {
	fn          *ssa.Function
	rootTracer  *tracer.RootTracer
	cfgAnalyzer *cfg.Analyzer
}

// NewAnalyzer creates a new Analyzer for the given function.
func NewAnalyzer(fn *ssa.Function, pureFuncs *directive.PureFuncSet) *Analyzer {
	return &Analyzer{
		fn:          fn,
		rootTracer:  tracer.New(pureFuncs),
		cfgAnalyzer: cfg.New(),
	}
}

// Analyze performs the complete analysis and returns detected violations.
func (a *Analyzer) Analyze() []Violation {
	tracker := pollution.New(a.cfgAnalyzer, a.fn)

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
func (a *Analyzer) processFunction(fn *ssa.Function, tracker *pollution.Tracker, visited map[*ssa.Function]bool) {
	if fn == nil || fn.Blocks == nil {
		return
	}
	if visited[fn] {
		return
	}
	visited[fn] = true

	// Detect loops
	loopInfo := a.cfgAnalyzer.DetectLoops(fn)

	// Create handler context
	ctx := &handler.Context{
		Tracker:    tracker,
		RootTracer: a.rootTracer,
		CFG:        a.cfgAnalyzer,
		LoopInfo:   loopInfo,
		CurrentFn:  fn,
	}

	// Collect defers for second pass
	var defers []*ssa.Defer

	// First pass: process regular instructions
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			// Check for MakeClosure to process closures recursively
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

			// Dispatch to appropriate handler
			handler.Dispatch(instr, ctx)
		}
	}

	// Second pass: process defer statements
	for _, d := range defers {
		handler.DispatchDefer(d, ctx)
	}
}
