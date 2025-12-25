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

	// PHASE 1: Add reassignments for non-finisher uses (Rule 1)
	for _, use := range nonFinisherUses {
		if edit := g.generateReassignmentEdit(use.Pos); edit != nil {
			edits = append(edits, *edit)
		}
	}

	// PHASE 2: Add Session to roots that need it (Rule 2)
	// Special handling for Phi nodes: add Session to each edge
	for _, pos := range rootsNeedingSession {
		sessionEdits := g.generateSessionEditsForRoot(pos, root)
		edits = append(edits, sessionEdits...)
	}

	if len(edits) == 0 {
		return nil
	}

	// Sort edits by position (earlier positions first)
	// This ensures correct application order
	sort.Slice(edits, func(i, j int) bool {
		return edits[i].Pos < edits[j].Pos
	})

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

// virtualRootKey represents a virtual root (either original or created by reassignment).
type virtualRootKey struct {
	isOriginal bool
	pos        token.Pos // For virtual roots, the position where created
}

// simulateReassignments simulates Phase 1 reassignments and returns virtual uses.
// Returns a map from virtual root keys to their uses.
func (g *Generator) simulateReassignments(root ssa.Value, uses []pollution.UsageInfo) map[virtualRootKey][]pollution.UsageInfo {
	// Sort uses by position
	sortedUses := make([]pollution.UsageInfo, len(uses))
	copy(sortedUses, uses)
	sort.Slice(sortedUses, func(i, j int) bool {
		return sortedUses[i].Pos < sortedUses[j].Pos
	})

	// Start with the original root
	currentRootKey := virtualRootKey{isOriginal: true, pos: root.Pos()}
	virtualUses := make(map[virtualRootKey][]pollution.UsageInfo)

	for _, use := range sortedUses {
		// This use is from currentRoot
		virtualUses[currentRootKey] = append(virtualUses[currentRootKey], use)

		// If non-finisher, this use's result becomes the new root
		if g.isNonFinisherExprStmt(use.Pos) {
			// Create a new virtual root at this position
			currentRootKey = virtualRootKey{isOriginal: false, pos: use.Pos}
		}
	}

	return virtualUses
}

// findRootsNeedingSession finds virtual roots that have 2+ uses after simulation.
// Returns the positions where Session should be inserted.
func (g *Generator) findRootsNeedingSession(virtualUses map[virtualRootKey][]pollution.UsageInfo) []token.Pos {
	var positions []token.Pos
	for root, uses := range virtualUses {
		if len(uses) >= 2 {
			// Only add Session to the original root (not virtual roots from reassignments)
			// Virtual roots from reassignments are created by non-finisher uses,
			// which already get reassignment edits
			if root.isOriginal {
				positions = append(positions, root.pos)
			}
		}
	}
	return positions
}

