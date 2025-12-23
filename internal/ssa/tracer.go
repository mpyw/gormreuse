// Package ssa provides SSA-based analysis for GORM *gorm.DB reuse detection.
package ssa

import (
	"go/token"

	"github.com/mpyw/gormreuse/internal/directive"
	"github.com/mpyw/gormreuse/internal/typeutil"
	"golang.org/x/tools/go/ssa"
)

// =============================================================================
// TraceResult - Validated Result Type for SSA Value Tracing
// =============================================================================

// traceResultKind represents the kind of trace result.
type traceResultKind int

const (
	traceResultImmutable traceResultKind = iota
	traceResultMutableRoot
)

// traceResult represents the result of tracing an SSA value.
type traceResult struct {
	kind traceResultKind
	root ssa.Value
}

func immutableResult() traceResult {
	return traceResult{kind: traceResultImmutable}
}

func mutableRootResult(root ssa.Value) traceResult {
	return traceResult{kind: traceResultMutableRoot, root: root}
}

// =============================================================================
// SSATracer - Common SSA Value Traversal Patterns
// =============================================================================

// SSATracer provides common SSA value traversal operations.
type SSATracer struct{}

// NewSSATracer creates a new SSATracer.
func NewSSATracer() *SSATracer {
	return &SSATracer{}
}

// TracePhiEdges traces all edges of a Phi node, skipping nil constants and cycles.
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
func (t *SSATracer) TraceFreeVar(fv *ssa.FreeVar, trace func(ssa.Value) ssa.Value) ssa.Value {
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
				return trace(mc.Bindings[idx])
			}
		}
	}
	return nil
}

// TraceAllocStore finds the value stored to an Alloc instruction.
func (t *SSATracer) TraceAllocStore(alloc *ssa.Alloc, trace func(ssa.Value) ssa.Value) ssa.Value {
	fn := alloc.Parent()
	if fn == nil {
		return nil
	}

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
			if storeFA.X == fa.X && storeFA.Field == fa.Field {
				return trace(store.Val)
			}
		}
	}
	return nil
}

// TraceIIFEReturns traces through an IIFE to find return values.
func (t *SSATracer) TraceIIFEReturns(fn *ssa.Function, visited map[ssa.Value]bool, trace func(ssa.Value, map[ssa.Value]bool) ssa.Value) ssa.Value {
	if fn.Signature == nil {
		return nil
	}
	results := fn.Signature.Results()
	if results == nil || results.Len() == 0 {
		return nil
	}

	retType := results.At(0).Type()
	if !typeutil.IsGormDB(retType) {
		return nil
	}

	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			ret, ok := instr.(*ssa.Return)
			if !ok || len(ret.Results) == 0 {
				continue
			}

			retVisited := cloneVisited(visited)
			if root := trace(ret.Results[0], retVisited); root != nil {
				return root
			}
		}
	}
	return nil
}

// TracePointerLoad traces a pointer load through various pointer sources.
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

// TraceUnOp traces through a UnOp instruction.
func (t *SSATracer) TraceUnOp(unop *ssa.UnOp, trace func(ssa.Value) ssa.Value) ssa.Value {
	if unop.Op == token.MUL {
		return t.TracePointerLoad(unop.X, trace)
	}
	return trace(unop.X)
}

// =============================================================================
// RootTracer
// =============================================================================

// RootTracer traces SSA values to find mutable *gorm.DB roots.
type RootTracer struct {
	ssaTracer *SSATracer
	pureFuncs *directive.PureFuncSet
}

// PureFuncs returns the pure functions set (for testing).
func (t *RootTracer) PureFuncs() *directive.PureFuncSet {
	return t.pureFuncs
}

// NewRootTracer creates a new RootTracer with the given pure functions.
func NewRootTracer(pureFuncs *directive.PureFuncSet) *RootTracer {
	return &RootTracer{
		ssaTracer: NewSSATracer(),
		pureFuncs: pureFuncs,
	}
}

// FindMutableRoot finds the mutable root for a receiver value.
func (t *RootTracer) FindMutableRoot(recv ssa.Value) ssa.Value {
	result := t.trace(recv, make(map[ssa.Value]bool))
	if result.kind == traceResultMutableRoot {
		return result.root
	}
	return nil
}

// FindAllMutableRoots finds ALL possible mutable roots from a value.
func (t *RootTracer) FindAllMutableRoots(v ssa.Value) []ssa.Value {
	return t.traceAll(v, make(map[ssa.Value]bool))
}

// IsPureFunctionUserDefined checks if a user-defined function is marked as pure.
func (t *RootTracer) IsPureFunctionUserDefined(fn *ssa.Function) bool {
	return t.pureFuncs.Contains(fn)
}

// IsPureFunction checks if a function is pure.
func (t *RootTracer) IsPureFunction(fn *ssa.Function) bool {
	if fn == nil {
		return false
	}
	return typeutil.IsPureFunctionBuiltin(fn.Name()) || t.IsPureFunctionUserDefined(fn)
}

func (t *RootTracer) trace(v ssa.Value, visited map[ssa.Value]bool) traceResult {
	if visited[v] {
		return immutableResult()
	}
	visited[v] = true

	if t.IsImmutableSource(v) {
		return immutableResult()
	}

	call, ok := v.(*ssa.Call)
	if !ok {
		return t.traceNonCall(v, visited)
	}

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

	sig := callee.Signature
	if sig == nil || sig.Recv() == nil || !typeutil.IsGormDB(sig.Recv().Type()) {
		if typeutil.IsGormDB(call.Type()) {
			if t.IsPureFunction(callee) {
				return immutableResult()
			}
			return mutableRootResult(call)
		}
		return immutableResult()
	}

	if len(call.Call.Args) == 0 {
		return immutableResult()
	}
	recv := call.Call.Args[0]

	if t.IsImmutableSource(recv) {
		return mutableRootResult(call)
	}

	return t.trace(recv, visited)
}

func (t *RootTracer) traceWithVisited(v ssa.Value, visited map[ssa.Value]bool) ssa.Value {
	result := t.trace(v, visited)
	if result.kind == traceResultMutableRoot {
		return result.root
	}
	return nil
}

func (t *RootTracer) traceNonCall(v ssa.Value, visited map[ssa.Value]bool) traceResult {
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

func (t *RootTracer) traceAll(v ssa.Value, visited map[ssa.Value]bool) []ssa.Value {
	if v == nil || visited[v] {
		return nil
	}
	visited[v] = true

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
		freshVisited := cloneVisited(visited)
		delete(freshVisited, v)
		result := t.trace(v, freshVisited)
		if result.kind == traceResultMutableRoot && result.root != nil {
			return []ssa.Value{result.root}
		}
		return nil
	}
}

// IsImmutableSource checks if a value is an immutable source.
func (t *RootTracer) IsImmutableSource(v ssa.Value) bool {
	switch val := v.(type) {
	case *ssa.Parameter:
		return true
	case *ssa.Call:
		callee := val.Call.StaticCallee()
		return t.IsPureFunction(callee)
	default:
		return false
	}
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

func cloneVisited(visited map[ssa.Value]bool) map[ssa.Value]bool {
	clone := make(map[ssa.Value]bool, len(visited))
	for k, v := range visited {
		clone[k] = v
	}
	return clone
}
