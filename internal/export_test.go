package internal

import (
	"go/token"

	"golang.org/x/tools/go/analysis"

	"github.com/mpyw/gormreuse/internal/directive"
	"github.com/mpyw/gormreuse/internal/fix"
)

// Export unexported types and functions for testing.

// EditKey exports editKey for external tests.
type EditKey = editKey

// NewChecker exports newChecker for external tests.
func NewChecker(
	pass *analysis.Pass,
	ignoreMap directive.IgnoreMap,
	pureFuncs *directive.DirectiveFuncSet,
	immutableReturnFuncs *directive.DirectiveFuncSet,
	reported map[token.Pos]bool,
	suggestedEdits map[editKey]bool,
	fixGen *fix.Generator,
) *checker {
	return newChecker(pass, ignoreMap, pureFuncs, immutableReturnFuncs, reported, suggestedEdits, fixGen)
}
