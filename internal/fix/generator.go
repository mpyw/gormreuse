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
	"golang.org/x/tools/go/ast/inspector"
	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/ssa/callsite"
	"github.com/mpyw/gormreuse/internal/ssa/cfg"
	"github.com/mpyw/gormreuse/internal/ssa/origin"
	"github.com/mpyw/gormreuse/internal/ssa/pollution"
	"github.com/mpyw/gormreuse/internal/ssa/pollutionsource"
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
	funcs []*ssa.Function // the analysed functions and the package initializer, to find callers
	//declscope:shared // param.go's paramCallersIsolate asks it
	seesAllCallers func(token.Pos) bool // whether this variant sees every caller of code at a position
	//declscope:shared // the fixes ask it about loops, reachability and repeats
	cfg *cfg.Analyzer // the CFG cache of the pass, shared with the checker (#299)
	//declscope:shared // holder.go memoizes the struct stores of each function in it for change.go
	holders holderCache
	//declscope:shared // handoff.go memoizes the finisher-only parameters of each function in it
	handoffs handoffCache
	//declscope:shared // handoff.go reads the finisher-only parameters of another package's functions with it
	importedFinisherOnly func(*ssa.Function) []int // from the facts of other packages
	//declscope:shared // change.go memoizes the go and defer statements of each function in it
	statements map[*ssa.Function]*changeStatements
	//declscope:shared // closure.go memoizes the variables each function's closures capture in it
	captured map[inspector.Cursor]map[*types.Var]bool
	//declscope:shared // call.go indexes the calls of each file by Lparen and by End in it
	calls map[*ast.File]fileCalls
	//declscope:shared // call.go keeps the functions referred to other than by a call in it (#377)
	uncalledFuncs map[types.Object]bool
	//declscope:shared // call.go indexes the calls of the analysed functions by callee in it (#377)
	callees map[*ssa.Function][]calleeCall
	//declscope:shared // stmt.go indexes the statements of each file in it (#377)
	stmtIndexes map[*ast.File]*stmtIndex
	//declscope:shared // virtual.go indexes the calls of each function by position in it (#377)
	ssaCalls map[*ssa.Function]map[token.Pos]*ssa.Call
	//declscope:shared // reassign.go indexes the variables calls are assigned to in it
	assigned map[*ast.File]map[token.Pos]ast.Expr
	//declscope:shared // change.go memoizes whether a Session changes the errors of each root's queries in it
	errorChanges map[ssa.Value]bool

	files   map[*token.File]*ast.File      // token.File -> ast.File mapping
	cursors map[*ast.File]inspector.Cursor // file cursors in the pass inspector

	discardsParam map[*ssa.Function]bool // immutable-param functions that discard a chain on a parameter (#346)

	//declscope:shared // param.go's paramCallersIsolate asks it
	immutableParam ImmutableParamRule // the inference's rules for immutable-param (#387)
}

