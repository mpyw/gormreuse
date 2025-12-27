package directive

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"strings"

	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/go/ast/inspector"
	"golang.org/x/tools/go/ssa"
)

// =============================================================================
// Type String Helpers (for receiver type matching)
// =============================================================================

// stripPointer removes leading "*" from a type string.
func stripPointer(s string) string {
	return strings.TrimPrefix(s, "*")
}

// exprToString converts an ast.Expr to a string representation.
// For generic types like GenericReceiver[T], returns just the base type name.
func exprToString(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.StarExpr:
		return "*" + exprToString(e.X)
	case *ast.SelectorExpr:
		return exprToString(e.X) + "." + e.Sel.Name
	case *ast.IndexExpr:
		// Generic type with single type parameter: Type[T] -> Type
		return exprToString(e.X)
	case *ast.IndexListExpr:
		// Generic type with multiple type parameters: Type[T, U] -> Type
		return exprToString(e.X)
	default:
		return ""
	}
}

// formatReceiverType extracts the base type name from a receiver type.
// Returns just the type name without pointer (e.g., "Orm" for both *Orm and Orm).
// Go doesn't allow both pointer and value receivers with the same method name,
// so the pointer is irrelevant for matching.
func formatReceiverType(t types.Type) string {
	// Unwrap pointer if present
	if ptr, ok := t.(*types.Pointer); ok {
		t = ptr.Elem()
	}
	if named, ok := t.(*types.Named); ok {
		return named.Obj().Name()
	}
	return ""
}

// FuncKey identifies a function by package, receiver type, and name.
type FuncKey struct {
	PkgPath      string // Package path (e.g., "github.com/example/pkg")
	ReceiverType string // Receiver type name without pointer (e.g., "Orm"), empty for functions
	FuncName     string // Function or method name
}

// directiveChecker is a function that checks if a comment is a specific directive.
type directiveChecker func(text string) bool

// signatureValidator checks if a function signature is valid for the directive.
// For pure: returns true if any parameter contains *gorm.DB
// For immutable-return: returns true if any return value contains *gorm.DB
type signatureValidator func(*types.Signature) bool

// DirectiveFuncSet is a generic set of functions matching a directive.
// It supports both pre-built sets (for current package) and on-demand
// directive checking (for external packages via source parsing).
// It also tracks which directives have invalid signatures to report unused directives.
type DirectiveFuncSet struct {
	known               map[FuncKey]struct{}
	fset                *token.FileSet
	typesInfo           *types.Info            // Type info for signature validation
	files               map[string]*ast.File   // Original parsed files (from analysis)
	cache               map[string]*ast.File   // Cache for external files (re-parsed)
	isDirective         directiveChecker       // Checks if comment is this directive
	validateSignature   signatureValidator     // Checks if signature is valid for this directive
	invalidDirectives   map[token.Pos]struct{} // Directives on functions with invalid signatures
	processedDirectives map[token.Pos]struct{} // All directive positions processed by this set

	// Cache for hasCodeBeforeComment results to avoid O(comments * nodes) complexity
	codeBeforeCommentCache map[*ast.File]map[token.Pos]bool
	// Cache for FuncLit line numbers per file to avoid O(nodes) lookup per line check
	funcLitLinesCache map[*ast.File]map[int]bool
	// Cache for ast/inspector to avoid repeated traversal setup
	inspectorCache map[*ast.File]*inspector.Inspector
}

// Node type filters for inspector
var (
	funcDeclTypes = []ast.Node{(*ast.FuncDecl)(nil)}
	funcLitTypes  = []ast.Node{(*ast.FuncLit)(nil)}
)

// newDirectiveFuncSet creates a new DirectiveFuncSet with the given directive checker and signature validator.
func newDirectiveFuncSet(fset *token.FileSet, typesInfo *types.Info, isDirective directiveChecker, validateSignature signatureValidator) *DirectiveFuncSet {
	return &DirectiveFuncSet{
		known:                  make(map[FuncKey]struct{}),
		fset:                   fset,
		typesInfo:              typesInfo,
		files:                  make(map[string]*ast.File),
		cache:                  make(map[string]*ast.File),
		isDirective:            isDirective,
		validateSignature:      validateSignature,
		invalidDirectives:      make(map[token.Pos]struct{}),
		processedDirectives:    make(map[token.Pos]struct{}),
		codeBeforeCommentCache: make(map[*ast.File]map[token.Pos]bool),
		funcLitLinesCache:      make(map[*ast.File]map[int]bool),
		inspectorCache:         make(map[*ast.File]*inspector.Inspector),
	}
}

