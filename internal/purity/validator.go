// Package purity provides 3-state purity analysis for *gorm.DB values.
package purity

import (
	"go/token"

	"golang.org/x/tools/go/ssa"
)

// Violation represents a pure function contract violation.
type Violation struct {
	Pos     token.Pos
	Message string
}

// ValidateFunction validates that a function marked as pure satisfies the pure contract:
// 1. Does not pollute *gorm.DB arguments (no non-pure method calls on them)
// 2. If returning *gorm.DB, the return value must be Clean or Depends (not Polluted)
func ValidateFunction(fn *ssa.Function, checker PurityChecker) []Violation {
	if fn == nil || fn.Blocks == nil {
		return nil
	}

	var violations []Violation

	// Find *gorm.DB parameters
	gormParams := findGormParams(fn, checker)

	// Track which values are derived from parameters (and thus should not be polluted)
	paramDerived := make(map[ssa.Value]bool)
	for _, p := range gormParams {
		paramDerived[p] = true
	}

	// Process all blocks
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			// Track derivation through phi nodes
			trackDerivation(instr, paramDerived, checker)

			// Check for pollution of parameters
			if call, ok := instr.(*ssa.Call); ok {
				violations = append(violations, checkCallPollution(call, paramDerived, checker)...)
			}

			// Check return statements
			if ret, ok := instr.(*ssa.Return); ok {
				violations = append(violations, checkReturnPurity(ret, fn, checker)...)
			}
		}
	}

	return violations
}

// findGormParams returns all parameters of type *gorm.DB.
func findGormParams(fn *ssa.Function, checker PurityChecker) []*ssa.Parameter {
	var params []*ssa.Parameter
	for _, p := range fn.Params {
		if checker.IsGormDB(p.Type()) {
			params = append(params, p)
		}
	}
	return params
}

// trackDerivation updates paramDerived map based on instruction.
func trackDerivation(instr ssa.Instruction, paramDerived map[ssa.Value]bool, checker PurityChecker) {
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
		// For method calls on *gorm.DB, track the result
		if i.Call.Method != nil {
			recv := i.Call.Value
			if checker.IsGormDB(recv.Type()) && paramDerived[recv] {
				methodName := i.Call.Method.Name()
				// If method is NOT pure, the result is still param-derived (polluted)
				// If method IS pure, the result is NOT param-derived (it's a new immutable instance)
				if !checker.IsPureBuiltinMethod(methodName) {
					if result := i.Value(); result != nil {
						paramDerived[result] = true
					}
				}
			}
			return
		}

		// Static call
		if callee := i.Call.StaticCallee(); callee != nil {
			sig := callee.Signature
			if sig != nil && sig.Recv() != nil && checker.IsGormDB(sig.Recv().Type()) {
				// Method call on *gorm.DB
				if len(i.Call.Args) > 0 {
					recv := i.Call.Args[0]
					if paramDerived[recv] && !checker.IsPureBuiltinMethod(callee.Name()) {
						if result := i.Value(); result != nil {
							paramDerived[result] = true
						}
					}
				}
				return
			}

			// Regular function call - if any *gorm.DB arg is param-derived, result may be too
			for _, arg := range i.Call.Args {
				if checker.IsGormDB(arg.Type()) && paramDerived[arg] {
					if result := i.Value(); result != nil && checker.IsGormDB(result.Type()) {
						// Only mark as derived if the callee is NOT a pure function
						if !checker.IsPureUserFunc(callee) {
							paramDerived[result] = true
						}
					}
					break
				}
			}
		}

	case *ssa.Extract:
		if paramDerived[i.Tuple] {
			paramDerived[i] = true
		}
	}
}

// checkCallPollution checks if a call pollutes a parameter-derived value.
func checkCallPollution(call *ssa.Call, paramDerived map[ssa.Value]bool, checker PurityChecker) []Violation {
	var violations []Violation

	// Interface method call
	if call.Call.Method != nil {
		recv := call.Call.Value
		if !checker.IsGormDB(recv.Type()) {
			return nil
		}

		methodName := call.Call.Method.Name()
		if paramDerived[recv] && !checker.IsPureBuiltinMethod(methodName) {
			violations = append(violations, Violation{
				Pos:     call.Pos(),
				Message: "pure function pollutes *gorm.DB argument by calling " + methodName,
			})
		}
		return violations
	}

	// Static call
	callee := call.Call.StaticCallee()
	if callee == nil {
		return violations
	}

	sig := callee.Signature
	if sig != nil && sig.Recv() != nil && checker.IsGormDB(sig.Recv().Type()) {
		// Method call on *gorm.DB
		if len(call.Call.Args) == 0 {
			return violations
		}

		recv := call.Call.Args[0]
		methodName := callee.Name()
		if paramDerived[recv] && !checker.IsPureBuiltinMethod(methodName) {
			violations = append(violations, Violation{
				Pos:     call.Pos(),
				Message: "pure function pollutes *gorm.DB argument by calling " + methodName,
			})
		}
		return violations
	}

	// Regular function call that receives *gorm.DB
	for _, arg := range call.Call.Args {
		if checker.IsGormDB(arg.Type()) && paramDerived[arg] {
			if !checker.IsPureUserFunc(callee) {
				violations = append(violations, Violation{
					Pos:     call.Pos(),
					Message: "pure function passes *gorm.DB argument to non-pure function " + callee.Name(),
				})
			}
		}
	}

	return violations
}

// checkReturnPurity checks if a return value is pure (Clean or Depends, not Polluted).
func checkReturnPurity(ret *ssa.Return, fn *ssa.Function, checker PurityChecker) []Violation {
	var violations []Violation

	analyzer := NewAnalyzer(fn, checker)

	for _, result := range ret.Results {
		if result == nil || !checker.IsGormDB(result.Type()) {
			continue
		}

		state := analyzer.AnalyzeValue(result)
		if state.IsPolluted() {
			violations = append(violations, Violation{
				Pos:     ret.Pos(),
				Message: "pure function returns Polluted *gorm.DB (should return result of Session/WithContext/etc., another pure function, or the parameter unchanged)",
			})
		}
	}

	return violations
}
