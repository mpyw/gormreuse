// Package internal provides SSA-based analysis for GORM *gorm.DB reuse detection.
package internal

import (
	"go/token"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/buildssa"
	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/directive"
	ssautil "github.com/mpyw/gormreuse/internal/ssa"
	"github.com/mpyw/gormreuse/internal/ssa/purity"
)

// =============================================================================
// Entry Point
// =============================================================================

// RunSSA performs SSA-based analysis for GORM *gorm.DB reuse detection.
func RunSSA(
	pass *analysis.Pass,
	ssaInfo *buildssa.SSA,
	ignoreMaps map[string]directive.IgnoreMap,
	funcIgnores map[string]map[token.Pos]directive.FunctionIgnoreEntry,
	pureFuncs *directive.PureFuncSet,
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

		// Validate pure function contracts using 3-state purity model
		if pureFuncs != nil && pureFuncs.Contains(fn) {
			for _, v := range purity.ValidateFunction(fn, pureFuncs) {
				pass.Reportf(v.Pos, "%s", v.Message)
			}
		}

		chk := newChecker(pass, ignoreMap, pureFuncs)
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

type checker struct {
	pass      *analysis.Pass
	ignoreMap directive.IgnoreMap
	pureFuncs *directive.PureFuncSet
	reported  map[token.Pos]bool
}

func newChecker(pass *analysis.Pass, ignoreMap directive.IgnoreMap, pureFuncs *directive.PureFuncSet) *checker {
	return &checker{
		pass:      pass,
		ignoreMap: ignoreMap,
		pureFuncs: pureFuncs,
		reported:  make(map[token.Pos]bool),
	}
}

func (c *checker) checkFunction(fn *ssa.Function) {
	analyzer := ssautil.NewAnalyzer(fn, c.pureFuncs)
	violations := analyzer.Analyze()

	for _, v := range violations {
		c.report(v.Pos, v.Message)
	}
}

func (c *checker) report(pos token.Pos, message string) {
	if c.reported[pos] {
		return
	}
	c.reported[pos] = true

	line := c.pass.Fset.Position(pos).Line
	if c.ignoreMap != nil && c.ignoreMap.ShouldIgnore(line) {
		return
	}

	c.pass.Reportf(pos, "%s", message)
}
