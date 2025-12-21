// Package internal provides SSA-based analysis for GORM *gorm.DB reuse detection.
package internal

import (
	"go/token"

	"golang.org/x/tools/go/ssa"
)

// =============================================================================
// RootTracer
//
// RootTracer traces SSA values back to their mutable roots.
// A "mutable root" is the origin *gorm.DB value that can be polluted by chain methods.
//
// This component is stateless and can be reused across multiple analyses.
// =============================================================================

// RootTracer traces SSA values to find mutable *gorm.DB roots.
type RootTracer struct {
	// pureFuncs is a set of functions marked as pure (don't pollute *gorm.DB).
	pureFuncs map[string]struct{}
}

// NewRootTracer creates a new RootTracer with the given pure functions.
func NewRootTracer(pureFuncs map[string]struct{}) *RootTracer {
	return &RootTracer{pureFuncs: pureFuncs}
}

// FindMutableRoot finds the mutable root for a receiver value.
// Returns nil if the receiver is immutable (Session result, parameter, etc.)
func (t *RootTracer) FindMutableRoot(recv ssa.Value) ssa.Value {
	return t.findMutableRootImpl(recv, make(map[ssa.Value]bool))
}

// FindAllMutableRoots finds ALL possible mutable roots from a value.
// For Phi nodes, this returns roots from ALL edges (not just the first).
// This is used for pollution checking where ANY polluted path should be detected.
func (t *RootTracer) FindAllMutableRoots(v ssa.Value) []ssa.Value {
	return t.findAllMutableRootsImpl(v, make(map[ssa.Value]bool))
}

// =============================================================================
// Internal Implementation
// =============================================================================

// findMutableRootImpl recursively finds the mutable root.
func (t *RootTracer) findMutableRootImpl(v ssa.Value, visited map[ssa.Value]bool) ssa.Value {
	if visited[v] {
		return nil
	}
	visited[v] = true

	// Check if this is an immutable source (Parameter, Safe Method result)
	if t.isImmutableSource(v) {
		return nil
	}

	call, ok := v.(*ssa.Call)
	if !ok {
		// Non-call values - check for Phi, UnOp, etc.
		return t.handleNonCallForRoot(v, visited)
	}

	callee := call.Call.StaticCallee()

	// Check if this is an IIFE (Immediately Invoked Function Expression)
	// e.g., func() *gorm.DB { return q.Where(...) }().Find(nil)
	if mc, ok := call.Call.Value.(*ssa.MakeClosure); ok {
		if closureFn, ok := mc.Fn.(*ssa.Function); ok {
			if root := t.traceIIFEReturns(closureFn, visited); root != nil {
				return root
			}
		}
	}

	if callee == nil {
		return nil
	}

	sig := callee.Signature
	if sig == nil || sig.Recv() == nil || !IsGormDB(sig.Recv().Type()) {
		// Not a *gorm.DB method - could be a helper function
		// Treat the function return as a mutable root
		if IsGormDB(call.Type()) {
			return call
		}
		return nil
	}

	// This is a *gorm.DB method call
	if len(call.Call.Args) == 0 {
		return nil
	}
	recv := call.Call.Args[0]

	// If receiver is immutable, this call is the mutable root
	if t.isImmutableSource(recv) {
		return call
	}

	// Receiver is mutable - trace back to find the root
	return t.findMutableRootImpl(recv, visited)
}

func (t *RootTracer) findAllMutableRootsImpl(v ssa.Value, visited map[ssa.Value]bool) []ssa.Value {
	if v == nil || visited[v] {
		return nil
	}
	visited[v] = true

	switch val := v.(type) {
	case *ssa.Phi:
		// Phi node - collect roots from ALL edges
		var roots []ssa.Value
		for _, edge := range val.Edges {
			if isNilConst(edge) {
				continue
			}
			edgeRoots := t.findAllMutableRootsImpl(edge, visited)
			roots = append(roots, edgeRoots...)
		}
		return roots

	case *ssa.UnOp:
		// Load operation - trace through to find stored values
		if val.Op == token.MUL {
			return t.traceAllPointerLoads(val.X, visited)
		}
		return t.findAllMutableRootsImpl(val.X, visited)

	case *ssa.Alloc:
		// Alloc - find ALL values stored to this alloc
		return t.traceAllAllocStores(val, visited)

	default:
		// For other values, use normal root finding.
		// We need a fresh visited map because this value was already marked visited above.
		freshVisited := make(map[ssa.Value]bool)
		for k := range visited {
			freshVisited[k] = true
		}
		delete(freshVisited, v) // Allow this value to be processed
		root := t.findMutableRootImpl(v, freshVisited)
		if root != nil {
			return []ssa.Value{root}
		}
		return nil
	}
}

