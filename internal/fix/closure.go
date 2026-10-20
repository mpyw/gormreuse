package fix

import (
	"go/ast"
	"go/token"
	"go/types"
	"slices"

	"golang.org/x/tools/go/ast/inspector"
	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/ssa/pollution"
)

// reassignsInClosure reports whether one of reassigned is in a closure of the
// function that defines root, such as `func() { q.Where("a") }()` or the body
// of a range-over-func loop. The reassignment writes the captured variable,
// and detection does not follow a store in a closure (see
// closureModifiesCaptured): the uses after it still read root. So root gets a
// Session too, or the next pass would add it (#247).
//
//declscope:shared // generator.go's Generate gives the root a Session with it
func reassignsInClosure(root ssa.Value, uses []pollution.UsageInfo, reassigned map[token.Pos]token.Pos) bool {
	return slices.ContainsFunc(uses, func(use pollution.UsageInfo) bool {
		_, ok := reassigned[use.Pos]
		return ok && use.Block.Parent() != root.Parent()
	})
}

// withClosures returns fn and the closures it declares, at any depth.
//
//declscope:shared // change.go's changedElsewhere and holder.go's holderChangesRoot scan each of them
func withClosures(fn *ssa.Function) []*ssa.Function {
	fns := []*ssa.Function{fn}
	for i := 0; i < len(fns); i++ {
		fns = append(fns, fns[i].AnonFuncs...)
	}
	return fns
}

// capturedByClosure reports whether a closure inside the function that
// declares v refers to v. An immediately invoked closure `func() { ... }()`
// runs in program order, so it does not count; one run by a go or defer
// statement does.
//
//declscope:shared // reassign.go's findReassignTarget rejects a captured variable
func (g *Generator) capturedByClosure(file *ast.File, v *types.Var) bool {
	cur, _ := g.fileCursor(file).FindByPos(v.Pos(), v.Pos())
	declFn := enclosingFuncOrClosure(cur)
	for c := range declFn.Preorder((*ast.Ident)(nil)) {
		if g.pass.TypesInfo.Uses[c.Node().(*ast.Ident)] != v {
			continue
		}
		for fn := enclosingFuncOrClosure(c); fn != declFn; fn = enclosingFuncOrClosure(fn.Parent()) {
			if !isInlineCall(fn) {
				return true
			}
		}
	}
	return false
}

// enclosingFuncOrClosure returns the innermost function declaration or literal
// that encloses cur, or cur itself when it is one.
func enclosingFuncOrClosure(cur inspector.Cursor) (fn inspector.Cursor) {
	for fn = range cur.Enclosing((*ast.FuncDecl)(nil), (*ast.FuncLit)(nil)) {
		break
	}
	return fn
}
