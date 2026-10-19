package callback

import (
	"go/types"
	"slices"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/directive"
	"github.com/mpyw/gormreuse/internal/ssa/callsite"
)

// DropReachableCallbacks removes from set every callback whose *gorm.DB
// parameter can receive something other than the fresh value of an immutable
// callback position (#191). set is keyed by the declared function, so without
// this a named function passed to Transaction once would be exempt for every
// caller, including a direct call with a mid-chain value.
//
// A function stays in set only when every reference to it in funcs (and in the
// package initializers) is an argument in an immutable callback position: a
// Transaction/FindInBatches callback or an immutable-input(name) argument,
// directly or as a closure or method value. It is dropped when:
//   - any other reference exists: a direct call (also under go and defer, and
//     recursion), a method expression, a store, a Phi, a capture, a return
//   - it is exported, so another package may call it directly (#195)
//   - it is a method and an interface call, method value or method expression
//     in the package names a method of the same name, which may dispatch to it
func DropReachableCallbacks(funcs []*ssa.Function, inputs *directive.ImmutableInputSet, set map[*ssa.Function]bool) {
	for fn := range set {
		if obj := fn.Object(); obj != nil && obj.Exported() {
			delete(set, fn)
		}
	}
	if len(set) == 0 {
		return
	}
	scan := slices.Clone(funcs)
	seenPkg := make(map[*ssa.Package]bool)
	for _, fn := range funcs {
		if fn.Pkg != nil && !seenPkg[fn.Pkg] {
			seenPkg[fn.Pkg] = true
			if init := fn.Pkg.Func("init"); init != nil {
				scan = append(scan, init)
			}
		}
	}
	var invoked []string
	callsite.WalkInstrs(scan, func(instr ssa.Instruction) {
		if call, ok := instr.(ssa.CallInstruction); ok && call.Common().IsInvoke() {
			invoked = append(invoked, call.Common().Method.Name())
		}
		for _, op := range instr.Operands(nil) {
			ref, ok := (*op).(*ssa.Function)
			if !ok {
				continue
			}
			if name, ok := reachedInterfaceMethod(ref); ok {
				invoked = append(invoked, name)
			}
			if fn := directive.DeclaredFunc(ref); set[fn] && !refReachesOnlyCallback(instr, ref, inputs) {
				delete(set, fn)
			}
		}
	})
	for fn := range set {
		if fn.Signature.Recv() != nil && slices.Contains(invoked, fn.Name()) {
			delete(set, fn)
		}
	}
}

// reachedInterfaceMethod returns the method name of a wrapper that calls an
// interface method: a method value (i.m) or method expression (I.m) of an
// interface. Its dispatch happens inside the wrapper, so no invoke call in the
// package shows it.
func reachedInterfaceMethod(fn *ssa.Function) (string, bool) {
	obj, ok := fn.Object().(*types.Func)
	if !ok {
		return "", false
	}
	recv := obj.Signature().Recv()
	if recv == nil || !types.IsInterface(recv.Type()) {
		return "", false
	}
	return obj.Name(), true
}

// refReachesOnlyCallback reports whether instr uses v only to hand it to an
// immutable callback position: v is a call argument in such a position, or it
// is the function of a closure (a capturing literal or a bound method value)
// that is itself used only that way. Any other use, such as an interface
// conversion, a store or a Phi, is not: Transaction and immutable-input take a
// func-typed argument, so such a value never reaches one directly.
func refReachesOnlyCallback(instr ssa.Instruction, v ssa.Value, inputs *directive.ImmutableInputSet) bool {
	switch x := instr.(type) {
	case *ssa.MakeClosure:
		return x.Fn == v && closureReachesOnlyCallback(x, inputs)
	case ssa.CallInstruction:
		common := x.Common()
		if common.Value == v {
			return false
		}
		positions := CollectedCallbackArgs(callsite.SiteOf(x, nil).StaticSite(), inputs)
		for i, arg := range common.Args {
			if arg == v && !slices.Contains(positions, i) {
				return false
			}
		}
		return true
	}
	return false
}

// closureReachesOnlyCallback reports whether every use of the closure c hands
// it to an immutable callback position.
func closureReachesOnlyCallback(c *ssa.MakeClosure, inputs *directive.ImmutableInputSet) bool {
	for _, ref := range *c.Referrers() {
		if !refReachesOnlyCallback(ref, c, inputs) {
			return false
		}
	}
	return true
}
