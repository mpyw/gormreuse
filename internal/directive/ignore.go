package directive

import (
	"cmp"
	"go/ast"
	"go/token"
	"slices"

	"golang.org/x/tools/go/ast/inspector"
)

// =============================================================================
// Ignore Directive Handling
// =============================================================================

// Ignores holds the //gormreuse:ignore directives of a package and decides
// which diagnostics they suppress. One rule covers every diagnostic the
// analyzer emits (#190):
//
//   - A line ignore covers the statement it is on or directly above. A
//     diagnostic is suppressed when the ignore covers any line of the
//     innermost statement that holds it. For if/for/switch/select and case
//     clauses only the header counts, so an ignore on the header does not
//     reach the body.
//   - A function ignore (in a func declaration's doc comment) covers the
//     whole declaration, its doc comment and every closure inside it.
//   - A file ignore (before the package clause) covers the whole file.
//
// Unused-ignore and internal-error reports are the exceptions: no ignore
// suppresses them.
type Ignores struct {
	fset  *token.FileSet
	files map[string]*fileIgnores
}

// NewIgnores creates an empty set. Add each analyzed file with AddFile.
func NewIgnores(fset *token.FileSet) *Ignores {
	return &Ignores{fset: fset, files: make(map[string]*fileIgnores)}
}

// AddFile records the ignore directives of one file. file is the file's
// cursor in the pass inspector.
func (ig *Ignores) AddFile(file inspector.Cursor) {
	f := file.Node().(*ast.File)
	filename := ig.fset.PositionFor(f.Pos(), false).Filename
	ig.files[filename] = &fileIgnores{
		file:  f,
		lines: buildIgnoreMap(ig.fset, f),
		funcs: buildIgnoredFuncSpans(ig.fset, file),
	}
}

// Ignore reports whether an ignore directive suppresses a diagnostic at pos.
// The directive that suppresses it is marked used. Lookups try the most
// specific scope first (line, then function, then file), so overlapping
// ignores leave the wider one unused.
func (ig *Ignores) Ignore(pos token.Pos) bool {
	if ig == nil || !pos.IsValid() {
		return false
	}
	position := ig.fset.PositionFor(pos, false)
	f := ig.files[position.Filename]
	if f == nil {
		return false
	}
	if entry := f.lineIgnoreFor(ig.fset, pos, position.Line); entry != nil {
		entry.used = true
		return true
	}
	// The spans are in source order and do not overlap: the last one that
	// starts at or before pos is the only one that can hold it.
	if i, _ := slices.BinarySearchFunc(f.funcs, pos, func(fn ignoredFuncSpan, pos token.Pos) int {
		return cmp.Compare(fn.start, pos+1)
	}); i > 0 {
		if fn := f.funcs[i-1]; pos < fn.end {
			if entry := f.lines[fn.directiveLine]; entry != nil {
				entry.used = true
			}
			return true
		}
	}
	if _, fileIgnore := f.lines[-1]; fileIgnore {
		return true
	}
	return false
}

// UnusedIgnores returns the positions of line and function ignores that
// suppressed nothing, in position order.
func (ig *Ignores) UnusedIgnores() []token.Pos {
	if ig == nil {
		return nil
	}
	var unused []token.Pos
	for _, f := range ig.files {
		unused = append(unused, f.lines.unusedIgnores()...)
	}
	slices.Sort(unused)
	return unused
}

// fileIgnores is the ignore scope of one file.
type fileIgnores struct {
	file  *ast.File
	lines ignoreMap
	funcs []ignoredFuncSpan // In source order
	// stmts and lits are the file's statements and closures, built on the
	// first lookup that needs them.
	stmts, lits *ignoreSpans
}

// ignoredFuncSpan is a func declaration with a function-level ignore.
type ignoredFuncSpan struct {
	start, end    token.Pos // From the doc comment to the closing brace
	directiveLine int       // Line of the ignore comment, a key of the line map
}

// lineIgnoreFor returns the line ignore that covers a diagnostic at pos (on
// line), or nil. It checks the diagnostic's own line, then every line of the
// innermost statement holding pos. An ignore comment inside a closure nested in
// that statement belongs to the closure's statements, not to this one.
func (f *fileIgnores) lineIgnoreFor(fset *token.FileSet, pos token.Pos, line int) *ignoreEntry {
	if _, fileIgnore := f.lines[-1]; len(f.lines) == 0 || fileIgnore && len(f.lines) == 1 {
		return nil // No line ignore: skip the statement walk
	}
	if entry := f.lines.lineIgnore(line); entry != nil {
		return entry
	}
	if f.stmts == nil {
		f.stmts, f.lits = buildIgnoreSpans(f.file)
	}
	stmt, _ := f.stmts.innermost(pos).(ast.Stmt)
	if stmt == nil {
		return nil
	}
	end := ignoreStmtHeaderEnd(stmt)
	if pos > end {
		return nil // In a compound statement's body, outside its statements
	}
	first := fset.PositionFor(stmt.Pos(), false).Line
	last := fset.PositionFor(end, false).Line
	for l := first; l <= last; l++ {
		if entry := f.lines.lineIgnore(l); entry != nil && !f.inIgnoreFuncLit(stmt, entry.pos) {
			return entry
		}
	}
	return nil
}