// getInspector returns a cached inspector for the given file.
func (s *DirectiveFuncSet) getInspector(file *ast.File) *inspector.Inspector {
	if insp, ok := s.inspectorCache[file]; ok {
		return insp
	}
	insp := inspector.New([]*ast.File{file})
	s.inspectorCache[file] = insp
	return insp
}

// AddFile adds an original parsed file to the set and collects all directive positions.
// This should be called for all files in the current package to avoid re-parsing.
func (s *DirectiveFuncSet) AddFile(file *ast.File) {
	if s == nil || s.fset == nil || file == nil {
		return
	}
	filename := s.fset.Position(file.Pos()).Filename
	if filename != "" {
		s.files[filename] = file
	}

	// Collect all directive positions in this file for unused detection
	s.collectDirectivePositions(file)
}

// collectDirectivePositions scans a file for all directives and validates their signatures.
// Directives on functions with invalid signatures are added to invalidDirectives.
func (s *DirectiveFuncSet) collectDirectivePositions(file *ast.File) {
	if s == nil || s.invalidDirectives == nil {
		return
	}

	insp := s.getInspector(file)

	// Check FuncDecl doc comments and same-line comments
	insp.Preorder(funcDeclTypes, func(n ast.Node) {
		fd := n.(*ast.FuncDecl)

		// Check doc comments (next-line pattern)
		if fd.Doc != nil {
			for _, c := range fd.Doc.List {
				if s.isDirective(c.Text) {
					s.processedDirectives[c.Pos()] = struct{}{}
					if !s.validateFuncDeclSignature(fd) {
						s.invalidDirectives[c.Pos()] = struct{}{}
					}
				}
			}
		}

		// Check same-line pattern (after opening brace)
		if pos := s.findDirectiveAfterFuncDeclBrace(fd); pos.IsValid() {
			s.processedDirectives[pos] = struct{}{}
			if !s.validateFuncDeclSignature(fd) {
				s.invalidDirectives[pos] = struct{}{}
			}
		}
	})

	// Check FuncLit directives (next-line and same-line patterns)
	insp.Preorder(funcLitTypes, func(n ast.Node) {
		fl := n.(*ast.FuncLit)
		if pos := s.findDirectiveForFuncLit(fl); pos.IsValid() {
			s.processedDirectives[pos] = struct{}{}
			if !s.validateFuncLitSignature(fl) {
				s.invalidDirectives[pos] = struct{}{}
			}
		}
	})

	// Find orphan directives (not associated with any function)
	// These are always invalid
	associatedDirectives := make(map[token.Pos]bool)

	// Collect all directive positions that are associated with functions
	insp.Preorder(funcDeclTypes, func(n ast.Node) {
		fd := n.(*ast.FuncDecl)
		if fd.Doc != nil {
			for _, c := range fd.Doc.List {
				if s.isDirective(c.Text) {
					associatedDirectives[c.Pos()] = true
				}
			}
		}
		if pos := s.findDirectiveAfterFuncDeclBrace(fd); pos.IsValid() {
			associatedDirectives[pos] = true
		}
	})
	insp.Preorder(funcLitTypes, func(n ast.Node) {
		fl := n.(*ast.FuncLit)
		if pos := s.findDirectiveForFuncLit(fl); pos.IsValid() {
			associatedDirectives[pos] = true
		}
	})

	// Mark all non-associated directives as invalid
	for _, cg := range file.Comments {
		for _, c := range cg.List {
			if s.isDirective(c.Text) && !associatedDirectives[c.Pos()] {
				s.processedDirectives[c.Pos()] = struct{}{}
				s.invalidDirectives[c.Pos()] = struct{}{}
			}
		}
	}
}

// validateFuncDeclSignature checks if a FuncDecl has a valid signature for this directive.
func (s *DirectiveFuncSet) validateFuncDeclSignature(fd *ast.FuncDecl) bool {
	if s.typesInfo == nil || s.validateSignature == nil {
		return true // Can't validate without type info, assume valid
	}
	obj := s.typesInfo.ObjectOf(fd.Name)
	if obj == nil {
		return true
	}
	fn, ok := obj.(*types.Func)
	if !ok {
		return true
	}
	sig, ok := fn.Type().(*types.Signature)
	if !ok {
		return true
	}
	return s.validateSignature(sig)
}

