package internal

import (
	"go/ast"
	"go/types"
	"slices"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/directive"
	"github.com/mpyw/gormreuse/internal/ssa/callsite"
	"github.com/mpyw/gormreuse/internal/ssa/cfg"
	"github.com/mpyw/gormreuse/internal/ssa/handler"
	"github.com/mpyw/gormreuse/internal/ssa/origin"
	"github.com/mpyw/gormreuse/internal/ssa/purity"
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
// last function was added.
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
				if proved[i] || !purity.ProvesImmutableInput(c.fn, c.cb, set, rt) {
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
// or the parameter of another inferred function. init is the package
// initializer, whose calls count too.
//
// The function must be called only directly, so that no caller in the pass
// is missed. A caller in a test file is not seen by the variant of the
// package without its test files, which may then infer the function. The
// test variant sees it and does not, so the function is still reported
// there. A function that changes its parameter in
// place, with a discarded chain statement such as tx.Where("a"), is not
// inferred: the change would be lost on an immutable value.
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
	cfgs *cfg.Analyzer,
	skip func(*ssa.Function) bool,
) {
	candidates := make(map[*ssa.Function]bool)
	var order []*ssa.Function
	for _, fn := range funcs {
		if inferable(fn, skip) && !set.Contains(fn) && inferHasGormDBParam(fn) && !inferChangesParamInPlace(fn) {
			candidates[fn] = true
			order = append(order, fn)
		}
	}
	if len(order) == 0 {
		return
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
			if !set.Inferred(fn) && inferCallersPassImmutable(fn, sites[fn], rt, cfgs) {
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
// immutable values at its *gorm.DB parameters, as the caller-side check of
// immutable-param asks (handler.BreaksImmutableParam).
func inferCallersPassImmutable(fn *ssa.Function, calls []ssa.CallInstruction, rt *tracer.RootTracer, cfgs *cfg.Analyzer) bool {
	params := fn.Signature.Params()
	for _, call := range calls {
		site := rt.Site(call)
		loopInfo := cfgs.DetectLoops(call.Parent())
		for i := range call.Common().Args {
			p := site.ArgParam(i)
			if p < 0 || p >= params.Len() || !typeutil.IsGormDB(params.At(p).Type()) {
				continue
			}
			if handler.BreaksImmutableParam(site, i, rt, loopInfo) {
				return false
			}
		}
	}
	return true
}

// inferChangesParamInPlace reports whether fn, or a closure in it, discards a
// chain that starts at one of fn's *gorm.DB parameters and does not end in a
// finisher, such as tx.Where("a") or tx.Where("a").Order("b"). On a mutable
// value that changes the caller's value in place; on an immutable one it does
// nothing. The chain is read as the immutable-param discard report reads it
// (immutableParamDiscardedChain, #346).
func inferChangesParamInPlace(fn *ssa.Function) bool {
	held := make(map[ssa.Value]bool)
	for _, p := range fn.Params {
		if typeutil.IsGormDB(p.Type()) {
			held[p] = true
		}
	}
	changes := false
	callsite.WalkInstrs([]*ssa.Function{fn}, func(instr ssa.Instruction) {
		switch instr := instr.(type) {
		case *ssa.MakeClosure:
			closure, ok := instr.Fn.(*ssa.Function)
			if !ok {
				return
			}
			for i, b := range instr.Bindings {
				if held[b] && i < len(closure.FreeVars) {
					held[closure.FreeVars[i]] = true
				}
			}
		case *ssa.Call:
			if !changes && len(instr.Call.Args) > 0 && held[instr.Call.Args[0]] {
				_, changes = immutableParamDiscardedChain(instr, instr.Call.Args[0])
			}
		}
	})
	return changes
}
