package directive

import (
	"go/ast"
	"go/token"
	"go/types"
	"maps"
	"slices"

	"golang.org/x/tools/go/ast/inspector"
	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/ssa/origin"
	"github.com/mpyw/gormreuse/internal/typeutil"
)

// DirectiveFuncSet is a generic set of functions matching a directive.
// It supports both pre-built sets (for current package) and on-demand
// directive checking: a function of the package by its syntax, and a function
// of another package by the fact that package exported (see Import).
// It also tracks which directives have invalid signatures to report unused directives.
type DirectiveFuncSet struct {
	known map[FuncKey]struct{}
	// placement finds the function each directive comment is written on.
	placement
	typesInfo           *types.Info                 // Type info for signature validation
	imported            func(fn *ssa.Function) bool // Directive of a function without syntax (another package)
	inferred            map[*ssa.Function]bool      // Functions the analysis proved to keep the contract (#261)
	validateSignature   signatureValidator          // Checks if signature is valid for this directive
	invalidDirectives   map[token.Pos]struct{}      // Directives on functions with invalid signatures
	processedDirectives map[token.Pos]struct{}      // All directive positions processed by this set
}

// NewPureFuncSet creates a DirectiveFuncSet for //gormreuse:pure.
// The typesInfo parameter is used to validate that functions have *gorm.DB parameters.
func NewPureFuncSet(fset *token.FileSet, typesInfo *types.Info) *DirectiveFuncSet {
	return newDirectiveFuncSet(fset, typesInfo, isPureDirective, typeutil.HasGormDBParameter)
}

// NewImmutableReturnFuncSet creates a DirectiveFuncSet for //gormreuse:immutable-return.
// The typesInfo parameter is used to validate that functions return *gorm.DB.
func NewImmutableReturnFuncSet(fset *token.FileSet, typesInfo *types.Info) *DirectiveFuncSet {
	return newDirectiveFuncSet(fset, typesInfo, isImmutableReturnDirective, typeutil.HasGormDBReturn)
}

// NewImmutableParamFuncSet creates a DirectiveFuncSet for //gormreuse:immutable-param.
// Like pure, the directive is only meaningful on a function with a *gorm.DB
// parameter, so it reuses typeutil.HasGormDBParameter for signature validation (an
// immutable-param directive on a parameter-less function is reported unused).
func NewImmutableParamFuncSet(fset *token.FileSet, typesInfo *types.Info) *DirectiveFuncSet {
	return newDirectiveFuncSet(fset, typesInfo, isImmutableParamDirective, typeutil.HasGormDBParameter)
}

// newDirectiveFuncSet creates a new DirectiveFuncSet with the given directive checker and signature validator.
//
//declscope:shared // immutable_input.go builds its placement-only set with it
func newDirectiveFuncSet(fset *token.FileSet, typesInfo *types.Info, isDirective directiveChecker, validateSignature signatureValidator) *DirectiveFuncSet {
	return &DirectiveFuncSet{
		known:               make(map[FuncKey]struct{}),
		placement:           newPlacement(fset, isDirective),
		typesInfo:           typesInfo,
		validateSignature:   validateSignature,
		invalidDirectives:   make(map[token.Pos]struct{}),
		processedDirectives: make(map[token.Pos]struct{}),
	}
}

// AddFile adds an original parsed file to the set and collects all directive positions.
// Directives are only looked for in the files added with it.
// cur is the file's cursor in the pass inspector.
func (s *DirectiveFuncSet) AddFile(cur inspector.Cursor) {
	if s == nil {
		return
	}
	file, ok := s.addFile(cur)
	if !ok {
		return
	}

	// Collect all directive positions in this file for unused detection
	s.collectDirectivePositions(file)
}

// Import sets how the set decides a function that has no syntax, which is a
// function of another package: imported reports whether its package's analysis
// found the directive on it and trusts it (#195). Without it, such a function
// has no directive.
func (s *DirectiveFuncSet) Import(imported func(fn *ssa.Function) bool) {
	if s != nil {
		s.imported = imported
	}
}

// Infer adds fn, a declared function of the package that has no directive,
// because the analysis proved that its body keeps the directive's contract
// (#261). Contains reports it, and nothing reports it unused.
func (s *DirectiveFuncSet) Infer(fn *ssa.Function) {
	if s.inferred == nil {
		s.inferred = make(map[*ssa.Function]bool)
	}
	s.inferred[fn] = true
}

