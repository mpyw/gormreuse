package tracer

import (
	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/facts"
	"github.com/mpyw/gormreuse/internal/ssa/callsite"
	"github.com/mpyw/gormreuse/internal/ssa/origin"
	"github.com/mpyw/gormreuse/internal/typeutil"
)

// PureVerdicts is what validating the //gormreuse:pure functions found. The
// purity package fills it before the reuse analysis runs.
type PureVerdicts struct {
	// Failed lists the pure functions whose bodies definitively break the
	// contract (pollute or leak an argument). They must NOT be trusted as pure
	// at call sites, since that would hide reuse of the value they leak (#66).
	Failed map[*ssa.Function]bool

	// Returned lists, for each pure function that keeps the contract, the
	// indexes into fn.Params of the parameters its result may be (#188). A
	// pure function does not pollute its argument, but `return db` hands the
	// same *gorm.DB back, so a branch from the result is a branch from the
	// argument.
	Returned map[*ssa.Function][]int

	// Imported holds the verdicts that other packages exported for their pure
	// functions (#195). Failed and Returned hold only this package's.
	Imported *facts.Table
}

// HasFailed reports whether fn is a pure function that broke its contract. The
// verdicts are keyed by the declared function, and fn may be a generic instance
// or a bound method wrapper of it (#180).
func (p *PureVerdicts) HasFailed(fn *ssa.Function) bool {
	if p == nil {
		return false
	}
	declared := origin.Declared(fn)
	return p.Failed[declared] || p.Imported.Of(declared).PureFailed
}

// ReturnedArgs returns the arguments of a call to fn (args, as in
// CallCommon.Args) that its result may be. A bound method wrapper (f := x.M;
// f(a)) takes no receiver argument, so the indexes are shifted by the
// parameters it lacks; the receiver itself is never in Returned. The count of
// parameters comes from the signature, since a function of another package
// has no Params.
func (p *PureVerdicts) ReturnedArgs(fn *ssa.Function, args []ssa.Value) []ssa.Value {
	if p == nil {
		return nil
	}
	declared := origin.Declared(fn)
	indexes := p.Returned[declared]
	if len(indexes) == 0 {
		indexes = p.Imported.Of(declared).Returned
	}
	if len(indexes) == 0 {
		return nil
	}
	nparams := declared.Signature.Params().Len()
	if declared.Signature.Recv() != nil {
		nparams++
	}
	shift := nparams - len(args)
	var returned []ssa.Value
	for _, i := range indexes {
		if j := i - shift; j >= 0 && j < len(args) {
			returned = append(returned, args[j])
		}
	}
	return returned
}

// IsPureFunction checks if a function is marked as pure (doesn't pollute arguments).
//
// A function is pure if:
//   - It's a builtin pure method (Session, WithContext, Debug, Open, Begin, Transaction)
//   - It's marked with //gormreuse:pure directive
//
// Pure functions don't pollute their *gorm.DB arguments.
// Note: User-defined pure functions may return mutable values - only builtin pure
// methods are guaranteed to return immutable values.
func (t *RootTracer) IsPureFunction(fn *ssa.Function) bool {
	if fn == nil {
		return false
	}
	if t.isImmutableReturningBuiltin(fn) {
		return true
	}
	// A //gormreuse:pure function that failed its own contract validation is
	// NOT trusted here: its callers must see the leak it hides.
	if t.pure.HasFailed(fn) {
		return false
	}
	return t.pureFuncs.Contains(fn)
}

// pureReturnedArgs returns the arguments of a call to a trusted
// //gormreuse:pure function that its *gorm.DB result may be (#188), or nil.
//
// The result is traced to these arguments first, so `r := pReturn(q)` makes r
// a branch of q's root. When none of them has a mutable root (they are all
// immutable, or the tracer cannot follow them), the result is immutable only
// if the function returns nothing else (pureReturnsOnlyParams). Otherwise the call
// stays its own root: the function may also return a fresh mutable chain.
//
// A tuple result such as (*gorm.DB, error) follows the same rule (#176): its
// *gorm.DB elements share the call as their root, so they are traced to the
// returned arguments together.
//
//declscope:shared // root.go's traceCall and traceAll follow a pure call to its arguments with it
func (t *RootTracer) pureReturnedArgs(site callsite.Site) []ssa.Value {
	if !typeutil.HasGormDBResult(site.Instr.(*ssa.Call).Type()) {
		return nil
	}
	// A call through a method value passes its receiver as a binding, not
	// as an argument; put it back so a pure method returning it is followed.
	args := site.Args
	if site.Recv != nil {
		args = append([]ssa.Value{site.Recv}, args...)
	}
	args = t.pure.ReturnedArgs(site.Declared, args)
	if len(args) == 0 || !t.IsPureFunction(site.Declared) {
		return nil
	}
	return args
}

// PureReturnsOnlyParams reports whether every mutable root of the values fn
// returns is one of fn's own parameters. The analyzer exports it for the pure
// functions that may return an argument (#195).
func (t *RootTracer) PureReturnsOnlyParams(fn *ssa.Function) bool {
	return t.pureReturnsOnlyParams(fn)
}

// pureReturnsOnlyParams reports whether every mutable root of the values fn
// returns is one of fn's own parameters, so fn returns an immutable value when
// its arguments are immutable. `if c { return db.Session(s).Where("x") };
// return db` also returns a fresh chain, so it does not.
//
//declscope:shared // root.go's traceCall decides a pure call with immutable arguments with it
func (t *RootTracer) pureReturnsOnlyParams(fn *ssa.Function) bool {
	// An instance or a bound method wrapper runs the declared body.
	fn = origin.Declared(fn)
	if only, ok := t.onlyParams[fn]; ok {
		return only
	}
	// The body of a function of another package is not built, so its package
	// decided (#195).
	if fn.Blocks == nil {
		return t.pure != nil && t.pure.Imported.Of(fn).ReturnsOnlyParams
	}
	// A recursive call may return anything until fn is decided.
	t.onlyParams[fn] = false
	only := true
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			ret, ok := instr.(*ssa.Return)
			if !ok {
				continue
			}
			// Every *gorm.DB result counts, in any position of a tuple (#176).
			for _, res := range ret.Results {
				if !typeutil.IsGormDB(res.Type()) {
					continue
				}
				for _, root := range t.traceAll(res, make(map[ssa.Value]bool), nil) {
					if p, isParam := root.(*ssa.Parameter); !isParam || p.Parent() != fn {
						only = false
					}
				}
			}
		}
	}
	t.onlyParams[fn] = only
	return only
}
