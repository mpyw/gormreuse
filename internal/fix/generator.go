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

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/ssa/callsite"
	"github.com/mpyw/gormreuse/internal/ssa/pollution"
	"github.com/mpyw/gormreuse/internal/ssa/tracer"
	"github.com/mpyw/gormreuse/internal/typeutil"
)

// Generator generates SuggestedFix for a violation.
type Generator struct {
	//declscope:shared // the files of the package read types and syntax through it
	pass *analysis.Pass
	//declscope:shared // param.go's paramSessionSite reads line positions with it
	fset *token.FileSet
	//declscope:shared // param.go's canFixParam reads it
	scopesCallbacks map[*ssa.Function]bool // Scopes/Preload callbacks (no parameter-root fix)
	//declscope:shared // change.go, param.go, session.go and virtual.go ask it what is immutable
	tracer *tracer.RootTracer // the analysis' mutability rule
	//declscope:shared // param.go's paramCallersIsolate scans them
	funcs []*ssa.Function // the analysed functions, to find callers
	//declscope:shared // param.go's paramCallersIsolate asks it
	seesAllCallers func(token.Pos) bool // whether this variant sees every caller of code at a position

	files   map[*token.File]*ast.File      // token.File -> ast.File mapping
	cursors map[*ast.File]inspector.Cursor // file cursors in the pass inspector
}

// NewGenerator creates a new fix Generator. scopesCallbacks lists
// Scopes/Preload callback functions, whose *gorm.DB parameters cannot be made
// immutable-param, so the parameter-root fix is withheld for them (stage 2c);
// it may be nil. rt decides which values are already immutable, and funcs
// are the analysed functions, package-level closures included, whose calls
// are checked before a parameter is declared immutable-param (#201, #245).
// seesAllCallers reports whether the pass sees every file that may call a
// function declared at a position: not so for a file other than a test file
// in a package with test files, which the pass without them cannot see
// (#245).
func NewGenerator(pass *analysis.Pass, scopesCallbacks map[*ssa.Function]bool, rt *tracer.RootTracer, funcs []*ssa.Function, seesAllCallers func(token.Pos) bool) *Generator {
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
		tracer:          rt,
		funcs:           funcs,
		seesAllCallers:  seesAllCallers,
	}
}

