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
//     map store, store to a package-level variable or through a pointer, or
//     a return inside a struct, a pointer, an interface or a closure)
//
// # Analysis Strategy
//
// The validator uses derivation tracking to follow *gorm.DB values:
//  1. Mark every parameter whose type holds a *gorm.DB (*gorm.DB itself,
//     **gorm.DB, []*gorm.DB, a struct with a *gorm.DB field, ...) as
//     "parameter-derived"
//  2. Track derivation through Phi, Extract, Call results, loads, field and
//     element access, conversions, interfaces the function boxes them in and
//     type assertions of them, and stores into local variables, until a
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
	"github.com/mpyw/gormreuse/internal/ssa/stmtwrite"
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
	violations = append(violations, v.checkDirectWrites(violations)...)

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
			case *ssa.Return:
				if fn == v.fn {
					violations = append(violations, v.checkWrappedReturn(i)...)
				}
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
			if _, ok := i.Tuple.(*ssa.TypeAssert); ok && types.IsInterface(i.Type()) {
				return v.markDerived(i)
			}
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

	case *ssa.MakeInterface:
		// An interface holds what it boxes. A type assertion or a method
		// call through it reaches the argument again (#376). Only these
		// interfaces are derived: one read from a field, such as
		// db.Statement.Context, holds no argument.
		if v.paramDerived[i.X] {
			return v.markDerived(i)
		}

	case *ssa.ChangeInterface:
		if v.paramDerived[i.X] {
			return v.markDerived(i)
		}

	case *ssa.TypeAssert:
		if v.paramDerived[i.X] {
			if types.IsInterface(i.AssertedType) {
				return v.markDerived(i)
			}
			return v.markHolding(i)
		}

	case *ssa.MakeClosure:
		// A closure holds what it captures. Stored in a variable that
		// another closure captures, it carries the derived bindings there,
		// so a call of it in that closure is seen (#308).
		if slices.ContainsFunc(i.Bindings, func(b ssa.Value) bool { return v.paramDerived[b] }) {
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
	// An interface method call on a derived interface may return what its
	// dynamic value holds: i.Where("x") on one that boxes db (#376).
	if common := &call.Call; common.IsInvoke() && v.paramDerived[common.Value] {
		return !typeutil.IsImmutableReturningBuiltin(common.Method.Name())
	}

	// A gorm method, called directly or as a method value (f := db.Find; f(nil)).
	site := callsite.SiteOf(call, v.funcVars)
	if site.IsGormMethod() {
		return v.paramDerived[site.Recv] && !typeutil.IsImmutableReturningBuiltin(site.Method())
	}

	common, callee := &call.Call, site.Callee

	// Like Session, an immutable-return function hands back a fresh handle,
	// except that an inferred getter returns what its argument holds (#342):
	// h := &Holder{db: db}; h.DB() is db.
	if v.immutableReturnFuncs != nil && v.immutableReturnFuncs.Contains(callee) {
		return slices.ContainsFunc(v.immutableReturnFuncs.ParamReads(callee), func(read directive.ParamRead) bool {
			arg := site.ParamArg(read.Param)
			return arg != nil && v.isDerivedArg(arg)
		})
	}

	// A closure may return what it captures: func() *gorm.DB { return db }(),
	// also when called through a captured variable that holds it (#308).
	if mc, ok := v.funcVars.LocalValue(common.Value).(*ssa.MakeClosure); ok && v.seedClosure(mc) && v.returnsDerived(callee) {
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
	// A *gorm.DB method called directly has a static callee. One called
	// through an interface that boxes the argument is an invoke on a derived
	// interface.
	site, pos := callsite.SiteOf(call, v.funcVars), call.Pos()

	if common := call.Common(); common.IsInvoke() && v.paramDerived[common.Value] {
		return v.checkInvokePollution(common, pos)
	}
	if site.IsGormMethod() {
		// Calling a method value is checked where the method value is
		// created (checkClosure), so f := db.Find; f(nil) is reported once.
		if site.Kind == callsite.SiteBound {
			return nil
		}
		// gorm chain method on a param-derived receiver pollutes the argument.
		if v.paramDerived[site.Recv] && !typeutil.IsImmutableReturningBuiltin(site.Method()) && !typeutil.IsStateOnlyMethod(site.Method()) {
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

// checkInvokePollution checks an interface method call on a derived
// interface (#376). One that boxes the argument itself, as `var i wherer =
// db; i.Where("x")` does, calls the *gorm.DB method: it pollutes the argument
// as db.Where("x") does. Any other dynamic value is handed to a method the
// validator cannot see, as an argument to a non-pure function is.
func (v *Validator) checkInvokePollution(common *ssa.CallCommon, pos token.Pos) []Violation {
	name := common.Method.Name()
	if _, ok := pollutionsource.UnwrapGormDB(common.Value); ok {
		if typeutil.IsImmutableReturningBuiltin(name) {
			return nil
		}
		return []Violation{{
			Pos:        pos,
			Message:    "pure function pollutes *gorm.DB argument by calling " + name,
			Definitive: true,
		}}
	}
	return []Violation{{
		Pos:     pos,
		Message: "pure function passes *gorm.DB argument to interface method " + name,
	}}
}

// checkWrappedReturn reports a pure function that returns a derived
// *gorm.DB inside another value: a struct, a pointer, an interface, or a
// closure that returns it. ReturnedParams covers only a *gorm.DB result, so a
// caller would trust `s := Wrap(q); s.DB.Where("a")` not to branch q (#376).
//
// A slice, a map or a channel is left out: its elements are shared memory,
// which the caller's store into it already counts as a leak. So is a closure
// that only uses what it captures, whose body is checked as part of the
// function.
func (v *Validator) checkWrappedReturn(ret *ssa.Return) []Violation {
	var violations []Violation
	for _, res := range ret.Results {
		if typeutil.IsGormDB(res.Type()) || !v.paramDerived[res] && !v.isDerivedArg(res) {
			continue
		}
		switch res.Type().Underlying().(type) {
		case *types.Slice, *types.Map, *types.Chan:
			continue
		case *types.Signature:
			if !v.closureReturnsDerived(res) {
				continue
			}
		}
		violations = append(violations, Violation{
			Pos:        ret.Pos(),
			Message:    "pure function leaks *gorm.DB argument via return of " + types.TypeString(res.Type(), (*types.Package).Name),
			Definitive: true,
		})
	}
	return violations
}

// closureReturnsDerived reports whether calling the function value f may
// return a derived value. A gorm method value (f := db.Where) is reported
// where it is created. A function value the validator cannot resolve to a
// closure may.
func (v *Validator) closureReturnsDerived(f ssa.Value) bool {
	mc, ok := v.funcVars.LocalValue(f).(*ssa.MakeClosure)
	if !ok {
		return true
	}
	if site, ok := callsite.BoundSite(mc); ok && site.IsGormMethod() {
		return false
	}
	closure, _ := mc.Fn.(*ssa.Function)
	return v.seedClosure(mc) && v.returnsDerived(closure)
}

// checkClosure checks a MakeClosure: a method value on a derived receiver
// (f := db.Find) pollutes it, and a closure capturing derived values must obey
// the contract in its own body (IIFEs, go func() {...}(), stored closures).
func (v *Validator) checkClosure(mc *ssa.MakeClosure) []Violation {
	if site, ok := callsite.BoundSite(mc); ok && site.IsGormMethod() {
		if v.paramDerived[site.Recv] && !typeutil.IsImmutableReturningBuiltin(site.Method()) && !typeutil.IsStateOnlyMethod(site.Method()) {
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

// checkDirectWrites reports the writes into the query of a *gorm.DB or
// *gorm.Statement parameter through a direct path (#405), such as
// `*db = gorm.DB{}`, `db.Statement.Distinct = true` or
// `delete(db.Statement.Clauses, "X")`: they change its query in place, as
// db.Where does. stmtwrite decides what is a direct path, as it does for the
// Statement writes of the inferred immutable-param. A write through anything
// else, such as a local copy or a call result, may not reach the argument,
// and is not reported. A method's receiver is not an argument (see
// seededParams). found holds the violations found already: a position that
// has a definitive one, such as the escape of db.Statement.DB = db, is not
// reported again.
func (v *Validator) checkDirectWrites(found []Violation) []Violation {
	var handles []ssa.Value
	for i, p := range v.fn.Params {
		if (i > 0 || v.fn.Signature.Recv() == nil) && stmtwrite.IsHandle(p.Type()) {
			handles = append(handles, stmtwrite.DirectHandles(p)...)
		}
	}
	reported := make(map[token.Pos]bool, len(found))
	for _, viol := range found {
		reported[viol.Pos] = reported[viol.Pos] || viol.Definitive
	}
	var violations []Violation
	for instr := range stmtwrite.Writes(handles) {
		if reported[instr.Pos()] {
			continue
		}
		violations = append(violations, Violation{
			Pos:        instr.Pos(),
			Message:    "pure function pollutes *gorm.DB argument by writing to it directly",
			Definitive: true,
		})
	}
	slices.SortFunc(violations, func(a, b Violation) int { return int(a.Pos - b.Pos) })
	return violations
}

// checkLeak reports a contract violation when a param-derived *gorm.DB escapes
// via a non-call pollution source (channel send, slice/array store, map store).
func (v *Validator) checkLeak(instr ssa.Instruction) []Violation {
	val, kind := pollutionsource.Leak(instr, v.funcVars)
	if kind == pollutionsource.KindNone || !v.paramDerived[val] {
		return nil
	}
	// Packing the variadic arguments of a call passes them to the callee,
	// which the call's check judges: `logv("x", db)` with `args ...any` is
	// the same as `one(db)` with `arg any` (#348).
	if store, ok := instr.(*ssa.Store); ok && v.isVarargsPack(store.Addr.(*ssa.IndexAddr).X) {
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
	if slice, ok := arg.(*ssa.Slice); ok && v.isVarargsPack(slice.X) && v.packsLeakedDerived(slice.X) {
		return true
	}
	return v.paramDerived[arg] && v.holdsGormDB(arg.Type())
}

// isVarargsPack reports whether array is only filled with elements and passed,
// sliced, as the variadic argument of calls, as Go packs the arguments of
// `f(a, b)` for `func f(args ...any)`.
func (v *Validator) isVarargsPack(array ssa.Value) bool {
	alloc, ok := array.(*ssa.Alloc)
	if !ok {
		return false
	}
	passed := false
	for _, ref := range *alloc.Referrers() {
		switch r := ref.(type) {
		case *ssa.IndexAddr:
			for _, ir := range *r.Referrers() {
				if store, ok := ir.(*ssa.Store); !ok || store.Addr != r {
					return false
				}
			}
		case *ssa.Slice:
			for _, sr := range *r.Referrers() {
				call, ok := sr.(ssa.CallInstruction)
				if !ok {
					return false
				}
				common := call.Common()
				// The slice is the last argument, and no other.
				if !common.Signature().Variadic() || slices.Index(common.Args, ssa.Value(r)) != len(common.Args)-1 {
					return false
				}
				passed = true
			}
		default:
			return false
		}
	}
	return passed
}

// packsLeakedDerived reports whether a parameter-derived *gorm.DB is stored
// into the variadic pack array in a way pollutionsource.Leak counts, so not
// into the arguments of a read-only call such as fmt.Println.
func (v *Validator) packsLeakedDerived(array ssa.Value) bool {
	for _, ref := range *array.Referrers() {
		idx, ok := ref.(*ssa.IndexAddr)
		if !ok {
			continue
		}
		for _, ir := range *idx.Referrers() {
			if val, kind := pollutionsource.Leak(ir, v.funcVars); kind == pollutionsource.KindSliceStore && v.paramDerived[val] {
				return true
			}
		}
	}
	return false
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