// Inferred reports whether Contains reports fn only because Infer added it.
func (s *DirectiveFuncSet) Inferred(fn *ssa.Function) bool {
	return s != nil && fn != nil && s.inferred[origin.Declared(fn)]
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

	return slices.Collect(maps.Keys(s.invalidDirectives))
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
// fn may be a generic instance or a bound method wrapper (see origin.Declared).
func (s *DirectiveFuncSet) Contains(fn *ssa.Function) bool {
	if fn == nil {
		return false
	}
	// A generic instance or a bound method wrapper carries the directive of
	// the function it stands for (#180).
	fn = origin.Declared(fn)

	if s.Inferred(fn) {
		return true
	}

	// First, check the pre-built set (for current package)
	if s != nil && s.known != nil {
		if _, exists := s.known[funcKeyOf(fn)]; exists {
			return true
		}
	}

	// Second, check the SSA function's syntax, or the fact of a function of
	// another package
	return s.hasDirective(fn)
}

// CommentsOf returns the positions of the directive comments written on fn,
// a function of the package: each one in a declaration's doc comment, the
// one after its opening brace, or the one a closure takes. A fix that deletes
// the directive edits these comments.
func (s *DirectiveFuncSet) CommentsOf(fn *ssa.Function) []token.Pos {
	var positions []token.Pos
	switch syntax := origin.Declared(fn).Syntax().(type) {
	case *ast.FuncDecl:
		if syntax.Doc != nil {
			for _, c := range syntax.Doc.List {
				if s.isDirective(c.Text) {
					positions = append(positions, c.Pos())
				}
			}
		}
		if pos := s.findDirectiveAfterFuncDeclBrace(syntax); pos.IsValid() {
			positions = append(positions, pos)
		}
	case *ast.FuncLit:
		if pos := s.findDirectiveForFuncLit(syntax); pos.IsValid() {
			positions = append(positions, pos)
		}
	}
	return positions
}

// collectDirectivePositions scans a file for all directives and validates their signatures.
// Directives on functions with invalid signatures are added to invalidDirectives.
func (s *DirectiveFuncSet) collectDirectivePositions(file *ast.File) {
	if s == nil || s.invalidDirectives == nil {
		return
	}

	cur := s.fileCursor(file)

	// Directive positions associated with a function. Any other directive is an
	// orphan and is always invalid.
	associatedDirectives := make(map[token.Pos]bool)

	// Check FuncDecl doc comments and same-line comments
	for fdCur := range cur.Preorder(funcDeclTypes...) {
		fd := fdCur.Node().(*ast.FuncDecl)

		// Check doc comments (next-line pattern)
		if fd.Doc != nil {
			for _, c := range fd.Doc.List {
				if s.isDirective(c.Text) {
					associatedDirectives[c.Pos()] = true
					s.processedDirectives[c.Pos()] = struct{}{}
					if !s.validateFuncDeclSignature(fd) {
						s.invalidDirectives[c.Pos()] = struct{}{}
					}
				}
			}
		}

		// Check same-line pattern (after opening brace)
		if pos := s.findDirectiveAfterFuncDeclBrace(fd); pos.IsValid() {
			associatedDirectives[pos] = true
			s.processedDirectives[pos] = struct{}{}
			if !s.validateFuncDeclSignature(fd) {
				s.invalidDirectives[pos] = struct{}{}
			}
		}
	}

	// Check FuncLit directives (next-line and same-line patterns).
	//
	// A single directive position can be shared by multiple FuncLits in a
	// multi-assignment: `a, b := func(q *gorm.DB){}, func(x int){}`. The directive
	// is valid as long as AT LEAST ONE of those FuncLits has a matching signature,
	// so track validity per position and mark a position invalid only when NONE of
	// its FuncLits are valid. (Marking invalid on the first non-matching sibling
	// would spuriously flag the whole directive unused.)
	funcLitSeen := make(map[token.Pos]bool)
	funcLitValid := make(map[token.Pos]bool)
	for flCur := range cur.Preorder(funcLitTypes...) {
		fl := flCur.Node().(*ast.FuncLit)
		if pos := s.findDirectiveForFuncLit(fl); pos.IsValid() {
			associatedDirectives[pos] = true
			s.processedDirectives[pos] = struct{}{}
			funcLitSeen[pos] = true
			if s.validateFuncLitSignature(fl) {
				funcLitValid[pos] = true
			}
		}
	}
	for pos := range funcLitSeen {
		if !funcLitValid[pos] {
			s.invalidDirectives[pos] = struct{}{}
		}
	}

	// Mark all non-associated (orphan) directives as invalid
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

// hasDirective checks if an SSA function has the directive.
func (s *DirectiveFuncSet) hasDirective(fn *ssa.Function) bool {
	if fn == nil {
		return false
	}

	switch syntax := fn.Syntax().(type) {
	case *ast.FuncDecl:
		// Check Doc comments (next-line pattern)
		if syntax.Doc != nil && s.commentGroupHasDirective(syntax.Doc) {
			return true
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
	case nil:
		// A function of another package has no syntax. Its directive comes
		// from the fact its package exported, never from its source on disk:
		// that source may be missing, and only the fact says whether the
		// body keeps the contract (#195).
		return s != nil && s.imported != nil && s.imported(fn)
	}
	return false
}

// BuildPureFuncSet builds a set of functions marked with //gormreuse:pure.
// file is the file's cursor in the pass inspector.
func BuildPureFuncSet(file inspector.Cursor, pkgPath string) map[FuncKey]struct{} {
	return buildFuncSet(file, pkgPath, isPureDirective)
}

// BuildImmutableReturnFuncSet builds a set of functions marked with //gormreuse:immutable-return.
func BuildImmutableReturnFuncSet(file inspector.Cursor, pkgPath string) map[FuncKey]struct{} {
	return buildFuncSet(file, pkgPath, isImmutableReturnDirective)
}

// BuildImmutableParamFuncSet builds a set of functions marked with //gormreuse:immutable-param.
func BuildImmutableParamFuncSet(file inspector.Cursor, pkgPath string) map[FuncKey]struct{} {
	return buildFuncSet(file, pkgPath, isImmutableParamDirective)
}

// =============================================================================
// Common Helper
// =============================================================================

// buildFuncSet builds a set of functions matching the given directive checker.
func buildFuncSet(file inspector.Cursor, pkgPath string, isDirective func(string) bool) map[FuncKey]struct{} {
	result := make(map[FuncKey]struct{})

	for cur := range file.Preorder(funcDeclTypes...) {
		node := cur.Node().(*ast.FuncDecl)
		if node.Doc == nil || !slices.ContainsFunc(node.Doc.List, func(c *ast.Comment) bool { return isDirective(c.Text) }) {
			continue
		}
		key := FuncKey{
			PkgPath:  pkgPath,
			FuncName: node.Name.Name,
		}
		if node.Recv != nil && len(node.Recv.List) > 0 {
			key.ReceiverType = receiverTypeStringFromExpr(node.Recv.List[0].Type)
		}
		result[key] = struct{}{}
	}

	return result
}
