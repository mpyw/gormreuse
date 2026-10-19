// Package purity validates that functions marked with //gormreuse:pure
// satisfy the pure function contract via SSA-based analysis.
//
// # Pure Contract
//
// A pure function must not pollute its *gorm.DB arguments. This means:
//   - No chain method calls (Where, Find, etc.) on parameter-derived values,
//     whether called directly, through go/defer, through a method value, or
//     inside a closure the function creates
//   - No passing parameter-derived values to non-pure functions, or to pure
//     functions that themselves fail this contract
//   - No letting parameter-derived values escape (channel send, slice/array or
//     map store, store to a package-level variable or through a pointer)
//
// # Analysis Strategy
//
// The validator uses derivation tracking to follow *gorm.DB values:
//  1. Mark every parameter whose type holds a *gorm.DB (*gorm.DB itself,
//     **gorm.DB, []*gorm.DB, a struct with a *gorm.DB field, ...) as
//     "parameter-derived"
//  2. Track derivation through Phi, Extract, Call results, loads, field and
//     element access, conversions, and stores into local variables, until a
//     fixed point (loops feed values back through Phi and Alloc)
//  3. Report violations when parameter-derived values are polluted or leak;
//     closures inherit the derivation of the values they capture
//
// # What Pure Functions CAN Do
//
//   - Call immutable-returning methods (Session, WithContext, Debug)
//   - Return a *gorm.DB derived from parameter (may be mutable). The
//     parameters a result may be are recorded (ReturnedParams), so callers
//     treat that result as the argument (#188)
//   - Pass *gorm.DB to other pure functions, and to read-only stdlib
//     formatting/logging functions (fmt, log, testing)
package purity

import (
	"go/token"
	"go/types"
	"slices"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/directive"
	"github.com/mpyw/gormreuse/internal/ssa/callsite"
	"github.com/mpyw/gormreuse/internal/ssa/pollutionsource"
	"github.com/mpyw/gormreuse/internal/ssa/tracer"
	"github.com/mpyw/gormreuse/internal/typeutil"
)

// =============================================================================
// Validator
// =============================================================================

// Validator validates pure function contracts.
type Validator struct {
	fn        *ssa.Function
	pureFuncs *directive.DirectiveFuncSet
	// immutableReturnFuncs lists the //gormreuse:immutable-return functions.
	// Like Session, their result is a fresh handle, never the argument. It may
	// be nil.
	immutableReturnFuncs *directive.DirectiveFuncSet
	// pure holds what is known so far about the other //gormreuse:pure
	// functions. Passing an argument to one that failed breaks the caller's
	// contract too, so failure propagates through pure wrappers. The result of
	// a trusted one is derived when it may be a derived argument. It may be nil.
	pure *tracer.PureVerdicts
	// funcVars resolves a call through a package-level variable that holds
	// one function, so a pure closure stored in one is trusted (#183). It may
	// be nil.
	funcVars     callsite.FuncVars
	paramDerived map[ssa.Value]bool
	holdsCache   map[types.Type]bool
}

// NewValidator creates a Validator for a function marked as pure.
func NewValidator(fn *ssa.Function, pureFuncs, immutableReturnFuncs *directive.DirectiveFuncSet, pure *tracer.PureVerdicts, funcVars callsite.FuncVars) *Validator {
	return &Validator{
		fn:                   fn,
		pureFuncs:            pureFuncs,
		immutableReturnFuncs: immutableReturnFuncs,
		pure:                 pure,
		funcVars:             funcVars,
		paramDerived:         make(map[ssa.Value]bool),
		holdsCache:           make(map[types.Type]bool),
	}
}

// Validate checks that the function satisfies the pure contract:
// - Does not pollute *gorm.DB arguments (no non-pure method calls on them)
// - Does not let them escape
//
// Note: Pure functions MAY return mutable *gorm.DB values. The "pure" contract only
// guarantees that the function doesn't pollute its arguments - callers must treat
// the return value as potentially mutable.
func (v *Validator) Validate() []Violation {
	fn := v.fn
	if fn == nil || fn.Blocks == nil {
		return nil
	}

	for _, i := range v.seededParams() {
		v.paramDerived[fn.Params[i]] = true
	}

	violations := v.validateBody(fn)

	// A call that passes several derived arguments reports each one at the
	// same position, so drop exact duplicates.
	seen := make(map[Violation]bool)
	return slices.DeleteFunc(violations, func(viol Violation) bool {
		if seen[viol] {
			return true
		}
		seen[viol] = true
		return false
	})
}

