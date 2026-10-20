package callsite

import (
	"slices"

	"golang.org/x/tools/go/ssa"
)

// ClosureRun is an instruction where a closure value may run (see
// ClosureRuns).
type ClosureRun struct {
	Instr ssa.Instruction
	// Called is set when Instr is a plain call of the closure value that does
	// not also pass it on.
	Called bool
}

// ClosureRuns returns the instructions where v, a closure value (a
// MakeClosure, or a function literal with no free variables), may run: each
// use of it, such as a call, a go or defer statement, an argument or a store
// into a slice or a field. A comparison with nil is not one.
//
// The value is followed through a merge (Phi), a conversion to a named func
// type (ChangeType, #341), and through a local variable it is stored to: the
// variable's loads are followed, also in the closures that capture the
// variable at any depth, so a closure that another closure
// calls runs at that call (#305). This is the reverse of FuncVars.LocalValue,
// which goes from a call to the closure it runs. A store of another value to
// the variable is not a use, so a variable assigned twice is followed too.
//
// It is the one notion of where a closure runs, for the checker's ordering
// and reachability of a closure's uses (#68, #179), for the stores a captured
// variable is read at (#251), and for whether a closure runs more than once
// (#249).
func ClosureRuns(v ssa.Value) []ClosureRun {
	var runs []ClosureRun
	seen := map[ssa.Value]bool{v: true}
	work := []ssa.Value{v}
	follow := func(next ssa.Value) {
		if !seen[next] {
			seen[next] = true
			work = append(work, next)
		}
	}
	// followVar follows a variable, an Alloc or the FreeVar that a closure
	// reads it through, that holds the closure value.
	var followVar func(addr ssa.Value)
	followVar = func(addr ssa.Value) {
		if seen[addr] {
			return
		}
		seen[addr] = true
		for _, ref := range *addr.Referrers() {
			switch ref := ref.(type) {
			case *ssa.DebugRef:
			case *ssa.Store:
				if ref.Addr != addr {
					runs = append(runs, ClosureRun{Instr: ref})
				}
			case *ssa.UnOp:
				// A load: an address has no other unary operation.
				follow(ref)
			case *ssa.MakeClosure:
				closure, ok := ref.Fn.(*ssa.Function)
				for i, binding := range ref.Bindings {
					if ok && binding == addr {
						followVar(closure.FreeVars[i])
					}
				}
			default:
				runs = append(runs, ClosureRun{Instr: ref})
			}
		}
	}
	for len(work) > 0 {
		v := work[len(work)-1]
		work = work[:len(work)-1]
		refs := v.Referrers()
		if refs == nil {
			continue
		}
		for _, ref := range *refs {
			switch ref := ref.(type) {
			case *ssa.DebugRef, *ssa.BinOp:
				// A func value can only be compared with nil, as in
				// `if f != nil { f() }`, which does not run it (#309).
				continue
			case *ssa.Phi, *ssa.ChangeType:
				// A conversion to a named func type, as in
				// `h := handlerFunc(func() { ... })`, is the same closure (#341).
				follow(ref.(ssa.Value))
				continue
			case *ssa.Store:
				if alloc, ok := ref.Addr.(*ssa.Alloc); ok && ref.Val == v {
					followVar(alloc)
					continue
				}
			case *ssa.Call:
				if ref.Call.Value == v && !slices.Contains(ref.Call.Args, v) {
					runs = append(runs, ClosureRun{Instr: ref, Called: true})
					continue
				}
			}
			runs = append(runs, ClosureRun{Instr: ref})
		}
	}
	return runs
}

// ClosureCalls returns the plain calls of mc, in the function that creates
// it, or nil when mc may also run anywhere else: deferred, spawned, stored,
// passed on, or called in another closure. Only then are the calls all the
// places it runs. A call through a Phi or a local variable that may hold mc
// is one of its calls (see ClosureRuns).
func ClosureCalls(mc *ssa.MakeClosure) []*ssa.Call {
	var calls []*ssa.Call
	for _, run := range ClosureRuns(mc) {
		if !run.Called || run.Instr.Parent() != mc.Parent() {
			return nil
		}
		calls = append(calls, run.Instr.(*ssa.Call))
	}
	return calls
}
