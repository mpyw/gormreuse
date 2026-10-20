package directive

import (
	"go/ast"
	"go/token"
	"slices"

	"golang.org/x/tools/go/ast/inspector"
)

// placement finds the function a directive comment is written on: the
// declaration whose doc comment or opening brace holds it, or the function
// literal on its line or the line below.
//
// addFile places every directive comment of a file once, so a lookup is a
// map read. Before #301 each lookup scanned every comment of the file, and a
// closure's walked its statement and the whole file again, so n functions
// with directives took O(n²).
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
	// indexes are the directive comments of each file added with addFile,
	// and the functions they are written on.
	//
	//declscope:private
	indexes map[*ast.File]*placementIndex
}

// placementIndex is the directive comments of one file and the functions
// they are written on.
type placementIndex struct {
	// groups are the comment groups that hold the directive, in file order.
	groups []placementGroup
	// byStartLine and byEndLine index groups by the line a group starts and
	// ends on.
	byStartLine, byEndLine map[int][]int
	// lits is the directive comment of each closure that has one.
	lits map[*ast.FuncLit]token.Pos
	// litBraces are the closures whose opening brace is on each line, for a
	// file with a directive (see placementWalk).
	litBraces map[int][]placementLitBrace
}

// placementGroup is a comment group that holds the directive.
type placementGroup struct {
	// directives are its comments that are the directive, in order.
	directives []*ast.Comment
	// startLine and startColumn are where the group starts, and endLine is
	// the line it ends on.
	startLine, startColumn, endLine int
}

// newPlacement is a placement of the directive isDirective tells, in the
// files of fset.
//
//declscope:shared // funcset.go makes one for each set
func newPlacement(fset *token.FileSet, isDirective directiveChecker) placement {
	return placement{
		fset:        fset,
		files:       make(map[string]*ast.File),
		isDirective: isDirective,
		cursors:     make(map[*ast.File]inspector.Cursor),
		indexes:     make(map[*ast.File]*placementIndex),
	}
}

// addFile records the file cur is the cursor of, so that its directives are
// looked for, and places its directive comments. It reports false when there
// is nothing to record.
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
	p.indexes[file] = p.buildIndex(file)
	return file, true
}

// fileCursor returns the cursor in the pass inspector of a file added with
// AddFile. Directives are only looked for in those files.
//
//declscope:shared // funcset.go walks a file through its cursor
func (p *placement) fileCursor(file *ast.File) inspector.Cursor {
	return p.cursors[file]
}

// directivesIn returns the directive comments of a file added with addFile,
// in file order.
//
//declscope:shared // funcset.go reports the ones attached to no function
func (p *placement) directivesIn(file *ast.File) []*ast.Comment {
	var out []*ast.Comment
	if idx := p.indexes[file]; idx != nil {
		for _, g := range idx.groups {
			out = append(out, g.directives...)
		}
	}
	return out
}

// fileAdded reports whether node is in a file added with addFile, the only
// files whose directives are looked for.
//
//declscope:shared // funcset.go trusts no directive in another file (#315)
func (p *placement) fileAdded(node ast.Node) bool {
	return p.fset != nil && p.indexFor(node) != nil
}

// indexFor returns the index of the file added with addFile that contains
// node, or nil. A node without a position has no file name, and addFile
// records no file under an empty one.
func (p *placement) indexFor(node ast.Node) *placementIndex {
	file := p.files[p.fset.PositionFor(node.Pos(), false).Filename]
	if file == nil {
		return nil
	}
	return p.indexes[file]
}

// buildIndex places the directive comments of file. A file without one is
// not walked.
func (p *placement) buildIndex(file *ast.File) *placementIndex {
	idx := &placementIndex{
		byStartLine: make(map[int][]int),
		byEndLine:   make(map[int][]int),
		lits:        make(map[*ast.FuncLit]token.Pos),
	}
	for _, cg := range file.Comments {
		var directives []*ast.Comment
		for _, c := range cg.List {
			if p.isDirective(c.Text) {
				directives = append(directives, c)
			}
		}
		if directives == nil {
			continue
		}
		start := p.fset.PositionFor(cg.Pos(), false)
		g := placementGroup{
			directives:  directives,
			startLine:   start.Line,
			startColumn: start.Column,
			endLine:     p.fset.PositionFor(cg.End(), false).Line,
		}
		idx.byStartLine[g.startLine] = append(idx.byStartLine[g.startLine], len(idx.groups))
		idx.byEndLine[g.endLine] = append(idx.byEndLine[g.endLine], len(idx.groups))
		idx.groups = append(idx.groups, g)
	}
	if len(idx.groups) == 0 {
		return idx
	}
	w := p.walkFile(file)
	idx.litBraces = w.litBraces
	for _, lit := range w.lits {
		if pos := p.placeFuncLit(idx, w, lit); pos.IsValid() {
			idx.lits[lit.lit] = pos
		}
	}
	return idx
}

