package directive

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"

	"golang.org/x/tools/go/ssa"
)

// =============================================================================
// //gormreuse:immutable-input(name) Directive
// =============================================================================
//
// This directive declares that a callback parameter (named name) of a
// function receives an immutable *gorm.DB. Inside the callback, that
// parameter can be reused without triggering Phase 1 reuse detection.
//
// Builtin equivalents live in internal/typeutil/gorm.go (Transaction,
// Connection, FindInBatches). The set here covers user-defined cases.

// ImmutableInputCallback identifies a single callback parameter that
// receives an immutable *gorm.DB.
type ImmutableInputCallback struct {
	ParamIdx  int    // Index of the callback parameter in the enclosing function's signature
	ParamName string // The name as written in immutable-input(name) — kept for diagnostics
}

// ImmutableInputUnused describes a directive whose target is not a usable
// callback. It corresponds to the U1–U4 cases in Issue #56.
type ImmutableInputUnused struct {
	Pos    token.Pos // Position of the directive comment
	Reason string    // Human-readable reason for reporting
}

// ImmutableInputSet stores all //gormreuse:immutable-input(name) directives
// found in the analyzed source, keyed by the enclosing function.
type ImmutableInputSet struct {
	known     map[FuncKey][]ImmutableInputCallback
	unused    []ImmutableInputUnused
	processed map[token.Pos]struct{} // Directive positions this set has seen

	fset      *token.FileSet
	typesInfo *types.Info
}

// NewImmutableInputSet creates an empty set tied to the analysis pass's
// FileSet and TypesInfo.
func NewImmutableInputSet(fset *token.FileSet, typesInfo *types.Info) *ImmutableInputSet {
	return &ImmutableInputSet{
		known:     make(map[FuncKey][]ImmutableInputCallback),
		processed: make(map[token.Pos]struct{}),
		fset:      fset,
		typesInfo: typesInfo,
	}
}

// AddFile scans an AST file for `//gormreuse:immutable-input(name)`
// directives, validates each against the surrounding function's signature,
// and either registers a usable callback or records an unused-directive
// diagnostic.
func (s *ImmutableInputSet) AddFile(file *ast.File, pkgPath string) {
	if s == nil || file == nil {
		return
	}
	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Doc == nil {
			continue
		}
		params := s.extractParamsFromComments(fd.Doc.List)
		if len(params) == 0 {
			continue
		}
		key := s.buildFuncKey(fd, pkgPath)
		s.processDirectiveTargets(fd, params, key)
	}
}

// extractParamsFromComments collects (commentPos, paramName) tuples for
// every immutable-input(name) directive in the comment list.
func (s *ImmutableInputSet) extractParamsFromComments(list []*ast.Comment) []paramRef {
	var refs []paramRef
	for _, c := range list {
		names := ExtractImmutableInputParams(c.Text)
		if len(names) == 0 {
			continue
		}
		s.processed[c.Pos()] = struct{}{}
		for _, name := range names {
			refs = append(refs, paramRef{commentPos: c.Pos(), name: name})
		}
	}
	return refs
}

type paramRef struct {
	commentPos token.Pos
	name       string
}

// processDirectiveTargets validates each (commentPos, paramName) against
// the function's signature, registering usable callbacks in s.known and
// pushing unused-diagnostic entries into s.unused otherwise.
func (s *ImmutableInputSet) processDirectiveTargets(fd *ast.FuncDecl, refs []paramRef, key FuncKey) {
	for _, ref := range refs {
		idx, paramExpr, paramTV := s.lookupParam(fd, ref.name)
		if idx < 0 {
			s.unused = append(s.unused, ImmutableInputUnused{
				Pos:    ref.commentPos,
				Reason: fmt.Sprintf("unused gormreuse:immutable-input directive: parameter %q not found", ref.name),
			})
			continue
		}
		// Verify parameter has function type (U2)
		funcSig := asFunctionSignature(paramExpr, paramTV)
		if funcSig == nil {
			s.unused = append(s.unused, ImmutableInputUnused{
				Pos:    ref.commentPos,
				Reason: fmt.Sprintf("unused gormreuse:immutable-input directive: parameter %q is not a function type", ref.name),
			})
			continue
		}
		// Verify the callback signature has a *gorm.DB parameter (U3)
		if !hasGormDBParameter(funcSig) {
			s.unused = append(s.unused, ImmutableInputUnused{
				Pos:    ref.commentPos,
				Reason: fmt.Sprintf("unused gormreuse:immutable-input directive: callback %q has no *gorm.DB parameter", ref.name),
			})
			continue
		}
		s.known[key] = append(s.known[key], ImmutableInputCallback{
			ParamIdx:  idx,
			ParamName: ref.name,
		})
	}
}

