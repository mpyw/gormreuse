package internal

import (
	"go/ast"
	"go/token"
	"go/types"
	"slices"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/directive"
	"github.com/mpyw/gormreuse/internal/fix"
	"github.com/mpyw/gormreuse/internal/ssa/callsite"
	"github.com/mpyw/gormreuse/internal/ssa/origin"
	"github.com/mpyw/gormreuse/internal/ssa/purity"
	"github.com/mpyw/gormreuse/internal/ssa/stmtwrite"
	"github.com/mpyw/gormreuse/internal/ssa/tracer"
	"github.com/mpyw/gormreuse/internal/typeutil"
)

// inferImmutableInputs adds to set the callback parameters of the
// unexported functions and the closures of funcs that hand the callback only
// immutable values, as //gormreuse:immutable-input(name) promises (#349): a
// function that only forwards cb to Transaction, FindInBatches or another
// immutable-input function, or calls it with a Transaction callback's tx. A
// closure counts, such as a wrapper held in a local variable: its callbacks
// are found through the variable, as a declared closure's are. The promise
// does not depend on the callers, so they need not all be seen.
//
// callbacks holds the immutable callbacks before inference, and collect
// gathers them again from the set. A callback of an inferred function is
// immutable too, so it may prove another function: the proofs run again with
// the callbacks collected after each step that added one. newTracer builds
// the tracer that proves the values handed to a callback, for the callbacks
// of the step. The returned callbacks are the ones collected last, after the
// last function was added. Each value handed to a callback must be proved
// (tracer.RootTracer.ProvesImmutable, #371), and globals holds the
// package-level variables a proof may read.
//
// Like inferImmutableReturns, this is a least fixed point. A function is
// proved without its own callbacks in the set, so a function that hands cb to
// itself is not inferred. A function that waits on others is proved again
// once one of the functions it calls is added.
//
//declscope:shared // run.go infers the immutable-input functions
func inferImmutableInputs(
	funcs []*ssa.Function,
	funcVars callsite.FuncVars,
	set *directive.ImmutableInputSet,
	callbacks map[*ssa.Function]bool,
	collect func() map[*ssa.Function]bool,
	newTracer func(callbacks map[*ssa.Function]bool) *tracer.RootTracer,
	globals tracer.ProvenGlobals,
	skip func(*ssa.Function) bool,
) map[*ssa.Function]bool {
	type candidate struct {
		fn *ssa.Function
		cb directive.ImmutableInputCallback
	}
	var candidates []candidate
	for _, fn := range funcs {
		_, closure := fn.Syntax().(*ast.FuncLit)
		if !closure && !inferable(fn, skip) || closure && skip(fn) || len(set.Callbacks(fn)) > 0 {
			continue
		}
		params := fn.Signature.Params()
		shift := len(fn.Params) - params.Len() // the receiver
		for i := range params.Len() {
			if fn.Signature.Variadic() && i == params.Len()-1 {
				continue // a variadic callback is not supported (#259)
			}
			// A callback the body never uses is handed nothing, and
			// inferring the promise would only hide what it is for.
			if !inferUsesParam(fn.Params[i+shift]) {
				continue
			}
			if sig, ok := params.At(i).Type().Underlying().(*types.Signature); ok && typeutil.HasGormDBParameter(sig) {
				candidates = append(candidates, candidate{fn, directive.ImmutableInputCallback{ParamIdx: i, ParamName: params.At(i).Name()}})
			}
		}
	}
	if len(candidates) == 0 {
		return callbacks
	}

	// dependents maps a function to the candidates that call it.
	dependents := make(map[*ssa.Function][]int)
	for i, c := range candidates {
		callsite.EachSite([]*ssa.Function{c.fn}, funcVars, func(site callsite.Site) {
			if site.Declared != nil {
				callee := origin.Declared(site.Declared)
				if deps := dependents[callee]; len(deps) == 0 || deps[len(deps)-1] != i {
					dependents[callee] = append(dependents[callee], i)
				}
			}
		})
	}

	proved := make([]bool, len(candidates))
	for {
		rt := newTracer(callbacks)
		queue := make([]int, len(candidates))
		for i := range candidates {
			queue[i] = i
		}
		progress := false
		for len(queue) > 0 {
			var added []*ssa.Function
			for _, i := range queue {
				c := candidates[i]
				if proved[i] || !purity.ProvesImmutableInput(c.fn, c.cb, set, rt, globals) {
					continue
				}
				proved[i] = true
				set.Infer(c.fn, c.cb)
				added = append(added, c.fn)
			}
			queue = queue[:0]
			for _, fn := range added {
				for _, i := range dependents[fn] {
					if !proved[i] {
						queue = append(queue, i)
					}
				}
			}
			slices.Sort(queue)
			queue = slices.Compact(queue)
			progress = progress || len(added) > 0
		}
		if !progress {
			return callbacks
		}
		callbacks = collect()
	}
}

