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
package internal

import (
	"cmp"
	"fmt"
	"go/token"
	"os"
	"slices"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/buildssa"
	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/directive"
	"github.com/mpyw/gormreuse/internal/fix"
	"github.com/mpyw/gormreuse/internal/scopeswarn"
	ssautil "github.com/mpyw/gormreuse/internal/ssa"
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
	globalReported := make(map[token.Pos]bool)

	// Share a single suggestedEdits map to avoid duplicate fix edits
	// across different violations that suggest the same edit.
	globalSuggestedEdits := make(map[editKey]bool)

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

	// PASS 1: validate pure function contracts and collect the functions that
	// definitively broke them, and the parameters each trusted one may return.
	// A //gormreuse:pure function that broke the contract must not be trusted
	// as pure at its call sites (issue #66), and the result of one that keeps
	// it may be its argument (#188), so this must complete for ALL functions
	// before the analysis pass runs — a caller may be visited before its callee.
	pureVerdicts := validatePureFuncs(ssaInfo, pureFuncs, immutableReturnFuncs, skip, report, guard)

	// Collect Scopes/Preload callbacks once: their *gorm.DB parameter receives a
	// mid-chain (clone==0) value, so reuse inside them must be detected (#60).
	scopesCallbacks := tracer.CollectScopesCallbacks(ssaInfo.SrcFuncs)

	// Collect immutable callbacks once: gorm's Transaction/FindInBatches hand
	// their callback a fresh forkable (clone>0) handle, and a user function
	// declared //gormreuse:immutable-input(cb) promises the same for cb. Their
	// callback's tx parameter is therefore exempt from the Phase 1b
	// mutable-by-default treatment (#60 SC103, #61, #62 case 2.2).
	immutableCallbacks := tracer.CollectImmutableCallbacks(ssaInfo.SrcFuncs)
	tracer.CollectImmutableInputCallbacks(ssaInfo.SrcFuncs, immutableInputSet, immutableCallbacks)
	// The sets are keyed by function, so keep only the functions that nothing
	// else can call: a named callback that is also called directly, or that is
	// exported, keeps a mutable parameter (#191).
	tracer.DropReachableCallbacks(ssaInfo.SrcFuncs, immutableInputSet, immutableCallbacks)

	// Enforce the body-side immutable-input contract (#62 cases 2.3/2.4) and
	// report unused immutable-input directives (U1-U3). Uses a tracer with the
	// full context so FindMutableRoot classifies immutable sources correctly.
	inputTracer := tracer.New(pureFuncs, immutableReturnFuncs, immutableParamFuncs, pureVerdicts, scopesCallbacks, immutableCallbacks)
	for _, fn := range ssaInfo.SrcFuncs {
		if skip(fn) {
			continue
		}
		guard.run(fn, func() {
			for _, v := range purity.ValidateImmutableInputs(fn, immutableInputSet, inputTracer) {
				report(v.Pos, v.Message)
			}
		})
	}
	if immutableInputSet != nil {
		for _, u := range immutableInputSet.GetUnused() {
			report(u.Pos, u.Reason)
		}
	}

	// Enforce the body-side immutable-return contract. Reuses inputTracer, which
	// carries the full pass context so other immutable-return / immutable-param
	// functions are classified correctly.
	reportImmutableReturnViolations(ssaInfo, immutableReturnFuncs, inputTracer, skip, report, guard)

	// TEMPORARY (GORM bug go-gorm/gorm#7592): warn on Session/WithContext/Debug
	// results returned from Scopes callbacks. Deletable by removing the scopeswarn
	// package and this block once the upstream fix ships in a supported release — see that package.
	scopesOnly := tracer.CollectScopesOnlyCallbacks(ssaInfo.SrcFuncs)
	for _, w := range scopeswarn.Validate(scopesOnly, skip) {
		report(w.Pos, w.Message)
	}

	// Share a single fix generator across all violations (it caches AST
	// inspectors). It needs scopesCallbacks to withhold the immutable-param fix on
	// Scopes/Preload callbacks, whose parameters cannot be exempted (stage 2c).
	fixGen := fix.New(pass, scopesCallbacks)

	// Determine which //gormreuse:immutable-param functions actually rely on
	// immutability — they would reuse a *gorm.DB parameter if it were treated as
	// mutable. This single classification drives two things: the caller-side
	// contract check (stage 2b, passed into the checker below) and, by its
	// complement, redundant-directive detection (a directive whose function does
	// NOT reuse a param suppresses nothing).
	needsImmutableParam := computeNeedsImmutableParam(ssaInfo, immutableParamFuncs, pureFuncs, immutableReturnFuncs, pureVerdicts, scopesCallbacks, immutableCallbacks, skip, guard)

	// PASS 2: run SSA reuse analysis.
	for _, fn := range ssaInfo.SrcFuncs {
		if skip(fn) {
			continue
		}

		chk := newChecker(pass, ignores, pureFuncs, immutableReturnFuncs, immutableParamFuncs, pureVerdicts, scopesCallbacks, immutableCallbacks, needsImmutableParam, globalReported, globalSuggestedEdits, fixGen)
		guard.run(fn, func() { chk.checkFunction(fn) })
	}

	// Report immutable-param directives that are signature-valid but have no
	// effect (no *gorm.DB parameter is reused).
	reportRedundantImmutableParam(ssaInfo, immutableParamFuncs, pureFuncs, needsImmutableParam, skip, report)

	reportUnusedDirectiveFuncs(pureFuncs, immutableReturnFuncs, immutableParamFuncs, report)

	// Report unused ignore directives last, once every other diagnostic has had
	// the chance to use one. No ignore suppresses these.
	for _, pos := range ignores.UnusedIgnores() {
		pass.Reportf(pos, "unused gormreuse:ignore directive")
	}
}

