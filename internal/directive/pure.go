package directive

import (
	"go/ast"
	"go/parser"
	"go/token"

	"golang.org/x/tools/go/ssa"
)

// PureFuncKey identifies a function marked as pure.
// This provides a structured way to match AST declarations with SSA functions,
// avoiding fragile string comparison with fn.String().
type PureFuncKey struct {
	PkgPath      string // Package path (e.g., "github.com/example/pkg")
	ReceiverType string // Receiver type name without pointer/package (e.g., "Orm"), empty for functions
	FuncName     string // Function or method name
}

// PureFuncSet is a set of pure functions with caching for external packages.
type PureFuncSet struct {
	known map[PureFuncKey]struct{}
	fset  *token.FileSet
	cache map[string]*ast.File // cached parsed files
}

// NewPureFuncSet creates a new PureFuncSet.
func NewPureFuncSet(fset *token.FileSet) *PureFuncSet {
	return &PureFuncSet{
		known: make(map[PureFuncKey]struct{}),
		fset:  fset,
		cache: make(map[string]*ast.File),
	}
}

// Add adds a pure function key to the set.
func (s *PureFuncSet) Add(key PureFuncKey) {
	if s != nil && s.known != nil {
		s.known[key] = struct{}{}
	}
}

// Contains checks if the given SSA function is in the set or has a pure directive.
func (s *PureFuncSet) Contains(fn *ssa.Function) bool {
	if fn == nil {
		return false
	}

	// First, check the pre-built set (for current package)
	if s != nil && s.known != nil {
		key := PureFuncKey{
			FuncName: fn.Name(),
		}

		// Get package path
		if fn.Pkg != nil && fn.Pkg.Pkg != nil {
			key.PkgPath = fn.Pkg.Pkg.Path()
		}

		// Get receiver type for methods
		sig := fn.Signature
		if sig != nil && sig.Recv() != nil {
			key.ReceiverType = formatReceiverType(sig.Recv().Type())
		}

		if _, exists := s.known[key]; exists {
			return true
		}
	}

	// Second, check the SSA function's syntax for pure directive (for external packages)
	return s.hasPureDirective(fn)
}

// hasPureDirective checks if an SSA function has a //gormreuse:pure directive.
// This allows detecting pure functions in external packages.
func (s *PureFuncSet) hasPureDirective(fn *ssa.Function) bool {
	if fn == nil {
		return false
	}

	// Try getting syntax from the SSA function (works for current package)
	syntax := fn.Syntax()
	if syntax != nil {
		if funcDecl, ok := syntax.(*ast.FuncDecl); ok && funcDecl.Doc != nil {
			for _, c := range funcDecl.Doc.List {
				if IsPureDirective(c.Text) {
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

	// Get the filename from the position
	position := s.fset.Position(pos)
	filename := position.Filename
	if filename == "" {
		return false
	}

	// Parse the file (with caching)
	file := s.parseFile(filename)
	if file == nil {
		return false
	}

	// Find the function declaration at this position
	funcName := fn.Name()
	var receiverType string
	if sig := fn.Signature; sig != nil && sig.Recv() != nil {
		receiverType = formatReceiverType(sig.Recv().Type())
	}

	return s.hasPureDirectiveInFile(file, funcName, receiverType)
}

// parseFile parses a Go source file with caching.
func (s *PureFuncSet) parseFile(filename string) *ast.File {
	if file, ok := s.cache[filename]; ok {
		return file
	}

	// Parse the file
	file, err := parser.ParseFile(s.fset, filename, nil, parser.ParseComments)
	if err != nil {
		s.cache[filename] = nil
		return nil
	}

	s.cache[filename] = file
	return file
}

// hasPureDirectiveInFile checks if a function in a file has a pure directive.
func (s *PureFuncSet) hasPureDirectiveInFile(file *ast.File, funcName, receiverType string) bool {
	for _, decl := range file.Decls {
		funcDecl, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}

		// Check if this is the function we're looking for
		if funcDecl.Name.Name != funcName {
			continue
		}

		// Check receiver type
		declReceiverType := ""
		if funcDecl.Recv != nil && len(funcDecl.Recv.List) > 0 {
			declReceiverType = stripPointer(exprToString(funcDecl.Recv.List[0].Type))
		}

		if declReceiverType != receiverType {
			continue
		}

		// Found the function, check for pure directive
		if funcDecl.Doc != nil {
			for _, c := range funcDecl.Doc.List {
				if IsPureDirective(c.Text) {
					return true
				}
			}
		}
	}
	return false
}

// BuildPureFunctionSet builds a set of functions marked as pure.
// Returns a map of PureFuncKey that can be added to a PureFuncSet.
// Functions marked pure are assumed NOT to pollute *gorm.DB arguments.
//
// Example:
//
//	//gormreuse:pure
//	func safeQuery(db *gorm.DB) *gorm.DB {
//	    return db.Session(&gorm.Session{})
//	}
//	→ PureFuncKey{PkgPath: "...", FuncName: "safeQuery"}
//
//	//gormreuse:pure
//	func (h *Handler) GetDB() *gorm.DB {
//	    return h.db.Session(&gorm.Session{})
//	}
//	→ PureFuncKey{PkgPath: "...", ReceiverType: "Handler", FuncName: "GetDB"}
//
// Key generation:
//   - PkgPath: full import path (e.g., "github.com/user/pkg")
//   - ReceiverType: type name without pointer (e.g., "Handler" not "*Handler")
//   - FuncName: function or method name
func BuildPureFunctionSet(fset *token.FileSet, file *ast.File, pkgPath string) map[PureFuncKey]struct{} {
	result := make(map[PureFuncKey]struct{})

	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.FuncDecl:
			if node.Doc != nil {
				for _, c := range node.Doc.List {
					if IsPureDirective(c.Text) {
						key := PureFuncKey{
							PkgPath:  pkgPath,
							FuncName: node.Name.Name,
						}
						if node.Recv != nil && len(node.Recv.List) > 0 {
							// Method - extract receiver type (without pointer)
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
