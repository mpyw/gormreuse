package directive

import (
	"go/ast"
	"go/token"
	"slices"

	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/go/ast/inspector"
)

// placement finds the function a directive comment is written on: the
// declaration whose doc comment or opening brace holds it, or the function
// literal on its line or the line below.
//
//declscope:shared // funcset.go embeds it in DirectiveFuncSet
type placement struct {
	// isDirective checks if a comment is this directive.
	isDirective directiveChecker

	//declscope:private
	fset *token.FileSet
	// files are the original parsed files, from the analysis, by name.
	//
	//declscope:private
	files map[string]*ast.File
	// cursors are the cursors of the files added with addFile, in the
	// shared pass inspector.
	//
	//declscope:private
	cursors map[*ast.File]inspector.Cursor
	// codeBeforeCommentCache caches hasCodeBeforeComment, to avoid
	// O(comments * nodes) complexity.
	//
	//declscope:private
	codeBeforeCommentCache map[*ast.File]map[token.Pos]bool
	// funcLitLinesCache caches the lines of each file that hold a function
	// literal, to avoid an O(nodes) lookup per line check.
	//
	//declscope:private
	funcLitLinesCache map[*ast.File]map[int]bool
	// funcLitDirectiveCache caches findDirectiveForFuncLit. A closure called
	// at n sites is asked n times, and each answer walks its statement (#304).
	//
	//declscope:private
	funcLitDirectiveCache map[*ast.FuncLit]token.Pos
}

// newPlacement is a placement of the directive isDirective tells, in the
// files of fset.
//
//declscope:shared // funcset.go makes one for each set
func newPlacement(fset *token.FileSet, isDirective directiveChecker) placement {
	return placement{
		fset:                   fset,
		files:                  make(map[string]*ast.File),
		isDirective:            isDirective,
		cursors:                make(map[*ast.File]inspector.Cursor),
		codeBeforeCommentCache: make(map[*ast.File]map[token.Pos]bool),
		funcLitLinesCache:      make(map[*ast.File]map[int]bool),
		funcLitDirectiveCache:  make(map[*ast.FuncLit]token.Pos),
	}
}

// addFile records the file cur is the cursor of, so that its directives are
// looked for. It reports false when there is nothing to record.
//
//declscope:shared // funcset.go adds the files of the pass
func (p *placement) addFile(cur inspector.Cursor) (*ast.File, bool) {
	if p.fset == nil || !cur.Valid() {
		return nil, false
	}
	file := cur.Node().(*ast.File)
	p.cursors[file] = cur
	filename := p.fset.PositionFor(file.Pos(), false).Filename
	if filename != "" {
		p.files[filename] = file
	}
	return file, true
}

// fileCursor returns the cursor in the pass inspector of a file added with
// AddFile. Directives are only looked for in those files.
//
//declscope:shared // funcset.go walks a file through its cursor
func (p *placement) fileCursor(file *ast.File) inspector.Cursor {
	return p.cursors[file]
}

// hasMatchingDirective finds a directive comment in the file that satisfies the given predicate.
//
// This is a common helper for directive detection. It:
//  1. Gets the file containing the node
//  2. Loops through all comments in the file
//  3. For each comment that has the directive, applies the predicate
//
// The predicate receives the file (for nested AST inspection) and the comment group.
//
// findMatchingDirective finds a directive comment matching the predicate and returns its position.
// Returns token.NoPos if no matching directive is found.
func (p *placement) findMatchingDirective(node ast.Node, predicate func(file *ast.File, cg *ast.CommentGroup) bool) token.Pos {
	file := p.getFileForNode(node)
	if file == nil {
		return token.NoPos
	}
	for _, cg := range file.Comments {
		if p.commentGroupHasDirective(cg) && predicate(file, cg) {
			// Return the position of the first directive comment in the group
			for _, c := range cg.List {
				if p.isDirective(c.Text) {
					return c.Pos()
				}
			}
		}
	}
	return token.NoPos
}

