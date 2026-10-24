package cfg

import (
	"go/constant"
	"maps"
	"slices"

	"golang.org/x/tools/go/ssa"
)

// rangeFuncLoopSynthetic is the ssa.Function.Synthetic value that go/ssa gives
// the yield closure holding the body of a range-over-func loop.
const rangeFuncLoopSynthetic = "range-over-func yield"

// IsRangeFuncLoopBody reports whether fn is the yield closure that holds the body
// of a range-over-func loop.
func IsRangeFuncLoopBody(fn *ssa.Function) bool {
	return fn.Synthetic == rangeFuncLoopSynthetic
}

// LoopInfo contains information about loops in a function.
//
// Used to detect when a mutable root defined outside a loop is used
// inside the loop, which is always a violation (reused on each iteration).
type LoopInfo struct {
	loopBlocks  map[*ssa.BasicBlock]bool // Blocks that are part of any loop
	loopHeaders map[*ssa.BasicBlock]bool // Blocks that are loop headers

	// loops holds the blocks of each loop, one set per header. The back-edges
	// of one header (a continue and the end of the body) make one loop. In a
	// range-over-func body, the blocks that can reach a `return true` are one
	// more loop with no header. A body that always leaves the loop has none.
	loops []map[*ssa.BasicBlock]bool
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

// HasLoopHeaders reports whether l marks any block as a loop header. A nil l
// marks none.
func (l *LoopInfo) HasLoopHeaders() bool {
	return l != nil && len(l.loopHeaders) > 0
}

// IsInLoopWithout reports whether block is in a loop that does not hold def,
// so it runs again without def running again. A nil def, or a def of
// another function, is in none of the loops.
func (l *LoopInfo) IsInLoopWithout(block, def *ssa.BasicBlock) bool {
	return slices.ContainsFunc(l.loops, func(loop map[*ssa.BasicBlock]bool) bool {
		return loop[block] && !loop[def]
	})
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
// per element (#177). Its blocks that can reach a `return true` are in a loop
// (rangeFuncLoopBlocks, #399). A block that only reaches a break or a return
// runs at most once. The loop has no header, because values from earlier
// iterations reach it through captured variables, not Phi nodes.
//
// The result is cached: each call for fn returns the same LoopInfo, which
// must not be changed.
func (a *Analyzer) DetectLoops(fn *ssa.Function) *LoopInfo {
	g := a.graphOf(fn)
	if g.loops == nil {
		g.loops = a.findLoops(fn)
	}
	return g.loops
}

// findLoops is DetectLoops without the cache.
func (a *Analyzer) findLoops(fn *ssa.Function) *LoopInfo {
	info := &LoopInfo{
		loopBlocks:  make(map[*ssa.BasicBlock]bool),
		loopHeaders: make(map[*ssa.BasicBlock]bool),
	}
	if fn.Blocks == nil {
		return info
	}
	if IsRangeFuncLoopBody(fn) {
		if body := rangeFuncLoopBlocks(fn); len(body) > 0 {
			info.loops = append(info.loops, body)
		}
	}

	// Build block index map (for back-edge detection)
	blockIndex := make(map[*ssa.BasicBlock]int)
	for i, b := range fn.Blocks {
		blockIndex[b] = i
	}

	// Detect back-edges that create cycles
	// A back-edge goes from a higher-indexed block to a lower-indexed block
	byHeader := make(map[*ssa.BasicBlock]map[*ssa.BasicBlock]bool)
	for _, block := range fn.Blocks {
		for _, succ := range block.Succs {
			// Potential back-edge: verify it creates a cycle
			if blockIndex[succ] <= blockIndex[block] && a.CanReach(succ, block) {
				loop := byHeader[succ]
				if loop == nil {
					loop = make(map[*ssa.BasicBlock]bool)
					byHeader[succ] = loop
					info.loops = append(info.loops, loop)
				}
				a.markLoopBlocks(fn, succ, block, loop)
				info.loopHeaders[succ] = true // succ is the loop header
			}
		}
	}
	for _, loop := range info.loops {
		maps.Copy(info.loopBlocks, loop)
	}
	return info
}

// rangeFuncLoopBlocks returns the blocks of fn, the yield closure of a
// range-over-func loop, that may run again on a later iteration (#399). go/ssa
// compiles the end of the body and a continue into `return true`, so the
// iterator calls the body again. A break, a return, or a jump out of the loop
// is `return false`, and the iterator must stop: go/ssa panics when it calls
// the body after that. So a block is in the loop only when it can reach a
// return of true. A result that is not a constant counts as true.
func rangeFuncLoopBlocks(fn *ssa.Function) map[*ssa.BasicBlock]bool {
	body := make(map[*ssa.BasicBlock]bool)
	var work []*ssa.BasicBlock
	for _, b := range fn.Blocks {
		if loopReturnsTrue(b) {
			body[b] = true
			work = append(work, b)
		}
	}
	for len(work) > 0 {
		b := work[len(work)-1]
		work = work[:len(work)-1]
		for _, p := range b.Preds {
			if !body[p] {
				body[p] = true
				work = append(work, p)
			}
		}
	}
	return body
}

// loopReturnsTrue reports whether b ends in a return of a yield closure that
// may return true.
func loopReturnsTrue(b *ssa.BasicBlock) bool {
	ret, ok := b.Instrs[len(b.Instrs)-1].(*ssa.Return)
	if !ok || len(ret.Results) != 1 {
		return false
	}
	c, ok := ret.Results[0].(*ssa.Const)
	return !ok || constant.BoolVal(c.Value)
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
//
// loopBlocks holds the blocks of this loop only: the walk uses it as its
// visited set. A set shared by every loop stopped the walk of an outer loop
// at the header of an inner one marked before, so the blocks of the outer
// loop before the inner loop were left out (#252).
func (a *Analyzer) markLoopBlocks(_ *ssa.Function, loopHead, loopTail *ssa.BasicBlock, loopBlocks map[*ssa.BasicBlock]bool) {
	// Standard natural-loop body for the back-edge loopTail -> loopHead: the
	// header plus every block that can reach loopTail without passing through the
	// header. Found by a single reverse walk from loopTail over predecessors,
	// bounded by the (pre-marked) header. This excludes loop-exit blocks — they
	// cannot reach loopTail — which the old index-interval marking wrongly
	// included (#113). O(V+E).
	//
	// The walk keeps to the blocks that loopHead reaches. The back-edges are
	// found by block index, so loopHead does not always dominate loopTail: go/ssa
	// puts the header of `for cond { ... }` after its body, and the edge from the
	// header to the body is taken as the back-edge. An unbounded walk from that
	// header then went on through the blocks before the loop, up to the entry,
	// and marked them in the loop (#252). When loopHead dominates loopTail, every
	// block the walk finds is reachable from it, so the bound changes nothing.
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
			if !loopBlocks[p] && a.CanReach(loopHead, p) {
				loopBlocks[p] = true
				stack = append(stack, p)
			}
		}
	}
}
