package internal

import (
	"go/token"
	"slices"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/directive"
	"github.com/mpyw/gormreuse/internal/facts"
	"github.com/mpyw/gormreuse/internal/ssa/callsite"
	"github.com/mpyw/gormreuse/internal/ssa/origin"
	"github.com/mpyw/gormreuse/internal/ssa/purity"
	"github.com/mpyw/gormreuse/internal/ssa/stmtwrite"
	"github.com/mpyw/gormreuse/internal/ssa/tracer"
)

// validatePureFuncs reports every //gormreuse:pure contract violation and
// returns the pure functions that definitively broke the contract, with the
// parameters each of the others may return. table holds the verdicts of pure
// functions in other packages.
//
// Failure is transitive: a pure function that passes its argument to a failed
// pure function fails too (issue #187). So is returning an argument: a pure
// function that returns what another one returns of its argument returns its
// own argument, and one that branches that result fails (#188). Validation
// therefore repeats until nothing changes, and violations are reported only
// from the final round, so each is reported once and sees the complete
// verdicts. A function is validated again only when the verdict of a function
// it calls changes, so a chain of n pure functions takes n validations, not n
// rounds of n (#260).
//
//declscope:shared // run.go validates the pure functions
func validatePureFuncs(
	funcs []*ssa.Function,
	pureFuncs, immutableReturnFuncs *directive.DirectiveFuncSet,
	funcVars callsite.FuncVars,
	table *facts.Table,
	directWrites stmtwrite.Cache,
	skip func(*ssa.Function) bool,
	report func(token.Pos, string),
	guard *panicGuard,
) *tracer.PureVerdicts {
	verdicts := &tracer.PureVerdicts{
		Failed:   make(map[*ssa.Function]bool),
		Returned: make(map[*ssa.Function][]int),
		Imported: table,
	}
	var targets []*ssa.Function
	for _, fn := range funcs {
		if !skip(fn) && pureFuncs.Contains(fn) {
			targets = append(targets, fn)
		}
	}

	// The failed set and each returned set only grow, and larger ones only
	// add violations and returned parameters, so this reaches a fixed point,
	// whatever order the functions are validated in.
	callers := pureCallers(targets, funcVars)
	queue := slices.Clone(targets)
	queued := make(map[*ssa.Function]bool, len(targets))
	for _, fn := range targets {
		queued[fn] = true
	}
	for len(queue) > 0 {
		fn := queue[0]
		queue = queue[1:]
		queued[fn] = false
		if verdicts.Failed[fn] {
			continue
		}
		changed := false
		finished := guard.run("pure", fn, func() {
			validator := purity.NewValidator(fn, pureFuncs, immutableReturnFuncs, verdicts, funcVars)
			// Only a definitive violation revokes pure-trust at call sites;
			// conservative func-arg violations do not (avoids FP cascades).
			if slices.ContainsFunc(validator.Validate(directWrites.Of(fn)), func(v purity.Violation) bool { return v.Definitive }) {
				verdicts.Failed[fn] = true
				delete(verdicts.Returned, fn)
				changed = true
				return
			}
			if returned := validator.ReturnedParams(); !slices.Equal(returned, verdicts.Returned[fn]) {
				verdicts.Returned[fn] = returned
				changed = true
			}
		})
		// A validation that panicked proves nothing, so the function is not
		// trusted (#315).
		if !finished {
			verdicts.Failed[fn] = true
			delete(verdicts.Returned, fn)
			changed = true
		}
		if !changed {
			continue
		}
		for _, caller := range callers[fn] {
			if !queued[caller] {
				queued[caller] = true
				queue = append(queue, caller)
			}
		}
	}

	verdicts.ChangesReceiver = pureChangedReceivers(targets, callers, pureFuncs, immutableReturnFuncs, verdicts, funcVars, guard)

	// A closure body is validated both as its own pure function and as part of
	// a pure function that creates it, so the same violation can come twice.
	reported := make(map[purity.Violation]bool)
	for _, fn := range targets {
		guard.run("pure", fn, func() {
			for _, v := range purity.NewValidator(fn, pureFuncs, immutableReturnFuncs, verdicts, funcVars).Validate(directWrites.Of(fn)) {
				if reported[v] {
					continue
				}
				reported[v] = true
				report(v.Pos, v.Message)
			}
		})
	}
	return verdicts
}

