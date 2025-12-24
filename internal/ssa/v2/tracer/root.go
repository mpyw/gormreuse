// Package tracer provides SSA value tracing for gormreuse.
package tracer

import (
	"go/token"
	"go/types"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/directive"
	"github.com/mpyw/gormreuse/internal/typeutil"
)

// RootTracer traces SSA values to find mutable *gorm.DB roots.
//
// Key difference from v1:
// - Variable assignment (Store to Alloc) creates a NEW mutable root
// - This prevents violation from propagating through assigned variables
//
// Example:
//
//	base := db.Where("x")     // base is mutable root
//	base.Find(nil)            // marks base polluted
//	q := base.Where("y")      // violation! AND q becomes NEW root
//	q.Count(nil)              // OK (q is fresh, not polluted)
type RootTracer struct {
	pureFuncs *directive.PureFuncSet
}

// New creates a new RootTracer.
func New(pureFuncs *directive.PureFuncSet) *RootTracer {
	return &RootTracer{pureFuncs: pureFuncs}
}

// FindMutableRoot finds the mutable root for a receiver value.
// Returns nil if the value traces back to an immutable source.
//
// The key semantic: We stop at gorm call results, NOT at variable origins.
// This means each gorm call result that is "directly used" is its own root.
func (t *RootTracer) FindMutableRoot(recv ssa.Value) ssa.Value {
	return t.trace(recv, make(map[ssa.Value]bool))
}

// FindAllMutableRoots finds ALL possible mutable roots (for Phi nodes).
func (t *RootTracer) FindAllMutableRoots(v ssa.Value) []ssa.Value {
	return t.traceAll(v, make(map[ssa.Value]bool))
}

// IsPureFunction checks if a function is marked as pure.
func (t *RootTracer) IsPureFunction(fn *ssa.Function) bool {
	if fn == nil {
		return false
	}
	return typeutil.IsPureFunctionBuiltin(fn.Name()) || t.pureFuncs.Contains(fn)
}

func (t *RootTracer) trace(v ssa.Value, visited map[ssa.Value]bool) ssa.Value {
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
		return t.traceCall(call, visited)
	}

	// Handle non-call values
	return t.traceNonCall(v, visited)
}

func (t *RootTracer) traceCall(call *ssa.Call, visited map[ssa.Value]bool) ssa.Value {
	// Handle closures (IIFE)
	if mc, ok := call.Call.Value.(*ssa.MakeClosure); ok {
		if closureFn, ok := mc.Fn.(*ssa.Function); ok {
			if root := t.traceIIFEReturns(closureFn, visited); root != nil {
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
			if t.IsPureFunction(callee) {
				return nil
			}
			return call
		}
		return nil
	}

	// This is a gorm method call - THIS CALL is the mutable root
	// Don't trace further - each gorm call result is its own root
	// This implements "variable assignment creates new root" semantics
	return call
}

func (t *RootTracer) traceNonCall(v ssa.Value, visited map[ssa.Value]bool) ssa.Value {
	switch val := v.(type) {
	case *ssa.Phi:
		return t.tracePhi(val, visited)

	case *ssa.UnOp:
		return t.traceUnOp(val, visited)

	case *ssa.ChangeType:
		return t.trace(val.X, visited)

	case *ssa.Extract:
		return t.trace(val.Tuple, visited)

	case *ssa.FreeVar:
		return t.traceFreeVar(val, visited)

	case *ssa.Alloc:
		return t.traceAlloc(val, visited)

	default:
		return nil
	}
}

func (t *RootTracer) tracePhi(phi *ssa.Phi, visited map[ssa.Value]bool) ssa.Value {
	for _, edge := range phi.Edges {
		if isNilConst(edge) || visited[edge] {
			continue
		}
		if root := t.trace(edge, visited); root != nil {
			return root
		}
	}
	return nil
}

func (t *RootTracer) traceUnOp(unop *ssa.UnOp, visited map[ssa.Value]bool) ssa.Value {
	if unop.Op == token.MUL {
		// Pointer dereference - trace through the pointer
		return t.tracePointerLoad(unop.X, visited)
	}
	return t.trace(unop.X, visited)
}

func (t *RootTracer) tracePointerLoad(ptr ssa.Value, visited map[ssa.Value]bool) ssa.Value {
	switch p := ptr.(type) {
	case *ssa.FreeVar:
		return t.traceFreeVar(p, visited)
	case *ssa.Alloc:
		return t.traceAlloc(p, visited)
	case *ssa.FieldAddr:
		return t.traceFieldStore(p, visited)
	default:
		return t.trace(ptr, visited)
	}
}

func (t *RootTracer) traceFreeVar(fv *ssa.FreeVar, visited map[ssa.Value]bool) ssa.Value {
	fn := fv.Parent()
	if fn == nil {
		return nil
	}

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
			if idx < len(mc.Bindings) {
				return t.trace(mc.Bindings[idx], visited)
			}
		}
	}
	return nil
}