// inferImmutableParams adds to set the unexported functions of funcs that
// every caller passes immutable *gorm.DB values, as
// //gormreuse:immutable-param asks of them (#349): a Session-family or
// gorm.Open result, an immutable-return result, a Transaction callback's tx,
// or the parameter of another inferred function. Only such a proof counts
// (tracer.RootTracer.ProvesImmutable): a value the tracer cannot follow, or
// the parameter of a declared immutable-param function, proves nothing
// (#371). init is the package initializer, whose calls count too.
//
// The function must be called only directly, so that no caller in the pass
// is missed. A caller in a test file is not seen by the variant of the
// package without its test files, which may then infer the function. The
// test variant sees it and does not, so the function is still reported
// there. A function that changes its parameter in place, with a discarded
// chain statement such as tx.Where("a"), or through a helper or a closure
// that does, is not inferred (inferChangesInPlace): the change would be lost
// on an immutable value. Those functions are returned, for the fix to ask
// too (#387). A panic returns none, and the fix then takes every function
// to change one. finisherOnly returns the finisher-only parameters of a
// function of another package, from its fact. globals holds the
// package-level variables a proof may read.
//
// newTracer builds a tracer with the current set. A larger set only proves
// more, so a function whose callers did not prove it is proved again once one
// of them is added. Each round uses a new tracer, since a tracer remembers
// what it traced with the set it had. The result is the least fixed point, so
// a function that passes its own parameter to itself is not inferred.
//
//declscope:shared // run.go infers the immutable-param functions
func inferImmutableParams(
	funcs []*ssa.Function,
	init *ssa.Function,
	set *directive.DirectiveFuncSet,
	newTracer func() *tracer.RootTracer,
	finisherOnly func(*ssa.Function) []int,
	globals tracer.ProvenGlobals,
	skip func(*ssa.Function) bool,
) map[*ssa.Function]bool {
	changes := inferChangesInPlace(funcs, finisherOnly)
	candidates := make(map[*ssa.Function]bool)
	var order []*ssa.Function
	for _, fn := range funcs {
		if inferable(fn, skip) && !set.Contains(fn) && inferHasGormDBParam(fn) && !changes[fn] {
			candidates[fn] = true
			order = append(order, fn)
		}
	}
	if len(order) == 0 {
		return changes
	}

	sites := inferDirectCalls(append(slices.Clone(funcs), init), candidates)
	order = slices.DeleteFunc(order, func(fn *ssa.Function) bool { return len(sites[fn]) == 0 })

	// dependents maps a function to the candidates it calls, also from its
	// closures, whose arguments may be its parameters.
	dependents := make(map[*ssa.Function][]*ssa.Function)
	for _, fn := range order {
		for _, call := range sites[fn] {
			for owner := call.Parent(); owner != nil; owner = owner.Parent() {
				if deps := dependents[owner]; len(deps) == 0 || deps[len(deps)-1] != fn {
					dependents[owner] = append(dependents[owner], fn)
				}
			}
		}
	}

	index := make(map[*ssa.Function]int, len(order))
	for i, fn := range order {
		index[fn] = i
	}
	queue := slices.Clone(order)
	for len(queue) > 0 {
		rt := newTracer()
		var added []*ssa.Function
		for _, fn := range queue {
			if !set.Inferred(fn) && inferCallersPassImmutable(fn, sites[fn], rt, set, globals) {
				set.Infer(fn)
				added = append(added, fn)
			}
		}
		queue = queue[:0]
		for _, fn := range added {
			for _, dep := range dependents[fn] {
				if !set.Inferred(dep) {
					queue = append(queue, dep)
				}
			}
		}
		slices.SortFunc(queue, func(a, b *ssa.Function) int { return index[a] - index[b] })
		queue = slices.Compact(queue)
	}
	return changes
}

// inferImmutableParamRule returns the rules of inferImmutableParams that the
// directive fix must meet too (#387): the function changes no parameter in
// place (changes, from inferChangesInPlace), and each call passes values
// proved immutable with the inferred set. changes is nil after a panic in
// the inference, and then every function may change one.
//
//declscope:shared // run.go hands it to the fix
func inferImmutableParamRule(changes map[*ssa.Function]bool, rt *tracer.RootTracer, set *directive.DirectiveFuncSet, globals tracer.ProvenGlobals) fix.ImmutableParamRule {
	return fix.ImmutableParamRule{
		ChangesInPlace: func(fn *ssa.Function) bool { return changes == nil || changes[fn] },
		ProvesArgs: func(fn *ssa.Function, call ssa.CallInstruction) bool {
			return rt.ProvesImmutableArgs(fn, call, set.Inferred, globals)
		},
	}
}

