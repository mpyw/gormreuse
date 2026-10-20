package fix

import (
	"go/ast"
	"slices"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/ssa/handler"
	"github.com/mpyw/gormreuse/internal/ssa/origin"
	"github.com/mpyw/gormreuse/internal/ssa/pollution"
	"github.com/mpyw/gormreuse/internal/typeutil"
)

// canFixParam reports whether a fix may rewrite the function that declares
// p, a reused *gorm.DB parameter. It may not when:
//   - the function is a Scopes/Preload callback — immutable-param is rejected
//     there (the parameter genuinely receives a mid-chain value), and a Session
//     the callback returns leaks gorm's transaction (go-gorm/gorm#7592);
//   - the function returns a *gorm.DB and code this pass cannot see may call
//     it (see calledOnlyDirectly). It may be a Scopes callback, as in
//     `var scopes = []func(*gorm.DB) *gorm.DB{scope}`. A closure has no
//     name, so any code may call it: one with a *gorm.DB result gets no fix.
//
// A closure without a *gorm.DB result, such as a Transaction callback, gets
// the body Session as a function declaration does (#313).
//
//declscope:shared // generator.go's Generate withholds a parameter's fix with it
func (g *Generator) canFixParam(p *ssa.Parameter) bool {
	fn := origin.Declared(p.Parent())
	if g.scopesCallbacks[fn] {
		return false
	}
	switch fn.Syntax().(type) {
	case *ast.FuncDecl:
		return !typeutil.HasGormDBResult(fn.Signature.Results()) || g.calledOnlyDirectly(fn)
	case *ast.FuncLit:
		return !typeutil.HasGormDBResult(fn.Signature.Results())
	}
	return false
}

// paramCallersIsolate reports whether no call to the function that declares p
// breaks the caller-side contract of //gormreuse:immutable-param, so declaring
// it keeps the reuse checked (#201). Each argument of each call in the
// analysed functions is checked as the caller-side check does
// (handler.BreaksImmutableParam), not only p's (#245). It must also see every
// caller, so the function must be called only directly (see
// calledOnlyDirectly), and the pass must see every file of the package (see
// NewGenerator). Declaring any other function would hide the reuse from the
// callers it cannot see, or move it to a caller that the caller-side check
// reports, so -fix would not end clean. The variant of a package without its
// test files and the test variant both take the body Session for a function
// outside the test files, so their fixes agree. A closure is never declared,
// since directive placement on a literal is left to the user. It takes the
// body Session (#313).
//
//declscope:shared // generator.go's Generate chooses immutable-param with it
func (g *Generator) paramCallersIsolate(p *ssa.Parameter) bool {
	fn := origin.Declared(p.Parent())
	if _, ok := fn.Syntax().(*ast.FuncDecl); !ok {
		return false
	}
	if !g.seesAllCallers(fn.Pos()) || !g.calledOnlyDirectly(fn) {
		return false
	}
	for _, c := range g.callsOf(fn) {
		loopInfo := g.cfg.DetectLoops(c.caller)
		// Every argument, not only p's: the caller-side check asks the
		// same of each one (#245).
		for i := range c.call.Common().Args {
			if handler.BreaksImmutableParam(c.site, i, g.tracer, loopInfo) {
				return false
			}
		}
	}
	return true
}

// paramDiscardsChain reports whether a use among uses, the uses of a
// parameter root, is a discarded chain statement (see
// discardedChainToReassign), such as tx.Where("a"). The statement changes the
// caller's value in place, which //gormreuse:immutable-param would undo.
//
//declscope:shared // generator.go's Generate withholds immutable-param with it
func (g *Generator) paramDiscardsChain(uses []pollution.UsageInfo) bool {
	return slices.ContainsFunc(uses, func(use pollution.UsageInfo) bool {
		_, _, ok := g.discardedChainToReassign(use)
		return ok
	})
}

// generateImmutableParamFix suggests annotating the enclosing function with
// //gormreuse:immutable-param when a *gorm.DB PARAMETER is the reused root
// and every caller passes an isolated value (see paramCallersIsolate). The
// caller-side check (stage 2b) then keeps the callers isolated.
//
//declscope:shared // generator.go's Generate returns it
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
// including one through an alias, then starts from the Session. The function
// is a declaration or a closure (see canFixParam).
//
//declscope:shared // generator.go's Generate writes it with the other Sessions
func (g *Generator) paramSessionSite(p *ssa.Parameter) sessionSite {
	var body *ast.BlockStmt
	switch fn := p.Parent().Syntax().(type) {
	case *ast.FuncDecl:
		body = fn.Body
	case *ast.FuncLit:
		body = fn.Body
	}
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
