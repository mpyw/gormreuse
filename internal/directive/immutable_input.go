package directive

import (
	"cmp"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"slices"
	"strings"

	"golang.org/x/tools/go/ast/inspector"
	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/ssa/origin"
	"github.com/mpyw/gormreuse/internal/typeutil"
)

// =============================================================================
// //gormreuse:immutable-input(name) Directive
// =============================================================================
//
// This directive declares that a callback parameter (named name) of a function
// receives an immutable *gorm.DB — the function is a user-defined equivalent of
// gorm's Transaction/FindInBatches. Inside such a callback, the
// *gorm.DB parameter can be reused without triggering Phase 1b reuse detection,
// and the declaring function's body must hand the callback an immutable value.

// ImmutableInputCallback identifies a single callback parameter that receives an
// immutable *gorm.DB.
type ImmutableInputCallback struct {
	ParamIdx  int    // Index of the callback parameter (counting from the first non-receiver parameter)
	ParamName string // The name as written in immutable-input(name) — kept for diagnostics
}

// ImmutableInputUnused describes a directive whose target is not a usable
// callback. It corresponds to the U1–U3 cases in issue #56/#62, plus a
// directive attached to no function (#193).
type ImmutableInputUnused struct {
	Pos    token.Pos // Position of the directive comment
	Reason string    // Human-readable reason for reporting
	Parts  []string  // The immutable-input(name) parts of the comment that are unused
}

// ImmutableInputSet stores all //gormreuse:immutable-input(name) directives found
// in the analyzed source, keyed by the enclosing function. A directive may sit
// where the other function directives may (#193): in a function's doc comment,
// after a function's or closure's opening brace on the same line, or on the
// line before a closure.
type ImmutableInputSet struct {
	known    map[FuncKey][]ImmutableInputCallback
	closures map[token.Pos][]ImmutableInputCallback // keyed by the FuncLit's position
	unused   []ImmutableInputUnused
	texts    map[token.Pos]string // text of each immutable-input comment, by position
	// imported returns the callbacks of a function of another package, from
	// the fact its package exported (#195). It may be nil.
	imported func(fn *ssa.Function) []ImmutableInputCallback

	fset      *token.FileSet
	typesInfo *types.Info
	// placement decides which function a directive comment belongs to, with
	// the rules pure and the other function directives use. It has no
	// signature check, so its unused list holds only comments attached to no
	// function.
	placement *DirectiveFuncSet
}

// NewImmutableInputSet creates an empty set tied to the pass's FileSet and
// TypesInfo.
func NewImmutableInputSet(fset *token.FileSet, typesInfo *types.Info) *ImmutableInputSet {
	return &ImmutableInputSet{
		known:     make(map[FuncKey][]ImmutableInputCallback),
		closures:  make(map[token.Pos][]ImmutableInputCallback),
		texts:     make(map[token.Pos]string),
		fset:      fset,
		typesInfo: typesInfo,
		placement: newDirectiveFuncSet(fset, typesInfo, hasImmutableInputDirective, nil),
	}
}