// lookupParam finds a parameter by name in the function declaration and
// returns (index, AST type expression, recorded type). Returns idx=-1 if
// not found.
func (s *ImmutableInputSet) lookupParam(fd *ast.FuncDecl, name string) (int, ast.Expr, types.Type) {
	if fd.Type == nil || fd.Type.Params == nil {
		return -1, nil, nil
	}
	idx := 0
	for _, field := range fd.Type.Params.List {
		// A field with empty Names list still contributes one parameter
		// position, but cannot match any name.
		if len(field.Names) == 0 {
			idx++
			continue
		}
		for _, ident := range field.Names {
			if ident.Name == name {
				var tv types.Type
				if s.typesInfo != nil {
					tv = s.typesInfo.TypeOf(field.Type)
				}
				return idx, field.Type, tv
			}
			idx++
		}
	}
	return -1, nil, nil
}

// asFunctionSignature returns the *types.Signature for a parameter that
// is itself a function, or nil if it isn't. We prefer recorded type info,
// falling back to AST shape (FuncType) so the check still works in
// degraded type-info environments.
func asFunctionSignature(expr ast.Expr, t types.Type) *types.Signature {
	if t != nil {
		if sig, ok := t.Underlying().(*types.Signature); ok {
			return sig
		}
	}
	// AST fallback: only confirms it's *some* function, not its detailed signature.
	if _, ok := expr.(*ast.FuncType); ok {
		// Without types.Signature we cannot validate the param list, but
		// we want to err on the side of accepting the directive in this
		// degraded mode rather than reporting U2.
		return types.NewSignatureType(nil, nil, nil, nil, nil, false)
	}
	return nil
}

// buildFuncKey produces a FuncKey for a function declaration that matches
// what tracer/handler will use for SSA functions.
func (s *ImmutableInputSet) buildFuncKey(fd *ast.FuncDecl, pkgPath string) FuncKey {
	key := FuncKey{
		PkgPath:  pkgPath,
		FuncName: fd.Name.Name,
	}
	if fd.Recv != nil && len(fd.Recv.List) > 0 {
		key.ReceiverType = stripPointer(exprToString(fd.Recv.List[0].Type))
	}
	return key
}

// CallbackParamIdx returns the callback parameter index registered for fn,
// or -1 if fn has no immutable-input directive.
func (s *ImmutableInputSet) CallbackParamIdx(fn *ssa.Function) int {
	if s == nil || fn == nil {
		return -1
	}
	key := FuncKey{FuncName: fn.Name()}
	if fn.Pkg != nil && fn.Pkg.Pkg != nil {
		key.PkgPath = fn.Pkg.Pkg.Path()
	}
	if sig := fn.Signature; sig != nil && sig.Recv() != nil {
		key.ReceiverType = formatReceiverType(sig.Recv().Type())
	}
	cbs := s.known[key]
	if len(cbs) == 0 {
		return -1
	}
	// One directive per (function, param) is enough for the tracer; if
	// multiple are declared, prefer the first.
	return cbs[0].ParamIdx
}

// AllCallbacks returns the registered callbacks for fn, used by the
// purity-style validator that checks 2.3/2.4.
func (s *ImmutableInputSet) AllCallbacks(fn *ssa.Function) []ImmutableInputCallback {
	if s == nil || fn == nil {
		return nil
	}
	key := FuncKey{FuncName: fn.Name()}
	if fn.Pkg != nil && fn.Pkg.Pkg != nil {
		key.PkgPath = fn.Pkg.Pkg.Path()
	}
	if sig := fn.Signature; sig != nil && sig.Recv() != nil {
		key.ReceiverType = formatReceiverType(sig.Recv().Type())
	}
	out := make([]ImmutableInputCallback, len(s.known[key]))
	copy(out, s.known[key])
	return out
}

// IsProcessed reports whether the directive at pos has already been seen
// by this set (used to avoid duplicating "unused" reports across sets).
func (s *ImmutableInputSet) IsProcessed(pos token.Pos) bool {
	if s == nil {
		return false
	}
	_, ok := s.processed[pos]
	return ok
}

// GetUnused returns the diagnostics for directives that don't apply to
// any usable callback (U1–U3). U4 is the success path: nothing to report.
func (s *ImmutableInputSet) GetUnused() []ImmutableInputUnused {
	if s == nil {
		return nil
	}
	out := make([]ImmutableInputUnused, len(s.unused))
	copy(out, s.unused)
	return out
}
