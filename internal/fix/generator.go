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
	"cmp"
	"go/ast"
	"go/token"
	"go/types"
	"slices"
	"strconv"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/go/ast/edge"
	"golang.org/x/tools/go/ast/inspector"
	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/ssa/pollution"
	"github.com/mpyw/gormreuse/internal/typeutil"
)

// Generator generates SuggestedFix for a violation.
type Generator struct {
	pass            *analysis.Pass
	fset            *token.FileSet
	files           map[*token.File]*ast.File      // token.File -> ast.File mapping
	cursors         map[*ast.File]inspector.Cursor // file cursors in the pass inspector
	scopesCallbacks map[*ssa.Function]bool         // Scopes/Preload callbacks (no immutable-param fix)
}

// New creates a new fix Generator. scopesCallbacks lists Scopes/Preload callback
// functions, whose *gorm.DB parameters cannot be made immutable-param, so the
// parameter-root fix is withheld for them (stage 2c); it may be nil.
func New(pass *analysis.Pass, scopesCallbacks map[*ssa.Function]bool) *Generator {
	// Build token.File -> ast.File mapping
	files := make(map[*token.File]*ast.File)
	for _, f := range pass.Files {
		tf := pass.Fset.File(f.Pos())
		if tf != nil {
			files[tf] = f
		}
	}

	// Reuse the pass inspector instead of building one per file.
	cursors := make(map[*ast.File]inspector.Cursor)
	for cur := range pass.ResultOf[inspect.Analyzer].(*inspector.Inspector).Root().Children() {
		cursors[cur.Node().(*ast.File)] = cur
	}

	return &Generator{
		pass:            pass,
		fset:            pass.Fset,
		files:           files,
		cursors:         cursors,
		scopesCallbacks: scopesCallbacks,
	}
}

// Generate generates SuggestedFix for a violation.
// Returns nil if the violation cannot be auto-fixed.
func (g *Generator) Generate(v pollution.Violation) []analysis.SuggestedFix {
	root := v.Root
	if root == nil {
		return nil // Cannot fix without root information
	}

	// A parameter root has no definition site to Session(), so the "insert
	// Session() at the root" model does not apply. Instead, suggest declaring the
	// enclosing function //gormreuse:immutable-param — the parameter is then
	// treated as immutable and the caller becomes responsible for passing an
	// isolated value (verified caller-side, stage 2b/2c).
	if p, ok := root.(*ssa.Parameter); ok {
		return g.generateImmutableParamFix(p)
	}

	// A call with a tuple result, such as q, err := helper(db), is a root whose
	// value is not the *gorm.DB itself (#176). Appending Session() to it would
	// not compile, so no fix is offered.
	if !typeutil.IsGormDB(root.Type()) {
		return nil
	}

	allUses := v.AllUses
	if len(allUses) == 0 {
		return nil // No uses to fix
	}

	// The variable a reassignment rewrites. nil means no reassignment is safe,
	// and the fix falls back to a Session at the root.
	target := g.findReassignTarget(root, allUses)

	// Phase 1: Identify non-finisher uses that need reassignment
	nonFinisherUses := g.findNonFinisherUses(allUses, target)

	// Phase 2: Simulate reassignments and find roots needing Session
	virtualUses := g.simulateReassignments(root, allUses, target)
	rootsNeedingSession := g.findRootsNeedingSession(virtualUses)

	// Generate TextEdits
	var edits []analysis.TextEdit

	// Track files that need gorm import (files where Session is added)
	filesNeedingImport := make(map[*ast.File]bool)

	// PHASE 1: Add reassignments for non-finisher uses (Rule 1)
	for _, use := range nonFinisherUses {
		if edit := g.generateReassignmentEdit(use.Pos, target); edit != nil {
			edits = append(edits, *edit)
		}
	}

	// PHASE 2: Add Session to roots that need it (Rule 2).
	// The original root is Phi-aware (Session on each incoming edge); a virtual
	// root created by a reassignment gets Session appended at its creation site
	// (e.g. q = q.Where("a").Session(&gorm.Session{})) — #71 defect 2.
	for _, vr := range rootsNeedingSession {
		var sessionEdits []analysis.TextEdit
		if vr.isOriginal {
			sessionEdits = g.generateSessionEditsForRoot(vr.pos, root)
		} else if edit := g.generateSessionEdit(vr.pos); edit != nil {
			sessionEdits = []analysis.TextEdit{*edit}
		}
		if len(sessionEdits) > 0 {
			edits = append(edits, sessionEdits...)
			if file := g.findFileContaining(vr.pos); file != nil {
				filesNeedingImport[file] = true
			}
		}
	}

	if len(edits) == 0 {
		return nil
	}

	// PHASE 3: Add gorm import if needed
	for file := range filesNeedingImport {
		if importEdit := g.generateGormImportEdit(file); importEdit != nil {
			edits = append(edits, *importEdit)
		}
	}

	// Deduplicate edits (same position and new text)
	// This can happen when multiple Phi edges from the same Phi are in rootsNeedingSession
	edits = g.deduplicateEdits(edits)

	// Sort edits by position (earlier positions first)
	// This ensures correct application order
	slices.SortFunc(edits, func(a, b analysis.TextEdit) int {
		return cmp.Compare(a.Pos, b.Pos)
	})

	return []analysis.SuggestedFix{
		{
			Message:   "Add reassignment and Session to fix reuse",
			TextEdits: edits,
		},
	}
}

