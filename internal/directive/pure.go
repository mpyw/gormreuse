package directive

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"strings"

	"golang.org/x/tools/go/ssa"
)

// =============================================================================
// Type String Helpers (for receiver type matching)
// =============================================================================

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

// FuncKey identifies a function by package, receiver type, and name.
type FuncKey struct {
	PkgPath      string // Package path (e.g., "github.com/example/pkg")
	ReceiverType string // Receiver type name without pointer (e.g., "Orm"), empty for functions
	FuncName     string // Function or method name
}

// directiveChecker is a function that checks if a comment is a specific directive.
type directiveChecker func(text string) bool

// DirectiveFuncSet is a generic set of functions matching a directive.
// It supports both pre-built sets (for current package) and on-demand
// directive checking (for external packages via source parsing).
type DirectiveFuncSet struct {
	known       map[FuncKey]struct{}
	fset        *token.FileSet
	cache       map[string]*ast.File
	isDirective directiveChecker
}

// newDirectiveFuncSet creates a new DirectiveFuncSet with the given directive checker.
func newDirectiveFuncSet(fset *token.FileSet, isDirective directiveChecker) *DirectiveFuncSet {
	return &DirectiveFuncSet{
		known:       make(map[FuncKey]struct{}),
		fset:        fset,
		cache:       make(map[string]*ast.File),
		isDirective: isDirective,
	}
}

// Add adds a function key to the set.
func (s *DirectiveFuncSet) Add(key FuncKey) {
	if s != nil && s.known != nil {
		s.known[key] = struct{}{}
	}
}

// Contains checks if the given SSA function is in the set or has the directive.
func (s *DirectiveFuncSet) Contains(fn *ssa.Function) bool {
	if fn == nil {
		return false
	}

	// First, check the pre-built set (for current package)
	if s != nil && s.known != nil {
		key := FuncKey{FuncName: fn.Name()}
		if fn.Pkg != nil && fn.Pkg.Pkg != nil {
			key.PkgPath = fn.Pkg.Pkg.Path()
		}
		if sig := fn.Signature; sig != nil && sig.Recv() != nil {
			key.ReceiverType = formatReceiverType(sig.Recv().Type())
		}
		if _, exists := s.known[key]; exists {
			return true
		}
	}

	// Second, check the SSA function's syntax for directive (for external packages)
	return s.hasDirective(fn)
}

// hasDirective checks if an SSA function has the directive.
func (s *DirectiveFuncSet) hasDirective(fn *ssa.Function) bool {
	if fn == nil {
		return false
	}

	// Try getting syntax from the SSA function (works for current package)
	if syntax := fn.Syntax(); syntax != nil {
		if funcDecl, ok := syntax.(*ast.FuncDecl); ok && funcDecl.Doc != nil {
			for _, c := range funcDecl.Doc.List {
				if s.isDirective(c.Text) {
					return true
				}
			}
		}
	}

	// Fallback: parse the source file for external packages
	if s == nil || s.fset == nil {
		return false
	}
	obj := fn.Object()
	if obj == nil {
		return false
	}
	pos := obj.Pos()
	if !pos.IsValid() {
		return false
	}
	filename := s.fset.Position(pos).Filename
	if filename == "" {
		return false
	}
	file := s.parseFile(filename)
	if file == nil {
		return false
	}

	funcName := fn.Name()
	var receiverType string
	if sig := fn.Signature; sig != nil && sig.Recv() != nil {
		receiverType = formatReceiverType(sig.Recv().Type())
	}
	return s.hasDirectiveInFile(file, funcName, receiverType)
}

// parseFile parses a Go source file with caching.
func (s *DirectiveFuncSet) parseFile(filename string) *ast.File {
	if file, ok := s.cache[filename]; ok {
		return file
	}
	file, err := parser.ParseFile(s.fset, filename, nil, parser.ParseComments)
	if err != nil {
		s.cache[filename] = nil
		return nil
	}
	s.cache[filename] = file
	return file
}

// hasDirectiveInFile checks if a function in a file has the directive.
func (s *DirectiveFuncSet) hasDirectiveInFile(file *ast.File, funcName, receiverType string) bool {
	for _, decl := range file.Decls {
		funcDecl, ok := decl.(*ast.FuncDecl)
		if !ok || funcDecl.Name.Name != funcName {
			continue
		}
		declReceiverType := ""
		if funcDecl.Recv != nil && len(funcDecl.Recv.List) > 0 {
			declReceiverType = stripPointer(exprToString(funcDecl.Recv.List[0].Type))
		}
		if declReceiverType != receiverType {
			continue
		}
		if funcDecl.Doc != nil {
			for _, c := range funcDecl.Doc.List {
				if s.isDirective(c.Text) {
					return true
				}
			}
		}
	}
	return false
}

// NewPureFuncSet creates a DirectiveFuncSet for //gormreuse:pure.
func NewPureFuncSet(fset *token.FileSet) *DirectiveFuncSet {
	return newDirectiveFuncSet(fset, IsPureDirective)
}

// NewImmutableReturnFuncSet creates a DirectiveFuncSet for //gormreuse:immutable-return.
func NewImmutableReturnFuncSet(fset *token.FileSet) *DirectiveFuncSet {
	return newDirectiveFuncSet(fset, IsImmutableReturnDirective)
}

// BuildPureFunctionSet builds a set of functions marked with //gormreuse:pure.
func BuildPureFunctionSet(file *ast.File, pkgPath string) map[FuncKey]struct{} {
	return buildFunctionSet(file, pkgPath, IsPureDirective)
}

// BuildImmutableReturnFunctionSet builds a set of functions marked with //gormreuse:immutable-return.
func BuildImmutableReturnFunctionSet(file *ast.File, pkgPath string) map[FuncKey]struct{} {
	return buildFunctionSet(file, pkgPath, IsImmutableReturnDirective)
}

// =============================================================================
// Common Helper
// =============================================================================

// buildFunctionSet builds a set of functions matching the given directive checker.
func buildFunctionSet(file *ast.File, pkgPath string, isDirective func(string) bool) map[FuncKey]struct{} {
	result := make(map[FuncKey]struct{})

	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.FuncDecl:
			if node.Doc != nil {
				for _, c := range node.Doc.List {
					if isDirective(c.Text) {
						key := FuncKey{
							PkgPath:  pkgPath,
							FuncName: node.Name.Name,
						}
						if node.Recv != nil && len(node.Recv.List) > 0 {
							key.ReceiverType = stripPointer(exprToString(node.Recv.List[0].Type))
						}
						result[key] = struct{}{}
						break
					}
				}
			}
		}
		return true
	})

	return result
}
