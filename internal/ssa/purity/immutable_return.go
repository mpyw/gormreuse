package purity

import (
	"go/token"
	"slices"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/directive"
	"github.com/mpyw/gormreuse/internal/ssa/origin"
	"github.com/mpyw/gormreuse/internal/ssa/tracer"
	"github.com/mpyw/gormreuse/internal/typeutil"
)

// ImmutableReturnValidator enforces the body-side contract of
// //gormreuse:immutable-return: the function must actually return an immutable
// *gorm.DB — a value callers can reuse (branch) freely, exactly like a builtin
// Session()/WithContext() result.
//
// It reports only the case it can prove: a return value that is a definitively
// mutable clone==0 handle. That is a gorm chain-method call (Where, Order, …),
// e.g. db.Where(...), or Session().Where(...) whose trailing chain method
// re-forks a fresh clone==0 Statement (see the e2e TestSessionInMiddle). So is
// Session(&gorm.Session{Initialized: true}), which returns tx.getInstance()
// (#174). The check sees through the places whose code it can read (#194):
//   - a call to a closure (an IIFE, a local closure, or one a package-level
//     variable always holds, #315): its returns are checked the same way
//   - a value that a deferred closure writes to a named result
//   - an unexported package-level variable whose address does not escape and
//     whose every assignment in the package is provably mutable
//
// The tracer otherwise trusts the directive (returnsImmutable) and silently
// allows unsafe reuse of the return value at every call site, so the mismatch
// is reported at the declaration instead of being tolerated.
//
// It deliberately does NOT flag roots the tracer treats as mutable only
// conservatively — a bare *gorm.DB parameter (the caller may have isolated it),
// a call into a named user function the tracer cannot see through (e.g. an
// unmarked helper that itself returns Session()), or a Session config it cannot
// read. The directive exists to let callers assert immutability the analyzer
// cannot prove; rejecting those would turn the escape hatch into a false
// positive.
type ImmutableReturnValidator struct {
	set *directive.DirectiveFuncSet
	rt  *tracer.RootTracer
	// funcs are the package's source functions, scanned for the stores into a
	// package-level variable.
	funcs []*ssa.Function
	// globals holds the stores into each package-level variable, or nil for
	// one whose address escapes. It is filled on first use, and the copies
	// that Distrusting makes share it.
	globals *map[*ssa.Global][]ssa.Value
	// distrusted reports the functions whose results count as provably
	// mutable (see Distrusting). It is nil for the body check.
	distrusted func(*ssa.Function) bool
}

// NewImmutableReturnValidator returns a validator for the functions in set.
// funcs are the package's source functions. rt must be a tracer configured
// with the pass's full context so FindAllMutableRoots classifies immutable
// sources (including other immutable-return functions and immutable-param'd
// parameters) correctly.
func NewImmutableReturnValidator(funcs []*ssa.Function, set *directive.DirectiveFuncSet, rt *tracer.RootTracer) *ImmutableReturnValidator {
	return &ImmutableReturnValidator{set: set, rt: rt, funcs: funcs, globals: new(map[*ssa.Global][]ssa.Value)}
}

// Distrusting returns a validator like v that uses rt, a tracer that does not
// trust the functions distrusted reports, and counts a call to one of them as
// provably mutable. A function it reports keeps its contract only by relying
// on one of them (#314). The copies share the stores of package-level
// variables, which do not depend on trust.
func (v *ImmutableReturnValidator) Distrusting(rt *tracer.RootTracer, distrusted func(*ssa.Function) bool) *ImmutableReturnValidator {
	d := *v
	d.rt = rt
	d.distrusted = distrusted
	return &d
}

// Validate reports fn when it is marked //gormreuse:immutable-return and
// returns a provably mutable *gorm.DB. An inferred function is not checked:
// its body was proved immutable (#261).
func (v *ImmutableReturnValidator) Validate(fn *ssa.Function) []Violation {
	if fn == nil || v.set == nil || v.rt == nil || !v.set.Contains(fn) || v.set.Inferred(fn) {
		return nil
	}
	if !v.returnsMutable(fn, make(map[ssa.Value]bool)) {
		return nil
	}
	// One diagnostic per function: the directive, not each return, is what is
	// wrong. Report at the declaration.
	return []Violation{{
		Pos:     fn.Pos(),
		Message: "immutable-return declared but function returns mutable *gorm.DB",
	}}
}

