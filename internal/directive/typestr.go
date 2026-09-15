// typestr.go turns a type or an expression into the string the directive
// matchers compare against. pure.go held it under a "Type String Helpers"
// heading, and immutable_input.go reaches for it too.

package directive

import (
	"go/ast"
	"go/types"
	"strings"
)

// stripPointer removes leading "*" from a type string.
//
//declscope:package // immutable_input.go compares receiver type strings too
func stripPointer(s string) string {
	return strings.TrimPrefix(s, "*")
}

// exprToString converts an ast.Expr to a string representation.
// For generic types like GenericReceiver[T], returns just the base type name.
//
//declscope:package // immutable_input.go compares receiver type strings too
func exprToString(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.StarExpr:
		return "*" + exprToString(e.X)
	case *ast.SelectorExpr:
		return exprToString(e.X) + "." + e.Sel.Name
	case *ast.IndexExpr:
		// Generic type with single type parameter: Type[T] -> Type
		return exprToString(e.X)
	case *ast.IndexListExpr:
		// Generic type with multiple type parameters: Type[T, U] -> Type
		return exprToString(e.X)
	default:
		return ""
	}
}

// formatReceiverType extracts the base type name from a receiver type.
// Returns just the type name without pointer (e.g., "Orm" for both *Orm and Orm).
// Go doesn't allow both pointer and value receivers with the same method name,
// so the pointer is irrelevant for matching.
//
//declscope:package // immutable_input.go compares receiver type strings too
func formatReceiverType(t types.Type) string {
	// Unwrap pointer if present
	if ptr, ok := types.Unalias(t).(*types.Pointer); ok {
		t = ptr.Elem()
	}
	if named, ok := types.Unalias(t).(*types.Named); ok {
		return named.Obj().Name()
	}
	return ""
}