// ReturnedParams returns the indexes into fn.Params of the parameters the
// function's result may be, or be derived from without an immutable-returning
// call in between (#188). Callers trace such a result to the argument. Each
// parameter is followed on its own, so `func(a, b *gorm.DB) *gorm.DB { return
// a }` returns only a.
func (v *Validator) ReturnedParams() []int {
	if v.fn == nil || v.fn.Blocks == nil {
		return nil
	}
	var returned []int
	for _, i := range v.seededParams() {
		one := NewValidator(v.fn, v.pureFuncs, v.immutableReturnFuncs, v.pure, v.funcVars)
		one.paramDerived[v.fn.Params[i]] = true
		if one.returnsDerived(v.fn) {
			returned = append(returned, i)
		}
	}
	return returned
}

// seededParams returns the indexes of the parameters that hold a *gorm.DB. A
// method's receiver is not an argument: it usually holds the application's
// root handle (r.db), and the directive's signature check ignores it too.
func (v *Validator) seededParams() []int {
	first := 0
	if v.fn.Signature.Recv() != nil {
		first = 1
	}
	var seeded []int
	for i := first; i < len(v.fn.Params); i++ {
		if v.holdsGormDB(v.fn.Params[i].Type()) {
			seeded = append(seeded, i)
		}
	}
	return seeded
}

// validateBody propagates derivation through fn to a fixed point, then checks
// every instruction. Closures created from parameter-derived values are
// validated recursively with their captured variables seeded; each closure
// has exactly one MakeClosure, so no body is visited twice.
func (v *Validator) validateBody(fn *ssa.Function) []Violation {
	v.propagate(fn)

	var violations []Violation
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			switch i := instr.(type) {
			case ssa.CallInstruction:
				violations = append(violations, v.checkCallPollution(i)...)
			case *ssa.MakeClosure:
				violations = append(violations, v.checkClosure(i)...)
			case *ssa.Store:
				violations = append(violations, v.checkStoreEscape(i)...)
			}

			// Non-call leaks (channel send, slice/array store, map store).
			// These share pollutionsource.Leak with the main handler so the
			// two cannot disagree on what escapes a value (issue #66).
			violations = append(violations, v.checkLeak(instr)...)
		}
	}

	return violations
}

// =============================================================================
// Derivation Tracking
// =============================================================================

// propagate marks the values of fn derived from parameter-derived values,
// repeating until nothing changes: loops feed values back through Phi edges
// and local variables, so a single forward pass can miss them.
func (v *Validator) propagate(fn *ssa.Function) {
	for changed := true; changed; {
		changed = false
		for _, block := range fn.Blocks {
			for _, instr := range block.Instrs {
				if v.trackDerivation(instr) {
					changed = true
				}
			}
		}
	}
}

// returnsDerived propagates derivation through fn and reports whether any of
// its results may be parameter-derived.
func (v *Validator) returnsDerived(fn *ssa.Function) bool {
	v.propagate(fn)
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			if ret, ok := instr.(*ssa.Return); ok && slices.ContainsFunc(ret.Results, v.isDerivedArg) {
				return true
			}
		}
	}
	return false
}

// trackDerivation marks the values instr derives from parameter-derived
// values. It reports whether it marked anything new.
func (v *Validator) trackDerivation(instr ssa.Instruction) bool {
	switch i := instr.(type) {
	case *ssa.Phi:
		if slices.ContainsFunc(i.Edges, func(edge ssa.Value) bool { return v.paramDerived[edge] }) {
			return v.markDerived(i)
		}

	case *ssa.Call:
		if v.callResultDerived(i) {
			return v.markHolding(i)
		}

	case *ssa.Extract:
		if v.paramDerived[i.Tuple] {
			return v.markHolding(i)
		}

	case *ssa.Store:
		// Storing into a local variable (or a field/element of one) makes the
		// variable derived, so later loads from it are too (p := &db; *p,
		// h := holder{db: db}; h.db).
		if !v.paramDerived[i.Val] {
			return false
		}
		if local := v.localAllocRoot(i.Addr); local != nil {
			return v.markDerived(local)
		}

	case *ssa.Range:
		if v.paramDerived[i.X] {
			return v.markDerived(i)
		}

	case ssa.Value:
		if x := v.derivationOperand(i); x != nil && v.paramDerived[x] {
			return v.markHolding(i)
		}
	}
	return false
}

