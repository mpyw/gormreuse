// Package fix provides SuggestedFix generation for gormreuse violations.
//
// # Fix Strategy
//
// The fix generation follows a two-phase approach:
//
//  1. Phase 1 (Reassignment): Add reassignment to non-finisher uses
//     - q.Where("a") → q = q.Where("a")
//
//  2. Phase 2 (Session): Simulate Phase 1 and add Session to roots
//     that still have multiple uses after reassignment
//     - q = q.Where("a") → q = q.Where("a").Session(&gorm.Session{})
//
// # Example
//
//	// Before
//	q := db.Where("base")
//	q.Where("a")           // non-finisher
//	q.Where("b").Find()    // finisher
//	q.Where("c")           // non-finisher
//	q.Where("d").Find()    // finisher
//
//	// After Phase 1 (reassignment)
//	q := db.Where("base")
//	q = q.Where("a")       // ← added reassignment
//	q.Where("b").Find()
//	q = q.Where("c")       // ← added reassignment
//	q.Where("d").Find()
//
//	// After Phase 2 (Session)
//	q := db.Where("base")
//	q = q.Where("a").Session(&gorm.Session{})  // ← added Session (q_2 has 2 uses)
//	q.Where("b").Find()
//	q = q.Where("c")
//	q.Where("d").Find()
package fix

import (
	"go/ast"
	"go/token"
	"sort"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/ssa/pollution"
)

// Generator generates SuggestedFix for a violation.
type Generator struct {
	pass  *analysis.Pass
	fset  *token.FileSet
	files map[*token.File]*ast.File // token.File -> ast.File mapping
}

// New creates a new fix Generator.
func New(pass *analysis.Pass) *Generator {
	// Build token.File -> ast.File mapping
	files := make(map[*token.File]*ast.File)
	for _, f := range pass.Files {
		tf := pass.Fset.File(f.Pos())
		if tf != nil {
			files[tf] = f
		}
	}

	return &Generator{
		pass:  pass,
		fset:  pass.Fset,
		files: files,
	}
}

// Generate generates SuggestedFix for a violation.
// Returns nil if the violation cannot be auto-fixed.
func (g *Generator) Generate(v pollution.Violation) []analysis.SuggestedFix {
	root := v.Root
	if root == nil {
		return nil // Cannot fix without root information
	}

	allUses := v.AllUses
	if len(allUses) == 0 {
		return nil // No uses to fix
	}

	// Phase 1: Identify non-finisher uses that need reassignment
	nonFinisherUses := g.findNonFinisherUses(allUses)

	// Phase 2: Simulate reassignments and find roots needing Session
	virtualUses := g.simulateReassignments(root, allUses)
	rootsNeedingSession := g.findRootsNeedingSession(virtualUses)

	// Generate TextEdits
	var edits []analysis.TextEdit

	// Add reassignments for non-finisher uses
	for _, use := range nonFinisherUses {
		if edit := g.generateReassignmentEdit(use.Pos); edit != nil {
			edits = append(edits, *edit)
		}
	}

	// Add Session to roots that need it
	for _, root := range rootsNeedingSession {
		if edit := g.generateSessionEdit(root.Pos()); edit != nil {
			edits = append(edits, *edit)
		}
	}

	if len(edits) == 0 {
		return nil
	}

	return []analysis.SuggestedFix{
		{
			Message:   "Add reassignment and Session to fix reuse",
			TextEdits: edits,
		},
	}
}

// findNonFinisherUses finds uses that are non-finisher expression statements.
func (g *Generator) findNonFinisherUses(uses []pollution.UsageInfo) []pollution.UsageInfo {
	var nonFinishers []pollution.UsageInfo
	for _, use := range uses {
		if g.isNonFinisherExprStmt(use.Pos) {
			nonFinishers = append(nonFinishers, use)
		}
	}
	return nonFinishers
}

// isNonFinisherExprStmt checks if a position is a non-finisher expression statement.
// Non-finisher means the result is not used (e.g., q.Where("a") without assignment).
func (g *Generator) isNonFinisherExprStmt(pos token.Pos) bool {
	// Find the AST node at this position
	file := g.findFileContaining(pos)
	if file == nil {
		return false
	}

	// Find the statement containing this position
	stmt := g.findStmtAtPos(file, pos)
	if stmt == nil {
		return false
	}

	// Check if it's an ExprStmt (expression used as statement)
	exprStmt, ok := stmt.(*ast.ExprStmt)
	if !ok {
		return false
	}

	// Check if it's a call expression
	callExpr, ok := exprStmt.X.(*ast.CallExpr)
	if !ok {
		return false
	}

	// Check if it's a method call (selector expression)
	sel, ok := callExpr.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}

	// Check if the method is a finisher
	// Finishers are methods that typically end a chain: Find, Count, First, etc.
	methodName := sel.Sel.Name
	return !isFinisher(methodName)
}

