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
	"go/token"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/buildssa"
	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/directive"
	"github.com/mpyw/gormreuse/internal/fix"
	ssautil "github.com/mpyw/gormreuse/internal/ssa"
	"github.com/mpyw/gormreuse/internal/ssa/pollution"
	"github.com/mpyw/gormreuse/internal/ssa/purity"
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
	pureFuncs, immutableReturnFuncs *directive.DirectiveFuncSet,
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

	// Share a single fix generator to avoid recreating it for each violation.
	// The generator caches AST inspectors internally, so sharing it improves performance.
	fixGen := fix.New(pass)

	for _, fn := range ssaInfo.SrcFuncs {
		pos := fn.Pos()
		if !pos.IsValid() {
			continue
		}

		filename := pass.Fset.Position(pos).Filename

		// Skip functions in excluded files
		if skipFiles[filename] {
			continue
		}

		ignoreMap := ignoreMaps[filename]

		// Check if entire function is ignored
		if funcIgnoreSet, ok := funcIgnores[filename]; ok {
			if entry, ignored := funcIgnoreSet[fn.Pos()]; ignored {
				// Mark the ignore directive as used (use the stored line number)
				if ignoreMap != nil {
					ignoreMap.MarkUsed(entry.DirectiveLine)
				}
				continue
			}
		}

		// Validate pure function contracts
		if pureFuncs != nil && pureFuncs.Contains(fn) {
			for _, v := range purity.ValidateFunction(fn, pureFuncs) {
				pass.Reportf(v.Pos, "%s", v.Message)
			}
		}

		// Warn on Session/WithContext/Debug calls inside Scopes callbacks
		// GORM has a bug where Session() in Scopes causes transaction leaks
		for _, warning := range validateScopesCallback(fn) {
			pass.Reportf(warning.Pos, "%s", warning.Message)
		}

		chk := newChecker(pass, ignoreMap, pureFuncs, immutableReturnFuncs, globalReported, globalSuggestedEdits, fixGen)
		chk.checkFunction(fn)
	}

	// Report unused ignore directives
	for _, ignoreMap := range ignoreMaps {
		if ignoreMap == nil {
			continue
		}
		for _, pos := range ignoreMap.GetUnusedIgnores() {
			pass.Reportf(pos, "unused gormreuse:ignore directive")
		}
	}

	// Report unused pure directives
	// For combined directives (e.g., //gormreuse:pure,immutable-return),
	// if either part is used, don't report the other as unused
	if pureFuncs != nil {
		for _, pos := range pureFuncs.GetUnusedDirectives() {
			// Skip if used by immutable-return (combined directive)
			if immutableReturnFuncs != nil && immutableReturnFuncs.IsUsed(pos) {
				continue
			}
			pass.Reportf(pos, "unused gormreuse:pure directive")
		}
	}

	// Report unused immutable-return directives
	if immutableReturnFuncs != nil {
		for _, pos := range immutableReturnFuncs.GetUnusedDirectives() {
			// Skip if used by pure (combined directive)
			if pureFuncs != nil && pureFuncs.IsUsed(pos) {
				continue
			}
			pass.Reportf(pos, "unused gormreuse:immutable-return directive")
		}
	}
}

// =============================================================================
// Scopes Callback Validation
// =============================================================================

// scopesWarning represents a warning about Session() in Scopes callback.
type scopesWarning struct {
	Pos     token.Pos
	Message string
}

// validateScopesCallback checks if a closure is a Scopes callback and warns
// about Session/WithContext/Debug calls which cause transaction leaks in GORM.
//
// This is a temporary warning for GORM bug: https://github.com/go-gorm/gorm/issues/7592
func validateScopesCallback(fn *ssa.Function) []scopesWarning {
	// Only check closures (functions with parents)
	parent := fn.Parent()
	if parent == nil {
		return nil
	}

	// Check if this closure is passed to Scopes
	if !isScopesCallback(fn, parent) {
		return nil
	}

	// Scan for Session/WithContext/Debug calls
	var warnings []scopesWarning
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			call, ok := instr.(*ssa.Call)
			if !ok {
				continue
			}

			methodName := getMethodName(call)
			if methodName == "" {
				continue
			}

			// Check for immutable-returning methods that cause transaction leak in Scopes
			switch methodName {
			case "Session":
				warnings = append(warnings, scopesWarning{
					Pos:     call.Pos(),
					Message: "Session() in Scopes callback causes transaction leak (GORM bug)",
				})
			case "WithContext":
				warnings = append(warnings, scopesWarning{
					Pos:     call.Pos(),
					Message: "WithContext() in Scopes callback causes transaction leak (calls Session internally)",
				})
			case "Debug":
				warnings = append(warnings, scopesWarning{
					Pos:     call.Pos(),
					Message: "Debug() in Scopes callback causes transaction leak (calls Session internally)",
				})
			}
		}
	}

	return warnings
}

