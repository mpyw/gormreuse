// Package tracer provides SSA value tracing for gormreuse.
//
// # Overview
//
// This package traces SSA values backward through the SSA graph to find the
// "mutable root" - the origin of a *gorm.DB chain that determines pollution state.
//
// # Tracing Model
//
// The tracer follows SSA values backward through:
//
//	┌─────────────────────────────────────────────────────────────────────────┐
//	│                         Tracing Directions                               │
//	│                                                                          │
//	│   Source (origin)                              Target (usage site)       │
//	│      │                                              │                    │
//	│      │  ┌────────────┐   ┌────────────┐            │                    │
//	│      └──│  gorm.DB   │──▶│  .Where()  │──▶ ... ────┘                    │
//	│         │  method    │   │  method    │                                 │
//	│         │  call      │   │  call      │                                 │
//	│         └────────────┘   └────────────┘                                 │
//	│              ▲                                                          │
//	│              │                                                          │
//	│         "Mutable Root"                                                  │
//	│         (This call result is the pollution tracking point)              │
//	└─────────────────────────────────────────────────────────────────────────┘
//
// # Key Semantic: "Variable Assignment Creates New Root"
//
// When a gorm chain result is stored in a variable, that variable becomes a
// NEW mutable root. This prevents pollution from propagating through assignments:
//
//	base := db.Where("x")     // base is mutable root #1
//	base.Find(nil)            // marks base (root #1) polluted
//	q := base.Where("y")      // VIOLATION (base polluted) AND q becomes root #2
//	q.Count(nil)              // OK (q is fresh root #2, not polluted)
//
// # Tracing Through SSA Constructs
//
//	┌───────────────────────────────────────────────────────────────────────┐
//	│  SSA Construct          │  Tracing Behavior                          │
//	├───────────────────────────────────────────────────────────────────────┤
//	│  *ssa.Call (gorm)       │  STOP - call result is the mutable root    │
//	│  *ssa.Call (IIFE)       │  Trace through closure returns             │
//	│  *ssa.Phi               │  Trace all edges (conditional merge)       │
//	│  *ssa.UnOp (deref)      │  Trace the pointer being dereferenced      │
//	│  *ssa.Alloc             │  Find Store instructions to this alloc     │
//	│  *ssa.FreeVar           │  Find binding in parent's MakeClosure      │
//	│  *ssa.FieldAddr         │  Find Store to this field                  │
//	│  *ssa.Parameter         │  STOP - parameter is immutable source      │
//	│  *ssa.Const (nil)       │  STOP - nil is ignored                     │
//	│  Builtin pure call      │  STOP - returns immutable (nil root)       │
//	│  User-defined pure call │  Returns call as root (may be mutable)     │
//	└───────────────────────────────────────────────────────────────────────┘
package tracer

import (
	"go/token"
	"go/types"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/directive"
	"github.com/mpyw/gormreuse/internal/ssa/cfg"
	"github.com/mpyw/gormreuse/internal/typeutil"
)

// RootTracer traces SSA values to find mutable *gorm.DB roots.
//
// # Core Responsibility
//
// Given a receiver value of a gorm method call, trace backward to find
// the "mutable root" that pollution tracking should use.
//
// # Variable Assignment Semantics
//
// Variable assignment (Store to Alloc) creates a NEW mutable root,
// which prevents violations from propagating through assigned variables:
//
//	base := db.Where("x")     // base is mutable root
//	base.Find(nil)            // marks base polluted
//	q := base.Where("y")      // VIOLATION! AND q becomes NEW root
//	q.Count(nil)              // OK (q is fresh, not polluted)
//
// # Immutable Sources
//
// The following are treated as immutable sources (return nil root):
//   - Function parameters: `func foo(db *gorm.DB)` - db is immutable
//   - Builtin pure function results: `db.Session(...)` returns immutable
//   - Const nil values
//
// Note: User-defined pure functions (//gormreuse:pure) are NOT immutable sources.
// They may return mutable values - only builtin pure methods guarantee immutable returns.
type RootTracer struct {
	pureFuncs            *directive.DirectiveFuncSet // User-defined pure functions
	immutableReturnFuncs *directive.DirectiveFuncSet // Functions returning immutable *gorm.DB
}

// New creates a new RootTracer.
func New(pureFuncs, immutableReturnFuncs *directive.DirectiveFuncSet) *RootTracer {
	return &RootTracer{
		pureFuncs:            pureFuncs,
		immutableReturnFuncs: immutableReturnFuncs,
	}
}

