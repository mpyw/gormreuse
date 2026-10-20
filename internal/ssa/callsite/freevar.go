package callsite

import (
	"slices"

	"golang.org/x/tools/go/ssa"
)

// FreeVarBinding resolves the value captured by fv: it locates the MakeClosure
// that creates fv's parent closure and returns the binding at fv's index in the
// captured-variable list. Returns nil if the closure can't be found, as for a
// closure created only in a block that SSA removed as unreachable. Shared by
// the tracer, the callback resolver, scopeswarn and the resolution of calls
// through a captured local function variable (FuncVars.LocalValue).
func FreeVarBinding(fv *ssa.FreeVar) ssa.Value {
	// A FreeVar belongs to a closure, whose MakeClosure binds each of its
	// FreeVars in order.
	fn := fv.Parent()
	if mc := FreeVarMakeClosure(fn); mc != nil {
		return mc.Bindings[slices.Index(fn.FreeVars, fv)]
	}
	return nil
}

// FreeVarMakeClosure returns the MakeClosure that creates fn's closure in its
// enclosing function, or nil when fn is not a closure.
func FreeVarMakeClosure(fn *ssa.Function) *ssa.MakeClosure {
	parent := fn.Parent()
	if parent == nil {
		return nil
	}
	for _, block := range parent.Blocks {
		for _, instr := range block.Instrs {
			if mc, ok := instr.(*ssa.MakeClosure); ok && mc.Fn == ssa.Value(fn) {
				return mc
			}
		}
	}
	return nil
}
