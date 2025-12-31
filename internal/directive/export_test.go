package directive

import (
	"go/ast"
	"go/token"
	"go/types"
)

// Export unexported functions for testing.

// ExprToString exports exprToString for external tests.
func ExprToString(expr ast.Expr) string {
	return exprToString(expr)
}

// ContainsGormDB exports containsGormDB for external tests.
func ContainsGormDB(t types.Type) bool {
	return containsGormDB(t)
}

// Add exports the ability to add an entry to IgnoreMap for external tests.
// For file-level ignores (line = -1), the entry is marked as used by default
// to match the behavior of BuildIgnoreMap.
func (m IgnoreMap) Add(line int, pos token.Pos) {
	if line == -1 {
		// File-level ignores are always considered "used" (no warning for them)
		m[line] = &ignoreEntry{pos: pos, used: true}
	} else {
		m[line] = &ignoreEntry{pos: pos, used: false}
	}
}

// ContainsKey exports the ability to check if a FuncKey is in the set for external tests.
func (s *DirectiveFuncSet) ContainsKey(key FuncKey) bool {
	if s == nil || s.known == nil {
		return false
	}
	_, exists := s.known[key]
	return exists
}
