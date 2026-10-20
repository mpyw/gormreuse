package fix

import (
	"go/ast"
	"go/token"

	"golang.org/x/tools/go/ast/edge"
	"golang.org/x/tools/go/ast/inspector"
	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/ssa/origin"
	"github.com/mpyw/gormreuse/internal/typeutil"
)

// callExprNodeTypes contains CallExpr for inspector filtering.
//
//declscope:shared // virtual.go's virtualRootEndsImmutable walks the calls of a file with it
var callExprNodeTypes = []ast.Node{(*ast.CallExpr)(nil)}

// findCallAtLparen returns the call under cur whose Lparen is pos, which is
// where SSA places a call and so where a use is recorded.
//
//declscope:shared // generator.go, reassign.go and session.go find a use's call with it
func findCallAtLparen(cur inspector.Cursor, pos token.Pos) *ast.CallExpr {
	for c := range cur.Preorder(callExprNodeTypes...) {
		if call := c.Node().(*ast.CallExpr); call.Lparen == pos {
			return call
		}
	}
	return nil
}

// calledOnlyDirectly reports whether every caller of fn is a call in this
// pass that names it: fn is an unexported function, not a method, and each
// reference to it is the function of a call. An exported function can be
// called from another package, which the caller-side check does not see
// (#195). A method can be called through an interface, and a function value
// can be called anywhere.
//
//declscope:shared // param.go's canFixParam and paramCallersIsolate ask it
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
	for _, caller := range g.funcs {
		for _, block := range caller.Blocks {
			for _, instr := range block.Instrs {
				call, ok := instr.(ssa.CallInstruction)
				if !ok || origin.Declared(g.tracer.Site(call).Callee) != fn {
					continue
				}
				if result := call.Value(); result == nil || !callResultUsed(result) {
					return false
				}
			}
		}
	}
	return true
}

// callResultUsed reports whether code reads the *gorm.DB that v, the result
// of a call, holds: v itself, or, for a tuple, an element of type *gorm.DB.
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
