package internal

import (
	"testing"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/directive"
	ssapkg "github.com/mpyw/gormreuse/internal/ssa"
)

// =============================================================================
// ssaAnalyzer Tests (analyzer.go)
// =============================================================================

func TestNewAnalyzer(t *testing.T) {
	pureFuncs := directive.NewPureFuncSet(nil)
	pureFuncs.Add(directive.PureFuncKey{PkgPath: "test", FuncName: "Pure"})
	analyzer := newSSAAnalyzer(nil, pureFuncs)

	if analyzer.fn != nil {
		t.Error("Expected fn to be nil")
	}
	if analyzer.rootTracer == nil {
		t.Error("Expected rootTracer to be initialized")
	}
	if analyzer.cfgAnalyzer == nil {
		t.Error("Expected cfgAnalyzer to be initialized")
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

func TestAnalyzer_ProcessFunction_NilFunction(t *testing.T) {
	analyzer := newSSAAnalyzer(nil, nil)
	cfgAnalyzer := ssapkg.NewCFGAnalyzer()
	tracker := ssapkg.NewPollutionTracker(cfgAnalyzer, nil)

	// Should not panic with nil function
	analyzer.processFunction(nil, tracker, make(map[*ssa.Function]bool))
}

func TestAnalyzer_ProcessFunction_EmptyFunction(t *testing.T) {
	analyzer := newSSAAnalyzer(nil, nil)
	cfgAnalyzer := ssapkg.NewCFGAnalyzer()
	tracker := ssapkg.NewPollutionTracker(cfgAnalyzer, nil)

	// Function with nil Blocks
	fn := &ssa.Function{}
	analyzer.processFunction(fn, tracker, make(map[*ssa.Function]bool))
}

func TestAnalyzer_ProcessFunction_AlreadyVisited(t *testing.T) {
	analyzer := newSSAAnalyzer(nil, nil)
	cfgAnalyzer := ssapkg.NewCFGAnalyzer()
	tracker := ssapkg.NewPollutionTracker(cfgAnalyzer, nil)

	fn := &ssa.Function{}
	visited := map[*ssa.Function]bool{fn: true}

	// Should return early without processing
	analyzer.processFunction(fn, tracker, visited)
}

func TestAnalyzer_Analyze_EmptyFunction(t *testing.T) {
	fn := &ssa.Function{}
	analyzer := newSSAAnalyzer(fn, nil)

	violations := analyzer.Analyze()
	if len(violations) != 0 {
		t.Errorf("Expected 0 violations for empty function, got %d", len(violations))
	}
}
