// Package internal provides SSA-based analysis for GORM *gorm.DB reuse detection.
//
// # Architecture
//
// This package serves as the bridge between the public analyzer and the
// internal SSA analysis machinery:
//
//	┌─────────────────────────────────────────────────────────────────────────┐
//	│                         Analysis Flow                                    │
//	│                                                                          │
//	│   analyzer.go (public)                                                   │
//	│        │                                                                 │
//	│        ▼                                                                 │
//	│   internal/analyzer.go   ◀── You are here                                │
//	│   ┌─────────────────────────────────────────────────────────────────┐   │
//	│   │  RunSSA()                                                       │   │
//	│   │    │                                                            │   │
//	│   │    ├── Skip excluded (generated) files                          │   │
//	│   │    ├── Validate pure function contracts                         │   │
//	│   │    ├── Run SSA analysis (ssa.Analyzer)                          │   │
//	│   │    └── Apply ignore directives to every diagnostic              │   │
//	│   └─────────────────────────────────────────────────────────────────┘   │
//	│        │                                                                 │
//	│        ▼                                                                 │
//	│   internal/ssa/analyzer.go                                               │
//	│   (Core SSA analysis)                                                    │
//	└─────────────────────────────────────────────────────────────────────────┘
//
// # Responsibilities
//
//   - Orchestrate SSA analysis for all source functions
//   - Apply line, function and file ignore directives to every diagnostic
//   - Report unused directives (ignore, pure, immutable-return)
//   - Validate pure function contracts
//   - Export what the directive functions do as facts, and read the facts of
//     callees in other packages (#195)
package internal

import (
	"cmp"
	"fmt"
	"go/token"
	"go/types"
	"os"
	"slices"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/buildssa"
	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/directive"
	"github.com/mpyw/gormreuse/internal/facts"
	"github.com/mpyw/gormreuse/internal/fix"
	"github.com/mpyw/gormreuse/internal/scopeswarn"
	ssautil "github.com/mpyw/gormreuse/internal/ssa"
	"github.com/mpyw/gormreuse/internal/ssa/callback"
	"github.com/mpyw/gormreuse/internal/ssa/callsite"
	"github.com/mpyw/gormreuse/internal/ssa/pollution"
	"github.com/mpyw/gormreuse/internal/ssa/purity"
	"github.com/mpyw/gormreuse/internal/ssa/tracer"
)

// =============================================================================
// Entry Point
// =============================================================================