// NewGenerator creates a new fix Generator. scopesCallbacks lists
// Scopes/Preload callback functions, whose *gorm.DB parameters cannot be made
// immutable-param, so the parameter-root fix is withheld for them (stage 2c);
// it may be nil. rt decides which values are already immutable, and funcs
// are the analysed functions, package-level closures and the package
// initializer included (#346), whose calls are checked before a parameter is
// declared immutable-param (#201, #245).
// seesAllCallers reports whether the pass sees every file that may call a
// function declared at a position: not so for a file other than a test file
// in a package with test files, which the pass without them cannot see
// (#245). cfgs is the CFG cache that the checker asks too, so a fix and its
// diagnostic share the answers (#299). discardsParam are the immutable-param
// functions that discard a chain on a parameter: no fix isolates an argument
// to one of them, since the isolation would drop the change (#346).
// importedFinisherOnly returns the finisher-only parameters of a function of
// another package, from its fact (see HandoffFinisherOnlyParams, #347); it
// may be nil. immutableParam holds the rules of the immutable-param
// inference that the directive fix must meet too (#387).
func NewGenerator(pass *analysis.Pass, insp *inspector.Inspector, scopesCallbacks map[*ssa.Function]bool, rt *tracer.RootTracer, funcs []*ssa.Function, seesAllCallers func(token.Pos) bool, cfgs *cfg.Analyzer, discardsParam map[*ssa.Function]bool, importedFinisherOnly func(*ssa.Function) []int, immutableParam ImmutableParamRule) *Generator {
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
	for cur := range insp.Root().Children() {
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
		cfg:             cfgs,
		discardsParam:   discardsParam,

		importedFinisherOnly: importedFinisherOnly,
		immutableParam:       immutableParam,
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
	//
	// Neither makes a value that the parameter merges with immutable, as in
	// `if c { db = db.Where("a") }` with db read twice after the if. Such a
	// value gets its own Session, as for a local root (#311). With the
	// directive, the rest of the fix proceeds for those Sessions, and the
	// directive alone is the fallback when it offers none.
	//
	// A discarded chain on the parameter, as in tx.Where("a"), changes the
	// caller's value in place. Under the directive the value is isolated, so
	// the chain would do nothing, and the body check of immutable-param
	// reports it (#346). Such a function never gets the directive.
	p, isParam := root.(*ssa.Parameter)
	var fallback []analysis.SuggestedFix
	if isParam {
		if !g.canFixParam(p) {
			return nil
		}
		if g.paramCallersIsolate(p) && !g.paramDiscardsChain(v.AllUses) {
			fallback = g.generateImmutableParamFix(p)
			if len(g.mergedSessionPositions(p, v.AllUses)) == 0 {
				return fallback
			}
		}
	}

	// A call with a tuple result, such as q, err := helper(db), is a root whose
	// value is not the *gorm.DB itself (#176). Appending Session() to it would
	// not compile, so no fix is offered.
	if !typeutil.IsGormDB(root.Type()) {
		return fallback
	}

	// A field of a struct a call returned (#181) has no chain expression to
	// append Session() to: the call returns the struct, not a *gorm.DB. The fix
	// belongs where the function builds the struct, so none is offered.
	if _, ok := root.(*tracer.FieldRoot); ok {
		return fallback
	}
	// Nor has what a call may store through a variable's address (#345).
	if _, ok := root.(*tracer.PointeeRoot); ok {
		return fallback
	}
	// Nor has a read from a container (#374): the call that made the value
	// it holds is not known.
	if _, ok := root.(*tracer.ElemRoot); ok {
		return fallback
	}

	allUses := v.AllUses
	if len(allUses) == 0 {
		return fallback // No uses to fix
	}

	// The variable a reassignment rewrites. nil means no reassignment is safe,
	// and the fix falls back to a Session at the root.
	target := g.findReassignTarget(root, allUses)

	// Phase 1: Identify non-finisher uses that need reassignment
	reassigned, ok := g.findReassignedChains(allUses, target)
	if !ok {
		return fallback
	}

	// A discarded statement on a value merged with the root, which the uses
	// of another edge list, would change a copy once a Session makes that
	// value immutable. It is reassigned too, or there is no fix (#373).
	if !g.reassignMergedChains(root, allUses, target, reassigned) {
		return fallback
	}

	// Code that changes the root in place, other than a discarded statement on
	// its variable, would change a copy once the root gets a Session (#243).
	if g.changedElsewhere(v, len(reassigned) == 0) {
		return fallback
	}

	// A query whose error nothing reads leaves it on a mutable root, and a
	// later query reports it and is skipped. A Session would let that query
	// run and report its own outcome (#372).
	if g.finisherErrorChanges(v) {
		return fallback
	}

	// A discarded statement on a parameter also changes the caller's value,
	// and the caller may rely on that. Reassigning it would change only the
	// parameter, unless the function returns a *gorm.DB that can carry the
	// change back (#243), and every caller reads that result (#312). So does
	// one on a value the parameter's variable holds, such as a Where merged
	// with the parameter in `if admin { q = q.Where("admin") }`: a mutable
	// Where returns the caller's value itself (#373).
	if len(reassigned) > 0 && g.paramReassignmentsDropChanges(root, target) {
		return fallback
	}

	// Phase 2: Simulate reassignments and find roots needing Session
	virtualUses := g.simulateVirtualRoots(root, allUses, reassigned)
	rootsNeedingSession := g.findRootsNeedingSession(virtualUses, reassigned, usesOrdered(v))
	rootsNeedingSession = g.mergedReassignmentsNeedingSession(root, virtualUses, rootsNeedingSession)
	if reassignsInClosure(root, allUses, reassigned) && !slices.ContainsFunc(rootsNeedingSession, func(vr virtualRootKey) bool { return vr.isOriginal }) {
		rootsNeedingSession = append(rootsNeedingSession, virtualRootKey{isOriginal: true, pos: root.Pos()})
	}

	// Generate TextEdits. A parameter declared immutable-param keeps its
	// directive.
	var edits []analysis.TextEdit
	message := "Add reassignment and Session to fix reuse"
	if fallback != nil {
		edits = append(edits, fallback[0].TextEdits...)
		message = "Declare //gormreuse:immutable-param and add Session to the values merged with the parameter"
	}

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
	paramEdits, sites := g.rootSessionSites(root, rootsNeedingSession, allUses, fallback != nil)
	edits = append(edits, paramEdits...)

	// A Session drops the Attrs, Assign and InstanceSet state of the
	// Statement it copies, so no Session is written for a root that may
	// carry it (#344), or for a value merged with it that may (#343). A
	// reassignment alone changes nothing at run time.
	if len(sites) > 0 && (g.tracer.SessionDropsState(root) || g.mergedDropsState(root, allUses)) {
		return fallback
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
			Message:   message,
			TextEdits: edits,
		},
	}
}