// hasDirectiveAfterFuncDeclBrace checks if a FuncDecl has a directive comment
// after its opening brace on the same line (same-line pattern).
//
// This is simpler than FuncLit handling because FuncDecl cannot be nested
// within another FuncDecl on the same line, so we don't need innermost detection.
//
// Example:
//
//	func foo(db *gorm.DB) { //gormreuse:pure
//	    ...
//	}
//
// Edge case handled:
// - External function bodies (funcDecl.Body == nil) return false
//
// findDirectiveAfterFuncDeclBrace finds a directive comment after the opening brace.
// Returns the position of the directive, or token.NoPos if not found.
//
//declscope:shared // immutable_input.go places its directives with the same rules
func (p *placement) findDirectiveAfterFuncDeclBrace(funcDecl *ast.FuncDecl) token.Pos {
	if p.fset == nil || funcDecl.Body == nil {
		return token.NoPos
	}
	bracePos := p.fset.PositionFor(funcDecl.Body.Lbrace, false)
	return p.findMatchingDirective(funcDecl, func(_ *ast.File, cg *ast.CommentGroup) bool {
		return p.isCommentAfterBrace(cg, bracePos)
	})
}

// isCommentAfterBrace checks if a comment is on the same line as a brace and appears after it.
//
// This is used for same-line directive detection:
//
//	func() { //comment  ← comment column > brace column, same line → true
//	//comment           ← different line → false
//	{ //comment         ← brace at column 1, comment at column 3 → true
func (p *placement) isCommentAfterBrace(cg *ast.CommentGroup, bracePos token.Position) bool {
	cgPos := p.fset.PositionFor(cg.Pos(), false)
	return cgPos.Line == bracePos.Line && cgPos.Column > bracePos.Column
}

// hasDirectiveForFuncLit checks if a FuncLit has a directive comment.
//
// Unlike FuncDecl which has Doc comments, FuncLit (closures) don't have attached
// documentation, so we need to search the file's comment list for directives.
//
// This supports two placement styles:
//
// # Next-line pattern
//
// Directive alone on its line (no code), applies to all direct FuncLits in the
// same assignment statement on the next line:
//
//	//gormreuse:pure
//	fn := func(q *gorm.DB) *gorm.DB { return q.Where("x") }
//
//	//gormreuse:pure
//	a, b := func(){}, func(){}  ← both get the directive (same statement)
//
//	//gormreuse:pure
//	a, b.Fn = func(){}, func(){}  ← both get the directive (direct assignment)
//
// Edge case: FuncLits inside composite literals (struct, slice, map) are NOT covered:
//
//	//gormreuse:pure
//	a, b := func(){}, &S{Fn: func(){}}  ← only 'a' gets the directive
//
// # Same-line pattern
//
// Directive after opening brace on same line, applies to that FuncLit:
//
//	fn := func(q *gorm.DB) *gorm.DB { //gormreuse:pure
//	    return q.Where("x")
//	}
//
// Edge case: For nested closures, applies to the innermost FuncLit whose { is
// immediately before the directive (not the outermost):
//
//	outer := func() { inner := func() { //gormreuse:pure  ← applies to inner, not outer
//	    ...
//	}}
//
// This "innermost" rule prevents ambiguity when closures are on the same line.
//
// findDirectiveForFuncLit finds a directive comment for a FuncLit and returns its position.
// Returns token.NoPos if no directive is found.
//
//declscope:shared // immutable_input.go places its directives with the same rules
func (p *placement) findDirectiveForFuncLit(funcLit *ast.FuncLit) token.Pos {
	if p.fset == nil {
		return token.NoPos
	}
	if pos, ok := p.funcLitDirectiveCache[funcLit]; ok {
		return pos
	}
	pos := p.findMatchingDirective(funcLit, func(file *ast.File, cg *ast.CommentGroup) bool {
		return p.matchesSameLineDirective(file, funcLit, cg) || p.matchesNextLineDirective(file, funcLit, cg)
	})
	p.funcLitDirectiveCache[funcLit] = pos
	return pos
}

