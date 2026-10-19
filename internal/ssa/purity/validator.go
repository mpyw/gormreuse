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
//   - Return a *gorm.DB derived from parameter (may be mutable)
//   - Pass *gorm.DB to other pure functions, and to read-only stdlib
//     formatting/logging functions (fmt, log, testing)
package purity

import (
	"go/token"
	"go/types"
	"slices"
	"strings"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/directive"
	"github.com/mpyw/gormreuse/internal/ssa/pollutionsource"
	"github.com/mpyw/gormreuse/internal/typeutil"
)

// =============================================================================
// Validator
// =============================================================================

// Validator validates pure function contracts.
type Validator struct {
	fn        *ssa.Function
	pureFuncs *directive.DirectiveFuncSet
	// failedPure lists //gormreuse:pure functions already known to break the
	// contract definitively. Passing an argument to one of them breaks the
	// caller's contract too, so failure propagates through pure wrappers. It
	// may be nil.
	failedPure   map[*ssa.Function]bool
	paramDerived map[ssa.Value]bool
	holdsCache   map[types.Type]bool
}

// NewValidator creates a Validator for a function marked as pure.
func NewValidator(fn *ssa.Function, pureFuncs *directive.DirectiveFuncSet, failedPure map[*ssa.Function]bool) *Validator {
	return &Validator{
		fn:           fn,
		pureFuncs:    pureFuncs,
		failedPure:   failedPure,
		paramDerived: make(map[ssa.Value]bool),
		holdsCache:   make(map[types.Type]bool),
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

	// Initialize with every parameter that holds a *gorm.DB. A method's
	// receiver is not an argument: it usually holds the application's root
	// handle (r.db), and the directive's signature check ignores it too.
	params := fn.Params
	if fn.Signature != nil && fn.Signature.Recv() != nil && len(params) > 0 {
		params = params[1:]
	}
	for _, p := range params {
		if v.holdsGormDB(p.Type()) {
			v.paramDerived[p] = true
		}
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

// validateBody propagates derivation through fn to a fixed point, then checks
// every instruction. Closures created from parameter-derived values are
// validated recursively with their captured variables seeded; each closure
// has exactly one MakeClosure, so no body is visited twice.
func (v *Validator) validateBody(fn *ssa.Function) []Violation {
	// Loops feed values back through Phi edges and local variables, so a
	// single forward pass can miss them.
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

	var violations []Violation
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			switch i := instr.(type) {
			case *ssa.Call:
				violations = append(violations, v.checkCallPollution(&i.Call, i.Pos())...)
			case *ssa.Go:
				violations = append(violations, v.checkCallPollution(&i.Call, i.Pos())...)
			case *ssa.Defer:
				violations = append(violations, v.checkCallPollution(&i.Call, i.Pos())...)
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

// trackDerivation marks the values instr derives from parameter-derived
// values. It reports whether it marked anything new.
func (v *Validator) trackDerivation(instr ssa.Instruction) bool {
	switch i := instr.(type) {
	case *ssa.Phi:
		if slices.ContainsFunc(i.Edges, func(edge ssa.Value) bool { return v.paramDerived[edge] }) {
			return v.markDerived(i)
		}

	case *ssa.Call:
		if v.callResultDerived(&i.Call) {
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
// argument's Statement: a non-immutable gorm method on a derived receiver, or
// a call that is not trusted as pure with a derived argument.
func (v *Validator) callResultDerived(common *ssa.CallCommon) bool {
	// An interface method call never has a *gorm.DB receiver (it is a
	// concrete type), so it falls through to the regular-call rule below.

	// Method value: f := db.Find; f(nil)
	if method, recv, ok := v.boundGormMethod(common.Value); ok {
		return v.paramDerived[recv] && !typeutil.IsImmutableReturningBuiltin(method)
	}

	callee := common.StaticCallee()
	if recv, ok := v.gormMethodReceiver(common, callee); ok {
		return v.paramDerived[recv] && !typeutil.IsImmutableReturningBuiltin(callee.Name())
	}

	// Regular function call
	if v.isTrustedPure(callee) {
		return false
	}
	return slices.ContainsFunc(common.Args, v.isDerivedArg)
}

// =============================================================================
// Pollution Checking
// =============================================================================

// checkCallPollution checks a call, go or defer statement.
func (v *Validator) checkCallPollution(common *ssa.CallCommon, pos token.Pos) []Violation {
	// Interface method calls (common.Method != nil) on *gorm.DB are not
	// possible: IsGormDB only matches the concrete type, and GORM's DB is a
	// struct, so all method calls go through StaticCallee.

	// Calling a method value is checked where the method value is created
	// (checkClosure), so f := db.Find; f(nil) is reported once.
	if _, _, ok := v.boundGormMethod(common.Value); ok {
		return nil
	}

	callee := common.StaticCallee()

	// gorm chain method on a param-derived receiver pollutes the argument.
	if recv, ok := v.gormMethodReceiver(common, callee); ok {
		if v.paramDerived[recv] && !typeutil.IsImmutableReturningBuiltin(callee.Name()) {
			return []Violation{{
				Pos:        pos,
				Message:    "pure function pollutes *gorm.DB argument by calling " + callee.Name(),
				Definitive: true,
			}}
		}
		return nil
	}

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
	if method, recv, ok := v.boundGormMethod(mc); ok {
		if v.paramDerived[recv] && !typeutil.IsImmutableReturningBuiltin(method) {
			return []Violation{{
				Pos:        mc.Pos(),
				Message:    "pure function pollutes *gorm.DB argument by taking method value " + method,
				Definitive: true,
			}}
		}
		return nil
	}

	// MakeClosure.Fn is always a *ssa.Function, with one FreeVar per binding.
	closure, _ := mc.Fn.(*ssa.Function)
	captured := false
	for i, binding := range mc.Bindings {
		if v.paramDerived[binding] {
			v.paramDerived[closure.FreeVars[i]] = true
			captured = true
		}
	}
	if !captured {
		return nil
	}
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
	val, kind := pollutionsource.Leak(instr)
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
	failed := callee != nil && v.failedPure[callee]

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
				Message:    "pure function passes *gorm.DB argument to " + callee.Name() + ", which violates its pure contract",
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
	return callee != nil && v.pureFuncs.Contains(callee) && !v.failedPure[callee]
}

// calleeName returns a human-readable name for a call's target, used in the
// Validator's violation messages: the function name for static calls, the
// builtin name for builtins (append, etc.), or a generic label for indirect
// calls through a func value.
func (v *Validator) calleeName(common *ssa.CallCommon, callee *ssa.Function) string {
	if callee != nil {
		return callee.Name()
	}
	if b, ok := common.Value.(*ssa.Builtin); ok {
		return b.Name()
	}
	return "(closure)"
}

// holdsGormDB reports whether a value of type t can reach a *gorm.DB without
// going through an interface: *gorm.DB itself, a pointer to one, or a
// struct, slice, array, map, channel or tuple containing one.
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

// gormMethodReceiver returns the receiver of a static call to a *gorm.DB
// method.
func (v *Validator) gormMethodReceiver(common *ssa.CallCommon, callee *ssa.Function) (ssa.Value, bool) {
	if callee == nil || len(common.Args) == 0 {
		return nil, false
	}
	sig := callee.Signature
	if sig == nil || sig.Recv() == nil || !typeutil.IsGormDB(sig.Recv().Type()) {
		return nil, false
	}
	return common.Args[0], true
}

// boundGormMethod recognizes a *gorm.DB method value (db.Find used as a
// value). SSA builds it as MakeClosure(Find$bound, [db]); the wrapper has no
// receiver, so the gorm-method check alone would miss it.
func (v *Validator) boundGormMethod(val ssa.Value) (method string, recv ssa.Value, ok bool) {
	mc, isClosure := val.(*ssa.MakeClosure)
	if !isClosure || len(mc.Bindings) == 0 {
		return "", nil, false
	}
	fn, isFunc := mc.Fn.(*ssa.Function)
	if !isFunc || !strings.HasSuffix(fn.Name(), "$bound") || !typeutil.IsGormDB(mc.Bindings[0].Type()) {
		return "", nil, false
	}
	return strings.TrimSuffix(fn.Name(), "$bound"), mc.Bindings[0], true
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
