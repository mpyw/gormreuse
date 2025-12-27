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
	pureFuncs := directive.NewPureFuncSet(nil)
	pureFuncs.Add(directive.FuncKey{PkgPath: "test", FuncName: "Pure"})
	immutableReturnFuncs := directive.NewImmutableReturnFuncSet(nil)
	analyzer := ssautil.NewAnalyzer(nil, pureFuncs, immutableReturnFuncs)

	if analyzer == nil {
		t.Error("Expected analyzer to be initialized")
	}
}

func TestNewChecker(t *testing.T) {
	ignoreMap := make(directive.IgnoreMap)
	pureFuncs := directive.NewPureFuncSet(nil)
	immutableReturnFuncs := directive.NewImmutableReturnFuncSet(nil)
	reported := make(map[token.Pos]bool)
	suggestedEdits := make(map[editKey]bool)

	chk := newChecker(nil, ignoreMap, pureFuncs, immutableReturnFuncs, reported, suggestedEdits, nil)

	if chk.pass != nil {
		t.Error("Expected pass to be nil")
	}
	if chk.ignoreMap == nil {
		t.Error("Expected ignoreMap to be set")
	}
	if chk.pureFuncs == nil {
		t.Error("Expected pureFuncs to be set")
	}
	if chk.immutableReturnFuncs == nil {
		t.Error("Expected immutableReturnFuncs to be set")
	}
	if chk.reported == nil {
		t.Error("Expected reported to be initialized")
	}
	if chk.suggestedEdits == nil {
		t.Error("Expected suggestedEdits to be initialized")
	}
	if chk.fixGen != nil {
		t.Error("Expected fixGen to be nil (passed as nil)")
	}
}

func TestAnalyzer_Analyze_NilFunction(t *testing.T) {
	analyzer := ssautil.NewAnalyzer(nil, nil, nil)

	// Should not panic with nil function
	violations := analyzer.Analyze()
	if len(violations) != 0 {
		t.Errorf("Expected 0 violations for nil function, got %d", len(violations))
	}
}

func TestAnalyzer_Analyze_EmptyFunction(t *testing.T) {
	fn := &ssa.Function{}
	analyzer := ssautil.NewAnalyzer(fn, nil, nil)

	violations := analyzer.Analyze()
	if len(violations) != 0 {
		t.Errorf("Expected 0 violations for empty function, got %d", len(violations))
	}
}
