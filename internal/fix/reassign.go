package fix

import (
	"go/ast"
	"go/token"
	"go/types"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/ssa/pollution"
	"github.com/mpyw/gormreuse/internal/typeutil"
)

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
//
//declscope:shared // generator.go's Generate picks the variable to rebind with it
func (g *Generator) findReassignTarget(root ssa.Value, uses []pollution.UsageInfo) *types.Var {
	v := g.varToReassign(root)
	if v == nil || v.Parent() == v.Pkg().Scope() {
		return nil
	}
	// v is local, so its declaration and every use of the root are inside one
	// function of this file.
	file := g.findFileContaining(v.Pos())
	if g.capturedByClosure(file, v) {
		return nil
	}
	for _, use := range uses {
		call := g.callAtLparen(file, use.Pos)
		if call == nil || !g.mentionsReassignTarget(call, v) {
			return nil
		}
	}
	return v
}

// varToReassign returns the variable that a reassignment of root rebinds: for
// a parameter, its own variable (#201). For a call it returns the variable the
// call is assigned to by `v := <call>`, `v = <call>` or `var v = <call>`, or
// nil. SSA places a call at its Lparen. A root that is not a call, such as a
// Phi, gets nil, so the fix falls back to a Session. The tracer usually
// resolves a Phi to its edges; a loop variable swap keeps the Phi as the root.
func (g *Generator) varToReassign(root ssa.Value) *types.Var {
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
	id, ok := ast.Unparen(g.reassignLHS(file)[pos]).(*ast.Ident)
	if !ok {
		return nil
	}
	v, _ := g.pass.TypesInfo.ObjectOf(id).(*types.Var)
	return v
}

// reassignLHS maps the Lparen of each call that `v := <call>`, `v = <call>`
// or `var v = <call>` assigns in file to its left-hand side, built on first
// use, so the variable of each of n roots is found without walking the file n
// times (#350).
func (g *Generator) reassignLHS(file *ast.File) map[token.Pos]ast.Expr {
	if assigned, ok := g.assigned[file]; ok {
		return assigned
	}
	assigned := make(map[token.Pos]ast.Expr)
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
			if call, ok := ast.Unparen(e).(*ast.CallExpr); ok {
				if _, seen := assigned[call.Lparen]; !seen {
					assigned[call.Lparen] = lhs[i]
				}
			}
		}
	}
	if g.assigned == nil {
		g.assigned = make(map[*ast.File]map[token.Pos]ast.Expr)
	}
	g.assigned[file] = assigned
	return assigned
}

// mentionsReassignTarget reports whether an identifier in n refers to v, a
// candidate reassignment target.
func (g *Generator) mentionsReassignTarget(n ast.Node, v *types.Var) bool {
	found := false
	ast.Inspect(n, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && g.pass.TypesInfo.Uses[id] == v {
			found = true
		}
		return !found
	})
	return found
}

// findReassignedChains returns the discarded chains (see
// discardedChainToReassign) among uses, which the fix reassigns to target, as
// a map from the position of the use to the end of its chain. Several uses can
// share a position, such as a method call's receiver that is also recorded as
// an argument; the map classifies them all.
//
// A discarded chain such as `q.Where("a").Limit(1)` changes the root in
// place, and nothing else keeps that change. Once the root gets a Session,
// the chain changes a copy instead, and the fixed code silently drops its
// conditions. So when a chain cannot be reassigned, ok is false and no fix is
// offered (#199).
//
//declscope:shared // generator.go's Generate finds the statements to reassign with it
func (g *Generator) findReassignedChains(uses []pollution.UsageInfo, target *types.Var) (chains map[token.Pos]token.Pos, ok bool) {
	chains = make(map[token.Pos]token.Pos)
	for _, use := range uses {
		stmt, recv, isChain := g.discardedChainToReassign(use)
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
	return target != nil && use.Call == use.Pos && recv != nil && g.refersToReassignTarget(recv, target)
}

// refersToReassignTarget reports whether expr is an identifier that refers to
// v, the reassignment target.
func (g *Generator) refersToReassignTarget(expr ast.Expr, v *types.Var) bool {
	id, ok := ast.Unparen(expr).(*ast.Ident)
	return ok && g.pass.TypesInfo.Uses[id] == v
}

// discardedChainToReassign reports whether use is the first call of a chain of
// *gorm.DB non-finisher calls whose result an expression statement discards,
// such as q.Where("a") or q.Where("a").Limit(1). Such a statement has an
// effect only by changing the receiver's Statement in place. It returns the
// statement and the receiver of the use's call. The receiver is nil when the
// statement cannot be reassigned as a whole: for a call through a method
// value, and for a chain with a Session-family call before its last call, such
// as q.Where("a").Session(s).Limit(1). Only q.Where("a") changes q there, so
// reassigning the statement would add Limit(1) to q.
//
// A statement that ends in a finisher runs its own query, and one whose
// top-level call is not a gorm method, such as
// require.NoError(t, tx.Create(...).Error), is not a chain. Nor is one whose
// first call is a Session-family call: it never changes the receiver.
//
//declscope:shared // param.go withholds immutable-param from a parameter with such a chain
func (g *Generator) discardedChainToReassign(use pollution.UsageInfo) (*ast.ExprStmt, ast.Expr, bool) {
	if use.Method == "" || typeutil.IsFinisher(use.Method) || typeutil.IsImmutableReturningBuiltin(use.Method) {
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
		if !isMethod || typeutil.IsFinisher(sel.Sel.Name) || !typeutil.IsGormDB(g.pass.TypesInfo.TypeOf(sel.X)) {
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

// generateReassignmentEdit generates a TextEdit for reassignment.
// Inserts "q = " before the statement, where q is target.
//
//declscope:shared // generator.go's Generate writes each reassignment with it
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