// returnsMutable reports whether some *gorm.DB result of fn is provably
// mutable. seen holds the functions, values and variables already visited, so
// recursion through closures and variables ends.
func (v *ImmutableReturnValidator) returnsMutable(fn *ssa.Function, seen map[ssa.Value]bool) bool {
	if seen[fn] {
		return false
	}
	seen[fn] = true

	deferred := immutableReturnDeferredWrites(fn)
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			ret, ok := instr.(*ssa.Return)
			if !ok {
				continue
			}
			for _, res := range ret.Results {
				// Only *gorm.DB results carry the contract. A vacuous directive
				// (no *gorm.DB in the return, e.g. interface{}) governs nothing
				// and is handled by the unused-directive path, not here.
				if !typeutil.IsGormDB(res.Type()) {
					continue
				}
				if v.resultMutable(res, block, deferred, seen) {
					return true
				}
			}
		}
	}
	return false
}

// resultMutable reports whether the value res returned from block is provably
// mutable, including the values that deferred closures write to it when res is
// a load of a named result.
func (v *ImmutableReturnValidator) resultMutable(res ssa.Value, block *ssa.BasicBlock, deferred map[*ssa.Alloc]*immutableReturnDeferredWrite, seen map[ssa.Value]bool) bool {
	if load, ok := res.(*ssa.UnOp); ok && load.Op == token.MUL {
		if alloc, ok := load.X.(*ssa.Alloc); ok {
			if w := deferred[alloc]; w != nil {
				if slices.ContainsFunc(w.vals, func(val ssa.Value) bool { return v.valueMutable(val, seen) }) {
					return true
				}
				// A deferred closure that always overwrites the result hides
				// every value stored before it runs. The recover block, reached
				// only after the deferred calls ran, has no dominator; it gets
				// the benefit of the doubt once any such closure exists.
				if len(w.overwritingDefers) > 0 && block == block.Parent().Recover {
					return false
				}
				if slices.ContainsFunc(w.overwritingDefers, func(b *ssa.BasicBlock) bool { return b.Dominates(block) }) {
					return false
				}
			}
		}
	}
	return v.valueMutable(res, seen)
}

// valueMutable reports whether val is provably mutable. It follows Phi edges
// and package-level variables, which the tracer does not follow, and checks
// each mutable root the tracer finds.
func (v *ImmutableReturnValidator) valueMutable(val ssa.Value, seen map[ssa.Value]bool) bool {
	if seen[val] {
		return false
	}
	seen[val] = true

	switch x := val.(type) {
	case *ssa.Phi:
		return slices.ContainsFunc(x.Edges, func(edge ssa.Value) bool { return v.valueMutable(edge, seen) })
	case *ssa.UnOp:
		if g, ok := x.X.(*ssa.Global); ok && x.Op == token.MUL {
			return v.globalMutable(g, seen)
		}
	}
	return slices.ContainsFunc(v.rt.FindAllMutableRoots(val, nil), func(root ssa.Value) bool { return v.rootMutable(root, seen) })
}

// rootMutable reports whether a mutable root found by the tracer is provably
// mutable: a gorm chain-method call, a Session call that sets Initialized, or a
// call to a closure that returns a provably mutable value. Every other root
// (a parameter, a call into a named user function) is only a conservative
// guess and must not drive an immutable-return contract violation.
func (v *ImmutableReturnValidator) rootMutable(root ssa.Value, seen map[ssa.Value]bool) bool {
	call, ok := root.(*ssa.Call)
	if !ok {
		return false
	}
	site := v.rt.Site(call)
	if v.distrusted != nil {
		if callee := origin.Declared(site.Declared); callee != nil && v.distrusted(callee) {
			return true
		}
	}
	// A method value call is the call of its method.
	if site.Recv != nil {
		if !site.IsGormMethod() {
			return false
		}
		if !typeutil.IsImmutableReturningBuiltin(site.Method()) {
			return true
		}
		return site.Method() == "Session" && site.SessionSetsInitialized()
	}
	// A call to a closure, in place or through a variable that always holds
	// it, local or package-level (#315), runs the closure's body, which is
	// checked here. A named function's body is not part of the caller. A
	// closure marked immutable-return is checked on its own.
	if closure := site.Callee; closure != nil && closure.Parent() != nil && !v.set.Contains(closure) {
		return v.returnsMutable(closure, seen)
	}
	return false
}

// globalMutable reports whether the package-level variable g provably holds a
// mutable value: it is unexported, its address does not escape, it is assigned
// at least once, and every value assigned to it is provably mutable. A read
// may run before or after any assignment, so one immutable assignment gives
// the benefit of the doubt. So does an exported variable, which another
// package may assign.
func (v *ImmutableReturnValidator) globalMutable(g *ssa.Global, seen map[ssa.Value]bool) bool {
	if g.Object() == nil || g.Object().Exported() {
		return false
	}
	stores := v.globalStores(g)
	if len(stores) == 0 {
		return false
	}
	return !slices.ContainsFunc(stores, func(val ssa.Value) bool { return !v.valueMutable(val, seen) })
}