// fileCursor returns the cursor for the given file in the pass inspector.
func (g *Generator) fileCursor(file *ast.File) inspector.Cursor {
	return g.cursors[file]
}

// generateImmutableParamFix suggests annotating the enclosing function with
// //gormreuse:immutable-param when a *gorm.DB PARAMETER is the reused root
// (Phase 1b). This declares the parameter immutable; the caller then owns
// passing an isolated value (verified caller-side, stage 2b).
//
// No fix is offered when:
//   - the function is a Scopes/Preload callback — immutable-param is rejected
//     there (the parameter genuinely receives a mid-chain value), so annotating
//     would not converge; the user must Session() inside the callback.
//   - the enclosing function is not a named declaration (a closure/func literal) —
//     directive placement on literals is left to the user.
func (g *Generator) generateImmutableParamFix(p *ssa.Parameter) []analysis.SuggestedFix {
	fn := p.Parent()
	if fn == nil || g.scopesCallbacks[fn] {
		return nil
	}
	fd, ok := fn.Syntax().(*ast.FuncDecl)
	if !ok {
		return nil
	}
	// Insert the directive on its own line immediately before `func` (after any
	// doc comment). FuncDecl.Pos() is the `func` keyword, so this is the
	// recognized next-line placement and needs no indentation for top-level decls.
	pos := fd.Pos()
	return []analysis.SuggestedFix{{
		Message: "Declare //gormreuse:immutable-param (caller must pass an isolated *gorm.DB)",
		TextEdits: []analysis.TextEdit{{
			Pos:     pos,
			End:     pos,
			NewText: []byte("//gormreuse:immutable-param\n"),
		}},
	}}
}

// findNonFinisherUses finds uses that are non-finisher expression statements
// on target.
func (g *Generator) findNonFinisherUses(uses []pollution.UsageInfo, target *types.Var) []pollution.UsageInfo {
	var nonFinishers []pollution.UsageInfo
	for _, use := range uses {
		if g.isNonFinisherExprStmt(use.Pos, target) {
			nonFinishers = append(nonFinishers, use)
		}
	}
	return nonFinishers
}