// getFileForNode returns the file added with AddFile that contains the given
// node, or nil.
func (p *placement) getFileForNode(node ast.Node) *ast.File {
	pos := node.Pos()
	if !pos.IsValid() {
		return nil
	}
	return p.files[p.fset.PositionFor(pos, false).Filename]
}

// commentGroupHasDirective checks if a comment group contains our directive.
//
//declscope:shared // funcset.go tells a comment group that holds the directive
func (p *placement) commentGroupHasDirective(cg *ast.CommentGroup) bool {
	return slices.ContainsFunc(cg.List, func(c *ast.Comment) bool { return p.isDirective(c.Text) })
}

// matchesNextLineDirective checks if a directive comment applies to a FuncLit via
// the "next-line" pattern: directive ends on line N, statement starts on line N+1,
// and the FuncLit is a direct value in that statement.
//
// Requirements:
//  1. Directive must be the only code on its line (not after }, or other code)
//  2. Directive line(s) must have NO FuncLit on them (otherwise same-line applies)
//  3. The FuncLit must be a DIRECT value in an assignment statement (not inside CompositeLit)
//
// All direct FuncLits in the same assignment statement get the directive:
//
//	//gormreuse:pure
//	a, b := func(){}, func(){}  ← both get the directive
//
//	//gormreuse:pure
//	a, b.Fn = func(){}, func(){}  ← both get the directive (direct assignment)
//
// FuncLits inside composite literals do NOT get the directive:
//
//	//gormreuse:pure
//	a, b := func(){}, &S{Fn: func(){}}  ← only 'a' gets the directive
//
// Semicolon-separated statements are separate:
//
//	//gormreuse:pure
//	a := func(){}; b := func(){}  ← only 'a' gets the directive
//
// Directive after code does NOT apply to next line:
//
//	}, //gormreuse:pure
//	func(){}  ← does NOT get the directive (directive is not alone on its line)
func (p *placement) matchesNextLineDirective(file *ast.File, funcLit *ast.FuncLit, cg *ast.CommentGroup) bool {
	cgStartLine := p.fset.PositionFor(cg.Pos(), false).Line
	cgEndLine := p.fset.PositionFor(cg.End(), false).Line

	// Requirement 1: Directive must be alone on its line (no code before it)
	if p.hasCodeBeforeComment(file, cg) {
		return false
	}

	// Requirement 2: If there's any FuncLit on any line of the comment,
	// this is a same-line pattern, not next-line
	for line := cgStartLine; line <= cgEndLine; line++ {
		if p.hasFuncLitOnLine(file, line) {
			return false
		}
	}

	// Find the enclosing assignment statement and check requirements
	return p.isDirectValueInStatementAfterLine(file, funcLit, cgEndLine)
}

// matchesSameLineDirective checks if a directive comment applies to a FuncLit via
// the "same-line" pattern: directive is after the FuncLit's opening brace on the same line.
//
// Requirements:
//  1. Directive must be on the same line as FuncLit's opening brace
//  2. Directive must be AFTER the brace (column > brace column)
//  3. No nested FuncLit brace between this FuncLit's brace and the directive
//
// The "innermost" rule (requirement 3) is critical for nested closures:
//
//	outer := func() { inner := func() { //gormreuse:pure
//	                  ^                 ^
//	                  outer's {         inner's { (closer to comment)
//
// The directive applies to 'inner' because its { is between outer's { and the comment.
//
// Example (matches inner, not outer):
//
//	outer := func() { inner := func() { //gormreuse:pure  ← inner matches
//
// Example (matches outer - no nested brace):
//
//	outer := func() { //gormreuse:pure
//	    inner := func() {}  ← inner is on different line
//	}
func (p *placement) matchesSameLineDirective(file *ast.File, funcLit *ast.FuncLit, cg *ast.CommentGroup) bool {
	if funcLit.Body == nil {
		return false
	}

	bracePos := p.fset.PositionFor(funcLit.Body.Lbrace, false)

	// Requirements 1 & 2: Comment must be after brace on same line
	if !p.isCommentAfterBrace(cg, bracePos) {
		return false
	}

	// Requirement 3: Check there's no nested FuncLit brace between this { and the comment
	cgColumn := p.fset.PositionFor(cg.Pos(), false).Column
	return !p.hasNestedFuncLitBetween(funcLit, bracePos.Column, cgColumn, bracePos.Line)
}

