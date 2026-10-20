package internal

import (
	"slices"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/directive"
	"github.com/mpyw/gormreuse/internal/ssa/callback"
	"github.com/mpyw/gormreuse/internal/ssa/callsite"
	"github.com/mpyw/gormreuse/internal/ssa/origin"
	"github.com/mpyw/gormreuse/internal/ssa/purity"
	"github.com/mpyw/gormreuse/internal/ssa/tracer"
)

// distrustedContracts holds the immutable-return and immutable-input
// functions that another package must not trust.
//
//declscope:shared // fact.go leaves them out of the facts
type distrustedContracts struct {
	returns, inputs map[*ssa.Function]bool
}

// distrustSources is what distrustContracts reads: the sets, the functions
// whose body breaks a contract, and the tracer that validated them.
//
//declscope:shared // run.go gathers them
type distrustSources struct {
	funcs                     []*ssa.Function
	immutableReturnFuncs      *directive.DirectiveFuncSet
	immutableInputSet         *directive.ImmutableInputSet
	failedReturn, failedInput map[*ssa.Function]bool
	tracer                    *tracer.RootTracer
	funcVars                  callsite.FuncVars
	skip                      func(*ssa.Function) bool
}

// distrustContracts returns the immutable-return and immutable-input
// functions, declared or inferred, that another package must not trust (#314):
// those whose body breaks the contract, and those that keep it only by relying
// on one of them. A function relies on a distrusted one when its body is
// checked again without trusting it and the check fails:
//
//   - a declared immutable-return function returns the result of a call to
//     one, as ImmutableReturnValidator.Distrusting counts it;
//   - an inferred one is no longer proved with it distrusted (#261);
//   - an immutable-input function hands its callback the result of one, hands
//     the callback to one, or hands it the parameter of a callback that only
//     one exempts.
//
// In this package each of them is still trusted, so a broken contract is
// reported once, at its declaration.
//
// Like pure's failures (#187), distrust is transitive and is computed from a
// worklist: when a function becomes distrusted, the functions that call or
// refer to it, or that create a callback it exempts, are checked again. The
// sets only grow, and a larger set only fails more checks, so this ends, at
// the same sets in any order. Each function is checked again at most once per
// function it depends on that becomes distrusted.
//
//declscope:shared // run.go computes what the facts leave out
func distrustContracts(src distrustSources, guard *panicGuard) distrustedContracts {
	d := distrustedContracts{
		returns: make(map[*ssa.Function]bool),
		inputs:  make(map[*ssa.Function]bool),
	}
	var seeds []*ssa.Function
	for _, fn := range src.funcs {
		if src.failedReturn[fn] || src.failedInput[fn] {
			d.returns[fn] = d.returns[fn] || src.failedReturn[fn]
			d.inputs[fn] = d.inputs[fn] || src.failedInput[fn]
			seeds = append(seeds, fn)
		}
	}
	if len(seeds) == 0 {
		return d
	}

	set := src.immutableReturnFuncs
	var targets []*ssa.Function
	for _, fn := range src.funcs {
		if !src.skip(fn) && (set.Contains(fn) || len(src.immutableInputSet.Callbacks(fn)) > 0) {
			targets = append(targets, fn)
		}
	}
	providers := callback.CollectImmutableInputProviders(src.funcs, src.funcVars, src.immutableInputSet)
	dependents := distrustDependents(targets, providers, src.funcVars)

	returnDistrusted := func(fn *ssa.Function) bool { return d.returns[origin.Declared(fn)] }
	inputDistrusted := func(fn *ssa.Function) bool { return d.inputs[origin.Declared(fn)] }
	// A callback is exempt only while every function that calls it keeps its
	// promise: a distrusted one may hand it a mutable value.
	callbackDistrusted := func(fn *ssa.Function) bool { return slices.ContainsFunc(providers[fn], inputDistrusted) }
	trusted := func(fn *ssa.Function) bool { return set.Contains(fn) && !returnDistrusted(fn) }
	validator := purity.NewImmutableReturnValidator(src.funcs, set, nil)
	builtins := tracer.NewRootTracer(nil, nil, nil, nil, nil, nil, src.funcVars)

	// recheck checks fn again with the distrusted functions distrusted and
	// reports whether it became distrusted. A check that panics distrusts
	// the function: the panic is reported, and another package then treats it
	// as unmarked, which reports more, not less.
	recheck := func(fn *ssa.Function) bool {
		checkReturn := set.Contains(fn) && !d.returns[fn]
		checkInput := len(src.immutableInputSet.Callbacks(fn)) > 0 && !d.inputs[fn]
		if !checkReturn && !checkInput {
			return false
		}
		failedReturn, failedInput := checkReturn, checkInput
		guard.run(fn, func() {
			rt := src.tracer.Distrusting(returnDistrusted, callbackDistrusted)
			if checkReturn {
				if set.Inferred(fn) {
					failedReturn = !purity.InferImmutableReturns([]*ssa.Function{fn}, trusted, builtins)[fn]
				} else {
					failedReturn = len(validator.Distrusting(rt, returnDistrusted).Validate(fn)) > 0
				}
			}
			if checkInput {
				failedInput = len(purity.ValidateImmutableInputs(fn, src.immutableInputSet, rt, inputDistrusted)) > 0
			}
		})
		d.returns[fn] = d.returns[fn] || failedReturn
		d.inputs[fn] = d.inputs[fn] || failedInput
		return failedReturn || failedInput
	}

	var queue []*ssa.Function
	queued := make(map[*ssa.Function]bool)
	enqueue := func(fn *ssa.Function) {
		for _, dep := range dependents[fn] {
			if !queued[dep] {
				queued[dep] = true
				queue = append(queue, dep)
			}
		}
	}
	for _, fn := range seeds {
		enqueue(fn)
	}
	for len(queue) > 0 {
		fn := queue[0]
		queue = queue[1:]
		queued[fn] = false
		if recheck(fn) {
			enqueue(fn)
		}
	}
	return d
}

// distrustDependents maps each function to the targets whose check may
// depend on whether it is trusted: those that call it or refer to it, in
// their own body or in a closure inside it, and those that create, or are, a
// callback it exempts. Functions are kept by declared function
// (origin.Declared), so a generic instance or a bound method wrapper counts
// as the function it stands for.
func distrustDependents(targets []*ssa.Function, providers map[*ssa.Function][]*ssa.Function, funcVars callsite.FuncVars) map[*ssa.Function][]*ssa.Function {
	dependents := make(map[*ssa.Function][]*ssa.Function)
	for _, target := range targets {
		seen := make(map[*ssa.Function]bool)
		add := func(fn *ssa.Function) {
			if fn = origin.Declared(fn); fn != nil && !seen[fn] {
				seen[fn] = true
				dependents[fn] = append(dependents[fn], target)
			}
		}
		var walk func(fn *ssa.Function)
		walk = func(fn *ssa.Function) {
			for _, p := range providers[fn] {
				add(p)
			}
			for _, block := range fn.Blocks {
				for _, instr := range block.Instrs {
					if call, ok := instr.(ssa.CallInstruction); ok {
						site := callsite.SiteOf(call, funcVars)
						add(site.Callee)
						add(site.Declared)
					}
					for _, op := range instr.Operands(nil) {
						if ref, ok := (*op).(*ssa.Function); ok {
							add(ref)
						}
					}
				}
			}
			for _, anon := range fn.AnonFuncs {
				walk(anon)
			}
		}
		walk(target)
	}
	return dependents
}
