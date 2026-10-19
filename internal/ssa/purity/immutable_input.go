package purity

import (
	"fmt"
	"go/token"
	"go/types"
	"slices"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/directive"
	"github.com/mpyw/gormreuse/internal/ssa/tracer"
	"github.com/mpyw/gormreuse/internal/typeutil"
)

// ValidateImmutableInputs enforces the body-side contract of
// //gormreuse:immutable-input(name): whenever the declaring function calls the
// named callback parameter, every *gorm.DB argument must be an immutable source
// (a Session()/WithContext()/Debug()/Begin result, an immutable-return user
// function result, an immutable-param'd value, or nil). Passing a mutable
// (clone==0) value violates the declaration.
//
// Issue #56/#62 cases 2.3 (declaration violated) and 2.4 (correct usage).
//
// The callback is followed through every flow the function keeps local (#193):
// go and defer, local variables and their address, struct fields, slices,
// arrays, maps, append, interfaces, and closures that capture it. Each call it reaches is
// checked. Handing it to another immutable-input function at its declared
// callback, or to gorm's Transaction/FindInBatches, keeps the promise. Any
// other flow (passed to a function, returned, stored outside the function,
// sent on a channel) is reported as an escape, since the
// calls it reaches cannot be checked.
//
// rt must be a tracer configured with the pass's full context so FindMutableRoot
// classifies immutable sources correctly.
func ValidateImmutableInputs(fn *ssa.Function, set *directive.ImmutableInputSet, rt *tracer.RootTracer) []Violation {
	if fn == nil || set == nil || rt == nil {
		return nil
	}
	callbacks := set.Callbacks(fn)
	if len(callbacks) == 0 {
		return nil
	}

	// ssa.Function.Params lists the receiver first for methods, while
	// ImmutableInputCallback.ParamIdx counts from the first non-receiver
	// parameter, so shift by one when the function has a receiver.
	hasReceiver := fn.Signature != nil && fn.Signature.Recv() != nil
	var violations []Violation
	for _, cb := range callbacks {
		idx := cb.ParamIdx
		if hasReceiver {
			idx++
		}
		if idx < 0 || idx >= len(fn.Params) {
			continue
		}
		flow := &immutableInputFlow{
			set:       set,
			rt:        rt,
			name:      cb.ParamName,
			param:     fn.Params[idx],
			derived:   make(map[ssa.Value]bool),
			funcCache: make(map[types.Type]bool),
			gorm:      NewValidator(fn, nil, nil, nil),
		}
		violations = append(violations, flow.check()...)
	}

	// A call that passes the callback twice reports at the same position.
	seen := make(map[Violation]bool)
	return slices.DeleteFunc(violations, func(viol Violation) bool {
		if seen[viol] {
			return true
		}
		seen[viol] = true
		return false
	})
}

// immutableInputFlow follows one immutable-input callback parameter through
// the function and the closures that capture it.
type immutableInputFlow struct {
	set   *directive.ImmutableInputSet
	rt    *tracer.RootTracer
	name  string
	param *ssa.Parameter

	// derived holds the values that may be the callback, or hold it.
	derived   map[ssa.Value]bool
	worklist  []ssa.Value
	funcCache map[types.Type]bool
	// gorm classifies the types of callback arguments.
	gorm *Validator

	violations []Violation
}

// check follows the callback from its parameter and returns the violations.
func (f *immutableInputFlow) check() []Violation {
	f.addImmutableInputValue(f.param)
	for len(f.worklist) > 0 {
		v := f.worklist[len(f.worklist)-1]
		f.worklist = f.worklist[:len(f.worklist)-1]
		// Every value followed here is a parameter, free variable or
		// instruction, so it has referrers.
		for _, instr := range *v.Referrers() {
			f.visitImmutableInputUse(v, instr)
		}
	}
	return f.violations
}

// addImmutableInputValue marks v as holding the callback. A value whose type
// cannot hold a function (an int field of a struct that holds the callback)
// is skipped.
func (f *immutableInputFlow) addImmutableInputValue(v ssa.Value) {
	// A map iterator has an opaque type, so it is followed whatever it holds.
	_, isRange := v.(*ssa.Range)
	if f.derived[v] || (!isRange && !f.holdsImmutableInputFunc(v.Type())) {
		return
	}
	f.derived[v] = true
	f.worklist = append(f.worklist, v)
}