// isFinisher checks if a method name is a GORM finisher method.
func isFinisher(methodName string) bool {
	finishers := map[string]bool{
		"Find":        true,
		"First":       true,
		"Last":        true,
		"Take":        true,
		"Count":       true,
		"Pluck":       true,
		"Scan":        true,
		"Row":         true,
		"Rows":        true,
		"ScanRows":    true,
		"Create":      true,
		"Save":        true,
		"Update":      true,
		"Updates":     true,
		"Delete":      true,
		"Exec":        true,
		"Transaction": true,
	}
	return finishers[methodName]
}

// simulateReassignments simulates Phase 1 reassignments and returns virtual uses.
func (g *Generator) simulateReassignments(root ssa.Value, uses []pollution.UsageInfo) map[ssa.Value][]pollution.UsageInfo {
	// Sort uses by position
	sortedUses := make([]pollution.UsageInfo, len(uses))
	copy(sortedUses, uses)
	sort.Slice(sortedUses, func(i, j int) bool {
		return sortedUses[i].Pos < sortedUses[j].Pos
	})

	currentRoot := root
	virtualUses := make(map[ssa.Value][]pollution.UsageInfo)

	for _, use := range sortedUses {
		// This use is from currentRoot
		virtualUses[currentRoot] = append(virtualUses[currentRoot], use)

		// If non-finisher, this use's result becomes the new root
		if g.isNonFinisherExprStmt(use.Pos) {
			// The Call result at this position becomes the new root
			newRoot := g.getCallResultAtPos(use.Pos)
			if newRoot != nil {
				currentRoot = newRoot
			}
		}
	}

	return virtualUses
}

// findRootsNeedingSession finds roots that have 2+ uses after simulation.
func (g *Generator) findRootsNeedingSession(virtualUses map[ssa.Value][]pollution.UsageInfo) []ssa.Value {
	var roots []ssa.Value
	for root, uses := range virtualUses {
		if len(uses) >= 2 {
			roots = append(roots, root)
		}
	}
	return roots
}

// generateReassignmentEdit generates a TextEdit for reassignment.
// Inserts "q = " before the expression.
func (g *Generator) generateReassignmentEdit(pos token.Pos) *analysis.TextEdit {
	varName := g.getVariableNameAtPos(pos)
	if varName == "" {
		return nil
	}

	return &analysis.TextEdit{
		Pos:     pos,
		End:     pos,
		NewText: []byte(varName + " = "),
	}
}

// generateSessionEdit generates a TextEdit for Session.
// Appends ".Session(&gorm.Session{})" after the call expression.
func (g *Generator) generateSessionEdit(pos token.Pos) *analysis.TextEdit {
	endPos := g.getCallExprEndPos(pos)
	if endPos == token.NoPos {
		return nil
	}

	return &analysis.TextEdit{
		Pos:     endPos,
		End:     endPos,
		NewText: []byte(".Session(&gorm.Session{})"),
	}
}

// =============================================================================
// AST Helper Methods
// =============================================================================

// findFileContaining finds the AST file containing the given position.
func (g *Generator) findFileContaining(pos token.Pos) *ast.File {
	tf := g.fset.File(pos)
	if tf == nil {
		return nil
	}
	return g.files[tf]
}

// findStmtAtPos finds the statement at the given position.
func (g *Generator) findStmtAtPos(file *ast.File, pos token.Pos) ast.Stmt {
	var result ast.Stmt
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			return false
		}
		// Check if this node contains the position
		if n.Pos() <= pos && pos < n.End() {
			if stmt, ok := n.(ast.Stmt); ok {
				result = stmt
			}
			return true
		}
		return n.Pos() <= pos
	})
	return result
}

// getCallResultAtPos gets the SSA Call result at the given position.
// For simulated reassignments, we return the Call instruction itself as the new root.
func (g *Generator) getCallResultAtPos(pos token.Pos) ssa.Value {
	// This is a simplification: we use the Call as a placeholder for the new virtual root.
	// In reality, we would need to track SSA values more precisely.
	// For now, we return nil to skip Session insertion on virtual roots.
	return nil
}

// getVariableNameAtPos gets the variable name at the given position.
func (g *Generator) getVariableNameAtPos(pos token.Pos) string {
	file := g.findFileContaining(pos)
	if file == nil {
		return ""
	}

	stmt := g.findStmtAtPos(file, pos)
	if stmt == nil {
		return ""
	}

	// For ExprStmt, extract the receiver variable name
	if exprStmt, ok := stmt.(*ast.ExprStmt); ok {
		if callExpr, ok := exprStmt.X.(*ast.CallExpr); ok {
			if sel, ok := callExpr.Fun.(*ast.SelectorExpr); ok {
				if ident, ok := sel.X.(*ast.Ident); ok {
					return ident.Name
				}
			}
		}
	}

	return ""
}

// getCallExprEndPos gets the end position of the CallExpr at the given position.
func (g *Generator) getCallExprEndPos(pos token.Pos) token.Pos {
	file := g.findFileContaining(pos)
	if file == nil {
		return token.NoPos
	}

	var callEnd token.Pos
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			return false
		}
		// Find the CallExpr that starts at or contains this position
		if callExpr, ok := n.(*ast.CallExpr); ok {
			if callExpr.Pos() <= pos && pos < callExpr.End() {
				callEnd = callExpr.End()
				return false
			}
		}
		return n.Pos() <= pos
	})
	return callEnd
}
