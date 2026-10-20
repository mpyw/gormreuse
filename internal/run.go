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
//	│   internal/run.go        ◀── You are here                                │
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
	"go/token"
	"slices"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/buildssa"
	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/directive"
	"github.com/mpyw/gormreuse/internal/facts"
	"github.com/mpyw/gormreuse/internal/fix"
	"github.com/mpyw/gormreuse/internal/scopeswarn"
	"github.com/mpyw/gormreuse/internal/ssa/callback"
	"github.com/mpyw/gormreuse/internal/ssa/callsite"
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

	// reportRemoval is report for a diagnostic whose fix deletes targets, the
	// parts of directive comments that have no effect (#248).
	removals := directive.NewRemovals(pass)
	reportRemoval := func(pos token.Pos, message string, targets ...directive.RemovalTarget) {
		if !ignores.Ignore(pos) {
			removals.Report(pos, message, targets...)
		}
	}

	// ssaInfo is nil for a package that does not reach gorm.io/gorm: it holds
	// no *gorm.DB, so the passes below would find nothing, and the analyzer
	// does not build its SSA (#195). The reports that need no SSA remain.
	if ssaInfo == nil {
		reportUnusedImmutableInputs(immutableInputSet, reportRemoval)
		reportUnusedDirectiveFuncs(pureFuncs, immutableReturnFuncs, immutableParamFuncs, reportRemoval)
		reportUnusedIgnores(removals, ignores, reportIgnoreRemovable(pass, false))
		removals.Flush()
		return
	}

	// Analyze the closures of package-level `var f = func(...)` initializers
	// too, and resolve calls through such variables (#183).
	funcs := runFuncs(ssaInfo)
	funcVars := callsite.CollectFuncVars(append(slices.Clone(funcs), ssaInfo.Pkg.Func("init")))

	// Callees in other packages are decided by the facts their packages
	// exported (#195). A package without facts leaves its functions unmarked.
	table := facts.NewTable(pass)
	pureFuncs.Import(func(fn *ssa.Function) bool { return table.Of(fn).Pure })
	immutableReturnFuncs.Import(func(fn *ssa.Function) bool { return table.Of(fn).ImmutableReturn })
	immutableInputSet.Import(func(fn *ssa.Function) []directive.ImmutableInputCallback { return table.Of(fn).ImmutableInput })
	// Only the immutable-param functions that branch a parameter export a
	// fact, and only those matter to a caller (#255).
	immutableParamFuncs.Import(func(fn *ssa.Function) bool { return table.Of(fn).BranchesImmutableParam })

	// Treat a function as immutable-return when its body proves it (#261),
	// before any pass reads the set.
	inferImmutableReturns(funcs, immutableReturnFuncs, funcVars, skip)

	// PASS 1: validate pure function contracts and collect the functions that
	// definitively broke them, and the parameters each trusted one may return.
	// A //gormreuse:pure function that broke the contract must not be trusted
	// as pure at its call sites (issue #66), and the result of one that keeps
	// it may be its argument (#188), so this must complete for ALL functions
	// before the analysis pass runs — a caller may be visited before its callee.
	pureVerdicts := validatePureFuncs(funcs, pureFuncs, immutableReturnFuncs, funcVars, table, skip, report, guard)

	// Collect Scopes/Preload callbacks once: their *gorm.DB parameter receives a
	// mid-chain (clone==0) value, so reuse inside them must be detected (#60).
	scopesCallbacks := callback.CollectScopesCallbacks(funcs, funcVars)

	// Collect immutable callbacks once: gorm's Transaction/FindInBatches hand
	// their callback a fresh forkable (clone>0) handle, and a user function
	// declared //gormreuse:immutable-input(cb) promises the same for cb. Their
	// callback's tx parameter is therefore exempt from the Phase 1b
	// mutable-by-default treatment (#60 SC103, #61, #62 case 2.2).
	immutableCallbacks := callback.CollectImmutableCallbacks(funcs, funcVars)
	callback.CollectImmutableInputCallbacks(funcs, funcVars, immutableInputSet, immutableCallbacks)
	// The sets are keyed by function, so keep only the functions that nothing
	// else can call: a named callback that is also called directly, or that is
	// exported, keeps a mutable parameter (#191).
	callback.DropReachableCallbacks(funcs, funcVars, immutableInputSet, immutableCallbacks)

	// Enforce the body-side immutable-input contract (#62 cases 2.3/2.4) and
	// report unused immutable-input directives (U1-U3). Uses a tracer with the
	// full context so FindMutableRoot classifies immutable sources correctly.
	inputTracer := tracer.NewRootTracer(pureFuncs, immutableReturnFuncs, immutableParamFuncs, pureVerdicts, scopesCallbacks, immutableCallbacks, funcVars)
	failedInput := make(map[*ssa.Function]bool)
	for _, fn := range funcs {
		if skip(fn) {
			continue
		}
		guard.run(fn, func() {
			for _, v := range purity.ValidateImmutableInputs(fn, immutableInputSet, inputTracer, nil) {
				failedInput[fn] = true
				report(v.Pos, v.Message)
			}
		})
	}
	reportUnusedImmutableInputs(immutableInputSet, reportRemoval)

	// Enforce the body-side immutable-return contract. Reuses inputTracer, which
	// carries the full pass context so other immutable-return / immutable-param
	// functions are classified correctly.
	failedReturn := reportImmutableReturnViolations(funcs, immutableReturnFuncs, inputTracer, skip, report, guard)

	// TEMPORARY (GORM bug go-gorm/gorm#7592): warn on Session/WithContext/Debug
	// results returned from Scopes callbacks. Deletable by removing the scopeswarn
	// package and this block once the upstream fix ships in a supported release — see that package.
	scopesOnly := callback.CollectScopesOnlyCallbacks(funcs, funcVars)
	for _, w := range scopeswarn.Validate(scopesOnly, funcVars, skip) {
		report(w.Pos, w.Message)
	}

	// Share a single fix generator across all violations (it caches AST
	// inspectors). It needs scopesCallbacks to withhold the immutable-param fix on
	// Scopes/Preload callbacks, whose parameters cannot be exempted (stage 2c).
	// inputTracer gives it the analysis' own mutability rule, and the analysed
	// functions let it check every caller, in package-level closures too,
	// before it suggests immutable-param (#201, #245).
	fixGen := fix.NewGenerator(pass, scopesCallbacks, inputTracer, funcs, reportSeesWholePackage(pass))

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
	reportRedundantImmutableParam(funcs, immutableParamFuncs, pureFuncs, needsImmutableParam, skip, reportRemoval)

	reportUnusedDirectiveFuncs(pureFuncs, immutableReturnFuncs, immutableParamFuncs, reportRemoval)

	exportFacts(pass, ssaInfo, factSources{
		pureFuncs:            pureFuncs,
		immutableReturnFuncs: immutableReturnFuncs,
		immutableInputSet:    immutableInputSet,
		pureVerdicts:         pureVerdicts,
		distrusted: distrustContracts(distrustSources{
			funcs:                funcs,
			immutableReturnFuncs: immutableReturnFuncs,
			immutableInputSet:    immutableInputSet,
			failedReturn:         failedReturn,
			failedInput:          failedInput,
			tracer:               inputTracer,
			funcVars:             funcVars,
			skip:                 skip,
		}, guard),
		needsImmutableParam: needsImmutableParam,
		tracer:              inputTracer,
	}, skip, guard)

	// Report unused ignore directives last, once every other diagnostic has had
	// the chance to use one. A function whose analysis panicked lost its
	// diagnostics, so an ignore that looks unused may cover one of them: no
	// fix deletes an ignore then.
	reportUnusedIgnores(removals, ignores, reportIgnoreRemovable(pass, len(guard.reported) > 0))
	removals.Flush()
}

// runFuncs returns the functions the analysis runs on: the source
// functions of the package, then the closures of package-level initializers
// such as `var f = func(db *gorm.DB) { ... }` (#183), nested ones included.
// Those closures belong to the synthetic package init function, which
// buildssa leaves out of SrcFuncs.
func runFuncs(ssaInfo *buildssa.SSA) []*ssa.Function {
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

// =============================================================================
// SSA Checker
// =============================================================================
