package fix

import (
	"go/ast"
	"go/token"

	"golang.org/x/tools/go/ast/edge"
	"golang.org/x/tools/go/ast/inspector"
	"golang.org/x/tools/go/ssa"
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