// placementWalk is what placing closure directives needs from one walk of a
// file.
type placementWalk struct {
	// codeEndColumns is the smallest column at which a syntax node ends on
	// each line. A comment that starts after it has code before it.
	codeEndColumns map[int]int
	// stmtColumns is the smallest column at which a statement starts on each
	// line.
	stmtColumns map[int]int
	// litLines are the lines a closure starts on.
	litLines map[int]bool
	// litBraces are the closures whose opening brace is on each line.
	litBraces map[int][]placementLitBrace
	// lits are the closures of the file, in source order.
	lits []placementLit
}

// placementLitBrace is a closure and the column of its opening brace.
type placementLitBrace struct {
	lit    *ast.FuncLit
	column int
}

// placementLit is a closure and the syntax around it that decides whether a
// directive on the line above applies.
type placementLit struct {
	lit *ast.FuncLit
	// stmt is the innermost statement that holds the closure, or nil for a
	// closure outside every function body.
	stmt ast.Stmt
	// spec is the innermost value spec that holds the closure when stmt is
	// nil, or nil when there is none or a composite literal is between the
	// two.
	spec *ast.ValueSpec
}

// placementFrame is what a node's descendants inherit from it and its
// ancestors during walkFile.
type placementFrame struct {
	stmt ast.Stmt
	spec *ast.ValueSpec
}

// walkFile walks file once and returns what placeFuncLit needs.
func (p *placement) walkFile(file *ast.File) *placementWalk {
	w := &placementWalk{
		codeEndColumns: make(map[int]int),
		stmtColumns:    make(map[int]int),
		litLines:       make(map[int]bool),
		litBraces:      make(map[int][]placementLitBrace),
	}
	keepMin := func(m map[int]int, pos token.Position) {
		if col, ok := m[pos.Line]; !ok || pos.Column < col {
			m[pos.Line] = pos.Column
		}
	}
	stack := []placementFrame{{}}
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		frame := stack[len(stack)-1]
		switch n := n.(type) {
		case *ast.CommentGroup, *ast.Comment:
			stack = append(stack, frame)
			return true
		case *ast.FuncLit:
			w.lits = append(w.lits, placementLit{lit: n, stmt: frame.stmt, spec: frame.spec})
			w.litLines[p.fset.PositionFor(n.Pos(), false).Line] = true
			if n.Body != nil {
				brace := p.fset.PositionFor(n.Body.Lbrace, false)
				w.litBraces[brace.Line] = append(w.litBraces[brace.Line], placementLitBrace{n, brace.Column})
			}
		case *ast.ValueSpec:
			frame.spec = n
		case *ast.CompositeLit:
			frame.spec = nil
		case ast.Stmt:
			frame.stmt = n
			keepMin(w.stmtColumns, p.fset.PositionFor(n.Pos(), false))
		}
		keepMin(w.codeEndColumns, p.fset.PositionFor(n.End(), false))
		stack = append(stack, frame)
		return true
	})
	return w
}

// findDirectiveAfterFuncDeclBrace finds a directive comment after the opening
// brace of a FuncDecl, on the same line (same-line pattern), and returns its
// position, or token.NoPos.
//
// Example:
//
//	func foo(db *gorm.DB) { //gormreuse:pure
//	    ...
//	}
//
// A closure in the body whose opening brace is between the declaration's and
// the directive takes the directive instead, by the innermost rule of
// matchesSameLineDirective (#315):
//
//	func outer(db *gorm.DB) { run(func(q *gorm.DB) { //gormreuse:pure  ← the closure's, not outer's
//
// An external function body (funcDecl.Body == nil) has none.
//
//declscope:shared // immutable_input.go places its directives with the same rules
func (p *placement) findDirectiveAfterFuncDeclBrace(funcDecl *ast.FuncDecl) token.Pos {
	if p.fset == nil || funcDecl.Body == nil {
		return token.NoPos
	}
	idx := p.indexFor(funcDecl)
	if idx == nil {
		return token.NoPos
	}
	brace := p.fset.PositionFor(funcDecl.Body.Lbrace, false)
	groups := idx.byStartLine[brace.Line]
	if len(groups) == 0 {
		return token.NoPos
	}
	// The directive is a line comment, which runs to the end of the line, so
	// a group that holds it and starts on the brace's line starts after the
	// brace, and it is the only one.
	g := idx.groups[groups[0]]
	body := funcDecl.Body
	if slices.ContainsFunc(idx.litBraces[brace.Line], func(lit placementLitBrace) bool {
		return lit.column > brace.Column && lit.column < g.startColumn &&
			body.Pos() <= lit.lit.Pos() && lit.lit.End() <= body.End()
	}) {
		return token.NoPos
	}
	return g.directives[0].Pos()
}

// findDirectiveForFuncLit finds the directive comment of a FuncLit and
// returns its position, or token.NoPos.
//
// Unlike FuncDecl which has Doc comments, FuncLit (closures) don't have attached
// documentation, so the directive is found by where it is written. Two
// placements are supported.
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
//declscope:shared // immutable_input.go places its directives with the same rules
func (p *placement) findDirectiveForFuncLit(funcLit *ast.FuncLit) token.Pos {
	if p.fset == nil {
		return token.NoPos
	}
	idx := p.indexFor(funcLit)
	if idx == nil {
		return token.NoPos
	}
	return idx.lits[funcLit]
}

