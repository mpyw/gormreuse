package directive

import (
	"go/ast"
	"go/types"
	"strings"

	"golang.org/x/tools/go/ssa"
)

// =============================================================================
// Shared Function-Level Primitives
// =============================================================================

// FuncKey identifies a function by package, receiver type, and name.
type FuncKey struct {
	PkgPath      string // Package path (e.g., "github.com/example/pkg")
	ReceiverType string // Receiver type name without pointer (e.g., "Orm"), empty for functions
	FuncName     string // Function or method name
}

// Node type filters for inspector
var (
	//declscope:shared // funcset.go and ignore.go walk function declarations with the same filter
	funcDeclTypes = []ast.Node{(*ast.FuncDecl)(nil)}
	//declscope:shared // funcset.go filters function literals with it
	funcLitTypes = []ast.Node{(*ast.FuncLit)(nil)}
)

// DeclaredFunc returns the function a directive is written on for fn, the
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
func DeclaredFunc(fn *ssa.Function) *ssa.Function {
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

// funcKeyOf returns the key a directive on fn is stored under. fn must be a
// declared function (see DeclaredFunc).
//
//declscope:shared // funcset.go and immutable_input.go look their sets up by it
func funcKeyOf(fn *ssa.Function) FuncKey {
	key := FuncKey{FuncName: fn.Name()}
	if fn.Pkg != nil && fn.Pkg.Pkg != nil {
		key.PkgPath = fn.Pkg.Pkg.Path()
	}
	if sig := fn.Signature; sig != nil && sig.Recv() != nil {
		key.ReceiverType = receiverTypeString(sig.Recv().Type())
	}
	return key
}
