package internal

import (
	"go/token"

	"golang.org/x/tools/go/ssa"
)

// PureViolation represents a violation of pure function contract.
type PureViolation struct {
	Pos     token.Pos
	Message string
}

// ValidatePureFunction checks if a function marked as pure actually satisfies
// the pure contract:
// 1. Does not pollute *gorm.DB arguments (no chain method calls on them)
// 2. If returning *gorm.DB, returns an immutable instance
func ValidatePureFunction(fn *ssa.Function, pureFuncs *PureFuncSet) []PureViolation {
	var violations []PureViolation

	// Find *gorm.DB parameters
	gormParams := findGormDBParams(fn)
	if len(gormParams) == 0 && !returnsGormDB(fn) {
		// No *gorm.DB involved, nothing to validate
		return nil
	}

	// Track which SSA values are derived from *gorm.DB parameters
	paramDerived := make(map[ssa.Value]bool)
	for _, p := range gormParams {
		paramDerived[p] = true
	}

	// Process all blocks
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			// Check for chain method calls on parameter-derived values
			if call, ok := instr.(*ssa.Call); ok {
				violations = append(violations, checkCallInPure(call, paramDerived, pureFuncs)...)
			}

			// Track derivation through phi nodes and other instructions
			trackDerivation(instr, paramDerived)

			// Check return statements
			if ret, ok := instr.(*ssa.Return); ok {
				violations = append(violations, checkReturnInPure(ret, paramDerived, pureFuncs)...)
			}
		}
	}

	return violations
}

// findGormDBParams returns all parameters of type *gorm.DB.
func findGormDBParams(fn *ssa.Function) []*ssa.Parameter {
	var params []*ssa.Parameter
	for _, p := range fn.Params {
		if IsGormDB(p.Type()) {
			params = append(params, p)
		}
	}
	return params
}

// returnsGormDB checks if the function returns *gorm.DB.
func returnsGormDB(fn *ssa.Function) bool {
	sig := fn.Signature
	results := sig.Results()
	for i := 0; i < results.Len(); i++ {
		if IsGormDB(results.At(i).Type()) {
			return true
		}
	}
	return false
}

// checkCallInPure checks if a call in a pure function violates the pure contract.
func checkCallInPure(call *ssa.Call, paramDerived map[ssa.Value]bool, pureFuncs *PureFuncSet) []PureViolation {
	var violations []PureViolation

	// Check if this is an interface method call on *gorm.DB
	if call.Call.Method != nil {
		recv := call.Call.Value
		if !IsGormDB(recv.Type()) {
			return nil
		}

		methodName := call.Call.Method.Name()

		// If receiver is derived from parameter and method is not pure,
		// this is a violation (polluting the argument)
		if isParamDerived(recv, paramDerived) && !IsPureFunctionBuiltin(methodName) {
			violations = append(violations, PureViolation{
				Pos:     call.Pos(),
				Message: "pure function pollutes *gorm.DB argument by calling " + methodName,
			})
		}

		// Track the result as param-derived if receiver was param-derived
		// BUT: if the method is pure (Session, WithContext, etc.), the result is
		// NOT param-derived because it's a new immutable instance
		if isParamDerived(recv, paramDerived) && !IsPureFunctionBuiltin(methodName) {
			if result := call.Value(); result != nil {
				paramDerived[result] = true
			}
		}
		return violations
	}

	// Check static function calls (including concrete type methods)
	callee := call.Call.StaticCallee()
	if callee == nil {
		return violations
	}

	sig := callee.Signature
	if sig != nil && sig.Recv() != nil && IsGormDB(sig.Recv().Type()) {
		// This is a method call on *gorm.DB (concrete type)
		// In SSA, the receiver is the first argument
		if len(call.Call.Args) == 0 {
			return violations
		}

		recv := call.Call.Args[0]
		methodName := callee.Name()

		// If receiver is derived from parameter and method is not pure,
		// this is a violation (polluting the argument)
		if isParamDerived(recv, paramDerived) && !IsPureFunctionBuiltin(methodName) {
			violations = append(violations, PureViolation{
				Pos:     call.Pos(),
				Message: "pure function pollutes *gorm.DB argument by calling " + methodName,
			})
		}

		// Track the result as param-derived if receiver was param-derived
		// BUT: if the method is pure (Session, WithContext, etc.), the result is
		// NOT param-derived because it's a new immutable instance
		if isParamDerived(recv, paramDerived) && !IsPureFunctionBuiltin(methodName) {
			if result := call.Value(); result != nil {
				paramDerived[result] = true
			}
		}
		return violations
	}

	// Regular function call that receives *gorm.DB as argument
	for _, arg := range call.Call.Args {
		if IsGormDB(arg.Type()) && isParamDerived(arg, paramDerived) {
			if !isPureFunction(callee, pureFuncs) {
				violations = append(violations, PureViolation{
					Pos:     call.Pos(),
					Message: "pure function passes *gorm.DB argument to non-pure function " + callee.Name(),
				})
			}
		}
	}

	return violations
}

