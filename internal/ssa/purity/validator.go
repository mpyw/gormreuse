// Package purity validates that functions marked with //gormreuse:pure
// satisfy the pure function contract via SSA-based analysis.
//
// # Pure Contract
//
// A pure function must not pollute its *gorm.DB arguments. This means:
//   - No chain method calls (Where, Find, etc.) on parameter-derived values
//   - No passing parameter-derived values to non-pure functions
//
// # Analysis Strategy
//
// The validator uses derivation tracking to follow *gorm.DB values:
//  1. Mark all *gorm.DB parameters as "parameter-derived"
//  2. Track derivation through Phi nodes, Extract, and Call results
//  3. Report violations when parameter-derived values are polluted
//
// # What Pure Functions CAN Do
//
//   - Call immutable-returning methods (Session, WithContext, Debug)
//   - Return a *gorm.DB derived from parameter (may be mutable)
//   - Pass *gorm.DB to other pure functions
package purity

import (
	"go/token"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/directive"
	"github.com/mpyw/gormreuse/internal/ssa/pollutionsource"
	"github.com/mpyw/gormreuse/internal/typeutil"
)

// =============================================================================
// Violation
// =============================================================================

// Violation represents a pure function contract violation.
type Violation struct {
	Pos     token.Pos
	Message string

	// Leak is true when the violation is a definitive escape of the argument
	// (channel send, slice/array store, map store) rather than a conservative
	// guess (passing to a not-yet-proven-pure function). Only definitive escapes
	// revoke the function's pure-trust at its call sites — a conservative
	// func-arg violation might still be pure in practice, and cascading it would
	// produce false positives (see nestedClosureOuterPureViolation).
	Leak bool
}

// =============================================================================
// Validator
// =============================================================================

// Validator validates pure function contracts.
type Validator struct {
	fn           *ssa.Function
	pureFuncs    *directive.DirectiveFuncSet
	paramDerived map[ssa.Value]bool
}

