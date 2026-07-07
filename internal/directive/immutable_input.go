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
// This directive declares that a callback parameter (named name) of a function
// receives an immutable *gorm.DB — the function is a user-defined equivalent of
// gorm's Transaction/Connection/FindInBatches. Inside such a callback, the
// *gorm.DB parameter can be reused without triggering Phase 1b reuse detection,
// and the declaring function's body must hand the callback an immutable value.

// ImmutableInputCallback identifies a single callback parameter that receives an
// immutable *gorm.DB.
type ImmutableInputCallback struct {
	ParamIdx  int    // Index of the callback parameter (counting from the first non-receiver parameter)
	ParamName string // The name as written in immutable-input(name) — kept for diagnostics
}

// ImmutableInputUnused describes a directive whose target is not a usable
// callback. It corresponds to the U1–U3 cases in issue #56/#62.
type ImmutableInputUnused struct {
	Pos    token.Pos // Position of the directive comment
	Reason string    // Human-readable reason for reporting
}

// ImmutableInputSet stores all //gormreuse:immutable-input(name) directives found
// in the analyzed source, keyed by the enclosing function.
type ImmutableInputSet struct {
	known  map[FuncKey][]ImmutableInputCallback
	unused []ImmutableInputUnused

	fset      *token.FileSet
	typesInfo *types.Info
}

// NewImmutableInputSet creates an empty set tied to the pass's FileSet and
// TypesInfo.
func NewImmutableInputSet(fset *token.FileSet, typesInfo *types.Info) *ImmutableInputSet {
	return &ImmutableInputSet{
		known:     make(map[FuncKey][]ImmutableInputCallback),
		fset:      fset,
		typesInfo: typesInfo,
	}
}

// AddFile scans an AST file for `//gormreuse:immutable-input(name)` directives,
// validates each against the surrounding function's signature, and either
// registers a usable callback or records an unused-directive diagnostic.
func (s *ImmutableInputSet) AddFile(file *ast.File, pkgPath string) {
	if s == nil || file == nil {
		return
	}
	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Doc == nil {
			continue
		}
		refs := s.extractParamsFromComments(fd.Doc.List)
		if len(refs) == 0 {
			continue
		}
		key := s.buildFuncKey(fd, pkgPath)
		s.processDirectiveTargets(fd, refs, key)
	}
}

type paramRef struct {
	commentPos token.Pos
	name       string
}

// extractParamsFromComments collects (commentPos, paramName) tuples for every
// immutable-input(name) directive in the comment list.
func (s *ImmutableInputSet) extractParamsFromComments(list []*ast.Comment) []paramRef {
	var refs []paramRef
	for _, c := range list {
		for _, name := range ExtractImmutableInputParams(c.Text) {
			refs = append(refs, paramRef{commentPos: c.Pos(), name: name})
		}
	}
	return refs
}

// processDirectiveTargets validates each (commentPos, paramName) against the
// function's signature, registering usable callbacks in s.known and pushing
// unused-diagnostic entries into s.unused otherwise.
func (s *ImmutableInputSet) processDirectiveTargets(fd *ast.FuncDecl, refs []paramRef, key FuncKey) {
	for _, ref := range refs {
		idx, paramType := s.lookupParam(fd, ref.name)
		if idx < 0 {
			s.unused = append(s.unused, ImmutableInputUnused{
				Pos:    ref.commentPos,
				Reason: fmt.Sprintf("unused gormreuse:immutable-input directive: parameter %q not found", ref.name),
			})
			continue
		}
		funcSig := asFunctionSignature(paramType)
		if funcSig == nil {
			s.unused = append(s.unused, ImmutableInputUnused{
				Pos:    ref.commentPos,
				Reason: fmt.Sprintf("unused gormreuse:immutable-input directive: parameter %q is not a function type", ref.name),
			})
			continue
		}
		if !hasGormDBParameter(funcSig) {
			s.unused = append(s.unused, ImmutableInputUnused{
				Pos:    ref.commentPos,
				Reason: fmt.Sprintf("unused gormreuse:immutable-input directive: callback %q has no *gorm.DB parameter", ref.name),
			})
			continue
		}
		s.known[key] = append(s.known[key], ImmutableInputCallback{ParamIdx: idx, ParamName: ref.name})
	}
}

// lookupParam finds a parameter by name in the function declaration and returns
// (index counting from the first non-receiver parameter, recorded type). Returns
// idx=-1 if not found.
func (s *ImmutableInputSet) lookupParam(fd *ast.FuncDecl, name string) (int, types.Type) {
	if fd.Type == nil || fd.Type.Params == nil {
		return -1, nil
	}
	idx := 0
	for _, field := range fd.Type.Params.List {
		// A field with no names still occupies one parameter position but cannot
		// match any name.
		if len(field.Names) == 0 {
			idx++
			continue
		}
		for _, ident := range field.Names {
			if ident.Name == name {
				var t types.Type
				if s.typesInfo != nil {
					t = s.typesInfo.TypeOf(field.Type)
				}
				return idx, t
			}
			idx++
		}
	}
	return -1, nil
}

// asFunctionSignature returns the *types.Signature for a parameter that is itself
// a function, or nil if it isn't (or if type info is unavailable — an honest U2
// in that degraded case rather than a misleading U3).
func asFunctionSignature(t types.Type) *types.Signature {
	if t == nil {
		return nil
	}
	sig, _ := t.Underlying().(*types.Signature)
	return sig
}

// buildFuncKey produces a FuncKey for a function declaration matching what the
// tracer/analyzer use for SSA functions.
func (s *ImmutableInputSet) buildFuncKey(fd *ast.FuncDecl, pkgPath string) FuncKey {
	key := FuncKey{PkgPath: pkgPath, FuncName: fd.Name.Name}
	if fd.Recv != nil && len(fd.Recv.List) > 0 {
		key.ReceiverType = stripPointer(exprToString(fd.Recv.List[0].Type))
	}
	return key
}

// Callbacks returns the registered immutable-input callbacks for fn, used to
// exempt callback arguments (case 2.2) and to validate the declaring function's
// body contract (cases 2.3/2.4).
func (s *ImmutableInputSet) Callbacks(fn *ssa.Function) []ImmutableInputCallback {
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

// GetUnused returns the diagnostics for directives that don't apply to any usable
// callback (U1–U3). U4 is the success path: nothing to report.
func (s *ImmutableInputSet) GetUnused() []ImmutableInputUnused {
	if s == nil {
		return nil
	}
	out := make([]ImmutableInputUnused, len(s.unused))
	copy(out, s.unused)
	return out
}
