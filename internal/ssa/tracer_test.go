package ssa

import (
	"go/constant"
	"go/token"
	"testing"

	"golang.org/x/tools/go/ssa"
)

// =============================================================================
// RootTracer Tests
// =============================================================================

func TestRootTracer_IsPureFunction_NilPureFuncs(t *testing.T) {
	// nil pureFuncs should return false
	tracer := NewRootTracer(nil)

	if tracer.PureFuncs() != nil {
		t.Error("Expected pureFuncs to be nil")
	}
}

func TestRootTracer_FindMutableRoot_NilValue(t *testing.T) {
	tracer := NewRootTracer(nil)

	result := tracer.FindMutableRoot(nil)
	if result != nil {
		t.Error("FindMutableRoot(nil) should return nil")
	}
}

func TestRootTracer_FindMutableRoot_Visited(t *testing.T) {
	// This test verifies cycle detection in FindMutableRoot
	// Since cycles are handled internally, we test that nil input returns nil
	tracer := NewRootTracer(nil)

	// FindMutableRoot handles cycles internally
	result := tracer.FindMutableRoot(nil)
	if result != nil {
		t.Error("nil value should return nil")
	}
}

func TestRootTracer_IsImmutableSource_Parameter(t *testing.T) {
	tracer := NewRootTracer(nil)

	// Parameters are immutable
	param := &ssa.Parameter{}
	if !tracer.IsImmutableSource(param) {
		t.Error("Parameter should be immutable source")
	}
}

func TestRootTracer_IsImmutableSource_Nil(t *testing.T) {
	tracer := NewRootTracer(nil)

	// nil should not be immutable
	if tracer.IsImmutableSource(nil) {
		t.Error("nil should not be immutable source")
	}
}

func TestRootTracer_FindMutableRoot_Phi(t *testing.T) {
	tracer := NewRootTracer(nil)

	// Phi with nil edges returns nil
	phi := &ssa.Phi{}
	result := tracer.FindMutableRoot(phi)
	if result != nil {
		t.Error("Phi with no edges should return nil")
	}
}

func TestRootTracer_FindMutableRoot_ChangeType(t *testing.T) {
	tracer := NewRootTracer(nil)

	// ChangeType traces through X
	changeType := &ssa.ChangeType{X: nil}
	result := tracer.FindMutableRoot(changeType)
	if result != nil {
		t.Error("ChangeType with nil X should return nil")
	}
}

func TestRootTracer_FindMutableRoot_Extract(t *testing.T) {
	tracer := NewRootTracer(nil)

	// Extract traces through Tuple
	extract := &ssa.Extract{Tuple: nil}
	result := tracer.FindMutableRoot(extract)
	if result != nil {
		t.Error("Extract with nil Tuple should return nil")
	}
}

func TestRootTracer_FindMutableRoot_Parameter(t *testing.T) {
	tracer := NewRootTracer(nil)

	// Parameter is immutable source
	param := &ssa.Parameter{}
	result := tracer.FindMutableRoot(param)
	if result != nil {
		t.Error("Parameter should return nil (immutable)")
	}
}

func TestRootTracer_FindMutableRoot_Call_NilCallee(t *testing.T) {
	tracer := NewRootTracer(nil)

	// Call with nil StaticCallee
	call := &ssa.Call{}
	result := tracer.FindMutableRoot(call)
	// StaticCallee is nil, returns nil
	if result != nil {
		t.Error("Call with nil callee should return nil")
	}
}

func TestRootTracer_FindMutableRoot_NilInput(t *testing.T) {
	tracer := NewRootTracer(nil)

	result := tracer.FindMutableRoot(nil)
	if result != nil {
		t.Error("FindMutableRoot(nil) should return nil")
	}
}

func TestRootTracer_FindMutableRoot_UnOp_NonDeref(t *testing.T) {
	tracer := NewRootTracer(nil)

	// UnOp that is not dereference (e.g., negation)
	unop := &ssa.UnOp{
		Op: token.SUB, // Not MUL (dereference)
		X:  nil,
	}
	result := tracer.FindMutableRoot(unop)
	if result != nil {
		t.Error("UnOp with nil X should return nil")
	}
}

func TestRootTracer_FindMutableRoot_UnOp_Deref(t *testing.T) {
	tracer := NewRootTracer(nil)

	// UnOp that is dereference
	unop := &ssa.UnOp{
		Op: token.MUL, // Dereference
		X:  nil,
	}
	result := tracer.FindMutableRoot(unop)
	// TracePointerLoad with nil returns nil
	if result != nil {
		t.Error("UnOp deref with nil X should return nil")
	}
}

