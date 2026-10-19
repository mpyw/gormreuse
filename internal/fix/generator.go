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
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/edge"
	"golang.org/x/tools/go/ast/inspector"
	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/directive"
	"github.com/mpyw/gormreuse/internal/ssa/callsite"
	"github.com/mpyw/gormreuse/internal/ssa/cfg"
	"github.com/mpyw/gormreuse/internal/ssa/handler"
	"github.com/mpyw/gormreuse/internal/ssa/pollution"
	"github.com/mpyw/gormreuse/internal/ssa/pollutionsource"
	"github.com/mpyw/gormreuse/internal/ssa/tracer"
	"github.com/mpyw/gormreuse/internal/typeutil"
)

// Generator generates SuggestedFix for a violation.
type Generator struct {
	pass            *analysis.Pass
	fset            *token.FileSet
	files           map[*token.File]*ast.File      // token.File -> ast.File mapping
	cursors         map[*ast.File]inspector.Cursor // file cursors in the pass inspector
	scopesCallbacks map[*ssa.Function]bool         // Scopes/Preload callbacks (no parameter-root fix)
	tracer          *tracer.RootTracer             // the analysis' mutability rule
	funcs           []*ssa.Function                // the analysed functions, to find callers
	seesAllCallers  func(token.Pos) bool           // whether this variant sees every caller of code at a position
}