// pureChangedReceivers returns the trusted pure methods in targets that may
// change a *gorm.DB their receiver holds (#404). A method that calls such a
// method on its receiver changes it too, so the methods are checked callees
// first: in the order the strongly connected components of the call graph
// (callers, see pureCallers) finish. A method outside a cycle is checked once,
// after every method it calls is decided, whatever calls it. The methods of a
// cycle are checked again until none changes. A check that panicked counts as
// a change.
func pureChangedReceivers(
	targets []*ssa.Function,
	callers map[*ssa.Function][]*ssa.Function,
	pureFuncs, immutableReturnFuncs *directive.DirectiveFuncSet,
	verdicts *tracer.PureVerdicts,
	funcVars callsite.FuncVars,
	guard *panicGuard,
) map[*ssa.Function]bool {
	changed := make(map[*ssa.Function]bool)
	changes := func(fn *ssa.Function) bool {
		return changed[fn] || verdicts.Imported.Of(fn).PureChangesReceiver
	}
	check := func(fn *ssa.Function) bool {
		yes := true
		guard.run("pure", fn, func() {
			yes = purity.NewValidator(fn, pureFuncs, immutableReturnFuncs, verdicts, funcVars).ChangesReceiver(changes)
		})
		return yes
	}
	for _, scc := range pureCalleesFirst(targets, callers, verdicts) {
		for again := true; again; {
			again = false
			for _, fn := range scc {
				if !changed[fn] && check(fn) {
					changed[fn] = true
					again = len(scc) > 1
				}
			}
		}
	}
	return changed
}

// pureCalleesFirst returns the strongly connected components of the call
// graph among the trusted pure methods of targets, each after every component
// it calls (Tarjan's order).
func pureCalleesFirst(targets []*ssa.Function, callers map[*ssa.Function][]*ssa.Function, verdicts *tracer.PureVerdicts) [][]*ssa.Function {
	node := func(fn *ssa.Function) bool { return !verdicts.Failed[fn] && fn.Signature.Recv() != nil }
	callees := make(map[*ssa.Function][]*ssa.Function)
	var nodes []*ssa.Function
	for _, fn := range targets {
		if !node(fn) {
			continue
		}
		nodes = append(nodes, fn)
		for _, caller := range callers[fn] {
			if node(caller) {
				callees[caller] = append(callees[caller], fn)
			}
		}
	}
	index := make(map[*ssa.Function]int)
	low := make(map[*ssa.Function]int)
	onStack := make(map[*ssa.Function]bool)
	var stack []*ssa.Function
	var order [][]*ssa.Function
	var visit func(fn *ssa.Function)
	visit = func(fn *ssa.Function) {
		index[fn] = len(index) + 1
		low[fn] = index[fn]
		stack = append(stack, fn)
		onStack[fn] = true
		for _, callee := range callees[fn] {
			if index[callee] == 0 {
				visit(callee)
				low[fn] = min(low[fn], low[callee])
			} else if onStack[callee] {
				low[fn] = min(low[fn], index[callee])
			}
		}
		if low[fn] != index[fn] {
			return
		}
		var scc []*ssa.Function
		for {
			top := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			onStack[top] = false
			scc = append(scc, top)
			if top == fn {
				break
			}
		}
		order = append(order, scc)
	}
	for _, fn := range nodes {
		if index[fn] == 0 {
			visit(fn)
		}
	}
	return order
}

// pureCallers maps each function to the functions in targets whose
// validation may read its verdict: those that call it or refer to it, in
// their own body or in a closure inside it. Verdicts are kept by declared
// function (origin.Declared), so a generic instance or a bound method
// wrapper counts as the function it stands for.
func pureCallers(targets []*ssa.Function, funcVars callsite.FuncVars) map[*ssa.Function][]*ssa.Function {
	callers := make(map[*ssa.Function][]*ssa.Function)
	for _, target := range targets {
		seen := make(map[*ssa.Function]bool)
		add := func(fn *ssa.Function) {
			if fn = origin.Declared(fn); fn != nil && !seen[fn] {
				seen[fn] = true
				callers[fn] = append(callers[fn], target)
			}
		}
		var walk func(fn *ssa.Function)
		walk = func(fn *ssa.Function) {
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
	return callers
}
