package fix

import (
	"go/ast"
	"go/token"
	"go/types"

	"golang.org/x/tools/go/ast/edge"
	"golang.org/x/tools/go/ast/inspector"
	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/effort"
	"github.com/mpyw/gormreuse/internal/ssa/callsite"
	"github.com/mpyw/gormreuse/internal/ssa/origin"
	"github.com/mpyw/gormreuse/internal/typeutil"
)

// callExprNodeTypes contains CallExpr for inspector filtering.
var callExprNodeTypes = []ast.Node{(*ast.CallExpr)(nil)}

// fileCalls indexes the calls of a file.
//
//declscope:shared // generator.go's Generator holds it for the pass
type fileCalls struct {
	//declscope:private
	byLparen map[token.Pos]*ast.CallExpr // the last call in preorder with that Lparen
	//declscope:private
	byEnd map[token.Pos]token.Pos // the Lparen of the first call in preorder with that End
}

// callAtLparen returns the call in file whose Lparen is pos, which is where
// SSA places a call and so where a use is recorded. The calls of a file are
// indexed by Lparen on first use, so finding the call of each of n uses does
// not walk the file n times (#350).
//
//declscope:shared // generator.go, reassign.go and session.go find a use's call with it
func (g *Generator) callAtLparen(file *ast.File, pos token.Pos) *ast.CallExpr {
	return g.fileCallsOf(file).byLparen[pos]
}

// callEndingAt returns the Lparen of the first call in file that ends at end,
// or token.NoPos.
//
//declscope:shared // virtual.go's virtualRootEndsImmutable finds the end of a reassigned chain with it
func (g *Generator) callEndingAt(file *ast.File, end token.Pos) token.Pos {
	return g.fileCallsOf(file).byEnd[end]
}

// fileCallsOf returns the call index of file, building it on first use.
func (g *Generator) fileCallsOf(file *ast.File) fileCalls {
	calls, ok := g.calls[file]
	if !ok {
		calls = fileCalls{
			byLparen: make(map[token.Pos]*ast.CallExpr),
			byEnd:    make(map[token.Pos]token.Pos),
		}
		for c := range g.fileCursor(file).Preorder(callExprNodeTypes...) {
			effort.Step()
			call := c.Node().(*ast.CallExpr)
			calls.byLparen[call.Lparen] = call
			if _, seen := calls.byEnd[call.End()]; !seen {
				calls.byEnd[call.End()] = call.Lparen
			}
		}
		if g.calls == nil {
			g.calls = make(map[*ast.File]fileCalls)
		}
		g.calls[file] = calls
	}
	return calls
}

// calledOnlyDirectly reports whether every caller of fn is a call in this
// pass that names it: fn is an unexported function, not a method, and each
// reference to it is the function of a call. An exported function can be
// called from another package, which the caller-side check does not see
// (#195). A method can be called through an interface, and a function value
// can be called anywhere. The functions referred to other than by a call are
// found in one walk of the package, not one per function asked about (#377).
//
//declscope:shared // param.go's canFixParam and paramCallersIsolate ask it
func (g *Generator) calledOnlyDirectly(fn *ssa.Function) bool {
	obj := fn.Object()
	if obj.Exported() || fn.Signature.Recv() != nil {
		return false
	}
	if g.uncalledFuncs == nil {
		g.uncalledFuncs = make(map[types.Object]bool)
		for _, file := range g.pass.Files {
			for c := range g.fileCursor(file).Preorder((*ast.Ident)(nil)) {
				effort.Step()
				if used, ok := g.pass.TypesInfo.Uses[c.Node().(*ast.Ident)].(*types.Func); ok && !isCalledFunc(c) {
					g.uncalledFuncs[used] = true
				}
			}
		}
	}
	return !g.uncalledFuncs[obj]
}

// calleeCall is a call in the analysed functions, with its site.
//
//declscope:shared // generator.go's Generator holds them for the pass, and param.go checks their arguments
type calleeCall struct {
	caller *ssa.Function
	call   ssa.CallInstruction
	site   callsite.Site
}

// callsOf returns the calls of fn, a declared function, in the analysed
// functions, in the order of the functions and of their instructions. The
// calls of every function are indexed by callee on first use, so the checks
// of n parameters do not scan every instruction n times (#377).
//
//declscope:shared // param.go's paramCallersIsolate checks each call with it
func (g *Generator) callsOf(fn *ssa.Function) []calleeCall {
	if g.callees == nil {
		g.callees = make(map[*ssa.Function][]calleeCall)
		for _, caller := range g.funcs {
			for _, block := range caller.Blocks {
				for _, instr := range block.Instrs {
					effort.Step()
					call, ok := instr.(ssa.CallInstruction)
					if !ok {
						continue
					}
					site := g.tracer.Site(call)
					if callee := origin.Declared(site.Callee); callee != nil {
						g.callees[callee] = append(g.callees[callee], calleeCall{caller, call, site})
					}
				}
			}
		}
	}
	return g.callees[fn]
}

// everyCallUsesResult reports whether each call of fn in the analysed
// functions uses the *gorm.DB that fn returns (#312). A call whose result is
// discarded, as in `applyTenant(q, out)`, relies on fn changing its argument
// in place, so a fix must not move that change to fn's result. A go or
// defer statement discards the result too. fn must be called only directly
// (see calledOnlyDirectly), and the pass must see every caller (see
// NewGenerator), so no call is missed.
//
//declscope:shared // generator.go's Generate reassigns a parameter's discarded statements only when it holds
func (g *Generator) everyCallUsesResult(fn *ssa.Function) bool {
	if !g.seesAllCallers(fn.Pos()) {
		return false
	}
	for _, c := range g.callsOf(fn) {
		if result := c.call.Value(); result == nil || !callResultUsed(result) {
			return false
		}
	}
	return true
}

// callResultUsed reports whether code reads the *gorm.DB that v, the result
// of a call, holds: v itself, or, for a tuple, an element of type *gorm.DB.
//
//declscope:shared // change.go's changeRefPassesError asks whether a call's result may carry an error on
func callResultUsed(v ssa.Value) bool {
	for _, ref := range *v.Referrers() {
		switch ref := ref.(type) {
		case *ssa.DebugRef:
		case *ssa.Extract:
			if typeutil.IsGormDB(ref.Type()) && callResultUsed(ref) {
				return true
			}
		default:
			return true
		}
	}
	return false
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

// isInlineCall reports whether lit, a function literal, is called where it is
// written, and not by a go or defer statement.
//
//declscope:shared // closure.go's capturedByClosure skips closures called inline
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
