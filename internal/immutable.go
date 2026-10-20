package internal

import (
	"go/token"
	"slices"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/directive"
	ssautil "github.com/mpyw/gormreuse/internal/ssa"
	"github.com/mpyw/gormreuse/internal/ssa/callsite"
	"github.com/mpyw/gormreuse/internal/ssa/cfg"
	"github.com/mpyw/gormreuse/internal/ssa/pollution"
	"github.com/mpyw/gormreuse/internal/ssa/purity"
	"github.com/mpyw/gormreuse/internal/ssa/tracer"
	"github.com/mpyw/gormreuse/internal/typeutil"
)

// reportImmutableReturnViolations enforces the body-side immutable-return
// contract: a function marked //gormreuse:immutable-return must actually return
// an immutable *gorm.DB. Otherwise the tracer trusts the directive
// (returnsImmutable) and silently allows unsafe reuse of the actually-mutable
// return value at every call site. It returns the functions that break the contract.
//
//declscope:shared // run.go reports the immutable-return functions that break their contract
func reportImmutableReturnViolations(
	funcs []*ssa.Function,
	immutableReturnFuncs *directive.DirectiveFuncSet,
	rt *tracer.RootTracer,
	skip func(*ssa.Function) bool,
	report func(token.Pos, string),
	guard *panicGuard,
) map[*ssa.Function]bool {
	failed := make(map[*ssa.Function]bool)
	validator := purity.NewImmutableReturnValidator(funcs, immutableReturnFuncs, rt)
	for _, fn := range funcs {
		if skip(fn) {
			continue
		}
		guard.run(fn, func() {
			for _, v := range validator.Validate(fn) {
				failed[fn] = true
				report(v.Pos, v.Message)
			}
		})
	}
	return failed
}

// inferImmutableReturns adds to set the functions of funcs that have no
// immutable-return directive but whose body proves that every *gorm.DB they
// return is immutable (#261). A function of a skipped file is not inferred,
// since its body is not analyzed.
//
// Here a function marked immutable-return counts as immutable even when its
// body breaks the contract, as everywhere else in the package: that body is
// reported once, at its declaration. distrustContracts keeps such trust out
// of the facts.
//
//declscope:shared // run.go infers the immutable-return functions
func inferImmutableReturns(funcs []*ssa.Function, set *directive.DirectiveFuncSet, funcVars callsite.FuncVars, skip func(*ssa.Function) bool) {
	var candidates []*ssa.Function
	for _, fn := range funcs {
		if !skip(fn) && !set.Contains(fn) && immutableReturnCandidate(fn) {
			candidates = append(candidates, fn)
		}
	}
	builtins := tracer.NewRootTracer(nil, nil, nil, nil, nil, nil, funcVars)
	inferred := purity.InferImmutableReturns(candidates, set.Contains, builtins)
	for _, fn := range candidates {
		if inferred[fn] {
			set.Infer(fn)
		}
	}
}

// immutableReturnCandidate reports whether fn has a *gorm.DB result, which
// inference needs.
func immutableReturnCandidate(fn *ssa.Function) bool {
	for v := range fn.Signature.Results().Variables() {
		if typeutil.IsGormDB(v.Type()) {
			return true
		}
	}
	return false
}