// generateReassignmentEdit generates a TextEdit for reassignment.
// Inserts "q = " before the statement.
func (g *Generator) generateReassignmentEdit(pos token.Pos) *analysis.TextEdit {
	varName := g.getVariableNameAtPos(pos)
	if varName == "" {
		return nil
	}

	// Find the statement containing this position
	file := g.findFileContaining(pos)
	if file == nil {
		return nil
	}

	stmt := g.findStmtAtPos(file, pos)
	if stmt == nil {
		return nil
	}

	// Insert at the beginning of the statement
	return &analysis.TextEdit{
		Pos:     stmt.Pos(),
		End:     stmt.Pos(),
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

// generateSessionEditsForRoot generates Session edits, handling Phi nodes specially.
// For Phi nodes, generates edits for each incoming edge.
// For non-Phi nodes, generates a single edit.
func (g *Generator) generateSessionEditsForRoot(pos token.Pos, root ssa.Value) []analysis.TextEdit {
	// Check if root is a Phi node
	if phi, isPhi := root.(*ssa.Phi); isPhi {
		// Phi node: generate edit for each edge
		return g.generatePhiEdgeEdits(phi)
	}

	// Check if root is part of a Phi (root is an edge)
	// Look for Phi nodes that use this root as an edge
	if phi := g.findPhiUsingValue(root); phi != nil {
		// This root is part of a Phi - fix all edges
		return g.generatePhiEdgeEdits(phi)
	}

	// Not related to Phi - generate single edit
	if edit := g.generateSessionEdit(pos); edit != nil {
		return []analysis.TextEdit{*edit}
	}
	return nil
}

// generatePhiEdgeEdits generates Session edits for all edges of a Phi node.
// Only generates edits for edges that don't already have Session.
func (g *Generator) generatePhiEdgeEdits(phi *ssa.Phi) []analysis.TextEdit {
	var edits []analysis.TextEdit
	for _, edge := range phi.Edges {
		// Skip nil constants
		if c, ok := edge.(*ssa.Const); ok && c.Value == nil {
			continue
		}

		// Skip if this edge is already immutable (has Session, WithContext, etc.)
		if g.isImmutableValue(edge) {
			continue
		}

		// Get the position of this edge's definition
		edgePos := edge.Pos()
		if edgePos == token.NoPos {
			continue
		}

		// Generate Session edit for this edge
		if edit := g.generateSessionEdit(edgePos); edit != nil {
			edits = append(edits, *edit)
		}
	}
	return edits
}

// findPhiUsingValue finds a Phi node that uses the given value as an edge,
// or finds all values stored to the same Alloc (implicit Phi through memory).
// Returns a Phi node if found, or synthesizes one from Alloc stores.
func (g *Generator) findPhiUsingValue(v ssa.Value) *ssa.Phi {
	if v == nil {
		return nil
	}

	// Get the function containing this value
	var fn *ssa.Function
	switch val := v.(type) {
	case ssa.Instruction:
		fn = val.Parent()
	case *ssa.Parameter:
		fn = val.Parent()
	default:
		return nil
	}

	if fn == nil {
		return nil
	}

	// Case 1: Direct Phi node (value flows through SSA values)
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			phi, ok := instr.(*ssa.Phi)
			if !ok {
				continue
			}

			// Check if v is one of the Phi's edges
			for _, edge := range phi.Edges {
				if edge == v {
					return phi
				}
			}
		}
	}

	// Case 2: Implicit Phi through Alloc/Store (var q *gorm.DB with conditional stores)
	// Find if this value is stored to an Alloc that has multiple stores
	var targetAlloc *ssa.Alloc
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			store, ok := instr.(*ssa.Store)
			if !ok || store.Val != v {
				continue
			}
			// Found a store of v to an Alloc
			if alloc, ok := store.Addr.(*ssa.Alloc); ok {
				targetAlloc = alloc
				break
			}
		}
		if targetAlloc != nil {
			break
		}
	}

	if targetAlloc == nil {
		return nil
	}

	// Find all values stored to this Alloc
	var storedValues []ssa.Value
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			store, ok := instr.(*ssa.Store)
			if !ok || store.Addr != targetAlloc {
				continue
			}
			storedValues = append(storedValues, store.Val)
		}
	}

	// If multiple values are stored (conditional branches), create synthetic Phi
	if len(storedValues) > 1 {
		phi := &ssa.Phi{
			Edges: storedValues,
		}
		// Set the phi's register to match the function (needed for Parent())
		// We can't fully initialize it, but the Edges are enough for fix generation
		return phi
	}

	return nil
}

// isImmutableValue checks if an SSA value is immutable (already has Session/WithContext/etc).
// Traces through the call chain to find if there's an immutable-returning method.
// Handles chains like: db.Where("x").Session(&gorm.Session{}).Where("y")
func (g *Generator) isImmutableValue(v ssa.Value) bool {
	visited := make(map[ssa.Value]bool)
	return g.traceForImmutable(v, visited)
}

