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
	//declscope:private
	returns, inputs map[*ssa.Function]bool
	// all is set when computing them panicked, so that every contract is
	// distrusted (#315).
	all bool
}

// distrustsReturn reports whether another package must not trust the
// immutable-return contract of fn.
//
//declscope:shared // fact.go leaves it out of the facts
func (d distrustedContracts) distrustsReturn(fn *ssa.Function) bool {
	return d.all || d.returns[fn]
}

// distrustsInput reports whether another package must not trust the
// immutable-input contract of fn.
//
//declscope:shared // fact.go leaves it out of the facts
func (d distrustedContracts) distrustsInput(fn *ssa.Function) bool {
	return d.all || d.inputs[fn]
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
// the same sets in any order. A function outside a cycle of dependents is
// checked again at most once (see distrustGroups).
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
	builtins := tracer.NewRootTracer(nil, nil, nil, nil, nil, nil, src.funcVars)
	validator := purity.NewImmutableReturnValidator(src.funcs, set, builtins)

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
		guard.run("distrust", fn, func() {
			rt := src.tracer.Distrusting(returnDistrusted, callbackDistrusted)
			if checkReturn {
				if set.Inferred(fn) {
					_, inferred := purity.InferImmutableReturns([]*ssa.Function{fn}, trusted, set.ParamReads, builtins, validator.AssignedMutable)[fn]
					failedReturn = !inferred
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

	// The functions are checked again one group of mutually dependent ones
	// at a time, each group after every group it depends on (#350). A
	// function outside a cycle is then checked again at most once, after all
	// the functions it depends on are settled, not once per one of them that
	// becomes distrusted. Inside a group the worklist runs as before.
	dirty := make(map[*ssa.Function]bool)
	for _, fn := range seeds {
		for _, dep := range dependents[fn] {
			dirty[dep] = true
		}
	}
	for _, group := range distrustGroups(seeds, dependents) {
		recheckDistrustGroup(group, dependents, dirty, recheck)
	}
	return d
}

// recheckDistrustGroup runs the worklist of distrustContracts on group, one
// of distrustGroups: it checks again each function of group that dirty holds,
// and marks dirty the dependents of each one that recheck distrusts. A
// dependent in group is checked again in turn.
func recheckDistrustGroup(group []*ssa.Function, dependents map[*ssa.Function][]*ssa.Function, dirty map[*ssa.Function]bool, recheck func(*ssa.Function) bool) {
	inGroup := make(map[*ssa.Function]bool, len(group))
	for _, fn := range group {
		inGroup[fn] = true
	}
	var queue []*ssa.Function
	queued := make(map[*ssa.Function]bool)
	for _, fn := range group {
		if dirty[fn] {
			queued[fn] = true
			queue = append(queue, fn)
		}
	}
	for len(queue) > 0 {
		fn := queue[0]
		queue = queue[1:]
		queued[fn] = false
		if !recheck(fn) {
			continue
		}
		for _, dep := range dependents[fn] {
			dirty[dep] = true
			if inGroup[dep] && !queued[dep] {
				queued[dep] = true
				queue = append(queue, dep)
			}
		}
	}
}

// distrustGroups returns the functions that the seeds' distrust may reach
// through dependents, as the strongly connected components of that graph, in
// topological order: each group comes after every group that a function in
// it depends on. It uses Tarjan's algorithm, which completes a group after
// every group reachable from it, so the groups are reversed at the end. The
// order is deterministic, given the order of seeds and of each dependents
// list.
func distrustGroups(seeds []*ssa.Function, dependents map[*ssa.Function][]*ssa.Function) [][]*ssa.Function {
	index := make(map[*ssa.Function]int)
	low := make(map[*ssa.Function]int)
	onStack := make(map[*ssa.Function]bool)
	var stack []*ssa.Function
	var groups [][]*ssa.Function
	var visit func(fn *ssa.Function)
	visit = func(fn *ssa.Function) {
		index[fn] = len(index)
		low[fn] = index[fn]
		stack = append(stack, fn)
		onStack[fn] = true
		for _, dep := range dependents[fn] {
			if _, ok := index[dep]; !ok {
				visit(dep)
				low[fn] = min(low[fn], low[dep])
			} else if onStack[dep] {
				low[fn] = min(low[fn], index[dep])
			}
		}
		if low[fn] != index[fn] {
			return
		}
		at := slices.Index(stack, fn)
		group := slices.Clone(stack[at:])
		for _, member := range group {
			onStack[member] = false
		}
		stack = stack[:at]
		groups = append(groups, group)
	}
	for _, fn := range seeds {
		if _, ok := index[fn]; !ok {
			visit(fn)
		}
	}
	slices.Reverse(groups)
	return groups
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
