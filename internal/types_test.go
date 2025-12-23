package internal

import (
	"go/types"
	"testing"
)

func TestReturnsImmutable(t *testing.T) {
	tests := []struct {
		name     string
		method   string
		expected bool
	}{
		// Safe methods
		{"Session returns immutable", "Session", true},
		{"WithContext returns immutable", "WithContext", true},
		{"Debug returns immutable", "Debug", true},
		// Init methods
		{"Open returns immutable", "Open", true},
		{"Begin returns immutable", "Begin", true},
		{"Transaction returns immutable", "Transaction", true},
		// Chain methods
		{"Find does not return immutable", "Find", false},
		{"Where does not return immutable", "Where", false},
		{"Create does not return immutable", "Create", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ReturnsImmutable(tt.method); got != tt.expected {
				t.Errorf("ReturnsImmutable(%q) = %v, want %v", tt.method, got, tt.expected)
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