// derivationOperand returns the operand a value is read from when the value
// carries what the operand holds: a load (*p) or channel receive (<-ch), a
// field or element (address), a map lookup, a slice, a type change, or the
// next range element. A Convert is not listed: it only converts to basic types
// and unsafe.Pointer, which cannot hold a *gorm.DB. It returns nil for any other value.
func (v *Validator) derivationOperand(val ssa.Value) ssa.Value {
	switch i := val.(type) {
	case *ssa.UnOp:
		return i.X
	case *ssa.FieldAddr:
		return i.X
	case *ssa.Field:
		return i.X
	case *ssa.IndexAddr:
		return i.X
	case *ssa.Index:
		return i.X
	case *ssa.Lookup:
		return i.X
	case *ssa.Slice:
		return i.X
	case *ssa.ChangeType:
		return i.X
	case *ssa.SliceToArrayPointer:
		return i.X
	case *ssa.Next:
		return i.Iter
	}
	return nil
}

// markDerived marks val as parameter-derived and reports whether it was new.
func (v *Validator) markDerived(val ssa.Value) bool {
	if v.paramDerived[val] {
		return false
	}
	v.paramDerived[val] = true
	return true
}

// markHolding marks val as parameter-derived when its type can hold a
// *gorm.DB; an int read from a derived struct carries nothing to pollute.
func (v *Validator) markHolding(val ssa.Value) bool {
	if !v.holdsGormDB(val.Type()) {
		return false
	}
	return v.markDerived(val)
}

// callResultDerived reports whether a call's result may still share the
// argument's Statement: a non-immutable gorm method on a derived receiver, a
// closure that returns a derived value it captures, a trusted pure function
// that may return a derived argument, or any other call with a derived
// argument.
func (v *Validator) callResultDerived(call *ssa.Call) bool {
	// An interface method call never has a *gorm.DB receiver (it is a
	// concrete type), so it falls through to the regular-call rule below.

	// A gorm method, called directly or as a method value (f := db.Find; f(nil)).
	site := callsite.SiteOf(call, v.funcVars)
	if site.IsGormMethod() {
		return v.paramDerived[site.Recv] && !typeutil.IsImmutableReturningBuiltin(site.Method())
	}

	common, callee := &call.Call, site.Callee

	// Like Session, an immutable-return function hands back a fresh handle.
	if v.immutableReturnFuncs != nil && v.immutableReturnFuncs.Contains(callee) {
		return false
	}

	// A closure may return what it captures: func() *gorm.DB { return db }().
	if mc, ok := common.Value.(*ssa.MakeClosure); ok && v.seedClosure(mc) && v.returnsDerived(callee) {
		return true
	}

	// Regular function call
	if v.isTrustedPure(callee) {
		return slices.ContainsFunc(v.pure.ReturnedArgs(callee, common.Args), v.isDerivedArg)
	}
	return slices.ContainsFunc(common.Args, v.isDerivedArg)
}

// seedClosure marks the free variables of a closure whose bindings are
// derived, and reports whether it captures any derived value.
func (v *Validator) seedClosure(mc *ssa.MakeClosure) bool {
	// MakeClosure.Fn is always a *ssa.Function, with one FreeVar per binding.
	closure, _ := mc.Fn.(*ssa.Function)
	captured := false
	for i, binding := range mc.Bindings {
		if v.paramDerived[binding] {
			v.paramDerived[closure.FreeVars[i]] = true
			captured = true
		}
	}
	return captured
}

// =============================================================================
// Pollution Checking
// =============================================================================

