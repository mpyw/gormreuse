// Package internal provides SSA-based analysis for GORM *gorm.DB reuse detection.
package internal

import (
	"go/token"

	"golang.org/x/tools/go/ssa"
)

// =============================================================================
// TraceResult - Validated Result Type for SSA Value Tracing
//
// Instead of returning (root, nil) pairs with unclear semantics, traceResult
// provides explicit states for tracing operations. This pattern is inspired
// by zerologlintctx's traceResult design.
//
// States:
//   - Immutable: Value comes from an immutable source (Parameter, Session result)
//   - MutableRoot: Found a mutable root that can be polluted
//   - Continue: Need to continue tracing (used internally)
// =============================================================================

// traceResultKind represents the kind of trace result.
type traceResultKind int

const (
	// traceResultImmutable indicates the value comes from an immutable source.
	// No pollution tracking is needed for this value.
	traceResultImmutable traceResultKind = iota

	// traceResultMutableRoot indicates a mutable root was found.
	// The root field contains the value that can be polluted.
	traceResultMutableRoot
)

// traceResult represents the result of tracing an SSA value.
type traceResult struct {
	kind traceResultKind
	root ssa.Value
}

// immutableResult returns a result indicating an immutable source.
func immutableResult() traceResult {
	return traceResult{kind: traceResultImmutable}
}

// mutableRootResult returns a result with a mutable root.
func mutableRootResult(root ssa.Value) traceResult {
	return traceResult{kind: traceResultMutableRoot, root: root}
}

// =============================================================================
// SSATracer - Common SSA Value Traversal Patterns
//
// SSATracer handles common SSA value types that appear in any SSA-based analysis:
//   - Phi nodes (conditional branches, loops)
//   - UnOp (pointer dereference)
//   - FreeVar (closure captures)
//   - Alloc/Store (heap allocations)
//   - FieldAddr (struct field access)
//
// This is the "mechanism" layer - HOW to traverse SSA values.
// The "policy" layer (what constitutes a mutable root) is in RootTracer.
// =============================================================================

// SSATracer provides common SSA value traversal operations.
type SSATracer struct{}

// NewSSATracer creates a new SSATracer.
func NewSSATracer() *SSATracer {
	return &SSATracer{}
}

// TracePhiEdges traces all edges of a Phi node, skipping nil constants and cycles.
// Returns the first non-nil result from the callback, or nil if all edges return nil.
//
// Example:
//
//	var x *gorm.DB
//	if cond {
//	    x = db.Where("a")  // edge[0]
//	} else {
//	    x = nil            // edge[1] - skipped (nil constant)
//	}
//	x.Find(nil)  // x is Phi node
//
//	SSA:
//	  x = phi [db.Where("a"), nil]
//	  └─ TracePhiEdges skips nil, traces db.Where("a")
func (t *SSATracer) TracePhiEdges(phi *ssa.Phi, visited map[ssa.Value]bool, trace func(ssa.Value) ssa.Value) ssa.Value {
	for _, edge := range phi.Edges {
		if IsNilConst(edge) {
			continue
		}
		if visited[edge] {
			continue
		}
		if root := trace(edge); root != nil {
			return root
		}
	}
	return nil
}

// TraceAllPhiEdges collects roots from ALL Phi edges (for pollution checking).
func (t *SSATracer) TraceAllPhiEdges(phi *ssa.Phi, visited map[ssa.Value]bool, trace func(ssa.Value) []ssa.Value) []ssa.Value {
	var roots []ssa.Value
	for _, edge := range phi.Edges {
		if IsNilConst(edge) {
			continue
		}
		if visited[edge] {
			continue
		}
		edgeRoots := trace(edge)
		roots = append(roots, edgeRoots...)
	}
	return roots
}

// TraceFreeVar traces a FreeVar back to the value bound in MakeClosure.
// FreeVars are variables captured from an enclosing function scope.
//
// Example:
//
//	func outer() {
//	    q := db.Where("x")           // q is bound to closure
//	    f := func() {
//	        q.Find(nil)              // q is FreeVar here
//	    }
//	    f()
//	}
//
//	SSA structure:
//	  outer:
//	    t0 = db.Where("x")           // q's value
//	    t1 = make closure (inner, [t0])  // MakeClosure binds t0
//	  inner (FreeVars: [q]):
//	    t2 = q.Find(nil)             // q is FreeVar
//
//	TraceFreeVar flow:
//	  1. Find FreeVar index in inner.FreeVars → 0
//	  2. Find MakeClosure in outer that creates inner
//	  3. Return mc.Bindings[0] → t0 (the bound value)
func (t *SSATracer) TraceFreeVar(fv *ssa.FreeVar, trace func(ssa.Value) ssa.Value) ssa.Value {
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

	// Look for MakeClosure instructions in the parent that create this closure
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
			// Check if this MakeClosure creates our function
			closureFn, ok := mc.Fn.(*ssa.Function)
			if !ok || closureFn != fn {
				continue
			}
			// mc.Bindings[idx] is the value bound to this FreeVar
			if idx < len(mc.Bindings) {
				return trace(mc.Bindings[idx])
			}
		}
	}
	return nil
}

