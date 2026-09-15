package directive

import "go/ast"

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
	//declscope:package // funcset.go and ignore.go walk function declarations with the same filter
	funcDeclTypes = []ast.Node{(*ast.FuncDecl)(nil)}
	//declscope:package // funcset.go filters function literals with it
	funcLitTypes = []ast.Node{(*ast.FuncLit)(nil)}
)
