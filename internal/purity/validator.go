package purity

import (
	"go/token"

	"golang.org/x/tools/go/ssa"
)

// =============================================================================
// Violation
// =============================================================================

// Violation represents a pure function contract violation.
type Violation struct {
	Pos     token.Pos
	Message string
}

// =============================================================================
// Validation Entry Point
// =============================================================================

// ValidateFunction validates that a function marked as pure satisfies the pure contract:
// 1. Does not pollute *gorm.DB arguments (no non-pure method calls on them)
// 2. If returning *gorm.DB, the return value must be Clean or Depends (not Polluted)
func ValidateFunction(fn *ssa.Function, checker PurityChecker) []Violation {
	if fn == nil || fn.Blocks == nil {
		return nil
	}

	var violations []Violation

	gormParams := findGormParams(fn, checker)
	paramDerived := make(map[ssa.Value]bool)
	for _, p := range gormParams {
		paramDerived[p] = true
	}

	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			trackDerivation(instr, paramDerived, checker)

			if call, ok := instr.(*ssa.Call); ok {
				violations = append(violations, checkCallPollution(call, paramDerived, checker)...)
			}

			if ret, ok := instr.(*ssa.Return); ok {
				violations = append(violations, checkReturnPurity(ret, fn, checker)...)
			}
		}
	}

	return violations
}

// =============================================================================
// Parameter Detection
// =============================================================================

func findGormParams(fn *ssa.Function, checker PurityChecker) []*ssa.Parameter {
	var params []*ssa.Parameter
	for _, p := range fn.Params {
		if checker.IsGormDB(p.Type()) {
			params = append(params, p)
		}
	}
	return params
}

// =============================================================================
// Derivation Tracking
// =============================================================================

func trackDerivation(instr ssa.Instruction, paramDerived map[ssa.Value]bool, checker PurityChecker) {
	switch i := instr.(type) {
	case *ssa.Phi:
		for _, edge := range i.Edges {
			if paramDerived[edge] {
				paramDerived[i] = true
				break
			}
		}

	case *ssa.Call:
		trackCallDerivation(i, paramDerived, checker)

	case *ssa.Extract:
		if paramDerived[i.Tuple] {
			paramDerived[i] = true
		}
	}
}

func trackCallDerivation(call *ssa.Call, paramDerived map[ssa.Value]bool, checker PurityChecker) {
	// Interface method call
	if call.Call.Method != nil {
		recv := call.Call.Value
		if checker.IsGormDB(recv.Type()) && paramDerived[recv] {
			if !checker.IsPureBuiltinMethod(call.Call.Method.Name()) {
				if result := call.Value(); result != nil {
					paramDerived[result] = true
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
	if sig != nil && sig.Recv() != nil && checker.IsGormDB(sig.Recv().Type()) {
		if len(call.Call.Args) > 0 {
			recv := call.Call.Args[0]
			if paramDerived[recv] && !checker.IsPureBuiltinMethod(callee.Name()) {
				if result := call.Value(); result != nil {
					paramDerived[result] = true
				}
			}
		}
		return
	}

	// Regular function call
	for _, arg := range call.Call.Args {
		if checker.IsGormDB(arg.Type()) && paramDerived[arg] {
			if result := call.Value(); result != nil && checker.IsGormDB(result.Type()) {
				if !checker.IsPureUserFunc(callee) {
					paramDerived[result] = true
				}
			}
			break
		}
	}
}

// =============================================================================
// Pollution Checking
// =============================================================================

func checkCallPollution(call *ssa.Call, paramDerived map[ssa.Value]bool, checker PurityChecker) []Violation {
	// Interface method call
	if call.Call.Method != nil {
		return checkInterfaceMethodPollution(call, paramDerived, checker)
	}

	// Static call
	callee := call.Call.StaticCallee()
	if callee == nil {
		return nil
	}

	sig := callee.Signature
	if sig != nil && sig.Recv() != nil && checker.IsGormDB(sig.Recv().Type()) {
		return checkStaticMethodPollution(call, callee, paramDerived, checker)
	}

	return checkFunctionCallPollution(call, callee, paramDerived, checker)
}

func checkInterfaceMethodPollution(call *ssa.Call, paramDerived map[ssa.Value]bool, checker PurityChecker) []Violation {
	recv := call.Call.Value
	if !checker.IsGormDB(recv.Type()) {
		return nil
	}

	methodName := call.Call.Method.Name()
	if paramDerived[recv] && !checker.IsPureBuiltinMethod(methodName) {
		return []Violation{{
			Pos:     call.Pos(),
			Message: "pure function pollutes *gorm.DB argument by calling " + methodName,
		}}
	}
	return nil
}

func checkStaticMethodPollution(call *ssa.Call, callee *ssa.Function, paramDerived map[ssa.Value]bool, checker PurityChecker) []Violation {
	if len(call.Call.Args) == 0 {
		return nil
	}

	recv := call.Call.Args[0]
	methodName := callee.Name()
	if paramDerived[recv] && !checker.IsPureBuiltinMethod(methodName) {
		return []Violation{{
			Pos:     call.Pos(),
			Message: "pure function pollutes *gorm.DB argument by calling " + methodName,
		}}
	}
	return nil
}

func checkFunctionCallPollution(call *ssa.Call, callee *ssa.Function, paramDerived map[ssa.Value]bool, checker PurityChecker) []Violation {
	var violations []Violation
	for _, arg := range call.Call.Args {
		if checker.IsGormDB(arg.Type()) && paramDerived[arg] && !checker.IsPureUserFunc(callee) {
			violations = append(violations, Violation{
				Pos:     call.Pos(),
				Message: "pure function passes *gorm.DB argument to non-pure function " + callee.Name(),
			})
		}
	}
	return violations
}

// =============================================================================
// Return Purity Checking
// =============================================================================

func checkReturnPurity(ret *ssa.Return, fn *ssa.Function, checker PurityChecker) []Violation {
	var violations []Violation
	analyzer := NewAnalyzer(fn, checker)

	for _, result := range ret.Results {
		if result == nil || !checker.IsGormDB(result.Type()) {
			continue
		}

		if analyzer.AnalyzeValue(result).IsPolluted() {
			violations = append(violations, Violation{
				Pos:     ret.Pos(),
				Message: "pure function returns Polluted *gorm.DB (should return result of Session/WithContext/etc., another pure function, or the parameter unchanged)",
			})
		}
	}

	return violations
}
