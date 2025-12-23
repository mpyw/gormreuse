package directive

import (
	"go/ast"
	"go/token"
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
//
// Example:
//
//	//gormreuse:ignore         // Line 5 → map[5] (line-level)
//	q.Find(nil)                // Line 6 → ignored (line 5 covers line 6)
//
//	// File-level ignore (in package doc):
//	// gormreuse:ignore        // → map[-1] (special marker)
//	package main               // All lines ignored
//
// The returned map uses line numbers as keys:
//   - Positive line: ignore directive for next line
//   - Line -1: file-level ignore (all lines)
func BuildIgnoreMap(fset *token.FileSet, file *ast.File) IgnoreMap {
	m := make(IgnoreMap)

	// Check for file-level ignore (comment before package declaration)
	for _, cg := range file.Comments {
		for _, c := range cg.List {
			pos := fset.Position(c.Pos())
			// File-level ignore: before package or at the very start
			if IsIgnoreDirective(c.Text) {
				// Mark this line
				m[pos.Line] = &ignoreEntry{pos: c.Pos(), used: false}
			}
		}
	}

	// Check for file-level ignore in doc comments
	if file.Doc != nil {
		for _, c := range file.Doc.List {
			if IsIgnoreDirective(c.Text) {
				// File-level ignore: mark all lines as ignored
				// We use line -1 as a special marker
				// File-level ignores are always considered "used" (no warning for them)
				m[-1] = &ignoreEntry{pos: c.Pos(), used: true}
			}
		}
	}

	return m
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

// FunctionIgnoreEntry represents a function-level ignore directive.
type FunctionIgnoreEntry struct {
	DirectiveLine int // Line number of the ignore directive (for marking as used)
}

// BuildFunctionIgnoreSet builds a set of functions that should be ignored.
// Returns a map of function name positions to ignore entry.
// We use Name.Pos() because SSA's Function.Pos() returns the name position.
func BuildFunctionIgnoreSet(fset *token.FileSet, file *ast.File) map[token.Pos]FunctionIgnoreEntry {
	result := make(map[token.Pos]FunctionIgnoreEntry)

	ast.Inspect(file, func(n ast.Node) bool {
		// Only handle FuncDecl - FuncLit (function literals) don't have doc comments in Go
		fd, ok := n.(*ast.FuncDecl)
		if !ok || fd.Doc == nil {
			return true
		}
		for _, c := range fd.Doc.List {
			if IsIgnoreDirective(c.Text) {
				// Use Name.Pos() to match SSA's fn.Pos()
				result[fd.Name.Pos()] = FunctionIgnoreEntry{
					DirectiveLine: fset.Position(c.Pos()).Line,
				}
				break
			}
		}
		return true
	})

	return result
}
