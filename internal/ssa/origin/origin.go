// Package origin finds the function a call resolves to as it is declared:
// the generic origin of an instance, and the method behind a bound method
// value or a method expression.
package origin

import (
	"go/types"
	"strings"

	"golang.org/x/tools/go/ssa"
)

// Declared returns the function a directive is written on for fn, the
// function a call resolves to. Directives, and every map keyed by a directive
// function, are keyed by the declared function, while a call may resolve to a
// synthetic stand-in for it (#180):
//
//   - an instance of a generic function or method (genBranch[int]) maps to its
//     generic origin;
//   - a bound method wrapper (r.m used as a value, named "m$bound") and a
//     method expression thunk (T.m, named "m$thunk") map to the declared
//     method m (#258). An interface method has no declared body, so its
//     wrapper is returned unchanged.
//
// Callers that look at the arguments of a call must align them to the
// declared method (callsite.Site does): a bound wrapper takes no receiver
// argument, and a thunk takes the receiver as its first parameter. fn may be
// nil.
func Declared(fn *ssa.Function) *ssa.Function {
	if fn == nil {
		return nil
	}
	if origin := fn.Origin(); origin != nil {
		fn = origin
	}
	if fn.Synthetic == "" || !strings.HasSuffix(fn.Name(), "$bound") && !strings.HasSuffix(fn.Name(), "$thunk") {
		return fn
	}
	if obj, ok := fn.Object().(*types.Func); ok {
		if method := fn.Prog.FuncValue(obj.Origin()); method != nil {
			return method
		}
	}
	return fn
}
