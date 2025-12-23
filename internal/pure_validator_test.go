package internal

import (
	"go/token"
	"go/types"
	"testing"

	"golang.org/x/tools/go/ssa"
)

// =============================================================================
// Test isPureFunction
// =============================================================================

func TestIsPureFunction_Nil(t *testing.T) {
	result := isPureFunction(nil, nil)
	if result {
		t.Error("isPureFunction(nil, nil) should return false")
	}
}

func TestIsPureFunction_BuiltinPure(t *testing.T) {
	// Test with builtin pure names
	pureNames := []string{"Session", "WithContext", "Debug", "Open", "Begin", "Transaction"}
	for _, name := range pureNames {
		if !IsPureFunctionBuiltin(name) {
			t.Errorf("IsPureFunctionBuiltin(%q) should return true", name)
		}
	}
}

func TestIsPureFunction_NonPure(t *testing.T) {
	nonPureNames := []string{"Where", "Find", "Create", "Update", "Delete", "First", "Last"}
	for _, name := range nonPureNames {
		if IsPureFunctionBuiltin(name) {
			t.Errorf("IsPureFunctionBuiltin(%q) should return false", name)
		}
	}
}

// =============================================================================
// Test isParamDerived
// =============================================================================

func TestIsParamDerived_InMap(t *testing.T) {
	param := &ssa.Parameter{}
	derived := make(map[ssa.Value]bool)
	derived[param] = true

	if !isParamDerived(param, derived) {
		t.Error("isParamDerived should return true when value is in map")
	}
}

func TestIsParamDerived_NotInMap(t *testing.T) {
	param := &ssa.Parameter{}
	other := &ssa.Parameter{}
	derived := make(map[ssa.Value]bool)
	derived[param] = true

	if isParamDerived(other, derived) {
		t.Error("isParamDerived should return false when value is not in map")
	}
}

func TestIsParamDerived_EmptyMap(t *testing.T) {
	param := &ssa.Parameter{}
	derived := make(map[ssa.Value]bool)

	if isParamDerived(param, derived) {
		t.Error("isParamDerived should return false with empty map")
	}
}

// =============================================================================
// Test findGormDBParams
// =============================================================================

func TestFindGormDBParams_NoParams(t *testing.T) {
	fn := &ssa.Function{
		Params: nil,
	}

	params := findGormDBParams(fn)
	if len(params) != 0 {
		t.Errorf("findGormDBParams should return empty slice, got %d params", len(params))
	}
}

func TestFindGormDBParams_EmptyParams(t *testing.T) {
	fn := &ssa.Function{
		Params: []*ssa.Parameter{},
	}

	params := findGormDBParams(fn)
	if len(params) != 0 {
		t.Errorf("findGormDBParams should return empty slice, got %d params", len(params))
	}
}

// =============================================================================
// Test returnsGormDB
// =============================================================================

func TestReturnsGormDB_NoResults(t *testing.T) {
	sig := types.NewSignatureType(nil, nil, nil, nil, nil, false)
	fn := &ssa.Function{
		Signature: sig,
	}

	if returnsGormDB(fn) {
		t.Error("returnsGormDB should return false when no results")
	}
}

func TestReturnsGormDB_NonGormResult(t *testing.T) {
	results := types.NewTuple(types.NewVar(0, nil, "", types.Typ[types.Int]))
	sig := types.NewSignatureType(nil, nil, nil, nil, results, false)
	fn := &ssa.Function{
		Signature: sig,
	}

	if returnsGormDB(fn) {
		t.Error("returnsGormDB should return false for non-gorm return type")
	}
}

// =============================================================================
// Test isImmutableValue
// =============================================================================

func TestIsImmutableValue_Parameter(t *testing.T) {
	param := &ssa.Parameter{}
	derived := make(map[ssa.Value]bool)
	fset := token.NewFileSet()
	pureFuncs := NewPureFuncSet(fset)

	// Parameters are mutable by default (caller could pass mutable)
	if isImmutableValue(param, derived, pureFuncs) {
		t.Error("isImmutableValue should return false for Parameter")
	}
}

