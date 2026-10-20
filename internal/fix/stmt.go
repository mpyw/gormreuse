package fix

import (
	"go/ast"
	"go/token"
	"slices"
	"sort"

	"github.com/mpyw/gormreuse/internal/effort"
)

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

// stmtIndex holds the statements of a file (see stmtNodeTypes) in preorder,
// with their ranges and the nearest statement that encloses each.
//
//declscope:shared // generator.go's Generator holds it for the pass
type stmtIndex struct {
	//declscope:private
	stmts []ast.Stmt
	//declscope:private
	pos []token.Pos
	//declscope:private
	end []token.Pos
	//declscope:private
	parent []int // index of the enclosing statement, or -1
	//declscope:private
	sorted bool // whether pos is in order, as it is for parsed syntax
}

// findStmtAtPos finds the innermost statement at the given position: the
// narrowest statement whose range holds pos, the outermost of equal ones.
//
// The statements of a file are indexed on first use. A walk of the file per
// use was quadratic in the uses, and cubic in an else-if ladder, since
// IfStmt.End recurses down the else chain (#377). The ranges of parsed
// syntax nest, so the statement is the last one to start at or before pos,
// or the first statement enclosing it that holds pos.
//
//declscope:shared // reassign.go finds the statement to reassign with it
func (g *Generator) findStmtAtPos(file *ast.File, pos token.Pos) ast.Stmt {
	x := g.stmtIndexOf(file)
	if !x.sorted {
		return x.scan(pos)
	}
	k := sort.Search(len(x.pos), func(i int) bool { return x.pos[i] > pos }) - 1
	for ; k >= 0; k = x.parent[k] {
		effort.Step()
		if pos < x.end[k] {
			break
		}
	}
	if k < 0 {
		return nil
	}
	// An enclosing statement of the same range comes first in preorder.
	for p := x.parent[k]; p >= 0 && x.end[p]-x.pos[p] == x.end[k]-x.pos[k]; p = x.parent[k] {
		k = p
	}
	return x.stmts[k]
}

// scan is findStmtAtPos by checking each statement, for syntax whose ranges
// are out of order.
func (x *stmtIndex) scan(pos token.Pos) ast.Stmt {
	result := -1
	for i := range x.stmts {
		effort.Step()
		if x.pos[i] <= pos && pos < x.end[i] && (result < 0 || x.end[i]-x.pos[i] < x.end[result]-x.pos[result]) {
			result = i
		}
	}
	if result < 0 {
		return nil
	}
	return x.stmts[result]
}

// stmtIndexOf returns the statement index of file, building it on first use.
func (g *Generator) stmtIndexOf(file *ast.File) *stmtIndex {
	if x, ok := g.stmtIndexes[file]; ok {
		return x
	}
	x := &stmtIndex{sorted: true}
	at := make(map[ast.Node]int)
	for c := range g.fileCursor(file).Preorder(stmtNodeTypes...) {
		effort.Step()
		stmt := c.Node().(ast.Stmt)
		parent := -1
		for p := c.Parent(); p.Node() != nil; p = p.Parent() {
			if i, ok := at[p.Node()]; ok {
				parent = i
				break
			}
		}
		at[stmt] = len(x.stmts)
		if n := len(x.pos); n > 0 && stmt.Pos() < x.pos[n-1] {
			x.sorted = false
		}
		x.stmts = append(x.stmts, stmt)
		x.pos = append(x.pos, stmt.Pos())
		x.parent = append(x.parent, parent)
	}
	// The ends are found last first, so an if statement takes its else
	// branch's end, found already, and the ladder is not walked once per if.
	x.end = make([]token.Pos, len(x.stmts))
	for i, stmt := range slices.Backward(x.stmts) {
		if s, ok := stmt.(*ast.IfStmt); ok && s.Else != nil {
			if j, ok := at[s.Else]; ok {
				x.end[i] = x.end[j]
				continue
			}
		}
		x.end[i] = stmt.End()
	}
	if g.stmtIndexes == nil {
		g.stmtIndexes = make(map[*ast.File]*stmtIndex)
	}
	g.stmtIndexes[file] = x
	return x
}