// traceAllPointerLoads traces a pointer load to find ALL possible stored values.
func (t *RootTracer) traceAllPointerLoads(ptr ssa.Value, visited map[ssa.Value]bool) []ssa.Value {
	switch p := ptr.(type) {
	case *ssa.Alloc:
		return t.traceAllAllocStores(p, visited)
	case *ssa.Phi:
		var roots []ssa.Value
		for _, edge := range p.Edges {
			if isNilConst(edge) {
				continue
			}
			edgeRoots := t.traceAllPointerLoads(edge, visited)
			roots = append(roots, edgeRoots...)
		}
		return roots
	default:
		return t.findAllMutableRootsImpl(ptr, visited)
	}
}

// traceAllAllocStores finds ALL values stored to an Alloc instruction.
func (t *RootTracer) traceAllAllocStores(alloc *ssa.Alloc, visited map[ssa.Value]bool) []ssa.Value {
	fn := alloc.Parent()
	if fn == nil {
		return nil
	}

	var roots []ssa.Value
	// Find ALL Store instructions that write to this alloc
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			store, ok := instr.(*ssa.Store)
			if !ok {
				continue
			}
			if store.Addr == alloc {
				// Found a store to this alloc - trace the stored value
				storeRoots := t.findAllMutableRootsImpl(store.Val, visited)
				roots = append(roots, storeRoots...)
			}
		}
	}
	return roots
}

// handleNonCallForRoot handles non-call values when finding mutable root.
func (t *RootTracer) handleNonCallForRoot(v ssa.Value, visited map[ssa.Value]bool) ssa.Value {
	switch val := v.(type) {
	case *ssa.Phi:
		// For Phi nodes, trace through all edges to find any mutable root.
		// If any edge has a mutable root, return it (conservative for false-negative reduction).
		// Skip nil constant edges - nil pointers can't have methods called on them.
		for _, edge := range val.Edges {
			if isNilConst(edge) {
				continue
			}
			if root := t.findMutableRootImpl(edge, visited); root != nil {
				return root
			}
		}
		return nil
	case *ssa.UnOp:
		// Dereference (*ptr) - trace through to find the stored value
		if val.Op == token.MUL {
			return t.tracePointerLoad(val.X, visited)
		}
		return t.findMutableRootImpl(val.X, visited)
	case *ssa.ChangeType:
		return t.findMutableRootImpl(val.X, visited)
	case *ssa.Extract:
		return t.findMutableRootImpl(val.Tuple, visited)
	case *ssa.FreeVar:
		// Trace FreeVar back through MakeClosure to find the bound value
		return t.traceFreeVar(val, visited)
	case *ssa.Alloc:
		// Alloc is a heap/stack allocation. Trace to find stored value.
		return t.traceAllocStore(val, visited)
	default:
		return nil
	}
}

// tracePointerLoad traces a pointer load (dereference) to find the mutable root.
// When we have *ptr, we need to find what value was stored to ptr.
func (t *RootTracer) tracePointerLoad(ptr ssa.Value, visited map[ssa.Value]bool) ssa.Value {
	// First, recursively resolve the pointer value (might be FreeVar, Alloc, etc.)
	switch p := ptr.(type) {
	case *ssa.FreeVar:
		// FreeVar pointer - trace back through MakeClosure
		return t.traceFreeVar(p, visited)
	case *ssa.Alloc:
		// Local allocation - find the stored value
		return t.traceAllocStore(p, visited)
	case *ssa.FieldAddr:
		// Struct field - find Store to this field and trace the stored value
		return t.traceFieldStore(p, visited)
	default:
		// Try to trace through other pointer sources
		return t.findMutableRootImpl(ptr, visited)
	}
}