// FindMutableRoot finds the mutable root for a receiver value.
//
// Returns nil if the value traces back to an immutable source (parameter,
// pure function result, or nil constant).
//
// # Key Semantic
//
// We stop at gorm call results, NOT at variable origins. This means each
// gorm call result that is "directly used" is its own root:
//
//	a := db.Where("x")        // Trace stops here: a's root is this call
//	b := a.Where("y")         // Trace stops here: b's root is this call
//	                          // (not traced back to a's call)
//
// # Example Trace
//
//	q := db.Where("x")        // q is root
//	q.Find(nil)               // receiver=q → trace → root=q's defining call
func (t *RootTracer) FindMutableRoot(recv ssa.Value, loopInfo *cfg.LoopInfo) ssa.Value {
	return t.trace(recv, make(map[ssa.Value]bool), loopInfo)
}

// FindAllMutableRoots finds ALL possible mutable roots (for Phi nodes).
//
// Unlike FindMutableRoot which returns the first root found, this function
// collects all possible roots from all Phi edges. This is needed for
// conditional code where different branches may have different roots:
//
//	var q *gorm.DB
//	if cond {
//	    q = db.Where("a")     // root #1
//	} else {
//	    q = db.Where("b")     // root #2
//	}
//	q.Find(nil)               // Phi node: needs to check BOTH roots
func (t *RootTracer) FindAllMutableRoots(v ssa.Value, loopInfo *cfg.LoopInfo) []ssa.Value {
	return t.traceAll(v, make(map[ssa.Value]bool), loopInfo)
}

// IsPureFunction checks if a function is marked as pure (doesn't pollute arguments).
//
// A function is pure if:
//   - It's a builtin pure method (Session, WithContext, Debug, Open, Begin, Transaction)
//   - It's marked with //gormreuse:pure directive
//
// Pure functions don't pollute their *gorm.DB arguments.
// Note: User-defined pure functions may return mutable values - only builtin pure
// methods are guaranteed to return immutable values.
func (t *RootTracer) IsPureFunction(fn *ssa.Function) bool {
	if fn == nil {
		return false
	}
	return typeutil.IsImmutableReturningBuiltin(fn.Name()) || t.pureFuncs.Contains(fn)
}

// IsImmutableReturningBuiltin checks if a function is a builtin method that returns immutable *gorm.DB.
// Builtin methods (Session, WithContext, Debug, etc.) return immutable *gorm.DB.
// This is used for tracing - only builtin methods have immutable return values.
func (t *RootTracer) IsImmutableReturningBuiltin(fn *ssa.Function) bool {
	if fn == nil {
		return false
	}
	return typeutil.IsImmutableReturningBuiltin(fn.Name())
}

// trace is the core tracing function that finds the mutable root for a value.
//
// Tracing flow:
//
//	┌──────────────┐
//	│  Input value │
//	└──────┬───────┘
//	       │
//	       ▼
//	┌──────────────────┐     yes    ┌─────────────────┐
//	│ Already visited? │───────────▶│ Return nil      │
//	└──────┬───────────┘            │ (cycle detected)│
//	       │ no                     └─────────────────┘
//	       ▼
//	┌──────────────────┐     yes    ┌─────────────────┐
//	│ Immutable source?│───────────▶│ Return nil      │
//	│ (param/const/    │            │ (no mutable     │
//	│  pure call)      │            │  root exists)   │
//	└──────┬───────────┘            └─────────────────┘
//	       │ no
//	       ▼
//	┌──────────────────┐     yes    ┌─────────────────┐
//	│   *ssa.Call?     │───────────▶│ traceCall()     │
//	└──────┬───────────┘            └─────────────────┘
//	       │ no
//	       ▼
//	┌──────────────────┐
//	│  traceNonCall()  │
//	│ (Phi/UnOp/etc.)  │
//	└──────────────────┘
func (t *RootTracer) trace(v ssa.Value, visited map[ssa.Value]bool, loopInfo *cfg.LoopInfo) ssa.Value {
	if v == nil || visited[v] {
		return nil
	}
	visited[v] = true

	// Check if this is an immutable source
	if t.isImmutableSource(v) {
		return nil
	}

	// Handle gorm method calls
	if call, ok := v.(*ssa.Call); ok {
		return t.traceCall(call, visited, loopInfo)
	}

	// Handle non-call values
	return t.traceNonCall(v, visited, loopInfo)
}