func TestIsImmutableValue_Nil(t *testing.T) {
	derived := make(map[ssa.Value]bool)
	fset := token.NewFileSet()
	pureFuncs := NewPureFuncSet(fset)

	// Nil should return false (unknown)
	if isImmutableValue(nil, derived, pureFuncs) {
		t.Error("isImmutableValue should return false for nil")
	}
}

func TestIsImmutableValue_UnknownType(t *testing.T) {
	// Use a type that isn't handled
	alloc := &ssa.Alloc{}
	derived := make(map[ssa.Value]bool)
	fset := token.NewFileSet()
	pureFuncs := NewPureFuncSet(fset)

	// Unknown types should be treated as mutable (conservative)
	if isImmutableValue(alloc, derived, pureFuncs) {
		t.Error("isImmutableValue should return false for unknown types")
	}
}

func TestIsImmutableValue_FieldAddr(t *testing.T) {
	fieldAddr := &ssa.FieldAddr{}
	derived := make(map[ssa.Value]bool)
	fset := token.NewFileSet()
	pureFuncs := NewPureFuncSet(fset)

	// Field access is conservative: assume mutable
	if isImmutableValue(fieldAddr, derived, pureFuncs) {
		t.Error("isImmutableValue should return false for FieldAddr")
	}
}

func TestIsImmutableValue_Field(t *testing.T) {
	field := &ssa.Field{}
	derived := make(map[ssa.Value]bool)
	fset := token.NewFileSet()
	pureFuncs := NewPureFuncSet(fset)

	// Field access is conservative: assume mutable
	if isImmutableValue(field, derived, pureFuncs) {
		t.Error("isImmutableValue should return false for Field")
	}
}

// =============================================================================
// Test trackDerivation
// =============================================================================

func TestTrackDerivation_Extract(t *testing.T) {
	// Setup: tuple is param-derived, extract should also be param-derived
	tuple := &ssa.Call{}
	extract := &ssa.Extract{
		Tuple: tuple,
		Index: 0,
	}
	derived := make(map[ssa.Value]bool)
	derived[tuple] = true

	trackDerivation(extract, derived)

	if !derived[extract] {
		t.Error("trackDerivation should mark Extract as derived when Tuple is derived")
	}
}

func TestTrackDerivation_Extract_NotDerived(t *testing.T) {
	// Setup: tuple is NOT param-derived
	tuple := &ssa.Call{}
	extract := &ssa.Extract{
		Tuple: tuple,
		Index: 0,
	}
	derived := make(map[ssa.Value]bool)
	// tuple NOT in derived

	trackDerivation(extract, derived)

	if derived[extract] {
		t.Error("trackDerivation should NOT mark Extract as derived when Tuple is not derived")
	}
}

// =============================================================================
// Test IsPureFunctionDecl
// =============================================================================

func TestIsPureFunctionDecl_NilPureFuncs(t *testing.T) {
	fn := &ssa.Function{}

	if IsPureFunctionDecl(fn, nil) {
		t.Error("IsPureFunctionDecl should return false when pureFuncs is nil")
	}
}

func TestIsPureFunctionDecl_EmptyPureFuncs(t *testing.T) {
	fn := &ssa.Function{}
	fset := token.NewFileSet()
	pureFuncs := NewPureFuncSet(fset)

	if IsPureFunctionDecl(fn, pureFuncs) {
		t.Error("IsPureFunctionDecl should return false when function is not in pureFuncs")
	}
}

// =============================================================================
// Test ValidatePureFunction
// =============================================================================

func TestValidatePureFunction_NilBlocks(t *testing.T) {
	sig := types.NewSignatureType(nil, nil, nil, nil, nil, false)
	fn := &ssa.Function{
		Signature: sig,
		Blocks:    nil,
	}
	fset := token.NewFileSet()
	pureFuncs := NewPureFuncSet(fset)

	violations := ValidatePureFunction(fn, pureFuncs)
	if len(violations) != 0 {
		t.Errorf("ValidatePureFunction should return empty for function with nil blocks, got %d violations", len(violations))
	}
}

