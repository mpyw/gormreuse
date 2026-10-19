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
	"maps"
	"slices"
	"strconv"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/edge"
	"golang.org/x/tools/go/ast/inspector"
	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/ssa/cfg"
	"github.com/mpyw/gormreuse/internal/ssa/pollution"
	"github.com/mpyw/gormreuse/internal/ssa/tracer"
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

	// A field of a struct a call returned (#181) has no chain expression to
	// append Session() to: the call returns the struct, not a *gorm.DB. The fix
	// belongs where the function builds the struct, so none is offered.
	if _, ok := root.(*tracer.FieldRoot); ok {
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
	reassigned, ok := g.findReassignedChains(allUses, target)
	if !ok {
		return nil
	}

	// Phase 2: Simulate reassignments and find roots needing Session
	virtualUses := g.simulateReassignments(root, allUses, reassigned)
	rootsNeedingSession := g.findRootsNeedingSession(virtualUses)

	// Generate TextEdits
	var edits []analysis.TextEdit

	// PHASE 1: Add reassignments for non-finisher uses (Rule 1)
	for _, pos := range slices.Sorted(maps.Keys(reassigned)) {
		if edit := g.generateReassignmentEdit(pos, target); edit != nil {
			edits = append(edits, *edit)
		}
	}

	// PHASE 2: Add Session to roots that need it (Rule 2).
	// The original root is Phi-aware (Session on each incoming edge); a virtual
	// root created by a reassignment gets Session appended at its creation site
	// (e.g. q = q.Where("a").Session(&gorm.Session{})) — #71 defect 2.
	var sessionPositions []token.Pos
	for _, vr := range rootsNeedingSession {
		if vr.isOriginal {
			sessionPositions = append(sessionPositions, g.sessionPositionsForRoot(vr.pos, root)...)
		} else {
			sessionPositions = append(sessionPositions, vr.end)
		}
	}

	// PHASE 3: Write the Sessions, with a gorm import when they need one. Each
	// fix carries its own import edit, so applying it alone compiles (#200).
	edits = append(edits, g.sessionEdits(sessionPositions)...)

	if len(edits) == 0 {
		return nil
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

// findReassignedChains returns the discarded chains (see discardedChain)
// among uses, which the fix reassigns to target, as a map from the position of
// the use to the end of its chain. Several uses can share a position, such as
// a method call's receiver that is also recorded as an argument; the map
// classifies them all.
//
// A discarded chain such as `q.Where("a").Limit(1)` changes the root in
// place, and nothing else keeps that change. Once the root gets a Session,
// the chain changes a copy instead, and the fixed code silently drops its
// conditions. So when a chain cannot be reassigned, ok is false and no fix is
// offered (#199).
func (g *Generator) findReassignedChains(uses []pollution.UsageInfo, target *types.Var) (chains map[token.Pos]token.Pos, ok bool) {
	chains = make(map[token.Pos]token.Pos)
	for _, use := range uses {
		stmt, recv, isChain := g.discardedChain(use)
		if !isChain {
			continue
		}
		if !g.isReassignable(use, recv, target) {
			return nil, false
		}
		chains[use.Pos] = stmt.X.End()
	}
	return chains, true
}

// isReassignable reports whether `target = <statement>` keeps the effect of
// the discarded chain that starts at use, whose first call has receiver recv.
// The receiver must be the variable that holds the root. In `r.Where("a")` r
// is an alias of q, and a method value `where("a")` has no receiver to
// rebind (issue #198). The use must also be at its own call: a use moved to
// the call site of a closure (#68) is in a statement that runs elsewhere.
func (g *Generator) isReassignable(use pollution.UsageInfo, recv ast.Expr, target *types.Var) bool {
	return target != nil && use.Call == use.Pos && recv != nil && g.refersTo(recv, target)
}

// discardedChain reports whether use is the first call of a chain of *gorm.DB
// non-finisher calls whose result an expression statement discards, such as
// q.Where("a") or q.Where("a").Limit(1). Such a statement has an effect only
// by changing the receiver's Statement in place. It returns the statement and
// the receiver of the use's call. The receiver is nil when the statement
// cannot be reassigned as a whole: for a call through a method value, and for
// a chain with a Session-family call before its last call, such as
// q.Where("a").Session(s).Limit(1). Only q.Where("a") changes q there, so
// reassigning the statement would add Limit(1) to q.
//
// A statement that ends in a finisher runs its own query, and one whose
// top-level call is not a gorm method, such as
// require.NoError(t, tx.Create(...).Error), is not a chain. Nor is one whose
// first call is a Session-family call: it never changes the receiver.
func (g *Generator) discardedChain(use pollution.UsageInfo) (*ast.ExprStmt, ast.Expr, bool) {
	if use.Method == "" || isFinisher(use.Method) || typeutil.IsImmutableReturningBuiltin(use.Method) {
		return nil, nil, false
	}
	file := g.findFileContaining(use.Call)
	if file == nil {
		return nil, nil, false
	}
	stmt, ok := g.findStmtAtPos(file, use.Call).(*ast.ExprStmt)
	if !ok {
		return nil, nil, false
	}
	// Walk down the receiver spine from the statement's top-level call to the
	// use's call. A use anywhere else, such as inside an argument, is not on
	// the spine and ends the walk at a non-call.
	copied := false
	for e := stmt.X; ; {
		call, ok := ast.Unparen(e).(*ast.CallExpr)
		if !ok || !typeutil.IsGormDB(g.pass.TypesInfo.TypeOf(call)) {
			return nil, nil, false
		}
		sel, isMethod := call.Fun.(*ast.SelectorExpr)
		if call.Lparen == use.Call {
			if !isMethod || copied {
				return stmt, nil, true
			}
			return stmt, sel.X, true
		}
		if !isMethod || isFinisher(sel.Sel.Name) || !typeutil.IsGormDB(g.pass.TypesInfo.TypeOf(sel.X)) {
			return nil, nil, false
		}
		// A Session-family call below the top-level call stops the calls above
		// it from changing the receiver.
		if e != stmt.X && typeutil.IsImmutableReturningBuiltin(sel.Sel.Name) {
			copied = true
		}
		e = sel.X
	}
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
		// These return *gorm.DB but run SQL or end a transaction, so a
		// discarded call is not a chain to reassign (#200).
		"CreateInBatches": true,
		"FindInBatches":   true,
		"UpdateColumn":    true,
		"UpdateColumns":   true,
		"Commit":          true,
		"Rollback":        true,
		"SavePoint":       true,
		"RollbackTo":      true,
	}
	return finishers[methodName]
}

// virtualRootKey represents a virtual root (either original or created by reassignment).
type virtualRootKey struct {
	isOriginal bool
	pos        token.Pos       // For virtual roots, the position where created
	end        token.Pos       // For virtual roots, the end of the reassigned chain
	block      *ssa.BasicBlock // For virtual roots, the block of the reassignment
}

// simulateReassignments simulates Phase 1 reassignments and returns virtual uses.
// Returns a map from virtual root keys to their uses.
func (g *Generator) simulateReassignments(root ssa.Value, uses []pollution.UsageInfo, reassigned map[token.Pos]token.Pos) map[virtualRootKey][]pollution.UsageInfo {
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
		if end, ok := reassigned[use.Pos]; ok {
			// Create a new virtual root at this position. Its Session goes
			// after the chain's last call: one after an earlier call would be
			// undone by the calls that follow it.
			currentRootKey = virtualRootKey{isOriginal: false, pos: use.Pos, end: end, block: use.Block}
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
//
// The simulation orders uses by position, so it misses the uses that a loop's
// next iteration makes of a virtual root created in the loop body. Such a root
// always gets a Session (#199): in `for { q.Where("id").Find(nil); q = q.Where("age") }`
// the next iteration's q.Where("id") would otherwise change it in place.
func (g *Generator) findRootsNeedingSession(virtualUses map[virtualRootKey][]pollution.UsageInfo) []virtualRootKey {
	var roots []virtualRootKey
	cfgAnalyzer := cfg.New()
	for root, uses := range virtualUses {
		if len(uses) >= 2 || !root.isOriginal && inCycle(cfgAnalyzer, root.block) {
			roots = append(roots, root)
		}
	}
	// Deterministic order (map iteration is random) so edits/goldens are stable.
	slices.SortFunc(roots, func(a, b virtualRootKey) int { return cmp.Compare(a.pos, b.pos) })
	return roots
}

// inCycle reports whether block can run again after itself, as a loop body
// does.
func inCycle(a *cfg.Analyzer, block *ssa.BasicBlock) bool {
	return slices.ContainsFunc(block.Succs, func(succ *ssa.BasicBlock) bool { return a.CanReach(succ, block) })
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

// sessionEndAfterCall returns where a Session goes for a value defined at
// pos: the end of the call whose Lparen is pos, where SSA places a call. It
// returns token.NoPos when there is no such call or the call does not return
// *gorm.DB. A closure parameter or the result of a call with several results
// has no call of its own, and appending a Session to the call that encloses
// it, such as `run(db, func(tx *gorm.DB) { ... })`, would not compile (#200).
func (g *Generator) sessionEndAfterCall(pos token.Pos) token.Pos {
	file := g.findFileContaining(pos)
	if file == nil {
		return token.NoPos
	}
	call := findCallAtLparen(g.fileCursor(file), pos)
	if call == nil || !typeutil.IsGormDB(g.pass.TypesInfo.TypeOf(call)) {
		return token.NoPos
	}
	return call.End()
}

// sessionEdits inserts ".Session(&gorm.Session{})" at each of positions. It
// qualifies Session with a name that refers to gorm.io/gorm at every one of
// them, and adds an import of gorm.io/gorm to a file where no such name exists
// (see gormNameAt).
func (g *Generator) sessionEdits(positions []token.Pos) []analysis.TextEdit {
	byFile := make(map[*ast.File][]token.Pos)
	var files []*ast.File
	for _, pos := range positions {
		file := g.findFileContaining(pos)
		if _, seen := byFile[file]; !seen {
			files = append(files, file)
		}
		byFile[file] = append(byFile[file], pos)
	}
	var edits []analysis.TextEdit
	for _, file := range files {
		qualifier, importName := g.gormNameAt(file, byFile[file])
		for _, pos := range byFile[file] {
			edits = append(edits, analysis.TextEdit{
				Pos:     pos,
				End:     pos,
				NewText: []byte(".Session(&" + qualifier + "Session{})"),
			})
		}
		if importName != "" {
			edits = append(edits, gormImportEdit(file, importName))
		}
	}
	return edits
}

// gormNameAt returns the selector prefix that refers to gorm.io/gorm at every
// one of positions in file: "gorm." for a plain import, "<alias>." for an
// aliased one, or "" for a dot import. Each name is resolved in the scope at
// the position, so a parameter or local variable named gorm that shadows the
// import there is seen (#200).
//
// When no import of the file can be used there, importName is the name of an
// import to add: "gorm", or "gormio", "gormio2" and so on when gorm is taken,
// such as by another package imported as gorm. A blank import has no usable
// name.
func (g *Generator) gormNameAt(file *ast.File, positions []token.Pos) (qualifier, importName string) {
	resolvesAt := func(name string, want func(types.Object) bool) bool {
		for _, pos := range positions {
			_, obj := g.pass.Pkg.Scope().Innermost(pos).LookupParent(name, pos)
			if !want(obj) {
				return false
			}
		}
		return true
	}
	isGorm := func(pkg *types.Package) bool { return pkg != nil && pkg.Path() == gormImportPath }
	for _, imp := range file.Imports {
		pkgName := g.pass.TypesInfo.PkgNameOf(imp)
		if pkgName == nil || !isGorm(pkgName.Imported()) {
			continue
		}
		switch pkgName.Name() {
		case "_":
		case ".":
			if resolvesAt("Session", func(obj types.Object) bool { return obj != nil && isGorm(obj.Pkg()) }) {
				return "", ""
			}
		default:
			if resolvesAt(pkgName.Name(), func(obj types.Object) bool { return obj == pkgName }) {
				return pkgName.Name() + ".", ""
			}
		}
	}
	isFree := func(obj types.Object) bool { return obj == nil }
	name := "gorm"
	for i := 1; !resolvesAt(name, isFree); i++ {
		name = "gormio"
		if i > 1 {
			name += strconv.Itoa(i)
		}
	}
	return name + ".", name
}

// sessionPositionsForRoot returns where Sessions go to make root immutable.
// For a Phi node, and for a root that is an edge of one, it returns a
// position for each incoming edge that can take one. Otherwise it returns at
// most one, after the call at pos.
func (g *Generator) sessionPositionsForRoot(pos token.Pos, root ssa.Value) []token.Pos {
	// Check if root is a Phi node
	if phi, isPhi := root.(*ssa.Phi); isPhi {
		return g.phiEdgeSessionPositions(phi)
	}

	// Check if root is part of a Phi (root is an edge)
	// Look for Phi nodes that use this root as an edge
	if phi := g.findPhiUsingValue(root); phi != nil {
		// This root is part of a Phi - fix all edges
		return g.phiEdgeSessionPositions(phi)
	}

	// Not related to Phi - a single Session
	if end := g.sessionEndAfterCall(pos); end.IsValid() {
		return []token.Pos{end}
	}
	return nil
}

// phiEdgeSessionPositions returns where Sessions go on the edges of a Phi
// node. Only edges that don't already have Session get one, and only an edge
// defined by a *gorm.DB call can (see sessionEndAfterCall).
// OPTIMIZATION: Only add Session if the Phi result is used multiple times.
func (g *Generator) phiEdgeSessionPositions(phi *ssa.Phi) []token.Pos {
	// Count how many times the Phi RESULT is used
	phiUseCount := g.countPhiResultUses(phi)

	// If Phi result is used only once, no Session needed on any edges
	if phiUseCount <= 1 {
		return nil
	}

	// If Phi result is used 2+ times, add Session to all mutable edges
	var positions []token.Pos
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

		if end := g.sessionEndAfterCall(edgePos); end.IsValid() {
			positions = append(positions, end)
		}
	}
	return positions
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
	// A Phi has no position, so it is in no file of the pass. Any other value
	// matches no call's Lparen below.
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

// gormImportEdit adds an import of gorm.io/gorm named name to file. A name
// other than gorm is written as an alias.
func gormImportEdit(file *ast.File, name string) analysis.TextEdit {
	spec := strconv.Quote(gormImportPath)
	if name != "gorm" {
		spec = name + " " + spec
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
			return analysis.TextEdit{
				Pos:     lastSpec.End(),
				End:     lastSpec.End(),
				NewText: []byte("\n\t" + spec),
			}
		}

		// Single import - add new import line after this declaration
		return analysis.TextEdit{
			Pos:     genDecl.End(),
			End:     genDecl.End(),
			NewText: []byte("\nimport " + spec),
		}
	}

	// No import declaration found - insert after package clause
	return analysis.TextEdit{
		Pos:     file.Name.End(),
		End:     file.Name.End(),
		NewText: []byte("\n\nimport " + spec),
	}
}
