package typeutil

import (
	"go/types"
	"testing"
)

func TestIsImmutableReturningBuiltin(t *testing.T) {
	tests := []struct {
		name     string
		method   string
		expected bool
	}{
		// Pure builtin methods
		{"Session is pure", "Session", true},
		{"WithContext is pure", "WithContext", true},
		{"Debug is pure", "Debug", true},
		{"Open is pure", "Open", true},
		{"Begin is pure", "Begin", true},
		{"Transaction is pure", "Transaction", true},
		// Non-pure methods (chain methods)
		{"Find is not pure", "Find", false},
		{"Where is not pure", "Where", false},
		{"Create is not pure", "Create", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsImmutableReturningBuiltin(tt.method); got != tt.expected {
				t.Errorf("IsImmutableReturningBuiltin(%q) = %v, want %v", tt.method, got, tt.expected)
			}
		})
	}
}

func TestIsGormDB(t *testing.T) {
	// Test with nil
	if IsGormDB(nil) {
		t.Error("IsGormDB(nil) should return false")
	}

	// Test with non-pointer type
	basicType := types.Typ[types.Int]
	if IsGormDB(basicType) {
		t.Error("IsGormDB(int) should return false")
	}

	// Test with pointer to basic type
	ptrToInt := types.NewPointer(types.Typ[types.Int])
	if IsGormDB(ptrToInt) {
		t.Error("IsGormDB(*int) should return false")
	}

	// Test with pointer to unnamed struct
	structType := types.NewStruct(nil, nil)
	ptrToStruct := types.NewPointer(structType)
	if IsGormDB(ptrToStruct) {
		t.Error("IsGormDB(*struct{}) should return false")
	}
}

func TestIsGormDBNamed(t *testing.T) {
	// Test with nil
	if isGormDBNamed(nil) {
		t.Error("isGormDBNamed(nil) should return false")
	}

	// Test with basic type
	if isGormDBNamed(types.Typ[types.Int]) {
		t.Error("isGormDBNamed(int) should return false")
	}

	// Test with named type from wrong package
	pkg := types.NewPackage("other/package", "other")
	obj := types.NewTypeName(0, pkg, "DB", nil)
	named := types.NewNamed(obj, types.NewStruct(nil, nil), nil)
	if isGormDBNamed(named) {
		t.Error("isGormDBNamed(other.DB) should return false")
	}

	// Test with named type with nil pkg
	objNilPkg := types.NewTypeName(0, nil, "DB", nil)
	namedNilPkg := types.NewNamed(objNilPkg, types.NewStruct(nil, nil), nil)
	if isGormDBNamed(namedNilPkg) {
		t.Error("isGormDBNamed with nil pkg should return false")
	}
}