// isDirectValueInStatementAfterLine checks if the FuncLit is a direct RHS value
// in an assignment statement that relates to directiveLine+1.
//
// This handles both:
//   - Statement starts on next line: //gormreuse:pure
//     a, b := func(){}, func(){}  ← all direct FuncLits get directive
//   - FuncLit starts on next line (multi-line): a, b := func(){},
//     //gormreuse:pure
//     func(){}  ← this FuncLit gets directive
//
// "Direct value" means not nested inside a composite literal (struct, slice, map).
//
// For semicolon-separated statements on the same line, only the FIRST statement applies:
//
//	a := func(){}; b := func(){}  ← only 'a' gets the directive
func (p *placement) isDirectValueInStatementAfterLine(file *ast.File, funcLit *ast.FuncLit, directiveLine int) bool {
	expectedLine := directiveLine + 1

	// Find the path to the FuncLit using astutil
	path, _ := astutil.PathEnclosingInterval(file, funcLit.Pos(), funcLit.End())
	if path == nil {
		return false
	}

	// Walk up the path to find the enclosing statement
	var enclosingStmt ast.Stmt
	for _, node := range path {
		if stmt, ok := node.(ast.Stmt); ok {
			enclosingStmt = stmt
			break
		}
	}

	if enclosingStmt == nil {
		// No enclosing statement: this is a package-level `var f = func(){}`
		// declaration (a GenDecl/ValueSpec is a Decl, not a Stmt). Local-var
		// closures reach here via a DeclStmt and are handled above; package-level
		// ones need the ValueSpec handled directly (issue #72 gap2).
		return p.isDirectValueInValueSpecAfterLine(path, funcLit, directiveLine)
	}

	stmtLine := p.fset.PositionFor(enclosingStmt.Pos(), false).Line
	funcLitLine := p.fset.PositionFor(funcLit.Pos(), false).Line

	// Case 1: Statement starts on expected line → all direct FuncLits in that statement
	// Case 2: FuncLit starts on expected line → this specific FuncLit (multi-line assignment)
	if stmtLine != expectedLine && funcLitLine != expectedLine {
		return false
	}

	// For semicolon-separated statements, only the first statement applies
	// Check based on which line is the expected line
	checkLine := stmtLine
	if stmtLine != expectedLine {
		checkLine = funcLitLine
	}
	if !p.isFirstStatementOnLine(file, enclosingStmt, checkLine) {
		return false
	}

	// Check if FuncLit is a direct value (not inside CompositeLit)
	return p.isDirectRHSValue(enclosingStmt, funcLit)
}