// traceCall handles *ssa.Call values during tracing.
//
// Three cases:
//  1. IIFE (Immediately Invoked Function Expression): trace through returns
//  2. Gorm method call: STOP - this call is the mutable root
//  3. Non-gorm function returning *gorm.DB: treat as root if not pure
//
// Example IIFE tracing:
//
//	_ = func() *gorm.DB { return q.Where("y") }().Find(nil)
//	      │                        │
//	      │                        └─── Inner call is traced
//	      └─── Outer IIFE call triggers traceIIFEReturns
func (t *RootTracer) traceCall(call *ssa.Call, visited map[ssa.Value]bool, loopInfo *cfg.LoopInfo) ssa.Value {
	// Handle closures (IIFE)
	if mc, ok := call.Call.Value.(*ssa.MakeClosure); ok {
		if closureFn, ok := mc.Fn.(*ssa.Function); ok {
			if root := t.traceIIFEReturns(closureFn, visited, loopInfo); root != nil {
				// If closure result is stored in a variable (Extract instruction),
				// treat each call as independent root.
				// Only trace through IIFE when result is directly chained.
				if isClosureResultStored(call) {
					return call
				}
				return root
			}
		}
	}

	callee := call.Call.StaticCallee()
	if callee == nil {
		return nil
	}

	sig := callee.Signature
	if sig == nil || sig.Recv() == nil || !typeutil.IsGormDB(sig.Recv().Type()) {
		// Not a gorm method - if it returns *gorm.DB, treat as root
		if typeutil.IsGormDB(call.Type()) {
			// Builtin pure function returns immutable
			if t.IsImmutableReturningBuiltin(callee) {
				return nil
			}
			// User-defined immutable-return function returns immutable
			if t.immutableReturnFuncs != nil && t.immutableReturnFuncs.Contains(callee) {
				return nil
			}
			// User-defined pure or non-pure function: treat call as mutable root
			// (user-defined pure functions may return mutable values)
			return call
		}
		return nil
	}

	// This is a gorm method call - THIS CALL is the mutable root
	// Don't trace further - each gorm call result is its own root
	// This implements "variable assignment creates new root" semantics
	return call
}

// traceNonCall handles non-call SSA values during tracing.
// Routes to specialized handlers based on the value type.
func (t *RootTracer) traceNonCall(v ssa.Value, visited map[ssa.Value]bool, loopInfo *cfg.LoopInfo) ssa.Value {
	switch val := v.(type) {
	case *ssa.Phi:
		// Phi: merge point from conditional branches (if/switch)
		return t.tracePhi(val, visited, loopInfo)

	case *ssa.UnOp:
		// UnOp: unary operations including pointer dereference (*ptr)
		return t.traceUnOp(val, visited, loopInfo)

	case *ssa.ChangeType:
		// ChangeType: type conversion (same underlying type)
		return t.trace(val.X, visited, loopInfo)

	case *ssa.Extract:
		// Extract: extract element from tuple (multi-return)
		return t.trace(val.Tuple, visited, loopInfo)

	case *ssa.FreeVar:
		// FreeVar: captured variable in a closure
		return t.traceFreeVar(val, visited, loopInfo)

	case *ssa.Alloc:
		// Alloc: local variable allocation
		return t.traceAlloc(val, visited, loopInfo)

	default:
		return nil
	}
}

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
	for i := 0; i < len(phiA.Edges); i++ {
		edgeA := phiA.Edges[i]
		edgeB := phiB.Edges[i]

		// Look for the swapped pattern in other edges
		for j := 0; j < len(phiA.Edges); j++ {
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
		if otherPhi, ok := instr.(*ssa.Phi); ok {
			if otherPhi != phi && isSwapPhiPair(phi, otherPhi) {
				return otherPhi
			}
		}
	}
	return nil
}