// inferDirectCalls returns the calls of each candidate in funcs, and none for
// a candidate that something else refers to: every reference must be the
// function of a call (instances included), and a method must not be called
// through an interface, so no interface call in funcs has its name.
func inferDirectCalls(funcs []*ssa.Function, candidates map[*ssa.Function]bool) map[*ssa.Function][]ssa.CallInstruction {
	sites := make(map[*ssa.Function][]ssa.CallInstruction)
	invoked := make(map[string]bool)
	callsite.WalkInstrs(funcs, func(instr ssa.Instruction) {
		call, isCall := instr.(ssa.CallInstruction)
		if isCall && call.Common().IsInvoke() {
			invoked[call.Common().Method.Name()] = true
		}
		if _, ok := instr.(*ssa.DebugRef); ok {
			return
		}
		for _, op := range instr.Operands(nil) {
			f, ok := (*op).(*ssa.Function)
			if !ok {
				continue
			}
			fn := origin.Declared(f)
			if !candidates[fn] {
				continue
			}
			if isCall && !call.Common().IsInvoke() && op == &call.Common().Value && (f == fn || f.Origin() == fn) {
				sites[fn] = append(sites[fn], call)
				continue
			}
			candidates[fn] = false
		}
	})
	for fn := range sites {
		if !candidates[fn] || fn.Signature.Recv() != nil && invoked[fn.Name()] {
			delete(sites, fn)
		}
	}
	return sites
}

// inferable reports whether fn may get an inferred directive: a declared
// function or method of a file that is analyzed, whose name is not
// exported, so no other package calls it.
func inferable(fn *ssa.Function, skip func(*ssa.Function) bool) bool {
	if _, ok := fn.Syntax().(*ast.FuncDecl); !ok || skip(fn) {
		return false
	}
	return fn.Object() != nil && !fn.Object().Exported()
}

// inferUsesParam reports whether the body of p's function uses p.
func inferUsesParam(p *ssa.Parameter) bool {
	return slices.ContainsFunc(*p.Referrers(), func(instr ssa.Instruction) bool {
		_, debug := instr.(*ssa.DebugRef)
		return !debug
	})
}

// inferHasGormDBParam reports whether fn has a *gorm.DB parameter, not counting
// its receiver: the parameters immutable-param governs.
func inferHasGormDBParam(fn *ssa.Function) bool {
	for v := range fn.Signature.Params().Variables() {
		if typeutil.IsGormDB(v.Type()) {
			return true
		}
	}
	return false
}

// inferCallersPassImmutable reports whether each call in calls passes fn
// values proved immutable at its *gorm.DB parameters
// (tracer.RootTracer.ProvesImmutableArgs). The parameter of a function proves
// a call only when set inferred it. A declared immutable-param function's
// callers are checked only while it branches the parameter, and it would
// stop branching once its helpers are inferred (#371).
func inferCallersPassImmutable(fn *ssa.Function, calls []ssa.CallInstruction, rt *tracer.RootTracer, set *directive.DirectiveFuncSet, globals tracer.ProvenGlobals) bool {
	for _, call := range calls {
		if !rt.ProvesImmutableArgs(fn, call, set.Inferred, globals) {
			return false
		}
	}
	return true
}