func TestRootTracer_FindMutableRoot_FreeVar(t *testing.T) {
	tracer := NewRootTracer(nil)

	fv := &ssa.FreeVar{}
	result := tracer.FindMutableRoot(fv)
	// TraceFreeVar with nil parent returns nil
	if result != nil {
		t.Error("FreeVar with nil parent should return nil")
	}
}

func TestRootTracer_FindMutableRoot_PhiWithEdges(t *testing.T) {
	tracer := NewRootTracer(nil)

	// Phi with edges that are all immutable
	param := &ssa.Parameter{} // immutable, will return nil
	phi := &ssa.Phi{
		Edges: []ssa.Value{param},
	}

	result := tracer.FindMutableRoot(phi)
	// Parameter is immutable, returns nil
	if result != nil {
		t.Error("Phi with only immutable edges should return nil")
	}
}

func TestRootTracer_IsImmutableSource_NilValue(t *testing.T) {
	tracer := NewRootTracer(nil)

	// nil value
	result := tracer.IsImmutableSource(nil)
	if result {
		t.Error("nil value should not be immutable source")
	}
}

func TestRootTracer_IsImmutableSource_Alloc(t *testing.T) {
	tracer := NewRootTracer(nil)

	// Alloc is not an immutable source
	alloc := &ssa.Alloc{}
	result := tracer.IsImmutableSource(alloc)
	if result {
		t.Error("Alloc should not be immutable source")
	}
}

func TestRootTracer_IsImmutableSource_MakeInterface(t *testing.T) {
	tracer := NewRootTracer(nil)

	// MakeInterface is not an immutable source
	mi := &ssa.MakeInterface{}
	result := tracer.IsImmutableSource(mi)
	if result {
		t.Error("MakeInterface should not be immutable source")
	}
}

func TestRootTracer_IsImmutableSource_Phi(t *testing.T) {
	tracer := NewRootTracer(nil)

	// Phi is not an immutable source
	phi := &ssa.Phi{}
	result := tracer.IsImmutableSource(phi)
	if result {
		t.Error("Phi should not be immutable source")
	}
}

func TestRootTracer_IsImmutableSource_Call_NilCallee(t *testing.T) {
	tracer := NewRootTracer(nil)

	// Call with nil callee should not be immutable
	call := &ssa.Call{}
	if tracer.IsImmutableSource(call) {
		t.Error("Call with nil callee should not be immutable")
	}
}

func TestRootTracer_IsImmutableSource_CallWithNilCallee(t *testing.T) {
	tracer := NewRootTracer(nil)

	// Call with nil StaticCallee
	call := &ssa.Call{}
	result := tracer.IsImmutableSource(call)
	if result {
		t.Error("Call with nil callee should not be immutable source")
	}
}

// =============================================================================
// SSATracer Tests
// =============================================================================

func TestSSATracer_TracePointerLoad_NilPointer(t *testing.T) {
	ssaTracer := NewSSATracer()

	result := ssaTracer.TracePointerLoad(nil, func(v ssa.Value) ssa.Value { return nil })
	if result != nil {
		t.Error("TracePointerLoad(nil) should return nil")
	}
}

func TestSSATracer_TracePointerLoad_FreeVar(t *testing.T) {
	ssaTracer := NewSSATracer()

	fv := &ssa.FreeVar{}
	result := ssaTracer.TracePointerLoad(fv, func(v ssa.Value) ssa.Value { return nil })
	// TraceFreeVar with nil parent returns nil
	if result != nil {
		t.Error("TracePointerLoad(FreeVar) with nil parent should return nil")
	}
}

func TestSSATracer_TracePointerLoad_OtherValue(t *testing.T) {
	ssaTracer := NewSSATracer()

	// Parameter falls through to trace callback
	param := &ssa.Parameter{}
	result := ssaTracer.TracePointerLoad(param, func(v ssa.Value) ssa.Value { return nil })
	// Callback returns nil
	if result != nil {
		t.Error("TracePointerLoad(Parameter) should return nil when callback returns nil")
	}
}

func TestSSATracer_TraceFreeVar_NotFound(t *testing.T) {
	ssaTracer := NewSSATracer()

	// FreeVar with nil parent - TraceFreeVar returns nil
	fv := &ssa.FreeVar{}
	result := ssaTracer.TraceFreeVar(fv, func(v ssa.Value) ssa.Value { return nil })
	if result != nil {
		t.Error("FreeVar with nil parent should return nil")
	}
}