// AddFile scans an AST file for `//gormreuse:immutable-input(name)` directives,
// validates each against the surrounding function's signature, and either
// registers a usable callback or records an unused-directive diagnostic.
// cur is the file's cursor in the pass inspector.
func (s *ImmutableInputSet) AddFile(cur inspector.Cursor, pkgPath string) {
	if s == nil || !cur.Valid() {
		return
	}
	file := cur.Node().(*ast.File)
	s.placement.AddFile(cur)
	comments := make(map[token.Pos]*ast.Comment)
	for _, cg := range file.Comments {
		for _, c := range cg.List {
			comments[c.Pos()] = c
			if hasImmutableInputDirective(c.Text) {
				s.texts[c.Pos()] = c.Text
			}
		}
	}

	for fdCur := range cur.Preorder(funcDeclTypes...) {
		fd := fdCur.Node().(*ast.FuncDecl)
		var list []*ast.Comment
		if fd.Doc != nil {
			list = slices.Clone(fd.Doc.List)
		}
		if pos := s.placement.findDirectiveAfterFuncDeclBrace(fd); pos.IsValid() {
			list = append(list, comments[pos])
		}
		key := s.buildFuncKey(fd, pkgPath)
		for _, ref := range s.extractParamsFromComments(list) {
			cb, reason := s.resolveImmutableInputRef(fd.Type, ref)
			if reason != "" {
				s.addUnusedImmutableInput(ref, reason)
				continue
			}
			s.known[key] = append(s.known[key], cb)
		}
	}

	// One directive comment can cover several closures of one assignment
	// (a, b := func(...) {...}, func(...) {...}). It applies to each closure
	// whose signature fits, and is unused only when none fits.
	litsByComment := make(map[token.Pos][]*ast.FuncLit)
	var commentOrder []token.Pos
	for flCur := range cur.Preorder(funcLitTypes...) {
		fl := flCur.Node().(*ast.FuncLit)
		pos := s.placement.findDirectiveForFuncLit(fl)
		if !pos.IsValid() {
			continue
		}
		if _, seen := litsByComment[pos]; !seen {
			commentOrder = append(commentOrder, pos)
		}
		litsByComment[pos] = append(litsByComment[pos], fl)
	}
	for _, pos := range commentOrder {
		for _, ref := range s.extractParamsFromComments([]*ast.Comment{comments[pos]}) {
			applied := false
			var firstReason string
			for _, fl := range litsByComment[pos] {
				cb, reason := s.resolveImmutableInputRef(fl.Type, ref)
				if reason != "" {
					firstReason = cmp.Or(firstReason, reason)
					continue
				}
				s.closures[fl.Pos()] = append(s.closures[fl.Pos()], cb)
				applied = true
			}
			if !applied {
				s.addUnusedImmutableInput(ref, firstReason)
			}
		}
	}
}

// Import sets how the set finds the callbacks of a function that has no
// syntax, which is a function of another package (see DirectiveFuncSet.Import).
func (s *ImmutableInputSet) Import(imported func(fn *ssa.Function) []ImmutableInputCallback) {
	if s != nil {
		s.imported = imported
	}
}

// Callbacks returns the registered immutable-input callbacks for fn, used to
// exempt callback arguments (case 2.2) and to validate the declaring function's
// body contract (cases 2.3/2.4). fn may be a closure, a generic instance or a
// bound method wrapper (see origin.Declared); ParamIdx never counts a receiver,
// so it indexes the arguments of a call to any of them.
func (s *ImmutableInputSet) Callbacks(fn *ssa.Function) []ImmutableInputCallback {
	if s == nil || fn == nil {
		return nil
	}
	fn = origin.Declared(fn)
	switch syntax := fn.Syntax().(type) {
	case *ast.FuncLit:
		return slices.Clone(s.closures[syntax.Pos()])
	case nil:
		if s.imported == nil {
			return nil
		}
		return slices.Clone(s.imported(fn))
	}
	return slices.Clone(s.known[funcKeyOf(fn)])
}

// GetUnused returns the diagnostics for directives that don't apply to any usable
// callback (U1–U3), and for directives attached to no function or closure,
// which would otherwise have no effect at all (#193). U4 is the success path:
// nothing to report.
func (s *ImmutableInputSet) GetUnused() []ImmutableInputUnused {
	if s == nil {
		return nil
	}
	unused := slices.Clone(s.unused)
	orphans := s.placement.GetUnusedDirectives()
	slices.Sort(orphans)
	for _, pos := range orphans {
		var parts []string
		for _, name := range extractImmutableInputParams(s.texts[pos]) {
			parts = append(parts, immutableInputPart(name))
		}
		unused = append(unused, ImmutableInputUnused{
			Pos:    pos,
			Reason: "unused gormreuse:immutable-input directive: not attached to a function or closure",
			Parts:  parts,
		})
	}
	return unused
}

// hasImmutableInputDirective reports whether a comment carries an
// immutable-input(name) directive.
func hasImmutableInputDirective(text string) bool {
	return len(extractImmutableInputParams(text)) > 0
}

