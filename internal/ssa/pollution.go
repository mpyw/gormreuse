package ssa

import (
	"go/token"

	"golang.org/x/tools/go/ssa"
)

// =============================================================================
// Violation
// =============================================================================

// Violation represents a detected reuse violation.
type Violation struct {
	Pos     token.Pos
	Message string
}

// =============================================================================
// CFGAnalyzer
//
// CFGAnalyzer provides control flow graph analysis for SSA functions.
// This includes loop detection, reachability analysis, and block classification.
//
// This component is stateless and can be reused across multiple analyses.
// =============================================================================

// CFGAnalyzer analyzes control flow graphs of SSA functions.
type CFGAnalyzer struct{}

// NewCFGAnalyzer creates a new CFGAnalyzer.
func NewCFGAnalyzer() *CFGAnalyzer {
	return &CFGAnalyzer{}
}

// LoopInfo contains information about loops in a function.
type LoopInfo struct {
	// LoopBlocks is a set of blocks that are inside loops.
	LoopBlocks map[*ssa.BasicBlock]bool
}

// IsInLoop returns true if the given block is inside a loop.
func (l *LoopInfo) IsInLoop(block *ssa.BasicBlock) bool {
	return l.LoopBlocks[block]
}

// DetectLoops analyzes the function and returns loop information.
//
// Loop detection algorithm:
//
//  1. Build block index map (block ordering from SSA)
//  2. Find back-edges: edge A→B where B appears before A in block order
//  3. Verify back-edge creates a cycle: CanReach(B, A) must be true
//  4. Mark all blocks between loop head (B) and loop tail (A) as in-loop
//
// Example:
//
//	for i := range items {  // Block 1 (loop head)
//	    q.Find(nil)         // Block 2 (loop body)
//	}                       // Block 2 → Block 1 (back-edge)
//	// Block 3 (after loop)
//
//	Block order: [0:entry, 1:loop.head, 2:loop.body, 3:exit]
//	Back-edge: Block2 → Block1 (Block1 index < Block2 index)
//	Cycle check: CanReach(Block1, Block2) = true
//	Result: {Block1: true, Block2: true}
//
// Why cycle check is needed:
//
//	if cond {               // Block 1
//	    // ...              // Block 2
//	} else {
//	    // ...              // Block 3 (appears after Block 1 in block order)
//	}
//	// merge                // Block 4 (might appear before Block 3!)
//
//	Without cycle check, Block4 → Block3 might look like a back-edge,
//	but it's just merge block ordering. CanReach(Block4, Block3) = false.
func (c *CFGAnalyzer) DetectLoops(fn *ssa.Function) *LoopInfo {
	loopBlocks := make(map[*ssa.BasicBlock]bool)
	if fn.Blocks == nil {
		return &LoopInfo{LoopBlocks: loopBlocks}
	}

	// Build block index map
	blockIndex := make(map[*ssa.BasicBlock]int)
	for i, b := range fn.Blocks {
		blockIndex[b] = i
	}

	// Detect back-edges: a true back-edge creates a cycle in the CFG.
	// For an edge block -> succ to be a back-edge:
	// 1. succ must appear before block in block ordering (potential back-edge)
	// 2. succ must be able to reach block (confirming there's a cycle)
	// This distinguishes real loops from if/else merge edges where the merge
	// block comes before else-branch in block ordering.
	for _, block := range fn.Blocks {
		for _, succ := range block.Succs {
			if blockIndex[succ] <= blockIndex[block] {
				// Potential back-edge - verify it creates a cycle
				if c.CanReach(succ, block) {
					// True back-edge detected: mark all blocks from succ to block as in-loop
					c.MarkLoopBlocks(fn, succ, block, loopBlocks, blockIndex)
				}
			}
		}
	}

	return &LoopInfo{LoopBlocks: loopBlocks}
}