// inferChangesInPlace returns the declared functions of funcs that may change
// one of their *gorm.DB parameters in place. On a mutable value such a change
// reaches the caller's value; on an immutable one it does nothing, so the
// function relies on its callers passing a mutable value. A function changes
// its parameter when it, or a closure in it, does any of these with it
// (inferParamUses):
//
//   - discards a chain that does not end in a finisher, such as tx.Where("a")
//     or tx.Where("a").Order("b"), as the immutable-param discard report reads
//     it (immutableParamDiscardedChain, #346);
//   - passes it to a function of funcs that changes it (#371), or to a
//     function of another package that reaches gorm, unless its fact says the
//     parameter only runs finishers;
//   - passes it to a dynamic or interface call, stores it outside a local
//     variable, boxes it, or uses it in another way the check does not
//     follow.
//
// A parameter captured by a closure, or held in a local variable, is followed
// through the variable, and a closure called with it gets it as its own.
// finisherOnly returns the finisher-only parameters of a function of another
// package, from its fact.
func inferChangesInPlace(funcs []*ssa.Function, finisherOnly func(*ssa.Function) []int) map[*ssa.Function]bool {
	declared := make(map[*ssa.Function]bool)
	for _, fn := range funcs {
		if _, ok := fn.Syntax().(*ast.FuncDecl); ok {
			declared[fn] = true
		}
	}
	changes := make(map[*ssa.Function]bool)
	callers := make(map[*ssa.Function][]*ssa.Function)
	var queue []*ssa.Function
	for _, fn := range funcs {
		if !declared[fn] {
			continue
		}
		own, callees := inferParamUses(fn, declared, finisherOnly)
		if own {
			changes[fn] = true
			queue = append(queue, fn)
		}
		for _, callee := range callees {
			callers[callee] = append(callers[callee], fn)
		}
	}
	for len(queue) > 0 {
		fn := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		for _, caller := range callers[fn] {
			if !changes[caller] {
				changes[caller] = true
				queue = append(queue, caller)
			}
		}
	}
	return changes
}

// inferParamUses reads how fn and its closures use fn's *gorm.DB parameters
// (see inferChangesInPlace). own reports a use that may change one in place.
// callees are the functions of declared that get one as an argument: fn
// changes it when one of them does.
func inferParamUses(fn *ssa.Function, declared map[*ssa.Function]bool, finisherOnly func(*ssa.Function) []int) (own bool, callees []*ssa.Function) {
	tree := make(map[*ssa.Function]bool)
	var instrs []ssa.Instruction
	callsite.WalkInstrs([]*ssa.Function{fn}, func(instr ssa.Instruction) {
		tree[instr.Parent()] = true
		instrs = append(instrs, instr)
	})
	held, vars := inferHeldParams(fn, instrs, tree)
	direct := make(map[ssa.Value]bool)
	for _, h := range stmtwrite.Seeds(fn) {
		direct[h] = true
	}

	for v := range vars {
		for _, ref := range *v.Referrers() {
			switch ref := ref.(type) {
			case *ssa.DebugRef, *ssa.MakeClosure:
			case *ssa.UnOp:
				own = own || ref.Op != token.MUL
			case *ssa.Store:
				own = own || ref.Addr != v
			default:
				own = true
			}
		}
	}
	for v := range held {
		for _, ref := range *v.Referrers() {
			switch ref := ref.(type) {
			case *ssa.DebugRef, *ssa.Phi, *ssa.ChangeType, *ssa.BinOp, *ssa.Return, *ssa.MakeClosure:
			case *ssa.Store:
				own = own || !vars[ref.Addr]
			case *ssa.FieldAddr:
				own = own || !inferReadsField(ref, direct[v])
			case ssa.CallInstruction:
				changes, callee := inferCallChangesParam(ref, v, tree, declared, finisherOnly)
				own = own || changes
				if callee != nil {
					callees = append(callees, callee)
				}
			default:
				own = true
			}
		}
	}
	return own, callees
}

// inferHeldParams returns the values in instrs, the instructions of fn and of
// the closures in tree, that hold one of fn's *gorm.DB parameters, and the
// addresses of the variables that hold one: a parameter a closure captures
// lives in one.
func inferHeldParams(fn *ssa.Function, instrs []ssa.Instruction, tree map[*ssa.Function]bool) (held, vars map[ssa.Value]bool) {
	held = make(map[ssa.Value]bool)
	vars = make(map[ssa.Value]bool)
	for _, p := range fn.Params {
		if typeutil.IsGormDB(p.Type()) {
			held[p] = true
		}
	}
	for changed := true; changed; {
		changed = false
		for _, instr := range instrs {
			changed = inferHoldStep(instr, held, vars, tree) || changed
		}
	}
	return held, vars
}