// isNonFinisherExprStmt checks if a position is a non-finisher expression
// statement on target. Non-finisher means the result is not used (e.g.,
// q.Where("a") without assignment).
//
// Returns true only when ALL of the following conditions are met:
//  1. The statement is an ExprStmt (expression used as statement)
//  2. The use is the statement's own top-level call, not a call nested in it
//  3. The top-level expression is a *gorm.DB method call (checked via receiver type)
//  4. The method is a non-finisher (chainable method like Where, Limit, etc.)
//  5. The receiver is an identifier that resolves to target
//
// This prevents inappropriate fixes for non-GORM expressions like:
//
//	require.NoError(t, tx.Create(...).Error)
func (g *Generator) isNonFinisherExprStmt(pos token.Pos, target *types.Var) bool {
	if target == nil {
		return false
	}

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

	// Check if pos is within the ExprStmt's expression
	if exprStmt.X.Pos() > pos || pos > exprStmt.X.End() {
		return false
	}

	// Check if it's a call expression
	callExpr, ok := exprStmt.X.(*ast.CallExpr)
	if !ok {
		return false
	}

	// A use is recorded at its call's Lparen. A use anywhere else in the
	// statement is nested: inside an argument (the reused q in
	// `base.Or(q.Where("y"))`, issue #71 defect 1) or in a call on the
	// receiver spine. Reassigning the whole statement would rebind the wrong
	// variable and never address the violation root. Such a use is not a
	// reassignable non-finisher; the root instead gets a Session at its own
	// definition (Phase 2).
	if callExpr.Lparen != pos {
		return false
	}

	// Check if it's a method call (selector expression)
	sel, ok := callExpr.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}

	// Check if the receiver's type is *gorm.DB
	// This prevents false positives for non-GORM methods like require.NoError
	if g.pass.TypesInfo != nil {
		receiverType := g.pass.TypesInfo.TypeOf(sel.X)
		if receiverType == nil || !typeutil.IsGormDB(receiverType) {
			return false
		}
	}

	// The receiver must be the variable that holds the root. In `base.Or(q)`
	// the root q is an argument, and in `r.Where("a")` r is an alias of q;
	// rebinding base or r would not address q (issue #198).
	if !g.refersTo(sel.X, target) {
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
		"Find":          true,
		"First":         true,
		"Last":          true,
		"Take":          true,
		"Count":         true,
		"Pluck":         true,
		"Scan":          true,
		"Row":           true,
		"Rows":          true,
		"ScanRows":      true,
		"Create":        true,
		"Save":          true,
		"Update":        true,
		"Updates":       true,
		"Delete":        true,
		"Exec":          true,
		"Transaction":   true,
		"FirstOrCreate": true, // terminal (executes); #71 secondary
		"FirstOrInit":   true, // terminal (executes); #71 secondary
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
func (g *Generator) simulateReassignments(root ssa.Value, uses []pollution.UsageInfo, target *types.Var) map[virtualRootKey][]pollution.UsageInfo {
	// Sort uses by position
	sortedUses := slices.Clone(uses)
	slices.SortFunc(sortedUses, func(a, b pollution.UsageInfo) int {
		return cmp.Compare(a.Pos, b.Pos)
	})

	// Start with the original root
	currentRootKey := virtualRootKey{isOriginal: true, pos: root.Pos()}
	virtualUses := make(map[virtualRootKey][]pollution.UsageInfo)

	for _, use := range sortedUses {
		// This use is from currentRoot
		virtualUses[currentRootKey] = append(virtualUses[currentRootKey], use)

		// If non-finisher, this use's result becomes the new root
		if g.isNonFinisherExprStmt(use.Pos, target) {
			// Create a new virtual root at this position
			currentRootKey = virtualRootKey{isOriginal: false, pos: use.Pos}
		}
	}

	return virtualUses
}

// findRootsNeedingSession finds the roots — the original AND reassignment-created
// virtual roots — that still branch 2+ times after reassignment simulation and
// therefore need a Session() to become immutable.
//
// Emitting Session for virtual roots too is what makes a fix single-pass
// complete (#71 defect 2): for `q := db.Where("base"); q.Where("a");
// q.Where("b").Find(); q.Where("c"); q.Where("d").Find()` the reassignment of
// the virtual root created at `q.Where("a")` still branches twice, so it needs
// `.Session()` — without it, re-linting the "fixed" output still warns.
func (g *Generator) findRootsNeedingSession(virtualUses map[virtualRootKey][]pollution.UsageInfo) []virtualRootKey {
	var roots []virtualRootKey
	for root, uses := range virtualUses {
		if len(uses) >= 2 {
			roots = append(roots, root)
		}
	}
	// Deterministic order (map iteration is random) so edits/goldens are stable.
	slices.SortFunc(roots, func(a, b virtualRootKey) int { return cmp.Compare(a.pos, b.pos) })
	return roots
}

// generateReassignmentEdit generates a TextEdit for reassignment.
// Inserts "q = " before the statement, where q is target.
func (g *Generator) generateReassignmentEdit(pos token.Pos, target *types.Var) *analysis.TextEdit {
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
		NewText: []byte(target.Name() + " = "),
	}
}

