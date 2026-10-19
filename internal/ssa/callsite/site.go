// Package callsite resolves a call, go or defer instruction to the function it
// runs, its receiver and its arguments, in one place.
//
// SSA spells one source call several ways. A method value call
// (f := r.m; f(x)) calls a MakeClosure over a synthetic "m$bound" wrapper; a
// call through a package-level variable that holds one function (#183) loads
// the variable first; a function literal called in place is a MakeClosure of
// its own. A Site reads each of these as the call it stands for, so callers do
// not each repeat the pattern matching.
package callsite

import (
	"strings"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/directive"
	"github.com/mpyw/gormreuse/internal/typeutil"
)

// SiteKind is how a Site names the function it calls.
type SiteKind int

// The kinds of a Site.
const (
	// SiteStatic calls a function or method by name: f(x), r.m(x).
	SiteStatic SiteKind = iota
	// SiteFuncVar calls through a package-level variable that holds one
	// function (see FuncVars).
	SiteFuncVar
	// SiteBound calls a method value: f := r.m; f(x).
	SiteBound
	// SiteThunk calls a method expression: T.m(r, x).
	SiteThunk
	// SiteClosure calls a function literal, possibly capturing: func() {...}().
	SiteClosure
	// SiteInvoke calls an interface method: i.m(x).
	SiteInvoke
	// SiteDynamic calls any other func-typed value, whose callee is unknown.
	SiteDynamic
	// SiteBuiltin calls a Go builtin such as append or len.
	SiteBuiltin
)

// Site is one call, go or defer instruction, resolved to the function it runs.
type Site struct {
	// Instr is the instruction the Site was read from.
	Instr ssa.CallInstruction
	// Kind is how the call names its function.
	Kind SiteKind
	// Callee is the function the instruction calls: its static callee
	// (the "m$bound" wrapper for a method value, the "m$thunk" wrapper for a
	// method expression, the literal for a closure), or the function a
	// package-level variable holds. It is nil for an invoke, a builtin or any
	// other dynamic call.
	Callee *ssa.Function
	// Declared is the function the call stands for. For a method value it is
	// the declared method (directive.DeclaredFunc); otherwise it is Callee.
	// A method expression thunk and a generic instance stay as they are.
	Declared *ssa.Function
	// Recv is the receiver of a method call: the closure binding of a method
	// value, or the first argument of a static method call. It is nil when
	// Declared has no receiver parameter.
	Recv ssa.Value
	// Args are the arguments passed to Declared, the receiver excluded.
	Args []ssa.Value
}

// SiteOf reads instr as a Site. vars resolves calls through package-level
// function variables; it may be nil.
func SiteOf(instr ssa.CallInstruction, vars FuncVars) Site {
	common := instr.Common()
	s := Site{Instr: instr, Args: common.Args}
	switch v := common.Value.(type) {
	case *ssa.Builtin:
		s.Kind = SiteBuiltin
		return s
	case *ssa.MakeClosure:
		if fn, ok := v.Fn.(*ssa.Function); ok && len(v.Bindings) > 0 && isSiteWrapper(fn, "$bound") {
			s.Kind = SiteBound
			s.Callee = fn
			s.Declared = directive.DeclaredFunc(fn)
			s.Recv = v.Bindings[0]
			return s
		}
		s.Kind = SiteClosure
	case *ssa.Function:
		s.Kind = SiteStatic
		if isSiteWrapper(v, "$thunk") {
			s.Kind = SiteThunk
		}
	default:
		s.Kind = SiteDynamic
		if common.IsInvoke() {
			s.Kind = SiteInvoke
		}
	}
	s.Callee = vars.Callee(common)
	if s.Kind == SiteDynamic && s.Callee != nil {
		s.Kind = SiteFuncVar
	}
	s.Declared = s.Callee
	if s.Declared != nil && s.Declared.Signature.Recv() != nil && len(s.Args) > 0 {
		s.Recv, s.Args = s.Args[0], s.Args[1:]
	}
	return s
}

// EachSite calls visit with the Site of every call, go and defer instruction in
// funcs and their nested anonymous functions, visiting each function once.
// vars may be nil.
func EachSite(funcs []*ssa.Function, vars FuncVars, visit func(Site)) {
	WalkInstrs(funcs, func(instr ssa.Instruction) {
		if call, ok := instr.(ssa.CallInstruction); ok {
			visit(SiteOf(call, vars))
		}
	})
}

// IsGormMethod reports whether the call stands for a method with a gorm.DB
// receiver.
func (s Site) IsGormMethod() bool {
	if s.Declared == nil {
		return false
	}
	recv := s.Declared.Signature.Recv()
	return recv != nil && typeutil.IsGormDB(recv.Type())
}

// isSiteWrapper reports whether fn is a synthetic wrapper with the given name
// suffix, such as "$bound" for a method value.
func isSiteWrapper(fn *ssa.Function, suffix string) bool {
	return fn.Synthetic != "" && strings.HasSuffix(fn.Name(), suffix)
}