// visitImmutableInputUse handles one use of v, a value holding the callback.
func (f *immutableInputFlow) visitImmutableInputUse(v ssa.Value, instr ssa.Instruction) {
	switch i := instr.(type) {
	case *ssa.DebugRef, *ssa.BinOp:
		// A comparison (cb == nil) cannot call the callback.
	case ssa.CallInstruction:
		f.visitImmutableInputCall(v, i)
	case *ssa.Store:
		if i.Val != v {
			return // storing into a variable that holds the callback
		}
		if root := immutableInputLocalRoot(i.Addr); root != nil {
			f.addImmutableInputValue(root)
			return
		}
		f.escapeImmutableInput(i.Pos(), "stored outside the function")
	case *ssa.MapUpdate:
		if i.Map == v {
			return
		}
		if mm, ok := i.Map.(*ssa.MakeMap); ok {
			f.addImmutableInputValue(mm)
			return
		}
		f.escapeImmutableInput(i.Pos(), "stored outside the function")
	case *ssa.MakeClosure:
		// The closure's body is visible, so follow the captured callback into
		// it. The closure value itself does not need to be followed.
		closure, _ := i.Fn.(*ssa.Function)
		for idx, binding := range i.Bindings {
			if binding == v {
				f.addImmutableInputValue(closure.FreeVars[idx])
			}
		}
	case *ssa.Phi, *ssa.UnOp, *ssa.FieldAddr, *ssa.Field, *ssa.IndexAddr, *ssa.Index,
		*ssa.Lookup, *ssa.Slice, *ssa.ChangeType, *ssa.Extract, *ssa.Range, *ssa.Next,
		*ssa.MakeInterface, *ssa.TypeAssert:
		// A boxed callback is followed too, so the flow that lets it escape is
		// reported where it happens (an implicit conversion has no position).

		f.addImmutableInputValue(i.(ssa.Value))
	case *ssa.Return:
		f.escapeImmutableInput(i.Pos(), "returned")
	case *ssa.Send:
		f.escapeImmutableInput(i.Pos(), "sent on a channel")
	default:
		f.escapeImmutableInput(instr.Pos(), "used in a way the checker does not follow")
	}
}

// visitImmutableInputCall handles a call, go or defer statement that uses v:
// as the function it calls, as an argument, or both.
func (f *immutableInputFlow) visitImmutableInputCall(v ssa.Value, call ssa.CallInstruction) {
	common := call.Common()
	if !common.IsInvoke() && common.Value == v {
		f.checkImmutableInputArgs(common, call.Pos())
	}
	for idx, arg := range common.Args {
		if arg != v {
			continue
		}
		if b, ok := common.Value.(*ssa.Builtin); ok {
			switch b.Name() {
			case "append":
				if result, isCall := call.(*ssa.Call); isCall {
					f.addImmutableInputValue(result)
					continue
				}
			case "len", "cap":
				continue
			}
		}
		if f.keepsImmutableInputPromise(common, idx) {
			continue
		}
		f.escapeImmutableInput(call.Pos(), "passed to "+immutableInputCalleeName(common))
	}
}

// keepsImmutableInputPromise reports whether a call that receives the callback
// as argument idx promises an immutable *gorm.DB to it too: gorm's
// Transaction or FindInBatches, or a function declaring immutable-input for
// that parameter.
func (f *immutableInputFlow) keepsImmutableInputPromise(common *ssa.CallCommon, idx int) bool {
	callee := common.StaticCallee()
	if callee == nil {
		return false
	}
	if tracer.IsImmutableCallbackMethod(callee) {
		return true
	}
	// A method call carries its receiver as Args[0]; ParamIdx counts from the
	// first non-receiver parameter.
	if callee.Signature.Recv() != nil {
		idx--
	}
	return slices.ContainsFunc(f.set.Callbacks(callee), func(cb directive.ImmutableInputCallback) bool {
		return cb.ParamIdx == idx
	})
}

// checkImmutableInputArgs checks the arguments of one call of the callback.
// It reports once per call: a mutable *gorm.DB first, else an argument whose
// *gorm.DB cannot be traced.
func (f *immutableInputFlow) checkImmutableInputArgs(common *ssa.CallCommon, pos token.Pos) {
	unverifiable := false
	for _, arg := range common.Args {
		mutable, known := f.immutableInputArgMutable(arg)
		if mutable {
			f.violations = append(f.violations, Violation{
				Pos:     pos,
				Message: fmt.Sprintf("immutable-input(%s) declared but mutable *gorm.DB passed to callback", f.name),
			})
			return
		}
		unverifiable = unverifiable || !known
	}
	if unverifiable {
		f.violations = append(f.violations, Violation{
			Pos:     pos,
			Message: fmt.Sprintf("immutable-input(%s) declared but cannot verify the *gorm.DB passed to callback", f.name),
		})
	}
}

