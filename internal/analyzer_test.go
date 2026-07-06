package internal

import (
	"go/token"
	"testing"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/directive"
	ssautil "github.com/mpyw/gormreuse/internal/ssa"
)

// TestRecoverPerFunction verifies that a panic in per-function analysis is
// contained (issue #77 item 3) so one pathological function cannot abort the
// whole vet run, and that GORMREUSE_DEBUG_PANIC re-surfaces it.
func TestRecoverPerFunction(t *testing.T) {
	// Default: panic is swallowed, execution continues.
	ran := false
	recoverPerFunction(nil, func() { panic("boom") })
	recoverPerFunction(nil, func() { ran = true })
	if !ran {
		t.Fatal("recoverPerFunction did not run work after a prior panic")
	}

	// With GORMREUSE_DEBUG_PANIC set, the panic is re-surfaced.
	t.Setenv("GORMREUSE_DEBUG_PANIC", "1")
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Error("expected re-panic under GORMREUSE_DEBUG_PANIC, got none")
			}
		}()
		recoverPerFunction(nil, func() { panic("boom") })
	}()
}

// =============================================================================
// Analyzer Tests
// =============================================================================

func TestNewAnalyzer(t *testing.T) {
	t.Parallel()

	pureFuncs := directive.NewPureFuncSet(nil, nil)
	pureFuncs.Add(directive.FuncKey{PkgPath: "test", FuncName: "Pure"})
	immutableReturnFuncs := directive.NewImmutableReturnFuncSet(nil, nil)
	analyzer := ssautil.NewAnalyzer(nil, pureFuncs, immutableReturnFuncs, nil, nil, nil, nil, nil)

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

	chk := newChecker(nil, ignoreMap, pureFuncs, immutableReturnFuncs, nil, nil, nil, nil, nil, reported, suggestedEdits, nil)

	if chk == nil {
		t.Error("Expected checker to be initialized")
	}
}

func TestAnalyzer_Analyze_NilFunction(t *testing.T) {
	t.Parallel()

	analyzer := ssautil.NewAnalyzer(nil, nil, nil, nil, nil, nil, nil, nil)

	// Should not panic with nil function
	violations := analyzer.Analyze()
	if len(violations) != 0 {
		t.Errorf("Expected 0 violations for nil function, got %d", len(violations))
	}
}

func TestAnalyzer_Analyze_EmptyFunction(t *testing.T) {
	t.Parallel()

	fn := &ssa.Function{}
	analyzer := ssautil.NewAnalyzer(fn, nil, nil, nil, nil, nil, nil, nil)

	violations := analyzer.Analyze()
	if len(violations) != 0 {
		t.Errorf("Expected 0 violations for empty function, got %d", len(violations))
	}
}