// RunSSA performs SSA-based analysis for GORM *gorm.DB reuse detection.
//
// This is the main entry point called from the public analyzer. It processes
// all source functions in the package and reports violations.
//
// Processing flow for each function:
//  1. Skip if file is excluded (generated files, etc.)
//  2. Validate pure function contract if marked with //gormreuse:pure
//  3. Run SSA analysis and collect violations
//  4. Report violations (unless suppressed by an ignore directive)
//  5. Report unused ignore directives
//
// An ignored function is still analyzed and its contracts still validated: an
// ignore only suppresses reports (#190). A //gormreuse:pure function that leaks
// its argument is therefore distrusted at call sites even when its own report
// is ignored, and a function-level ignore that suppresses nothing is reported
// unused.
func RunSSA(
	pass *analysis.Pass,
	ssaInfo *buildssa.SSA,
	ignores *directive.Ignores,
	pureFuncs, immutableReturnFuncs, immutableParamFuncs *directive.DirectiveFuncSet,
	immutableInputSet *directive.ImmutableInputSet,
	skipFiles map[string]bool,
) {
	// Share a single reported map across all functions to deduplicate
	// violations across parent functions and their closures.
	// When a closure accesses a parent scope variable, the same violation
	// may be detected from both the parent function and the closure.
	globalReported := make(map[reportKey]bool)

	// Every per-function step runs under guard, which reports a panic at the
	// function instead of dropping its diagnostics silently (#186).
	guard := &panicGuard{pass: pass, reported: make(map[*ssa.Function]bool)}

	// skip reports whether a function is in an excluded file. Ignore directives
	// do not skip anything: they only suppress reports (see report).
	skip := func(fn *ssa.Function) bool {
		pos := fn.Pos()
		return !pos.IsValid() || skipFiles[pass.Fset.PositionFor(pos, false).Filename]
	}

	// report emits a diagnostic unless an ignore directive suppresses it. Every
	// diagnostic except unused-ignore and internal-error reports goes through it.
	report := func(pos token.Pos, message string) {
		if !ignores.Ignore(pos) {
			pass.Reportf(pos, "%s", message)
		}
	}

	// ssaInfo is nil for a package that does not reach gorm.io/gorm: it holds
	// no *gorm.DB, so the passes below would find nothing, and the analyzer
	// does not build its SSA (#195). The reports that need no SSA remain.
	if ssaInfo == nil {
		reportUnusedImmutableInputs(immutableInputSet, report)
		reportUnusedDirectiveFuncs(pureFuncs, immutableReturnFuncs, immutableParamFuncs, report)
		reportUnusedIgnores(pass, ignores)
		return
	}

	// Analyze the closures of package-level `var f = func(...)` initializers
	// too, and resolve calls through such variables (#183).
	funcs := analyzedFuncs(ssaInfo)
	funcVars := callsite.CollectFuncVars(append(slices.Clone(funcs), ssaInfo.Pkg.Func("init")))

	// Callees in other packages are decided by the facts their packages
	// exported (#195). A package without facts leaves its functions unmarked.
	table := facts.NewTable(pass)
	pureFuncs.Import(func(fn *ssa.Function) bool { return table.Of(fn).Pure })
	immutableReturnFuncs.Import(func(fn *ssa.Function) bool { return table.Of(fn).ImmutableReturn })
	immutableInputSet.Import(func(fn *ssa.Function) []directive.ImmutableInputCallback { return table.Of(fn).ImmutableInput })

	// PASS 1: validate pure function contracts and collect the functions that
	// definitively broke them, and the parameters each trusted one may return.
	// A //gormreuse:pure function that broke the contract must not be trusted
	// as pure at its call sites (issue #66), and the result of one that keeps
	// it may be its argument (#188), so this must complete for ALL functions
	// before the analysis pass runs — a caller may be visited before its callee.
	pureVerdicts := validatePureFuncs(funcs, pureFuncs, immutableReturnFuncs, funcVars, table, skip, report, guard)

	// Collect Scopes/Preload callbacks once: their *gorm.DB parameter receives a
	// mid-chain (clone==0) value, so reuse inside them must be detected (#60).
	scopesCallbacks := callback.CollectScopesCallbacks(funcs)

	// Collect immutable callbacks once: gorm's Transaction/FindInBatches hand
	// their callback a fresh forkable (clone>0) handle, and a user function
	// declared //gormreuse:immutable-input(cb) promises the same for cb. Their
	// callback's tx parameter is therefore exempt from the Phase 1b
	// mutable-by-default treatment (#60 SC103, #61, #62 case 2.2).
	immutableCallbacks := callback.CollectImmutableCallbacks(funcs)
	callback.CollectImmutableInputCallbacks(funcs, immutableInputSet, immutableCallbacks)
	// The sets are keyed by function, so keep only the functions that nothing
	// else can call: a named callback that is also called directly, or that is
	// exported, keeps a mutable parameter (#191).
	callback.DropReachableCallbacks(funcs, immutableInputSet, immutableCallbacks)

	// Enforce the body-side immutable-input contract (#62 cases 2.3/2.4) and
	// report unused immutable-input directives (U1-U3). Uses a tracer with the
	// full context so FindMutableRoot classifies immutable sources correctly.
	inputTracer := tracer.New(pureFuncs, immutableReturnFuncs, immutableParamFuncs, pureVerdicts, scopesCallbacks, immutableCallbacks, funcVars)
	failedInput := make(map[*ssa.Function]bool)
	for _, fn := range funcs {
		if skip(fn) {
			continue
		}
		guard.run(fn, func() {
			for _, v := range purity.ValidateImmutableInputs(fn, immutableInputSet, inputTracer) {
				failedInput[fn] = true
				report(v.Pos, v.Message)
			}
		})
	}
	reportUnusedImmutableInputs(immutableInputSet, report)

	// Enforce the body-side immutable-return contract. Reuses inputTracer, which
	// carries the full pass context so other immutable-return / immutable-param
	// functions are classified correctly.
	failedReturn := reportImmutableReturnViolations(funcs, immutableReturnFuncs, inputTracer, skip, report, guard)

	// TEMPORARY (GORM bug go-gorm/gorm#7592): warn on Session/WithContext/Debug
	// results returned from Scopes callbacks. Deletable by removing the scopeswarn
	// package and this block once the upstream fix ships in a supported release — see that package.
	scopesOnly := callback.CollectScopesOnlyCallbacks(funcs)
	for _, w := range scopeswarn.Validate(scopesOnly, skip) {
		report(w.Pos, w.Message)
	}

	// Share a single fix generator across all violations (it caches AST
	// inspectors). It needs scopesCallbacks to withhold the immutable-param fix on
	// Scopes/Preload callbacks, whose parameters cannot be exempted (stage 2c).
	// inputTracer gives it the analysis' own mutability rule, and the source
	// functions let it check every caller before it suggests immutable-param
	// (#201).
	fixGen := fix.New(pass, scopesCallbacks, inputTracer, ssaInfo.SrcFuncs)

	// Determine which //gormreuse:immutable-param functions actually rely on
	// immutability — they would reuse a *gorm.DB parameter if it were treated as
	// mutable. This single classification drives two things: the caller-side
	// contract check (stage 2b, passed into the checker below) and, by its
	// complement, redundant-directive detection (a directive whose function does
	// NOT reuse a param suppresses nothing).
	needsImmutableParam := computeNeedsImmutableParam(funcs, immutableParamFuncs, pureFuncs, immutableReturnFuncs, pureVerdicts, scopesCallbacks, immutableCallbacks, funcVars, skip, guard)

	// The caller-side check also covers callees in other packages (#195).
	branchesImmutableParam := func(fn *ssa.Function) bool {
		return needsImmutableParam[fn] || table.Of(fn).BranchesImmutableParam
	}

	// PASS 2: run SSA reuse analysis.
	for _, fn := range funcs {
		if skip(fn) {
			continue
		}

		chk := newChecker(pass, ignores, pureFuncs, immutableReturnFuncs, immutableParamFuncs, pureVerdicts, scopesCallbacks, immutableCallbacks, branchesImmutableParam, funcVars, globalReported, fixGen)
		guard.run(fn, func() { chk.checkFunction(fn) })
	}

	// Report immutable-param directives that are signature-valid but have no
	// effect (no *gorm.DB parameter is reused).
	reportRedundantImmutableParam(funcs, immutableParamFuncs, pureFuncs, needsImmutableParam, skip, report)

	reportUnusedDirectiveFuncs(pureFuncs, immutableReturnFuncs, immutableParamFuncs, report)

	exportFacts(pass, ssaInfo, factSources{
		pureFuncs:            pureFuncs,
		immutableReturnFuncs: immutableReturnFuncs,
		immutableInputSet:    immutableInputSet,
		pureVerdicts:         pureVerdicts,
		failedReturn:         failedReturn,
		failedInput:          failedInput,
		needsImmutableParam:  needsImmutableParam,
		tracer:               inputTracer,
	}, guard)

	// Report unused ignore directives last, once every other diagnostic has had
	// the chance to use one.
	reportUnusedIgnores(pass, ignores)
}

