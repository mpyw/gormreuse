// Package internal provides SSA-based analysis for GORM *gorm.DB reuse detection.
package internal

import (
	"golang.org/x/tools/go/ssa"
)

// =============================================================================
// Control Flow Graph (CFG) Analysis
//
// This file contains methods for analyzing the control flow graph of SSA functions.
// This includes loop detection, reachability analysis, and block classification.
// =============================================================================

// detectLoopBlocks returns a set of blocks that are inside loops.
func (a *usageAnalyzer) detectLoopBlocks(fn *ssa.Function) map[*ssa.BasicBlock]bool {
	loopBlocks := make(map[*ssa.BasicBlock]bool)
	if fn.Blocks == nil {
		return loopBlocks
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
				if a.canReach(succ, block) {
					// True back-edge detected: mark all blocks from succ to block as in-loop
					a.markLoopBlocks(fn, succ, block, loopBlocks, blockIndex)
				}
			}
		}
	}

	return loopBlocks
}

// markLoopBlocks marks blocks that are part of a loop.
func (a *usageAnalyzer) markLoopBlocks(fn *ssa.Function, loopHead, loopTail *ssa.BasicBlock, loopBlocks map[*ssa.BasicBlock]bool, blockIndex map[*ssa.BasicBlock]int) {
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

// canReach checks if srcBlock can reach dstBlock in the CFG using BFS.
func (a *usageAnalyzer) canReach(srcBlock, dstBlock *ssa.BasicBlock) bool {
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

// isRootDefinedOutsideLoop checks if the mutable root is defined outside loop blocks.
func (a *usageAnalyzer) isRootDefinedOutsideLoop(root ssa.Value, loopBlocks map[*ssa.BasicBlock]bool) bool {
	// Get the instruction that defines this value
	instr, ok := root.(ssa.Instruction)
	if !ok {
		return true // Non-instruction values (parameters, etc.) are outside loops
	}

	// Check if the defining block is in a loop
	block := instr.Block()
	if block == nil {
		return true
	}

	return !loopBlocks[block]
}
