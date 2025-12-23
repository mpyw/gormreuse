// Package internal provides SSA-based analysis for GORM *gorm.DB reuse detection.
package internal

import (
	"go/token"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/buildssa"
	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/directive"
	ssapkg "github.com/mpyw/gormreuse/internal/ssa"
	"github.com/mpyw/gormreuse/internal/ssa/purity"
)

// =============================================================================
// Entry Point
// =============================================================================

// RunSSA performs SSA-based analysis for GORM *gorm.DB reuse detection.
func RunSSA(
	pass *analysis.Pass,
	ssaInfo *buildssa.SSA,
	ignoreMaps map[string]directive.IgnoreMap,
	funcIgnores map[string]map[token.Pos]struct{},
	pureFuncs *directive.PureFuncSet,
	skipFiles map[string]bool,
) {
	for _, fn := range ssaInfo.SrcFuncs {
		pos := fn.Pos()
		if !pos.IsValid() {
			continue
		}

		filename := pass.Fset.Position(pos).Filename

		// Skip functions in excluded files
		if skipFiles[filename] {
			continue
		}

		ignoreMap := ignoreMaps[filename]

		// Check if entire function is ignored
		if funcIgnoreSet, ok := funcIgnores[filename]; ok {
			if _, ignored := funcIgnoreSet[fn.Pos()]; ignored {
				// Mark the ignore directive as used
				if ignoreMap != nil {
					fnLine := pass.Fset.Position(fn.Pos()).Line
					// The ignore comment is on the line before the function name
					ignoreMap.MarkUsed(fnLine - 1)
				}
				continue
			}
		}

		// Validate pure function contracts using 3-state purity model
		if IsPureFunctionDecl(fn, pureFuncs) {
			checker := newPurityChecker(pureFuncs)
			for _, v := range purity.ValidateFunction(fn, checker) {
				pass.Reportf(v.Pos, "%s", v.Message)
			}
		}

		chk := newChecker(pass, ignoreMap, pureFuncs)
		chk.checkFunction(fn)
	}

	// Report unused ignore directives
	for _, ignoreMap := range ignoreMaps {
		if ignoreMap == nil {
			continue
		}
		for _, pos := range ignoreMap.GetUnusedIgnores() {
			pass.Reportf(pos, "unused gormreuse:ignore directive")
		}
	}
}

// =============================================================================
// SSA Checker
// =============================================================================

type checker struct {
	pass      *analysis.Pass
	ignoreMap directive.IgnoreMap
	pureFuncs *directive.PureFuncSet
	reported  map[token.Pos]bool
}

func newChecker(pass *analysis.Pass, ignoreMap directive.IgnoreMap, pureFuncs *directive.PureFuncSet) *checker {
	return &checker{
		pass:      pass,
		ignoreMap: ignoreMap,
		pureFuncs: pureFuncs,
		reported:  make(map[token.Pos]bool),
	}
}

func (c *checker) checkFunction(fn *ssa.Function) {
	analyzer := newSSAAnalyzer(fn, c.pureFuncs)
	violations := analyzer.Analyze()

	for _, v := range violations {
		c.report(v.Pos, v.Message)
	}
}

func (c *checker) report(pos token.Pos, message string) {
	if c.reported[pos] {
		return
	}
	c.reported[pos] = true

	line := c.pass.Fset.Position(pos).Line
	if c.ignoreMap != nil && c.ignoreMap.ShouldIgnore(line) {
		return
	}

	c.pass.Reportf(pos, "%s", message)
}

// =============================================================================
// SSA Analyzer - Orchestrates the Analysis Pipeline
//
// The analyzer composes:
//   - RootTracer: Traces SSA values to mutable roots
//   - CFGAnalyzer: Analyzes control flow (loops, reachability)
//   - PollutionTracker: Tracks pollution state and violations
//   - InstructionHandlers: Process different instruction types (Strategy pattern)
//
// Analysis Pipeline:
//   1. TRACKING PHASE: Process instructions, track pollution
//   2. DETECTION PHASE: Detect cross-block violations via CFG reachability
//   3. COLLECTION PHASE: Collect and return all violations
// =============================================================================

