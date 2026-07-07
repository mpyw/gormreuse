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
//	│   │    ├── Skip excluded files/functions                            │   │
//	│   │    ├── Validate pure function contracts                         │   │
//	│   │    ├── Run SSA analysis (ssa.Analyzer)                          │   │
//	│   │    └── Apply ignore directives                                  │   │
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
//   - Handle function-level and line-level ignore directives
//   - Report unused directives (ignore, pure, immutable-return)
//   - Validate pure function contracts
package internal

import (
	"fmt"
	"go/token"
	"os"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/buildssa"
	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/directive"
	"github.com/mpyw/gormreuse/internal/fix"
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
//  2. Skip if function has //gormreuse:ignore directive
//  3. Validate pure function contract if marked with //gormreuse:pure
//  4. Run SSA analysis and collect violations
//  5. Report violations (unless suppressed by line-level ignore)
//  6. Report unused ignore directives
func RunSSA(
	pass *analysis.Pass,
	ssaInfo *buildssa.SSA,
	ignoreMaps map[string]directive.IgnoreMap,
	funcIgnores map[string]map[token.Pos]directive.FunctionIgnoreEntry,
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

	// skip reports whether a function is in an excluded file or wholly ignored.
	// When markUsed is true it also records the function-level ignore directive
	// as used; that side effect must happen exactly once per function, so only
	// the analysis pass passes markUsed=true.
	skip := func(fn *ssa.Function, markUsed bool) bool {
		pos := fn.Pos()
		if !pos.IsValid() {
			return true
		}
		filename := pass.Fset.Position(pos).Filename
		if skipFiles[filename] {
			return true
		}
		if funcIgnoreSet, ok := funcIgnores[filename]; ok {
			if entry, ignored := funcIgnoreSet[fn.Pos()]; ignored {
				if markUsed {
					if ignoreMap := ignoreMaps[filename]; ignoreMap != nil {
						ignoreMap.MarkUsed(entry.DirectiveLine)
					}
				}
				return true
			}
		}
		return false
	}

	// PASS 1: validate pure function contracts and collect the functions that
	// definitively leaked their argument. Such a //gormreuse:pure function must
	// not be trusted as pure at its call sites (issue #66), so this must complete
	// for ALL functions before the analysis pass runs — a caller may be visited
	// before its callee.
	failedPure := make(map[*ssa.Function]bool)
	for _, fn := range ssaInfo.SrcFuncs {
		if skip(fn, false) {
			continue
		}
		if pureFuncs != nil && pureFuncs.Contains(fn) {
			recoverPerFunction(fn, func() {
				for _, v := range purity.ValidateFunction(fn, pureFuncs) {
					pass.Reportf(v.Pos, "%s", v.Message)
					// Only a definitive escape revokes pure-trust at call sites;
					// conservative func-arg violations do not (avoids FP cascades).
					if v.Leak {
						failedPure[fn] = true
					}
				}
			})
		}
	}

	// Collect Scopes/Preload callbacks once: their *gorm.DB parameter receives a
	// mid-chain (clone==0) value, so reuse inside them must be detected (#60).
	scopesCallbacks := tracer.CollectScopesCallbacks(ssaInfo.SrcFuncs)

	// Collect immutable callbacks once: gorm's Transaction/Connection/FindInBatches
	// hand their callback a fresh forkable (clone>0) handle, and a user function
	// declared //gormreuse:immutable-input(cb) promises the same for cb. Their
	// callback's tx parameter is therefore exempt from the Phase 1b
	// mutable-by-default treatment (#60 SC103, #61, #62 case 2.2).
	immutableCallbacks := tracer.CollectImmutableCallbacks(ssaInfo.SrcFuncs)
	tracer.CollectImmutableInputCallbacks(ssaInfo.SrcFuncs, immutableInputSet, immutableCallbacks)

	// Enforce the body-side immutable-input contract (#62 cases 2.3/2.4) and
	// report unused immutable-input directives (U1-U3). Uses a tracer with the
	// full context so FindMutableRoot classifies immutable sources correctly.
	inputTracer := tracer.New(pureFuncs, immutableReturnFuncs, immutableParamFuncs, failedPure, scopesCallbacks, immutableCallbacks)
	for _, fn := range ssaInfo.SrcFuncs {
		if skip(fn, false) {
			continue
		}
		recoverPerFunction(fn, func() {
			for _, v := range purity.ValidateImmutableInputs(fn, immutableInputSet, inputTracer) {
				pass.Reportf(v.Pos, "%s", v.Message)
			}
		})
	}
	if immutableInputSet != nil {
		for _, u := range immutableInputSet.GetUnused() {
			pass.Reportf(u.Pos, "%s", u.Reason)
		}
	}

	// TEMPORARY (GORM bug go-gorm/gorm#7592): warn on Session/WithContext/Debug
	// inside Scopes callbacks. Deletable by removing scopes_session_warning.go and
	// this loop once the upstream fix ships in a supported release — see that file.
	for _, fn := range ssaInfo.SrcFuncs {
		if skip(fn, false) {
			continue
		}
		for _, w := range validateScopesCallback(fn) {
			pass.Reportf(w.Pos, "%s", w.Message)
		}
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
	needsImmutableParam := computeNeedsImmutableParam(ssaInfo, immutableParamFuncs, pureFuncs, immutableReturnFuncs, failedPure, scopesCallbacks, immutableCallbacks, skip)

	// PASS 2: run SSA reuse analysis.
	for _, fn := range ssaInfo.SrcFuncs {
		if skip(fn, true) {
			continue
		}

		chk := newChecker(pass, ignoreMaps[pass.Fset.Position(fn.Pos()).Filename], pureFuncs, immutableReturnFuncs, immutableParamFuncs, failedPure, scopesCallbacks, immutableCallbacks, needsImmutableParam, globalReported, globalSuggestedEdits, fixGen)
		recoverPerFunction(fn, func() { chk.checkFunction(fn) })
	}

	// Report immutable-param directives that are signature-valid but have no
	// effect (no *gorm.DB parameter is reused).
	reportRedundantImmutableParam(pass, ssaInfo, immutableParamFuncs, pureFuncs, needsImmutableParam, skip)

	// Report unused ignore directives
	for _, ignoreMap := range ignoreMaps {
		if ignoreMap == nil {
			continue
		}
		for _, pos := range ignoreMap.GetUnusedIgnores() {
			pass.Reportf(pos, "unused gormreuse:ignore directive")
		}
	}

	reportUnusedDirectiveFuncs(pass, pureFuncs, immutableReturnFuncs, immutableParamFuncs)
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
	failedPure, scopesCallbacks, immutableCallbacks map[*ssa.Function]bool,
	skip func(*ssa.Function, bool) bool,
) map[*ssa.Function]bool {
	needs := make(map[*ssa.Function]bool)
	if immutableParamFuncs == nil {
		return needs
	}
	for _, fn := range ssaInfo.SrcFuncs {
		if skip(fn, false) || !immutableParamFuncs.Contains(fn) {
			continue
		}
		if fn.Signature == nil || !directive.HasGormDBParameter(fn.Signature) {
			continue
		}
		if pureFuncs != nil && pureFuncs.Contains(fn) {
			continue
		}
		recoverPerFunction(fn, func() {
			// Counterfactual: analyze fn with its parameters treated as mutable.
			cf := ssautil.NewAnalyzer(fn, pureFuncs, immutableReturnFuncs, nil, failedPure, scopesCallbacks, immutableCallbacks, nil)
			for _, v := range cf.Analyze() {
				if p, ok := v.Root.(*ssa.Parameter); ok && p.Parent() == fn {
					needs[fn] = true
					break
				}
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
	pass *analysis.Pass,
	ssaInfo *buildssa.SSA,
	immutableParamFuncs, pureFuncs *directive.DirectiveFuncSet,
	needsImmutableParam map[*ssa.Function]bool,
	skip func(*ssa.Function, bool) bool,
) {
	if immutableParamFuncs == nil {
		return
	}
	for _, fn := range ssaInfo.SrcFuncs {
		if skip(fn, false) || !immutableParamFuncs.Contains(fn) {
			continue
		}
		if fn.Signature == nil || !directive.HasGormDBParameter(fn.Signature) {
			continue // signature-invalid: handled by reportUnusedDirectiveFuncs
		}
		if pureFuncs != nil && pureFuncs.Contains(fn) {
			continue // pure ⇒ param never branched ⇒ redundant by construction (not flagged)
		}
		if !needsImmutableParam[fn] {
			pass.Reportf(fn.Pos(), "redundant gormreuse:immutable-param directive: no *gorm.DB parameter is reused")
		}
	}
}

// reportUnusedDirectiveFuncs reports pure / immutable-return / immutable-param
// directives that matched no valid function. For combined directives (e.g.
// //gormreuse:pure,immutable-return,immutable-param) a directive at a position
// is "used" if ANY of its combined siblings is used, so each set is suppressed
// when another set reports that position as used.
func reportUnusedDirectiveFuncs(pass *analysis.Pass, pureFuncs, immutableReturnFuncs, immutableParamFuncs *directive.DirectiveFuncSet) {
	usedByOther := func(pos token.Pos, others ...*directive.DirectiveFuncSet) bool {
		for _, s := range others {
			if s != nil && s.IsUsed(pos) {
				return true
			}
		}
		return false
	}
	report := func(set *directive.DirectiveFuncSet, message string, others ...*directive.DirectiveFuncSet) {
		if set == nil {
			return
		}
		for _, pos := range set.GetUnusedDirectives() {
			if usedByOther(pos, others...) {
				continue
			}
			pass.Reportf(pos, "%s", message)
		}
	}

	report(pureFuncs, "unused gormreuse:pure directive", immutableReturnFuncs, immutableParamFuncs)
	report(immutableReturnFuncs, "unused gormreuse:immutable-return directive", pureFuncs, immutableParamFuncs)
	report(immutableParamFuncs, "unused gormreuse:immutable-param directive", pureFuncs, immutableReturnFuncs)
}

// recoverPerFunction runs work, recovering from any panic so that a single
// pathological function (exotic SSA the tracer mishandles) cannot abort the
// entire `go vet` run for the package. Aborting would be worse than any false
// positive and contrary to the conservative-bias design, so the offending
// function is simply skipped.
//
// Set GORMREUSE_DEBUG_PANIC to a non-empty value to re-panic instead, surfacing
// the stack trace for debugging.
func recoverPerFunction(fn *ssa.Function, work func()) {
	defer func() {
		if r := recover(); r != nil && os.Getenv("GORMREUSE_DEBUG_PANIC") != "" {
			panic(fmt.Sprintf("gormreuse: panic analyzing %s: %v", fn, r))
		}
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
//   - Line-level ignore directives suppress violations
//   - Violations are reported through the analysis.Pass
type checker struct {
	pass                 *analysis.Pass              // For reporting diagnostics
	ignoreMap            directive.IgnoreMap         // Line-level ignore directives
	pureFuncs            *directive.DirectiveFuncSet // Pure functions for analysis
	immutableReturnFuncs *directive.DirectiveFuncSet // Immutable-return functions
	immutableParamFuncs  *directive.DirectiveFuncSet // Immutable-param functions (params opt out of Phase 1b)
	failedPure           map[*ssa.Function]bool      // Pure functions that failed contract validation
	scopesCallbacks      map[*ssa.Function]bool      // Scopes/Preload callbacks (params are mutable roots)
	immutableCallbacks   map[*ssa.Function]bool      // Transaction/Connection/FindInBatches callbacks (fresh tx)
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
func newChecker(pass *analysis.Pass, ignoreMap directive.IgnoreMap, pureFuncs, immutableReturnFuncs, immutableParamFuncs *directive.DirectiveFuncSet, failedPure, scopesCallbacks, immutableCallbacks, needsImmutableParam map[*ssa.Function]bool, reported map[token.Pos]bool, suggestedEdits map[editKey]bool, fixGen *fix.Generator) *checker {
	return &checker{
		pass:                 pass,
		ignoreMap:            ignoreMap,
		pureFuncs:            pureFuncs,
		immutableReturnFuncs: immutableReturnFuncs,
		immutableParamFuncs:  immutableParamFuncs,
		failedPure:           failedPure,
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
	analyzer := ssautil.NewAnalyzer(fn, c.pureFuncs, c.immutableReturnFuncs, c.immutableParamFuncs, c.failedPure, c.scopesCallbacks, c.immutableCallbacks, c.needsImmutableParam)
	violations := analyzer.Analyze()

	// Deduplicate violations by root to avoid generating duplicate fixes.
	// Multiple violations from the same root (e.g., tripleUse) should only
	// generate one set of fixes.
	seenRoots := make(map[ssa.Value]bool)

	for _, v := range violations {
		// For fix generation, only process one violation per root
		if v.Root != nil && seenRoots[v.Root] {
			// Still report the violation, but don't generate duplicate fixes
			c.reportViolationWithoutFix(v)
			continue
		}
		if v.Root != nil {
			seenRoots[v.Root] = true
		}

		c.reportViolation(v)
	}
}

// reportViolation reports a violation with SuggestedFix if possible.
func (c *checker) reportViolation(v pollution.Violation) {
	pos := v.Pos

	// Deduplicate: same position may be reached multiple times
	if c.reported[pos] {
		return
	}
	c.reported[pos] = true

	// Check if line is ignored
	line := c.pass.Fset.Position(pos).Line
	if c.ignoreMap != nil && c.ignoreMap.ShouldIgnore(line) {
		return // Suppressed by ignore directive
	}

	// Generate SuggestedFix if possible
	suggestedFixes := c.fixGen.Generate(v)

	// Deduplicate suggested fix edits globally
	// This prevents the same edit from being applied multiple times
	// when different violations suggest the same fix (e.g., for shared Phi edges)
	suggestedFixes = c.deduplicateFixes(suggestedFixes)

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

// reportViolationWithoutFix reports a violation without generating fixes.
// Used for duplicate violations from the same root to avoid generating
// duplicate fixes while still reporting all violation positions.
func (c *checker) reportViolationWithoutFix(v pollution.Violation) {
	pos := v.Pos

	// Deduplicate: same position may be reached multiple times
	if c.reported[pos] {
		return
	}
	c.reported[pos] = true

	// Check if line is ignored
	line := c.pass.Fset.Position(pos).Line
	if c.ignoreMap != nil && c.ignoreMap.ShouldIgnore(line) {
		return // Suppressed by ignore directive
	}

	// Report without suggested fixes
	c.pass.Report(analysis.Diagnostic{
		Pos:     pos,
		Message: v.Message,
	})
}