// inferHoldStep adds to held and vars what instr makes hold a parameter, and
// reports whether it added anything (see inferHeldParams).
func inferHoldStep(instr ssa.Instruction, held, vars map[ssa.Value]bool, tree map[*ssa.Function]bool) bool {
	added := false
	mark := func(m map[ssa.Value]bool, v ssa.Value) {
		if !m[v] {
			m[v] = true
			added = true
		}
	}
	switch instr := instr.(type) {
	case *ssa.Store:
		// A store anywhere else is a use of its own.
		switch instr.Addr.(type) {
		case *ssa.Alloc, *ssa.FreeVar:
			if held[instr.Val] {
				mark(vars, instr.Addr)
			}
		}
	case *ssa.UnOp:
		if instr.Op == token.MUL && vars[instr.X] {
			mark(held, instr)
		}
	case *ssa.Phi:
		if slices.ContainsFunc(instr.Edges, func(e ssa.Value) bool { return held[e] }) {
			mark(held, instr)
		}
	case *ssa.ChangeType:
		if held[instr.X] {
			mark(held, instr)
		}
	case *ssa.MakeClosure:
		closure, _ := instr.Fn.(*ssa.Function)
		for i, b := range instr.Bindings {
			if closure == nil || i >= len(closure.FreeVars) {
				break
			}
			if held[b] {
				mark(held, closure.FreeVars[i])
			}
			if vars[b] {
				mark(vars, closure.FreeVars[i])
			}
		}
	case ssa.CallInstruction:
		// A closure called with a parameter gets it as its own.
		callee := instr.Common().StaticCallee()
		if callee == nil || !tree[callee] || len(callee.Params) != len(instr.Common().Args) {
			return false
		}
		for i, arg := range instr.Common().Args {
			if held[arg] {
				mark(held, callee.Params[i])
			}
		}
	}
	return added
}

// inferReadsField reports whether fa, a field of a *gorm.DB, is only read,
// as in tx.Error or tx.Dialector. The Statement is a pointer that a write
// through it changes in place, so only the reads of its own fields count, as
// in tx.Statement.Context. A field of the Statement that is a map, a slice or
// a handle is shared with it, so a write through one (#405), as in
// tx.Statement.Clauses["X"] = c, is a change too when fa is a direct path
// from the parameter (direct). stmtwrite decides it, as it does for the pure
// contract. Through anything else, only the loads count, as before #405.
func inferReadsField(fa *ssa.FieldAddr, direct bool) bool {
	ptr, ok := fa.X.Type().Underlying().(*types.Pointer)
	if !ok {
		return false
	}
	st, ok := ptr.Elem().Underlying().(*types.Struct)
	if !ok || fa.Field >= st.NumFields() {
		return false
	}
	statement := st.Field(fa.Field).Name() == "Statement"
	var readsOnly func(*ssa.UnOp) bool
	if direct {
		readsOnly = func(load *ssa.UnOp) bool { return !stmtwrite.WritesThrough(load) }
	}
	return inferOnlyLoads(fa, func(load *ssa.UnOp) bool {
		return !statement || !slices.ContainsFunc(*load.Referrers(), func(ref ssa.Instruction) bool {
			field, isField := ref.(*ssa.FieldAddr)
			_, debug := ref.(*ssa.DebugRef)
			return !debug && (!isField || !inferOnlyLoads(field, readsOnly))
		})
	})
}

// inferOnlyLoads reports whether every use of addr is a load, for which ok,
// when it is not nil, holds.
func inferOnlyLoads(addr ssa.Value, ok func(*ssa.UnOp) bool) bool {
	return !slices.ContainsFunc(*addr.Referrers(), func(ref ssa.Instruction) bool {
		if _, debug := ref.(*ssa.DebugRef); debug {
			return false
		}
		load, isLoad := ref.(*ssa.UnOp)
		return !isLoad || load.Op != token.MUL || ok != nil && !ok(load)
	})
}

// inferCallChangesParam reports whether call, which gets v, a parameter, as
// its receiver or an argument, may change it in place. callee is the function
// of declared that gets it, whose own uses decide.
func inferCallChangesParam(call ssa.CallInstruction, v ssa.Value, tree, declared map[*ssa.Function]bool, finisherOnly func(*ssa.Function) []int) (changes bool, callee *ssa.Function) {
	common := call.Common()
	if b, ok := common.Value.(*ssa.Builtin); ok {
		return b.Name() == "append", nil
	}
	fn := common.StaticCallee()
	if fn == nil {
		return true, nil // a dynamic or interface call
	}
	if recv := fn.Signature.Recv(); recv != nil && typeutil.IsGormDB(recv.Type()) {
		if c, ok := call.(*ssa.Call); ok && common.Args[0] == v {
			_, changes = immutableParamDiscardedChain(c, v)
		}
		return changes, nil
	}
	if tree[fn] || fn.Pkg != nil && typeutil.IsGormPackage(fn.Pkg.Pkg) {
		return false, nil // a closure of the function, or a gorm function
	}
	decl := origin.Declared(fn)
	if declared[decl] {
		return false, decl
	}
	if decl.Pkg != nil && !typeutil.ReachesGorm(decl.Pkg.Pkg) {
		return false, nil
	}
	var only []int
	if finisherOnly != nil {
		only = finisherOnly(decl)
	}
	for i, arg := range common.Args {
		if arg == v && !slices.Contains(only, i) {
			return true, nil
		}
	}
	return false, nil
}
