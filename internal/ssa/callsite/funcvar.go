package callsite

import (
	"go/token"
	"slices"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/ssa/origin"
)

// FuncVars resolves calls through variables that always hold one function.
//
// A package-level variable holds one function when the package stores to it
// exactly once, the stored value is a function, its address is used only to
// load and store it, and it is unexported, such as
// `var helper = func(db *gorm.DB) { ... }` (#183). Any other variable may hold
// another function when it is called: a second assignment, a write through a
// pointer, or, for an exported one, an assignment in another package.
//
// A local variable holds one function under the same rules, read in its
// function and in every closure that captures it (#304, see LocalValue).
//
// A call through such a variable has no static callee, so without FuncVars
// it would carry none of the directives written on the function it holds.
// The zero FuncVars resolves no variable. A FuncVars is not safe for
// concurrent use: it remembers the local variables it has resolved.
type FuncVars struct {
	table *funcVarTable
}

// funcVarTable is what a FuncVars knows.
type funcVarTable struct {
	globals map[*ssa.Global]*ssa.Function
	// locals remembers funcVarSoleStore for each local variable resolved so
	// far, so that a variable read at n calls is scanned once, not n times.
	locals map[*ssa.Alloc]ssa.Value
}

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

	table := &funcVarTable{globals: make(map[*ssa.Global]*ssa.Function), locals: make(map[*ssa.Alloc]ssa.Value)}
	for g, vals := range stored {
		if escaped[g] || len(vals) != 1 || g.Object() == nil || g.Object().Exported() {
			continue
		}
		if fn, ok := vals[0].(*ssa.Function); ok {
			table.globals[g] = fn
		}
	}
	return FuncVars{table: table}
}

// Held returns the function that the package-level variable g always holds,
// or nil when g is not such a variable.
func (vars FuncVars) Held(g *ssa.Global) *ssa.Function {
	if vars.table == nil {
		return nil
	}
	return vars.table.globals[g]
}

// LocalValue returns the value that a call through v runs (#304). When v
// loads a local variable assigned exactly once, it is the value stored there,
// followed again when that is a load of another such variable. Otherwise, and
// for the zero FuncVars, it is v. A local function variable that a closure
// captures, such as one the closure calls or a recursive closure, is an Alloc
// in SSA, and the closure reads it through a FreeVar, so without this every
// call of it would have no callee.
//
// The variable may be read before its store runs, but then it holds nil and
// the call panics, so every call that runs, runs the stored value. A store in
// a loop stores a fresh closure of the same function on each iteration.
func (vars FuncVars) LocalValue(v ssa.Value) ssa.Value {
	if vars.table == nil {
		return v
	}
	var seen []*ssa.Alloc
	for {
		load, ok := v.(*ssa.UnOp)
		if !ok || load.Op != token.MUL {
			return v
		}
		addr := load.X
		for {
			fv, ok := addr.(*ssa.FreeVar)
			if !ok {
				break
			}
			addr = FreeVarBinding(fv)
		}
		alloc, ok := addr.(*ssa.Alloc)
		if !ok || slices.Contains(seen, alloc) {
			return v
		}
		seen = append(seen, alloc)
		stored, known := vars.table.locals[alloc]
		if !known {
			stored = funcVarSoleStore(alloc)
			vars.table.locals[alloc] = stored
		}
		if stored == nil {
			return v
		}
		v = stored
	}
}

// globalFuncVar returns the function held by the package-level variable that
// v loads, or nil when v is no such load.
//
//declscope:shared // SiteOf in site.go reads Site.Callee from it
func (vars FuncVars) globalFuncVar(v ssa.Value) *ssa.Function {
	if load, ok := v.(*ssa.UnOp); ok && load.Op == token.MUL {
		if g, ok := load.X.(*ssa.Global); ok {
			return vars.Held(g)
		}
	}
	return nil
}

// funcVarSoleStore returns the only value ever stored into the local alloc,
// or nil when it is assigned more than once, assigned by a closure that
// captures it, or its address is used another way than to load and store it,
// in its function or in a closure that captures it at any depth.
func funcVarSoleStore(alloc *ssa.Alloc) ssa.Value {
	var val ssa.Value
	var visit func(addr ssa.Value) bool
	visit = func(addr ssa.Value) bool {
		for _, ref := range *addr.Referrers() {
			switch r := ref.(type) {
			case *ssa.Store:
				// A store in a closure may run at any point.
				if r.Addr != addr || addr != ssa.Value(alloc) || val != nil {
					return false
				}
				val = r.Val
			case *ssa.MakeClosure:
				closure := r.Fn.(*ssa.Function)
				for i, binding := range r.Bindings {
					if binding == addr && !visit(closure.FreeVars[i]) {
						return false
					}
				}
			case *ssa.UnOp, *ssa.DebugRef:
				// A load: an address has no other unary operation.
			default:
				return false
			}
		}
		return true
	}
	if !visit(alloc) {
		return nil
	}
	return val
}

// varOf returns the package-level variable that holds fn, or nil when no
// variable in vars holds it. A function literal is stored in at most one
// variable, since a second variable initialized from the first stores a load,
// not the function.
func (vars FuncVars) varOf(fn *ssa.Function) *ssa.Global {
	if vars.table == nil {
		return nil
	}
	for g, held := range vars.table.globals {
		if held == fn && fn.Object() == nil {
			return g
		}
	}
	return nil
}

// FuncName names fn in a diagnostic as the source spells it (#259): the
// variable that holds a package-level closure instead of "init$1", the
// declared method of a method value or method expression wrapper instead of
// "m$bound" or "m$thunk", and the generic function of an instance instead of
// "f[int]". Every diagnostic that names a called function goes through it.
func (vars FuncVars) FuncName(fn *ssa.Function) string {
	fn = origin.Declared(fn)
	if g := vars.varOf(fn); g != nil {
		return g.Name()
	}
	if obj := fn.Object(); obj != nil && fn.Synthetic != "" {
		return obj.Name()
	}
	return fn.Name()
}