// ReportInOrder holds back every diagnostic pass reports until flush, which
// reports them sorted by file, offset and message (#273). Some diagnostics come
// from map ranges, such as the unused-directive ones, and the -json output
// lists diagnostics in report order, unlike the text output, which the driver
// sorts. Sorting them all keeps -json output the same on every run.
//
// Positions are compared by file name and offset, not as token.Pos: the
// loader parses files concurrently, so their order in the file set can vary.
func ReportInOrder(pass *analysis.Pass) (flush func()) {
	report := pass.Report
	var diags []analysis.Diagnostic
	pass.Report = func(d analysis.Diagnostic) { diags = append(diags, d) }
	return func() {
		pass.Report = report
		slices.SortStableFunc(diags, func(a, b analysis.Diagnostic) int {
			pa, pb := pass.Fset.PositionFor(a.Pos, false), pass.Fset.PositionFor(b.Pos, false)
			return cmp.Or(cmp.Compare(pa.Filename, pb.Filename), cmp.Compare(pa.Offset, pb.Offset), cmp.Compare(a.Message, b.Message))
		})
		for _, d := range diags {
			report(d)
		}
	}
}

// reportUnusedImmutableInputs reports the immutable-input directives that name
// no usable callback or are attached to no function.
func reportUnusedImmutableInputs(set *directive.ImmutableInputSet, report func(token.Pos, string)) {
	for _, u := range set.GetUnused() {
		report(u.Pos, u.Reason)
	}
}