// storeRefersToFunction reports whether store's value is fn directly or
// a MakeClosure wrapping fn.
func storeRefersToFunction(store *ssa.Store, fn *ssa.Function) bool {
	if directFn, ok := store.Val.(*ssa.Function); ok && directFn == fn {
		return true
	}
	if mc, ok := store.Val.(*ssa.MakeClosure); ok {
		if closureFn, ok := mc.Fn.(*ssa.Function); ok && closureFn == fn {
			return true
		}
	}
	return false
}

// isScopesCallback checks if fn is a callback passed to the Scopes method.
func isScopesCallback(fn *ssa.Function, parent *ssa.Function) bool {
	// Look for Store instructions that store our function
	for _, block := range parent.Blocks {
		for _, instr := range block.Instrs {
			store, ok := instr.(*ssa.Store)
			if !ok || !storeRefersToFunction(store, fn) {
				continue
			}

			// Found a Store of our function. Trace the destination.
			// store.Addr is typically an IndexAddr for variadic calls
			indexAddr, ok := store.Addr.(*ssa.IndexAddr)
			if !ok {
				continue
			}

			// indexAddr.X is the array being indexed
			alloc, ok := indexAddr.X.(*ssa.Alloc)
			if !ok {
				continue
			}

			// Check if this array is used in a Scopes call
			allocRefs := alloc.Referrers()
			if allocRefs == nil {
				continue
			}
			for _, ref := range *allocRefs {
				// Look for Slice that converts array to slice
				if slice, ok := ref.(*ssa.Slice); ok {
					sliceRefs := slice.Referrers()
					if sliceRefs == nil {
						continue
					}
					for _, sliceRef := range *sliceRefs {
						if call, ok := sliceRef.(*ssa.Call); ok {
							if getMethodName(call) == "Scopes" {
								return true
							}
						}
					}
				}
			}
		}
	}

	// Also check for direct function reference (no free variables, single arg)
	for _, block := range parent.Blocks {
		for _, instr := range block.Instrs {
			call, ok := instr.(*ssa.Call)
			if !ok {
				continue
			}
			if getMethodName(call) != "Scopes" {
				continue
			}
			// Check all arguments
			for _, arg := range call.Call.Args {
				// Direct function reference
				if funcVal, ok := arg.(*ssa.Function); ok && funcVal == fn {
					return true
				}
				// MakeClosure
				if mc, ok := arg.(*ssa.MakeClosure); ok {
					if closureFn, ok := mc.Fn.(*ssa.Function); ok && closureFn == fn {
						return true
					}
				}
			}
		}
	}

	return false
}

// getMethodName extracts the method name from a call instruction.
func getMethodName(call *ssa.Call) string {
	// Interface method call
	if call.Call.IsInvoke() {
		return call.Call.Method.Name()
	}

	// Static callee
	if callee := call.Call.StaticCallee(); callee != nil {
		return callee.Name()
	}

	return ""
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
func newChecker(pass *analysis.Pass, ignoreMap directive.IgnoreMap, pureFuncs, immutableReturnFuncs *directive.DirectiveFuncSet, reported map[token.Pos]bool, suggestedEdits map[editKey]bool, fixGen *fix.Generator) *checker {
	return &checker{
		pass:                 pass,
		ignoreMap:            ignoreMap,
		pureFuncs:            pureFuncs,
		immutableReturnFuncs: immutableReturnFuncs,
		reported:             reported,
		suggestedEdits:       suggestedEdits,
		fixGen:               fixGen,
	}
}

// checkFunction runs SSA analysis on a single function and reports violations.
func (c *checker) checkFunction(fn *ssa.Function) {
	analyzer := ssautil.NewAnalyzer(fn, c.pureFuncs, c.immutableReturnFuncs)
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
