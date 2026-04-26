package purity

import (
	"fmt"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/directive"
	"github.com/mpyw/gormreuse/internal/ssa/tracer"
	"github.com/mpyw/gormreuse/internal/typeutil"
)

// ValidateImmutableInputs checks the contract declared by
// `//gormreuse:immutable-input(name)`: whenever the function calls the
// declared callback parameter, every *gorm.DB argument must be an
// immutable source (Session()/WithContext()/Debug() result, builtin
// init, immutable-return user function, or nil constant).
//
// Issue #56 cases 2.3 (declaration violated) and 2.4 (correct usage)
// are handled here.
func ValidateImmutableInputs(
	fn *ssa.Function,
	set *directive.ImmutableInputSet,
	pureFuncs, immutableReturnFuncs *directive.DirectiveFuncSet,
) []Violation {
	if fn == nil || set == nil {
		return nil
	}
	callbacks := set.AllCallbacks(fn)
	if len(callbacks) == 0 {
		return nil
	}

	// Resolve declared callback param indices to actual SSA Parameters.
	// ssa.Function.Params lists the receiver first for methods, while
	// ImmutableInputCallback.ParamIdx counts from the first non-receiver
	// parameter, so shift by one when the function has a receiver.
	hasReceiver := fn.Signature != nil && fn.Signature.Recv() != nil
	type cbInfo struct {
		param *ssa.Parameter
		name  string
	}
	cbParams := make(map[*ssa.Parameter]cbInfo, len(callbacks))
	for _, cb := range callbacks {
		idx := cb.ParamIdx
		if hasReceiver {
			idx++
		}
		if idx < 0 || idx >= len(fn.Params) {
			continue
		}
		cbParams[fn.Params[idx]] = cbInfo{param: fn.Params[idx], name: cb.ParamName}
	}
	if len(cbParams) == 0 {
		return nil
	}

	rt := tracer.New(pureFuncs, immutableReturnFuncs, set)

	var violations []Violation
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			call, ok := instr.(*ssa.Call)
			if !ok {
				continue
			}
			calleeParam, ok := call.Call.Value.(*ssa.Parameter)
			if !ok {
				continue
			}
			info, watched := cbParams[calleeParam]
			if !watched {
				continue
			}

			for _, arg := range call.Call.Args {
				if !typeutil.IsGormDB(arg.Type()) {
					continue
				}
				if rt.FindMutableRoot(arg, nil) == nil {
					continue // immutable source — fine
				}
				violations = append(violations, Violation{
					Pos: call.Pos(),
					Message: fmt.Sprintf(
						"immutable-input(%s) declared but mutable *gorm.DB passed to callback",
						info.name,
					),
				})
				break // one diagnostic per call site
			}
		}
	}
	return violations
}
