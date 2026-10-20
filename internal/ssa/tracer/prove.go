package tracer

import (
	"go/token"
	"go/types"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/effort"
	"github.com/mpyw/gormreuse/internal/ssa/callsite"
	"github.com/mpyw/gormreuse/internal/typeutil"
)

// ProvesImmutable reports whether every value that v, a *gorm.DB, may hold is
// known to be immutable (#371). The inferences of immutable-param and
// immutable-input ask it. Unlike FindMutableRoot, a value the tracer cannot
// follow is not immutable here. A channel receive, a map or slice element, a
// type assertion of an unknown interface, a field or a pointee read through
// a parameter all prove nothing.
//
// The immutable values are:
//   - nil;
//   - the result of an immutable-returning gorm builtin, such as Session or
//     gorm.Open (also through the Extract of its tuple), or of an
//     immutable-return function whose result reads no parameter;
//   - the parameter of a Transaction or immutable-input callback;
//   - the parameter of a function that inferred reports. A declared
//     immutable-param function does not count: its callers are checked only
//     while it branches the parameter, so the parameter may hold a mid-chain
//     value;
//   - a merge, a conversion or a captured value of immutable values;
//   - a load of a local variable whose address goes only to loads, stores
//     and closures, when every value stored into it, also from a closure, is
//     immutable;
//   - a load of a package-level variable of globals, when every value the
//     package stores into it is immutable (see ProvenGlobalsOf).
func (t *RootTracer) ProvesImmutable(v ssa.Value, inferred func(*ssa.Function) bool, globals ProvenGlobals) bool {
	p := &immutableProver{t: t, inferred: inferred, globals: globals, visited: make(map[ssa.Value]bool)}
	return p.proves(v)
}

// ProvesImmutableArgs reports whether call, a call of fn, passes values
// proved immutable (see ProvesImmutable) at every *gorm.DB parameter of fn,
// not counting a receiver. The immutable-param inference asks it of every
// call, and so does the fix before it suggests the directive (#387).
func (t *RootTracer) ProvesImmutableArgs(fn *ssa.Function, call ssa.CallInstruction, inferred func(*ssa.Function) bool, globals ProvenGlobals) bool {
	params := fn.Signature.Params()
	site := t.Site(call)
	for i, arg := range call.Common().Args {
		p := site.ArgParam(i)
		if p < 0 || p >= params.Len() || !typeutil.IsGormDB(params.At(p).Type()) {
			continue
		}
		if !t.ProvesImmutable(arg, inferred, globals) {
			return false
		}
	}
	return true
}

// ProvenGlobals maps a package-level *gorm.DB variable of the package to the
// values that the package stores into it. A variable that the package never
// stores into, or whose address it uses for anything but a load or a store,
// is not in it.
type ProvenGlobals map[*ssa.Global][]ssa.Value

// ProvenGlobalsOf reads the stores into the package-level *gorm.DB
// variables in funcs and their closures, which should hold every function of
// the package, its initializer included. A store in another package is not
// seen, so an exported variable that another package assigns a mid-chain
// value is a limitation.
func ProvenGlobalsOf(funcs []*ssa.Function) ProvenGlobals {
	stores := make(ProvenGlobals)
	escaped := make(map[*ssa.Global]bool)
	callsite.WalkInstrs(funcs, func(instr ssa.Instruction) {
		for _, op := range instr.Operands(nil) {
			g, ok := (*op).(*ssa.Global)
			if !ok || !typeutil.IsGormDB(g.Type().(*types.Pointer).Elem()) {
				continue
			}
			switch instr := instr.(type) {
			case *ssa.Store:
				if instr.Addr == g && instr.Val != g {
					stores[g] = append(stores[g], instr.Val)
					continue
				}
			case *ssa.UnOp:
				if instr.Op == token.MUL {
					continue
				}
			}
			escaped[g] = true
		}
	})
	for g := range escaped {
		delete(stores, g)
	}
	return stores
}

// immutableProver is the state of one ProvesImmutable search.
type immutableProver struct {
	t        *RootTracer
	inferred func(*ssa.Function) bool
	globals  ProvenGlobals
	// visited holds the values the search has entered. A value entered again
	// is on a cycle, through a loop Phi or a variable stored its own value,
	// and adds nothing: the other values on the cycle decide.
	visited map[ssa.Value]bool
}

// proves reports whether every value v may hold is immutable.
func (p *immutableProver) proves(v ssa.Value) bool {
	if v == nil {
		return false
	}
	if p.visited[v] {
		return true
	}
	p.visited[v] = true
	effort.Step()
	switch v := v.(type) {
	case *ssa.Const:
		return true
	case *ssa.Parameter:
		return p.provesParam(v)
	case *ssa.Call:
		return p.t.isImmutableSource(v)
	case *ssa.Extract:
		call, ok := v.Tuple.(*ssa.Call)
		return ok && p.t.isImmutableSource(call)
	case *ssa.Phi:
		for _, edge := range v.Edges {
			if !p.proves(edge) {
				return false
			}
		}
		return true
	case *ssa.ChangeType:
		return p.proves(v.X)
	case *ssa.MakeInterface:
		return p.proves(v.X)
	case *ssa.TypeAssert:
		return !v.CommaOk && p.proves(v.X)
	case *ssa.FreeVar:
		// A value captured by a closure.
		return p.proves(callsite.FreeVarBinding(v))
	case *ssa.UnOp:
		if v.Op != token.MUL {
			return false
		}
		switch addr := v.X.(type) {
		case *ssa.Alloc:
			return p.provesVar(addr)
		case *ssa.FreeVar:
			alloc, ok := freeVarAlloc(addr)
			return ok && p.provesVar(alloc)
		case *ssa.Global:
			stores, ok := p.globals[addr]
			if !ok {
				return false
			}
			for _, val := range stores {
				if !p.proves(val) {
					return false
				}
			}
			return true
		}
		return false
	default:
		return false
	}
}

// provesParam reports whether the *gorm.DB parameter param is immutable at
// every call: the parameter of a Transaction or immutable-input callback, or
// of an inferred immutable-param function.
func (p *immutableProver) provesParam(param *ssa.Parameter) bool {
	if !typeutil.IsGormDB(param.Type()) {
		return false
	}
	fn := param.Parent()
	return p.t.isExemptCallback(fn) || p.inferred != nil && p.inferred(fn)
}

// provesVar reports whether every value stored into the variable at addr, a
// local Alloc or the FreeVar of a closure that captures it, is immutable,
// and the address goes nowhere but to loads, stores and closures. The
// stores are read wherever they are, before or after the load, so a closure
// that reassigns the variable counts even when it runs before the load.
func (p *immutableProver) provesVar(addr ssa.Value) bool {
	if p.visited[addr] {
		return true
	}
	p.visited[addr] = true
	refs := addr.Referrers()
	if refs == nil {
		return false
	}
	for _, ref := range *refs {
		switch ref := ref.(type) {
		case *ssa.DebugRef:
		case *ssa.UnOp:
			if ref.Op != token.MUL {
				return false
			}
		case *ssa.Store:
			if ref.Addr != addr || !p.proves(ref.Val) {
				return false
			}
		case *ssa.MakeClosure:
			closure, ok := ref.Fn.(*ssa.Function)
			if !ok {
				return false
			}
			for i, b := range ref.Bindings {
				if b == addr && (i >= len(closure.FreeVars) || !p.provesVar(closure.FreeVars[i])) {
					return false
				}
			}
		default:
			return false
		}
	}
	return true
}
