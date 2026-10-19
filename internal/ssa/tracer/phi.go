package tracer

import (
	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/ssa/cfg"
)

// isSwapPhiPair checks if two Phi nodes form a swap pattern.
//
// A swap pattern occurs when two Phi nodes in the same block have edges that are
// swapped versions of each other. This typically happens when variables are swapped
// in conditional code.
//
// # Pattern Detection
//
// Given two Phi nodes:
//
//	phiA = phi [pred0: x, pred1: y, ...]
//	phiB = phi [pred0: y, pred1: x, ...]
//	                ^^^       ^^^
//	                swapped!
//
// The function returns true if there exists at least one pair of edges (i, j) where:
//   - phiA.Edges[i] == phiB.Edges[j]  AND
//   - phiA.Edges[j] == phiB.Edges[i]
//
// # Example 1: Variable Swap in Loop
//
// Go code:
//
//	q1 := db.Where("q1")
//	q2 := db.Where("q2")
//	for _, item := range items {
//	    if item%2 == 0 {
//	        temp := q1
//	        q1 = q2  // Swap
//	        q2 = temp
//	    }
//	    q1 = q1.Where(...)
//	}
//
// SSA representation:
//
//	Block 1 (loop header):
//	  t5 = phi [entry: t1, back: t23] #q1
//	  t6 = phi [entry: t3, back: t17] #q2
//
//	Block 5 (after if, inside loop):
//	  t16 = phi [no-swap: t5, swap: t6] #q1
//	  t17 = phi [no-swap: t6, swap: t5] #q2
//	                      ^^        ^^
//	                      These are swapped!
//
// In this case:
//   - t16 has edges [t5, t6]
//   - t17 has edges [t6, t5]  ← swapped!
//   - isSwapPhiPair(t16, t17) returns true
//
// # Example 2: Conditional Swap (Non-Loop)
//
// Go code:
//
//	q1 := db.Where("q1")
//	q2 := db.Where("q2")
//	if swap {
//	    q1, q2 = q2, q1  // Modern swap syntax
//	}
//	q1.Find(nil)
//
// SSA representation:
//
//	Block 0 (entry):
//	  t1 = db.Where("q1")
//	  t3 = db.Where("q2")
//
//	Block 2 (after if):
//	  t7 = phi [no-swap: t1, swap: t3] #q1
//	  t8 = phi [no-swap: t3, swap: t1] #q2
//	                      ^^        ^^
//	                      Also swapped!
//
// In this case:
//   - t7 has edges [t1, t3]
//   - t8 has edges [t3, t1]  ← swapped!
//   - isSwapPhiPair(t7, t8) returns true
//
// # Why This Matters
//
// Detecting swap patterns is crucial for correct pollution tracking:
//   - In loops with only assignments: swapped variables remain independent
//   - In conditionals with pre-pollution: swap propagates pollution to both
//
// See collectInitialEdgeRoots() for how this is used in pollution detection.
func isSwapPhiPair(phiA, phiB *ssa.Phi) bool {
	if len(phiA.Edges) != len(phiB.Edges) {
		return false
	}

	// Check if there's at least one pair of swapped edges
	for i := range phiA.Edges {
		edgeA := phiA.Edges[i]
		edgeB := phiB.Edges[i]

		// Look for the swapped pattern in other edges
		for j := range phiA.Edges {
			if i == j {
				continue
			}
			otherA := phiA.Edges[j]
			otherB := phiB.Edges[j]

			// Check if (edgeA, edgeB) == (otherB, otherA) - swap pattern
			if edgeA == otherB && edgeB == otherA {
				return true
			}
		}
	}
	return false
}

// findSwapPhiSibling finds a swap-pair sibling of phi in the same block.
//
// When variables are swapped (e.g., q1, q2 = q2, q1), SSA creates two Phi nodes
// in the same block with swapped edges. This function searches for that sibling.
//
// # Search Process
//
// Given a Phi node, this function:
//  1. Iterates through all instructions in the same basic block
//  2. For each Phi instruction found:
//     - Skip if it's the same phi (self)
//     - Check if it forms a swap-pair using isSwapPhiPair()
//  3. Returns the first matching sibling, or nil if none found
//
// # Example: Loop Variable Swap
//
// SSA block structure:
//
//	Block 5 (if.done - after conditional swap):
//	  t16 = phi [no-swap: t5, swap: t6] #q1  ← Input phi
//	  t17 = phi [no-swap: t6, swap: t5] #q2  ← Sibling found!
//	  t18 = phi [...]                        ← Not a swap-pair, skipped
//	  ...
//
// Given t16 as input:
//   - findSwapPhiSibling(t16) returns t17
//   - findSwapPhiSibling(t17) returns t16 (symmetric)
//
// # Why Search in Same Block?
//
// Swap Phi nodes ALWAYS appear in the same basic block because:
//   - They represent the merge point after a conditional branch
//   - Both variables are updated at the exact same control flow point
//   - SSA ensures all Phi nodes for a block are at the block's start
//
// # Usage Pattern
//
// This is used as a quick check to detect swap patterns:
//
//	if findSwapPhiSibling(phi) != nil {
//	    // This phi is part of a swap - apply special handling
//	    ...
//	}
//
// See tracePhi(), traceAll(), and traceAllPointerLoads() for actual usage.
func findSwapPhiSibling(phi *ssa.Phi) *ssa.Phi {
	block := phi.Block()
	for _, instr := range block.Instrs {
		if otherPhi, ok := instr.(*ssa.Phi); ok && otherPhi != phi && isSwapPhiPair(phi, otherPhi) {
			return otherPhi
		}
	}
	return nil
}

// loopSwapPhis returns the loop-header Phis that phi swaps when it is part of
// a loop variable swap pattern, or nil otherwise. It combines the swap
// detection checks.
//
//declscope:shared // root.go's tracePhi, traceAll and traceAllPointerLoads detect loop swaps with it
func loopSwapPhis(phi *ssa.Phi, loopInfo *cfg.LoopInfo) []*ssa.Phi {
	if loopInfo == nil {
		return nil
	}
	// Don't apply to loop-header phis themselves
	if loopInfo.IsLoopHeader(phi.Block()) {
		return nil
	}
	// Must have a swap sibling
	if findSwapPhiSibling(phi) == nil {
		return nil
	}
	// Edges must all be loop-header phis
	return getLoopHeaderPhiEdges(phi, loopInfo)
}

// getLoopHeaderPhiEdges checks if all edges of a Phi are loop-header Phis.
// Returns the list of loop-header Phis if all edges qualify, nil otherwise.
func getLoopHeaderPhiEdges(phi *ssa.Phi, loopInfo *cfg.LoopInfo) []*ssa.Phi {
	if loopInfo == nil {
		return nil
	}

	var loopHeaderPhis []*ssa.Phi
	for _, edge := range phi.Edges {
		edgePhi, ok := edge.(*ssa.Phi)
		if !ok || !loopInfo.IsLoopHeader(edgePhi.Block()) {
			return nil
		}
		loopHeaderPhis = append(loopHeaderPhis, edgePhi)
	}
	return loopHeaderPhis
}

// isNilPhiEdge checks if a Phi edge is a nil constant.
//
//declscope:shared // root.go's Phi tracing skips nil edges with it
func isNilPhiEdge(v ssa.Value) bool {
	c, ok := v.(*ssa.Const)
	return ok && c.Value == nil
}
