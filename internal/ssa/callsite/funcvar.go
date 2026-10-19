package callsite

import (
	"go/token"

	"golang.org/x/tools/go/ssa"
)

// FuncVars maps each package-level variable that always holds one function to
// that function, such as `var helper = func(db *gorm.DB) { ... }` (#183). A
// call through such a variable has no static callee, so without this map it
// would carry none of the directives written on the closure.
//
// A variable is in the map only when the package stores to it exactly once, the
// stored value is a function, its address is used only to load and store it,
// and it is unexported. Any other variable may hold another function when it
// is called: a second assignment, a write through a pointer, or, for an
// exported one, an assignment in another package.
type FuncVars map[*ssa.Global]*ssa.Function

// CollectFuncVars builds the FuncVars of the package that funcs belong to.
// funcs must hold every function of the package with a body, including the
// package initializer, which runs package-level `var x = ...` initializers.
// Nested anonymous functions are visited too.
func CollectFuncVars(funcs []*ssa.Function) FuncVars {
	stored := make(map[*ssa.Global][]ssa.Value)
	escaped := make(map[*ssa.Global]bool)
	WalkInstrs(funcs, func(instr ssa.Instruction) {
		switch x := instr.(type) {
		case *ssa.Store:
			if g, ok := x.Addr.(*ssa.Global); ok {
				stored[g] = append(stored[g], x.Val)
			}
			if g, ok := x.Val.(*ssa.Global); ok {
				escaped[g] = true
			}
			return
		case *ssa.UnOp:
			if x.Op == token.MUL {
				return
			}
		case *ssa.DebugRef:
			return
		}
		for _, op := range instr.Operands(nil) {
			if g, ok := (*op).(*ssa.Global); ok {
				escaped[g] = true
			}
		}
	})

	vars := make(FuncVars)
	for g, vals := range stored {
		if escaped[g] || len(vals) != 1 || g.Object() == nil || g.Object().Exported() {
			continue
		}
		if fn, ok := vals[0].(*ssa.Function); ok {
			vars[g] = fn
		}
	}
	return vars
}

// callee returns the function that common calls: its static callee, or the
// function held by the package-level variable it loads. It returns nil when
// neither is known. vars may be nil.
//
//declscope:shared // SiteOf in site.go reads Site.Callee from it
func (vars FuncVars) callee(common *ssa.CallCommon) *ssa.Function {
	if fn := common.StaticCallee(); fn != nil {
		return fn
	}
	if load, ok := common.Value.(*ssa.UnOp); ok && load.Op == token.MUL {
		if g, ok := load.X.(*ssa.Global); ok {
			return vars[g]
		}
	}
	return nil
}
