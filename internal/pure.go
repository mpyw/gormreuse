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
// method on its receiver changes it too. Each method is checked once, and
// again only when a method it calls or refers to (callers, see pureCallers)
// turns out to change its receiver, so a chain of n methods takes about 2n
// checks. A check that panicked counts as a change.
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
	var queue []*ssa.Function
	queued := make(map[*ssa.Function]bool)
	enqueue := func(fn *ssa.Function) {
		if !queued[fn] && !changed[fn] && !verdicts.Failed[fn] && fn.Signature.Recv() != nil {
			queued[fn] = true
			queue = append(queue, fn)
		}
	}
	for _, fn := range targets {
		enqueue(fn)
	}
	for len(queue) > 0 {
		fn := queue[0]
		queue = queue[1:]
		queued[fn] = false
		yes := true
		guard.run("pure", fn, func() {
			yes = purity.NewValidator(fn, pureFuncs, immutableReturnFuncs, verdicts, funcVars).ChangesReceiver(changes)
		})
		if !yes {
			continue
		}
		changed[fn] = true
		for _, caller := range callers[fn] {
			enqueue(caller)
		}
	}
	return changed
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
