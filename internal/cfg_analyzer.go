// Package internal provides SSA-based analysis for GORM *gorm.DB reuse detection.
package internal

import (
	"golang.org/x/tools/go/ssa"
)

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
					c.markLoopBlocks(fn, succ, block, loopBlocks, blockIndex)
				}
			}
		}
	}

	return &LoopInfo{LoopBlocks: loopBlocks}
}

// CanReach checks if srcBlock can reach dstBlock in the CFG using BFS.
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

// =============================================================================
// Internal Implementation
// =============================================================================

// markLoopBlocks marks blocks that are part of a loop.
func (c *CFGAnalyzer) markLoopBlocks(fn *ssa.Function, loopHead, loopTail *ssa.BasicBlock, loopBlocks map[*ssa.BasicBlock]bool, blockIndex map[*ssa.BasicBlock]int) {
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
