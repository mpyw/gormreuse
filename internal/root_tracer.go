// Package internal provides SSA-based analysis for GORM *gorm.DB reuse detection.
package internal

import (
	"go/token"

	"golang.org/x/tools/go/ssa"
)

// =============================================================================
// RootTracer
//
// RootTracer finds mutable roots of *gorm.DB values using SSA traversal.
// A "mutable root" is the origin *gorm.DB value that can be polluted by chain methods.
//
// This component composes SSATracer for common SSA patterns and adds
// *gorm.DB-specific classification logic (immutability, safe methods, etc.)
//
// Design:
//   - SSATracer: HOW to traverse SSA values (mechanism)
//   - RootTracer: WHAT constitutes a mutable root (policy)
// =============================================================================

// RootTracer traces SSA values to find mutable *gorm.DB roots.
type RootTracer struct {
	ssaTracer *SSATracer
	pureFuncs *PureFuncSet
}

// NewRootTracer creates a new RootTracer with the given pure functions.
func NewRootTracer(pureFuncs *PureFuncSet) *RootTracer {
	return &RootTracer{
		ssaTracer: NewSSATracer(),
		pureFuncs: pureFuncs,
	}
}

// FindMutableRoot finds the mutable root for a receiver value.
// Returns nil if the receiver is immutable (Session result, parameter, etc.)
func (t *RootTracer) FindMutableRoot(recv ssa.Value) ssa.Value {
	result := t.trace(recv, make(map[ssa.Value]bool))
	if result.kind == traceResultMutableRoot {
		return result.root
	}
	return nil
}

// FindAllMutableRoots finds ALL possible mutable roots from a value.
// For Phi nodes, this returns roots from ALL edges (not just the first).
// This is used for pollution checking where ANY polluted path should be detected.
func (t *RootTracer) FindAllMutableRoots(v ssa.Value) []ssa.Value {
	return t.traceAll(v, make(map[ssa.Value]bool))
}

// IsPureFunction checks if a function is marked as pure.
func (t *RootTracer) IsPureFunction(fn *ssa.Function) bool {
	return t.pureFuncs.Contains(fn)
}

// =============================================================================
// Core Tracing Logic
// =============================================================================

// trace traces a value to find its mutable root using traceResult pattern.
func (t *RootTracer) trace(v ssa.Value, visited map[ssa.Value]bool) traceResult {
	if visited[v] {
		return immutableResult() // Cycle detected - treat as immutable
	}
	visited[v] = true

	// Check for immutable sources first
	if t.isImmutableSource(v) {
		return immutableResult()
	}

	// Handle non-call values (Phi, UnOp, FreeVar, etc.)
	call, ok := v.(*ssa.Call)
	if !ok {
		return t.traceNonCall(v, visited)
	}

	// Handle IIFE (Immediately Invoked Function Expression)
	if mc, ok := call.Call.Value.(*ssa.MakeClosure); ok {
		if closureFn, ok := mc.Fn.(*ssa.Function); ok {
			if root := t.ssaTracer.TraceIIFEReturns(closureFn, visited, t.traceWithVisited); root != nil {
				return mutableRootResult(root)
			}
		}
	}

	callee := call.Call.StaticCallee()
	if callee == nil {
		return immutableResult()
	}

	// Check if this is a *gorm.DB method call
	sig := callee.Signature
	if sig == nil || sig.Recv() == nil || !IsGormDB(sig.Recv().Type()) {
		// Not a *gorm.DB method - could be a helper function
		if IsGormDB(call.Type()) {
			// pure function returns immutable
			if t.IsPureFunction(callee) {
				return immutableResult()
			}
			return mutableRootResult(call)
		}
		return immutableResult()
	}

	// This is a *gorm.DB method call - get receiver
	if len(call.Call.Args) == 0 {
		return immutableResult()
	}
	recv := call.Call.Args[0]

	// If receiver is immutable, this call is the mutable root
	if t.isImmutableSource(recv) {
		return mutableRootResult(call)
	}

	// Receiver is mutable - continue tracing
	return t.trace(recv, visited)
}

// traceWithVisited is a helper for SSATracer callbacks.
func (t *RootTracer) traceWithVisited(v ssa.Value, visited map[ssa.Value]bool) ssa.Value {
	result := t.trace(v, visited)
	if result.kind == traceResultMutableRoot {
		return result.root
	}
	return nil
}

// traceNonCall handles non-call SSA values.
func (t *RootTracer) traceNonCall(v ssa.Value, visited map[ssa.Value]bool) traceResult {
	// Create trace callback for SSATracer
	traceCallback := func(val ssa.Value) ssa.Value {
		result := t.trace(val, visited)
		if result.kind == traceResultMutableRoot {
			return result.root
		}
		return nil
	}

	switch val := v.(type) {
	case *ssa.Phi:
		if root := t.ssaTracer.TracePhiEdges(val, visited, traceCallback); root != nil {
			return mutableRootResult(root)
		}
		return immutableResult()

	case *ssa.UnOp:
		if root := t.ssaTracer.TraceUnOp(val, traceCallback); root != nil {
			return mutableRootResult(root)
		}
		return immutableResult()

	case *ssa.ChangeType:
		return t.trace(val.X, visited)

	case *ssa.Extract:
		return t.trace(val.Tuple, visited)

	case *ssa.FreeVar:
		if root := t.ssaTracer.TraceFreeVar(val, traceCallback); root != nil {
			return mutableRootResult(root)
		}
		return immutableResult()

	case *ssa.Alloc:
		if root := t.ssaTracer.TraceAllocStore(val, traceCallback); root != nil {
			return mutableRootResult(root)
		}
		return immutableResult()

	default:
		return immutableResult()
	}
}

// =============================================================================
// FindAllMutableRoots Implementation
// =============================================================================

// traceAll collects ALL possible mutable roots (for pollution checking).
func (t *RootTracer) traceAll(v ssa.Value, visited map[ssa.Value]bool) []ssa.Value {
	if v == nil || visited[v] {
		return nil
	}
	visited[v] = true

	// Create trace callback for SSATracer
	traceAllCallback := func(val ssa.Value) []ssa.Value {
		return t.traceAll(val, visited)
	}

	switch val := v.(type) {
	case *ssa.Phi:
		return t.ssaTracer.TraceAllPhiEdges(val, visited, traceAllCallback)

	case *ssa.UnOp:
		if val.Op == token.MUL {
			return t.ssaTracer.TraceAllPointerLoads(val.X, visited, traceAllCallback)
		}
		return t.traceAll(val.X, visited)

	case *ssa.Alloc:
		return t.ssaTracer.TraceAllAllocStores(val, traceAllCallback)

	default:
		// For other values, use single-root tracing with fresh visited map
		freshVisited := cloneVisited(visited)
		delete(freshVisited, v)
		result := t.trace(v, freshVisited)
		if result.kind == traceResultMutableRoot && result.root != nil {
			return []ssa.Value{result.root}
		}
		return nil
	}
}

// =============================================================================
// Classification Logic
// =============================================================================

// isImmutableSource checks if a value is an immutable source.
// This includes: Session/WithContext results, function parameters, and DB init methods.
func (t *RootTracer) isImmutableSource(v ssa.Value) bool {
	switch val := v.(type) {
	case *ssa.Parameter:
		return true
	case *ssa.Call:
		callee := val.Call.StaticCallee()
		if callee == nil {
			return false
		}
		// Immutable-returning methods (Session, WithContext, Open, Begin, etc.)
		if ReturnsImmutable(callee.Name()) {
			return true
		}
		return false
	default:
		return false
	}
}
