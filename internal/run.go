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
	"maps"
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
	"github.com/mpyw/gormreuse/internal/ssa/cfg"
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
	// The work that reads the whole package runs under it too, and a panic
	// there leaves the conservative result: nothing inferred or exempted, and
	// no contract trusted by another package (#315).
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
		reportUnusedIgnores(removals, ignores, reportIgnoreRemovable(pass, false), reportIgnoresLeftToTests(pass))
		removals.Flush()
		return
	}

	// Analyze the closures of package-level `var f = func(...)` initializers
	// too, and resolve calls through such variables (#183).
	funcs := runFuncs(ssaInfo)
	// After a panic no call resolves through a variable, so such calls have
	// no known callee and are judged conservatively (#348).
	var funcVars callsite.FuncVars
	guard.runPackage("function variables", func() {
		funcVars = callsite.CollectFuncVars(append(slices.Clone(funcs), ssaInfo.Pkg.Func("init")))
	})

	// Callees in other packages are decided by the facts their packages
	// exported (#195). A package without facts leaves its functions unmarked.
	table := facts.NewTable(pass)
	pureFuncs.Import(func(fn *ssa.Function) bool { return table.Of(fn).Pure })
	immutableReturnFuncs.Import(func(fn *ssa.Function) bool { return table.Of(fn).ImmutableReturn })
	immutableReturnFuncs.ImportParamReads(func(fn *ssa.Function) []directive.ParamRead { return table.Of(fn).ParamReads })
	immutableInputSet.Import(func(fn *ssa.Function) []directive.ImmutableInputCallback { return table.Of(fn).ImmutableInput })
	// Only the immutable-param functions that branch a parameter export a
	// fact, and only those matter to a caller (#255).
	immutableParamFuncs.Import(func(fn *ssa.Function) bool { return table.Of(fn).BranchesImmutableParam })

	// Treat a function as immutable-return when its body proves it (#261),
	// before any pass reads the set.
	guard.runPackage("immutable-return inference", func() {
		inferImmutableReturns(funcs, immutableReturnFuncs, funcVars, skip)
	})

	// PASS 1: validate pure function contracts and collect the functions that
	// definitively broke them, and the parameters each trusted one may return.
	// A //gormreuse:pure function that broke the contract must not be trusted
	// as pure at its call sites (issue #66), and the result of one that keeps
	// it may be its argument (#188), so this must complete for ALL functions
	// before the analysis pass runs — a caller may be visited before its callee.
	pureVerdicts := validatePureFuncs(funcs, pureFuncs, immutableReturnFuncs, funcVars, table, skip, report, guard)

	// Collect Scopes/Preload callbacks once: their *gorm.DB parameter receives a
	// mid-chain (clone==0) value, so reuse inside them must be detected (#60).
	// A Connection callback receives one too (#173), so the tracers treat its
	// parameter as they treat a Scopes callback's: as a mutable root that
	// //gormreuse:immutable-param cannot exempt (#348). The fixes keep the
	// Scopes set: a Connection callback returns no *gorm.DB, so it may take
	// the body Session (#313).
	var scopesCallbacks, mutableCallbacks map[*ssa.Function]bool
	guard.runPackage("Scopes callbacks", func() {
		scopesCallbacks = callback.CollectScopesCallbacks(funcs, funcVars)
		mutableCallbacks = maps.Clone(scopesCallbacks)
		maps.Copy(mutableCallbacks, callback.CollectConnectionCallbacks(funcs, funcVars))
	})

	// Collect immutable callbacks once: gorm's Transaction/FindInBatches hand
	// their callback a fresh forkable (clone>0) handle, and a user function
	// declared //gormreuse:immutable-input(cb) promises the same for cb. Their
	// callback's tx parameter is therefore exempt from the Phase 1b
	// mutable-by-default treatment (#60 SC103, #61, #62 case 2.2).
	// The sets are keyed by function, so keep only the functions that nothing
	// else can call: a named callback that is also called directly, or that is
	// exported, keeps a mutable parameter (#191). After a panic no callback is
	// exempt.
	var immutableCallbacks map[*ssa.Function]bool
	if !guard.runPackage("immutable callbacks", func() {
		immutableCallbacks = callback.CollectImmutableCallbacks(funcs, funcVars)
		callback.CollectImmutableInputCallbacks(funcs, funcVars, immutableInputSet, immutableCallbacks)
		callback.DropReachableCallbacks(funcs, funcVars, immutableInputSet, immutableCallbacks)
	}) {
		immutableCallbacks = nil
	}

	// Enforce the body-side immutable-input contract (#62 cases 2.3/2.4) and
	// report unused immutable-input directives (U1-U3). Uses a tracer with the
	// full context so FindMutableRoot classifies immutable sources correctly.
	inputTracer := tracer.NewRootTracer(pureFuncs, immutableReturnFuncs, immutableParamFuncs, pureVerdicts, mutableCallbacks, immutableCallbacks, funcVars)
	failedInput := make(map[*ssa.Function]bool)
	for _, fn := range funcs {
		if skip(fn) {
			continue
		}
		// A check that panicked proves nothing, so the function is not
		// trusted (#315).
		if !guard.run("immutable-input", fn, func() {
			for _, v := range purity.ValidateImmutableInputs(fn, immutableInputSet, inputTracer, nil) {
				failedInput[fn] = true
				report(v.Pos, v.Message)
			}
		}) {
			failedInput[fn] = true
		}
	}
	guard.runPackage("unused immutable-input", func() {
		reportUnusedImmutableInputs(immutableInputSet, reportRemoval)
	})

	// Enforce the body-side immutable-return contract. Reuses inputTracer, which
	// carries the full pass context so other immutable-return / immutable-param
	// functions are classified correctly.
	failedReturn := reportImmutableReturnViolations(funcs, immutableReturnFuncs, inputTracer, skip, report, guard)

	// TEMPORARY (GORM bug go-gorm/gorm#7592): warn on Session/WithContext/Debug
	// results returned from Scopes callbacks. Deletable by removing the scopeswarn
	// package and this block once the upstream fix ships in a supported release — see that package.
	guard.runPackage("Scopes warnings", func() {
		scopesOnly := callback.CollectScopesOnlyCallbacks(funcs, funcVars, func(site callsite.Site) bool {
			return !scopeswarn.ReadsOnly(site, funcVars)
		})
		for _, w := range scopeswarn.Validate(scopesOnly, funcVars, skip) {
			report(w.Pos, w.Message)
		}
	})

	// Share a single fix generator across all violations (it caches AST
	// inspectors). It needs scopesCallbacks to withhold the immutable-param fix on
	// Scopes/Preload callbacks, whose parameters cannot be exempted (stage 2c).
	// inputTracer gives it the analysis' own mutability rule, and the analysed
	// functions let it check every caller, in package-level closures too,
	// before it suggests immutable-param (#201, #245).
	// The checker and the fixes ask one CFG cache about the functions (#299).
	cfgs := cfg.NewAnalyzer()
	// The package initializer is a caller too (#346).
	callers := funcs
	if init := ssaInfo.Pkg.Func("init"); init != nil {
		callers = append(slices.Clone(funcs), init)
	}
	// An immutable-param function that discards a chain on its parameter
	// relies on changing its caller's value, so no fix isolates an argument
	// to it (#346).
	discardsParam := reportImmutableParamDiscards(funcs, immutableParamFuncs, pureFuncs, skip, report, guard)
	fixGen := fix.NewGenerator(pass, scopesCallbacks, inputTracer, callers, reportSeesWholePackage(pass), cfgs, discardsParam)

	// Determine which //gormreuse:immutable-param functions actually rely on
	// immutability — they would reuse a *gorm.DB parameter if it were treated as
	// mutable. This single classification drives two things: the caller-side
	// contract check (stage 2b, passed into the checker below) and, by its
	// complement, redundant-directive detection (a directive whose function does
	// NOT reuse a param suppresses nothing).
	needsImmutableParam := computeNeedsImmutableParam(funcs, immutableParamFuncs, pureFuncs, immutableReturnFuncs, pureVerdicts, mutableCallbacks, immutableCallbacks, funcVars, skip, guard, cfgs)

	// The caller-side check also covers callees in other packages (#195).
	branchesImmutableParam := func(fn *ssa.Function) bool {
		return needsImmutableParam[fn] || table.Of(fn).BranchesImmutableParam
	}

	// PASS 2: run SSA reuse analysis. Every function shares one tracer: the
	// analysis of a closure traces the values of the function that declares it
	// again, so a tracer per function traced a parent's values once per closure
	// (#325). Its memos depend only on the values asked about, since the sets it
	// holds are final by now.
	checkTracer := tracer.NewRootTracer(pureFuncs, immutableReturnFuncs, immutableParamFuncs, pureVerdicts, mutableCallbacks, immutableCallbacks, funcVars)
	chk := newChecker(pass, ignores, checkTracer, branchesImmutableParam, globalReported, fixGen, cfgs)
	// Every function is analyzed before any is reported: an ignore in one
	// function, such as a stored closure, withholds the fix of a root that
	// another function reports (#346).
	analyzed := make([]runFuncViolations, 0, len(funcs))
	for _, fn := range funcs {
		if skip(fn) {
			continue
		}
		var violations []pollution.Violation
		if guard.run("reuse", fn, func() { violations = chk.analyze(fn) }) {
			analyzed = append(analyzed, runFuncViolations{fn, violations})
		}
	}
	for _, a := range analyzed {
		guard.run("reuse", a.fn, func() { chk.report(a.violations) })
	}
	// The package initializer calls functions too, as in
	// `var n = count(DB.Where("x"))`. buildssa leaves it out of SrcFuncs.
	// Its reuse is not checked, but its arguments to an immutable-param
	// function are, as any caller's are (#346). It has no position, so a
	// panic is reported for the package.
	if init := ssaInfo.Pkg.Func("init"); init != nil {
		guard.runPackage("package initializer", func() {
			var calls []pollution.Violation
			for _, v := range chk.analyze(init) {
				if v.Call != nil && !skipFiles[pass.Fset.PositionFor(v.Pos, false).Filename] {
					calls = append(calls, v)
				}
			}
			chk.report(calls)
		})
	}

	// Report immutable-param directives that are signature-valid but have no
	// effect (no *gorm.DB parameter is reused).
	guard.runPackage("directive reports", func() {
		reportRedundantImmutableParam(funcs, immutableParamFuncs, pureFuncs, needsImmutableParam, skip, reportRemoval)
		reportUnusedDirectiveFuncs(pureFuncs, immutableReturnFuncs, immutableParamFuncs, reportRemoval)
	})

	// A panic while computing what another package must not trust leaves
	// every contract distrusted.
	distrusted := distrustedContracts{all: true}
	guard.runPackage("distrusted contracts", func() {
		distrusted = distrustContracts(distrustSources{
			funcs:                funcs,
			immutableReturnFuncs: immutableReturnFuncs,
			immutableInputSet:    immutableInputSet,
			failedReturn:         failedReturn,
			failedInput:          failedInput,
			tracer:               inputTracer,
			funcVars:             funcVars,
			skip:                 skip,
		}, guard)
	})
	exportFacts(pass, ssaInfo, factSources{
		pureFuncs:            pureFuncs,
		immutableReturnFuncs: immutableReturnFuncs,
		immutableInputSet:    immutableInputSet,
		pureVerdicts:         pureVerdicts,
		distrusted:           distrusted,
		needsImmutableParam:  needsImmutableParam,
		tracer:               inputTracer,
	}, skip, guard)

	// Report unused ignore directives last, once every other diagnostic has had
	// the chance to use one. A function whose analysis panicked lost its
	// diagnostics, so an ignore that looks unused may cover one of them: no
	// fix deletes an ignore then.
	removable := reportIgnoreRemovable(pass, guard.panicked())
	guard.runPackage("unused ignores", func() {
		reportUnusedIgnores(removals, ignores, removable, reportIgnoresLeftToTests(pass))
	})
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

// runFuncViolations is a function and the violations its analysis found.
type runFuncViolations struct {
	fn         *ssa.Function
	violations []pollution.Violation
}