// extractImmutableInputParams returns the callback parameter names declared by
// //gormreuse:immutable-input(name) directives in a comment. A comment may carry
// several (comma-combinable with other directives), so it returns a slice; nil if
// none. It reads the comment through the same parseDirective as hasDirective,
// so only a valid //gormreuse: directive counts, a trailing "//" comment is
// ignored (#62), and a malformed immutable-input part drops the comment.
func extractImmutableInputParams(text string) []string {
	var params []string
	for _, part := range parseDirective(text) {
		if inner, ok := strings.CutPrefix(part, "immutable-input("); ok {
			params = append(params, strings.TrimSuffix(inner, ")"))
		}
	}
	return params
}

// immutableInputRef is one occurrence of an immutable-input(name) directive:
// the comment position and the parameter name it declares.
type immutableInputRef struct {
	commentPos token.Pos
	name       string
}

// extractParamsFromComments collects (commentPos, paramName) tuples for every
// immutable-input(name) directive in the comment list.
func (s *ImmutableInputSet) extractParamsFromComments(list []*ast.Comment) []immutableInputRef {
	var refs []immutableInputRef
	for _, c := range list {
		for _, name := range extractImmutableInputParams(c.Text) {
			refs = append(refs, immutableInputRef{commentPos: c.Pos(), name: name})
		}
	}
	return refs
}

// resolveImmutableInputRef validates ref against the parameters of ft. It
// returns the usable callback, or the reason the directive is unused.
func (s *ImmutableInputSet) resolveImmutableInputRef(ft *ast.FuncType, ref immutableInputRef) (ImmutableInputCallback, string) {
	idx, field := lookupImmutableInputParam(ft, ref.name)
	if idx < 0 {
		return ImmutableInputCallback{}, fmt.Sprintf("unused gormreuse:immutable-input directive: parameter %q not found", ref.name)
	}
	// Each element of a variadic parameter would be a callback, and the
	// caller packs them into a slice the collectors do not read (#259).
	if _, ok := field.Type.(*ast.Ellipsis); ok {
		return ImmutableInputCallback{}, fmt.Sprintf("unused gormreuse:immutable-input directive: parameter %q is variadic; variadic callbacks are not supported", ref.name)
	}
	var paramType types.Type
	if s.typesInfo != nil {
		paramType = s.typesInfo.TypeOf(field.Type)
	}
	funcSig := asFunctionSignature(paramType)
	if funcSig == nil {
		return ImmutableInputCallback{}, fmt.Sprintf("unused gormreuse:immutable-input directive: parameter %q is not a function type", ref.name)
	}
	if !typeutil.HasGormDBParameter(funcSig) {
		return ImmutableInputCallback{}, fmt.Sprintf("unused gormreuse:immutable-input directive: callback %q has no *gorm.DB parameter", ref.name)
	}
	return ImmutableInputCallback{ParamIdx: idx, ParamName: ref.name}, ""
}

// addUnusedImmutableInput records an unused-directive diagnostic for ref.
func (s *ImmutableInputSet) addUnusedImmutableInput(ref immutableInputRef, reason string) {
	s.unused = append(s.unused, ImmutableInputUnused{Pos: ref.commentPos, Reason: reason, Parts: []string{immutableInputPart(ref.name)}})
}

// immutableInputPart returns the directive part that declares the callback
// name: immutable-input(name).
func immutableInputPart(name string) string {
	return "immutable-input(" + name + ")"
}

// lookupImmutableInputParam finds a parameter by name in a function type and returns its
// index, counting from the first non-receiver parameter, and its field.
// Returns idx=-1 if not found.
func lookupImmutableInputParam(ft *ast.FuncType, name string) (int, *ast.Field) {
	idx := 0
	for _, field := range ft.Params.List {
		// A field with no names still occupies one parameter position but cannot
		// match any name.
		if len(field.Names) == 0 {
			idx++
			continue
		}
		for _, ident := range field.Names {
			if ident.Name == name {
				return idx, field
			}
			idx++
		}
	}
	return -1, nil
}

// buildFuncKey produces a FuncKey for a function declaration matching what the
// tracer/analyzer use for SSA functions.
func (s *ImmutableInputSet) buildFuncKey(fd *ast.FuncDecl, pkgPath string) FuncKey {
	key := FuncKey{PkgPath: pkgPath, FuncName: fd.Name.Name}
	if fd.Recv != nil && len(fd.Recv.List) > 0 {
		key.ReceiverType = receiverTypeStringFromExpr(fd.Recv.List[0].Type)
	}
	return key
}