// globalStores returns the values stored into g by the package, or nil when
// g's address is used other than to load or store it.
func (v *ImmutableReturnValidator) globalStores(g *ssa.Global) []ssa.Value {
	if *v.globals == nil {
		globals := make(map[*ssa.Global][]ssa.Value)
		*v.globals = globals
		escaped := make(map[*ssa.Global]bool)
		for _, fn := range immutableReturnPackageFuncs(v.funcs, g.Pkg) {
			for _, block := range fn.Blocks {
				for _, instr := range block.Instrs {
					switch x := instr.(type) {
					case *ssa.Store:
						if sg, ok := x.Addr.(*ssa.Global); ok {
							globals[sg] = append(globals[sg], x.Val)
						}
						if sg, ok := x.Val.(*ssa.Global); ok {
							escaped[sg] = true
						}
						continue
					case *ssa.UnOp:
						if x.Op == token.MUL {
							continue
						}
					case *ssa.DebugRef:
						continue
					}
					for _, op := range instr.Operands(nil) {
						if sg, ok := (*op).(*ssa.Global); ok {
							escaped[sg] = true
						}
					}
				}
			}
		}
		for sg := range escaped {
			globals[sg] = nil
		}
	}
	return (*v.globals)[g]
}

// immutableReturnPackageFuncs returns funcs plus the package initializer, which
// runs package-level `var x = ...` initializers but is not a source function,
// and every closure nested in either.
func immutableReturnPackageFuncs(funcs []*ssa.Function, pkg *ssa.Package) []*ssa.Function {
	all := slices.Clone(funcs)
	if pkg != nil {
		if init := pkg.Func("init"); init != nil {
			all = append(all, init)
		}
	}
	seen := make(map[*ssa.Function]bool)
	var out []*ssa.Function
	for len(all) > 0 {
		fn := all[len(all)-1]
		all = all[:len(all)-1]
		if seen[fn] {
			continue
		}
		seen[fn] = true
		out = append(out, fn)
		all = append(all, fn.AnonFuncs...)
	}
	return out
}

// immutableReturnDeferredWrite is what deferred closures do to one named
// result.
type immutableReturnDeferredWrite struct {
	// vals are the values the closures store into the result.
	vals []ssa.Value
	// overwritingDefers are the blocks of the defer statements whose closure
	// stores into the result on every path. Such a store runs after every
	// return the block dominates.
	overwritingDefers []*ssa.BasicBlock
}

// immutableReturnDeferredWrites finds the stores that closures deferred in fn
// make to its captured locals, which are its named results when fn returns
// them. Only direct stores in the deferred closure are seen.
func immutableReturnDeferredWrites(fn *ssa.Function) map[*ssa.Alloc]*immutableReturnDeferredWrite {
	var writes map[*ssa.Alloc]*immutableReturnDeferredWrite
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			d, ok := instr.(*ssa.Defer)
			if !ok {
				continue
			}
			mc, ok := d.Call.Value.(*ssa.MakeClosure)
			if !ok {
				continue
			}
			closure := mc.Fn.(*ssa.Function)
			for i, binding := range mc.Bindings {
				alloc, ok := binding.(*ssa.Alloc)
				if !ok {
					continue
				}
				stores := immutableReturnFreeVarStores(closure.FreeVars[i])
				if len(stores) == 0 {
					continue
				}
				if writes == nil {
					writes = make(map[*ssa.Alloc]*immutableReturnDeferredWrite)
				}
				w := writes[alloc]
				if w == nil {
					w = &immutableReturnDeferredWrite{}
					writes[alloc] = w
				}
				for _, store := range stores {
					w.vals = append(w.vals, store.Val)
				}
				if slices.ContainsFunc(stores, immutableReturnStoreAlwaysRuns) {
					w.overwritingDefers = append(w.overwritingDefers, block)
				}
			}
		}
	}
	return writes
}

// immutableReturnFreeVarStores returns the stores into fv in its closure.
func immutableReturnFreeVarStores(fv *ssa.FreeVar) []*ssa.Store {
	var stores []*ssa.Store
	for _, ref := range *fv.Referrers() {
		if store, ok := ref.(*ssa.Store); ok && store.Addr == fv {
			stores = append(stores, store)
		}
	}
	return stores
}

// immutableReturnStoreAlwaysRuns reports whether store runs on every path
// through its function that returns normally.
func immutableReturnStoreAlwaysRuns(store *ssa.Store) bool {
	for _, block := range store.Parent().Blocks {
		if _, ok := block.Instrs[len(block.Instrs)-1].(*ssa.Return); ok && !store.Block().Dominates(block) {
			return false
		}
	}
	return true
}