// isLoopVariableSwap checks if a Phi node is part of a loop variable swap pattern.
// Returns the loop-header Phis if this is a swap, nil otherwise.
// This is a convenience function that combines swap detection checks.
func isLoopVariableSwap(phi *ssa.Phi, loopInfo *cfg.LoopInfo) []*ssa.Phi {
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

// collectInitialEdgeRoots collects roots from INITIAL edges (entry edges) of
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
//     → collectInitialEdgeRoots([t5, t6], t17, ...)
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
//     → collectInitialEdgeRoots([t5, t6], t16, ...)
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
func (t *RootTracer) collectInitialEdgeRoots(loopHeaderPhis []*ssa.Phi, swapPhi ssa.Value, visited map[ssa.Value]bool, loopInfo *cfg.LoopInfo) []ssa.Value {
	var roots []ssa.Value
	for _, loopPhi := range loopHeaderPhis {
		// Get the initial edge (first edge, not back-edge)
		if len(loopPhi.Edges) > 0 {
			initialEdge := loopPhi.Edges[0]
			if !isNilConst(initialEdge) && !visited[initialEdge] {
				roots = append(roots, t.traceAll(initialEdge, visited, loopInfo)...)
			}
		}
	}
	// Also include the swap-phi itself as a root
	roots = append(roots, swapPhi)
	return roots
}

func (t *RootTracer) tracePhi(phi *ssa.Phi, visited map[ssa.Value]bool, loopInfo *cfg.LoopInfo) ssa.Value {
	// For loop-header Phi nodes, prioritize back-edges over initial edges
	// This ensures we return the loop-internal assignment result, not the pre-loop initial value
	if loopInfo != nil && loopInfo.IsLoopHeader(phi.Block()) {
		phiBlock := phi.Block()
		phiBlockIndex := -1
		for i, block := range phi.Parent().Blocks {
			if block == phiBlock {
				phiBlockIndex = i
				break
			}
		}

		// First pass: try back-edges (predecessors with higher indices)
		if phiBlockIndex >= 0 {
			for i, edge := range phi.Edges {
				if isNilConst(edge) || visited[edge] {
					continue
				}
				// Check if this is a back-edge (predecessor comes after phi block)
				pred := phiBlock.Preds[i]
				predIndex := -1
				for j, block := range phi.Parent().Blocks {
					if block == pred {
						predIndex = j
						break
					}
				}
				// Back-edge: predecessor comes after phi block (loops back)
				if predIndex > phiBlockIndex {
					if root := t.trace(edge, visited, loopInfo); root != nil {
						return root
					}
				}
			}

			// Second pass: try forward-edges if no back-edge root found
			for _, edge := range phi.Edges {
				if isNilConst(edge) || visited[edge] {
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
	if isLoopVariableSwap(phi, loopInfo) != nil {
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
		if isNilConst(edge) || visited[edge] {
			continue
		}
		if root := t.trace(edge, visited, loopInfo); root != nil {
			return root
		}
	}
	return nil
}

func (t *RootTracer) traceUnOp(unop *ssa.UnOp, visited map[ssa.Value]bool, loopInfo *cfg.LoopInfo) ssa.Value {
	if unop.Op == token.MUL {
		// Pointer dereference - trace through the pointer
		return t.tracePointerLoad(unop.X, visited, loopInfo)
	}
	return t.trace(unop.X, visited, loopInfo)
}

func (t *RootTracer) tracePointerLoad(ptr ssa.Value, visited map[ssa.Value]bool, loopInfo *cfg.LoopInfo) ssa.Value {
	switch p := ptr.(type) {
	case *ssa.FreeVar:
		return t.traceFreeVar(p, visited, loopInfo)
	case *ssa.Alloc:
		return t.traceAlloc(p, visited, loopInfo)
	case *ssa.FieldAddr:
		return t.traceFieldStore(p, visited, loopInfo)
	default:
		return t.trace(ptr, visited, loopInfo)
	}
}

// traceFreeVar traces a captured variable in a closure back to its binding.
//
// When a closure captures a variable, SSA represents it as:
//
//	Parent function:
//	    t1 = MakeClosure fn [q]    // q is bound to the closure
//	                      ↑
//	                      └─── Bindings[0] = q
//
//	Closure function (fn):
//	    t2 = FreeVar [0]           // References Bindings[0] from parent
//
// This function finds the MakeClosure in the parent and traces the binding.
func (t *RootTracer) traceFreeVar(fv *ssa.FreeVar, visited map[ssa.Value]bool, loopInfo *cfg.LoopInfo) ssa.Value {
	fn := fv.Parent()
	if fn == nil {
		return nil
	}

	// Find the index of this FreeVar in the function's FreeVars list
	idx := -1
	for i, v := range fn.FreeVars {
		if v == fv {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil
	}

	// Find the MakeClosure instruction in the parent function
	parent := fn.Parent()
	if parent == nil {
		return nil
	}

	for _, block := range parent.Blocks {
		for _, instr := range block.Instrs {
			mc, ok := instr.(*ssa.MakeClosure)
			if !ok {
				continue
			}
			closureFn, ok := mc.Fn.(*ssa.Function)
			if !ok || closureFn != fn {
				continue
			}
			// Found the MakeClosure - trace the corresponding binding
			if idx < len(mc.Bindings) {
				return t.trace(mc.Bindings[idx], visited, loopInfo)
			}
		}
	}
	return nil
}

// traceAlloc traces a local variable (Alloc) by finding Store instructions.
//
// In SSA, local variables are represented as:
//
//	t1 = Alloc *gorm.DB (q)     // Allocate space for variable q
//	Store t1 <value>            // Store a value into q
//	t2 = UnOp * t1              // Load value from q (dereference)
//
// This function finds the Store instruction that writes to the Alloc.
func (t *RootTracer) traceAlloc(alloc *ssa.Alloc, visited map[ssa.Value]bool, loopInfo *cfg.LoopInfo) ssa.Value {
	fn := alloc.Parent()
	if fn == nil {
		return nil
	}

	// Find Store instructions that write to this Alloc
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			store, ok := instr.(*ssa.Store)
			if !ok || store.Addr != alloc {
				continue
			}
			return t.trace(store.Val, visited, loopInfo)
		}
	}
	return nil
}

// traceFieldStore traces a struct field access by finding Store instructions.
//
// When a struct field is accessed:
//
//	type Handler struct { db *gorm.DB }
//	h.db.Find(nil)
//
// SSA represents this as:
//
//	t1 = FieldAddr h.db        // Get address of field
//	t2 = UnOp * t1             // Dereference to get value
//	                           // (traced here to find the Store)
//
// This function finds the Store that wrote to the same field.
func (t *RootTracer) traceFieldStore(fa *ssa.FieldAddr, visited map[ssa.Value]bool, loopInfo *cfg.LoopInfo) ssa.Value {
	fn := fa.Parent()
	if fn == nil {
		return nil
	}

	// Find Store instructions that write to the same field
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			store, ok := instr.(*ssa.Store)
			if !ok {
				continue
			}
			storeFA, ok := store.Addr.(*ssa.FieldAddr)
			if !ok || storeFA.X != fa.X || storeFA.Field != fa.Field {
				continue
			}
			return t.trace(store.Val, visited, loopInfo)
		}
	}
	return nil
}

// traceIIFEReturns traces through an immediately invoked function expression.
//
// IIFE pattern:
//
//	_ = func() *gorm.DB {
//	    return q.Where("y")   // ← trace this return value
//	}().Find(nil)
//
// This allows treating the entire IIFE chain as a single branch from q,
// rather than two separate branches (IIFE internal + Find external).
//
// We clone the visited map to avoid polluting the parent's tracking state.
func (t *RootTracer) traceIIFEReturns(fn *ssa.Function, visited map[ssa.Value]bool, loopInfo *cfg.LoopInfo) ssa.Value {
	if fn.Signature == nil {
		return nil
	}
	results := fn.Signature.Results()
	if results == nil || results.Len() == 0 || !typeutil.IsGormDB(results.At(0).Type()) {
		return nil
	}

	// Collect ALL roots from all return statements
	var allRoots []ssa.Value
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			ret, ok := instr.(*ssa.Return)
			if !ok || len(ret.Results) == 0 {
				continue
			}
			// Clone visited map to isolate closure's tracing state
			retVisited := cloneVisited(visited)
			if root := t.trace(ret.Results[0], retVisited, loopInfo); root != nil {
				allRoots = append(allRoots, root)
			}
		}
	}

	if len(allRoots) == 0 {
		return nil
	}

	// If all roots are the same, return that root
	firstRoot := allRoots[0]
	allSame := true
	for _, root := range allRoots[1:] {
		if root != firstRoot {
			allSame = false
			break
		}
	}

	if allSame {
		return firstRoot
	}

	// Multiple different roots - return nil to signal caller should use call itself
	// This happens with conditional returns like:
	//   func() *gorm.DB { if cond { return q1 } else { return q2 } }()
	return nil
}

