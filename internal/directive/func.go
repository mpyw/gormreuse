package directive

import (
	"go/ast"

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

// funcKeyOf returns the key a directive on fn is stored under. fn must be a
// declared function (see origin.Declared).
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