func (t *RootTracer) traceAlloc(alloc *ssa.Alloc, visited map[ssa.Value]bool) ssa.Value {
	fn := alloc.Parent()
	if fn == nil {
		return nil
	}

	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			store, ok := instr.(*ssa.Store)
			if !ok || store.Addr != alloc {
				continue
			}
			return t.trace(store.Val, visited)
		}
	}
	return nil
}

func (t *RootTracer) traceFieldStore(fa *ssa.FieldAddr, visited map[ssa.Value]bool) ssa.Value {
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
			if !ok || storeFA.X != fa.X || storeFA.Field != fa.Field {
				continue
			}
			return t.trace(store.Val, visited)
		}
	}
	return nil
}

func (t *RootTracer) traceIIFEReturns(fn *ssa.Function, visited map[ssa.Value]bool) ssa.Value {
	if fn.Signature == nil {
		return nil
	}
	results := fn.Signature.Results()
	if results == nil || results.Len() == 0 || !typeutil.IsGormDB(results.At(0).Type()) {
		return nil
	}

	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			ret, ok := instr.(*ssa.Return)
			if !ok || len(ret.Results) == 0 {
				continue
			}
			retVisited := cloneVisited(visited)
			if root := t.trace(ret.Results[0], retVisited); root != nil {
				return root
			}
		}
	}
	return nil
}

func (t *RootTracer) traceAll(v ssa.Value, visited map[ssa.Value]bool) []ssa.Value {
	if v == nil || visited[v] {
		return nil
	}
	visited[v] = true

	switch val := v.(type) {
	case *ssa.Phi:
		var roots []ssa.Value
		for _, edge := range val.Edges {
			if isNilConst(edge) || visited[edge] {
				continue
			}
			roots = append(roots, t.traceAll(edge, visited)...)
		}
		return roots

	case *ssa.UnOp:
		if val.Op == token.MUL {
			return t.traceAllPointerLoads(val.X, visited)
		}
		return t.traceAll(val.X, visited)

	case *ssa.Alloc:
		return t.traceAllAllocStores(val, visited)

	default:
		freshVisited := cloneVisited(visited)
		delete(freshVisited, v)
		if root := t.trace(v, freshVisited); root != nil {
			return []ssa.Value{root}
		}
		return nil
	}
}

func (t *RootTracer) traceAllPointerLoads(ptr ssa.Value, visited map[ssa.Value]bool) []ssa.Value {
	switch p := ptr.(type) {
	case *ssa.Alloc:
		return t.traceAllAllocStores(p, visited)
	case *ssa.Phi:
		var roots []ssa.Value
		for _, edge := range p.Edges {
			if isNilConst(edge) || visited[edge] {
				continue
			}
			roots = append(roots, t.traceAll(edge, visited)...)
		}
		return roots
	default:
		return t.traceAll(ptr, visited)
	}
}

func (t *RootTracer) traceAllAllocStores(alloc *ssa.Alloc, visited map[ssa.Value]bool) []ssa.Value {
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
			roots = append(roots, t.traceAll(store.Val, visited)...)
		}
	}
	return roots
}

func (t *RootTracer) isImmutableSource(v ssa.Value) bool {
	switch val := v.(type) {
	case *ssa.Parameter:
		return true
	case *ssa.Const:
		return true
	case *ssa.Call:
		callee := val.Call.StaticCallee()
		return t.IsPureFunction(callee)
	default:
		return false
	}
}

// Helper functions
func isNilConst(v ssa.Value) bool {
	c, ok := v.(*ssa.Const)
	return ok && c.Value == nil
}

func cloneVisited(visited map[ssa.Value]bool) map[ssa.Value]bool {
	clone := make(map[ssa.Value]bool, len(visited))
	for k, v := range visited {
		clone[k] = v
	}
	return clone
}

// ClosureCapturesGormDB checks if a closure captures *gorm.DB values.
func ClosureCapturesGormDB(mc *ssa.MakeClosure) bool {
	for _, binding := range mc.Bindings {
		t := binding.Type()
		if typeutil.IsGormDB(t) {
			return true
		}
		if ptr, ok := t.(*types.Pointer); ok {
			if typeutil.IsGormDB(ptr.Elem()) {
				return true
			}
		}
	}
	return false
}
