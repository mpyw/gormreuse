// typestr.go turns a type or an expression into the string the directive
// matchers compare against. funcset.go (formerly pure.go) held it under a
// "Type String Helpers" heading, and immutable_input.go reaches for it too.

package directive

import (
	"go/ast"
	"go/types"
	"strings"
)

// receiverTypeStringFromExpr extracts the base type name from a receiver type
// expression, without the pointer marker (e.g., "Orm" for both *Orm and Orm) —
// the AST-side counterpart of receiverTypeString.
//
//declscope:package // immutable_input.go compares receiver type strings too
func receiverTypeStringFromExpr(expr ast.Expr) string {
	return strings.TrimPrefix(exprToTypeString(expr), "*")
}

// exprToTypeString converts a type ast.Expr to a string representation.
// For generic types like GenericReceiver[T], returns just the base type name.
//
//declscope:package // immutable_input.go compares receiver type strings too
func exprToTypeString(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.StarExpr:
		return "*" + exprToTypeString(e.X)
	case *ast.SelectorExpr:
		return exprToTypeString(e.X) + "." + e.Sel.Name
	case *ast.IndexExpr:
		// Generic type with single type parameter: Type[T] -> Type
		return exprToTypeString(e.X)
	case *ast.IndexListExpr:
		// Generic type with multiple type parameters: Type[T, U] -> Type
		return exprToTypeString(e.X)
	default:
		return ""
	}
}

// receiverTypeString extracts the base type name from a receiver type.
// Returns just the type name without pointer (e.g., "Orm" for both *Orm and Orm).
// Go doesn't allow both pointer and value receivers with the same method name,
// so the pointer is irrelevant for matching.
//
//declscope:package // immutable_input.go compares receiver type strings too
func receiverTypeString(t types.Type) string {
	// Unwrap pointer if present
	if ptr, ok := types.Unalias(t).(*types.Pointer); ok {
		t = ptr.Elem()
	}
	if named, ok := types.Unalias(t).(*types.Named); ok {
		return named.Obj().Name()
	}
	return ""
}