// rootSessionSites returns where the Sessions go for roots, the original
// root and the roots its reassignments create that need one. A parameter
// declared immutable-param (declared) gets no Session of its own, only on
// the values merged with it.
func (g *Generator) rootSessionSites(root ssa.Value, roots []virtualRootKey, allUses []pollution.UsageInfo, declared bool) (edits []analysis.TextEdit, sites []sessionSite) {
	p, isParam := root.(*ssa.Parameter)
	for _, vr := range roots {
		switch {
		case vr.isOriginal && isParam:
			if !declared {
				sites = append(sites, g.paramSessionSite(p))
			}
			for _, pos := range g.mergedSessionPositions(p, allUses) {
				sites = append(sites, sessionSite{pos: pos})
			}
		case vr.isOriginal:
			for _, pos := range g.sessionPositionsForRoot(vr.pos, root, allUses) {
				sites = append(sites, sessionSite{pos: pos})
			}
			paramEdits, paramSites := g.mergedParamFixes(root, allUses)
			edits = append(edits, paramEdits...)
			sites = append(sites, paramSites...)
		case !g.virtualRootEndsImmutable(vr):
			sites = append(sites, sessionSite{pos: vr.end})
		}
	}
	// A reassignment that takes the root's place on the edge of a Phi used
	// twice needs the Phi's other edges isolated too, as the root does (#373).
	if !isParam && !slices.ContainsFunc(roots, func(vr virtualRootKey) bool { return vr.isOriginal }) &&
		slices.ContainsFunc(roots, func(vr virtualRootKey) bool { return g.mergeReassignmentReachesTwice(root, vr) }) {
		for _, pos := range g.mergedSessionPositions(root, allUses) {
			sites = append(sites, sessionSite{pos: pos})
		}
		paramEdits, paramSites := g.mergedParamFixes(root, allUses)
		edits = append(edits, paramEdits...)
		sites = append(sites, paramSites...)
	}
	return edits, sites
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
		call = g.callAtLparen(file, lparen)
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
	// The callee discards a chain on its parameter, which changes this
	// argument in place today. Under the Session it would change a copy, so
	// the callee's own queries would lose the condition (#346).
	if g.discardsParam[origin.Declared(g.tracer.Site(v.Call).Callee)] {
		return nil
	}
	// The Session would drop the Attrs, Assign or InstanceSet state the
	// argument carries (#344).
	if ssaArg, ok := pollutionsource.UnwrapGormDB(v.Call.Common().Args[len(v.Call.Common().Args)-len(g.tracer.Site(v.Call).Args)+v.Param]); ok && g.tracer.SessionDropsState(ssaArg) {
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