// computeNeedsImmutableParam returns the //gormreuse:immutable-param functions
// that genuinely rely on immutability: analyzed with their parameters treated as
// mutable (a counterfactual with immutableParamFuncs = nil), they produce at
// least one violation rooted at one of their own parameters — i.e. they branch a
// parameter, so the directive suppresses a real diagnostic.
//
// The result classifies every valid immutable-param function:
//   - in the set   → the directive is necessary; a caller passing a mutable value
//     violates the contract (stage 2b caller-side check consumes this).
//   - not in the set → the directive suppresses nothing; it is redundant
//     (reportRedundantImmutableParam consumes the complement).
//
// Signature-invalid directives (no *gorm.DB parameter) are excluded — they are
// handled by the existing unused-directive path. Functions also marked
// //gormreuse:pure are excluded because a valid pure function cannot branch its
// parameter (branching is pollution the pure contract forbids), so they can never
// need — nor, for redundancy purposes, be flagged for — immutability.
//
// Known limitation: a function that ONLY forwards its parameter once to another
// immutable-param function (never using it a second time) is not detected as
// needing immutability — the counterfactual sees a single use, not a reuse — so
// it is reported redundant even though removing the directive would surface a 2b
// contract violation at the forwarding call. This is an uncommon delegation
// pattern; suppress with //gormreuse:ignore if intended.
//
//declscope:shared // run.go finds the parameters that need immutable-param
func computeNeedsImmutableParam(
	funcs []*ssa.Function,
	immutableParamFuncs, pureFuncs, immutableReturnFuncs *directive.DirectiveFuncSet,
	pureVerdicts *tracer.PureVerdicts,
	scopesCallbacks, immutableCallbacks map[*ssa.Function]bool,
	funcVars callsite.FuncVars,
	skip func(*ssa.Function) bool,
	guard *panicGuard,
	cfgs *cfg.Analyzer,
) map[*ssa.Function]bool {
	needs := make(map[*ssa.Function]bool)
	if immutableParamFuncs == nil {
		return needs
	}
	for _, fn := range funcs {
		if skip(fn) || !immutableParamFuncs.Contains(fn) {
			continue
		}
		if fn.Signature == nil || !typeutil.HasGormDBParameter(fn.Signature) {
			continue
		}
		if pureFuncs != nil && pureFuncs.Contains(fn) {
			continue
		}
		guard.run(fn, func() {
			// Counterfactual: analyze fn with its parameters treated as mutable.
			cf := ssautil.NewAnalyzer(fn, pureFuncs, immutableReturnFuncs, nil, pureVerdicts, scopesCallbacks, immutableCallbacks, nil, funcVars, cfgs)
			if slices.ContainsFunc(cf.Analyze(), func(v pollution.Violation) bool {
				p, ok := v.Root.(*ssa.Parameter)
				return ok && p.Parent() == fn
			}) {
				needs[fn] = true
			}
		})
	}
	return needs
}

// reportRedundantImmutableParam flags //gormreuse:immutable-param directives that
// are signature-valid (the function has a *gorm.DB parameter) yet have no effect:
// even if the parameter were treated as mutable it would never be reused, so the
// directive suppresses nothing. These are exactly the valid, non-pure directives
// NOT in the needsImmutableParam set (see computeNeedsImmutableParam).
//
// Scope note: Phase 1b stage 2a/2b makes immutable-param a callee-side contract
// plus a caller-side check keyed on needsImmutableParam. A redundant directive is
// (by construction) not in that set, so it triggers no caller-side obligation
// either — removing it changes no diagnostic, which is what "redundant" means.
//
// Signature-invalid directives (no *gorm.DB parameter) are reported as unused by
// reportUnusedDirectiveFuncs, not here; the same guards used when building
// needsImmutableParam keep the two paths from overlapping.
//
// The fix deletes immutable-param from the function's directive comments
// (#248). A comment before an assignment of several closures applies to each
// of them, so it is edited only when no other closure it covers keeps the
// directive.
//
//declscope:shared // run.go reports the immutable-param directives that change nothing
func reportRedundantImmutableParam(
	funcs []*ssa.Function,
	immutableParamFuncs, pureFuncs *directive.DirectiveFuncSet,
	needsImmutableParam map[*ssa.Function]bool,
	skip func(*ssa.Function) bool,
	report reportRemovalFunc,
) {
	if immutableParamFuncs == nil {
		return
	}
	var redundant []*ssa.Function
	kept := make(map[token.Pos]bool) // Comments that a function not reported keeps
	for _, fn := range funcs {
		if skip(fn) || !immutableParamFuncs.Contains(fn) {
			continue
		}
		if fn.Signature == nil || !typeutil.HasGormDBParameter(fn.Signature) {
			continue // signature-invalid: handled by reportUnusedDirectiveFuncs
		}
		// pure ⇒ param never branched ⇒ redundant by construction (not flagged)
		if pureFuncs != nil && pureFuncs.Contains(fn) || needsImmutableParam[fn] {
			for _, pos := range immutableParamFuncs.CommentsOf(fn) {
				kept[pos] = true
			}
			continue
		}
		redundant = append(redundant, fn)
	}
	for _, fn := range redundant {
		var targets []directive.RemovalTarget
		for _, pos := range immutableParamFuncs.CommentsOf(fn) {
			if kept[pos] {
				targets = nil
				break
			}
			targets = append(targets, directive.RemovalTarget{Comment: pos, Part: "immutable-param"})
		}
		report(fn.Pos(), "redundant gormreuse:immutable-param directive: no *gorm.DB parameter is reused", targets...)
	}
}
