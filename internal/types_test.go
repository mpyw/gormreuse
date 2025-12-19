package internal

import (
	"go/types"
	"testing"
)

func TestIsSafeMethod(t *testing.T) {
	tests := []struct {
		name     string
		method   string
		expected bool
	}{
		{"Session is safe", "Session", true},
		{"WithContext is safe", "WithContext", true},
		{"Find is not safe", "Find", false},
		{"Where is not safe", "Where", false},
		{"Create is not safe", "Create", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsSafeMethod(tt.method); got != tt.expected {
				t.Errorf("IsSafeMethod(%q) = %v, want %v", tt.method, got, tt.expected)
			}
		})
	}
}

func TestIsDBInitMethod(t *testing.T) {
	tests := []struct {
		name     string
		method   string
		expected bool
	}{
		{"Begin is init", "Begin", true},
		{"Transaction is init", "Transaction", true},
		{"Find is not init", "Find", false},
		{"Session is not init", "Session", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsDBInitMethod(tt.method); got != tt.expected {
				t.Errorf("IsDBInitMethod(%q) = %v, want %v", tt.method, got, tt.expected)
			}
		})
	}
}

func TestIsChainMethod(t *testing.T) {
	tests := []struct {
		name     string
		method   string
		expected bool
	}{
		{"Find is chain", "Find", true},
		{"Where is chain", "Where", true},
		{"Create is chain", "Create", true},
		{"Session is not chain", "Session", false},
		{"WithContext is not chain", "WithContext", false},
		{"Begin is not chain", "Begin", false},
		{"Transaction is not chain", "Transaction", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsChainMethod(tt.method); got != tt.expected {
				t.Errorf("IsChainMethod(%q) = %v, want %v", tt.method, got, tt.expected)
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