// validatePureFuncs reports every //gormreuse:pure contract violation and
// returns the pure functions that definitively broke the contract, with the
// parameters each of the others may return.
//
// Failure is transitive: a pure function that passes its argument to a failed
// pure function fails too (issue #187). So is returning an argument: a pure
// function that returns what another one returns of its argument returns its
// own argument, and one that branches that result fails (#188). Validation
// therefore repeats until nothing changes, and violations are reported only
// from the final round, so each is reported once and sees the complete
// verdicts.
func validatePureFuncs(
	ssaInfo *buildssa.SSA,
	pureFuncs, immutableReturnFuncs *directive.DirectiveFuncSet,
	skip func(*ssa.Function) bool,
	report func(token.Pos, string),
	guard *panicGuard,
) *tracer.PureVerdicts {
	verdicts := &tracer.PureVerdicts{
		Failed:   make(map[*ssa.Function]bool),
		Returned: make(map[*ssa.Function][]int),
	}
	var targets []*ssa.Function
	for _, fn := range ssaInfo.SrcFuncs {
		if !skip(fn) && pureFuncs.Contains(fn) {
			targets = append(targets, fn)
		}
	}

	// The failed set and each returned set only grow, and larger ones only
	// add violations and returned parameters, so this reaches a fixed point.
	for grew := true; grew; {
		grew = false
		for _, fn := range targets {
			if verdicts.Failed[fn] {
				continue
			}
			guard.run(fn, func() {
				validator := purity.NewValidator(fn, pureFuncs, immutableReturnFuncs, verdicts)
				// Only a definitive violation revokes pure-trust at call sites;
				// conservative func-arg violations do not (avoids FP cascades).
				if slices.ContainsFunc(validator.Validate(), func(v purity.Violation) bool { return v.Definitive }) {
					verdicts.Failed[fn] = true
					delete(verdicts.Returned, fn)
					grew = true
					return
				}
				if returned := validator.ReturnedParams(); !slices.Equal(returned, verdicts.Returned[fn]) {
					verdicts.Returned[fn] = returned
					grew = true
				}
			})
		}
	}

	// A closure body is validated both as its own pure function and as part of
	// a pure function that creates it, so the same violation can come twice.
	reported := make(map[purity.Violation]bool)
	for _, fn := range targets {
		guard.run(fn, func() {
			for _, v := range purity.NewValidator(fn, pureFuncs, immutableReturnFuncs, verdicts).Validate() {
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

// reportImmutableReturnViolations enforces the body-side immutable-return
// contract: a function marked //gormreuse:immutable-return must actually return
// an immutable *gorm.DB. Otherwise the tracer trusts the directive
// (returnsImmutable) and silently allows unsafe reuse of the actually-mutable
// return value at every call site.
func reportImmutableReturnViolations(
	ssaInfo *buildssa.SSA,
	immutableReturnFuncs *directive.DirectiveFuncSet,
	rt *tracer.RootTracer,
	skip func(*ssa.Function) bool,
	report func(token.Pos, string),
	guard *panicGuard,
) {
	validator := purity.NewImmutableReturnValidator(ssaInfo.SrcFuncs, immutableReturnFuncs, rt)
	for _, fn := range ssaInfo.SrcFuncs {
		if skip(fn) {
			continue
		}
		guard.run(fn, func() {
			for _, v := range validator.Validate(fn) {
				report(v.Pos, v.Message)
			}
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
	ssaInfo *buildssa.SSA,
	immutableParamFuncs, pureFuncs, immutableReturnFuncs *directive.DirectiveFuncSet,
	pureVerdicts *tracer.PureVerdicts,
	scopesCallbacks, immutableCallbacks map[*ssa.Function]bool,
	skip func(*ssa.Function) bool,
	guard *panicGuard,
) map[*ssa.Function]bool {
	needs := make(map[*ssa.Function]bool)
	if immutableParamFuncs == nil {
		return needs
	}
	for _, fn := range ssaInfo.SrcFuncs {
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
			cf := ssautil.NewAnalyzer(fn, pureFuncs, immutableReturnFuncs, nil, pureVerdicts, scopesCallbacks, immutableCallbacks, nil)
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
	ssaInfo *buildssa.SSA,
	immutableParamFuncs, pureFuncs *directive.DirectiveFuncSet,
	needsImmutableParam map[*ssa.Function]bool,
	skip func(*ssa.Function) bool,
	report func(token.Pos, string),
) {
	if immutableParamFuncs == nil {
		return
	}
	for _, fn := range ssaInfo.SrcFuncs {
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
	needsImmutableParam  map[*ssa.Function]bool      // immutable-param fns that branch a param (2b caller check)
	reported             map[token.Pos]bool          // Deduplication of reports
	suggestedEdits       map[editKey]bool            // Global deduplication of suggested fixes
	fixGen               *fix.Generator              // Cached fix generator for all violations
}

// editKey uniquely identifies an edit to avoid duplicates across violations.
type editKey struct {
	pos  token.Pos
	end  token.Pos
	text string
}

// newChecker creates a new checker for a specific file.
// The reported map is shared across all functions to deduplicate violations
// across parent functions and their closures.
// The suggestedEdits map is shared to avoid duplicate fix edits.
// The fixGen is shared to avoid recreating the generator for each violation.
func newChecker(pass *analysis.Pass, ignores *directive.Ignores, pureFuncs, immutableReturnFuncs, immutableParamFuncs *directive.DirectiveFuncSet, pureVerdicts *tracer.PureVerdicts, scopesCallbacks, immutableCallbacks, needsImmutableParam map[*ssa.Function]bool, reported map[token.Pos]bool, suggestedEdits map[editKey]bool, fixGen *fix.Generator) *checker {
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
		reported:             reported,
		suggestedEdits:       suggestedEdits,
		fixGen:               fixGen,
	}
}

// checkFunction runs SSA analysis on a single function and reports violations.
func (c *checker) checkFunction(fn *ssa.Function) {
	analyzer := ssautil.NewAnalyzer(fn, c.pureFuncs, c.immutableReturnFuncs, c.immutableParamFuncs, c.pureVerdicts, c.scopesCallbacks, c.immutableCallbacks, c.needsImmutableParam)
	violations := analyzer.Analyze()

	// Deduplicate violations by root to avoid generating duplicate fixes.
	// Multiple violations from the same root (e.g., tripleUse) should only
	// generate one set of fixes.
	seenRoots := make(map[ssa.Value]bool)

	// Group violations by position. Several roots can be reused at one position
	// (use2(q, r)), and only one diagnostic is reported there, so it carries the
	// fix of every root in the group whose fix was not emitted yet. Otherwise
	// only the first root would get a fix (#184).
	for _, group := range groupByPos(violations) {
		var fixFor []pollution.Violation
		for _, v := range group {
			if v.Root == nil || seenRoots[v.Root] {
				continue // No root to fix, or this root's fix was already emitted
			}
			seenRoots[v.Root] = true
			fixFor = append(fixFor, v)
		}
		c.reportViolation(group[0], fixFor)
	}
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

// reportViolation reports v with the suggested fixes for the roots of fixFor.
// The fixes are merged into one, because drivers apply only the first
// suggested fix of a diagnostic.
func (c *checker) reportViolation(v pollution.Violation, fixFor []pollution.Violation) {
	pos := v.Pos

	// Deduplicate: same position may be reached multiple times
	if c.reported[pos] {
		return
	}
	c.reported[pos] = true

	if c.ignores.Ignore(pos) {
		return // Suppressed by ignore directive
	}

	// Generate SuggestedFix if possible
	var suggestedFixes []analysis.SuggestedFix
	for _, fv := range fixFor {
		suggestedFixes = append(suggestedFixes, c.fixGen.Generate(fv)...)
	}

	// Deduplicate suggested fix edits globally
	// This prevents the same edit from being applied multiple times
	// when different violations suggest the same fix (e.g., for shared Phi edges)
	suggestedFixes = mergeFixes(c.deduplicateFixes(suggestedFixes))

	// Report with diagnostic
	c.pass.Report(analysis.Diagnostic{
		Pos:            pos,
		Message:        v.Message,
		SuggestedFixes: suggestedFixes,
	})
}

// deduplicateFixes removes edits that have already been suggested by previous violations.
func (c *checker) deduplicateFixes(fixes []analysis.SuggestedFix) []analysis.SuggestedFix {
	if len(fixes) == 0 {
		return fixes
	}

	var result []analysis.SuggestedFix
	for _, fix := range fixes {
		var dedupedEdits []analysis.TextEdit
		for _, edit := range fix.TextEdits {
			key := editKey{
				pos:  edit.Pos,
				end:  edit.End,
				text: string(edit.NewText),
			}
			if !c.suggestedEdits[key] {
				c.suggestedEdits[key] = true
				dedupedEdits = append(dedupedEdits, edit)
			}
		}
		if len(dedupedEdits) > 0 {
			result = append(result, analysis.SuggestedFix{
				Message:   fix.Message,
				TextEdits: dedupedEdits,
			})
		}
	}
	return result
}

// mergeFixes combines fixes into one fix. Its edits are sorted by position, and
// its message joins the distinct messages in order.
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
		edits = append(edits, f.TextEdits...)
	}
	slices.SortStableFunc(edits, func(a, b analysis.TextEdit) int { return cmp.Compare(a.Pos, b.Pos) })
	return []analysis.SuggestedFix{{Message: strings.Join(messages, "; "), TextEdits: edits}}
}
