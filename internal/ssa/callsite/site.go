// Package callsite resolves a call, go or defer instruction to the function it
// runs, its receiver and its arguments, in one place.
//
// SSA spells one source call several ways. A method value call
// (f := r.m; f(x)) calls a MakeClosure over a synthetic "m$bound" wrapper; a
// method expression (T.m(r, x)) calls a synthetic "m$thunk" wrapper that takes
// the receiver as its first argument; a call through a package-level variable
// that holds one function (#183), or through a local one that a closure
// captures (#304), loads the variable first; a function literal called in
// place is a MakeClosure of its own. A Site reads each of these as the call it
// stands for, so callers do not each repeat the pattern matching.
package callsite

import (
	"strings"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/ssa/origin"
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

// SiteMode is when a Site runs: where it is, in a new goroutine, or when the
// function returns.
type SiteMode int

// The modes of a Site.
const (
	// SiteCall is a plain call: it runs where it is.
	SiteCall SiteMode = iota
	// SiteGo is a go statement: the call runs in a new goroutine.
	SiteGo
	// SiteDefer is a defer statement: the call runs when the function returns.
	SiteDefer
)

// Site is one call, go or defer instruction, resolved to the function it runs.
type Site struct {
	// Instr is the instruction the Site was read from.
	Instr ssa.CallInstruction
	// Mode is the kind of instruction: a call, a go or a defer.
	Mode SiteMode
	// Kind is how the call names its function.
	Kind SiteKind
	// Callee is the function the instruction calls: its static callee
	// (the "m$bound" wrapper for a method value, the "m$thunk" wrapper for a
	// method expression, the literal for a closure), or the function a
	// package-level variable holds. It is nil for an invoke, a builtin or any
	// other dynamic call.
	Callee *ssa.Function
	// Declared is the function the call stands for. For a method value or a
	// method expression it is the declared method (origin.Declared);
	// otherwise it is Callee. A generic instance stays as it is. The wrapper
	// of an interface method has no declared method, so it stays too.
	Declared *ssa.Function
	// Recv is the receiver of a method call: the closure binding of a method
	// value, or the first argument of a static method call or a method
	// expression. It is nil when Declared has no receiver parameter.
	Recv ssa.Value
	// Args are the arguments passed to Declared, the receiver excluded.
	Args []ssa.Value
}

// SiteOf reads instr as a Site. vars resolves calls through package-level
// and local function variables: a call through a local variable is read as
// the call of the value it holds (FuncVars.LocalValue). Pass the FuncVars of
// the package, so a call through such a variable lines up its receiver and
// arguments as the checker does (#293); a read that cannot reach them uses
// SiteOfWithoutFuncVars instead.
func SiteOf(instr ssa.CallInstruction, vars FuncVars) Site {
	common := instr.Common()
	s := Site{Instr: instr, Mode: siteModeOf(instr), Args: common.Args}
	if common.IsInvoke() {
		s.Kind = SiteInvoke
		return s
	}
	// A call through a local variable assigned once is the call of the value
	// it holds (#304).
	switch v := vars.LocalValue(common.Value).(type) {
	case *ssa.Builtin:
		s.Kind = SiteBuiltin
		return s
	case *ssa.MakeClosure:
		if bound, ok := BoundSite(v); ok {
			bound.Instr, bound.Mode, bound.Args = instr, s.Mode, common.Args
			return bound
		}
		s.Kind, s.Callee = SiteClosure, v.Fn.(*ssa.Function)
	case *ssa.Function:
		s.Kind, s.Callee = SiteStatic, v
		if isSiteWrapper(v, "$thunk") {
			s.Kind = SiteThunk
		}
	default:
		s.Kind = SiteDynamic
		if s.Callee = vars.globalFuncVar(v); s.Callee != nil {
			s.Kind = SiteFuncVar
		}
	}
	s.Declared = s.Callee
	if s.Kind == SiteThunk || s.Kind == SiteFuncVar && isSiteWrapper(s.Callee, "$thunk") {
		// A method expression, called in place or through a variable that
		// holds it.
		s.Declared = origin.Declared(s.Callee)
	}
	if s.Declared != nil && s.Declared.Signature.Recv() != nil && len(s.Args) > 0 {
		s.Recv, s.Args = s.Args[0], s.Args[1:]
	}
	return s
}

// SiteOfWithoutFuncVars reads instr as a Site without resolving calls through
// package-level or local function variables: such a call is a SiteDynamic
// with no Callee. It is for reads that look only for a *gorm.DB method or a
// library function called by name, where the FuncVars are not at hand. Every
// other read calls SiteOf with the package's FuncVars; a test keeps SiteOf
// from being called with a zero FuncVars literal outside tests.
func SiteOfWithoutFuncVars(instr ssa.CallInstruction) Site {
	return SiteOf(instr, FuncVars{})
}

// siteModeOf returns the mode of instr.
func siteModeOf(instr ssa.CallInstruction) SiteMode {
	switch instr.(type) {
	case *ssa.Go:
		return SiteGo
	case *ssa.Defer:
		return SiteDefer
	}
	return SiteCall
}

// BoundSite reads the method value mc (r.m used as a value) as the Site of a
// call to it, without Instr and Args. ok is false when mc is a function
// literal.
func BoundSite(mc *ssa.MakeClosure) (s Site, ok bool) {
	fn, isFunc := mc.Fn.(*ssa.Function)
	if !isFunc || len(mc.Bindings) == 0 || !isSiteWrapper(fn, "$bound") {
		return Site{}, false
	}
	return Site{Kind: SiteBound, Callee: fn, Declared: origin.Declared(fn), Recv: mc.Bindings[0]}, true
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

// Method returns the name of the function or method the call stands for,
// such as "Find" for a call of the method value db.Find. It is empty when
// Declared is nil.
func (s Site) Method() string {
	switch {
	case s.Kind == SiteBound || s.Callee != nil && isSiteWrapper(s.Callee, "$thunk"):
		// The wrapper's object, not Declared: the wrapper of an interface
		// method value or expression has no declared method to stand for.
		return s.Callee.Object().Name()
	case s.Declared != nil:
		return s.Declared.Name()
	}
	return ""
}

// ArgParam returns the index into Args of the instruction's argument i, the
// index of the parameter of Declared it is passed to. It is -1 for the
// receiver.
func (s Site) ArgParam(i int) int {
	return i - (len(s.Instr.Common().Args) - len(s.Args))
}

// isSiteWrapper reports whether fn is a synthetic wrapper with the given name
// suffix, such as "$bound" for a method value.
func isSiteWrapper(fn *ssa.Function, suffix string) bool {
	return fn.Synthetic != "" && strings.HasSuffix(fn.Name(), suffix)
}