// ValidateFunction validates that a function marked as pure satisfies the pure contract:
// - Does not pollute *gorm.DB arguments (no non-pure method calls on them)
//
// Note: Pure functions MAY return mutable *gorm.DB values. The "pure" contract only
// guarantees that the function doesn't pollute its arguments - callers must treat
// the return value as potentially mutable.
func ValidateFunction(fn *ssa.Function, pureFuncs *directive.DirectiveFuncSet) []Violation {
	if fn == nil || fn.Blocks == nil {
		return nil
	}

	v := &Validator{
		fn:           fn,
		pureFuncs:    pureFuncs,
		paramDerived: make(map[ssa.Value]bool),
	}

	// Initialize with *gorm.DB parameters
	for _, p := range fn.Params {
		if typeutil.IsGormDB(p.Type()) {
			v.paramDerived[p] = true
		}
	}

	var violations []Violation

	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			v.trackDerivation(instr)

			if call, ok := instr.(*ssa.Call); ok {
				violations = append(violations, v.checkCallPollution(call)...)
				continue
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

func (v *Validator) trackDerivation(instr ssa.Instruction) {
	switch i := instr.(type) {
	case *ssa.Phi:
		for _, edge := range i.Edges {
			if v.paramDerived[edge] {
				v.paramDerived[i] = true
				break
			}
		}

	case *ssa.Call:
		v.trackCallDerivation(i)

	case *ssa.Extract:
		if v.paramDerived[i.Tuple] {
			v.paramDerived[i] = true
		}
	}
}

func (v *Validator) trackCallDerivation(call *ssa.Call) {
	// Interface method call
	if call.Call.Method != nil {
		recv := call.Call.Value
		if typeutil.IsGormDB(recv.Type()) && v.paramDerived[recv] {
			if !typeutil.IsImmutableReturningBuiltin(call.Call.Method.Name()) {
				if result := call.Value(); result != nil {
					v.paramDerived[result] = true
				}
			}
		}
		return
	}

	// Static call
	callee := call.Call.StaticCallee()
	if callee == nil {
		return
	}

	sig := callee.Signature
	if sig != nil && sig.Recv() != nil && typeutil.IsGormDB(sig.Recv().Type()) {
		if len(call.Call.Args) > 0 {
			recv := call.Call.Args[0]
			if v.paramDerived[recv] && !typeutil.IsImmutableReturningBuiltin(callee.Name()) {
				if result := call.Value(); result != nil {
					v.paramDerived[result] = true
				}
			}
		}
		return
	}

	// Regular function call
	for _, arg := range call.Call.Args {
		if typeutil.IsGormDB(arg.Type()) && v.paramDerived[arg] {
			if result := call.Value(); result != nil && typeutil.IsGormDB(result.Type()) {
				if !v.pureFuncs.Contains(callee) {
					v.paramDerived[result] = true
				}
			}
			break
		}
	}
}

// =============================================================================
// Pollution Checking
// =============================================================================

func (v *Validator) checkCallPollution(call *ssa.Call) []Violation {
	// Interface method calls (call.Call.Method != nil) are not supported
	// because IsGormDB only matches *gorm.DB (concrete pointer type).
	// GORM's DB is a struct, so all method calls go through StaticCallee.

	callee := call.Call.StaticCallee()

	// gorm chain method on a param-derived receiver pollutes the argument.
	if callee != nil {
		sig := callee.Signature
		if sig != nil && sig.Recv() != nil && typeutil.IsGormDB(sig.Recv().Type()) {
			return v.checkStaticMethodPollution(call, callee)
		}
	}

	// Any other call — a static function, a builtin (e.g. append, which is how
	// slice storage lowers), or an indirect call through a func value — leaks a
	// param-derived *gorm.DB passed to it, unless the callee is itself pure.
	// callee is nil for builtins and indirect calls; those are never pure, so
	// they leak conservatively, matching the main handler.
	return v.checkFunctionCallPollution(call, callee)
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
		Pos:     instr.Pos(),
		Message: "pure function leaks *gorm.DB argument via " + via,
		Leak:    true,
	}}
}

func (v *Validator) checkStaticMethodPollution(call *ssa.Call, callee *ssa.Function) []Violation {
	if len(call.Call.Args) == 0 {
		return nil
	}

	recv := call.Call.Args[0]
	methodName := callee.Name()
	if v.paramDerived[recv] && !typeutil.IsImmutableReturningBuiltin(methodName) {
		return []Violation{{
			Pos:     call.Pos(),
			Message: "pure function pollutes *gorm.DB argument by calling " + methodName,
		}}
	}
	return nil
}

func (v *Validator) checkFunctionCallPollution(call *ssa.Call, callee *ssa.Function) []Violation {
	// Pure callees don't pollute their arguments.
	if callee != nil && v.pureFuncs.Contains(callee) {
		return nil
	}

	var violations []Violation
	for _, arg := range call.Call.Args {
		// Unwrap interface-boxed args so a *gorm.DB passed as interface{}
		// (e.g. to a variadic ...any function) is still caught.
		gormArg, ok := pollutionsource.UnwrapGormDB(arg)
		if !ok || !v.paramDerived[gormArg] {
			continue
		}
		violations = append(violations, Violation{
			Pos:     call.Pos(),
			Message: "pure function passes *gorm.DB argument to non-pure function " + calleeName(call, callee),
		})
	}
	return violations
}

// calleeName returns a human-readable name for a call's target: the function
// name for static calls, the builtin name for builtins (append, etc.), or a
// generic label for indirect calls through a func value.
func calleeName(call *ssa.Call, callee *ssa.Function) string {
	if callee != nil {
		return callee.Name()
	}
	if b, ok := call.Call.Value.(*ssa.Builtin); ok {
		return b.Name()
	}
	return "(closure)"
}