// isDirectValueInValueSpecAfterLine handles the package-level counterpart of
// isDirectValueInStatementAfterLine: a closure that is a direct RHS value of a
// package-level `var` declaration whose directive sits on the preceding line.
//
//	//gormreuse:pure
//	var pkgFn = func(q *gorm.DB) { ... }   ← pkgFn gets the directive
//
// As with the statement form, the closure must be a DIRECT value in the spec
// (not nested inside a composite literal), and either the spec or the closure
// must begin on directiveLine+1. All direct closures in a multi-value spec
// (`var a, b = func(){}, func(){}`) get the directive.
func (p *placement) isDirectValueInValueSpecAfterLine(path []ast.Node, funcLit *ast.FuncLit, directiveLine int) bool {
	expectedLine := directiveLine + 1

	var spec *ast.ValueSpec
	for _, node := range path {
		if vs, ok := node.(*ast.ValueSpec); ok {
			spec = vs
			break
		}
		// A composite literal between the FuncLit and the spec means the closure
		// is not a direct value (e.g. `var x = &S{Fn: func(){}}`).
		if _, ok := node.(*ast.CompositeLit); ok {
			return false
		}
	}
	if spec == nil {
		return false
	}

	specLine := p.fset.PositionFor(spec.Pos(), false).Line
	funcLitLine := p.fset.PositionFor(funcLit.Pos(), false).Line
	if specLine != expectedLine && funcLitLine != expectedLine {
		return false
	}

	// The closure must be a direct value of the spec.
	return slices.Contains(spec.Values, ast.Expr(funcLit))
}

// isFirstStatementOnLine checks if the given statement is the first (leftmost) one on its line.
// This is important for semicolon-separated statements: `a := f1(); b := f2()`
func (p *placement) isFirstStatementOnLine(file *ast.File, target ast.Stmt, line int) bool {
	targetColumn := p.fset.PositionFor(target.Pos(), false).Column
	isFirst := true

	// Check all function bodies in the file for statements on this line
	p.fileCursor(file).Inspect(nil, func(c inspector.Cursor) bool {
		n := c.Node()
		if !isFirst {
			return false
		}
		if stmt, ok := n.(ast.Stmt); ok && stmt != target {
			stmtPos := p.fset.PositionFor(stmt.Pos(), false)
			if stmtPos.Line == line && stmtPos.Column < targetColumn {
				// Found a statement earlier on the same line
				isFirst = false
				return false
			}
		}
		return true
	})
	return isFirst
}

// isDirectRHSValue checks if the FuncLit is a direct RHS value in the statement,
// not nested inside a composite literal (struct, slice, map, array).
//
// The target is matched by position, not by identity.
func (p *placement) isDirectRHSValue(stmt ast.Stmt, target *ast.FuncLit) bool {
	var rhsExprs []ast.Expr

	switch st := stmt.(type) {
	case *ast.AssignStmt:
		rhsExprs = st.Rhs
	case *ast.DeclStmt:
		if gd, ok := st.Decl.(*ast.GenDecl); ok {
			for _, spec := range gd.Specs {
				if vs, ok := spec.(*ast.ValueSpec); ok {
					rhsExprs = append(rhsExprs, vs.Values...)
				}
			}
		}
	default:
		return false
	}

	targetPos := target.Pos()
	targetEnd := target.End()

	// Check if target is one of the direct RHS expressions
	return slices.ContainsFunc(rhsExprs, func(rhs ast.Expr) bool {
		return p.isDirectExprOrUnaryByPos(rhs, targetPos, targetEnd)
	})
}

// isDirectExprOrUnaryByPos checks if expr contains a FuncLit at the given position,
// and that FuncLit is a direct expression or inside unary/paren (not inside CompositeLit).
func (p *placement) isDirectExprOrUnaryByPos(expr ast.Expr, targetPos, targetEnd token.Pos) bool {
	switch e := expr.(type) {
	case *ast.FuncLit:
		// Use position matching instead of identity
		return e.Pos() == targetPos && e.End() == targetEnd
	case *ast.UnaryExpr:
		// Handle &func(){} case
		return p.isDirectExprOrUnaryByPos(e.X, targetPos, targetEnd)
	case *ast.ParenExpr:
		// Handle (func(){}) case
		return p.isDirectExprOrUnaryByPos(e.X, targetPos, targetEnd)
	default:
		// CompositeLit, CallExpr, IndexExpr, etc. - target is nested, not direct
		return false
	}
}