// Generate generates SuggestedFix for a violation.
// Returns nil if the violation cannot be auto-fixed.
func (g *Generator) Generate(v pollution.Violation) []analysis.SuggestedFix {
	root := v.Root
	if root == nil {
		return nil // Cannot fix without root information
	}

	// A parameter root has no definition site to Session(). When every caller
	// is known and passes an isolated value, the fix declares the function
	// //gormreuse:immutable-param, which the caller-side check (stage 2b) then
	// keeps true. Otherwise the parameter gets a Session at the top of the
	// body, and the rest of the fix proceeds as for a local root (#201).
	p, isParam := root.(*ssa.Parameter)
	if isParam {
		if !g.canFixParam(p) {
			return nil
		}
		if g.paramCallersIsolate(p) {
			return g.generateImmutableParamFix(p)
		}
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

	// Code that changes the root in place, other than a discarded statement on
	// its variable, would change a copy once the root gets a Session (#243).
	if g.changedElsewhere(v) {
		return nil
	}

	// The variable a reassignment rewrites. nil means no reassignment is safe,
	// and the fix falls back to a Session at the root.
	target := g.findReassignTarget(root, allUses)

	// Phase 1: Identify non-finisher uses that need reassignment
	reassigned, ok := g.findReassignedChains(allUses, target)
	if !ok {
		return nil
	}

	// A discarded statement on a parameter also changes the caller's value,
	// and the caller may rely on that. Reassigning it would change only the
	// parameter, unless the function returns a *gorm.DB that can carry the
	// change back (#243).
	if isParam && len(reassigned) > 0 && !typeutil.HasGormDBResult(p.Parent().Signature.Results()) {
		return nil
	}

	// Phase 2: Simulate reassignments and find roots needing Session
	virtualUses := g.simulateVirtualRoots(root, allUses, reassigned)
	rootsNeedingSession := g.findRootsNeedingSession(virtualUses, reassigned, usesOrdered(v))
	if reassignsInClosure(root, allUses, reassigned) && !slices.ContainsFunc(rootsNeedingSession, func(vr virtualRootKey) bool { return vr.isOriginal }) {
		rootsNeedingSession = append(rootsNeedingSession, virtualRootKey{isOriginal: true, pos: root.Pos()})
	}

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
	var sites []sessionSite
	for _, vr := range rootsNeedingSession {
		switch {
		case vr.isOriginal && isParam:
			sites = append(sites, g.paramSessionSite(p))
		case vr.isOriginal:
			for _, pos := range g.sessionPositionsForRoot(vr.pos, root, allUses) {
				sites = append(sites, sessionSite{pos: pos})
			}
		case !g.virtualRootEndsImmutable(vr):
			sites = append(sites, sessionSite{pos: vr.end})
		}
	}

	// PHASE 3: Write the Sessions, with a gorm import when they need one. Each
	// fix carries its own import edit, so applying it alone compiles (#200).
	edits = append(edits, g.sessionEdits(sites)...)

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

// GenerateIsolation returns the fix for a violation of the caller-side
// immutable-param contract: .Session(&gorm.Session{}) after the mutable
// argument, as the diagnostic says (#248). The Session result is immutable,
// so the call no longer breaks the contract. It returns nil for any other
// violation, or when the argument is not a *gorm.DB expression of the call.
func (g *Generator) GenerateIsolation(v pollution.Violation) []analysis.SuggestedFix {
	if v.Call == nil {
		return nil
	}
	// The call's Lparen, for a go or defer statement too.
	lparen := v.Call.Common().Pos()
	var call *ast.CallExpr
	if file := g.findFileContaining(lparen); file != nil {
		call = findCallAtLparen(g.fileCursor(file), lparen)
	}
	// Param counts the parameters without a receiver, as the call's
	// arguments do, except for a method expression (T.m(r, x)), whose
	// receiver is its first argument (#258). Only a variadic parameter can
	// differ otherwise, and it never holds a *gorm.DB argument of its own.
	var arg ast.Expr
	if call != nil && v.Param >= 0 {
		if i := v.Param + g.receiverArgs(call, v.Call); i < len(call.Args) && typeutil.IsGormDB(g.pass.TypesInfo.TypeOf(call.Args[i])) {
			arg = call.Args[i]
		}
	}
	if arg == nil {
		return nil
	}

	var edits []analysis.TextEdit
	site := sessionSite{pos: arg.End()}
	switch arg.(type) {
	case *ast.Ident, *ast.ParenExpr, *ast.SelectorExpr, *ast.CallExpr, *ast.IndexExpr, *ast.IndexListExpr, *ast.TypeAssertExpr:
	default:
		// Session binds tighter than *p or a conversion's operand.
		edits = append(edits, analysis.TextEdit{Pos: arg.Pos(), End: arg.Pos(), NewText: []byte("(")})
		site.prefix = ")"
	}
	edits = append(edits, g.sessionEdits([]sessionSite{site})...)
	slices.SortFunc(edits, func(a, b analysis.TextEdit) int { return cmp.Compare(a.Pos, b.Pos) })
	return []analysis.SuggestedFix{{Message: "Isolate the argument with Session", TextEdits: edits}}
}

// receiverArgs returns how many arguments of call pass the receiver of the
// method that instr stands for: 1 for a method expression, whose function
// takes the receiver as its first parameter, and 0 otherwise. instr is the
// call of an immutable-param violation, read with the same FuncVars as the
// checker read it, so it always stands for a declared function (#293).
func (g *Generator) receiverArgs(call *ast.CallExpr, instr ssa.CallInstruction) int {
	declared := callsite.SiteOf(instr, g.tracer.FuncVars()).Declared
	sig := g.pass.TypesInfo.TypeOf(call.Fun).Underlying().(*types.Signature)
	return sig.Params().Len() - declared.Signature.Params().Len()
}

// fileCursor returns the cursor for the given file in the pass inspector.
//
//declscope:shared // every file of the package walks a file's syntax from it
func (g *Generator) fileCursor(file *ast.File) inspector.Cursor {
	return g.cursors[file]
}

// findFileContaining finds the AST file containing the given position.
//
//declscope:shared // reassign.go, session.go and virtual.go find the file of a position with it
func (g *Generator) findFileContaining(pos token.Pos) *ast.File {
	tf := g.fset.File(pos)
	if tf == nil {
		return nil
	}
	return g.files[tf]
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
