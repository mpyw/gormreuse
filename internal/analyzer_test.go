package internal

import (
	"go/token"
	"testing"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/directive"
	ssautil "github.com/mpyw/gormreuse/internal/ssa"
)

// =============================================================================
// Analyzer Tests
// =============================================================================

func TestNewAnalyzer(t *testing.T) {
	t.Parallel()

	pureFuncs := directive.NewPureFuncSet(nil, nil)
	pureFuncs.Add(directive.FuncKey{PkgPath: "test", FuncName: "Pure"})
	immutableReturnFuncs := directive.NewImmutableReturnFuncSet(nil, nil)
	analyzer := ssautil.NewAnalyzer(nil, pureFuncs, immutableReturnFuncs)

	if analyzer == nil {
		t.Error("Expected analyzer to be initialized")
	}
}

func TestNewChecker(t *testing.T) {
	t.Parallel()

	ignoreMap := make(directive.IgnoreMap)
	pureFuncs := directive.NewPureFuncSet(nil, nil)
	immutableReturnFuncs := directive.NewImmutableReturnFuncSet(nil, nil)
	reported := make(map[token.Pos]bool)
	suggestedEdits := make(map[editKey]bool)

	chk := newChecker(nil, ignoreMap, pureFuncs, immutableReturnFuncs, reported, suggestedEdits, nil)

	if chk == nil {
		t.Error("Expected checker to be initialized")
	}
}

func TestAnalyzer_Analyze_NilFunction(t *testing.T) {
	t.Parallel()

	analyzer := ssautil.NewAnalyzer(nil, nil, nil)

	// Should not panic with nil function
	violations := analyzer.Analyze()
	if len(violations) != 0 {
		t.Errorf("Expected 0 violations for nil function, got %d", len(violations))
	}
}

func TestAnalyzer_Analyze_EmptyFunction(t *testing.T) {
	t.Parallel()

	fn := &ssa.Function{}
	analyzer := ssautil.NewAnalyzer(fn, nil, nil)

	violations := analyzer.Analyze()
	if len(violations) != 0 {
		t.Errorf("Expected 0 violations for empty function, got %d", len(violations))
	}
}
