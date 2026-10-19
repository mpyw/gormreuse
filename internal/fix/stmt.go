package fix

import (
	"go/ast"
	"go/token"
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

// findStmtAtPos finds the innermost statement at the given position.
//
//declscope:shared // reassign.go finds the statement to reassign with it
func (g *Generator) findStmtAtPos(file *ast.File, pos token.Pos) ast.Stmt {
	var result ast.Stmt
	for c := range g.fileCursor(file).Preorder(stmtNodeTypes...) {
		stmt := c.Node().(ast.Stmt)
		// Check if this statement contains the position
		if stmt.Pos() <= pos && pos < stmt.End() {
			// Keep the innermost (narrowest) statement
			if result == nil || stmtWidth(stmt) < stmtWidth(result) {
				result = stmt
			}
		}
	}
	return result
}

// stmtWidth returns the width (end - start) of a statement's position range.
func stmtWidth(n ast.Node) token.Pos {
	return n.End() - n.Pos()
}