// New creates a new fix Generator. scopesCallbacks lists Scopes/Preload callback
// functions, whose *gorm.DB parameters cannot be made immutable-param, so the
// parameter-root fix is withheld for them (stage 2c); it may be nil. rt
// decides which values are already immutable, and funcs are the analysed
// functions, package-level closures included, whose calls are checked before
// a parameter is declared immutable-param (#201, #245). seesAllCallers
// reports whether the pass sees every file that may call a function declared
// at a position: not so for a file other than a test file in a package with
// test files, which the pass without them cannot see (#245).
func New(pass *analysis.Pass, scopesCallbacks map[*ssa.Function]bool, rt *tracer.RootTracer, funcs []*ssa.Function, seesAllCallers func(token.Pos) bool) *Generator {
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
		if g.callersAllIsolate(p) {
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
	virtualUses := g.simulateReassignments(root, allUses, reassigned)
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
		case !g.endsImmutable(vr):
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
		if i := v.Param + receiverArgs(g.pass.TypesInfo, call, v.Call); i < len(call.Args) && typeutil.IsGormDB(g.pass.TypesInfo.TypeOf(call.Args[i])) {
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
// takes the receiver as its first parameter, and 0 otherwise.
func receiverArgs(info *types.Info, call *ast.CallExpr, instr ssa.CallInstruction) int {
	declared := callsite.SiteOf(instr, nil).Declared
	sig, ok := info.TypeOf(call.Fun).Underlying().(*types.Signature)
	if !ok || declared == nil {
		return 0
	}
	return sig.Params().Len() - declared.Signature.Params().Len()
}

// fileCursor returns the cursor for the given file in the pass inspector.
func (g *Generator) fileCursor(file *ast.File) inspector.Cursor {
	return g.cursors[file]
}

// canFixParam reports whether a fix may rewrite the function that declares
// p, a reused *gorm.DB parameter. It may not when:
//   - the function is a Scopes/Preload callback — immutable-param is rejected
//     there (the parameter genuinely receives a mid-chain value), and a Session
//     the callback returns leaks gorm's transaction (go-gorm/gorm#7592);
//   - the function returns a *gorm.DB and code this pass cannot see may call
//     it (see calledOnlyDirectly). It may be a Scopes callback, as in
//     `var scopes = []func(*gorm.DB) *gorm.DB{scope}`;
//   - the enclosing function is not a named declaration (a closure/func literal) —
//     directive placement on literals is left to the user.
func (g *Generator) canFixParam(p *ssa.Parameter) bool {
	fn := directive.DeclaredFunc(p.Parent())
	if g.scopesCallbacks[fn] {
		return false
	}
	if _, ok := fn.Syntax().(*ast.FuncDecl); !ok {
		return false
	}
	return !typeutil.HasGormDBResult(fn.Signature.Results()) || g.calledOnlyDirectly(fn)
}

// callersAllIsolate reports whether no call to the function that declares p
// breaks the caller-side contract of //gormreuse:immutable-param, so
// declaring it keeps the reuse checked (#201). Each argument of each call in
// the analysed functions is checked as the caller-side check does
// (handler.BreaksImmutableParam), not only p's (#245). It must also see every
// caller, so the function must be called only directly (see
// calledOnlyDirectly), and the pass must see every file of the package (see
// New). Declaring any other function would hide the reuse from the callers it
// cannot see, or move it to a caller that the caller-side check reports, so
// -fix would not end clean. The variant of a package without its test files
// and the test variant both take the body Session for a function outside the
// test files, so their fixes agree.
func (g *Generator) callersAllIsolate(p *ssa.Parameter) bool {
	fn := directive.DeclaredFunc(p.Parent())
	if !g.seesAllCallers(fn.Pos()) || !g.calledOnlyDirectly(fn) {
		return false
	}
	loops := cfg.NewAnalyzer()
	for _, caller := range g.funcs {
		var loopInfo *cfg.LoopInfo
		for _, block := range caller.Blocks {
			for _, instr := range block.Instrs {
				call, ok := instr.(ssa.CallInstruction)
				if !ok {
					continue
				}
				site := g.tracer.Site(call)
				if directive.DeclaredFunc(site.Callee) != fn {
					continue
				}
				if loopInfo == nil {
					loopInfo = loops.DetectLoops(caller)
				}
				// Every argument, not only p's: the caller-side check
				// asks the same of each one (#245).
				for i := range call.Common().Args {
					if handler.BreaksImmutableParam(site, i, g.tracer, loopInfo) {
						return false
					}
				}
			}
		}
	}
	return true
}

// calledOnlyDirectly reports whether every caller of fn is a call in this
// pass that names it: fn is an unexported function, not a method, and each
// reference to it is the function of a call. An exported function can be
// called from another package, which the caller-side check does not see
// (#195). A method can be called through an interface, and a function value
// can be called anywhere.
func (g *Generator) calledOnlyDirectly(fn *ssa.Function) bool {
	obj := fn.Object()
	if obj.Exported() || fn.Signature.Recv() != nil {
		return false
	}
	for _, file := range g.pass.Files {
		for c := range g.fileCursor(file).Preorder((*ast.Ident)(nil)) {
			if g.pass.TypesInfo.Uses[c.Node().(*ast.Ident)] == obj && !isCalledFunc(c) {
				return false
			}
		}
	}
	return true
}

// isCalledFunc reports whether cur, an identifier, is the function of a call,
// possibly instantiated or in parentheses, as in f(x), f[int](x) or (f)(x).
func isCalledFunc(cur inspector.Cursor) bool {
	for {
		switch cur.ParentEdgeKind() {
		case edge.ParenExpr_X, edge.IndexExpr_X, edge.IndexListExpr_X:
			cur = cur.Parent()
		case edge.CallExpr_Fun:
			return true
		default:
			return false
		}
	}
}

// generateImmutableParamFix suggests annotating the enclosing function with
// //gormreuse:immutable-param when a *gorm.DB PARAMETER is the reused root
// and every caller passes an isolated value (see callersAllIsolate). The
// caller-side check (stage 2b) then keeps the callers isolated.
func (g *Generator) generateImmutableParamFix(p *ssa.Parameter) []analysis.SuggestedFix {
	fd := p.Parent().Syntax().(*ast.FuncDecl)
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

// paramSessionSite makes p, a parameter, immutable by rebinding it at the top
// of its function's body: `tx = tx.Session(&gorm.Session{})`. Every later use,
// including one through an alias, then starts from the Session.
func (g *Generator) paramSessionSite(p *ssa.Parameter) sessionSite {
	body := p.Parent().Syntax().(*ast.FuncDecl).Body
	// The parameter is used, so the body has a statement.
	first := body.List[0]
	sep := "; "
	// Unadjusted, so a //line directive cannot move the line or zero the column.
	if line := g.fset.PositionFor(first.Pos(), false); line.Line != g.fset.PositionFor(body.Lbrace, false).Line {
		sep = "\n" + strings.Repeat("\t", line.Column-1)
	}
	name := p.Name()
	return sessionSite{pos: first.Pos(), prefix: name + " = " + name, suffix: sep}
}

// changedElsewhere reports whether a fix could make an in-place change to
// v's root disappear, other than that of a discarded statement on its
// variable (see findReassignedChains). That is so when:
//   - a use hands the root to other code, such as a helper, a closure or a
//     container, which may change it in place (see UsageInfo.Escapes), and
//     a later use may see the change (see escapeSeen);
//   - the Error, RowsAffected or Statement field is read from the root, or
//     from a chain on it that runs no query. Such a read sees what the calls
//     on the root left in it: the errors of its queries, or the conditions of
//     a chain whose only result is the field, as in
//     `if err := q.Where("a").Error; ...`;
//   - a go statement hands the root to a call, as in `go applyTenant(q)`. The
//     goroutine may change it while the uses after the statement run.
//
// A Session at the root makes each of them see a copy (#243).
func (g *Generator) changedElsewhere(v pollution.Violation) bool {
	loops := cfg.NewAnalyzer()
	ordered := usesOrdered(v)
	for _, use := range v.AllUses {
		if use.Escapes && escapeSeen(tracer.RootBlock(v.Root), use, v.AllUses, ordered, g.tracer.FuncVars()) {
			return true
		}
	}
	for _, f := range withAnonFuncs(v.Root.Parent()) {
		loopInfo := loops.DetectLoops(f)
		for _, block := range f.Blocks {
			for _, instr := range block.Instrs {
				switch instr := instr.(type) {
				case *ssa.FieldAddr:
					if isGormStateField(instr) && g.readsRootState(v.Root, instr.X, loopInfo) {
						return true
					}
				case *ssa.Go:
					if slices.ContainsFunc(instr.Call.Args, func(arg ssa.Value) bool {
						gormArg, ok := pollutionsource.UnwrapGormDB(arg)
						return ok && g.tracer.FindMutableRoot(gormArg, loopInfo) == v.Root
					}) {
						return true
					}
				}
			}
		}
	}
	return false
}

// escapeSeen reports whether code may see what escape, a use that hands the
// root to other code, changes in place. A use of the root may, when it is
// reachable from escape or escape runs again in a loop. So may a use of the
// result of a call that returns a *gorm.DB, such as `q = withTenant(q)`: the
// result may be the root itself. Without the order of every use (see
// usesOrdered) the change is assumed to be seen. An escape that is the last
// use, as in `q.Find(&a); render(q)`, keeps its fix. def is the block that
// defines the root: an escape runs again when the checker's cfg.Repeats says
// so. vars resolves calls through package-level function variables.
func escapeSeen(def *ssa.BasicBlock, escape pollution.UsageInfo, uses []pollution.UsageInfo, ordered bool, vars callsite.FuncVars) bool {
	if !ordered || repeatsAfter(escape.Block, def) || returnsGormDBAt(escape, vars) {
		return true
	}
	a := cfg.NewAnalyzer()
	return slices.ContainsFunc(uses, func(use pollution.UsageInfo) bool {
		if use.Block == escape.Block {
			return use.Pos > escape.Pos
		}
		return a.CanReach(escape.Block, use.Block)
	})
}

// returnsGormDBAt reports whether use is an argument of a call that returns
// a *gorm.DB, alone or in a tuple, other than a *gorm.DB method. A method
// such as base.Model(q) returns a chain on its receiver, also when it is
// called as a method value.
func returnsGormDBAt(use pollution.UsageInfo, vars callsite.FuncVars) bool {
	return slices.ContainsFunc(use.Block.Instrs, func(instr ssa.Instruction) bool {
		call, ok := instr.(*ssa.Call)
		return ok && call.Pos() == use.Pos && typeutil.HasGormDBResult(call.Type()) &&
			!callsite.SiteOf(call, vars).IsGormMethod()
	})
}

// withAnonFuncs returns fn and the closures it declares, at any depth.
func withAnonFuncs(fn *ssa.Function) []*ssa.Function {
	fns := []*ssa.Function{fn}
	for i := 0; i < len(fns); i++ {
		fns = append(fns, fns[i].AnonFuncs...)
	}
	return fns
}

// readsRootState reports whether x, a *gorm.DB whose state field is read, is
// root or a chain on root of *gorm.DB methods that runs no query. The tracer
// makes each call its own root, so the chain is walked down its receivers.
// The walk stops at a finisher, whose result holds its own query's outcome, as
// in q.Where("a").Find(&x).Error, and at a Session-family call, whose result
// is a copy.
func (g *Generator) readsRootState(root, x ssa.Value, loopInfo *cfg.LoopInfo) bool {
	for {
		if g.tracer.FindMutableRoot(x, loopInfo) == root {
			return true
		}
		call, ok := x.(*ssa.Call)
		if !ok {
			return false
		}
		site := g.tracer.Site(call)
		if !site.IsGormMethod() || isFinisher(site.Method()) || typeutil.IsImmutableReturningBuiltin(site.Method()) {
			return false
		}
		x = site.Recv
	}
}

// isGormStateField reports whether fa takes the Error, RowsAffected or
// Statement field of a *gorm.DB.
func isGormStateField(fa *ssa.FieldAddr) bool {
	if !typeutil.IsGormDB(fa.X.Type()) {
		return false
	}
	name := fa.X.Type().Underlying().(*types.Pointer).Elem().Underlying().(*types.Struct).Field(fa.Field).Name()
	return name == "Error" || name == "RowsAffected" || name == "Statement"
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
	block      *ssa.BasicBlock // The block of the reassignment, or of the original root's definition (nil if unknown)
}

// simulateReassignments simulates Phase 1 reassignments and returns virtual uses.
// Returns a map from virtual root keys to their uses.
//
// A use is a use of every root it may see: each earlier reassignment that can
// reach it, back to the first one whose block dominates it. When none
// dominates it, it may also see the original root. So in
// `if c { q.Where("a") }; q.Where("b").Find(nil); q.Where("c").Find(nil)` the
// original root, used on the path where c is false, needs a Session as well
// as the reassignment in the if (#202). Uses are taken in position order, the
// uses of one block in run order (see runOrderedUses), so
// a loop's next iteration is not seen here (see findRootsNeedingSession).
func (g *Generator) simulateReassignments(root ssa.Value, uses []pollution.UsageInfo, reassigned map[token.Pos]token.Pos) map[virtualRootKey][]pollution.UsageInfo {
	sortedUses := runOrderedUses(uses)

	original := virtualRootKey{isOriginal: true, pos: root.Pos(), block: tracer.RootBlock(root)}
	virtualUses := make(map[virtualRootKey][]pollution.UsageInfo)
	cfgAnalyzer := cfg.NewAnalyzer()

	// The virtual roots made so far, in position order.
	var made []virtualRootKey
	for _, use := range sortedUses {
		dominated := false
		for _, vr := range slices.Backward(made) {
			reaches, dominates := reassignmentReaches(cfgAnalyzer, vr.block, use.Block)
			if reaches {
				virtualUses[vr] = append(virtualUses[vr], use)
			}
			if dominates {
				dominated = true
				break
			}
		}
		if !dominated {
			virtualUses[original] = append(virtualUses[original], use)
		}

		// If non-finisher, this use's result becomes the new root
		if end, ok := reassigned[use.Pos]; ok {
			// Create a new virtual root at this position. Its Session goes
			// after the chain's last call: one after an earlier call would be
			// undone by the calls that follow it.
			vr := virtualRootKey{isOriginal: false, pos: use.Pos, end: end, block: use.Block}
			made = append(made, vr)
			// A root with no later use still needs a Session when a loop
			// runs it again (see findRootsNeedingSession).
			virtualUses[vr] = nil
		}
	}

	return virtualUses
}

// runOrderedUses returns uses sorted by position, except that the uses of one
// block are in the order the block runs them. go/ssa puts the post statement
// of a `for` loop at the end of the body's block when nothing else jumps to
// it, so in `for i := 0; i < 2; q.Where("a") { q.Find(nil); i++ }` the post
// statement's call comes first by position but runs after q.Find (#247). The
// uses of each block keep the places that position gives them.
func runOrderedUses(uses []pollution.UsageInfo) []pollution.UsageInfo {
	sorted := slices.Clone(uses)
	slices.SortFunc(sorted, func(a, b pollution.UsageInfo) int {
		return cmp.Compare(a.Pos, b.Pos)
	})
	slots := make(map[*ssa.BasicBlock][]int)
	for i, use := range sorted {
		slots[use.Block] = append(slots[use.Block], i)
	}
	ordered := slices.Clone(sorted)
	for block, idx := range slots {
		if len(idx) < 2 {
			continue
		}
		inBlock := make([]pollution.UsageInfo, len(idx))
		for k, i := range idx {
			inBlock[k] = sorted[i]
		}
		slices.SortStableFunc(inBlock, func(a, b pollution.UsageInfo) int {
			return cmp.Compare(runIndex(block, a.Pos), runIndex(block, b.Pos))
		})
		for k, i := range idx {
			ordered[i] = inBlock[k]
		}
	}
	return ordered
}

// runIndex returns the index in block of the instruction at pos, the
// position of a use, or -1 when there is none, as for a use recorded at a
// closure's call site. Such uses keep their position order.
func runIndex(block *ssa.BasicBlock, pos token.Pos) int {
	return slices.IndexFunc(block.Instrs, func(instr ssa.Instruction) bool { return instr.Pos() == pos })
}

// endsImmutable reports whether the chain that vr, a virtual root, reassigns
// already ends in a call whose result is immutable by the tracer's rule, as
// q.Where("a").Session(&gorm.Session{}) does. A second Session after it
// would be redundant (#202).
func (g *Generator) endsImmutable(vr virtualRootKey) bool {
	file := g.findFileContaining(vr.end)
	lparen := token.NoPos
	for c := range g.fileCursor(file).Preorder(callExprNodeTypes...) {
		if call := c.Node().(*ast.CallExpr); call.End() == vr.end {
			lparen = call.Lparen
			break
		}
	}
	for _, block := range vr.block.Parent().Blocks {
		for _, instr := range block.Instrs {
			// A statement in parentheses, (q.Where("a")), ends after its
			// call, and leaves lparen invalid.
			if call, ok := instr.(*ssa.Call); ok && lparen.IsValid() && call.Pos() == lparen {
				return g.tracer.IsImmutableCall(call)
			}
		}
	}
	return false
}

// reassignmentReaches reports whether a use in block use, later in position,
// can see a reassignment made in block def, and whether every path to it
// passes the reassignment, so that it cannot see any earlier root. Blocks of
// different functions, such as a use inside a closure called inline, are
// taken in position order, so the reassignment both reaches and dominates
// the use.
func reassignmentReaches(a *cfg.Analyzer, def, use *ssa.BasicBlock) (reaches, dominates bool) {
	if def == use || def.Parent() != use.Parent() {
		return true, true
	}
	return a.CanReach(def, use), def.Dominates(use)
}

// findRootsNeedingSession finds the roots — the original AND reassignment-created
// virtual roots — that still branch 2+ times on one path after reassignment
// simulation and therefore need a Session() to become immutable.
//
// Emitting Session for virtual roots too is what makes a fix single-pass
// complete (#71 defect 2): for `q := db.Where("base"); q.Where("a");
// q.Where("b").Find(); q.Where("c"); q.Where("d").Find()` the reassignment of
// the virtual root created at `q.Where("a")` still branches twice, so it needs
// `.Session()` — without it, re-linting the "fixed" output still warns.
//
// Two uses of a root branch it when the earlier one can reach the later one
// (see usesConflict). A reassignment does not branch the root it reads with
// the uses after it: on a path through the reassignment those uses see the
// new root, so `for ... { q = q.Where(x) }; q.Find(nil)` needs no Session on
// the original root.
//
// The simulation orders uses by position, so it misses the uses that a loop's
// next iteration makes of a virtual root created in the loop body. Such a root
// always gets a Session (#199): in `for { q.Where("id").Find(nil); q = q.Where("age") }`
// the next iteration's q.Where("id") would otherwise change it in place.
//
// The path rules hold only when ordered is set, that is, when the uses are
// every use of the root, in the function that defines it (see usesOrdered).
// Otherwise a root gets a Session when it has a use other than the one
// reassignment that reads it.
func (g *Generator) findRootsNeedingSession(virtualUses map[virtualRootKey][]pollution.UsageInfo, reassigned map[token.Pos]token.Pos, ordered bool) []virtualRootKey {
	var roots []virtualRootKey
	cfgAnalyzer := cfg.NewAnalyzer()
	var def *ssa.BasicBlock
	for root := range virtualUses {
		if root.isOriginal {
			def = root.block
		}
	}
	for root, uses := range virtualUses {
		var needs bool
		switch {
		case !root.isOriginal && repeatsAfter(root.block, def):
			needs = true
		case !ordered:
			needs = len(uses) >= 2 || len(uses) == 1 && (!root.isOriginal || !reassigned[uses[0].Pos].IsValid())
		case root.isOriginal && usedAgainInLoop(root.block, uses, reassigned):
			needs = true
		default:
			needs = usesConflict(cfgAnalyzer, uses, reassigned)
		}
		if needs {
			roots = append(roots, root)
		}
	}
	// Deterministic order (map iteration is random) so edits/goldens are stable.
	slices.SortFunc(roots, func(a, b virtualRootKey) int { return cmp.Compare(a.pos, b.pos) })
	return roots
}

// reassignsInClosure reports whether one of reassigned is in a closure of the
// function that defines root, such as `func() { q.Where("a") }()` or the body
// of a range-over-func loop. The reassignment writes the captured variable,
// and detection does not follow a store in a closure (see
// closureModifiesCaptured): the uses after it still read root. So root gets a
// Session too, or the next pass would add it (#247).
func reassignsInClosure(root ssa.Value, uses []pollution.UsageInfo, reassigned map[token.Pos]token.Pos) bool {
	return slices.ContainsFunc(uses, func(use pollution.UsageInfo) bool {
		_, ok := reassigned[use.Pos]
		return ok && use.Block.Parent() != root.Parent()
	})
}

// usesConflict reports whether two of uses, which are sorted by position,
// may both run on one path: the earlier one can reach the later one, and the
// earlier one is not a reassignment. The uses are in one function.
func usesConflict(a *cfg.Analyzer, uses []pollution.UsageInfo, reassigned map[token.Pos]token.Pos) bool {
	for i, first := range uses {
		if _, ok := reassigned[first.Pos]; ok {
			continue
		}
		for _, later := range uses[i+1:] {
			if a.CanReach(first.Block, later.Block) {
				return true
			}
		}
	}
	return false
}

// usedAgainInLoop reports whether one of uses, other than a reassignment,
// runs again on the same value of a root defined in block def: the use is in
// a loop that does not define the root again, as in
// `q := db.Where("x"); for ... { q.Find(nil) }`. A reassignment reads the
// root only on the first iteration; later ones read its result. The uses are
// in def's function.
func usedAgainInLoop(def *ssa.BasicBlock, uses []pollution.UsageInfo, reassigned map[token.Pos]token.Pos) bool {
	return slices.ContainsFunc(uses, func(use pollution.UsageInfo) bool {
		_, isReassignment := reassigned[use.Pos]
		return !isReassignment && repeatsAfter(use.Block, def)
	})
}

// usesOrdered reports whether the fix sees every use of v's root, in the
// function that defines the root, so that findRootsNeedingSession can follow
// their paths. It does not when the root has uses that v.AllUses does not
// list (see pollution.Violation.HiddenUses), when the reported use is not in
// v.AllUses, as for a use through a Phi that the tracker files under another
// of its edges, or when a use is in a closure, such as the body of a
// range-over-func loop.
func usesOrdered(v pollution.Violation) bool {
	def := tracer.RootBlock(v.Root)
	if v.HiddenUses || def == nil {
		return false
	}
	reported := false
	for _, use := range v.AllUses {
		if use.Block.Parent() != def.Parent() {
			return false
		}
		reported = reported || use.Pos == v.Pos
	}
	return reported
}

// repeatsAfter reports whether a use in block use may run more than once for
// one run of def, as the checker asks it (cfg.Repeats): a fix and the
// diagnostic it fixes agree on which uses run again.
func repeatsAfter(use, def *ssa.BasicBlock) bool {
	return cfg.RepeatsOf(use.Parent()).RepeatsAfter(use, def)
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

// sessionSite is a place where a fix writes a Session: prefix, then
// ".Session(&gorm.Session{})", then suffix, inserted at pos. A Session
// appended to a call has neither; a parameter's has a whole statement around
// it (see paramSessionSite).
type sessionSite struct {
	pos            token.Pos
	prefix, suffix string
}

// sessionEdits writes a Session at each of sites. It qualifies Session with a
// name that refers to gorm.io/gorm at every one of them, and adds an import of
// gorm.io/gorm to a file where no such name exists (see gormNameAt).
func (g *Generator) sessionEdits(sites []sessionSite) []analysis.TextEdit {
	byFile := make(map[*ast.File][]sessionSite)
	var files []*ast.File
	for _, site := range sites {
		file := g.findFileContaining(site.pos)
		if _, seen := byFile[file]; !seen {
			files = append(files, file)
		}
		byFile[file] = append(byFile[file], site)
	}
	var edits []analysis.TextEdit
	for _, file := range files {
		var positions []token.Pos
		for _, site := range byFile[file] {
			positions = append(positions, site.pos)
		}
		qualifier, importName := g.gormNameAt(file, positions)
		for _, site := range byFile[file] {
			edits = append(edits, analysis.TextEdit{
				Pos:     site.pos,
				End:     site.pos,
				NewText: []byte(site.prefix + ".Session(&" + qualifier + "Session{})" + site.suffix),
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
// For a Phi node it returns a position for each incoming edge that can take
// one. Otherwise it returns the position after the call at pos and those of
// the other values its uses may read instead of it (see mergedEdges).
//
// The root's own Session is needed even when the Phi it flows into is used
// once: the root has 2+ uses, and some of them may come before the merge, as
// in `q := db.Where("x"); q.Find(nil); q.Count(nil); if c { q = db.Where("z") }`
// (#202).
func (g *Generator) sessionPositionsForRoot(pos token.Pos, root ssa.Value, uses []pollution.UsageInfo) []token.Pos {
	if phi, isPhi := root.(*ssa.Phi); isPhi {
		if !phiUsedTwice(phi) {
			return nil
		}
		return g.edgeSessionPositions(phi.Edges)
	}

	var positions []token.Pos
	if end := g.sessionEndAfterCall(pos); end.IsValid() {
		positions = append(positions, end)
	}
	return append(positions, g.edgeSessionPositions(mergedEdges(root, uses))...)
}

// edgeSessionPositions returns where Sessions go on edges, the values that
// merge with a root. Only edges that don't already have Session get one, and
// only an edge defined by a *gorm.DB call can (see sessionEndAfterCall).
func (g *Generator) edgeSessionPositions(edges []ssa.Value) []token.Pos {
	var positions []token.Pos
	for _, edge := range edges {
		// Skip nil constants
		if c, ok := edge.(*ssa.Const); ok && c.Value == nil {
			continue
		}

		// Skip an edge that is already immutable by the tracer's rule: its own
		// call, the last of its chain, returns an immutable value. A Session
		// earlier in the chain, as in db.Session(s).Where("y"), does not count,
		// and the edge would stay reported after every pass (#201).
		if g.tracer.IsImmutableCall(edge) {
			continue
		}

		if end := g.sessionEndAfterCall(edge.Pos()); end.IsValid() {
			positions = append(positions, end)
		}
	}
	return positions
}

// phiUsedTwice reports whether the result of phi is used more than once.
func phiUsedTwice(phi *ssa.Phi) bool {
	return len(*phi.Referrers()) > 1
}

// mergedEdges returns the other values that the uses of root may read
// instead of it, when they are read twice:
//   - the edges of a Phi that takes root, when the Phi's result is used twice;
//   - for a variable that a closure captures, which SSA keeps in memory with
//     no Phi, the other values stored to it whose store may run before two of
//     uses. The tracer resolves each read of the variable to its first store
//     (see tracer.AllocStoredValues), so uses holds every read of it.
//
// So in `if c { q = db.Where("a") } else { q = db.Where("b") }` with q read
// twice in a goroutine, both stores get a Session, not only the root's (#247).
func mergedEdges(root ssa.Value, uses []pollution.UsageInfo) []ssa.Value {
	instr, ok := root.(ssa.Instruction)
	if !ok {
		return nil
	}
	fn := instr.Parent()
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			if phi, ok := instr.(*ssa.Phi); ok && slices.Contains(phi.Edges, root) {
				if !phiUsedTwice(phi) {
					return nil
				}
				return phi.Edges
			}
		}
	}

	alloc := storedTo(fn, root)
	if alloc == nil {
		return nil
	}
	var edges []ssa.Value
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			if store, ok := instr.(*ssa.Store); ok && store.Addr == alloc && store.Val != root && storeReadTwice(store, uses) {
				edges = append(edges, store.Val)
			}
		}
	}
	return edges
}

// storedTo returns the variable of fn that v is stored to, or nil.
func storedTo(fn *ssa.Function, v ssa.Value) *ssa.Alloc {
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			if store, ok := instr.(*ssa.Store); ok && store.Val == v {
				alloc, _ := store.Addr.(*ssa.Alloc)
				return alloc
			}
		}
	}
	return nil
}

// storeReadTwice reports whether two of uses may run after store. A use in
// another function, such as a closure, may run after it whatever its block.
func storeReadTwice(store *ssa.Store, uses []pollution.UsageInfo) bool {
	a := cfg.NewAnalyzer()
	after := 0
	for _, use := range uses {
		switch {
		case use.Block.Parent() != store.Parent():
		case use.Block == store.Block():
			if use.Pos < store.Pos() && !cfg.RepeatsOf(store.Parent()).RunsAgain(use.Block) {
				continue
			}
		case !a.CanReach(store.Block(), use.Block):
			continue
		}
		after++
	}
	return after >= 2
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

// findRootVar returns the variable of root when it is a parameter (#201).
// For a call it returns the variable the call is assigned to by
// `v := <call>`, `v = <call>` or `var v = <call>`, or nil. SSA places a call
// at its Lparen. A root that is not a call, such as a Phi, gets nil, so the
// fix falls back to a Session. The tracer usually resolves a Phi to its
// edges; a loop variable swap keeps the Phi as the root.
func (g *Generator) findRootVar(root ssa.Value) *types.Var {
	// A parameter is held by its own variable.
	if p, ok := root.(*ssa.Parameter); ok {
		v, _ := p.Object().(*types.Var)
		return v
	}
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