// checkReturnInPure checks if a return statement in a pure function returns
// a mutable (polluted) *gorm.DB.
func checkReturnInPure(ret *ssa.Return, paramDerived map[ssa.Value]bool, pureFuncs *PureFuncSet) []PureViolation {
	var violations []PureViolation

	for _, result := range ret.Results {
		if result == nil || !IsGormDB(result.Type()) {
			continue
		}

		// Check if the returned value is immutable
		if !isImmutableValue(result, paramDerived, pureFuncs) {
			violations = append(violations, PureViolation{
				Pos:     ret.Pos(),
				Message: "pure function returns mutable *gorm.DB (should return result of Session/WithContext/etc. or another pure function)",
			})
		}
	}

	return violations
}

// isParamDerived checks if a value is derived from a *gorm.DB parameter.
func isParamDerived(v ssa.Value, paramDerived map[ssa.Value]bool) bool {
	return paramDerived[v]
}

// isImmutableValue checks if a *gorm.DB value is immutable.
// A value is immutable if:
// - It comes from a pure function (builtin or user-defined)
// - It comes from a call to a function that returns immutable (like gorm.Open)
func isImmutableValue(v ssa.Value, paramDerived map[ssa.Value]bool, pureFuncs *PureFuncSet) bool {
	switch val := v.(type) {
	case *ssa.Const:
		// nil is immutable (and any other constant)
		return true

	case *ssa.Call:
		// Interface method call
		if val.Call.Method != nil {
			return IsPureFunctionBuiltin(val.Call.Method.Name())
		}
		// Static call (including method calls on known receiver types)
		if callee := val.Call.StaticCallee(); callee != nil {
			// Check if it's a pure function (builtin or user-defined)
			return isPureFunction(callee, pureFuncs)
		}
		return false

	case *ssa.Phi:
		// Phi node - all incoming values must be immutable
		for _, edge := range val.Edges {
			if !isImmutableValue(edge, paramDerived, pureFuncs) {
				return false
			}
		}
		return len(val.Edges) > 0

	case *ssa.Parameter:
		// Parameter is mutable by default (caller could pass mutable)
		// But if we're in a pure function and this is a *gorm.DB param,
		// we assume the caller passes something that can be reused,
		// so returning it directly is a violation
		return false

	case *ssa.Extract:
		// Extract from tuple - trace back to the tuple source
		return isImmutableValue(val.Tuple, paramDerived, pureFuncs)

	case *ssa.FieldAddr, *ssa.Field:
		// Field access - conservative: assume mutable
		return false

	case *ssa.UnOp:
		// Dereference or other unary op
		if val.Op == token.MUL {
			return isImmutableValue(val.X, paramDerived, pureFuncs)
		}
		return false

	default:
		// Unknown - conservative: assume mutable
		return false
	}
}

// trackDerivation tracks which values are derived from *gorm.DB parameters.
func trackDerivation(instr ssa.Instruction, paramDerived map[ssa.Value]bool) {
	switch i := instr.(type) {
	case *ssa.Phi:
		// If any edge is param-derived, the phi result is param-derived
		for _, edge := range i.Edges {
			if paramDerived[edge] {
				paramDerived[i] = true
				break
			}
		}

	case *ssa.Call:
		// Already handled in checkCallInPure for *gorm.DB method calls
		// This handles other calls that might pass through param-derived *gorm.DB
		if i.Call.Method == nil && i.Call.StaticCallee() != nil {
			callee := i.Call.StaticCallee()
			sig := callee.Signature

			// Skip *gorm.DB method calls - handled in checkCallInPure
			if sig != nil && sig.Recv() != nil && IsGormDB(sig.Recv().Type()) {
				// Don't mark as param-derived here - checkCallInPure already handles this
				// with proper pure method detection
				return
			}

			// For other calls, if any *gorm.DB argument is param-derived,
			// mark the result as param-derived (conservative)
			for _, arg := range i.Call.Args {
				if IsGormDB(arg.Type()) && paramDerived[arg] {
					if result := i.Value(); result != nil && IsGormDB(result.Type()) {
						paramDerived[result] = true
					}
					break
				}
			}
		}

	case *ssa.Extract:
		// Extract from tuple - if tuple is param-derived, extract is too
		if paramDerived[i.Tuple] {
			paramDerived[i] = true
		}
	}
}

// isPureFunction checks if a function is pure (builtin or user-defined).
func isPureFunction(fn *ssa.Function, pureFuncs *PureFuncSet) bool {
	if fn == nil {
		return false
	}
	// Check builtin pure functions
	if IsPureFunctionBuiltin(fn.Name()) {
		return true
	}
	// Check user-defined pure functions
	if pureFuncs != nil && pureFuncs.Contains(fn) {
		return true
	}
	return false
}

// IsPureFunctionDecl checks if a function declaration has a pure directive.
// This is used to find which functions to validate.
func IsPureFunctionDecl(fn *ssa.Function, pureFuncs *PureFuncSet) bool {
	if pureFuncs == nil {
		return false
	}
	return pureFuncs.Contains(fn)
}