// hasFuncLitOnLine checks if there's any FuncLit that starts on the given line.
// Results are cached per file to avoid O(nodes) traversal per line check.
func (p *placement) hasFuncLitOnLine(file *ast.File, line int) bool {
	// Build cache if not exists
	if _, ok := p.funcLitLinesCache[file]; !ok {
		p.buildFuncLitLinesCache(file)
	}
	return p.funcLitLinesCache[file][line]
}

// buildFuncLitLinesCache pre-computes all FuncLit line numbers for a file.
func (p *placement) buildFuncLitLinesCache(file *ast.File) {
	lines := make(map[int]bool)
	for c := range p.fileCursor(file).Preorder(funcLitTypes...) {
		fl := c.Node().(*ast.FuncLit)
		lines[p.fset.PositionFor(fl.Pos(), false).Line] = true
	}
	p.funcLitLinesCache[file] = lines
}

// hasCodeBeforeComment checks if there's any code (non-whitespace) before the comment on the same line.
// This is used to determine if a directive is "alone" on its line or follows other code like "}, //gormreuse:pure".
// Results are cached per file to avoid O(comments * nodes) complexity.
func (p *placement) hasCodeBeforeComment(file *ast.File, cg *ast.CommentGroup) bool {
	// Check cache first
	if fileCache, ok := p.codeBeforeCommentCache[file]; ok {
		if result, ok := fileCache[cg.Pos()]; ok {
			return result
		}
	}

	// Initialize cache for this file if not exists
	if p.codeBeforeCommentCache[file] == nil {
		p.codeBeforeCommentCache[file] = make(map[token.Pos]bool)
	}

	// Compute result
	result := p.computeCodeBeforeComment(file, cg)
	p.codeBeforeCommentCache[file][cg.Pos()] = result
	return result
}

// computeCodeBeforeComment does the actual computation for hasCodeBeforeComment.
func (p *placement) computeCodeBeforeComment(file *ast.File, cg *ast.CommentGroup) bool {
	cgPos := p.fset.PositionFor(cg.Pos(), false)
	cgLine := cgPos.Line
	cgColumn := cgPos.Column

	// Check if any AST node ends on the same line before the comment
	hasCode := false
	p.fileCursor(file).Inspect(nil, func(c inspector.Cursor) bool {
		n := c.Node()
		if hasCode {
			return false
		}
		if n == nil || n == cg {
			return true
		}
		// Skip comment groups and comments
		switch n.(type) {
		case *ast.CommentGroup, *ast.Comment:
			return true
		}

		// Early termination optimization: skip nodes that can't be on the target line
		nodeStart := p.fset.PositionFor(n.Pos(), false)
		nodeEnd := p.fset.PositionFor(n.End(), false)

		// If node ends before the comment's line, skip it and its children
		if nodeEnd.Line < cgLine {
			return false
		}
		// If node starts after the comment's line, skip it
		if nodeStart.Line > cgLine {
			return false
		}

		// Node ends on the same line, before the comment
		if nodeEnd.Line == cgLine && nodeEnd.Column < cgColumn {
			hasCode = true
			return false
		}
		return true
	})
	return hasCode
}

// hasNestedFuncLitBetween checks if there's a nested FuncLit whose opening brace
// is between startColumn and endColumn on the given line.
// This is used for same-line pattern to ensure the directive applies to the innermost FuncLit.
func (p *placement) hasNestedFuncLitBetween(parent *ast.FuncLit, startColumn, endColumn, line int) bool {
	found := false
	ast.Inspect(parent.Body, func(n ast.Node) bool {
		if found {
			return false
		}
		if nested, ok := n.(*ast.FuncLit); ok && nested != parent {
			if nested.Body != nil {
				nestedPos := p.fset.PositionFor(nested.Body.Lbrace, false)
				if nestedPos.Line == line && nestedPos.Column > startColumn && nestedPos.Column < endColumn {
					found = true
					return false
				}
			}
		}
		return true
	})
	return found
}
