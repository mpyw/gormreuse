package tracer

import (
	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/ssa/callsite"
	"github.com/mpyw/gormreuse/internal/ssa/cfg"
)

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
//
//declscope:shared // root.go's traceNonCall and tracePointerLoad send captured variables here
func (t *RootTracer) traceFreeVar(fv *ssa.FreeVar, visited map[ssa.Value]bool, loopInfo *cfg.LoopInfo) ssa.Value {
	if binding := callsite.FreeVarBinding(fv); binding != nil {
		return t.trace(binding, visited, loopInfo)
	}
	return nil
}

// traceFreeVarLoad traces load, a read through fv of a variable that the
// closure captured by reference because it is reassigned. The read sees the
// stores that reach the closure's calls (see allocReachingStoreValues), not
// the first one (#251).
//
//declscope:shared // root.go's tracePointerLoad sends reads of captured variables here
func (t *RootTracer) traceFreeVarLoad(fv *ssa.FreeVar, load ssa.Instruction, visited map[ssa.Value]bool, loopInfo *cfg.LoopInfo) ssa.Value {
	if alloc, ok := freeVarAlloc(fv); ok {
		return t.traceAllocStore(alloc, load, visited, loopInfo)
	}
	return t.traceFreeVar(fv, visited, loopInfo)
}

// freeVarAlloc returns the variable that fv reads, through the FreeVars of
// the closures it is nested in (#341). A closure nested in another one binds
// the outer closure's FreeVar, not the variable. Read as that FreeVar, a load
// lost its own position and saw every store of the variable, also the stores
// of the range-over-func body that holds it.
//
//declscope:shared // closure.go's closureHoldsGormFunc finds a captured function value's variable with it
func freeVarAlloc(fv *ssa.FreeVar) (*ssa.Alloc, bool) {
	binding := callsite.FreeVarBinding(fv)
	for {
		outer, ok := binding.(*ssa.FreeVar)
		if !ok {
			break
		}
		binding = callsite.FreeVarBinding(outer)
	}
	alloc, ok := binding.(*ssa.Alloc)
	return alloc, ok
}

// traceAllFreeVarLoads is traceFreeVarLoad for every root.
//
//declscope:shared // root.go's traceAllPointerLoads sends reads of captured variables here
func (t *RootTracer) traceAllFreeVarLoads(fv *ssa.FreeVar, load ssa.Instruction, search *allRootsSearch, loopInfo *cfg.LoopInfo) []ssa.Value {
	if alloc, ok := freeVarAlloc(fv); ok {
		return t.traceAllAllocStores(alloc, load, search, loopInfo)
	}
	return t.traceAllFreeVar(fv, search, loopInfo)
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
//
//declscope:shared // root.go's traceAll sends captured variables here
func (t *RootTracer) traceAllFreeVar(fv *ssa.FreeVar, search *allRootsSearch, loopInfo *cfg.LoopInfo) []ssa.Value {
	if binding := callsite.FreeVarBinding(fv); binding != nil {
		return t.traceAll(binding, search, loopInfo)
	}
	return nil
}
