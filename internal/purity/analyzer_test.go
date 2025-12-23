package purity

import (
	"go/types"
	"testing"

	"golang.org/x/tools/go/ssa"
)

// mockChecker implements PurityChecker for testing.
type mockChecker struct {
	gormTypes     map[types.Type]bool
	pureMethods   map[string]bool
	pureUserFuncs map[*ssa.Function]bool
}

func newMockChecker() *mockChecker {
	return &mockChecker{
		gormTypes:     make(map[types.Type]bool),
		pureMethods:   make(map[string]bool),
		pureUserFuncs: make(map[*ssa.Function]bool),
	}
}

func (m *mockChecker) IsGormDB(t types.Type) bool {
	return m.gormTypes[t]
}

func (m *mockChecker) IsPureBuiltinMethod(methodName string) bool {
	return m.pureMethods[methodName]
}

func (m *mockChecker) IsPureUserFunc(fn *ssa.Function) bool {
	return m.pureUserFuncs[fn]
}

func TestNewAnalyzer(t *testing.T) {
	checker := newMockChecker()
	a := NewAnalyzer(nil, checker)

	if a == nil {
		t.Fatal("NewAnalyzer returned nil")
	}
	if a.checker != checker {
		t.Error("Analyzer checker not set correctly")
	}
	if a.cache == nil {
		t.Error("Analyzer cache not initialized")
	}
	if a.visiting == nil {
		t.Error("Analyzer visiting not initialized")
	}
}

func TestAnalyzer_AnalyzeValue_Nil(t *testing.T) {
	checker := newMockChecker()
	a := NewAnalyzer(nil, checker)

	state := a.AnalyzeValue(nil)
	if !state.IsClean() {
		t.Errorf("AnalyzeValue(nil) = %v, want Clean", state)
	}
}

func TestAnalyzer_AnalyzeValue_Const(t *testing.T) {
	checker := newMockChecker()
	a := NewAnalyzer(nil, checker)

	// Create a nil constant
	nilConst := &ssa.Const{}
	state := a.AnalyzeValue(nilConst)
	if !state.IsClean() {
		t.Errorf("AnalyzeValue(Const) = %v, want Clean", state)
	}
}

func TestAnalyzer_AnalyzeValue_Caching(t *testing.T) {
	checker := newMockChecker()
	a := NewAnalyzer(nil, checker)

	nilConst := &ssa.Const{}

	// First call
	state1 := a.AnalyzeValue(nilConst)

	// Second call should return cached value
	state2 := a.AnalyzeValue(nilConst)

	if !state1.Equal(state2) {
		t.Errorf("Cached value mismatch: %v != %v", state1, state2)
	}

	// Verify it was cached
	if _, ok := a.cache[nilConst]; !ok {
		t.Error("Value was not cached")
	}
}

// Note: More comprehensive tests would require building actual SSA programs.
// The analyzer is primarily tested through integration tests with the validator.

func TestValidateFunction_Nil(t *testing.T) {
	checker := newMockChecker()

	violations := ValidateFunction(nil, checker)
	if len(violations) != 0 {
		t.Errorf("ValidateFunction(nil) returned %d violations, want 0", len(violations))
	}
}
