package internal

import (
	"testing"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/directive"
	v2 "github.com/mpyw/gormreuse/internal/ssa/v2"
)

// =============================================================================
// v2.Analyzer Tests
// =============================================================================

func TestNewAnalyzer(t *testing.T) {
	pureFuncs := directive.NewPureFuncSet(nil)
	pureFuncs.Add(directive.PureFuncKey{PkgPath: "test", FuncName: "Pure"})
	analyzer := v2.NewAnalyzer(nil, pureFuncs)

	if analyzer == nil {
		t.Error("Expected analyzer to be initialized")
	}
}

func TestNewChecker(t *testing.T) {
	ignoreMap := make(directive.IgnoreMap)
	pureFuncs := directive.NewPureFuncSet(nil)

	chk := newChecker(nil, ignoreMap, pureFuncs)

	if chk.pass != nil {
		t.Error("Expected pass to be nil")
	}
	if chk.ignoreMap == nil {
		t.Error("Expected ignoreMap to be set")
	}
	if chk.pureFuncs == nil {
		t.Error("Expected pureFuncs to be set")
	}
	if chk.reported == nil {
		t.Error("Expected reported to be initialized")
	}
}

func TestAnalyzer_Analyze_NilFunction(t *testing.T) {
	analyzer := v2.NewAnalyzer(nil, nil)

	// Should not panic with nil function
	violations := analyzer.Analyze()
	if len(violations) != 0 {
		t.Errorf("Expected 0 violations for nil function, got %d", len(violations))
	}
}

func TestAnalyzer_Analyze_EmptyFunction(t *testing.T) {
	fn := &ssa.Function{}
	analyzer := v2.NewAnalyzer(fn, nil)

	violations := analyzer.Analyze()
	if len(violations) != 0 {
		t.Errorf("Expected 0 violations for empty function, got %d", len(violations))
	}
}