// checkCallPollution checks a call, go or defer statement.
func (v *Validator) checkCallPollution(call ssa.CallInstruction) []Violation {
	// Interface method calls (common.Method != nil) on *gorm.DB are not
	// possible: IsGormDB only matches the concrete type, and GORM's DB is a
	// struct, so all method calls have a static callee.
	site, pos := callsite.SiteOf(call, v.funcVars), call.Pos()

	if site.IsGormMethod() {
		// Calling a method value is checked where the method value is
		// created (checkClosure), so f := db.Find; f(nil) is reported once.
		if site.Kind == callsite.SiteBound {
			return nil
		}
		// gorm chain method on a param-derived receiver pollutes the argument.
		if v.paramDerived[site.Recv] && !typeutil.IsImmutableReturningBuiltin(site.Method()) {
			return []Violation{{
				Pos:        pos,
				Message:    "pure function pollutes *gorm.DB argument by calling " + site.Method(),
				Definitive: true,
			}}
		}
		return nil
	}
	// A wrapper such as sql.Named("s", sub) whose result is only a read-only
	// argument hands sub to the method that reads it, as the main handler
	// sees it (#253).
	if c, ok := call.(*ssa.Call); ok && pollutionsource.IsReadOnlyWrapperCall(c, site) {
		return nil
	}
	common, callee := call.Common(), site.Callee

	// Any other call — a static function, a builtin (e.g. append, which is how
	// slice storage lowers), or an indirect call through a func value — leaks a
	// param-derived *gorm.DB passed to it, unless the callee is itself pure.
	// callee is nil for builtins and indirect calls; those are never pure, so
	// they leak conservatively, matching the main handler.
	return v.checkFunctionCallPollution(common, callee, pos)
}

// checkClosure checks a MakeClosure: a method value on a derived receiver
// (f := db.Find) pollutes it, and a closure capturing derived values must obey
// the contract in its own body (IIFEs, go func() {...}(), stored closures).
func (v *Validator) checkClosure(mc *ssa.MakeClosure) []Violation {
	if site, ok := callsite.BoundSite(mc); ok && site.IsGormMethod() {
		if v.paramDerived[site.Recv] && !typeutil.IsImmutableReturningBuiltin(site.Method()) {
			return []Violation{{
				Pos:        mc.Pos(),
				Message:    "pure function pollutes *gorm.DB argument by taking method value " + site.Method(),
				Definitive: true,
			}}
		}
		return nil
	}

	if !v.seedClosure(mc) {
		return nil
	}
	closure, _ := mc.Fn.(*ssa.Function)
	return v.validateBody(closure)
}

// checkStoreEscape reports a param-derived value stored outside the
// function's own local variables: into a package-level variable, or through a
// pointer it did not allocate. Element stores are left to checkLeak.
func (v *Validator) checkStoreEscape(store *ssa.Store) []Violation {
	if !v.paramDerived[store.Val] || !v.holdsGormDB(store.Val.Type()) {
		return nil
	}
	if _, ok := store.Addr.(*ssa.IndexAddr); ok {
		return nil // slice/array store: pollutionsource.Leak reports it
	}
	root := v.addressRoot(store.Addr)
	if _, ok := root.(*ssa.Alloc); ok {
		return nil // a local variable: tracked by trackDerivation
	}

	via := "store through a pointer"
	if _, ok := root.(*ssa.Global); ok {
		via = "store to a package-level variable"
	}
	return []Violation{{
		Pos:        store.Pos(),
		Message:    "pure function leaks *gorm.DB argument via " + via,
		Definitive: true,
	}}
}

// checkLeak reports a contract violation when a param-derived *gorm.DB escapes
// via a non-call pollution source (channel send, slice/array store, map store).
func (v *Validator) checkLeak(instr ssa.Instruction) []Violation {
	val, kind := pollutionsource.Leak(instr, v.funcVars)
	if kind == pollutionsource.KindNone || !v.paramDerived[val] {
		return nil
	}

	var via string
	switch kind {
	case pollutionsource.KindChannelSend:
		via = "channel send"
	case pollutionsource.KindSliceStore:
		via = "slice/array store"
	case pollutionsource.KindMapStore:
		via = "map store"
	}
	return []Violation{{
		Pos:        instr.Pos(),
		Message:    "pure function leaks *gorm.DB argument via " + via,
		Definitive: true,
	}}
}

func (v *Validator) checkFunctionCallPollution(common *ssa.CallCommon, callee *ssa.Function, pos token.Pos) []Violation {
	// Pure callees don't pollute their arguments.
	if v.isTrustedPure(callee) {
		return nil
	}
	failed := callee != nil && v.pure.HasFailed(callee)

	var violations []Violation
	for _, arg := range v.callArgsToCheck(common) {
		if !v.isDerivedArg(arg) {
			continue
		}
		if failed {
			// The callee is marked pure but breaks the contract, so this
			// function breaks it too (issue #187).
			violations = append(violations, Violation{
				Pos:        pos,
				Message:    "pure function passes *gorm.DB argument to " + v.calleeName(common, callee) + ", which violates its pure contract",
				Definitive: true,
			})
			continue
		}
		violations = append(violations, Violation{
			Pos:     pos,
			Message: "pure function passes *gorm.DB argument to non-pure function " + v.calleeName(common, callee),
		})
	}
	return violations
}

