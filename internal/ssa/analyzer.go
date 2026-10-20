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
	"go/ast"
	"go/token"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/directive"
	"github.com/mpyw/gormreuse/internal/ssa/callsite"
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
	fn                  *ssa.Function            // Function being analyzed
	rootTracer          *tracer.RootTracer       // Traces values to mutable roots
	cfgAnalyzer         *cfg.Analyzer            // Control flow analysis
	needsImmutableParam func(*ssa.Function) bool // immutable-param fns that branch a param (2b caller check)
}

// NewAnalyzer creates a new Analyzer for the given function.
//
// Parameters:
//   - fn: The SSA function to analyze (can be nil, will return no violations)
//   - pureFuncs: Set of functions marked with //gormreuse:pure directive
//   - immutableReturnFuncs: Set of functions marked with //gormreuse:immutable-return directive
//   - pure: What validating the pure functions found (the failed ones are not trusted
//     as pure; the result of a trusted one may be its argument)
//   - scopesCallbacks: Scopes/Preload callbacks whose *gorm.DB param is a mutable root
//   - immutableParamFuncs: Functions marked //gormreuse:immutable-param (params opt out of Phase 1b)
//   - immutableCallbacks: Transaction callbacks whose tx param is forkable (immutable)
//   - needsImmutableParam: reports the immutable-param functions that actually branch a param,
//     so a caller passing a mutable value to them violates the contract (Phase 1b stage 2b).
//     It may be nil
//   - funcVars: Package-level variables that hold one function, so a call through one
//     honours that function's directives (#183)
//   - cfgs: The CFG cache of the pass, shared with the suggested fixes (#299). When
//     nil, the analyzer makes its own
func NewAnalyzer(fn *ssa.Function, pureFuncs, immutableReturnFuncs, immutableParamFuncs *directive.DirectiveFuncSet, pure *tracer.PureVerdicts, scopesCallbacks, immutableCallbacks map[*ssa.Function]bool, needsImmutableParam func(*ssa.Function) bool, funcVars callsite.FuncVars, cfgs *cfg.Analyzer) *Analyzer {
	if cfgs == nil {
		cfgs = cfg.NewAnalyzer()
	}
	return &Analyzer{
		fn:                  fn,
		rootTracer:          tracer.NewRootTracer(pureFuncs, immutableReturnFuncs, immutableParamFuncs, pure, scopesCallbacks, immutableCallbacks, funcVars),
		cfgAnalyzer:         cfgs,
		needsImmutableParam: needsImmutableParam,
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
	tracker := pollution.New(a.cfgAnalyzer, a.fset())

	// PHASE 1: TRACKING
	// Process all instructions and record usages
	a.processFunction(a.fn, tracker, make(map[*ssa.Function]bool), token.NoPos, nil)

	// PHASE 2: DETECTION
	// Detect violations using CFG reachability
	tracker.DetectViolations()

	// PHASE 3: COLLECTION
	violations := tracker.CollectViolations()
	for i := range violations {
		violations[i].Pos = positionedViolation(violations[i], a.fn)
	}
	return violations
}

// positionedViolation returns the position to report v at. A violation found
// at an instruction without a position, such as one in a synthetic wrapper,
// falls back to its call, then to fn, so no diagnostic is reported at no
// position, where no ignore directive can reach it and some drivers drop it
// (#255).
func positionedViolation(v Violation, fn *ssa.Function) token.Pos {
	switch {
	case v.Pos.IsValid():
		return v.Pos
	case v.Call != nil && v.Call.Pos().IsValid():
		return v.Call.Pos()
	}
	return fn.Pos()
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
//
// posOverride, when valid, is the call-site position at which fn (a closure) is
// invoked; uses recorded while analyzing fn adopt it instead of their body
// position, so define-early/call-late reuse orders by execution, not source,
// position (#68).
//
// parent, when not nil, is the Repeats of the function that declares fn, so
// a use in fn is asked whether fn runs more than once there (#249). The
// function the analysis starts from has none: a closure analysed on its own
// asks only its own loops, so a reuse it shares with its parent's analysis
// is not reported twice.
func (a *Analyzer) processFunction(fn *ssa.Function, tracker *pollution.Tracker, visited map[*ssa.Function]bool, posOverride token.Pos, parent *cfg.Repeats) {
	if fn == nil || fn.Blocks == nil {
		return
	}
	if visited[fn] {
		return
	}
	visited[fn] = true

	// Detect loops for special handling of loop-external roots. A closure
	// also runs again when its parent runs it more than once (#249).
	loopInfo := a.cfgAnalyzer.DetectLoops(fn)
	repeats := a.cfgAnalyzer.Repeats(fn)
	if parent != nil {
		repeats = parent.Closure(fn)
	}

	// Create handler context shared across all instruction handlers
	ctx := &handler.Context{
		Tracker:             tracker,
		RootTracer:          a.rootTracer,
		LoopInfo:            loopInfo,
		Repeats:             repeats,
		CurrentFn:           fn,
		PosOverride:         posOverride,
		NeedsImmutableParam: a.needsImmutableParam,
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
					// Recurse into closures that capture *gorm.DB, and into
					// Scopes/Preload callbacks (which operate on their mutable
					// parameter rather than a captured variable — see #60).
					// Skip provably-dead closures: their uses never execute, so
					// analyzing them yields false positives (#68).
					if !isMethodValue(mc) && (tracer.ClosureCapturesGormDB(mc) || a.rootTracer.IsScopesCallbackFunc(closureFn)) && !isDeadClosure(mc) {
						// If the closure is invoked at a single call site, order
						// its captured uses by that call-site position (#68). When
						// there is no single such site (IIFE, defer/go, multiple
						// or no invocations), inherit the enclosing override.
						childOverride := posOverride
						call := closureInvocation(mc)
						if p := closureInvocationPos(call, closureFn, a.fset()); p.IsValid() {
							childOverride = p
						}
						// A single call site, IIFE included, also places the
						// closure's uses at the call's block for reachability
						// (#179).
						if call != nil {
							tracker.RecordInvocationSite(closureFn, call.Block())
						}
						a.processFunction(closureFn, tracker, visited, childOverride, repeats)
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

// isMethodValue reports whether mc is a method value (r.m), a closure over the
// synthetic "m$bound" wrapper, not a function literal. A call through it is
// handled as the method call it stands for (callsite.SiteBound). The wrapper
// has no syntax, and analyzing it as a closure reported its forwarding call
// at no position (#255). A method expression thunk is a plain function, never
// a closure. The synthetic yield function of a range-over-func loop is the
// loop body, so it is analyzed.
func isMethodValue(mc *ssa.MakeClosure) bool {
	_, bound := callsite.BoundSite(mc)
	return bound
}

// isDeadClosure reports whether mc's closure value is provably never used: it
// has no referrers at all, so nothing can invoke it (e.g. `h := func(){...}; _ =
// h`). Such a closure cannot run, so recording pollution from its body would
// produce false positives — a captured value "reused" only inside an
// unreachable closure is not actually reused (#68). Any referrer (a call, a Go/
// Defer, a store that might escape) makes the closure potentially live, so it is
// conservatively analyzed.
func isDeadClosure(mc *ssa.MakeClosure) bool {
	refs := mc.Referrers()
	return refs == nil || len(*refs) == 0
}

// closureInvocation returns the one plain call that invokes the closure value
// mc, or nil when there is no such single call: it is invoked more than once,
// or used in any other way (deferred, spawned, passed as an argument, stored).
func closureInvocation(mc *ssa.MakeClosure) *ssa.Call {
	var found *ssa.Call
	for _, r := range *mc.Referrers() {
		call, ok := r.(*ssa.Call)
		if !ok || call.Call.Value != ssa.Value(mc) {
			// A non-call referrer (store, defer, go, arg) makes the invocation
			// site ambiguous; bail out to the safe (no-site) behavior.
			return nil
		}
		if found != nil {
			return nil // more than one call site
		}
		found = call
	}
	return found
}

// closureInvocationPos returns the position of call, the single invocation of
// closureFn (see closureInvocation), but ONLY for the define-early/call-late
// case that #68 targets: a call on a LATER line than the closure literal's end
// (i.e. `f := func(){...}; …; f()`). It returns token.NoPos otherwise — no
// single call, or an IIFE (invoked on its own closing-brace line) — so those
// keep their body positions unchanged.
func closureInvocationPos(call *ssa.Call, closureFn *ssa.Function, fset *token.FileSet) token.Pos {
	if call == nil || fset == nil {
		return token.NoPos
	}

	// Distinguish define-early/call-late from an inline IIFE: the former's call
	// is on a later line than the closure literal's closing brace.
	lit, ok := closureFn.Syntax().(*ast.FuncLit)
	if !ok {
		return token.NoPos
	}
	if fset.PositionFor(call.Pos(), false).Line <= fset.PositionFor(lit.End(), false).Line {
		return token.NoPos // IIFE / same-line invocation: keep body positions
	}
	return call.Pos()
}

// fset returns the program's FileSet (nil if unavailable).
func (a *Analyzer) fset() *token.FileSet {
	if a.fn != nil && a.fn.Prog != nil {
		return a.fn.Prog.Fset
	}
	return nil
}