// traceAllIIFEReturns is the multi-root version of traceIIFEReturns.
// It collects roots from ALL return statements in the closure.
func (t *RootTracer) traceAllIIFEReturns(fn *ssa.Function, visited map[ssa.Value]bool, loopInfo *cfg.LoopInfo) []ssa.Value {
	if fn.Signature == nil {
		return nil
	}
	results := fn.Signature.Results()
	if results == nil || results.Len() == 0 || !typeutil.IsGormDB(results.At(0).Type()) {
		return nil
	}

	var allRoots []ssa.Value
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			ret, ok := instr.(*ssa.Return)
			if !ok || len(ret.Results) == 0 {
				continue
			}
			retVisited := cloneVisited(visited)
			roots := t.traceAll(ret.Results[0], retVisited, loopInfo)
			allRoots = append(allRoots, roots...)
		}
	}
	return allRoots
}

// traceAll finds ALL possible mutable roots, handling Phi nodes specially.
//
// Unlike trace() which returns the first root found, this collects all roots.
// This is essential for Phi nodes where different branches may have different roots:
//
//	var q *gorm.DB
//	if cond {
//	    q = db.Where("a")  // root A
//	} else {
//	    q = db.Where("b")  // root B
//	}
//	q.Find(nil)            // Phi has edges from both branches
//	                       // Need to check pollution of BOTH roots
func (t *RootTracer) traceAll(v ssa.Value, visited map[ssa.Value]bool, loopInfo *cfg.LoopInfo) []ssa.Value {
	if v == nil || visited[v] {
		return nil
	}
	visited[v] = true

	switch val := v.(type) {
	case *ssa.Phi:
		// =====================================================================
		// PHI NODE: Collect All Possible Roots for Pollution Checking
		// =====================================================================
		//
		// Unlike FindMutableRoot() which returns the PRIMARY root for a value,
		// FindAllMutableRoots() must return ALL possible roots that could
		// contribute pollution. This is critical for conditional code where
		// different branches may have different pollution states.
		//
		// ## Why We Need All Roots
		//
		// Consider:
		//   q1 := db.Where("a")
		//   q2 := db.Where("b")
		//   q1.Find(nil)  // Pollute q1
		//   var q *gorm.DB
		//   if cond {
		//       q = q1  // Branch 1: polluted
		//   } else {
		//       q = q2  // Branch 2: clean
		//   }
		//   q.Count(nil)  // ← Need to check BOTH q1 and q2
		//
		// The phi after the if has edges [q1, q2]. We must check if ANY
		// of these roots is polluted, not just pick one.
		//
		// ## Special Case: Loop Variable Swaps
		//
		// However, loop variable swaps need special handling:
		//
		//   q1 := db.Where("q1")  // Clean
		//   q2 := db.Where("q2")  // Clean
		//   for _, item := range items {
		//       if item%2 == 0 {
		//           q1, q2 = q2, q1  // Swap
		//       }
		//       q1 = q1.Where(...)  // Assignment
		//   }
		//
		// SSA creates swap-phi nodes inside the loop. These should NOT
		// mix pollution between q1 and q2 during iteration, because they're
		// just swapping roles, not creating cross-contamination.
		//
		// BUT we still need to check if q1 or q2 were polluted BEFORE
		// entering the loop, because that pollution would propagate.
		//
		// Solution: For loop variable swaps, return:
		//   - INITIAL edges of loop-header phis (pre-loop values)
		//   - The swap-phi itself (as an independent entity)
		//
		// This allows pollution checking to:
		//   - Detect pre-loop pollution: check if t1 or t3 is polluted
		//   - Treat swap-phi independently: t16 and t17 have separate pollution
		//
		if loopHeaderPhis := isLoopVariableSwap(val, loopInfo); loopHeaderPhis != nil {
			// Loop variable swap detected:
			// Return initial edges (to check pre-loop pollution) + swap-phi itself
			return t.collectInitialEdgeRoots(loopHeaderPhis, val, visited, loopInfo)
		}

		// =====================================================================
		// REGULAR PHI: Collect Roots from All Edges
		// =====================================================================
		//
		// For non-swap phis, or swaps outside loop context:
		//   - Recursively trace ALL edges
		//   - Collect roots from each edge
		//   - Return union of all roots found
		//
		// This handles:
		//   - Conditional assignment: if { q = a } else { q = b }
		//   - Reassignment in one branch: if { q = q.Where(...) }
		//   - Conditional swaps outside loops
		//
		var roots []ssa.Value
		for _, edge := range val.Edges {
			if isNilConst(edge) || visited[edge] {
				continue
			}
			roots = append(roots, t.traceAll(edge, visited, loopInfo)...)
		}
		return roots

	case *ssa.UnOp:
		if val.Op == token.MUL {
			return t.traceAllPointerLoads(val.X, visited, loopInfo)
		}
		return t.traceAll(val.X, visited, loopInfo)

	case *ssa.Alloc:
		return t.traceAllAllocStores(val, visited, loopInfo)

	case *ssa.FreeVar:
		return t.traceAllFreeVar(val, visited, loopInfo)

	case *ssa.Call:
		// Handle closure calls (IIFE) - collect ALL roots from all returns
		if mc, ok := val.Call.Value.(*ssa.MakeClosure); ok {
			if closureFn, ok := mc.Fn.(*ssa.Function); ok {
				if roots := t.traceAllIIFEReturns(closureFn, visited, loopInfo); len(roots) > 0 {
					// If closure result is stored, treat call itself as root
					if isClosureResultStored(val) {
						return []ssa.Value{val}
					}
					return roots
				}
			}
		}
		// Non-closure call - treat as potential root
		if typeutil.IsGormDB(val.Type()) {
			return []ssa.Value{val}
		}
		return nil

	default:
		// For non-special cases (ChangeType, Extract, etc.), delegate to single-root trace.
		// Clone visited and remove v to allow trace() to process it.
		// This is safe because trace() doesn't call back to traceAll(), so no cycle risk.
		freshVisited := cloneVisited(visited)
		delete(freshVisited, v)
		if root := t.trace(v, freshVisited, loopInfo); root != nil {
			return []ssa.Value{root}
		}
		return nil
	}
}

