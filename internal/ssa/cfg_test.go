package ssa

import (
	"testing"

	"golang.org/x/tools/go/ssa"
)

// =============================================================================
// CFGAnalyzer Tests
// =============================================================================

func TestCFGAnalyzer_CanReach_NilCases(t *testing.T) {
	cfgAnalyzer := NewCFGAnalyzer()

	// Both nil
	if cfgAnalyzer.CanReach(nil, nil) {
		t.Error("CanReach(nil, nil) should return false")
	}
}

func TestCFGAnalyzer_CanReach_SameBlock(t *testing.T) {
	cfgAnalyzer := NewCFGAnalyzer()
	block := &ssa.BasicBlock{}

	// Same block should be reachable
	if !cfgAnalyzer.CanReach(block, block) {
		t.Error("CanReach(block, block) should return true")
	}
}

func TestCFGAnalyzer_CanReach_WithSuccessors(t *testing.T) {
	cfgAnalyzer := NewCFGAnalyzer()

	// Create a chain: block1 -> block2 -> block3
	block1 := &ssa.BasicBlock{}
	block2 := &ssa.BasicBlock{}
	block3 := &ssa.BasicBlock{}

	block1.Succs = []*ssa.BasicBlock{block2}
	block2.Succs = []*ssa.BasicBlock{block3}

	// block1 can reach block3 via block2
	if !cfgAnalyzer.CanReach(block1, block3) {
		t.Error("block1 should reach block3")
	}

	// block3 cannot reach block1 (no back edge)
	if cfgAnalyzer.CanReach(block3, block1) {
		t.Error("block3 should not reach block1")
	}

	// block1 can reach block2 directly
	if !cfgAnalyzer.CanReach(block1, block2) {
		t.Error("block1 should reach block2")
	}
}

func TestCFGAnalyzer_CanReach_Unreachable(t *testing.T) {
	cfgAnalyzer := NewCFGAnalyzer()

	block1 := &ssa.BasicBlock{}
	block2 := &ssa.BasicBlock{}

	// No successors, so unreachable
	if cfgAnalyzer.CanReach(block1, block2) {
		t.Error("Disconnected blocks should not be reachable")
	}
}

func TestCFGAnalyzer_DetectLoops_NilBlocks(t *testing.T) {
	cfgAnalyzer := NewCFGAnalyzer()

	// Function with nil Blocks
	fn := &ssa.Function{}
	loopInfo := cfgAnalyzer.DetectLoops(fn)

	if loopInfo.LoopBlocks == nil {
		t.Error("Expected non-nil map")
	}
	if len(loopInfo.LoopBlocks) != 0 {
		t.Error("Expected empty map for nil Blocks")
	}
}

func TestCFGAnalyzer_DetectLoops_WithBlocks(t *testing.T) {
	cfgAnalyzer := NewCFGAnalyzer()

	// Create blocks for a simple loop: block0 -> block1 -> block0 (back edge)
	block0 := &ssa.BasicBlock{Index: 0}
	block1 := &ssa.BasicBlock{Index: 1}

	block0.Succs = []*ssa.BasicBlock{block1}
	block1.Succs = []*ssa.BasicBlock{block0} // back edge

	fn := &ssa.Function{
		Blocks: []*ssa.BasicBlock{block0, block1},
	}

	loopInfo := cfgAnalyzer.DetectLoops(fn)

	// Both blocks should be marked as in loop
	if !loopInfo.IsInLoop(block0) {
		t.Error("block0 should be in loop")
	}
	if !loopInfo.IsInLoop(block1) {
		t.Error("block1 should be in loop")
	}
}

func TestCFGAnalyzer_DetectLoops_NoLoop(t *testing.T) {
	cfgAnalyzer := NewCFGAnalyzer()

	// Linear blocks: block0 -> block1 -> block2 (no back edge)
	block0 := &ssa.BasicBlock{Index: 0}
	block1 := &ssa.BasicBlock{Index: 1}
	block2 := &ssa.BasicBlock{Index: 2}

	block0.Succs = []*ssa.BasicBlock{block1}
	block1.Succs = []*ssa.BasicBlock{block2}

	fn := &ssa.Function{
		Blocks: []*ssa.BasicBlock{block0, block1, block2},
	}

	loopInfo := cfgAnalyzer.DetectLoops(fn)

	// No blocks should be in loop
	if len(loopInfo.LoopBlocks) != 0 {
		t.Errorf("Expected no loop blocks, got %d", len(loopInfo.LoopBlocks))
	}
}

func TestCFGAnalyzer_IsDefinedOutsideLoop_NilValue(t *testing.T) {
	cfgAnalyzer := NewCFGAnalyzer()
	loopInfo := &LoopInfo{LoopBlocks: make(map[*ssa.BasicBlock]bool)}

	// nil value should be considered outside loop (non-instruction)
	result := cfgAnalyzer.IsDefinedOutsideLoop(nil, loopInfo)
	if !result {
		t.Error("nil value should be considered outside loop")
	}
}

func TestCFGAnalyzer_IsDefinedOutsideLoop_NilBlock(t *testing.T) {
	cfgAnalyzer := NewCFGAnalyzer()

	// Call is an instruction but Block() returns nil for uninitialized Call
	call := &ssa.Call{}

	loopInfo := &LoopInfo{LoopBlocks: map[*ssa.BasicBlock]bool{}}

	// Instruction with nil block should be considered outside loop
	result := cfgAnalyzer.IsDefinedOutsideLoop(call, loopInfo)
	if !result {
		t.Error("Instruction with nil block should be outside loop")
	}
}

func TestCFGAnalyzer_MarkLoopBlocks(t *testing.T) {
	cfgAnalyzer := NewCFGAnalyzer()

	block0 := &ssa.BasicBlock{Index: 0}
	block1 := &ssa.BasicBlock{Index: 1}
	block2 := &ssa.BasicBlock{Index: 2}

	fn := &ssa.Function{
		Blocks: []*ssa.BasicBlock{block0, block1, block2},
	}

	blockIndex := map[*ssa.BasicBlock]int{
		block0: 0,
		block1: 1,
		block2: 2,
	}

	loopBlocks := make(map[*ssa.BasicBlock]bool)

	// Mark blocks from block0 to block2 as loop
	cfgAnalyzer.MarkLoopBlocks(fn, block0, block2, loopBlocks, blockIndex)

	if !loopBlocks[block0] || !loopBlocks[block1] || !loopBlocks[block2] {
		t.Error("All blocks should be marked as in loop")
	}
}
