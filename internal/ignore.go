package internal

import (
	"go/ast"
	"go/token"
	"strings"
)

// ignoreEntry tracks an ignore directive and whether it was used.
type ignoreEntry struct {
	pos  token.Pos // Position of the ignore comment
	used bool      // Whether this ignore was actually used to suppress a warning
}

// IgnoreMap tracks line numbers that have ignore comments.
type IgnoreMap map[int]*ignoreEntry

// BuildIgnoreMap scans a file for ignore comments and returns a map.
// It also handles file-level and function-level ignore directives.
func BuildIgnoreMap(fset *token.FileSet, file *ast.File) IgnoreMap {
	m := make(IgnoreMap)

	// Check for file-level ignore (comment before package declaration)
	for _, cg := range file.Comments {
		for _, c := range cg.List {
			pos := fset.Position(c.Pos())
			// File-level ignore: before package or at the very start
			if isIgnoreComment(c.Text) {
				// Mark this line
				m[pos.Line] = &ignoreEntry{pos: c.Pos(), used: false}
			}
		}
	}

	// Check for file-level ignore in doc comments
	if file.Doc != nil {
		for _, c := range file.Doc.List {
			if isIgnoreComment(c.Text) {
				// File-level ignore: mark all lines as ignored
				// We use line -1 as a special marker
				// File-level ignores are always considered "used" (no warning for them)
				m[-1] = &ignoreEntry{pos: c.Pos(), used: true}
			}
		}
	}

	return m
}

// isIgnoreComment checks if a comment is an ignore directive.
// Supports both "//gormreuse:ignore" and "// gormreuse:ignore".
func isIgnoreComment(text string) bool {
	text = strings.TrimPrefix(text, "//")
	text = strings.TrimSpace(text)
	return strings.HasPrefix(text, "gormreuse:ignore")
}

// isPureComment checks if a comment is a pure directive.
// Supports both "//gormreuse:pure" and "// gormreuse:pure".
// Functions marked pure are assumed NOT to pollute *gorm.DB arguments.
func isPureComment(text string) bool {
	text = strings.TrimPrefix(text, "//")
	text = strings.TrimSpace(text)
	return strings.HasPrefix(text, "gormreuse:pure")
}

// ShouldIgnore returns true if the given line should be ignored.
// It checks if:
// - File-level ignore is active (marker at line -1)
// - The same line has an ignore comment
// - The previous line has an ignore comment
// When an ignore is used, it marks the entry as used.
func (m IgnoreMap) ShouldIgnore(line int) bool {
	// File-level ignore
	if entry, fileIgnore := m[-1]; fileIgnore {
		entry.used = true
		return true
	}
	if entry, onSameLine := m[line]; onSameLine {
		entry.used = true
		return true
	}
	if entry, onPrevLine := m[line-1]; onPrevLine {
		entry.used = true
		return true
	}
	return false
}

// GetUnusedIgnores returns the positions of ignore directives that were not used.
func (m IgnoreMap) GetUnusedIgnores() []token.Pos {
	var unused []token.Pos
	for line, entry := range m {
		if line == -1 {
			// Skip file-level ignores
			continue
		}
		if !entry.used {
			unused = append(unused, entry.pos)
		}
	}
	return unused
}

// MarkUsed marks the ignore directive at the given line as used.
func (m IgnoreMap) MarkUsed(line int) {
	if entry, ok := m[line]; ok {
		entry.used = true
	}
}

// BuildFunctionIgnoreSet builds a set of functions that should be ignored.
// Returns a map of function name positions to ignore.
// We use Name.Pos() because SSA's Function.Pos() returns the name position.
func BuildFunctionIgnoreSet(fset *token.FileSet, file *ast.File) map[token.Pos]struct{} {
	result := make(map[token.Pos]struct{})

	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.FuncDecl:
			if node.Doc != nil {
				for _, c := range node.Doc.List {
					if isIgnoreComment(c.Text) {
						// Use Name.Pos() to match SSA's fn.Pos()
						result[node.Name.Pos()] = struct{}{}
						break
					}
				}
			}
		case *ast.FuncLit:
			// Function literals don't have doc comments in Go
			// But we can check for inline comments
		}
		return true
	})

	return result
}

// BuildPureFunctionSet builds a set of functions marked as pure.
// Returns a map of function name (package path + function name) to struct{}.
// Functions marked pure are assumed NOT to pollute *gorm.DB arguments.
func BuildPureFunctionSet(fset *token.FileSet, file *ast.File, pkgPath string) map[string]struct{} {
	result := make(map[string]struct{})

	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.FuncDecl:
			if node.Doc != nil {
				for _, c := range node.Doc.List {
					if isPureComment(c.Text) {
						// Build full function name: pkgPath.FuncName
						// For methods: pkgPath.(ReceiverType).MethodName
						funcName := node.Name.Name
						if node.Recv != nil && len(node.Recv.List) > 0 {
							// Method - include receiver type
							recvType := exprToString(node.Recv.List[0].Type)
							funcName = "(" + recvType + ")." + funcName
						}
						fullName := pkgPath + "." + funcName
						result[fullName] = struct{}{}
						break
					}
				}
			}
		}
		return true
	})

	return result
}

// exprToString converts an ast.Expr to a string representation.
func exprToString(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.StarExpr:
		return "*" + exprToString(e.X)
	case *ast.SelectorExpr:
		return exprToString(e.X) + "." + e.Sel.Name
	default:
		return ""
	}
}