// InferImmutableReturns returns the functions of candidates whose every
// *gorm.DB result is provably immutable, so that they can be treated as
// //gormreuse:immutable-return without the directive (#261). A result is
// provably immutable when it is, through Phi edges:
//   - the nil constant;
//   - a call, or a result of a call such as gorm.Open's (*gorm.DB, error),
//     whose callee is Session, WithContext, Debug, Begin or gorm.Open. A
//     Session config must be a literal that leaves Initialized unset or false
//     (callsite.Site.SessionEffect);
//   - a call to a function that trusted accepts, or to another function this
//     returns.
//
// Anything else, such as a parameter, a field, a captured variable or a call
// the rule does not prove, rules the function out. So does a function with no
// return that carries a *gorm.DB.
//
// The result is the least fixed point: a function is added only once its
// body is proved with the functions already added, so a function never
// relies on itself or on a function whose body breaks the contract, and the
// result is the same in any order. rt must resolve builtins only: a tracer
// without an immutable-return set, so that IsImmutableCall accepts only the
// gorm builtins.
func InferImmutableReturns(candidates []*ssa.Function, trusted func(*ssa.Function) bool, rt *tracer.RootTracer) map[*ssa.Function]bool {
	inferred := make(map[*ssa.Function]bool)
	accept := func(fn *ssa.Function) bool { return inferred[fn] || trusted(fn) }
	// queue holds each candidate once, as candidates may repeat.
	isCandidate := make(map[*ssa.Function]bool, len(candidates))
	var queue []*ssa.Function
	for _, fn := range candidates {
		if !isCandidate[fn] {
			isCandidate[fn] = true
			queue = append(queue, fn)
		}
	}

	// A function that candidates block waits for all of them, and is proved
	// again only once the last of them is added, so each function is proved
	// at most twice (#298). Its blockers only shrink as functions are added,
	// so that proof holds. A function that a non-candidate blocks can never
	// be proved and does not wait.
	waiting := make(map[*ssa.Function][]*ssa.Function)
	pending := make(map[*ssa.Function]int)
	for len(queue) > 0 {
		fn := queue[0]
		queue = queue[1:]
		proved, blockers := immutableReturnProof(fn, accept, rt)
		if proved {
			inferred[fn] = true
			for _, w := range waiting[fn] {
				if pending[w]--; pending[w] == 0 {
					queue = append(queue, w)
				}
			}
			delete(waiting, fn)
			continue
		}
		distinct := make(map[*ssa.Function]bool, len(blockers))
		for _, callee := range blockers {
			if !isCandidate[callee] {
				distinct = nil
				break
			}
			distinct[callee] = true
		}
		if len(distinct) == 0 {
			continue
		}
		pending[fn] = len(distinct)
		for _, callee := range blockers {
			if distinct[callee] {
				delete(distinct, callee)
				waiting[callee] = append(waiting[callee], fn)
			}
		}
	}
	return inferred
}

// immutableReturnProof reports whether every *gorm.DB result of fn is
// provably immutable when accept decides the functions it calls. When it is
// not, blockers are the callees that accept refused and that alone kept the
// proof from holding; they are nil when something else is unprovable.
func immutableReturnProof(fn *ssa.Function, accept func(*ssa.Function) bool, rt *tracer.RootTracer) (proved bool, blockers []*ssa.Function) {
	results := 0
	unprovable := false
	seen := make(map[ssa.Value]bool)
	callProved := func(call *ssa.Call) {
		if rt.IsImmutableCall(call) {
			return
		}
		callee := origin.Declared(rt.Site(call).Declared)
		switch {
		case callee == nil:
			unprovable = true
		case !accept(callee):
			blockers = append(blockers, callee)
		}
	}
	var prove func(v ssa.Value)
	prove = func(v ssa.Value) {
		if seen[v] || unprovable {
			return
		}
		seen[v] = true
		switch x := v.(type) {
		case *ssa.Phi:
			for _, edge := range x.Edges {
				prove(edge)
			}
		case *ssa.Const:
			// nil: there is no handle to reuse.
		case *ssa.Call:
			callProved(x)
		case *ssa.Extract:
			if call, ok := x.Tuple.(*ssa.Call); ok {
				callProved(call)
			} else {
				unprovable = true
			}
		default:
			unprovable = true
		}
	}
	for _, block := range fn.Blocks {
		ret, ok := block.Instrs[len(block.Instrs)-1].(*ssa.Return)
		if !ok {
			continue
		}
		for _, res := range ret.Results {
			if typeutil.IsGormDB(res.Type()) {
				results++
				prove(res)
			}
		}
	}
	if unprovable || results == 0 {
		return false, nil
	}
	return len(blockers) == 0, blockers
}