// ignoreSpans are the nodes of one kind in a file, in source order, each with
// the index of the innermost one that holds it. Syntax nodes nest, so the
// nodes that hold a position are the last one that starts at or before it and
// the ones that hold that one: a lookup is a binary search and a walk up
// (#301). Before, each lookup walked the file from its root.
type ignoreSpans struct {
	nodes  []ast.Node
	parent []int // Index in nodes of the innermost node that holds each one, or -1
}

// buildIgnoreSpans walks file once and returns its statements (blocks and
// labeled statements left out) and its closures.
func buildIgnoreSpans(file *ast.File) (stmts, lits *ignoreSpans) {
	stmts, lits = &ignoreSpans{}, &ignoreSpans{}
	// The innermost statement and closure that hold the node being visited.
	type open struct{ stmt, lit int }
	stack := []open{{-1, -1}}
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		top := stack[len(stack)-1]
		switch n.(type) {
		case *ast.BlockStmt, *ast.LabeledStmt:
		case ast.Stmt:
			top.stmt = stmts.add(n, top.stmt)
		case *ast.FuncLit:
			top.lit = lits.add(n, top.lit)
		}
		stack = append(stack, top)
		return true
	})
	return stmts, lits
}

// add appends n, held by the node at index parent, and returns its index.
func (s *ignoreSpans) add(n ast.Node, parent int) int {
	s.nodes = append(s.nodes, n)
	s.parent = append(s.parent, parent)
	return len(s.nodes) - 1
}

// innermost returns the innermost node that holds pos, or nil.
func (s *ignoreSpans) innermost(pos token.Pos) ast.Node {
	if i := s.innermostIndex(pos); i >= 0 {
		return s.nodes[i]
	}
	return nil
}

// innermostIndex returns the index of the innermost node that holds pos, or -1.
func (s *ignoreSpans) innermostIndex(pos token.Pos) int {
	i, _ := slices.BinarySearchFunc(s.nodes, pos, func(n ast.Node, pos token.Pos) int {
		return cmp.Compare(n.Pos(), pos+1)
	})
	for i--; i >= 0 && s.nodes[i].End() <= pos; {
		i = s.parent[i]
	}
	return i
}

// ignoreStmtHeaderEnd returns where the part of stmt that a line ignore covers
// ends: the opening brace or colon of a compound statement, or the statement's
// end otherwise.
func ignoreStmtHeaderEnd(stmt ast.Stmt) token.Pos {
	switch s := stmt.(type) {
	case *ast.IfStmt:
		return s.Body.Lbrace
	case *ast.ForStmt:
		return s.Body.Lbrace
	case *ast.RangeStmt:
		return s.Body.Lbrace
	case *ast.SwitchStmt:
		return s.Body.Lbrace
	case *ast.TypeSwitchStmt:
		return s.Body.Lbrace
	case *ast.SelectStmt:
		return s.Body.Lbrace
	case *ast.CaseClause:
		return s.Colon
	case *ast.CommClause:
		return s.Colon
	}
	return stmt.End()
}

// inIgnoreFuncLit reports whether pos lies inside the body of a closure
// nested in stmt.
func (f *fileIgnores) inIgnoreFuncLit(stmt ast.Stmt, pos token.Pos) bool {
	if pos < stmt.Pos() || stmt.End() <= pos {
		return false
	}
	// The closures that hold pos, innermost first. The ones nested in stmt
	// come before the ones that hold stmt.
	for i := f.lits.innermostIndex(pos); i >= 0; i = f.lits.parent[i] {
		lit := f.lits.nodes[i].(*ast.FuncLit)
		if lit.Pos() < stmt.Pos() {
			return false
		}
		if lit.Body.Lbrace < pos {
			return true
		}
	}
	return false
}

// buildIgnoredFuncSpans returns the func declarations of a file whose doc
// comment holds an ignore directive. file is the file's cursor in the pass
// inspector.
func buildIgnoredFuncSpans(fset *token.FileSet, file inspector.Cursor) []ignoredFuncSpan {
	var spans []ignoredFuncSpan
	for cur := range file.Preorder(funcDeclTypes...) {
		fd := cur.Node().(*ast.FuncDecl)
		if fd.Doc == nil {
			continue
		}
		for _, c := range fd.Doc.List {
			if isIgnoreDirective(c.Text) {
				spans = append(spans, ignoredFuncSpan{
					start:         fd.Doc.Pos(),
					end:           fd.End(),
					directiveLine: fset.PositionFor(c.Pos(), false).Line,
				})
				break
			}
		}
	}
	return spans
}

