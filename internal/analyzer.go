// Package internal provides SSA-based analysis for GORM *gorm.DB reuse detection.
package internal

import (
	"go/token"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/buildssa"
	"golang.org/x/tools/go/ssa"
)

// =============================================================================
// Entry Point
// =============================================================================

// RunSSA performs SSA-based analysis for GORM *gorm.DB reuse detection.
func RunSSA(
	pass *analysis.Pass,
	ssaInfo *buildssa.SSA,
	ignoreMaps map[string]IgnoreMap,
	funcIgnores map[string]map[token.Pos]struct{},
	pureFuncs map[string]struct{},
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
	ignoreMap IgnoreMap
	pureFuncs map[string]struct{}
	reported  map[token.Pos]bool
}

func newChecker(pass *analysis.Pass, ignoreMap IgnoreMap, pureFuncs map[string]struct{}) *checker {
	return &checker{
		pass:      pass,
		ignoreMap: ignoreMap,
		pureFuncs: pureFuncs,
		reported:  make(map[token.Pos]bool),
	}
}

func (c *checker) checkFunction(fn *ssa.Function) {
	analyzer := newUsageAnalyzer(fn, c.pureFuncs)
	violations := analyzer.analyze()

	for _, v := range violations {
		c.report(v.pos, v.message)
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
// Usage Analyzer
// =============================================================================

type violation struct {
	pos     token.Pos
	message string
}

// =============================================================================
// valueState - State Machine for *gorm.DB Value Tracking
//
// Lifecycle:
//   1. TRACKING PHASE (processMethodCalls):
//      - pollutedBlocks: WRITE (via markPolluted)
//      - violations: WRITE (same-block violations detected inline)
//
//   2. DETECTION PHASE (detectReachabilityViolations):
//      - pollutedBlocks: READ-ONLY (tracking complete)
//      - violations: WRITE (cross-block violations detected)
//
//   3. COLLECTION PHASE (collectViolations):
//      - pollutedBlocks: READ-ONLY
//      - violations: READ-ONLY
//
// Note: Same-block violations are detected inline during tracking for efficiency.
// This prevents a strict separation between tracking and detection phases,
// but avoids redundant iteration over instructions.
// =============================================================================

// valueState tracks the pollution state and violations for a *gorm.DB value.
// See the lifecycle documentation above for state transition rules.
type valueState struct {
	// pollutedBlocks maps blocks where this value was polluted to the position.
	// This enables flow-sensitive analysis: pollution in block A only affects
	// uses in block B if A can reach B in the CFG.
	// INVARIANT: Only modified during TRACKING PHASE.
	pollutedBlocks map[*ssa.BasicBlock]token.Pos

	// violations tracks positions where polluted value was reused.
	// INVARIANT: Appended during TRACKING and DETECTION phases, read during COLLECTION.
	violations []token.Pos
}

type usageAnalyzer struct {
	fn *ssa.Function

	// states maps each tracked *gorm.DB chain root to its state.
	states map[ssa.Value]*valueState

	// pureFuncs is a set of functions marked as pure (don't pollute *gorm.DB).
	pureFuncs map[string]struct{}
}

func newUsageAnalyzer(fn *ssa.Function, pureFuncs map[string]struct{}) *usageAnalyzer {
	return &usageAnalyzer{
		fn:        fn,
		states:    make(map[ssa.Value]*valueState),
		pureFuncs: pureFuncs,
	}
}

// getOrCreateState returns the state for the given root, creating it if necessary.
func (a *usageAnalyzer) getOrCreateState(root ssa.Value) *valueState {
	state := a.states[root]
	if state == nil {
		state = &valueState{
			pollutedBlocks: make(map[*ssa.BasicBlock]token.Pos),
		}
		a.states[root] = state
	}
	return state
}

// markPolluted marks the root as polluted at the given block and position.
// Returns true if the block was newly polluted (not already polluted).
func (a *usageAnalyzer) markPolluted(root ssa.Value, block *ssa.BasicBlock, pos token.Pos) bool {
	state := a.getOrCreateState(root)
	if _, exists := state.pollutedBlocks[block]; exists {
		return false
	}
	state.pollutedBlocks[block] = pos
	return true
}

// analyze performs the complete analysis pipeline.
// See valueState documentation for the phase lifecycle.
func (a *usageAnalyzer) analyze() []violation {
	// PHASE 1: TRACKING
	// - Tracks pollution by marking pollutedBlocks
	// - Detects same-block violations inline (optimization)
	// - Also processes closures that capture *gorm.DB values
	a.processMethodCalls(a.fn, make(map[*ssa.Function]bool))

	// PHASE 2: DETECTION
	// - pollutedBlocks are now frozen (read-only)
	// - Detects cross-block violations using CFG reachability
	a.detectReachabilityViolations()

	// PHASE 3: COLLECTION
	// - All violations have been recorded
	// - Collect and return final results
	return a.collectViolations()
}

// processMethodCalls processes all *gorm.DB method calls to track pollution.
// It also recursively processes closures that capture *gorm.DB values.
func (a *usageAnalyzer) processMethodCalls(fn *ssa.Function, visited map[*ssa.Function]bool) {
	if fn == nil || fn.Blocks == nil {
		return
	}
	if visited[fn] {
		return
	}
	visited[fn] = true

	// Detect which blocks are in loops (for loop-based reuse detection)
	loopBlocks := a.detectLoopBlocks(fn)

	// Collect defers for second pass
	var defers []*ssa.Defer

	// First pass: process regular calls and go statements
	for _, block := range fn.Blocks {
		isInLoop := loopBlocks[block]

		for _, instr := range block.Instrs {
			// Check for MakeClosure to process closures recursively
			if mc, ok := instr.(*ssa.MakeClosure); ok {
				if closureFn, ok := mc.Fn.(*ssa.Function); ok {
					// Check if closure captures any *gorm.DB values
					if a.closureCapturesGormDB(mc) {
						a.processMethodCalls(closureFn, visited)
					}
				}
				continue
			}

			switch v := instr.(type) {
			case *ssa.Call:
				a.processCall(v, isInLoop, loopBlocks)
			case *ssa.Defer:
				// Collect defers for second pass
				defers = append(defers, v)
			case *ssa.Go:
				// Go statements: process the closure, check for violations
				a.processGoStatement(v)
			case *ssa.Send:
				// Channel send: if sending *gorm.DB, mark as polluted
				a.processSend(v)
			case *ssa.Store:
				// Store to slice/struct field: if storing *gorm.DB, mark as polluted
				a.processStore(v)
			case *ssa.MapUpdate:
				// Map update: if storing *gorm.DB, mark as polluted
				a.processMapUpdate(v)
			case *ssa.MakeInterface:
				// MakeInterface: if wrapping *gorm.DB in interface{}, mark as polluted
				// (may be extracted via type assertion and used elsewhere)
				a.processMakeInterface(v)
			}
		}
	}

	// Second pass: process defer statements (after all regular calls)
	for _, d := range defers {
		a.processDeferStatement(d)
	}
}

// =============================================================================
// Violation Detection
// =============================================================================

// detectReachabilityViolations performs the second pass to detect violations.
// For each polluted block, checks if ANY OTHER polluted block can reach it.
// This handles SSA block ordering that doesn't match execution order.
func (a *usageAnalyzer) detectReachabilityViolations() {
	for _, state := range a.states {
		if len(state.pollutedBlocks) <= 1 {
			continue // Need at least 2 pollution sites for a violation
		}

		// For each polluted block, check if another polluted block can reach it
		for targetBlock, targetPos := range state.pollutedBlocks {
			for srcBlock := range state.pollutedBlocks {
				if srcBlock == targetBlock {
					continue // Same block
				}

				srcPos := state.pollutedBlocks[srcBlock]

				// Different functions (closure): check cross-function pollution.
				// Use isDescendantOf for nested closure support.
				if srcBlock.Parent() != targetBlock.Parent() {
					srcInParent := srcBlock.Parent() == a.fn
					targetInParent := targetBlock.Parent() == a.fn
					srcIsDescendant := isDescendantOf(srcBlock.Parent(), a.fn)
					targetIsDescendant := isDescendantOf(targetBlock.Parent(), a.fn)

					// Case 1: src in descendant closure, target in parent function
					// Violation if src executes before target (by code position)
					if srcIsDescendant && targetInParent && srcPos < targetPos {
						state.violations = append(state.violations, targetPos)
						break
					}

					// Case 2: src in parent function, target in descendant closure
					// Violation if src executes before target (by code position)
					if srcInParent && targetIsDescendant && srcPos < targetPos {
						state.violations = append(state.violations, targetPos)
						break
					}

					// Case 3: both in descendant closures (potentially different ones)
					// Violation if src executes before target (by code position)
					if srcIsDescendant && targetIsDescendant && srcPos < targetPos {
						state.violations = append(state.violations, targetPos)
						break
					}

					continue
				}

				// Same function: check CFG reachability
				if a.canReach(srcBlock, targetBlock) {
					state.violations = append(state.violations, targetPos)
					break // Only need to find one reaching pollution site
				}
			}
		}
	}
}

// collectViolations collects all detected violations.
func (a *usageAnalyzer) collectViolations() []violation {
	var violations []violation

	for _, state := range a.states {
		for _, pos := range state.violations {
			violations = append(violations, violation{
				pos:     pos,
				message: "*gorm.DB instance reused after chain method (use .Session(&gorm.Session{}) to make it safe)",
			})
		}
	}

	return violations
}

// isDescendantOf checks if fn is a descendant of ancestor by traversing the Parent() chain.
// This is used to detect nested closures - a closure inside another closure has the
// inner closure as Parent, not the original function.
func isDescendantOf(fn, ancestor *ssa.Function) bool {
	if fn == nil || ancestor == nil {
		return false
	}
	for current := fn; current != nil; current = current.Parent() {
		if current == ancestor {
			return true
		}
	}
	return false
}