// CanReach checks if srcBlock can reach dstBlock in the CFG using BFS.
//
// This is a forward reachability check: can we get from src to dst
// by following successor edges?
//
// Example CFG:
//
//	        ┌─────┐
//	        │  0  │ entry
//	        └──┬──┘
//	           ↓
//	        ┌─────┐
//	   ┌───→│  1  │←───┐ loop head
//	   │    └──┬──┘    │
//	   │       ↓       │
//	   │    ┌─────┐    │
//	   │    │  2  │────┘ back-edge
//	   │    └──┬──┘
//	   │       ↓
//	   │    ┌─────┐
//	   └────│  3  │ exit (returns to 1 or exits)
//	        └─────┘
//
//	CanReach(0, 2) = true  (0 → 1 → 2)
//	CanReach(2, 1) = true  (2 → 1 via back-edge, or 2 → 3 → 1)
//	CanReach(3, 0) = false (no path from exit to entry)
//
// Used for:
//   - Loop detection: verify back-edges create cycles
//   - Pollution reachability: can pollution reach a use site?
func (c *CFGAnalyzer) CanReach(srcBlock, dstBlock *ssa.BasicBlock) bool {
	if srcBlock == nil || dstBlock == nil {
		return false
	}
	// Same block means reachable (important for loops with back-edges)
	if srcBlock == dstBlock {
		return true
	}

	visited := make(map[*ssa.BasicBlock]bool)
	queue := []*ssa.BasicBlock{srcBlock}
	visited[srcBlock] = true

	for len(queue) > 0 {
		block := queue[0]
		queue = queue[1:]

		for _, succ := range block.Succs {
			if succ == dstBlock {
				return true
			}
			if !visited[succ] {
				visited[succ] = true
				queue = append(queue, succ)
			}
		}
	}
	return false
}

// IsDefinedOutsideLoop checks if a value is defined outside the given loop blocks.
func (c *CFGAnalyzer) IsDefinedOutsideLoop(v ssa.Value, loopInfo *LoopInfo) bool {
	// Get the instruction that defines this value
	instr, ok := v.(ssa.Instruction)
	if !ok {
		return true // Non-instruction values (parameters, etc.) are outside loops
	}

	// Check if the defining block is in a loop
	block := instr.Block()
	if block == nil {
		return true
	}

	return !loopInfo.IsInLoop(block)
}

// MarkLoopBlocks marks blocks that are part of a loop.
func (c *CFGAnalyzer) MarkLoopBlocks(fn *ssa.Function, loopHead, loopTail *ssa.BasicBlock, loopBlocks map[*ssa.BasicBlock]bool, blockIndex map[*ssa.BasicBlock]int) {
	headIdx := blockIndex[loopHead]
	tailIdx := blockIndex[loopTail]

	// Mark all blocks between head and tail (inclusive) as in-loop
	for _, block := range fn.Blocks {
		idx := blockIndex[block]
		if idx >= headIdx && idx <= tailIdx {
			loopBlocks[block] = true
		}
	}
}

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
//
// Example:
//
//	q := db.Where("x")      // Block 0
//	if cond {
//	    q.Find(nil)         // Block 1: pollutes q
//	}
//	q.Find(nil)             // Block 2: IsPollutedAt(q, Block2)?
//
//	CFG:
//	  Block0 → Block1 (if true)
//	        ↘ Block2 (if false, or after Block1)
//	  Block1 → Block2
//
//	Analysis:
//	  - q is polluted in Block1
//	  - CanReach(Block1, Block2) = true
//	  - IsPollutedAt(q, Block2) returns true → violation detected
//
// Cross-function case:
//
//	func outer() {
//	    q := db.Where("x")
//	    f := func() { q.Find(nil) }  // Closure pollutes q
//	    q.Find(nil)                  // Parent function uses polluted q
//	}
//
//	If polluted block is in a different function (closure), we
//	conservatively consider it reachable (returns true).
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
//
// This method is called AFTER all instructions have been processed.
// It detects violations where multiple uses of the same root exist
// and one can reach another through CFG paths.
//
// Example:
//
//	q := db.Where("x")
//	q.Find(nil)              // Block 1: pollutes q at pos=10
//	q.Find(nil)              // Block 2: pollutes q at pos=20
//
//	SSA blocks may be in arbitrary order. DetectReachabilityViolations:
//	  1. Finds all polluted blocks for q: {Block1:pos10, Block2:pos20}
//	  2. For Block2, checks if Block1 can reach it
//	  3. If yes, adds violation at pos=20
//
// Cross-function detection (closures):
//
//	func outer() {
//	    q := db.Where("x")
//	    f := func() { q.Find(nil) }  // pos=10, closure
//	    f()
//	    q.Find(nil)                  // pos=20, parent
//	}
//
//	Cases handled:
//	  - Case 1: src in closure, target in parent (pos order check)
//	  - Case 2: src in parent, target in closure (pos order check)
//	  - Case 3: both in closures (pos order check)
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
					srcIsDescendant := IsFunctionDescendantOf(srcBlock.Parent(), p.analyzedFn)
					targetIsDescendant := IsFunctionDescendantOf(targetBlock.Parent(), p.analyzedFn)

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

// =============================================================================
// Helper Functions
// =============================================================================

// IsFunctionDescendantOf checks if fn is a descendant of ancestor by traversing the Parent() chain.
func IsFunctionDescendantOf(fn, ancestor *ssa.Function) bool {
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