// ignoreMap tracks line numbers that have ignore comments.
//
// Keys:
//   - Positive integer: line number with ignore directive
//   - -1: special marker for file-level ignore
//
// The map is built during AST scanning and used during violation reporting
// to determine if a violation should be suppressed.
type ignoreMap map[int]*ignoreEntry

// buildIgnoreMap scans a file for ignore comments and returns a map.
// It also records a file-level ignore. A function-level ignore is a line entry
// too; Ignores widens it to the function.
//
// Example:
//
//	//gormreuse:ignore         // Line 5 → map[5] (line-level)
//	q.Find(nil)                // Line 6 → ignored (line 5 covers line 6)
//
//	q.Count(nil) //gormreuse:ignore // Line 7 → map[7], covers line 7 only
//
//	// File-level ignore (before package declaration):
//	//gormreuse:ignore         // → map[-1] (special marker)
//	package main               // All lines ignored
//
// The returned map uses line numbers as keys:
//   - Positive line: ignore directive for that line, and for the next line
//     when the comment is the first token on its line
//   - Line -1: file-level ignore (all lines)
func buildIgnoreMap(fset *token.FileSet, file *ast.File) ignoreMap {
	m := make(ignoreMap)

	// Get package declaration line for file-level ignore detection
	packageLine := fset.PositionFor(file.Package, false).Line

	// Computed lazily: most files have no line-level ignore.
	var codeStart map[int]int

	// Check all comments for ignore directives
	for _, cg := range file.Comments {
		for _, c := range cg.List {
			pos := fset.PositionFor(c.Pos(), false)
			if isIgnoreDirective(c.Text) {
				// A directive anywhere BEFORE the package clause is a file-level
				// ignore, regardless of distance: there is no code above the
				// package clause for a line-level ignore to attach to, so the only
				// sensible meaning is "ignore this whole file". The previous fixed
				// 5-line window both missed directives sitting above a long license
				// header and mis-classified nearby ones.
				if pos.Line < packageLine {
					// File-level ignore: mark all lines as ignored (line -1 marker).
					// File-level ignores are always considered "used" (no warning).
					m[-1] = &ignoreEntry{pos: c.Pos(), used: true}
				} else {
					// Regular line-level ignore. A trailing comment after code
					// covers its own line only; a comment that starts its line
					// also covers the next line (#189).
					if codeStart == nil {
						codeStart = buildIgnoreCodeStartColumns(fset, file)
					}
					col, hasCode := codeStart[pos.Line]
					m[pos.Line] = &ignoreEntry{
						pos:      c.Pos(),
						nextLine: !hasCode || col >= pos.Column,
					}
				}
			}
		}
	}

	// Also check for file-level ignore in doc comments (package documentation)
	if file.Doc != nil {
		for _, c := range file.Doc.List {
			if isIgnoreDirective(c.Text) {
				// File-level ignore: mark all lines as ignored
				// File-level ignores are always considered "used" (no warning for them)
				m[-1] = &ignoreEntry{pos: c.Pos(), used: true}
			}
		}
	}

	return m
}

// lineIgnore returns the line-level ignore that covers line, or nil: one on
// the same line, or one on the previous line that starts its line. It does not
// mark the entry used, and it does not look at a file-level ignore.
func (m ignoreMap) lineIgnore(line int) *ignoreEntry {
	if entry, onSameLine := m[line]; onSameLine {
		return entry
	}
	if entry, onPrevLine := m[line-1]; onPrevLine && entry.nextLine {
		return entry
	}
	return nil
}

// unusedIgnores returns the positions of ignore directives that were not used.
func (m ignoreMap) unusedIgnores() []token.Pos {
	var unused []token.Pos
	for line, entry := range m {
		if line == -1 {
			// Skip file-level ignores
			continue
		}
		if !entry.used {
			unused = append(unused, entry.pos)
		}
	}
	return unused
}

// ignoreEntry tracks an ignore directive and whether it was used.
// Used to report "unused ignore directive" warnings.
type ignoreEntry struct {
	pos      token.Pos // Position of the ignore comment (for reporting unused)
	used     bool      // Whether this ignore was actually used to suppress a warning
	nextLine bool      // Whether the comment starts its line, so it also covers the next line
}

// buildIgnoreCodeStartColumns maps each line to the smallest column at which
// a syntax node starts or ends on it. A comment whose column is greater than
// this value has code before it on the same line.
func buildIgnoreCodeStartColumns(fset *token.FileSet, file *ast.File) map[int]int {
	cols := make(map[int]int)
	record := func(p token.Pos) {
		if !p.IsValid() {
			return
		}
		pos := fset.PositionFor(p, false)
		if col, ok := cols[pos.Line]; !ok || pos.Column < col {
			cols[pos.Line] = pos.Column
		}
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch n.(type) {
		case nil:
			return false
		case *ast.CommentGroup, *ast.Comment:
			return false
		}
		record(n.Pos())
		if end := n.End(); end.IsValid() && end > n.Pos() {
			// End is one past the last character; record the last character.
			record(end - 1)
		}
		return true
	})
	return cols
}
