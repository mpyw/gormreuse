package cfg

import (
	"golang.org/x/tools/go/ssa"
)

// rangeFuncLoopSynthetic is the ssa.Function.Synthetic value that go/ssa gives
// the yield closure holding the body of a range-over-func loop.
const rangeFuncLoopSynthetic = "range-over-func yield"

// LoopInfo contains information about loops in a function.
//
// Used to detect when a mutable root defined outside a loop is used
// inside the loop, which is always a violation (reused on each iteration).
type LoopInfo struct {
	loopBlocks  map[*ssa.BasicBlock]bool // Blocks that are part of any loop
	loopHeaders map[*ssa.BasicBlock]bool // Blocks that are loop headers
}

// IsInLoop returns true if the block is inside a loop.
func (l *LoopInfo) IsInLoop(block *ssa.BasicBlock) bool {
	return l.loopBlocks[block]
}

// IsLoopHeader returns true if the block is a loop header.
// Loop headers are entry points to loops where Phi nodes merge
// values from outside the loop and from loop back-edges.
func (l *LoopInfo) IsLoopHeader(block *ssa.BasicBlock) bool {
	return l.loopHeaders[block]
}

// IsDefinedOutsideLoop checks if a value whose defining block is def is
// defined outside the loop.
//
// Used to detect the pattern:
//
//	q := db.Where("x")        // Defined OUTSIDE loop
//	for _, item := range items {
//	    q.Where(item).Find(nil)  // Used INSIDE loop - VIOLATION
//	}
//
// When a mutable root is defined outside a loop but used inside, each
// iteration reuses the same root, which is always a violation. The caller
// passes the root's defining block (tracer.RootBlock), so that a root that is
// not an instruction, such as a field of a struct a call returned, is placed
// by the instruction that defines it (#250). A nil block (no definition, such
// as a global) is outside, and so is a block of another function, such as
// the function that declares a closure: l holds only its own function's
// loops.
func (l *LoopInfo) IsDefinedOutsideLoop(def *ssa.BasicBlock) bool {
	return def == nil || !l.IsInLoop(def)
}

// DetectLoops analyzes the function and returns loop information.
//
// Algorithm:
//  1. Assign index to each block based on order in fn.Blocks
//  2. Find back-edges: edges where succ.index <= src.index
//  3. For each back-edge, verify it's a real loop (succ can reach src)
//  4. Mark all blocks between loop header and tail as loop blocks
//  5. Mark loop headers for special handling of Phi nodes
//
// This handles for, for-range, and while-style loops.
//
// The body of a range-over-func loop (`for v := range seq`) has no back-edge:
// SSA compiles it into a synthetic yield closure that the iterator calls once
// per element. Every block of that closure is therefore in a loop (#177). It
// has no header, because values from earlier iterations reach it through
// captured variables, not Phi nodes.
func (a *Analyzer) DetectLoops(fn *ssa.Function) *LoopInfo {
	loopBlocks := make(map[*ssa.BasicBlock]bool)
	loopHeaders := make(map[*ssa.BasicBlock]bool)
	if fn.Blocks == nil {
		return &LoopInfo{
			loopBlocks:  loopBlocks,
			loopHeaders: loopHeaders,
		}
	}
	if fn.Synthetic == rangeFuncLoopSynthetic {
		for _, b := range fn.Blocks {
			loopBlocks[b] = true
		}
	}

	// Build block index map (for back-edge detection)
	blockIndex := make(map[*ssa.BasicBlock]int)
	for i, b := range fn.Blocks {
		blockIndex[b] = i
	}

	// Detect back-edges that create cycles
	// A back-edge goes from a higher-indexed block to a lower-indexed block
	for _, block := range fn.Blocks {
		for _, succ := range block.Succs {
			// Potential back-edge: verify it creates a cycle
			if blockIndex[succ] <= blockIndex[block] && a.CanReach(succ, block) {
				a.markLoopBlocks(fn, succ, block, loopBlocks)
				loopHeaders[succ] = true // succ is the loop header
			}
		}
	}

	return &LoopInfo{
		loopBlocks:  loopBlocks,
		loopHeaders: loopHeaders,
	}
}

// markLoopBlocks marks blocks that are part of the loop whose header is loopHead
// and whose back-edge originates at loopTail.
//
// A block is inside the loop iff it lies on a path from the header to the
// back-edge source: reachable FROM the header AND able to reach loopTail (from
// which the back-edge returns to the header). This deliberately excludes
// loop-EXIT blocks — reachable from the header but unable to reach loopTail.
//
// The previous implementation marked every block whose CFG index fell in the
// interval [headIdx, tailIdx]. That wrongly included blocks that merely happen to
// be ordered between the header and the back-edge, most notably a loop-exit block
// living inside a switch arm (issue #113): such a block was then treated as
// in-loop, and the "externally-defined root used in a loop" heuristic fired a
// spurious reuse violation.
func (a *Analyzer) markLoopBlocks(_ *ssa.Function, loopHead, loopTail *ssa.BasicBlock, loopBlocks map[*ssa.BasicBlock]bool) {
	// Standard natural-loop body for the back-edge loopTail -> loopHead: the
	// header plus every block that can reach loopTail without passing through the
	// header. Found by a single reverse walk from loopTail over predecessors,
	// bounded by the (pre-marked) header. This excludes loop-exit blocks — they
	// cannot reach loopTail — which the old index-interval marking wrongly
	// included (#113). O(V+E), reusing loopBlocks with no extra allocation.
	loopBlocks[loopHead] = true
	if loopTail == loopHead {
		return
	}
	loopBlocks[loopTail] = true
	stack := []*ssa.BasicBlock{loopTail}
	for len(stack) > 0 {
		b := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, p := range b.Preds {
			if !loopBlocks[p] {
				loopBlocks[p] = true
				stack = append(stack, p)
			}
		}
	}
}
