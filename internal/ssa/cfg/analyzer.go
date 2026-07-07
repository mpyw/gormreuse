// Package cfg provides control flow graph analysis for gormreuse.
//
// # Overview
//
// This package analyzes the Control Flow Graph (CFG) of SSA functions to:
//   - Detect loops (for special handling of loop-external roots)
//   - Check block reachability (for violation detection)
//
// # CFG Concepts
//
// An SSA function has a CFG where:
//
//   - Nodes are BasicBlocks (sequences of instructions)
//
//   - Edges are control flow transitions (jumps, branches, returns)
//
//     ┌─────────────────────────────────────────────────────────────────────────┐
//     │                    Example: if-else CFG                                 │
//     │                                                                         │
//     │                    ┌─────────────┐                                      │
//     │                    │  Entry (0)  │                                      │
//     │                    │  if cond    │                                      │
//     │                    └──────┬──────┘                                      │
//     │                    true /   \ false                                     │
//     │                        /     \                                          │
//     │              ┌────────▼───┐ ┌─▼────────┐                                │
//     │              │  Then (1)  │ │ Else (2) │                                │
//     │              └────────┬───┘ └─┬────────┘                                │
//     │                       \     /                                           │
//     │                        \   /                                            │
//     │                    ┌────▼─▼────┐                                        │
//     │                    │  Exit (3) │                                        │
//     │                    │ (Phi)     │                                        │
//     │                    └───────────┘                                        │
//     └─────────────────────────────────────────────────────────────────────────┘
//
// # Loop Detection
//
// A loop exists when there's a back-edge in the CFG (edge to a block with
// lower index that can reach the source):
//
//	┌─────────────────────────────────────────────────────────────────────────┐
//	│                    Example: for loop CFG                                │
//	│                                                                         │
//	│                    ┌─────────────┐                                      │
//	│                    │  Entry (0)  │                                      │
//	│                    └──────┬──────┘                                      │
//	│                           │                                             │
//	│                    ┌──────▼──────┐◀───────┐                             │
//	│                    │  Header (1) │        │  back-edge                  │
//	│                    │  for cond   │        │  (loop)                     │
//	│                    └──────┬──────┘        │                             │
//	│                    true /   \ false       │                             │
//	│                        /     \            │                             │
//	│              ┌────────▼───┐ ┌─▼────────┐  │                             │
//	│              │  Body (2)  │ │ Exit (3) │  │                             │
//	│              └────────┬───┘ └──────────┘  │                             │
//	│                       │                   │                             │
//	│                       └───────────────────┘                             │
//	└─────────────────────────────────────────────────────────────────────────┘
package cfg

import (
	"golang.org/x/tools/go/ssa"
)

// Analyzer provides control flow graph analysis for SSA functions.
//
// It is stateless and can be reused across multiple analyses.
// All analysis results are returned as new objects, not stored internally.
type Analyzer struct{}

// New creates a new Analyzer.
func New() *Analyzer {
	return &Analyzer{}
}

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

// CanReach checks if srcBlock can reach dstBlock in the CFG using BFS.
//
// This is used for violation detection: if an earlier use of a mutable root
// can reach a later use, the later use is a violation.
//
// Algorithm: Breadth-first search through successor edges.
//
// Time complexity: O(V + E) where V = blocks, E = edges
func (a *Analyzer) CanReach(src, dst *ssa.BasicBlock) bool {
	if src == nil || dst == nil {
		return false
	}
	if src == dst {
		return true // Same block is trivially reachable
	}

	// BFS traversal
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
//
// Algorithm:
//  1. Assign index to each block based on order in fn.Blocks
//  2. Find back-edges: edges where succ.index <= src.index
//  3. For each back-edge, verify it's a real loop (succ can reach src)
//  4. Mark all blocks between loop header and tail as loop blocks
//  5. Mark loop headers for special handling of Phi nodes
//
// This handles for, for-range, and while-style loops.
func (a *Analyzer) DetectLoops(fn *ssa.Function) *LoopInfo {
	loopBlocks := make(map[*ssa.BasicBlock]bool)
	loopHeaders := make(map[*ssa.BasicBlock]bool)
	if fn.Blocks == nil {
		return &LoopInfo{
			loopBlocks:  loopBlocks,
			loopHeaders: loopHeaders,
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
			if blockIndex[succ] <= blockIndex[block] {
				// Potential back-edge: verify it creates a cycle
				if a.CanReach(succ, block) {
					a.markLoopBlocks(fn, succ, block, loopBlocks)
					loopHeaders[succ] = true // succ is the loop header
				}
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

// IsDefinedOutsideLoop checks if a value is defined outside the loop.
//
// Used to detect the pattern:
//
//	q := db.Where("x")        // Defined OUTSIDE loop
//	for _, item := range items {
//	    q.Where(item).Find(nil)  // Used INSIDE loop - VIOLATION
//	}
//
// When a mutable root is defined outside a loop but used inside, each
// iteration reuses the same root, which is always a violation.
func (a *Analyzer) IsDefinedOutsideLoop(v ssa.Value, loopInfo *LoopInfo) bool {
	instr, ok := v.(ssa.Instruction)
	if !ok {
		// Non-instructions (parameters, constants) are "outside"
		return true
	}

	block := instr.Block()
	if block == nil {
		return true
	}

	return !loopInfo.IsInLoop(block)
}
