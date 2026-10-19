package tracer

import (
	"slices"

	"golang.org/x/tools/go/ssa"

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
	if binding := FreeVarBinding(fv); binding != nil {
		return t.trace(binding, visited, loopInfo)
	}
	return nil
}

// FreeVarBinding resolves the value captured by fv: it locates the MakeClosure
// that creates fv's parent closure and returns the binding at fv's index in the
// captured-variable list. Returns nil if the closure or binding can't be found.
// Shared by traceFreeVar (single root), traceAllFreeVar (all roots), the Scopes
// callback resolver, and scopeswarn.
func FreeVarBinding(fv *ssa.FreeVar) ssa.Value {
	fn := fv.Parent()
	if fn == nil {
		return nil
	}

	// Find the index of this FreeVar in the function's FreeVars list
	idx := slices.Index(fn.FreeVars, fv)
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
			if idx < len(mc.Bindings) {
				return mc.Bindings[idx]
			}
		}
	}
	return nil
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
func (t *RootTracer) traceAllFreeVar(fv *ssa.FreeVar, visited map[ssa.Value]bool, loopInfo *cfg.LoopInfo) []ssa.Value {
	if binding := FreeVarBinding(fv); binding != nil {
		return t.traceAll(binding, visited, loopInfo)
	}
	return nil
}
