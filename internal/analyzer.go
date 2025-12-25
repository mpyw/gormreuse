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
	"fmt"
	"go/token"
	"os"
	"regexp"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/buildssa"
	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/debug"
	"github.com/mpyw/gormreuse/internal/directive"
	ssautil "github.com/mpyw/gormreuse/internal/ssa"
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
	debugFilter string,
) {
	// Compile debug filter regex if provided
	var debugFilterRegex *regexp.Regexp
	if debugFilter != "" {
		var err error
		debugFilterRegex, err = regexp.Compile(debugFilter)
		if err != nil {
			// Report regex error but continue analysis without debug mode
			pass.Reportf(token.NoPos, "invalid debug filter regex: %v", err)
			debugFilterRegex = nil
		}
	}

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

		chk := newChecker(pass, ignoreMap, pureFuncs, immutableReturnFuncs, debugFilterRegex)
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
	debugFilterRegex     *regexp.Regexp                    // Debug filter regex (nil if disabled)
}

// newChecker creates a new checker for a specific file.
func newChecker(pass *analysis.Pass, ignoreMap directive.IgnoreMap, pureFuncs *directive.PureFuncSet, immutableReturnFuncs *directive.ImmutableReturnFuncSet, debugFilterRegex *regexp.Regexp) *checker {
	return &checker{
		pass:                 pass,
		ignoreMap:            ignoreMap,
		pureFuncs:            pureFuncs,
		immutableReturnFuncs: immutableReturnFuncs,
		reported:             make(map[token.Pos]bool),
		debugFilterRegex:     debugFilterRegex,
	}
}

// checkFunction runs SSA analysis on a single function and reports violations.
func (c *checker) checkFunction(fn *ssa.Function) {
	// Check if debug mode is enabled for this function
	debugMode := c.debugFilterRegex != nil && c.debugFilterRegex.MatchString(fn.String())

	analyzer := ssautil.NewAnalyzer(fn, c.pureFuncs, c.immutableReturnFuncs)
	violations := analyzer.Analyze(debugMode)

	// Output debug info if enabled
	if debugMode && len(violations) > 0 {
		fmt.Fprintf(os.Stderr, "\n=== Debug output for %s ===\n", fn.String())
		for _, v := range violations {
			// Try to get debug info if this is a debug violation
			if dv, ok := v.(debug.Violation); ok {
				if debugInfo := dv.DebugInfo(); debugInfo != nil {
					debugOutput := debug.FormatViolation(fn.String(), debugInfo, c.pass.Fset)
					fmt.Fprint(os.Stderr, debugOutput)
				}
			}
		}
	}

	for _, v := range violations {
		c.report(v.Pos(), v.Message())
	}
}

// report reports a violation if not ignored or already reported.
func (c *checker) report(pos token.Pos, message string) {
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

	c.pass.Reportf(pos, "%s", message)
}