// traceForImmutable recursively traces through method chains to find immutable-returning calls.
func (g *Generator) traceForImmutable(v ssa.Value, visited map[ssa.Value]bool) bool {
	if v == nil || visited[v] {
		return false
	}
	visited[v] = true

	call, ok := v.(*ssa.Call)
	if !ok {
		return false
	}

	callee := call.Call.StaticCallee()
	if callee == nil {
		return false
	}

	// Check if this call is an immutable-returning method
	methodName := callee.Name()
	immutableMethods := map[string]bool{
		"Session":     true,
		"WithContext": true,
		"Debug":       true,
		"Open":        true,
		"Begin":       true,
		"Transaction": true,
	}

	if immutableMethods[methodName] {
		return true
	}

	// Trace through the receiver (for method chains)
	// If call is: receiver.Method(...), check receiver
	if len(call.Call.Args) > 0 {
		receiver := call.Call.Args[0]
		if g.traceForImmutable(receiver, visited) {
			return true
		}
	}

	return false
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

// getVariableNameAtPos gets the assignable left-hand side expression at the given position.
// Returns the full expression that can be assigned to (e.g., "q", "obj.field").
// Returns empty string if not assignable (e.g., function call result).
func (g *Generator) getVariableNameAtPos(pos token.Pos) string {
	file := g.findFileContaining(pos)
	if file == nil {
		return ""
	}

	// Find the CallExpr that contains or starts at this position
	var lhs string
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			return false
		}

		// Look for CallExpr that matches the position
		if callExpr, ok := n.(*ast.CallExpr); ok {
			// Check if this is the call at the target position
			if callExpr.Pos() <= pos && pos <= callExpr.End() {
				// Extract receiver from selector (e.g., q.Where -> q)
				if sel, ok := callExpr.Fun.(*ast.SelectorExpr); ok {
					lhs = g.extractAssignableLHS(sel.X)
					return false // Found it, stop searching
				}
			}
		}

		return n.Pos() <= pos
	})

	return lhs
}

// extractAssignableLHS extracts the assignable left-hand side from an expression.
// Returns empty string if the expression is not assignable.
//
// Assignable expressions in Go:
// - Identifiers: x
// - Field selectors: x.f
// - Index expressions: x[i]
// - Pointer indirection: *ptr
// - Parenthesized expressions: (x)
//
// Not assignable:
// - Function calls: f()
// - Slice expressions: x[i:j]
// - Type assertions: x.(T)
// - Literals, composite literals
func (g *Generator) extractAssignableLHS(expr ast.Expr) string {
	// Use a buffer to reconstruct the expression as we traverse
	return g.extractAssignableLHSImpl(expr)
}

func (g *Generator) extractAssignableLHSImpl(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		// Simple identifier: q
		return e.Name

	case *ast.SelectorExpr:
		// Field selector: obj.field
		base := g.extractAssignableLHSImpl(e.X)
		if base == "" {
			return ""
		}
		return base + "." + e.Sel.Name

	case *ast.IndexExpr:
		// Index expression: arr[i], map[key]
		// Assignable, but complex to reconstruct with dynamic index
		// For now, return empty to skip
		// TODO: Could use go/printer to format the full expression
		return ""

	case *ast.StarExpr:
		// Pointer indirection: *ptr
		base := g.extractAssignableLHSImpl(e.X)
		if base == "" {
			return ""
		}
		return "*" + base

	case *ast.ParenExpr:
		// Parenthesized expression: (x)
		// Recurse to unwrap
		return g.extractAssignableLHSImpl(e.X)

	case *ast.CallExpr:
		// Function call: not assignable
		return ""

	case *ast.SliceExpr:
		// Slice expression: not assignable
		return ""

	case *ast.TypeAssertExpr:
		// Type assertion: not assignable
		return ""

	case *ast.BasicLit, *ast.CompositeLit:
		// Literals: not assignable
		return ""

	default:
		// Other expressions: conservatively treat as not assignable
		return ""
	}
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
