// Package internal provides SSA-based analysis for GORM *gorm.DB reuse detection.
package internal

import (
	"go/token"

	"golang.org/x/tools/go/ssa"
)

// =============================================================================
// PollutionTracker
//
// PollutionTracker tracks the pollution state of *gorm.DB values.
// It maintains a mapping from mutable roots to their pollution state and violations.
//
// Lifecycle:
//   1. TRACKING PHASE: MarkPolluted() and AddViolation() can be called
//   2. DETECTION PHASE: IsPollutedAt/Anywhere() for reads, AddViolation() for writes
//   3. COLLECTION PHASE: CollectViolations() returns all detected violations
// =============================================================================

// PollutionTracker tracks pollution state and violations for *gorm.DB values.
type PollutionTracker struct {
	// states maps each tracked *gorm.DB chain root to its state.
	states map[ssa.Value]*valueState

	// cfgAnalyzer is used for reachability checks.
	cfgAnalyzer *CFGAnalyzer

	// analyzedFn is the root function being analyzed (for closure detection).
	analyzedFn *ssa.Function
}

// valueState tracks the state of a single *gorm.DB value.
type valueState struct {
	// pollutedBlocks maps blocks where this value was polluted to the position.
	pollutedBlocks map[*ssa.BasicBlock]token.Pos

	// violations tracks positions where polluted value was reused.
	violations []token.Pos
}

// NewPollutionTracker creates a new PollutionTracker.
func NewPollutionTracker(cfgAnalyzer *CFGAnalyzer, fn *ssa.Function) *PollutionTracker {
	return &PollutionTracker{
		states:      make(map[ssa.Value]*valueState),
		cfgAnalyzer: cfgAnalyzer,
		analyzedFn:  fn,
	}
}

// MarkPolluted marks the root as polluted at the given block and position.
// Returns true if the block was newly polluted (not already polluted).
func (p *PollutionTracker) MarkPolluted(root ssa.Value, block *ssa.BasicBlock, pos token.Pos) bool {
	state := p.getOrCreateState(root)
	if _, exists := state.pollutedBlocks[block]; exists {
		return false
	}
	state.pollutedBlocks[block] = pos
	return true
}

// AddViolation records a violation for the given root at the specified position.
func (p *PollutionTracker) AddViolation(root ssa.Value, pos token.Pos) {
	state := p.getOrCreateState(root)
	state.violations = append(state.violations, pos)
}

// IsPollutedInBlock checks if the root is polluted in the given block.
func (p *PollutionTracker) IsPollutedInBlock(root ssa.Value, block *ssa.BasicBlock) bool {
	state := p.states[root]
	if state == nil {
		return false
	}
	_, exists := state.pollutedBlocks[block]
	return exists
}

// IsPollutedAt checks if the value is polluted at the given block.
// A value is polluted at block B if there exists a polluted block A
// such that A can reach B (there is a path from A to B in the CFG).
func (p *PollutionTracker) IsPollutedAt(root ssa.Value, targetBlock *ssa.BasicBlock) bool {
	state := p.states[root]
	if state == nil || len(state.pollutedBlocks) == 0 {
		return false
	}
	if targetBlock == nil {
		return false
	}
	for pollutedBlock := range state.pollutedBlocks {
		if pollutedBlock == nil {
			continue
		}
		// If pollution is from a different function (closure), conservatively consider it reachable
		if pollutedBlock.Parent() != targetBlock.Parent() {
			return true
		}
		// Same function: check if polluted block can reach target block
		if p.cfgAnalyzer.CanReach(pollutedBlock, targetBlock) {
			return true
		}
	}
	return false
}

// IsPollutedAnywhere checks if the value is polluted anywhere in the given function.
// Used for defer statements which execute at function exit.
func (p *PollutionTracker) IsPollutedAnywhere(root ssa.Value, fn *ssa.Function) bool {
	state := p.states[root]
	if state == nil || len(state.pollutedBlocks) == 0 {
		return false
	}
	for pollutedBlock := range state.pollutedBlocks {
		if pollutedBlock == nil {
			continue
		}
		// Check if this pollution is from the same function or a closure of it
		if pollutedBlock.Parent() == fn {
			return true
		}
		// Also check closures (parent function captures the value)
		// Closure pollution affects the parent function's defers
		return true
	}
	return false
}

// GetState returns the state for a root, or nil if not tracked.
func (p *PollutionTracker) GetState(root ssa.Value) *valueState {
	return p.states[root]
}

// CollectViolations returns all detected violations.
func (p *PollutionTracker) CollectViolations() []Violation {
	var violations []Violation

	for _, state := range p.states {
		for _, pos := range state.violations {
			violations = append(violations, Violation{
				Pos:     pos,
				Message: "*gorm.DB instance reused after chain method (use .Session(&gorm.Session{}) to make it safe)",
			})
		}
	}

	return violations
}

// DetectReachabilityViolations performs cross-block violation detection.
// For each polluted block, checks if ANY OTHER polluted block can reach it.
// This handles SSA block ordering that doesn't match execution order.
func (p *PollutionTracker) DetectReachabilityViolations() {
	for _, state := range p.states {
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
				if srcBlock.Parent() != targetBlock.Parent() {
					srcInParent := srcBlock.Parent() == p.analyzedFn
					targetInParent := targetBlock.Parent() == p.analyzedFn
					srcIsDescendant := isDescendantOf(srcBlock.Parent(), p.analyzedFn)
					targetIsDescendant := isDescendantOf(targetBlock.Parent(), p.analyzedFn)

					// Case 1: src in descendant closure, target in parent function
					if srcIsDescendant && targetInParent && srcPos < targetPos {
						state.violations = append(state.violations, targetPos)
						break
					}

					// Case 2: src in parent function, target in descendant closure
					if srcInParent && targetIsDescendant && srcPos < targetPos {
						state.violations = append(state.violations, targetPos)
						break
					}

					// Case 3: both in descendant closures (potentially different ones)
					if srcIsDescendant && targetIsDescendant && srcPos < targetPos {
						state.violations = append(state.violations, targetPos)
						break
					}

					continue
				}

				// Same function: check CFG reachability
				if p.cfgAnalyzer.CanReach(srcBlock, targetBlock) {
					state.violations = append(state.violations, targetPos)
					break // Only need to find one reaching pollution site
				}
			}
		}
	}
}

// =============================================================================
// Internal Implementation
// =============================================================================

// getOrCreateState returns the state for the given root, creating it if necessary.
func (p *PollutionTracker) getOrCreateState(root ssa.Value) *valueState {
	state := p.states[root]
	if state == nil {
		state = &valueState{
			pollutedBlocks: make(map[*ssa.BasicBlock]token.Pos),
		}
		p.states[root] = state
	}
	return state
}

// isDescendantOf checks if fn is a descendant of ancestor by traversing the Parent() chain.
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

// Violation represents a detected reuse violation.
type Violation struct {
	Pos     token.Pos
	Message string
}