// ssaAnalyzer orchestrates the SSA-based analysis for *gorm.DB reuse detection.
type ssaAnalyzer struct {
	fn           *ssa.Function
	rootTracer   *ssapkg.RootTracer
	cfgAnalyzer  *ssapkg.CFGAnalyzer
	handlers     []ssapkg.InstructionHandler
	deferHandler *ssapkg.DeferHandler
}

// newSSAAnalyzer creates a new ssaAnalyzer for the given function.
func newSSAAnalyzer(fn *ssa.Function, pureFuncs *directive.PureFuncSet) *ssaAnalyzer {
	return &ssaAnalyzer{
		fn:           fn,
		rootTracer:   ssapkg.NewRootTracer(pureFuncs),
		cfgAnalyzer:  ssapkg.NewCFGAnalyzer(),
		handlers:     ssapkg.DefaultHandlers(),
		deferHandler: &ssapkg.DeferHandler{},
	}
}

// Analyze performs the complete analysis and returns detected violations.
func (a *ssaAnalyzer) Analyze() []ssapkg.Violation {
	tracker := ssapkg.NewPollutionTracker(a.cfgAnalyzer, a.fn)

	// PHASE 1: TRACKING
	// Process all instructions and track pollution
	a.processFunction(a.fn, tracker, make(map[*ssa.Function]bool))

	// PHASE 2: DETECTION
	// Detect cross-block violations using CFG reachability
	tracker.DetectReachabilityViolations()

	// PHASE 3: COLLECTION
	// Return all detected violations
	return tracker.CollectViolations()
}

// processFunction processes all instructions in a function and its closures.
//
// This method implements a two-pass analysis:
//
//	Pass 1 (Regular Instructions):
//	  - Process regular instructions (Call, Go, Send, Store, etc.)
//	  - Recursively process closures that capture *gorm.DB
//	  - Collect defer statements for pass 2
//
//	Pass 2 (Defer Statements):
//	  - Process defers after all regular instructions
//	  - Defers use IsPollutedAnywhere (not IsPollutedAt)
//	  - This is because defers execute at function exit
//
// Example:
//
//	func example(db *gorm.DB) {
//	    q := db.Where("x")
//	    defer q.Find(nil)     // Collected in pass 1, processed in pass 2
//
//	    f := func() {         // MakeClosure capturing q
//	        q.Find(nil)       // Processed via recursive call
//	    }
//
//	    q.Find(nil)           // Pass 1: pollutes q
//	    f()                   // Closure already processed
//	}                         // Pass 2: defer sees q is polluted → violation
//
// Closure recursion:
//
//	When a MakeClosure captures *gorm.DB (detected by ClosureCapturesGormDB),
//	we recursively process the closure function. This ensures pollution
//	inside closures is tracked in the parent's PollutionTracker.
func (a *ssaAnalyzer) processFunction(fn *ssa.Function, tracker *ssapkg.PollutionTracker, visited map[*ssa.Function]bool) {
	if fn == nil || fn.Blocks == nil {
		return
	}
	if visited[fn] {
		return
	}
	visited[fn] = true

	// Detect loops in this function
	loopInfo := a.cfgAnalyzer.DetectLoops(fn)

	// Create handler context
	ctx := &ssapkg.HandlerContext{
		Tracker:     tracker,
		RootTracer:  a.rootTracer,
		CFGAnalyzer: a.cfgAnalyzer,
		LoopInfo:    loopInfo,
		CurrentFn:   fn,
	}

	// Collect defers for second pass
	var defers []*ssa.Defer

	// First pass: process regular instructions
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			// Check for MakeClosure to process closures recursively
			if mc, ok := instr.(*ssa.MakeClosure); ok {
				if closureFn, ok := mc.Fn.(*ssa.Function); ok {
					if ssapkg.ClosureCapturesGormDB(mc) {
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
			for _, handler := range a.handlers {
				if handler.CanHandle(instr) {
					handler.Handle(instr, ctx)
					break
				}
			}
		}
	}

	// Second pass: process defer statements
	for _, d := range defers {
		a.deferHandler.Handle(d, ctx)
	}
}