// immutableInputArgMutable classifies one callback argument. A *gorm.DB is
// traced to its root. A pointer to a local *gorm.DB variable (&q) is mutable
// when any value stored into the variable is. Any other value that holds a
// *gorm.DB (a pointer from elsewhere, a struct, a slice) cannot be traced, so
// known is false.
func (f *immutableInputFlow) immutableInputArgMutable(arg ssa.Value) (mutable, known bool) {
	if _, ok := arg.(*ssa.Const); ok {
		return false, true // nil
	}
	if typeutil.IsGormDB(arg.Type()) {
		return f.rt.FindMutableRoot(arg, nil) != nil, true
	}
	if !f.gorm.holdsGormDB(arg.Type()) {
		return false, true
	}
	alloc, ok := arg.(*ssa.Alloc)
	if !ok || !typeutil.IsGormDB(alloc.Type().(*types.Pointer).Elem()) {
		return false, false
	}
	return slices.ContainsFunc(tracer.AllocStoredValues(alloc), func(val ssa.Value) bool {
		return f.rt.FindMutableRoot(val, nil) != nil
	}), true
}

// escapeImmutableInput reports a flow of the callback that the checker cannot
// follow. A position-less instruction (an implicit conversion) is reported
// at the callback parameter.
func (f *immutableInputFlow) escapeImmutableInput(pos token.Pos, how string) {
	if !pos.IsValid() {
		pos = f.param.Pos()
	}
	f.violations = append(f.violations, Violation{
		Pos:     pos,
		Message: fmt.Sprintf("immutable-input(%s) declared but callback %s escapes (%s); cannot verify the *gorm.DB it receives", f.name, f.name, how),
	})
}

// holdsImmutableInputFunc reports whether a value of type t can hold a
// function: a function itself, or a pointer, struct, slice, array, map,
// channel or tuple containing one. An interface counts too, since
// the callback may be boxed in one.
func (f *immutableInputFlow) holdsImmutableInputFunc(t types.Type) bool {
	if result, ok := f.funcCache[t]; ok {
		return result
	}
	f.funcCache[t] = false // cycle guard for recursive types
	result := false
	switch typ := t.Underlying().(type) {
	case *types.Signature, *types.Interface:
		// An interface may hold the callback once it is boxed.
		result = true
	case *types.Pointer:
		result = f.holdsImmutableInputFunc(typ.Elem())
	case *types.Slice:
		result = f.holdsImmutableInputFunc(typ.Elem())
	case *types.Array:
		result = f.holdsImmutableInputFunc(typ.Elem())
	case *types.Chan:
		result = f.holdsImmutableInputFunc(typ.Elem())
	case *types.Map:
		result = f.holdsImmutableInputFunc(typ.Elem())
	case *types.Struct:
		for field := range typ.Fields() {
			if f.holdsImmutableInputFunc(field.Type()) {
				result = true
				break
			}
		}
	case *types.Tuple:
		for elem := range typ.Variables() {
			if f.holdsImmutableInputFunc(elem.Type()) {
				result = true
				break
			}
		}
	}
	f.funcCache[t] = result
	return result
}

// immutableInputLocalRoot returns the variable or container created by the
// function that addr points into, or nil. Every later use of it is a referrer
// the flow follows, so a callback stored there is still tracked.
func immutableInputLocalRoot(addr ssa.Value) ssa.Value {
	for {
		switch a := addr.(type) {
		case *ssa.FieldAddr:
			addr = a.X
		case *ssa.IndexAddr:
			addr = a.X
		case *ssa.Slice:
			addr = a.X
		case *ssa.Alloc, *ssa.MakeSlice:
			return a
		default:
			return nil
		}
	}
}

// immutableInputCalleeName names a call's target for an escape report.
func immutableInputCalleeName(common *ssa.CallCommon) string {
	if callee := common.StaticCallee(); callee != nil {
		return callee.Name()
	}
	if common.IsInvoke() {
		return common.Method.Name()
	}
	return "a function value"
}