// validateFuncLitSignature checks if a FuncLit has a valid signature for this directive.
func (s *DirectiveFuncSet) validateFuncLitSignature(fl *ast.FuncLit) bool {
	if s.typesInfo == nil || s.validateSignature == nil {
		return true // Can't validate without type info, assume valid
	}
	tv, ok := s.typesInfo.Types[fl]
	if !ok {
		return true
	}
	sig, ok := tv.Type.(*types.Signature)
	if !ok {
		return true
	}
	return s.validateSignature(sig)
}

// Add adds a function key to the set.
func (s *DirectiveFuncSet) Add(key FuncKey) {
	if s != nil && s.known != nil {
		s.known[key] = struct{}{}
	}
}

// GetUnusedDirectives returns the positions of directives on functions with invalid signatures.
// A directive is "unused" if:
//   - For pure: the function has no *gorm.DB in its parameters
//   - For immutable-return: the function has no *gorm.DB in its return values
func (s *DirectiveFuncSet) GetUnusedDirectives() []token.Pos {
	if s == nil || s.invalidDirectives == nil {
		return nil
	}

	var unused []token.Pos
	for pos := range s.invalidDirectives {
		unused = append(unused, pos)
	}
	return unused
}

// IsUsed returns true if the directive at the given position has a valid signature.
// This is used for combined directive handling (e.g., //gormreuse:pure,immutable-return)
// where if one directive type is valid, the other shouldn't report it as unused.
func (s *DirectiveFuncSet) IsUsed(pos token.Pos) bool {
	if s == nil || s.processedDirectives == nil {
		return false
	}
	// First check if this directive was processed by this set
	// (i.e., it matches our isDirective check)
	if _, processed := s.processedDirectives[pos]; !processed {
		return false
	}
	// Directive is "used" (valid) if it's NOT in invalidDirectives
	_, invalid := s.invalidDirectives[pos]
	return !invalid
}

// Contains checks if the given SSA function is in the set or has the directive.
func (s *DirectiveFuncSet) Contains(fn *ssa.Function) bool {
	if fn == nil {
		return false
	}

	// First, check the pre-built set (for current package)
	if s != nil && s.known != nil {
		key := FuncKey{FuncName: fn.Name()}
		if fn.Pkg != nil && fn.Pkg.Pkg != nil {
			key.PkgPath = fn.Pkg.Pkg.Path()
		}
		if sig := fn.Signature; sig != nil && sig.Recv() != nil {
			key.ReceiverType = formatReceiverType(sig.Recv().Type())
		}
		if _, exists := s.known[key]; exists {
			return true
		}
	}

	// Second, check the SSA function's syntax for directive (for external packages)
	return s.hasDirective(fn)
}