// traceAllocStore finds the value stored to an Alloc instruction.
// Variables captured by closures are allocated on heap and use Store instructions.
func (t *RootTracer) traceAllocStore(alloc *ssa.Alloc, visited map[ssa.Value]bool) ssa.Value {
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
				// Found a store to this alloc - trace the stored value
				return t.findMutableRootImpl(store.Val, visited)
			}
		}
	}
	return nil
}

// traceFieldStore finds the value stored to a struct field.
// When we have h.field, we find Store instructions that write to the same field.
func (t *RootTracer) traceFieldStore(fa *ssa.FieldAddr, visited map[ssa.Value]bool) ssa.Value {
	fn := fa.Parent()
	if fn == nil {
		return nil
	}

	// Find Store instructions that write to a FieldAddr with same base and field
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
				return t.findMutableRootImpl(store.Val, visited)
			}
		}
	}
	return nil
}

// traceFreeVar traces a FreeVar back to the value bound in MakeClosure.
// FreeVars are variables captured from an enclosing function scope.
func (t *RootTracer) traceFreeVar(fv *ssa.FreeVar, visited map[ssa.Value]bool) ssa.Value {
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
				return t.findMutableRootImpl(mc.Bindings[idx], visited)
			}
		}
	}
	return nil
}

// traceIIFEReturns traces through an IIFE (Immediately Invoked Function Expression)
// to find the mutable root. It finds all return statements in the function and
// traces the returned values to find any mutable root.
//
// Example:
//
//	func() *gorm.DB {
//	    return q.Where("x = ?", 1)
//	}().Find(nil)
//
// The analyzer traces through the IIFE's return value to find q as the mutable root.
func (t *RootTracer) traceIIFEReturns(fn *ssa.Function, visited map[ssa.Value]bool) ssa.Value {
	// Check if the function returns *gorm.DB
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

	// Find all return statements and trace their values
	// Return the first mutable root found (conservative approach)
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			ret, ok := instr.(*ssa.Return)
			if !ok {
				continue
			}
			if len(ret.Results) == 0 {
				continue
			}

			// Clone visited to trace each return path independently
			retVisited := make(map[ssa.Value]bool)
			for k, v := range visited {
				retVisited[k] = v
			}

			if root := t.findMutableRootImpl(ret.Results[0], retVisited); root != nil {
				return root
			}
		}
	}

	return nil
}

// =============================================================================
// Helper Functions
// =============================================================================

// isNilConst checks if a value is a nil constant.
// Nil pointers cannot have methods called on them, so they are safe to skip
// when tracing Phi nodes (the nil path would panic before reaching the call).
func isNilConst(v ssa.Value) bool {
	c, ok := v.(*ssa.Const)
	if !ok {
		return false
	}
	// For nil pointer constants, Value is nil
	return c.Value == nil
}

// isImmutableSource checks if a value is an immutable source.
// This includes: Session/WithContext results, function parameters, and DB init methods.
// Note: FreeVar is NOT immutable - it needs to be traced back through MakeClosure.
func (t *RootTracer) isImmutableSource(v ssa.Value) bool {
	switch val := v.(type) {
	case *ssa.Parameter:
		return true
	case *ssa.Call:
		callee := val.Call.StaticCallee()
		if callee == nil {
			return false
		}
		// Safe Methods return immutable
		if IsSafeMethod(callee.Name()) {
			return true
		}
		// DB Init Methods return immutable
		if IsDBInitMethod(callee.Name()) {
			return true
		}
		// Function calls returning *gorm.DB are treated as mutable
		// (we don't know what they do internally)
		return false
	default:
		return false
	}
}

// IsPureFunction checks if a function is marked as pure.
func (t *RootTracer) IsPureFunction(fn *ssa.Function) bool {
	if t.pureFuncs == nil {
		return false
	}

	// Build function name: pkgPath.FuncName or pkgPath.(ReceiverType).MethodName
	fullName := fn.String()
	_, exists := t.pureFuncs[fullName]
	return exists
}