// generateSessionEdit generates a TextEdit for Session.
// Appends ".Session(&gorm.Session{})" after the call expression.
func (g *Generator) generateSessionEdit(pos token.Pos) *analysis.TextEdit {
	endPos := g.getCallExprEndPos(pos)
	if endPos == token.NoPos {
		return nil
	}

	// Qualify Session with the file's actual local name for gorm.io/gorm so the
	// fix compiles under an aliased or dot import (issue #71, defect 3).
	qualifier := "gorm."
	if file := g.findFileContaining(pos); file != nil {
		qualifier = gormQualifier(file)
	}

	return &analysis.TextEdit{
		Pos:     endPos,
		End:     endPos,
		NewText: []byte(".Session(&" + qualifier + "Session{})"),
	}
}

// gormQualifier returns the selector prefix for referring to gorm types in file:
// "gorm." for a normal import, "<alias>." for an aliased import, or "" for a
// dot-import. It defaults to "gorm." when gorm is not imported (an import edit is
// added separately by generateGormImportEdit).
func gormQualifier(file *ast.File) string {
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil || path != gormImportPath {
			continue
		}
		if imp.Name == nil {
			return "gorm." // normal import; gorm.io/gorm's package name is "gorm"
		}
		switch imp.Name.Name {
		case ".":
			return "" // dot-import: reference Session unqualified
		case "_":
			return "gorm." // blank import provides no usable name; treat as absent
		default:
			return imp.Name.Name + "." // aliased import
		}
	}
	return "gorm."
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
// OPTIMIZATION: Only add Session if the Phi result is used multiple times.
func (g *Generator) generatePhiEdgeEdits(phi *ssa.Phi) []analysis.TextEdit {
	// Count how many times the Phi RESULT is used
	phiUseCount := g.countPhiResultUses(phi)

	// If Phi result is used only once, no Session needed on any edges
	if phiUseCount <= 1 {
		return nil
	}

	// If Phi result is used 2+ times, add Session to all mutable edges
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

// countPhiResultUses counts how many times the Phi result is used.
func (g *Generator) countPhiResultUses(phi *ssa.Phi) int {
	if phi.Referrers() == nil {
		return 0
	}
	return len(*phi.Referrers())
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
			if slices.Contains(phi.Edges, v) {
				return phi
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
	if typeutil.IsImmutableReturningBuiltin(callee.Name()) {
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

// stmtNodeTypes contains all AST statement types for inspector filtering.
var stmtNodeTypes = []ast.Node{
	(*ast.AssignStmt)(nil),
	(*ast.BlockStmt)(nil),
	(*ast.BranchStmt)(nil),
	(*ast.CaseClause)(nil),
	(*ast.CommClause)(nil),
	(*ast.DeclStmt)(nil),
	(*ast.DeferStmt)(nil),
	(*ast.EmptyStmt)(nil),
	(*ast.ExprStmt)(nil),
	(*ast.ForStmt)(nil),
	(*ast.GoStmt)(nil),
	(*ast.IfStmt)(nil),
	(*ast.IncDecStmt)(nil),
	(*ast.LabeledStmt)(nil),
	(*ast.RangeStmt)(nil),
	(*ast.ReturnStmt)(nil),
	(*ast.SelectStmt)(nil),
	(*ast.SendStmt)(nil),
	(*ast.SwitchStmt)(nil),
	(*ast.TypeSwitchStmt)(nil),
}

// findStmtAtPos finds the innermost statement at the given position.
func (g *Generator) findStmtAtPos(file *ast.File, pos token.Pos) ast.Stmt {
	var result ast.Stmt
	for c := range g.fileCursor(file).Preorder(stmtNodeTypes...) {
		stmt := c.Node().(ast.Stmt)
		// Check if this statement contains the position
		if stmt.Pos() <= pos && pos < stmt.End() {
			// Keep the innermost (narrowest) statement
			if result == nil || nodeWidth(stmt) < nodeWidth(result) {
				result = stmt
			}
		}
	}
	return result
}

// callExprNodeTypes contains CallExpr for inspector filtering.
var callExprNodeTypes = []ast.Node{(*ast.CallExpr)(nil)}

// findReassignTarget returns the local variable that holds root, when a
// reassignment `v = v.Where(...)` of it is safe. The reassignment fix rewrites
// only this variable (issue #198). It returns nil, and the fix falls back to a
// Session at the root, when:
//   - the root is not a call assigned to a named variable (a field, an
//     argument of another call);
//   - the variable is not local to a function, such as a package-level DB;
//   - a closure captures the variable. A goroutine that rewrites it races with
//     its parent, and a deferred or stored closure would see the new value;
//   - a use does not mention the variable, such as a use through an alias
//     `r := q`. Rebinding q would leave that branch on the old root.
func (g *Generator) findReassignTarget(root ssa.Value, uses []pollution.UsageInfo) *types.Var {
	v := g.findRootVar(root)
	if v == nil || v.Parent() == v.Pkg().Scope() {
		return nil
	}
	// v is local, so its declaration and every use of the root are inside one
	// function of this file.
	file := g.findFileContaining(v.Pos())
	if g.isCapturedVar(file, v) {
		return nil
	}
	for _, use := range uses {
		call := findCallAtLparen(g.fileCursor(file), use.Pos)
		if call == nil || !g.mentions(call, v) {
			return nil
		}
	}
	return v
}

// findRootVar returns the variable that root, a call, is assigned to by
// `v := <call>`, `v = <call>` or `var v = <call>`, or nil. SSA places a call
// at its Lparen. A root that is not a call, such as a Phi, gets nil, so the
// fix falls back to a Session. The tracer usually resolves a Phi to its
// edges; a loop variable swap keeps the Phi as the root.
func (g *Generator) findRootVar(root ssa.Value) *types.Var {
	// A Phi has no position, and a call inside a synthetic wrapper, such as
	// the one behind a method value `where := db.Where`, is in no file of the
	// pass. Any other value matches no call's Lparen below.
	pos := root.Pos()
	file := g.findFileContaining(pos)
	if file == nil {
		return nil
	}
	isRoot := func(e ast.Expr) bool {
		call, ok := ast.Unparen(e).(*ast.CallExpr)
		return ok && call.Lparen == pos
	}
	for c := range g.fileCursor(file).Preorder((*ast.AssignStmt)(nil), (*ast.ValueSpec)(nil)) {
		var lhs []ast.Expr
		var rhs []ast.Expr
		switch n := c.Node().(type) {
		case *ast.AssignStmt:
			lhs, rhs = n.Lhs, n.Rhs
		case *ast.ValueSpec:
			for _, name := range n.Names {
				lhs = append(lhs, name)
			}
			rhs = n.Values
		}
		if len(lhs) != len(rhs) {
			continue
		}
		for i, e := range rhs {
			if !isRoot(e) {
				continue
			}
			id, ok := ast.Unparen(lhs[i]).(*ast.Ident)
			if !ok {
				return nil
			}
			v, _ := g.pass.TypesInfo.ObjectOf(id).(*types.Var)
			return v
		}
	}
	return nil
}

// isCapturedVar reports whether a closure inside the function that declares v
// refers to v. An immediately invoked closure `func() { ... }()` runs in
// program order, so it does not count; one run by a go or defer statement
// does.
func (g *Generator) isCapturedVar(file *ast.File, v *types.Var) bool {
	cur, _ := g.fileCursor(file).FindByPos(v.Pos(), v.Pos())
	declFn := enclosingFunc(cur)
	for c := range declFn.Preorder((*ast.Ident)(nil)) {
		if g.pass.TypesInfo.Uses[c.Node().(*ast.Ident)] != v {
			continue
		}
		for fn := enclosingFunc(c); fn != declFn; fn = enclosingFunc(fn.Parent()) {
			if !isInlineCall(fn) {
				return true
			}
		}
	}
	return false
}

// isInlineCall reports whether lit, a function literal, is called where it is
// written, and not by a go or defer statement.
func isInlineCall(lit inspector.Cursor) bool {
	if lit.ParentEdgeKind() != edge.CallExpr_Fun {
		return false
	}
	switch lit.Parent().ParentEdgeKind() {
	case edge.GoStmt_Call, edge.DeferStmt_Call:
		return false
	}
	return true
}

// enclosingFunc returns the innermost function declaration or literal that
// encloses cur, or cur itself when it is one.
func enclosingFunc(cur inspector.Cursor) (fn inspector.Cursor) {
	for fn = range cur.Enclosing((*ast.FuncDecl)(nil), (*ast.FuncLit)(nil)) {
		break
	}
	return fn
}

// findCallAtLparen returns the call under cur whose Lparen is pos, which is
// where SSA places a call and so where a use is recorded.
func findCallAtLparen(cur inspector.Cursor, pos token.Pos) *ast.CallExpr {
	for c := range cur.Preorder(callExprNodeTypes...) {
		if call := c.Node().(*ast.CallExpr); call.Lparen == pos {
			return call
		}
	}
	return nil
}

// mentions reports whether an identifier in n refers to v.
func (g *Generator) mentions(n ast.Node, v *types.Var) bool {
	found := false
	ast.Inspect(n, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && g.pass.TypesInfo.Uses[id] == v {
			found = true
		}
		return !found
	})
	return found
}

// refersTo reports whether expr is an identifier that refers to v.
func (g *Generator) refersTo(expr ast.Expr, v *types.Var) bool {
	id, ok := ast.Unparen(expr).(*ast.Ident)
	return ok && g.pass.TypesInfo.Uses[id] == v
}

// getCallExprEndPos gets the end position of the CallExpr at the given position.
func (g *Generator) getCallExprEndPos(pos token.Pos) token.Pos {
	file := g.findFileContaining(pos)
	if file == nil {
		return token.NoPos
	}

	if call := g.findInnermostCallExpr(file, pos); call != nil {
		return call.End()
	}
	return token.NoPos
}

// findInnermostCallExpr finds the innermost (narrowest) CallExpr containing the given position.
// This is important for nested calls like t.Run(func() { db.Where(...) }) where we want
// to find db.Where(...), not the outer t.Run(...) call.
func (g *Generator) findInnermostCallExpr(file *ast.File, pos token.Pos) *ast.CallExpr {
	var best *ast.CallExpr
	for c := range g.fileCursor(file).Preorder(callExprNodeTypes...) {
		call := c.Node().(*ast.CallExpr)
		if call.Pos() <= pos && pos < call.End() {
			// Found a CallExpr containing pos; keep the narrowest one
			if best == nil || nodeWidth(call) < nodeWidth(best) {
				best = call
			}
		}
	}
	return best
}

// nodeWidth returns the width (end - start) of a node's position range.
func nodeWidth(n ast.Node) token.Pos {
	return n.End() - n.Pos()
}

// deduplicateEdits removes duplicate edits (same position and new text).
// This is necessary because when multiple edges of the same Phi are in rootsNeedingSession,
// generatePhiEdgeEdits will be called multiple times for the same Phi, generating duplicate edits.
func (g *Generator) deduplicateEdits(edits []analysis.TextEdit) []analysis.TextEdit {
	if len(edits) <= 1 {
		return edits
	}

	// Use a struct as map key to uniquely identify edits
	type editKey struct {
		pos  token.Pos
		text string
	}

	seen := make(map[editKey]bool)
	var result []analysis.TextEdit

	for _, edit := range edits {
		key := editKey{pos: edit.Pos, text: string(edit.NewText)}
		if !seen[key] {
			seen[key] = true
			result = append(result, edit)
		}
	}

	return result
}

const gormImportPath = "gorm.io/gorm"

// generateGormImportEdit generates a TextEdit to add gorm import if not already present.
// Returns nil if gorm is already imported.
func (g *Generator) generateGormImportEdit(file *ast.File) *analysis.TextEdit {
	if astutil.UsesImport(file, gormImportPath) {
		return nil
	}

	// Find existing import declaration
	for _, decl := range file.Decls {
		genDecl, ok := decl.(*ast.GenDecl)
		if !ok || genDecl.Tok != token.IMPORT || len(genDecl.Specs) == 0 {
			continue
		}

		// Check if it's a grouped import: import ( ... )
		if genDecl.Lparen.IsValid() {
			// Grouped import - append after last spec
			lastSpec := genDecl.Specs[len(genDecl.Specs)-1]
			return &analysis.TextEdit{
				Pos:     lastSpec.End(),
				End:     lastSpec.End(),
				NewText: []byte("\n\t\"" + gormImportPath + "\""),
			}
		}

		// Single import - add new import line after this declaration
		return &analysis.TextEdit{
			Pos:     genDecl.End(),
			End:     genDecl.End(),
			NewText: []byte("\nimport \"" + gormImportPath + "\""),
		}
	}

	// No import declaration found - insert after package clause
	return &analysis.TextEdit{
		Pos:     file.Name.End(),
		End:     file.Name.End(),
		NewText: []byte("\n\nimport \"" + gormImportPath + "\""),
	}
}
