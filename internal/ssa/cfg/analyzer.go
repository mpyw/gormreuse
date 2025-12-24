// Package cfg provides control flow graph analysis for gormreuse.
package cfg

import (
	"golang.org/x/tools/go/ssa"
)

// Analyzer provides control flow graph analysis for SSA functions.
// It is stateless and can be reused across multiple analyses.
type Analyzer struct{}

// New creates a new Analyzer.
func New() *Analyzer {
	return &Analyzer{}
}

// LoopInfo contains information about loops in a function.
type LoopInfo struct {
	loopBlocks map[*ssa.BasicBlock]bool
}

// IsInLoop returns true if the block is inside a loop.
func (l *LoopInfo) IsInLoop(block *ssa.BasicBlock) bool {
	return l.loopBlocks[block]
}

// CanReach checks if srcBlock can reach dstBlock in the CFG using BFS.
func (a *Analyzer) CanReach(src, dst *ssa.BasicBlock) bool {
	if src == nil || dst == nil {
		return false
	}
	if src == dst {
		return true
	}

	visited := make(map[*ssa.BasicBlock]bool)
	queue := []*ssa.BasicBlock{src}
	visited[src] = true

	for len(queue) > 0 {
		block := queue[0]
		queue = queue[1:]

		for _, succ := range block.Succs {
			if succ == dst {
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

// DetectLoops analyzes the function and returns loop information.
func (a *Analyzer) DetectLoops(fn *ssa.Function) *LoopInfo {
	loopBlocks := make(map[*ssa.BasicBlock]bool)
	if fn.Blocks == nil {
		return &LoopInfo{loopBlocks: loopBlocks}
	}

	// Build block index map
	blockIndex := make(map[*ssa.BasicBlock]int)
	for i, b := range fn.Blocks {
		blockIndex[b] = i
	}

	// Detect back-edges that create cycles
	for _, block := range fn.Blocks {
		for _, succ := range block.Succs {
			if blockIndex[succ] <= blockIndex[block] {
				if a.CanReach(succ, block) {
					a.markLoopBlocks(fn, succ, block, loopBlocks, blockIndex)
				}
			}
		}
	}

	return &LoopInfo{loopBlocks: loopBlocks}
}

// markLoopBlocks marks blocks that are part of a loop.
func (a *Analyzer) markLoopBlocks(fn *ssa.Function, loopHead, loopTail *ssa.BasicBlock, loopBlocks map[*ssa.BasicBlock]bool, blockIndex map[*ssa.BasicBlock]int) {
	headIdx := blockIndex[loopHead]
	tailIdx := blockIndex[loopTail]

	for _, block := range fn.Blocks {
		idx := blockIndex[block]
		if idx >= headIdx && idx <= tailIdx {
			loopBlocks[block] = true
		}
	}
}

// IsDefinedOutsideLoop checks if a value is defined outside the loop.
func (a *Analyzer) IsDefinedOutsideLoop(v ssa.Value, loopInfo *LoopInfo) bool {
	instr, ok := v.(ssa.Instruction)
	if !ok {
		return true
	}

	block := instr.Block()
	if block == nil {
		return true
	}

	return !loopInfo.IsInLoop(block)
}