// TraceAllocStore finds the value stored to an Alloc instruction.
// Variables captured by closures are allocated on heap and use Store instructions.
//
// Example:
//
//	func example() {
//	    var q *gorm.DB              // Alloc instruction
//	    q = db.Where("x")           // Store to Alloc
//	    q.Find(nil)                 // Load from Alloc (UnOp *q)
//	}
//
//	SSA:
//	  t0 = local *gorm.DB (q)       // Alloc
//	  t1 = db.Where("x")
//	  *t0 = t1                      // Store: Addr=t0, Val=t1
//	  t2 = *t0                      // UnOp (MUL): load q's value
//	  t3 = t2.Find(nil)
//
//	TraceAllocStore flow:
//	  1. Given Alloc t0, find Store with Addr=t0
//	  2. Return Store.Val → t1 (db.Where("x"))
func (t *SSATracer) TraceAllocStore(alloc *ssa.Alloc, trace func(ssa.Value) ssa.Value) ssa.Value {
	fn := alloc.Parent()
	if fn == nil {
		return nil
	}

	// Find Store instructions that write to this alloc
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			store, ok := instr.(*ssa.Store)
			if !ok {
				continue
			}
			if store.Addr == alloc {
				return trace(store.Val)
			}
		}
	}
	return nil
}

// TraceAllAllocStores finds ALL values stored to an Alloc instruction.
func (t *SSATracer) TraceAllAllocStores(alloc *ssa.Alloc, trace func(ssa.Value) []ssa.Value) []ssa.Value {
	fn := alloc.Parent()
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
			if store.Addr == alloc {
				storeRoots := trace(store.Val)
				roots = append(roots, storeRoots...)
			}
		}
	}
	return roots
}

// TraceFieldStore finds the value stored to a struct field.
func (t *SSATracer) TraceFieldStore(fa *ssa.FieldAddr, trace func(ssa.Value) ssa.Value) ssa.Value {
	fn := fa.Parent()
	if fn == nil {
		return nil
	}

	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			store, ok := instr.(*ssa.Store)
			if !ok {
				continue
			}
			storeFA, ok := store.Addr.(*ssa.FieldAddr)
			if !ok {
				continue
			}
			// Match by same base and same field index
			if storeFA.X == fa.X && storeFA.Field == fa.Field {
				return trace(store.Val)
			}
		}
	}
	return nil
}

// TraceIIFEReturns traces through an IIFE to find return values.
// Returns the first non-nil result from tracing return statements.
//
// Example:
//
//	q := db.Where("x")
//	_ = func() *gorm.DB {       // IIFE - Immediately Invoked Function Expression
//	    return q.Where("y")
//	}().Find(nil)
//
//	SSA:
//	  t0 = db.Where("x")            // q
//	  t1 = make closure (anon)
//	  t2 = t1()                     // Call the IIFE
//	  t3 = t2.Find(nil)             // We're tracing t2
//
//	  anon:
//	    t4 = q.Where("y")           // q is FreeVar
//	    return t4
//
//	TraceIIFEReturns flow:
//	  1. Check return type is *gorm.DB
//	  2. Find Return instruction in anon
//	  3. Trace ret.Results[0] → t4 (q.Where("y"))
//	  4. Continue tracing to find q as mutable root
func (t *SSATracer) TraceIIFEReturns(fn *ssa.Function, visited map[ssa.Value]bool, trace func(ssa.Value, map[ssa.Value]bool) ssa.Value) ssa.Value {
	if fn.Signature == nil {
		return nil
	}
	results := fn.Signature.Results()
	if results == nil || results.Len() == 0 {
		return nil
	}

	// Only trace if return type is *gorm.DB
	retType := results.At(0).Type()
	if !IsGormDB(retType) {
		return nil
	}

	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			ret, ok := instr.(*ssa.Return)
			if !ok || len(ret.Results) == 0 {
				continue
			}

			// Clone visited for independent path tracing
			retVisited := cloneVisited(visited)
			if root := trace(ret.Results[0], retVisited); root != nil {
				return root
			}
		}
	}
	return nil
}

// =============================================================================
// Helper Functions
// =============================================================================

// IsNilConst checks if a value is a nil constant.
func IsNilConst(v ssa.Value) bool {
	c, ok := v.(*ssa.Const)
	if !ok {
		return false
	}
	return c.Value == nil
}

// cloneVisited creates a shallow copy of a visited map.
func cloneVisited(visited map[ssa.Value]bool) map[ssa.Value]bool {
	clone := make(map[ssa.Value]bool, len(visited))
	for k, v := range visited {
		clone[k] = v
	}
	return clone
}

// TracePointerLoad traces a pointer load (dereference) through various pointer sources.
func (t *SSATracer) TracePointerLoad(ptr ssa.Value, trace func(ssa.Value) ssa.Value) ssa.Value {
	switch p := ptr.(type) {
	case *ssa.FreeVar:
		return t.TraceFreeVar(p, trace)
	case *ssa.Alloc:
		return t.TraceAllocStore(p, trace)
	case *ssa.FieldAddr:
		return t.TraceFieldStore(p, trace)
	default:
		return trace(ptr)
	}
}

// TraceAllPointerLoads traces a pointer load collecting ALL possible stored values.
func (t *SSATracer) TraceAllPointerLoads(ptr ssa.Value, visited map[ssa.Value]bool, traceAll func(ssa.Value) []ssa.Value) []ssa.Value {
	switch p := ptr.(type) {
	case *ssa.Alloc:
		return t.TraceAllAllocStores(p, traceAll)
	case *ssa.Phi:
		return t.TraceAllPhiEdges(p, visited, traceAll)
	default:
		return traceAll(ptr)
	}
}

// TraceUnOp traces through a UnOp instruction (typically pointer dereference).
func (t *SSATracer) TraceUnOp(unop *ssa.UnOp, trace func(ssa.Value) ssa.Value) ssa.Value {
	if unop.Op == token.MUL {
		// Pointer dereference - trace through to find stored value
		return t.TracePointerLoad(unop.X, trace)
	}
	return trace(unop.X)
}
