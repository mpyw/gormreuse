package purity

import (
	"go/token"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/directive"
	"github.com/mpyw/gormreuse/internal/typeutil"
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
// Validator
// =============================================================================

// Validator validates pure function contracts.
type Validator struct {
	fn           *ssa.Function
	pureFuncs    *directive.PureFuncSet
	paramDerived map[ssa.Value]bool
}

// ValidateFunction validates that a function marked as pure satisfies the pure contract:
// 1. Does not pollute *gorm.DB arguments (no non-pure method calls on them)
// 2. If returning *gorm.DB, the return value must be Clean or Depends (not Polluted)
func ValidateFunction(fn *ssa.Function, pureFuncs *directive.PureFuncSet) []Violation {
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
			}

			if ret, ok := instr.(*ssa.Return); ok {
				violations = append(violations, v.checkReturnPurity(ret)...)
			}
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
			if !typeutil.IsPureFunctionBuiltin(call.Call.Method.Name()) {
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
			if v.paramDerived[recv] && !typeutil.IsPureFunctionBuiltin(callee.Name()) {
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

	// Static call
	callee := call.Call.StaticCallee()
	if callee == nil {
		return nil
	}

	sig := callee.Signature
	if sig != nil && sig.Recv() != nil && typeutil.IsGormDB(sig.Recv().Type()) {
		return v.checkStaticMethodPollution(call, callee)
	}

	return v.checkFunctionCallPollution(call, callee)
}

func (v *Validator) checkStaticMethodPollution(call *ssa.Call, callee *ssa.Function) []Violation {
	if len(call.Call.Args) == 0 {
		return nil
	}

	recv := call.Call.Args[0]
	methodName := callee.Name()
	if v.paramDerived[recv] && !typeutil.IsPureFunctionBuiltin(methodName) {
		return []Violation{{
			Pos:     call.Pos(),
			Message: "pure function pollutes *gorm.DB argument by calling " + methodName,
		}}
	}
	return nil
}

func (v *Validator) checkFunctionCallPollution(call *ssa.Call, callee *ssa.Function) []Violation {
	var violations []Violation
	for _, arg := range call.Call.Args {
		if typeutil.IsGormDB(arg.Type()) && v.paramDerived[arg] && !v.pureFuncs.Contains(callee) {
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

func (v *Validator) checkReturnPurity(ret *ssa.Return) []Violation {
	var violations []Violation
	inferencer := NewInferencer(v.fn, v.pureFuncs)

	for _, result := range ret.Results {
		if result == nil || !typeutil.IsGormDB(result.Type()) {
			continue
		}

		if inferencer.InferValue(result).IsPolluted() {
			violations = append(violations, Violation{
				Pos:     ret.Pos(),
				Message: "pure function returns Polluted *gorm.DB (should return result of Session/WithContext/etc., another pure function, or the parameter unchanged)",
			})
		}
	}

	return violations
}