func (t *RootTracer) traceAllPointerLoads(ptr ssa.Value, visited map[ssa.Value]bool, loopInfo *cfg.LoopInfo) []ssa.Value {
	switch p := ptr.(type) {
	case *ssa.Alloc:
		return t.traceAllAllocStores(p, visited, loopInfo)
	case *ssa.FieldAddr:
		return t.traceAllFieldStores(p, visited, loopInfo)
	case *ssa.Phi:
		// Check for loop variable swap pattern
		if loopHeaderPhis := isLoopVariableSwap(p, loopInfo); loopHeaderPhis != nil {
			return t.collectInitialEdgeRoots(loopHeaderPhis, p, visited, loopInfo)
		}

		// Regular Phi - traverse all edges
		var roots []ssa.Value
		for _, edge := range p.Edges {
			if isNilConst(edge) || visited[edge] {
				continue
			}
			roots = append(roots, t.traceAll(edge, visited, loopInfo)...)
		}
		return roots
	default:
		return t.traceAll(ptr, visited, loopInfo)
	}
}

func (t *RootTracer) traceAllAllocStores(alloc *ssa.Alloc, visited map[ssa.Value]bool, loopInfo *cfg.LoopInfo) []ssa.Value {
	fn := alloc.Parent()
	if fn == nil {
		return nil
	}

	var roots []ssa.Value
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			store, ok := instr.(*ssa.Store)
			if !ok || store.Addr != alloc {
				continue
			}
			roots = append(roots, t.traceAll(store.Val, visited, loopInfo)...)
		}
	}
	return roots
}