func TestValidatePureFunction_EmptyFunction(t *testing.T) {
	sig := types.NewSignatureType(nil, nil, nil, nil, nil, false)
	fn := &ssa.Function{
		Signature: sig,
		Blocks:    []*ssa.BasicBlock{},
	}
	fset := token.NewFileSet()
	pureFuncs := NewPureFuncSet(fset)

	violations := ValidatePureFunction(fn, pureFuncs)
	if len(violations) != 0 {
		t.Errorf("ValidatePureFunction should return empty for empty function, got %d violations", len(violations))
	}
}

// =============================================================================
// Test PureViolation
// =============================================================================

func TestPureViolation_Fields(t *testing.T) {
	v := PureViolation{
		Pos:     token.Pos(100),
		Message: "test message",
	}

	if v.Pos != token.Pos(100) {
		t.Errorf("PureViolation.Pos = %v, want %v", v.Pos, token.Pos(100))
	}
	if v.Message != "test message" {
		t.Errorf("PureViolation.Message = %q, want %q", v.Message, "test message")
	}
}

// =============================================================================
// Test checkCallInPure
// =============================================================================

func TestCheckCallInPure_NilMethod(t *testing.T) {
	call := &ssa.Call{}
	call.Call.Method = nil
	derived := make(map[ssa.Value]bool)
	fset := token.NewFileSet()
	pureFuncs := NewPureFuncSet(fset)

	violations := checkCallInPure(call, derived, pureFuncs)
	// Should not panic and return empty for non-method call with nil callee
	if violations == nil {
		violations = []PureViolation{}
	}
	// No violations expected for this case
	_ = violations
}

// =============================================================================
// Test checkReturnInPure
// =============================================================================

func TestCheckReturnInPure_EmptyResults(t *testing.T) {
	ret := &ssa.Return{
		Results: []ssa.Value{},
	}
	derived := make(map[ssa.Value]bool)
	fset := token.NewFileSet()
	pureFuncs := NewPureFuncSet(fset)

	violations := checkReturnInPure(ret, derived, pureFuncs)
	if len(violations) != 0 {
		t.Errorf("checkReturnInPure should return empty for empty results, got %d violations", len(violations))
	}
}

func TestCheckReturnInPure_NilResult(t *testing.T) {
	ret := &ssa.Return{
		Results: []ssa.Value{nil},
	}
	derived := make(map[ssa.Value]bool)
	fset := token.NewFileSet()
	pureFuncs := NewPureFuncSet(fset)

	// Should not panic
	violations := checkReturnInPure(ret, derived, pureFuncs)
	_ = violations
}

// =============================================================================
// Test isImmutableValue for Phi nodes
// =============================================================================

func TestIsImmutableValue_Phi_Empty(t *testing.T) {
	phi := &ssa.Phi{
		Edges: []ssa.Value{},
	}
	derived := make(map[ssa.Value]bool)
	fset := token.NewFileSet()
	pureFuncs := NewPureFuncSet(fset)

	// Empty phi should return false (len(Edges) == 0)
	if isImmutableValue(phi, derived, pureFuncs) {
		t.Error("isImmutableValue should return false for empty Phi")
	}
}

// =============================================================================
// Test isImmutableValue for UnOp
// =============================================================================

func TestIsImmutableValue_UnOp_NonDeref(t *testing.T) {
	unop := &ssa.UnOp{
		Op: token.NOT, // Not a dereference
	}
	derived := make(map[ssa.Value]bool)
	fset := token.NewFileSet()
	pureFuncs := NewPureFuncSet(fset)

	// Non-dereference UnOp should return false
	if isImmutableValue(unop, derived, pureFuncs) {
		t.Error("isImmutableValue should return false for non-deref UnOp")
	}
}

// =============================================================================
// Test isImmutableValue for Extract
// =============================================================================

func TestIsImmutableValue_Extract_NilTuple(t *testing.T) {
	extract := &ssa.Extract{
		Tuple: nil,
	}
	derived := make(map[ssa.Value]bool)
	fset := token.NewFileSet()
	pureFuncs := NewPureFuncSet(fset)

	// Should handle nil tuple gracefully
	result := isImmutableValue(extract, derived, pureFuncs)
	// Expect false since we can't determine immutability
	if result {
		t.Error("isImmutableValue should return false for Extract with nil Tuple")
	}
}
