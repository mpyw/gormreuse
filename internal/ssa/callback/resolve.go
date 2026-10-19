// Package callback finds the functions that gorm, or a function declaring
// //gormreuse:immutable-input, calls back with a *gorm.DB.
//
// It has three parts:
//   - resolve.go reads a func-typed value back to the functions it can be.
//   - collect.go finds the callbacks passed to Scopes/Preload,
//     Transaction/FindInBatches and immutable-input functions.
//   - reach.go drops the callbacks that something else can also call.
package callback

import (
	"go/token"
	"slices"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/ssa/callsite"
	"github.com/mpyw/gormreuse/internal/ssa/origin"
	"github.com/mpyw/gormreuse/internal/ssa/tracer"
)

// resolveMayFuncs returns the functions that the slices (or array pointers) vals
// may hold, as the slice handed to Scopes/Preload (#196). It follows:
//
//   - the varargs array (`Scopes(a, b)`) and slice literals
//   - `append` (`s = append(s, f)`; `Scopes(s...)`) and Phi edges
//   - local, captured and package-level variables, and struct fields
//   - method values (`Scopes(r.scope)`): both the $bound wrapper and the method
//
// It is a may-hold set: an unresolvable source (a parameter, an interface
// call) adds nothing. funcs are the functions whose stores into package-level
// variables count, with their package initializer.
//
//declscope:shared // collect.go resolves the Scopes/Preload callbacks with it
func resolveMayFuncs(funcs []*ssa.Function, vals []ssa.Value) map[*ssa.Function]bool {
	set := make(map[*ssa.Function]bool)
	r := &funcResolver{may: true, funcs: funcs, seen: make(map[ssa.Value]bool)}
	r.found = func(fn *ssa.Function) { set[fn] = true }
	for _, v := range vals {
		r.addSliceElems(v)
	}
	return set
}

// resolveExactFunc returns the function that the value v passed as a callback
// is: a MakeClosure (capturing closure), a bare *ssa.Function (named function
// reference), either boxed in an interface, or a read of a holder that holds
// one of those (#257). A generic instance or a bound method wrapper is mapped
// to the function whose body receives the argument (#180). It is nil for
// anything else, such as a parameter or a variable assigned twice. h may be
// nil.
//
//declscope:shared // collect.go reads the Transaction and immutable-input callbacks with it
func resolveExactFunc(v ssa.Value, h *holders) *ssa.Function {
	var found *ssa.Function
	r := &funcResolver{found: func(fn *ssa.Function) { found = fn }, held: h, seen: make(map[ssa.Value]bool)}
	r.addFunc(v)
	return found
}

// funcResolver walks a func-typed value back to the functions it can be, for
// both queries. The exact query follows only the steps that keep one
// function: a function, a closure, an interface conversion and a read of a
// holder.
type funcResolver struct {
	// may makes the walk follow every step of resolveMayFuncs.
	may bool
	// found receives each function the value can be.
	found func(*ssa.Function)
	// held is the holders the exact query reads through. It may be nil.
	held *holders
	// seen stops cycles.
	seen map[ssa.Value]bool
	// funcs and globalStores are used by the may query only.
	funcs        []*ssa.Function
	globalStores map[*ssa.Global][]ssa.Value // built on first use
}

// addSliceElems adds the functions that the slice (or array pointer) v may hold.
func (r *funcResolver) addSliceElems(v ssa.Value) {
	if v == nil || r.seen[v] {
		return
	}
	r.seen[v] = true

	// Element stores: `arr[i] = f` for varargs and literals, `s[i] = f` for make.
	if refs := v.Referrers(); refs != nil {
		for _, ref := range *refs {
			idx, ok := ref.(*ssa.IndexAddr)
			if !ok || idx.X != v || idx.Referrers() == nil {
				continue
			}
			for _, ir := range *idx.Referrers() {
				if store, ok := ir.(*ssa.Store); ok && store.Addr == idx {
					r.addFunc(store.Val)
				}
			}
		}
	}

	switch x := v.(type) {
	case *ssa.Slice:
		r.addSliceElems(x.X)
	case *ssa.Phi:
		for _, e := range x.Edges {
			r.addSliceElems(e)
		}
	case *ssa.Call:
		if b, ok := x.Call.Value.(*ssa.Builtin); ok && b.Name() == "append" {
			for _, a := range x.Call.Args {
				r.addSliceElems(a)
			}
		}
	case *ssa.UnOp:
		if x.Op == token.MUL {
			for _, sv := range r.storedValues(x.X) {
				r.addSliceElems(sv)
			}
		}
	case *ssa.FreeVar:
		r.addSliceElems(tracer.FreeVarBinding(x))
	case *ssa.ChangeType:
		r.addSliceElems(x.X)
	}
}

// addFunc adds the functions that the func value v may be.
func (r *funcResolver) addFunc(v ssa.Value) {
	if v == nil || r.seen[v] {
		return
	}
	r.seen[v] = true

	switch x := v.(type) {
	case *ssa.Function:
		r.found(origin.Declared(x))
		return
	case *ssa.MakeClosure:
		fn, ok := x.Fn.(*ssa.Function)
		if !ok {
			return
		}
		// A method value is a closure over a synthetic $bound wrapper; the
		// declared method receives the *gorm.DB, so it is a callback too
		// (#180). The may query keeps the wrapper as well.
		if r.may {
			r.found(fn)
		}
		r.found(origin.Declared(fn))
		return
	case *ssa.MakeInterface:
		r.addFunc(x.X)
		return
	}
	if !r.may {
		r.addFunc(r.held.heldValue(v))
		return
	}

	switch x := v.(type) {
	case *ssa.ChangeType:
		r.addFunc(x.X)
	case *ssa.Phi:
		for _, e := range x.Edges {
			r.addFunc(e)
		}
	case *ssa.UnOp:
		if x.Op == token.MUL {
			for _, sv := range r.storedValues(x.X) {
				r.addFunc(sv)
			}
		}
	case *ssa.FreeVar:
		r.addFunc(tracer.FreeVarBinding(x))
	}
}

// storedValues returns the values stored at addr: a local, a captured local, a
// struct field, or a package-level variable.
func (r *funcResolver) storedValues(addr ssa.Value) []ssa.Value {
	switch a := addr.(type) {
	case *ssa.Alloc:
		return tracer.AllocStoredValues(a)
	case *ssa.FieldAddr:
		// Every store, not only the ones that reach the load: a larger
		// callback set only makes more parameters mutable roots, so the
		// over-approximation stays on the reporting side.
		return tracer.FieldStoredValues(a, nil)
	case *ssa.FreeVar:
		return r.storedValues(tracer.FreeVarBinding(a))
	case *ssa.Global:
		return r.globalStoredValues(a)
	}
	return nil
}

// globalStoredValues returns the values stored into g by the analyzed functions
// and by the package initializer, which runs package-level `var x = ...`
// initializers but is not a source function.
func (r *funcResolver) globalStoredValues(g *ssa.Global) []ssa.Value {
	if r.globalStores == nil {
		r.globalStores = make(map[*ssa.Global][]ssa.Value)
		funcs := slices.Clone(r.funcs)
		if g.Pkg != nil {
			if init := g.Pkg.Func("init"); init != nil {
				funcs = append(funcs, init)
			}
		}
		callsite.WalkInstrs(funcs, func(instr ssa.Instruction) {
			if store, ok := instr.(*ssa.Store); ok {
				if sg, ok := store.Addr.(*ssa.Global); ok {
					r.globalStores[sg] = append(r.globalStores[sg], store.Val)
				}
			}
		})
	}
	return r.globalStores[g]
}