// traceAllFieldStores finds ALL possible roots from stores to a struct field.
//
// Unlike traceFieldStore which returns the first Store found, this collects
// all roots from ALL Store instructions writing to the same field. This is
// needed for conditional code where different branches may store different values:
//
//	if cond {
//	    h.db = q1  // Store #1
//	} else {
//	    h.db = q2  // Store #2
//	}
//	h.db.Find(nil)  // Need to check BOTH q1 and q2
func (t *RootTracer) traceAllFieldStores(fa *ssa.FieldAddr, visited map[ssa.Value]bool, loopInfo *cfg.LoopInfo) []ssa.Value {
	fn := fa.Parent()
	if fn == nil {
		return nil
	}

	var roots []ssa.Value
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			store, ok := instr.(*ssa.Store)
			if !ok {
				continue
			}
			storeFA, ok := store.Addr.(*ssa.FieldAddr)
			if !ok || storeFA.X != fa.X || storeFA.Field != fa.Field {
				continue
			}
			roots = append(roots, t.traceAll(store.Val, visited, loopInfo)...)
		}
	}
	return roots
}

// traceAllFreeVar finds ALL possible roots from a captured closure variable.
//
// Unlike traceFreeVar which calls single-root trace on the binding, this calls
// traceAll to get all possible roots. This is needed when the binding is a Phi
// (from conditional code):
//
//	var q *gorm.DB
//	if cond {
//	    q = q1  // Branch 1
//	} else {
//	    q = q2  // Branch 2
//	}
//	fn := func() { q.Find(nil) }  // FreeVar captures Phi of [q1, q2]
//	fn()  // Need to check BOTH q1 and q2
func (t *RootTracer) traceAllFreeVar(fv *ssa.FreeVar, visited map[ssa.Value]bool, loopInfo *cfg.LoopInfo) []ssa.Value {
	fn := fv.Parent()
	if fn == nil {
		return nil
	}

	// Find the index of this FreeVar in the function's FreeVars list
	idx := -1
	for i, v := range fn.FreeVars {
		if v == fv {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil
	}

	// Find the MakeClosure instruction in the parent function
	parent := fn.Parent()
	if parent == nil {
		return nil
	}

	for _, block := range parent.Blocks {
		for _, instr := range block.Instrs {
			mc, ok := instr.(*ssa.MakeClosure)
			if !ok {
				continue
			}
			closureFn, ok := mc.Fn.(*ssa.Function)
			if !ok || closureFn != fn {
				continue
			}
			// Found the MakeClosure - trace ALL roots from the corresponding binding
			if idx < len(mc.Bindings) {
				return t.traceAll(mc.Bindings[idx], visited, loopInfo)
			}
		}
	}
	return nil
}

// isImmutableSource checks if a value is an immutable source (no mutable root).
//
// Immutable sources:
//   - Parameter: function arguments are treated as fresh values
//   - Const: constant values (especially nil)
//   - Builtin pure function call: returns immutable *gorm.DB (e.g., Session())
//   - User-defined immutable-return function: marked with //gormreuse:immutable-return
//
// Note: User-defined pure functions (without immutable-return) are NOT immutable sources -
// they may return mutable values.
func (t *RootTracer) isImmutableSource(v ssa.Value) bool {
	switch val := v.(type) {
	case *ssa.Parameter:
		// Function parameters are immutable sources
		// (caller's responsibility to ensure safety)
		return true
	case *ssa.Const:
		// Constants (including nil) are immutable
		return true
	case *ssa.Call:
		callee := val.Call.StaticCallee()
		// Builtin pure function calls return immutable values
		if t.IsImmutableReturningBuiltin(callee) {
			return true
		}
		// User-defined immutable-return functions return immutable values
		if t.immutableReturnFuncs != nil && t.immutableReturnFuncs.Contains(callee) {
			return true
		}
		return false
	default:
		return false
	}
}

// =============================================================================
// Helper Functions
// =============================================================================

// isNilConst checks if a value is a nil constant.
func isNilConst(v ssa.Value) bool {
	c, ok := v.(*ssa.Const)
	return ok && c.Value == nil
}

// isClosureResultStored checks if a closure call's result is stored in a variable.
// This happens when the closure returns multiple values and the result is extracted.
// Example: `publishedQuery, err := closureFunc()` - result goes through Extract.
// In contrast, IIFE chains like `closure().Find()` use the result directly.
// isClosureResultStored checks if a closure call's result is stored rather than
// directly chained to a method call.
//
// Returns true (stored) for patterns like:
//   - `q, err := closureFunc()` (multi-return with Extract)
//   - `q := closureFunc()` (single-return assigned to variable)
//   - Result flows into Phi node
//
// Returns false (chained) for IIFE patterns like:
//   - `closureFunc().Find(nil)` (result directly used as method receiver)
//
// isClosureResultStored checks if a closure call's result is stored (assigned to
// a variable) rather than directly chained in an IIFE pattern.
//
// Returns true (stored) for patterns like:
//   - `q, err := closureFunc()` (flows to Extract → stored)
//   - `q := closureFunc()` (flows to Store/Alloc → stored)
//   - `if cond { q = closureFunc() }` (flows to Phi → stored)
//   - `q := closureFunc().Where("x")` (chain eventually stored → stored)
//
// Returns false (chained IIFE) for patterns like:
//   - `closureFunc().Find(nil)` (chain ends with terminal, never stored)
func isClosureResultStored(call *ssa.Call) bool {
	return isClosureResultStoredRecursive(call, make(map[*ssa.Call]bool))
}

func isClosureResultStoredRecursive(call *ssa.Call, visited map[*ssa.Call]bool) bool {
	if visited[call] {
		return false
	}
	visited[call] = true

	refs := call.Referrers()
	if refs == nil {
		return false
	}

	for _, ref := range *refs {
		switch user := ref.(type) {
		case *ssa.Extract:
			// Multi-return extraction → stored
			return true

		case *ssa.Phi:
			// Flows to Phi → stored (conditional assignment)
			return true

		case *ssa.Store:
			// Store to Alloc → stored (variable assignment)
			if _, ok := user.Addr.(*ssa.Alloc); ok {
				return true
			}

		case *ssa.MakeInterface:
			// Value converted to interface{} (e.g., for variadic args) → stored
			// This happens when passing *gorm.DB to methods with interface{} args
			return true

		case *ssa.Call:
			callee := user.Call.StaticCallee()
			if callee == nil {
				continue
			}
			sig := callee.Signature
			if sig == nil || sig.Recv() == nil || !typeutil.IsGormDB(sig.Recv().Type()) {
				continue
			}

			// Check if our result is receiver of this gorm method
			if len(user.Call.Args) > 0 && user.Call.Args[0] == call {
				// Our result is receiver - check if THAT call is stored
				if isClosureResultStoredRecursive(user, visited) {
					return true
				}
			} else {
				// Our result is passed as argument (e.g., condition.Or(ourResult))
				// This is effectively "stored" - the value is consumed by another call
				return true
			}
		}
	}

	return false
}

// cloneVisited creates a copy of the visited map.
// Used to isolate tracing state when entering closures.
func cloneVisited(visited map[ssa.Value]bool) map[ssa.Value]bool {
	clone := make(map[ssa.Value]bool, len(visited))
	for k, v := range visited {
		clone[k] = v
	}
	return clone
}

// ClosureCapturesGormDB checks if a closure captures *gorm.DB values.
//
// Used to determine if a closure needs recursive analysis. A closure
// that doesn't capture *gorm.DB can be skipped for efficiency.
//
// Checks both direct *gorm.DB and **gorm.DB (pointer to pointer).
func ClosureCapturesGormDB(mc *ssa.MakeClosure) bool {
	for _, binding := range mc.Bindings {
		t := binding.Type()
		if typeutil.IsGormDB(t) {
			return true
		}
		// Check **gorm.DB (pointer to pointer, from captured variables)
		if ptr, ok := t.(*types.Pointer); ok {
			if typeutil.IsGormDB(ptr.Elem()) {
				return true
			}
		}
	}
	return false
}