// hasDirective checks if an SSA function has the directive.
func (s *DirectiveFuncSet) hasDirective(fn *ssa.Function) bool {
	if fn == nil {
		return false
	}

	// Try getting syntax from the SSA function (works for current package)
	switch syntax := fn.Syntax().(type) {
	case *ast.FuncDecl:
		// Check Doc comments (next-line pattern)
		if syntax.Doc != nil {
			for _, c := range syntax.Doc.List {
				if s.isDirective(c.Text) {
					return true
				}
			}
		}
		// Check same-line pattern (after opening brace)
		if pos := s.findDirectiveAfterFuncDeclBrace(syntax); pos.IsValid() {
			return true
		}
	case *ast.FuncLit:
		// Closures don't have Doc comments in Go, so we look for comments
		// immediately before or after the opening brace.
		if pos := s.findDirectiveForFuncLit(syntax); pos.IsValid() {
			return true
		}
	}

	// Fallback: parse the source file for external packages
	if s == nil || s.fset == nil {
		return false
	}
	obj := fn.Object()
	if obj == nil {
		return false
	}
	pos := obj.Pos()
	if !pos.IsValid() {
		return false
	}
	filename := s.fset.Position(pos).Filename
	if filename == "" {
		return false
	}
	file := s.parseFile(filename)
	if file == nil {
		return false
	}

	funcName := fn.Name()
	var receiverType string
	if sig := fn.Signature; sig != nil && sig.Recv() != nil {
		receiverType = formatReceiverType(sig.Recv().Type())
	}
	return s.hasDirectiveInFile(file, funcName, receiverType)
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
func (s *DirectiveFuncSet) findMatchingDirective(node ast.Node, predicate func(file *ast.File, cg *ast.CommentGroup) bool) token.Pos {
	file := s.getFileForNode(node)
	if file == nil {
		return token.NoPos
	}
	for _, cg := range file.Comments {
		if s.commentGroupHasDirective(cg) && predicate(file, cg) {
			// Return the position of the first directive comment in the group
			for _, c := range cg.List {
				if s.isDirective(c.Text) {
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
func (s *DirectiveFuncSet) findDirectiveAfterFuncDeclBrace(funcDecl *ast.FuncDecl) token.Pos {
	if s == nil || s.fset == nil || funcDecl.Body == nil {
		return token.NoPos
	}
	bracePos := s.fset.Position(funcDecl.Body.Lbrace)
	return s.findMatchingDirective(funcDecl, func(_ *ast.File, cg *ast.CommentGroup) bool {
		return s.isCommentAfterBrace(cg, bracePos)
	})
}

// isCommentAfterBrace checks if a comment is on the same line as a brace and appears after it.
//
// This is used for same-line directive detection:
//
//	func() { //comment  ← comment column > brace column, same line → true
//	//comment           ← different line → false
//	{ //comment         ← brace at column 1, comment at column 3 → true
func (s *DirectiveFuncSet) isCommentAfterBrace(cg *ast.CommentGroup, bracePos token.Position) bool {
	cgPos := s.fset.Position(cg.Pos())
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
func (s *DirectiveFuncSet) findDirectiveForFuncLit(funcLit *ast.FuncLit) token.Pos {
	if s == nil || s.fset == nil {
		return token.NoPos
	}
	return s.findMatchingDirective(funcLit, func(file *ast.File, cg *ast.CommentGroup) bool {
		return s.matchesSameLineDirective(file, funcLit, cg) || s.matchesNextLineDirective(file, funcLit, cg)
	})
}

// getFileForNode returns the AST file containing the given node.
// It first checks for original files (from analysis), then falls back to re-parsing.
func (s *DirectiveFuncSet) getFileForNode(node ast.Node) *ast.File {
	pos := node.Pos()
	if !pos.IsValid() {
		return nil
	}
	filename := s.fset.Position(pos).Filename
	if filename == "" {
		return nil
	}
	// First, try to use the original file (avoids position mismatch)
	if file, ok := s.files[filename]; ok {
		return file
	}
	// Fall back to re-parsing (for external packages)
	return s.parseFile(filename)
}

// commentGroupHasDirective checks if a comment group contains our directive.
func (s *DirectiveFuncSet) commentGroupHasDirective(cg *ast.CommentGroup) bool {
	for _, c := range cg.List {
		if s.isDirective(c.Text) {
			return true
		}
	}
	return false
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
func (s *DirectiveFuncSet) matchesNextLineDirective(file *ast.File, funcLit *ast.FuncLit, cg *ast.CommentGroup) bool {
	cgStartLine := s.fset.Position(cg.Pos()).Line
	cgEndLine := s.fset.Position(cg.End()).Line

	// Requirement 1: Directive must be alone on its line (no code before it)
	if s.hasCodeBeforeComment(file, cg) {
		return false
	}

	// Requirement 2: If there's any FuncLit on any line of the comment,
	// this is a same-line pattern, not next-line
	for line := cgStartLine; line <= cgEndLine; line++ {
		if s.hasFuncLitOnLine(file, line) {
			return false
		}
	}

	// Find the enclosing assignment statement and check requirements
	return s.isDirectValueInStatementAfterLine(file, funcLit, cgEndLine)
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
func (s *DirectiveFuncSet) matchesSameLineDirective(file *ast.File, funcLit *ast.FuncLit, cg *ast.CommentGroup) bool {
	if funcLit.Body == nil {
		return false
	}

	bracePos := s.fset.Position(funcLit.Body.Lbrace)

	// Requirements 1 & 2: Comment must be after brace on same line
	if !s.isCommentAfterBrace(cg, bracePos) {
		return false
	}

	// Requirement 3: Check there's no nested FuncLit brace between this { and the comment
	cgColumn := s.fset.Position(cg.Pos()).Column
	return !s.hasNestedFuncLitBetween(funcLit, bracePos.Column, cgColumn, bracePos.Line)
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
func (s *DirectiveFuncSet) isDirectValueInStatementAfterLine(file *ast.File, funcLit *ast.FuncLit, directiveLine int) bool {
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
		return false
	}

	stmtLine := s.fset.Position(enclosingStmt.Pos()).Line
	funcLitLine := s.fset.Position(funcLit.Pos()).Line

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
	if !s.isFirstStatementOnLine(file, enclosingStmt, checkLine) {
		return false
	}

	// Check if FuncLit is a direct value (not inside CompositeLit)
	return s.isDirectRHSValue(enclosingStmt, funcLit)
}

// isFirstStatementOnLine checks if the given statement is the first (leftmost) one on its line.
// This is important for semicolon-separated statements: `a := f1(); b := f2()`
func (s *DirectiveFuncSet) isFirstStatementOnLine(file *ast.File, target ast.Stmt, line int) bool {
	targetColumn := s.fset.Position(target.Pos()).Column
	isFirst := true

	// Check all function bodies in the file for statements on this line
	ast.Inspect(file, func(n ast.Node) bool {
		if !isFirst {
			return false
		}
		if stmt, ok := n.(ast.Stmt); ok && stmt != target {
			stmtPos := s.fset.Position(stmt.Pos())
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
// Note: We use position matching because the target FuncLit may be from a different
// AST parse than stmt (SSA analysis vs our re-parsed file).
func (s *DirectiveFuncSet) isDirectRHSValue(stmt ast.Stmt, target *ast.FuncLit) bool {
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
	for _, rhs := range rhsExprs {
		if s.isDirectExprOrUnaryByPos(rhs, targetPos, targetEnd) {
			return true
		}
	}
	return false
}

// isDirectExprOrUnaryByPos checks if expr contains a FuncLit at the given position,
// and that FuncLit is a direct expression or inside unary/paren (not inside CompositeLit).
func (s *DirectiveFuncSet) isDirectExprOrUnaryByPos(expr ast.Expr, targetPos, targetEnd token.Pos) bool {
	switch e := expr.(type) {
	case *ast.FuncLit:
		// Use position matching instead of identity
		return e.Pos() == targetPos && e.End() == targetEnd
	case *ast.UnaryExpr:
		// Handle &func(){} case
		return s.isDirectExprOrUnaryByPos(e.X, targetPos, targetEnd)
	case *ast.ParenExpr:
		// Handle (func(){}) case
		return s.isDirectExprOrUnaryByPos(e.X, targetPos, targetEnd)
	default:
		// CompositeLit, CallExpr, IndexExpr, etc. - target is nested, not direct
		return false
	}
}

// hasFuncLitOnLine checks if there's any FuncLit that starts on the given line.
// Results are cached per file to avoid O(nodes) traversal per line check.
func (s *DirectiveFuncSet) hasFuncLitOnLine(file *ast.File, line int) bool {
	// Build cache if not exists
	if _, ok := s.funcLitLinesCache[file]; !ok {
		s.buildFuncLitLinesCache(file)
	}
	return s.funcLitLinesCache[file][line]
}

// buildFuncLitLinesCache pre-computes all FuncLit line numbers for a file.
func (s *DirectiveFuncSet) buildFuncLitLinesCache(file *ast.File) {
	lines := make(map[int]bool)
	insp := s.getInspector(file)
	insp.Preorder(funcLitTypes, func(n ast.Node) {
		fl := n.(*ast.FuncLit)
		lines[s.fset.Position(fl.Pos()).Line] = true
	})
	s.funcLitLinesCache[file] = lines
}

// hasCodeBeforeComment checks if there's any code (non-whitespace) before the comment on the same line.
// This is used to determine if a directive is "alone" on its line or follows other code like "}, //gormreuse:pure".
// Results are cached per file to avoid O(comments * nodes) complexity.
func (s *DirectiveFuncSet) hasCodeBeforeComment(file *ast.File, cg *ast.CommentGroup) bool {
	// Check cache first
	if fileCache, ok := s.codeBeforeCommentCache[file]; ok {
		if result, ok := fileCache[cg.Pos()]; ok {
			return result
		}
	}

	// Initialize cache for this file if not exists
	if s.codeBeforeCommentCache[file] == nil {
		s.codeBeforeCommentCache[file] = make(map[token.Pos]bool)
	}

	// Compute result
	result := s.computeCodeBeforeComment(file, cg)
	s.codeBeforeCommentCache[file][cg.Pos()] = result
	return result
}

// computeCodeBeforeComment does the actual computation for hasCodeBeforeComment.
func (s *DirectiveFuncSet) computeCodeBeforeComment(file *ast.File, cg *ast.CommentGroup) bool {
	cgPos := s.fset.Position(cg.Pos())
	cgLine := cgPos.Line
	cgColumn := cgPos.Column

	// Check if any AST node ends on the same line before the comment
	hasCode := false
	ast.Inspect(file, func(n ast.Node) bool {
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
		nodeStart := s.fset.Position(n.Pos())
		nodeEnd := s.fset.Position(n.End())

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
func (s *DirectiveFuncSet) hasNestedFuncLitBetween(parent *ast.FuncLit, startColumn, endColumn, line int) bool {
	found := false
	ast.Inspect(parent.Body, func(n ast.Node) bool {
		if found {
			return false
		}
		if nested, ok := n.(*ast.FuncLit); ok && nested != parent {
			if nested.Body != nil {
				nestedPos := s.fset.Position(nested.Body.Lbrace)
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

// parseFile parses a Go source file with caching.
func (s *DirectiveFuncSet) parseFile(filename string) *ast.File {
	if file, ok := s.cache[filename]; ok {
		return file
	}
	file, err := parser.ParseFile(s.fset, filename, nil, parser.ParseComments)
	if err != nil {
		s.cache[filename] = nil
		return nil
	}
	s.cache[filename] = file
	return file
}

// hasDirectiveInFile checks if a function in a file has the directive.
func (s *DirectiveFuncSet) hasDirectiveInFile(file *ast.File, funcName, receiverType string) bool {
	for _, decl := range file.Decls {
		funcDecl, ok := decl.(*ast.FuncDecl)
		if !ok || funcDecl.Name.Name != funcName {
			continue
		}
		declReceiverType := ""
		if funcDecl.Recv != nil && len(funcDecl.Recv.List) > 0 {
			declReceiverType = stripPointer(exprToString(funcDecl.Recv.List[0].Type))
		}
		if declReceiverType != receiverType {
			continue
		}
		if funcDecl.Doc != nil {
			for _, c := range funcDecl.Doc.List {
				if s.isDirective(c.Text) {
					return true
				}
			}
		}
	}
	return false
}

// NewPureFuncSet creates a DirectiveFuncSet for //gormreuse:pure.
// The typesInfo parameter is used to validate that functions have *gorm.DB parameters.
func NewPureFuncSet(fset *token.FileSet, typesInfo *types.Info) *DirectiveFuncSet {
	return newDirectiveFuncSet(fset, typesInfo, IsPureDirective, hasGormDBParameter)
}

// NewImmutableReturnFuncSet creates a DirectiveFuncSet for //gormreuse:immutable-return.
// The typesInfo parameter is used to validate that functions return *gorm.DB.
func NewImmutableReturnFuncSet(fset *token.FileSet, typesInfo *types.Info) *DirectiveFuncSet {
	return newDirectiveFuncSet(fset, typesInfo, IsImmutableReturnDirective, hasGormDBReturn)
}

// BuildPureFunctionSet builds a set of functions marked with //gormreuse:pure.
func BuildPureFunctionSet(file *ast.File, pkgPath string) map[FuncKey]struct{} {
	return buildFunctionSet(file, pkgPath, IsPureDirective)
}

// BuildImmutableReturnFunctionSet builds a set of functions marked with //gormreuse:immutable-return.
func BuildImmutableReturnFunctionSet(file *ast.File, pkgPath string) map[FuncKey]struct{} {
	return buildFunctionSet(file, pkgPath, IsImmutableReturnDirective)
}

// =============================================================================
// Common Helper
// =============================================================================

// buildFunctionSet builds a set of functions matching the given directive checker.
func buildFunctionSet(file *ast.File, pkgPath string, isDirective func(string) bool) map[FuncKey]struct{} {
	result := make(map[FuncKey]struct{})

	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.FuncDecl:
			if node.Doc != nil {
				for _, c := range node.Doc.List {
					if isDirective(c.Text) {
						key := FuncKey{
							PkgPath:  pkgPath,
							FuncName: node.Name.Name,
						}
						if node.Recv != nil && len(node.Recv.List) > 0 {
							key.ReceiverType = stripPointer(exprToString(node.Recv.List[0].Type))
						}
						result[key] = struct{}{}
						break
					}
				}
			}
		}
		return true
	})

	return result
}
