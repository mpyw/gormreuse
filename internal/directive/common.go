package directive

import (
	"go/ast"
	"go/types"
	"strings"
)

// IsIgnoreDirective checks if a comment is an ignore directive.
// Supports both "//gormreuse:ignore" and "// gormreuse:ignore".
func IsIgnoreDirective(text string) bool {
	text = strings.TrimPrefix(text, "//")
	text = strings.TrimSpace(text)
	return strings.HasPrefix(text, "gormreuse:ignore")
}

// IsPureDirective checks if a comment is a pure directive.
// Supports both "//gormreuse:pure" and "// gormreuse:pure".
// Functions marked pure are assumed NOT to pollute *gorm.DB arguments.
func IsPureDirective(text string) bool {
	text = strings.TrimPrefix(text, "//")
	text = strings.TrimSpace(text)
	return strings.HasPrefix(text, "gormreuse:pure")
}

// stripPointer removes leading "*" from a type string.
func stripPointer(s string) string {
	return strings.TrimPrefix(s, "*")
}

// exprToString converts an ast.Expr to a string representation.
// For generic types like GenericReceiver[T], returns just the base type name.
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
func formatReceiverType(t types.Type) string {
	// Unwrap pointer if present
	if ptr, ok := t.(*types.Pointer); ok {
		t = ptr.Elem()
	}
	if named, ok := t.(*types.Named); ok {
		return named.Obj().Name()
	}
	return ""
}