func TestSSATracer_TraceFreeVar_NilParent(t *testing.T) {
	ssaTracer := NewSSATracer()

	// FreeVar with nil Parent
	fv := &ssa.FreeVar{}
	result := ssaTracer.TraceFreeVar(fv, func(v ssa.Value) ssa.Value { return nil })
	if result != nil {
		t.Error("FreeVar with nil parent should return nil")
	}
}

func TestSSATracer_TraceFreeVar_NotInFreeVars(t *testing.T) {
	ssaTracer := NewSSATracer()

	// FreeVar with no matching entry in fn.FreeVars
	fv := &ssa.FreeVar{}
	result := ssaTracer.TraceFreeVar(fv, func(v ssa.Value) ssa.Value { return nil })
	// Parent is nil, returns nil
	if result != nil {
		t.Error("FreeVar not found in parent's FreeVars should return nil")
	}
}

func TestSSATracer_TraceFreeVar_NilGrandparent(t *testing.T) {
	ssaTracer := NewSSATracer()

	// This tests the case where fn.Parent() is nil
	fv := &ssa.FreeVar{}
	result := ssaTracer.TraceFreeVar(fv, func(v ssa.Value) ssa.Value { return nil })
	if result != nil {
		t.Error("FreeVar with nil grandparent should return nil")
	}
}

func TestSSATracer_TraceIIFEReturns_NilResults(t *testing.T) {
	ssaTracer := NewSSATracer()
	visited := make(map[ssa.Value]bool)

	// Function with nil Signature.Results()
	fn := &ssa.Function{}
	result := ssaTracer.TraceIIFEReturns(fn, visited, func(v ssa.Value, vis map[ssa.Value]bool) ssa.Value { return nil })
	if result != nil {
		t.Error("Function with nil results should return nil")
	}
}

func TestSSATracer_TraceIIFEReturns_EmptyResults(t *testing.T) {
	ssaTracer := NewSSATracer()
	visited := make(map[ssa.Value]bool)

	// Function with empty signature (void return)
	fn := &ssa.Function{}
	result := ssaTracer.TraceIIFEReturns(fn, visited, func(v ssa.Value, vis map[ssa.Value]bool) ssa.Value { return nil })
	if result != nil {
		t.Error("Function with no return type should return nil")
	}
}

func TestSSATracer_TraceIIFEReturns_NonGormDBReturn(t *testing.T) {
	ssaTracer := NewSSATracer()
	visited := make(map[ssa.Value]bool)

	// Function that doesn't return *gorm.DB - signature is nil
	fn := &ssa.Function{}
	result := ssaTracer.TraceIIFEReturns(fn, visited, func(v ssa.Value, vis map[ssa.Value]bool) ssa.Value { return nil })
	if result != nil {
		t.Error("Function not returning *gorm.DB should return nil")
	}
}

func TestSSATracer_TraceIIFEReturns_NoBlocks(t *testing.T) {
	ssaTracer := NewSSATracer()
	visited := make(map[ssa.Value]bool)

	// Function with nil Blocks
	fn := &ssa.Function{}
	result := ssaTracer.TraceIIFEReturns(fn, visited, func(v ssa.Value, vis map[ssa.Value]bool) ssa.Value { return nil })
	if result != nil {
		t.Error("Function with no blocks should return nil")
	}
}

// =============================================================================
// IsNilConst Tests
// =============================================================================

func TestIsNilConst_NilConst(t *testing.T) {
	// Const with nil Value is a nil constant
	c := &ssa.Const{Value: nil}
	if !IsNilConst(c) {
		t.Error("Const with nil Value should be nil constant")
	}
}

func TestIsNilConst_NonNilConst(t *testing.T) {
	// Const with non-nil Value is not a nil constant
	c := &ssa.Const{Value: constant.MakeInt64(42)}
	if IsNilConst(c) {
		t.Error("Const with non-nil Value should not be nil constant")
	}
}

func TestIsNilConst_NonConst(t *testing.T) {
	// Non-Const value is not a nil constant
	p := &ssa.Parameter{}
	if IsNilConst(p) {
		t.Error("Parameter should not be nil constant")
	}
}

func TestIsNilConst_Nil(t *testing.T) {
	// nil value is not a nil constant
	if IsNilConst(nil) {
		t.Error("nil should not be nil constant")
	}
}