// reportUnusedIgnores reports the ignore directives that suppressed nothing.
// No ignore suppresses these.
func reportUnusedIgnores(pass *analysis.Pass, ignores *directive.Ignores) {
	for _, pos := range ignores.UnusedIgnores() {
		pass.Reportf(pos, "unused gormreuse:ignore directive")
	}
}

// analyzedFuncs returns the functions the analysis runs on: the source
// functions of the package, then the closures of package-level initializers
// such as `var f = func(db *gorm.DB) { ... }` (#183), nested ones included.
// Those closures belong to the synthetic package init function, which
// buildssa leaves out of SrcFuncs.
func analyzedFuncs(ssaInfo *buildssa.SSA) []*ssa.Function {
	funcs := slices.Clone(ssaInfo.SrcFuncs)
	if init := ssaInfo.Pkg.Func("init"); init != nil {
		var addAnons func(fn *ssa.Function)
		addAnons = func(fn *ssa.Function) {
			for _, anon := range fn.AnonFuncs {
				funcs = append(funcs, anon)
				addAnons(anon)
			}
		}
		addAnons(init)
	}
	return funcs
}

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
func validatePureFuncs(
	funcs []*ssa.Function,
	pureFuncs, immutableReturnFuncs *directive.DirectiveFuncSet,
	funcVars callsite.FuncVars,
	table *facts.Table,
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
		guard.run(fn, func() {
			validator := purity.NewValidator(fn, pureFuncs, immutableReturnFuncs, verdicts, funcVars)
			// Only a definitive violation revokes pure-trust at call sites;
			// conservative func-arg violations do not (avoids FP cascades).
			if slices.ContainsFunc(validator.Validate(), func(v purity.Violation) bool { return v.Definitive }) {
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

	// A closure body is validated both as its own pure function and as part of
	// a pure function that creates it, so the same violation can come twice.
	reported := make(map[purity.Violation]bool)
	for _, fn := range targets {
		guard.run(fn, func() {
			for _, v := range purity.NewValidator(fn, pureFuncs, immutableReturnFuncs, verdicts, funcVars).Validate() {
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

// pureCallers maps each function to the functions in targets whose
// validation may read its verdict: those that call it or refer to it, in
// their own body or in a closure inside it. Verdicts are kept by declared
// function (directive.DeclaredFunc), so a generic instance or a bound method
// wrapper counts as the function it stands for.
func pureCallers(targets []*ssa.Function, funcVars callsite.FuncVars) map[*ssa.Function][]*ssa.Function {
	callers := make(map[*ssa.Function][]*ssa.Function)
	for _, target := range targets {
		seen := make(map[*ssa.Function]bool)
		add := func(fn *ssa.Function) {
			if fn = directive.DeclaredFunc(fn); fn != nil && !seen[fn] {
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

// reportImmutableReturnViolations enforces the body-side immutable-return
// contract: a function marked //gormreuse:immutable-return must actually return
// an immutable *gorm.DB. Otherwise the tracer trusts the directive
// (returnsImmutable) and silently allows unsafe reuse of the actually-mutable
// return value at every call site. It returns the functions that break the contract.
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

// factSources is what exportFacts reads: the directive sets and what
// validating each contract found.
type factSources struct {
	pureFuncs, immutableReturnFuncs *directive.DirectiveFuncSet
	immutableInputSet               *directive.ImmutableInputSet
	pureVerdicts                    *tracer.PureVerdicts
	failedReturn, failedInput       map[*ssa.Function]bool
	needsImmutableParam             map[*ssa.Function]bool
	tracer                          *tracer.RootTracer
}

// exportFacts exports a facts.Func for every exported function and method of
// the package that carries a directive, so that the packages that
// call it see what this pass found (#195):
//
//   - pure, whether the body broke the contract, and which parameters the
//     result may be;
//   - immutable-return, only when the body keeps the contract;
//   - immutable-param, only when the function branches a parameter, which is
//     when its callers must pass an isolated value;
//   - immutable-input(name), only when the body keeps the contract.
//
// A contract that the body breaks is reported here, at the declaration, and
// still trusted in this package so that the report is not repeated at every
// call. Another package does not trust it: its author may never see this
// package's reports.
func exportFacts(pass *analysis.Pass, ssaInfo *buildssa.SSA, src factSources, guard *panicGuard) {
	for _, fn := range ssaInfo.SrcFuncs {
		if obj, ok := fn.Object().(*types.Func); !ok || !obj.Exported() {
			continue // another package cannot name it
		}
		guard.run(fn, func() {
			var f facts.Func
			if src.pureFuncs.Contains(fn) {
				f.Pure = true
				f.PureFailed = src.pureVerdicts.Failed[fn]
				f.Returned = src.pureVerdicts.Returned[fn]
				f.ReturnsOnlyParams = len(f.Returned) > 0 && src.tracer.ReturnsOnlyParams(fn)
			}
			f.ImmutableReturn = src.immutableReturnFuncs.Contains(fn) && !src.failedReturn[fn]
			f.BranchesImmutableParam = src.needsImmutableParam[fn]
			if !src.failedInput[fn] {
				f.ImmutableInput = src.immutableInputSet.Callbacks(fn)
			}
			facts.Export(pass, fn, f)
		})
	}
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
func computeNeedsImmutableParam(
	funcs []*ssa.Function,
	immutableParamFuncs, pureFuncs, immutableReturnFuncs *directive.DirectiveFuncSet,
	pureVerdicts *tracer.PureVerdicts,
	scopesCallbacks, immutableCallbacks map[*ssa.Function]bool,
	funcVars callsite.FuncVars,
	skip func(*ssa.Function) bool,
	guard *panicGuard,
) map[*ssa.Function]bool {
	needs := make(map[*ssa.Function]bool)
	if immutableParamFuncs == nil {
		return needs
	}
	for _, fn := range funcs {
		if skip(fn) || !immutableParamFuncs.Contains(fn) {
			continue
		}
		if fn.Signature == nil || !directive.HasGormDBParameter(fn.Signature) {
			continue
		}
		if pureFuncs != nil && pureFuncs.Contains(fn) {
			continue
		}
		guard.run(fn, func() {
			// Counterfactual: analyze fn with its parameters treated as mutable.
			cf := ssautil.NewAnalyzer(fn, pureFuncs, immutableReturnFuncs, nil, pureVerdicts, scopesCallbacks, immutableCallbacks, nil, funcVars)
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
func reportRedundantImmutableParam(
	funcs []*ssa.Function,
	immutableParamFuncs, pureFuncs *directive.DirectiveFuncSet,
	needsImmutableParam map[*ssa.Function]bool,
	skip func(*ssa.Function) bool,
	report func(token.Pos, string),
) {
	if immutableParamFuncs == nil {
		return
	}
	for _, fn := range funcs {
		if skip(fn) || !immutableParamFuncs.Contains(fn) {
			continue
		}
		if fn.Signature == nil || !directive.HasGormDBParameter(fn.Signature) {
			continue // signature-invalid: handled by reportUnusedDirectiveFuncs
		}
		if pureFuncs != nil && pureFuncs.Contains(fn) {
			continue // pure ⇒ param never branched ⇒ redundant by construction (not flagged)
		}
		if !needsImmutableParam[fn] {
			report(fn.Pos(), "redundant gormreuse:immutable-param directive: no *gorm.DB parameter is reused")
		}
	}
}

// reportUnusedDirectiveFuncs reports pure / immutable-return / immutable-param
// directives that matched no valid function. For combined directives (e.g.
// //gormreuse:pure,immutable-return,immutable-param) a directive at a position
// is "used" if ANY of its combined siblings is used, so each set is suppressed
// when another set reports that position as used.
func reportUnusedDirectiveFuncs(pureFuncs, immutableReturnFuncs, immutableParamFuncs *directive.DirectiveFuncSet, report func(token.Pos, string)) {
	usedByOther := func(pos token.Pos, others ...*directive.DirectiveFuncSet) bool {
		return slices.ContainsFunc(others, func(s *directive.DirectiveFuncSet) bool {
			return s != nil && s.IsUsed(pos)
		})
	}
	reportSet := func(set *directive.DirectiveFuncSet, message string, others ...*directive.DirectiveFuncSet) {
		if set == nil {
			return
		}
		for _, pos := range set.GetUnusedDirectives() {
			if usedByOther(pos, others...) {
				continue
			}
			report(pos, message)
		}
	}

	reportSet(pureFuncs, "unused gormreuse:pure directive", immutableReturnFuncs, immutableParamFuncs)
	reportSet(immutableReturnFuncs, "unused gormreuse:immutable-return directive", pureFuncs, immutableParamFuncs)
	reportSet(immutableParamFuncs, "unused gormreuse:immutable-param directive", pureFuncs, immutableReturnFuncs)
}

// panicGuard runs per-function work and recovers from any panic, so that a
// single pathological function (exotic SSA the tracer mishandles) cannot abort
// the entire `go vet` run for the package. Aborting would be worse than any
// false positive and contrary to the conservative-bias design.
//
// A recovered panic is reported at the function as an internal error, once per
// function, so that the diagnostics it may have lost are not dropped silently
// (#186). The report goes through pass.Reportf directly, so no ignore directive
// suppresses it.
//
// Set GORMREUSE_DEBUG_PANIC to a non-empty value to re-panic instead, surfacing
// the stack trace for debugging.
type panicGuard struct {
	pass     *analysis.Pass
	reported map[*ssa.Function]bool
}

func (g *panicGuard) run(fn *ssa.Function, work func()) {
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		if os.Getenv("GORMREUSE_DEBUG_PANIC") != "" {
			panic(fmt.Sprintf("gormreuse: panic analyzing %s: %v", fn, r))
		}
		if g.reported[fn] {
			return
		}
		g.reported[fn] = true
		g.pass.Reportf(fn.Pos(), "gormreuse: internal error analyzing %s: %v; please report this at https://github.com/mpyw/gormreuse/issues", fn.RelString(fn.Pkg.Pkg), r)
	}()
	work()
}

// =============================================================================
// SSA Checker
// =============================================================================

// checker wraps SSA analysis with ignore directive handling.
//
// It ensures:
//   - Violations at the same position are only reported once
//   - Ignore directives suppress violations
//   - Violations are reported through the analysis.Pass
type checker struct {
	pass                 *analysis.Pass              // For reporting diagnostics
	ignores              *directive.Ignores          // Line, function and file ignore directives
	pureFuncs            *directive.DirectiveFuncSet // Pure functions for analysis
	immutableReturnFuncs *directive.DirectiveFuncSet // Immutable-return functions
	immutableParamFuncs  *directive.DirectiveFuncSet // Immutable-param functions (params opt out of Phase 1b)
	pureVerdicts         *tracer.PureVerdicts        // What validating the pure functions found
	scopesCallbacks      map[*ssa.Function]bool      // Scopes/Preload callbacks (params are mutable roots)
	immutableCallbacks   map[*ssa.Function]bool      // Transaction/FindInBatches callbacks (fresh tx)
	needsImmutableParam  func(*ssa.Function) bool    // immutable-param fns that branch a param (2b caller check)
	funcVars             callsite.FuncVars           // Package-level variables that hold one function (#183)
	reported             map[reportKey]bool          // Deduplication of reports
	fixGen               *fix.Generator              // Cached fix generator for all violations
}

// newChecker creates a new checker for a specific file.
// The reported map is shared across all functions to deduplicate violations
// across parent functions and their closures.
// The fixGen is shared to avoid recreating the generator for each violation.
func newChecker(pass *analysis.Pass, ignores *directive.Ignores, pureFuncs, immutableReturnFuncs, immutableParamFuncs *directive.DirectiveFuncSet, pureVerdicts *tracer.PureVerdicts, scopesCallbacks, immutableCallbacks map[*ssa.Function]bool, needsImmutableParam func(*ssa.Function) bool, funcVars callsite.FuncVars, reported map[reportKey]bool, fixGen *fix.Generator) *checker {
	return &checker{
		pass:                 pass,
		ignores:              ignores,
		pureFuncs:            pureFuncs,
		immutableReturnFuncs: immutableReturnFuncs,
		immutableParamFuncs:  immutableParamFuncs,
		pureVerdicts:         pureVerdicts,
		scopesCallbacks:      scopesCallbacks,
		immutableCallbacks:   immutableCallbacks,
		needsImmutableParam:  needsImmutableParam,
		funcVars:             funcVars,
		reported:             reported,
		fixGen:               fixGen,
	}
}

// checkFunction runs SSA analysis on a single function and reports violations.
func (c *checker) checkFunction(fn *ssa.Function) {
	analyzer := ssautil.NewAnalyzer(fn, c.pureFuncs, c.immutableReturnFuncs, c.immutableParamFuncs, c.pureVerdicts, c.scopesCallbacks, c.immutableCallbacks, c.needsImmutableParam, c.funcVars)
	violations := analyzer.Analyze()

	// Every diagnostic of a root carries the root's fix, not only the first:
	// the first may be suppressed by an ignore directive or already reported,
	// and an editor offers a quick fix only on the diagnostic that carries it
	// (#246). Each fix is complete on its own (#200), so the copies are
	// identical, and the drivers apply an edit that several fixes share once.
	// A root's fix is generated once, for the first diagnostic of the root that
	// is reported. It depends only on the root and its uses.
	fixes := make(map[ssa.Value][]analysis.SuggestedFix)
	fixOf := func(v pollution.Violation) []analysis.SuggestedFix {
		f, ok := fixes[v.Root]
		if !ok {
			f = c.fixGen.Generate(v)
			fixes[v.Root] = f
		}
		return f
	}

	// Group violations by position. Several roots can be reused at one position
	// (use2(q, r)), and only one diagnostic is reported there, so it carries the
	// fix of every root in the group. Otherwise only the first root would get a
	// fix (#184). A violation with no root, such as a read-only argument that
	// sees a change, is another bug, and is reported on its own (see
	// reportKey).
	for _, group := range groupByPos(violations) {
		var reuse, fixFor []pollution.Violation
		for _, v := range group {
			if v.Root == nil {
				c.reportViolation(v, nil, fixOf)
				continue
			}
			reuse = append(reuse, v)
			if !slices.ContainsFunc(fixFor, func(f pollution.Violation) bool { return f.Root == v.Root }) {
				fixFor = append(fixFor, v)
			}
		}
		if len(reuse) > 0 {
			c.reportViolation(reuse[0], fixFor, fixOf)
		}
	}
}

// reportKey identifies a diagnostic for deduplication. A position has one
// reuse diagnostic, which names one of the roots reused there and carries the
// fixes of all of them, and one diagnostic of each other message. Reporting
// only the first diagnostic at a position hid the others until a fix removed
// it, as for `q = q.Where(r); q.Count(nil); r.Find(nil); q.Find(nil)`, where
// q.Find both reuses q and reads r after r.Find changed it (#247).
type reportKey struct {
	pos     token.Pos
	message string // "" for the reuse diagnostic
}

// keyOf returns the reportKey of v.
func keyOf(v pollution.Violation) reportKey {
	if v.Root != nil {
		return reportKey{pos: v.Pos}
	}
	return reportKey{pos: v.Pos, message: v.Message}
}

// groupByPos groups violations by position. Groups keep the order in which
// their position first appears, and violations keep their order in a group.
func groupByPos(violations []pollution.Violation) [][]pollution.Violation {
	var groups [][]pollution.Violation
	index := make(map[token.Pos]int)
	for _, v := range violations {
		i, ok := index[v.Pos]
		if !ok {
			i = len(groups)
			index[v.Pos] = i
			groups = append(groups, nil)
		}
		groups[i] = append(groups[i], v)
	}
	return groups
}

// reportViolation reports v with the suggested fixes, given by fixOf, for the
// roots of fixFor. The fixes are merged into one, because drivers apply only
// the first suggested fix of a diagnostic.
func (c *checker) reportViolation(v pollution.Violation, fixFor []pollution.Violation, fixOf func(pollution.Violation) []analysis.SuggestedFix) {
	pos := v.Pos

	// Deduplicate: same position may be reached multiple times
	if c.reported[keyOf(v)] {
		return
	}
	c.reported[keyOf(v)] = true

	if c.ignores.Ignore(pos) {
		return // Suppressed by ignore directive
	}

	var suggestedFixes []analysis.SuggestedFix
	for _, fv := range fixFor {
		suggestedFixes = append(suggestedFixes, fixOf(fv)...)
	}

	// Each diagnostic carries every edit its roots need, even one that another
	// diagnostic carries too, such as a shared Phi edge's Session or the gorm
	// import. Applying one diagnostic's fix alone must compile (#200). Drivers
	// that apply several fixes, such as -fix, merge identical edits.
	suggestedFixes = mergeFixes(suggestedFixes)

	// Report with diagnostic
	c.pass.Report(analysis.Diagnostic{
		Pos:            pos,
		Message:        v.Message,
		SuggestedFixes: suggestedFixes,
	})
}

// mergeFixes combines fixes into one fix. Its edits are sorted by position,
// an edit that two roots both need appears once, and its message joins the
// distinct messages in order.
func mergeFixes(fixes []analysis.SuggestedFix) []analysis.SuggestedFix {
	if len(fixes) <= 1 {
		return fixes
	}
	var messages []string
	var edits []analysis.TextEdit
	for _, f := range fixes {
		if !slices.Contains(messages, f.Message) {
			messages = append(messages, f.Message)
		}
		for _, e := range f.TextEdits {
			if !slices.ContainsFunc(edits, func(x analysis.TextEdit) bool {
				return x.Pos == e.Pos && x.End == e.End && string(x.NewText) == string(e.NewText)
			}) {
				edits = append(edits, e)
			}
		}
	}
	slices.SortStableFunc(edits, func(a, b analysis.TextEdit) int { return cmp.Compare(a.Pos, b.Pos) })
	return []analysis.SuggestedFix{{Message: strings.Join(messages, "; "), TextEdits: edits}}
}