// placeFuncLit returns the directive comment of a closure, or token.NoPos:
// the first directive group in the file that matches it by either pattern.
// Only a group on the line of its opening brace, or one that ends on the
// line above it or above its statement, can match, so only those are tried.
func (p *placement) placeFuncLit(idx *placementIndex, w *placementWalk, lit placementLit) token.Pos {
	var candidates []int
	if lit.lit.Body != nil {
		candidates = append(candidates, idx.byStartLine[p.fset.PositionFor(lit.lit.Body.Lbrace, false).Line]...)
	}
	candidates = append(candidates, idx.byEndLine[p.fset.PositionFor(lit.lit.Pos(), false).Line-1]...)
	switch {
	case lit.stmt != nil:
		candidates = append(candidates, idx.byEndLine[p.fset.PositionFor(lit.stmt.Pos(), false).Line-1]...)
	case lit.spec != nil:
		candidates = append(candidates, idx.byEndLine[p.fset.PositionFor(lit.spec.Pos(), false).Line-1]...)
	}
	slices.Sort(candidates)
	for _, i := range slices.Compact(candidates) {
		g := idx.groups[i]
		if p.matchesSameLineDirective(w, lit.lit, g) || p.matchesNextLineDirective(w, lit, g) {
			return g.directives[0].Pos()
		}
	}
	return token.NoPos
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
func (p *placement) matchesNextLineDirective(w *placementWalk, lit placementLit, g placementGroup) bool {
	// Requirement 1: Directive must be alone on its line (no code before it)
	if col, ok := w.codeEndColumns[g.startLine]; ok && col < g.startColumn {
		return false
	}

	// Requirement 2: If there's any FuncLit on any line of the comment,
	// this is a same-line pattern, not next-line
	for line := g.startLine; line <= g.endLine; line++ {
		if w.litLines[line] {
			return false
		}
	}

	// Find the enclosing assignment statement and check requirements
	return p.isDirectValueInStatementAfterLine(w, lit, g.endLine)
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
func (p *placement) matchesSameLineDirective(w *placementWalk, funcLit *ast.FuncLit, g placementGroup) bool {
	if funcLit.Body == nil {
		return false
	}

	// Requirements 1 & 2: Comment must be after brace on same line
	brace := p.fset.PositionFor(funcLit.Body.Lbrace, false)
	if g.startLine != brace.Line || g.startColumn <= brace.Column {
		return false
	}

	// Requirement 3: no closure nested in this one has its { between this {
	// and the comment
	body := funcLit.Body
	return !slices.ContainsFunc(w.litBraces[brace.Line], func(nested placementLitBrace) bool {
		return nested.lit != funcLit && nested.column > brace.Column && nested.column < g.startColumn &&
			body.Pos() <= nested.lit.Pos() && nested.lit.End() <= body.End()
	})
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
func (p *placement) isDirectValueInStatementAfterLine(w *placementWalk, lit placementLit, directiveLine int) bool {
	expectedLine := directiveLine + 1

	if lit.stmt == nil {
		// No enclosing statement: this is a package-level `var f = func(){}`
		// declaration (a GenDecl/ValueSpec is a Decl, not a Stmt). Local-var
		// closures reach here via a DeclStmt and are handled below; package-level
		// ones need the ValueSpec handled directly (issue #72 gap2).
		return p.isDirectValueInValueSpecAfterLine(lit, directiveLine)
	}

	stmtPos := p.fset.PositionFor(lit.stmt.Pos(), false)
	funcLitLine := p.fset.PositionFor(lit.lit.Pos(), false).Line

	// Case 1: Statement starts on expected line → all direct FuncLits in that statement
	// Case 2: FuncLit starts on expected line → this specific FuncLit (multi-line assignment)
	if stmtPos.Line != expectedLine && funcLitLine != expectedLine {
		return false
	}

	// For semicolon-separated statements, only the first statement applies:
	// no statement may start before this one's column on the expected line.
	checkLine := stmtPos.Line
	if stmtPos.Line != expectedLine {
		checkLine = funcLitLine
	}
	if col, ok := w.stmtColumns[checkLine]; ok && col < stmtPos.Column {
		return false
	}

	// Check if FuncLit is a direct value (not inside CompositeLit)
	return p.isDirectRHSValue(lit.stmt, lit.lit)
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
func (p *placement) isDirectValueInValueSpecAfterLine(lit placementLit, directiveLine int) bool {
	expectedLine := directiveLine + 1

	// A composite literal between the FuncLit and the spec means the closure
	// is not a direct value (e.g. `var x = &S{Fn: func(){}}`); walkFile
	// leaves spec nil then.
	if lit.spec == nil {
		return false
	}

	specLine := p.fset.PositionFor(lit.spec.Pos(), false).Line
	funcLitLine := p.fset.PositionFor(lit.lit.Pos(), false).Line
	if specLine != expectedLine && funcLitLine != expectedLine {
		return false
	}

	// The closure must be a direct value of the spec.
	return slices.Contains(lit.spec.Values, ast.Expr(lit.lit))
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
