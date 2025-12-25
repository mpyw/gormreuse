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
//   - Report unused ignore directives
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
	pureFuncs *directive.PureFuncSet,
	immutableReturnFuncs *directive.ImmutableReturnFuncSet,
	skipFiles map[string]bool,
) {
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

		chk := newChecker(pass, ignoreMap, pureFuncs, immutableReturnFuncs)
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
	pass                 *analysis.Pass                    // For reporting diagnostics
	ignoreMap            directive.IgnoreMap               // Line-level ignore directives
	pureFuncs            *directive.PureFuncSet            // Pure functions for analysis
	immutableReturnFuncs *directive.ImmutableReturnFuncSet // Immutable-return functions
	reported             map[token.Pos]bool                // Deduplication of reports
}

// newChecker creates a new checker for a specific file.
func newChecker(pass *analysis.Pass, ignoreMap directive.IgnoreMap, pureFuncs *directive.PureFuncSet, immutableReturnFuncs *directive.ImmutableReturnFuncSet) *checker {
	return &checker{
		pass:                 pass,
		ignoreMap:            ignoreMap,
		pureFuncs:            pureFuncs,
		immutableReturnFuncs: immutableReturnFuncs,
		reported:             make(map[token.Pos]bool),
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
	fixGen := fix.New(c.pass)
	suggestedFixes := fixGen.Generate(v)

	// Report with diagnostic
	c.pass.Report(analysis.Diagnostic{
		Pos:            pos,
		Message:        v.Message,
		SuggestedFixes: suggestedFixes,
	})
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
