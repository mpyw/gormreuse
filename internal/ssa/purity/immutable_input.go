package purity

import (
	"fmt"

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

	// Resolve declared callback param indices to actual SSA Parameters.
	// ssa.Function.Params lists the receiver first for methods, while
	// ImmutableInputCallback.ParamIdx counts from the first non-receiver
	// parameter, so shift by one when the function has a receiver.
	hasReceiver := fn.Signature != nil && fn.Signature.Recv() != nil
	cbParams := make(map[*ssa.Parameter]string, len(callbacks))
	for _, cb := range callbacks {
		idx := cb.ParamIdx
		if hasReceiver {
			idx++
		}
		if idx < 0 || idx >= len(fn.Params) {
			continue
		}
		cbParams[fn.Params[idx]] = cb.ParamName
	}
	if len(cbParams) == 0 {
		return nil
	}

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
			name, watched := cbParams[calleeParam]
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
					Pos:     call.Pos(),
					Message: fmt.Sprintf("immutable-input(%s) declared but mutable *gorm.DB passed to callback", name),
				})
				break // one diagnostic per call site
			}
		}
	}
	return violations
}