// callArgsToCheck returns the arguments through which a call can receive a
// derived value. Builtins only take one when they copy elements (append, copy)
// or are handed a *gorm.DB itself; len(dbs) or cap(dbs) cannot pollute.
func (v *Validator) callArgsToCheck(common *ssa.CallCommon) []ssa.Value {
	b, ok := common.Value.(*ssa.Builtin)
	if !ok {
		return common.Args
	}
	switch b.Name() {
	case "append", "copy":
		return common.Args
	}
	return slices.DeleteFunc(slices.Clone(common.Args), func(arg ssa.Value) bool {
		_, isGorm := pollutionsource.UnwrapGormDB(arg)
		return !isGorm
	})
}

// isDerivedArg reports whether arg passes a parameter-derived value: a
// *gorm.DB (possibly boxed in an interface, e.g. for a ...any parameter) or a
// value that holds one (a **gorm.DB, a slice, a struct).
func (v *Validator) isDerivedArg(arg ssa.Value) bool {
	if gormArg, ok := pollutionsource.UnwrapGormDB(arg); ok {
		return v.paramDerived[gormArg]
	}
	return v.paramDerived[arg] && v.holdsGormDB(arg.Type())
}

// isTrustedPure reports whether callee is marked pure and has not failed
// validation.
func (v *Validator) isTrustedPure(callee *ssa.Function) bool {
	return callee != nil && v.pureFuncs.Contains(callee) && !v.pure.HasFailed(callee)
}

// calleeName returns a human-readable name for a call's target, used in the
// Validator's violation messages: the function name for static calls, the
// builtin name for builtins (append, etc.), or a generic label for indirect
// calls through a func value.
func (v *Validator) calleeName(common *ssa.CallCommon, callee *ssa.Function) string {
	if callee != nil {
		return v.funcVars.FuncName(callee)
	}
	if b, ok := common.Value.(*ssa.Builtin); ok {
		return b.Name()
	}
	return "(closure)"
}

// holdsGormDB reports whether a value of type t can reach a *gorm.DB without
// going through an interface: *gorm.DB itself, a pointer to one, or a
// struct, slice, array, map, channel or tuple containing one.
//
//declscope:shared // immutable_input.go classifies callback arguments with it
func (v *Validator) holdsGormDB(t types.Type) bool {
	if result, ok := v.holdsCache[t]; ok {
		return result
	}
	v.holdsCache[t] = false // cycle guard for recursive types
	if typeutil.IsGormDB(t) {
		v.holdsCache[t] = true
		return true
	}

	result := false
	switch typ := t.Underlying().(type) {
	case *types.Pointer:
		result = v.holdsGormDB(typ.Elem())
	case *types.Slice:
		result = v.holdsGormDB(typ.Elem())
	case *types.Array:
		result = v.holdsGormDB(typ.Elem())
	case *types.Chan:
		result = v.holdsGormDB(typ.Elem())
	case *types.Map:
		result = v.holdsGormDB(typ.Key()) || v.holdsGormDB(typ.Elem())
	case *types.Struct:
		for field := range typ.Fields() {
			if v.holdsGormDB(field.Type()) {
				result = true
				break
			}
		}
	case *types.Tuple:
		for elem := range typ.Variables() {
			if v.holdsGormDB(elem.Type()) {
				result = true
				break
			}
		}
	}
	v.holdsCache[t] = result
	return result
}

// addressRoot walks field and element addresses back to the base address.
func (v *Validator) addressRoot(addr ssa.Value) ssa.Value {
	for {
		switch a := addr.(type) {
		case *ssa.FieldAddr:
			addr = a.X
		case *ssa.IndexAddr:
			// Only an array's element address belongs to the same variable;
			// a slice element lives in memory the slice points to.
			if _, isArrayPtr := a.X.Type().Underlying().(*types.Pointer); !isArrayPtr {
				return a
			}
			addr = a.X
		default:
			return addr
		}
	}
}

// localAllocRoot returns the local variable addr points into, or nil.
func (v *Validator) localAllocRoot(addr ssa.Value) *ssa.Alloc {
	alloc, _ := v.addressRoot(addr).(*ssa.Alloc)
	return alloc
}
