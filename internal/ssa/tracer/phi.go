package tracer

import (
	"slices"

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
// See collectInitialPhiEdgeRoots() for how this is used in pollution detection.
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
func isNilPhiEdge(v ssa.Value) bool {
	c, ok := v.(*ssa.Const)
	return ok && c.Value == nil
}

// tracePhi returns the first mutable root of phi's edges, trying a loop
// header's back edges first, or phi itself for a loop variable swap.
//
//declscope:shared // root.go's traceNonCall sends Phis here
func (t *RootTracer) tracePhi(phi *ssa.Phi, visited map[ssa.Value]bool, loopInfo *cfg.LoopInfo) ssa.Value {
	// For loop-header Phi nodes, prioritize back-edges over initial edges
	// This ensures we return the loop-internal assignment result, not the pre-loop initial value
	if loopInfo != nil && loopInfo.IsLoopHeader(phi.Block()) {
		phiBlock := phi.Block()
		phiBlockIndex := slices.Index(phi.Parent().Blocks, phiBlock)

		// First pass: try back-edges (predecessors with higher indices)
		if phiBlockIndex >= 0 {
			for i, edge := range phi.Edges {
				if isNilPhiEdge(edge) || visited[edge] {
					continue
				}
				// Check if this is a back-edge (predecessor comes after phi block)
				predIndex := slices.Index(phi.Parent().Blocks, phiBlock.Preds[i])
				// Back-edge: predecessor comes after phi block (loops back)
				if predIndex > phiBlockIndex {
					if root := t.trace(edge, visited, loopInfo); root != nil {
						return root
					}
				}
			}

			// Second pass: try forward-edges if no back-edge root found
			for _, edge := range phi.Edges {
				if isNilPhiEdge(edge) || visited[edge] {
					continue
				}
				if root := t.trace(edge, visited, loopInfo); root != nil {
					return root
				}
			}
		}
		return nil
	}

	// =========================================================================
	// NON-LOOP-HEADER PHI: Conditional Merge Patterns
	// =========================================================================
	//
	// Non-loop-header Phis represent merges in conditional code (if/else).
	// We need to handle several patterns correctly:
	//
	// ## Pattern 1: Loop Variable Swap (Special Case)
	//
	// When variables are swapped inside a loop:
	//
	//   for _, item := range items {
	//       if item%2 == 0 {
	//           q1, q2 = q2, q1  // ← Creates swap-phi
	//       }
	//       q1 = q1.Where(...)    // ← Uses merged value
	//   }
	//
	// SSA:
	//   Block 1 (loop header):
	//     t5 = phi [entry, back] #q1
	//     t6 = phi [entry, back] #q2
	//   Block 5 (after if):
	//     t16 = phi [no-swap: t5, swap: t6]  ← Swap-phi
	//     t17 = phi [no-swap: t6, swap: t5]  ← Swap-phi sibling
	//
	// For loop variable swaps, we treat the swap-phi itself as the root,
	// keeping q1 and q2 independent during loop iteration.
	//
	// ## Pattern 2: Reassignment to Same Symbol in If (Regular Phi)
	//
	// When a variable is reassigned in only one branch:
	//
	//   q := db.Where("base")
	//   if cond {
	//       q = q.Where("extra")  // ← Reassignment
	//   }
	//   q.Find(nil)  // ← Uses merged value
	//
	// SSA:
	//   Block 0:
	//     t1 = db.Where("base")
	//   Block 1 (if.then):
	//     t2 = t1.Where("extra")
	//   Block 2 (if.done):
	//     t3 = phi [no-if: t1, if: t2] #q
	//                      ^^      ^^
	//                      Different values, but same LINEAGE
	//
	// This is NOT a swap (no sibling), just a regular conditional update.
	// We trace through the phi normally to find the root (t1 or t2's root).
	//
	// ## Pattern 3: Different Symbols Merged (Regular Phi)
	//
	// When different variables are assigned in different branches:
	//
	//   var q *gorm.DB
	//   if cond {
	//       q = db.Where("a")  // ← First definition
	//   } else {
	//       q = db.Where("b")  // ← Second definition
	//   }
	//   q.Find(nil)  // ← Uses merged value
	//
	// SSA:
	//   Block 2 (if.done):
	//     t3 = phi [if: t1, else: t2] #q
	//                   ^^       ^^
	//                   Different roots, need to check ALL
	//
	// This is also NOT a swap, we trace through normally and FindAllMutableRoots
	// will collect all possible roots (t1 and t2) for pollution checking.
	//
	// ## Detection Logic
	//
	// We only apply special swap-phi handling if ALL conditions are met:
	//   1. loopInfo != nil (we're inside a loop context)
	//   2. findSwapPhiSibling(phi) != nil (this phi has a swap sibling)
	//   3. getLoopHeaderPhiEdges(phi) != nil (edges are loop-header phis)
	//
	// If any condition fails, we fall through to regular tracing.
	//
	if loopSwapPhis(phi, loopInfo) != nil {
		// Loop variable swap detected - return phi as independent root
		// This prevents cross-contamination between swapped loop variables
		return phi
	}

	// =========================================================================
	// REGULAR TRACING: Standard Phi Handling
	// =========================================================================
	//
	// For all other cases (non-swap phis, or swaps without loop context):
	//   - Trace through all edges to find the first mutable root
	//   - Skip nil constants and already-visited values (cycle detection)
	//   - Return the first root found, or nil if no root exists
	//
	// This handles:
	//   - Simple reassignment in if: q = q.Where("x") in one branch
	//   - Different values merged: if { q = a } else { q = b }
	//   - Conditional swaps outside loops
	//
	for _, edge := range phi.Edges {
		if isNilPhiEdge(edge) || visited[edge] {
			continue
		}
		if root := t.trace(edge, visited, loopInfo); root != nil {
			return root
		}
	}
	return nil
}

// collectInitialPhiEdgeRoots collects roots from INITIAL edges (entry edges) of
// loop-header Phis, used to detect pre-loop pollution that can propagate through swaps.
//
// # Why Only Initial Edges?
//
// Loop-header Phis have two types of edges:
//  1. Initial edge (index 0): Value entering the loop from outside
//  2. Back-edge (index 1+): Value from loop body (loop iteration update)
//
// For loop variable swaps, we only care about pollution from BEFORE the loop starts:
//   - If variables are clean entering the loop → they stay independent during iteration
//   - If one is polluted before loop → swap propagates pollution to both
//
// Back-edges represent loop-internal assignments (e.g., q1 = q1.Where(...)) which
// don't pollute the variables themselves - they're just updates.
//
// # The Two Scenarios
//
// ## Scenario 1: Clean Variables Before Loop (No Violation)
//
// Go code:
//
//	q1 := db.Where("q1")  // Clean
//	q2 := db.Where("q2")  // Clean
//	for _, item := range items {
//	    if item%2 == 0 {
//	        q1, q2 = q2, q1  // Swap
//	    }
//	    q1 = q1.Where(...)   // Assignment (not polluting)
//	}
//	q1.Find(nil)  // First actual use - OK
//	q2.Find(nil)  // First actual use - OK
//
// SSA trace for q2.Find():
//
//	Block 1 (loop header):
//	  t5 = phi [entry: t1, back: t23] #q1
//	            initial ^^       ^^^ back-edge (assignment result)
//	  t6 = phi [entry: t3, back: t17] #q2
//	            initial ^^       ^^^ back-edge (swap result)
//
//	Block 5 (swap merge):
//	  t16 = phi [t5, t6]  ← Swap-phi for q1
//	  t17 = phi [t6, t5]  ← Swap-phi for q2
//
// For q2.Find():
//   - FindMutableRoot(t6) → t17 (swap-phi, treated as independent)
//   - FindAllMutableRoots(t6) calls this function:
//     → collectInitialPhiEdgeRoots([t5, t6], t17, ...)
//     → Traces t5's initial edge: t1 (clean, no pollution)
//     → Traces t6's initial edge: t3 (clean, no pollution)
//     → Returns [t1, t3, t17]
//   - Check pollution: t1 (clean), t3 (clean), t17 (clean)
//   - Result: No violation ✓
//
// ## Scenario 2: Pre-Polluted Variable (Violation Detected)
//
// Go code:
//
//	q1 := db.Where("q1")
//	q2 := db.Where("q2")
//	q1.Find(nil)  // ← Pollute q1 BEFORE loop
//	for _, item := range items {
//	    if item%2 == 0 {
//	        q1, q2 = q2, q1  // Swap propagates pollution!
//	    }
//	    q1 = q1.Where(...)  // ← Uses potentially polluted value
//	}
//
// SSA trace for q1.Where() inside loop:
//
//	Block 0:
//	  t1 = db.Where("q1")
//	  t3 = db.Where("q2")
//	  t1.Find(nil)  ← t1 is now polluted
//
//	Block 1 (loop header):
//	  t5 = phi [entry: t1, back: t23]  ← t1 is polluted!
//	            initial ^^
//	  t6 = phi [entry: t3, back: t17]
//	            initial ^^
//
//	Block 5 (swap merge):
//	  t16 = phi [t5, t6]  ← May contain t1 (polluted) or t6 (clean)
//	  t17 = phi [t6, t5]  ← May contain t6 (clean) or t5 (polluted)
//
// For t16.Where() (the assignment in loop):
//   - FindMutableRoot(t16) → t16 (swap-phi)
//   - FindAllMutableRoots(t16) calls this function:
//     → collectInitialPhiEdgeRoots([t5, t6], t16, ...)
//     → Traces t5's initial edge: t1 (POLLUTED! ✗)
//     → Traces t6's initial edge: t3 (clean)
//     → Returns [t1, t3, t16]
//   - Check pollution: t1 is polluted!
//   - Result: Violation detected ✓
//
// # Implementation Details
//
// For each loop-header Phi:
//  1. Get the first edge (index 0) - this is the initial/entry edge
//  2. Skip if nil constant or already visited (cycle detection)
//  3. Recursively trace using traceAll() to find all possible roots
//  4. Collect all roots from all loop-header Phis
//  5. Add the swap-phi itself as a root (for independent tracking)
//
// # Return Value
//
// Returns a list of roots that includes:
//   - All roots from initial edges of all loop-header Phis
//   - The swap-phi itself (for treating it as an independent root in some contexts)
//
// This allows pollution detection to:
//   - Check if ANY initial value was polluted before the loop
//   - Treat the swap-phi as a separate entity for FindMutableRoot()
//
//declscope:shared // root.go's traceAll and traceAllPointerLoads send loop variable swaps here
func (t *RootTracer) collectInitialPhiEdgeRoots(loopHeaderPhis []*ssa.Phi, swapPhi ssa.Value, visited map[ssa.Value]bool, loopInfo *cfg.LoopInfo) []ssa.Value {
	var roots []ssa.Value
	for _, loopPhi := range loopHeaderPhis {
		// Get the initial edge (first edge, not back-edge)
		if len(loopPhi.Edges) > 0 {
			initialEdge := loopPhi.Edges[0]
			if !isNilPhiEdge(initialEdge) && !visited[initialEdge] {
				roots = append(roots, t.traceAll(initialEdge, visited, loopInfo)...)
			}
		}
	}
	// Also include the swap-phi itself as a root
	roots = append(roots, swapPhi)
	return roots
}

// traceAllPhiEdges collects all mutable roots reachable through a Phi node's
// incoming edges, skipping nil constants and already-visited values. Shared by
// the Phi cases of traceAll and traceAllPointerLoads.
//
//declscope:shared // root.go's traceAll and traceAllPointerLoads send the other Phis here
func (t *RootTracer) traceAllPhiEdges(phi *ssa.Phi, visited map[ssa.Value]bool, loopInfo *cfg.LoopInfo) []ssa.Value {
	var roots []ssa.Value
	for _, edge := range phi.Edges {
		if isNilPhiEdge(edge) || visited[edge] {
			continue
		}
		roots = append(roots, t.traceAll(edge, visited, loopInfo)...)
	}
	return roots
}
