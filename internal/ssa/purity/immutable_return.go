package purity

import (
	"go/token"
	"go/types"
	"slices"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/directive"
	"github.com/mpyw/gormreuse/internal/ssa/callsite"
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
	// package-level variable or a struct field.
	funcs []*ssa.Function
	// assigned holds those stores. It is filled on first use, and the copies
	// that Distrusting makes share it.
	assigned *immutableReturnAssignments
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
	return &ImmutableReturnValidator{set: set, rt: rt, funcs: funcs, assigned: &immutableReturnAssignments{}}
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

// AssignedMutable reports whether read, a load of a package-level variable or
// of a struct field, reads a variable that the package may assign a provably
// mutable value. Inference does not trust such a read (#308): a caller may
// assign the chain before it calls the getter. It is true for a global whose
// address escapes. A store in another package, or through a field address
// that escapes, is not seen. Any other read has no stores.
func (v *ImmutableReturnValidator) AssignedMutable(read *ssa.UnOp) bool {
	a := v.assignments(read.Parent().Pkg)
	var stores []ssa.Value
	switch addr := read.X.(type) {
	case *ssa.Global:
		if a.escaped[addr] {
			return true
		}
		stores = a.globals[addr]
	case *ssa.FieldAddr:
		stores = a.fields[immutableReturnField(addr.X.Type(), addr.Field)]
	}
	return slices.ContainsFunc(stores, func(val ssa.Value) bool { return v.valueMutable(val, make(map[ssa.Value]bool)) })
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
	a := v.assignments(g.Pkg)
	stores := a.globals[g]
	if a.escaped[g] || len(stores) == 0 {
		return false
	}
	return !slices.ContainsFunc(stores, func(val ssa.Value) bool { return !v.valueMutable(val, seen) })
}

// immutableReturnAssignments are the stores of a package into its
// package-level variables and into struct fields.
type immutableReturnAssignments struct {
	scanned bool
	globals map[*ssa.Global][]ssa.Value
	// escaped holds the globals whose address is used other than to load or
	// store it.
	escaped map[*ssa.Global]bool
	fields  map[*types.Var][]ssa.Value
}

// assignments scans the package's functions for their stores once. pkg is
// the package whose initializer is scanned too.
func (v *ImmutableReturnValidator) assignments(pkg *ssa.Package) *immutableReturnAssignments {
	a := v.assigned
	if a.scanned {
		return a
	}
	a.scanned = true
	a.globals = make(map[*ssa.Global][]ssa.Value)
	a.escaped = make(map[*ssa.Global]bool)
	a.fields = make(map[*types.Var][]ssa.Value)
	for _, fn := range immutableReturnPackageFuncs(v.funcs, pkg) {
		for _, block := range fn.Blocks {
			for _, instr := range block.Instrs {
				switch x := instr.(type) {
				case *ssa.Store:
					switch addr := x.Addr.(type) {
					case *ssa.Global:
						a.globals[addr] = append(a.globals[addr], x.Val)
					case *ssa.FieldAddr:
						field := immutableReturnField(addr.X.Type(), addr.Field)
						a.fields[field] = append(a.fields[field], x.Val)
					}
					if sg, ok := x.Val.(*ssa.Global); ok {
						a.escaped[sg] = true
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
						a.escaped[sg] = true
					}
				}
			}
		}
	}
	return a
}

// immutableReturnField returns field i of the struct that t is or points to.
func immutableReturnField(t types.Type, i int) *types.Var {
	if ptr, ok := t.Underlying().(*types.Pointer); ok {
		t = ptr.Elem()
	}
	return t.Underlying().(*types.Struct).Field(i)
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
// //gormreuse:immutable-return without the directive (#261), each with the
// reads of its parameters that its results may be (#342). A result is
// provably immutable when it is, through Phi edges:
//   - the nil constant;
//   - a call, or a result of a call such as gorm.Open's (*gorm.DB, error),
//     whose callee is Session, WithContext, Debug, Begin or gorm.Open. A
//     Session config must be a literal that leaves Initialized unset or false
//     (callsite.Site.SessionEffect);
//   - a call to a function that trusted accepts, or to another function this
//     returns. The reads of its parameters (trustedReads, or what this found)
//     select from the arguments of the call, and every value they select
//     there is proved;
//   - a local variable, such as one a closure captures, whose every store is
//     in its function and proved (#308, #342);
//   - a field read through a parameter or a receiver, or a read of a
//     package-level variable, when every store in the function that reaches
//     the read is proved, as reuse detection reads them (#308). With no such
//     store the value is immutable. assignedMutable must not report the
//     variable: it reports one that the package may assign a provably
//     mutable value (ImmutableReturnValidator.AssignedMutable). A field read
//     through the function's own parameter is one of its reads: it holds
//     what the caller's argument holds;
//   - a type assertion of a value with no *gorm.DB root, such as
//     ctx.Value(k).(*gorm.DB), as reuse detection reads it (#342). An
//     assertion of the function's own parameter is one of its reads.
//
// Anything else, such as a *gorm.DB parameter, a field of a local struct or a
// call the rule does not prove, rules the function out. So does a function
// with no return that carries a *gorm.DB.
//
// The result is the least fixed point: a function is added only once its
// body is proved with the functions already added, so a function never
// relies on itself or on a function whose body breaks the contract, and the
// result is the same in any order. rt must resolve builtins only: a tracer
// without an immutable-return set, so that IsImmutableCall accepts only the
// gorm builtins.
func InferImmutableReturns(candidates []*ssa.Function, trusted func(*ssa.Function) bool, trustedReads func(*ssa.Function) []directive.ParamRead, rt *tracer.RootTracer, assignedMutable func(*ssa.UnOp) bool) map[*ssa.Function][]directive.ParamRead {
	inferred := make(map[*ssa.Function][]directive.ParamRead)
	accept := func(fn *ssa.Function) bool {
		_, ok := inferred[fn]
		return ok || trusted(fn)
	}
	readsOf := func(fn *ssa.Function) []directive.ParamRead {
		if reads, ok := inferred[fn]; ok {
			return reads
		}
		return trustedReads(fn)
	}
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
		p := &immutableReturnProof{fn: fn, accept: accept, readsOf: readsOf, rt: rt, assignedMutable: assignedMutable, seen: make(map[ssa.Value]bool)}
		if p.run() {
			inferred[fn] = p.reads
			for _, w := range waiting[fn] {
				if pending[w]--; pending[w] == 0 {
					queue = append(queue, w)
				}
			}
			delete(waiting, fn)
			continue
		}
		if p.unprovable {
			continue
		}
		distinct := make(map[*ssa.Function]bool, len(p.blockers))
		for _, callee := range p.blockers {
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
		for _, callee := range p.blockers {
			if distinct[callee] {
				delete(distinct, callee)
				waiting[callee] = append(waiting[callee], fn)
			}
		}
	}
	return inferred
}

// immutableReturnProof proves that every *gorm.DB result of fn is immutable
// when accept decides the functions it calls.
type immutableReturnProof struct {
	fn              *ssa.Function
	accept          func(*ssa.Function) bool
	readsOf         func(*ssa.Function) []directive.ParamRead
	rt              *tracer.RootTracer
	assignedMutable func(*ssa.UnOp) bool
	seen            map[ssa.Value]bool

	// unprovable is set when something other than a callee that accept
	// refused keeps the proof from holding.
	unprovable bool
	// blockers are the callees that accept refused.
	blockers []*ssa.Function
	// reads are the reads of fn's parameters that its results may be.
	reads []directive.ParamRead
}

// run reports whether the proof holds. When it does not, blockers are the
// callees that alone kept it from holding, unless unprovable is set.
func (p *immutableReturnProof) run() bool {
	results := 0
	for _, block := range p.fn.Blocks {
		ret, ok := block.Instrs[len(block.Instrs)-1].(*ssa.Return)
		if !ok {
			continue
		}
		for _, res := range ret.Results {
			if typeutil.IsGormDB(res.Type()) {
				results++
				p.prove(res)
			} else if paths := ImmutableReturnFieldPaths(res.Type()); len(paths) > 0 {
				results++
				p.proveStruct(res, paths, ret)
			}
		}
	}
	if results == 0 {
		p.unprovable = true
	}
	return !p.unprovable && len(p.blockers) == 0
}

// prove checks that v is immutable, adding to reads the reads of fn's
// parameters it may be.
func (p *immutableReturnProof) prove(v ssa.Value) {
	if p.seen[v] || p.unprovable {
		return
	}
	p.seen[v] = true
	switch x := v.(type) {
	case *ssa.Phi:
		for _, edge := range x.Edges {
			p.prove(edge)
		}
	case *ssa.Const:
		// nil: there is no handle to reuse.
	case *ssa.Call:
		p.proveCall(x)
	case *ssa.Extract:
		switch tuple := x.Tuple.(type) {
		case *ssa.Call:
			p.proveCall(tuple)
		case *ssa.TypeAssert:
			p.prove(tuple)
		default:
			p.unprovable = true
		}
	case *ssa.TypeAssert:
		// ctx.Value(k).(*gorm.DB) is what the interface holds (#342).
		p.prove(x.X)
	case *ssa.MakeInterface:
		p.prove(x.X)
	case *ssa.Parameter:
		// Only an interface reaches here, through a type assertion, and it
		// holds what the caller passes. A *gorm.DB parameter may be mutable.
		if !types.IsInterface(x.Type()) || x.Parent() != p.fn {
			p.unprovable = true
			return
		}
		p.reads = append(p.reads, directive.ParamRead{Param: slices.Index(p.fn.Params, x)})
	case *ssa.UnOp:
		p.proveLoad(x)
	default:
		p.unprovable = true
	}
}

// proveLoad checks a load: of a local variable, a field or a package-level
// variable.
func (p *immutableReturnProof) proveLoad(x *ssa.UnOp) {
	// A local variable assigned once, such as one a closure captures,
	// holds its stored value (#308).
	if stored := p.rt.FuncVars().LocalValue(x); stored != x {
		p.prove(stored)
		return
	}
	// One assigned more than once holds one of its stored values, when
	// every store is in its function (#342).
	if vals, ok := immutableReturnLocalStores(x); ok {
		for _, val := range vals {
			p.prove(val)
		}
		return
	}
	// A field read through a parameter, and a package-level variable, are
	// immutable where no store in the function reaches the read, as reuse
	// detection reads them (#308). A store that does reach it decides,
	// through the roots the tracer finds.
	if !immutableReturnReadsShared(x) || p.assignedMutable(x) {
		p.unprovable = true
		return
	}
	for _, root := range p.rt.FindAllMutableRoots(x, nil) {
		if call, ok := root.(*ssa.Call); ok {
			p.proveCall(call)
		} else {
			p.unprovable = true
		}
	}
	// A field read through fn's own parameter holds what the caller's
	// argument holds there (#342).
	if param, path, ok := immutableReturnSelector(x); ok && param.Parent() == p.fn {
		p.reads = append(p.reads, directive.ParamRead{Param: slices.Index(p.fn.Params, param), Path: path})
	}
}

// proveCall checks a call: an immutable builtin, or a callee that accept
// takes, whose parameter reads select proved values at the call.
func (p *immutableReturnProof) proveCall(call *ssa.Call) {
	if p.rt.IsImmutableCall(call) {
		return
	}
	// A call that returns no *gorm.DB, such as ctx.Value(k), has no root,
	// as reuse detection reads it (#342). Only a type assertion reaches it.
	if !typeutil.HasGormDBResult(call.Type()) {
		return
	}
	site := p.rt.Site(call)
	callee := origin.Declared(site.Declared)
	switch {
	case callee == nil:
		p.unprovable = true
	case !p.accept(callee):
		p.blockers = append(p.blockers, callee)
	default:
		for _, read := range p.readsOf(callee) {
			p.proveRead(site, read)
		}
	}
}

// proveRead checks what a callee's parameter read selects at site: the
// values stored there that reach the call, and, when the argument is
// selected from fn's own parameter, the read of it that the call makes.
func (p *immutableReturnProof) proveRead(site callsite.Site, read directive.ParamRead) {
	for _, val := range p.rt.ParamReadValues(site, read) {
		p.prove(val)
	}
	if len(read.Path) == 0 {
		// The argument itself, proved above.
		return
	}
	if param, path, ok := immutableReturnSelector(site.ParamArg(read.Param)); ok && param.Parent() == p.fn {
		p.reads = append(p.reads, directive.ParamRead{Param: slices.Index(p.fn.Params, param), Path: append(path, read.Path...)})
	}
}

// proveStruct checks the *gorm.DB fields of v, a struct or a pointer to one
// that fn returns at ret: paths are the fields, as ImmutableReturnFieldPaths
// lists them. The struct must be one that fn builds, or one that a callee it
// accepts returns, so that what fn stored is what the caller reads. Its fields
// hold the values stored into them that reach ret, or nil. A field that holds
// a read of fn's parameter rules fn out: a caller reads such a field as the
// call's FieldRoot, which has no argument to select from (#342).
func (p *immutableReturnProof) proveStruct(v ssa.Value, paths [][]int, ret *ssa.Return) {
	if p.seen[v] || p.unprovable {
		return
	}
	p.seen[v] = true
	switch x := v.(type) {
	case *ssa.Phi:
		for _, edge := range x.Edges {
			p.proveStruct(edge, paths, ret)
		}
	case *ssa.Const:
		// A nil pointer or a zero struct holds no handle.
	case *ssa.Call:
		p.proveStructCall(x)
	case *ssa.Extract:
		if call, ok := x.Tuple.(*ssa.Call); ok {
			p.proveStructCall(call)
		} else {
			p.unprovable = true
		}
	case *ssa.Alloc, *ssa.UnOp:
		// &T{...}, or the T{...} that a load of a local variable copies. A
		// pointer that a captured variable holds is stored into it, which
		// immutableReturnBuiltStruct does not accept.
		p.proveBuiltStruct(x, paths, ret)
	default:
		p.unprovable = true
	}
}

// proveBuiltStruct checks the fields of a struct that fn builds, an Alloc or
// a load of one.
func (p *immutableReturnProof) proveBuiltStruct(x ssa.Value, paths [][]int, ret *ssa.Return) {
	if !immutableReturnBuiltStruct(x, p.fn) {
		p.unprovable = true
		return
	}
	reads := len(p.reads)
	for _, path := range paths {
		for _, val := range p.rt.ParamReadSelected(x, path, ret) {
			p.prove(val)
		}
	}
	if len(p.reads) > reads {
		p.unprovable = true
	}
}

// proveStructCall checks a call that returns a struct holding a *gorm.DB: its
// callee must be accepted. A struct result has no parameter reads.
func (p *immutableReturnProof) proveStructCall(call *ssa.Call) {
	switch callee := origin.Declared(p.rt.Site(call).Declared); {
	case callee == nil:
		p.unprovable = true
	case !p.accept(callee):
		p.blockers = append(p.blockers, callee)
	}
}

// ImmutableReturnFieldPaths returns the paths of the *gorm.DB fields of t, a
// struct or a pointer to one, that reuse detection reads as the FieldRoot of
// a call returning t (#181, #250): the fields of the struct and of the structs
// it holds by value. A field behind a pointer field is not one. It is nil for
// any other type.
func ImmutableReturnFieldPaths(t types.Type) [][]int {
	if ptr, ok := t.Underlying().(*types.Pointer); ok {
		t = ptr.Elem()
	}
	st, ok := t.Underlying().(*types.Struct)
	if !ok {
		return nil
	}
	var paths [][]int
	for i := range st.NumFields() {
		ft := st.Field(i).Type()
		if typeutil.IsGormDB(ft) {
			paths = append(paths, []int{i})
			continue
		}
		if _, isStruct := ft.Underlying().(*types.Struct); isStruct {
			for _, sub := range ImmutableReturnFieldPaths(ft) {
				paths = append(paths, append([]int{i}, sub...))
			}
		}
	}
	return paths
}

// immutableReturnBuiltStruct reports whether v is a struct that fn builds
// and only fills in: an Alloc of fn, or a load of one, whose address is used
// only to store into its fields, to load them and to return it. A struct
// passed to a call or stored elsewhere may be changed where fn cannot see it.
func immutableReturnBuiltStruct(v ssa.Value, fn *ssa.Function) bool {
	if load, ok := v.(*ssa.UnOp); ok {
		// A load of a pointer variable is not the struct: the pointer was
		// stored into the variable, where fn cannot follow it.
		if _, isPtr := load.Type().Underlying().(*types.Pointer); load.Op != token.MUL || isPtr {
			return false
		}
		v = load.X
	}
	alloc, ok := v.(*ssa.Alloc)
	if !ok || alloc.Parent() != fn {
		return false
	}
	var used func(addr ssa.Value, seen map[ssa.Value]bool) bool
	used = func(addr ssa.Value, seen map[ssa.Value]bool) bool {
		if seen[addr] {
			return true
		}
		seen[addr] = true
		for _, ref := range *addr.Referrers() {
			switch r := ref.(type) {
			case *ssa.Store:
				if r.Val == addr {
					return false
				}
			case *ssa.FieldAddr:
				if !used(r, seen) {
					return false
				}
			case *ssa.Phi:
				if !used(r, seen) {
					return false
				}
			case *ssa.UnOp, *ssa.Return, *ssa.DebugRef:
			default:
				return false
			}
		}
		return true
	}
	return used(alloc, make(map[ssa.Value]bool))
}

// immutableReturnLocalStores returns the values stored into the local
// variable that load reads, directly or through the free variables of
// closures, when every store into it is in its function
// (callsite.LocalStores).
func immutableReturnLocalStores(load *ssa.UnOp) ([]ssa.Value, bool) {
	if load.Op != token.MUL {
		return nil, false
	}
	addr := load.X
	for {
		fv, ok := addr.(*ssa.FreeVar)
		if !ok {
			break
		}
		addr = callsite.FreeVarBinding(fv)
	}
	alloc, ok := addr.(*ssa.Alloc)
	if !ok {
		return nil, false
	}
	return callsite.LocalStores(alloc)
}

// immutableReturnReadsShared reports whether load reads a field through a
// parameter, at any depth of fields and pointer fields, also when the
// parameter is captured or copied into a variable assigned once, or reads a
// package-level variable. Reuse detection treats both as immutable where no
// store in the function reaches the read (see "Struct roots" and
// "Package-level variables" in the design notes).
func immutableReturnReadsShared(load *ssa.UnOp) bool {
	if load.Op != token.MUL {
		return false
	}
	switch load.X.(type) {
	case *ssa.Global:
		return true
	case *ssa.FieldAddr:
		_, _, ok := immutableReturnSelector(load)
		return ok
	}
	// A load through a pointer parameter is not a field.
	return false
}

// immutableReturnSelector returns the parameter that v is selected from and
// the path of fields the selector reads, as a directive.ParamRead holds it:
// r.inner.db is r with the indexes of inner and db. It looks through loads of
// fields, the free variables of closures, and variables assigned once, such
// as the copy of a value receiver. ok is false for anything else.
func immutableReturnSelector(v ssa.Value) (param *ssa.Parameter, path []int, ok bool) {
	for {
		switch x := v.(type) {
		case *ssa.Parameter:
			slices.Reverse(path)
			return x, path, true
		case *ssa.FieldAddr:
			path = append(path, x.Field)
			v = x.X
		case *ssa.UnOp:
			// A pointer field, or a variable that holds the struct pointer.
			if x.Op != token.MUL {
				return nil, nil, false
			}
			switch x.X.(type) {
			case *ssa.FieldAddr, *ssa.Alloc, *ssa.FreeVar:
				v = x.X
			default:
				return nil, nil, false
			}
		case *ssa.FreeVar:
			v = callsite.FreeVarBinding(x)
		case *ssa.Alloc:
			// A variable that holds a copy of a parameter, such as a value
			// receiver whose field is read through its address.
			stored := tracer.AllocStoredValues(x)
			if len(stored) != 1 {
				return nil, nil, false
			}
			v = stored[0]
		default:
			return nil, nil, false
		}
	}
}
