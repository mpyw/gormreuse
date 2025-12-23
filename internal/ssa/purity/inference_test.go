package purity

import (
	"testing"

	"golang.org/x/tools/go/ssa"
)

func TestNewInferencer(t *testing.T) {
	inf := NewInferencer(nil, nil)

	if inf == nil {
		t.Fatal("NewInferencer returned nil")
	}
	if inf.pureFuncs != nil {
		t.Error("Inferencer pureFuncs should be nil")
	}
	if inf.cache == nil {
		t.Error("Inferencer cache not initialized")
	}
	if inf.visiting == nil {
		t.Error("Inferencer visiting not initialized")
	}
}

func TestInferencer_InferValue_Nil(t *testing.T) {
	inf := NewInferencer(nil, nil)

	state := inf.InferValue(nil)
	if state.IsPolluted() || state.IsDepends() {
		t.Error("InferValue(nil) should be Clean")
	}
}

func TestInferencer_InferValue_Const(t *testing.T) {
	inf := NewInferencer(nil, nil)

	// Create a nil constant
	nilConst := &ssa.Const{}
	state := inf.InferValue(nilConst)
	if state.IsPolluted() || state.IsDepends() {
		t.Error("InferValue(Const) should be Clean")
	}
}

func TestInferencer_InferValue_Caching(t *testing.T) {
	inf := NewInferencer(nil, nil)

	nilConst := &ssa.Const{}

	// First call
	state1 := inf.InferValue(nilConst)

	// Second call should return cached value
	state2 := inf.InferValue(nilConst)

	// Both should be Clean (constants are immutable)
	if state1.IsPolluted() || state1.IsDepends() || state2.IsPolluted() || state2.IsDepends() {
		t.Error("Cached value mismatch: both should be Clean")
	}

	// Verify it was cached
	if _, ok := inf.cache[nilConst]; !ok {
		t.Error("Value was not cached")
	}
}

// Note: More comprehensive tests would require building actual SSA programs.
// The integration tests in analyzer_test.go cover real-world scenarios.
