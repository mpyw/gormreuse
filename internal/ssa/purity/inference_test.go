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

func TestNewInferencer(t *testing.T) {
	checker := newMockChecker()
	inf := NewInferencer(nil, checker)

	if inf == nil {
		t.Fatal("NewInferencer returned nil")
	}
	if inf.checker != checker {
		t.Error("Inferencer checker not set correctly")
	}
	if inf.cache == nil {
		t.Error("Inferencer cache not initialized")
	}
	if inf.visiting == nil {
		t.Error("Inferencer visiting not initialized")
	}
}

func TestInferencer_InferValue_Nil(t *testing.T) {
	checker := newMockChecker()
	inf := NewInferencer(nil, checker)

	state := inf.InferValue(nil)
	if !state.IsClean() {
		t.Errorf("InferValue(nil) = %v, want Clean", state)
	}
}

func TestInferencer_InferValue_Const(t *testing.T) {
	checker := newMockChecker()
	inf := NewInferencer(nil, checker)

	// Create a nil constant
	nilConst := &ssa.Const{}
	state := inf.InferValue(nilConst)
	if !state.IsClean() {
		t.Errorf("InferValue(Const) = %v, want Clean", state)
	}
}

func TestInferencer_InferValue_Caching(t *testing.T) {
	checker := newMockChecker()
	inf := NewInferencer(nil, checker)

	nilConst := &ssa.Const{}

	// First call
	state1 := inf.InferValue(nilConst)

	// Second call should return cached value
	state2 := inf.InferValue(nilConst)

	if !state1.Equal(state2) {
		t.Errorf("Cached value mismatch: %v != %v", state1, state2)
	}

	// Verify it was cached
	if _, ok := inf.cache[nilConst]; !ok {
		t.Error("Value was not cached")
	}
}

// Note: More comprehensive tests would require building actual SSA programs.
// The inferencer is primarily tested through integration tests with the validator.

func TestValidateFunction_Nil(t *testing.T) {
	checker := newMockChecker()

	violations := ValidateFunction(nil, checker)
	if len(violations) != 0 {
		t.Errorf("ValidateFunction(nil) returned %d violations, want 0", len(violations))
	}
}
