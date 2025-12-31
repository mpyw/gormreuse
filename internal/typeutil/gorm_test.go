package typeutil

import (
	"go/types"
	"testing"
)

func TestIsImmutableReturningBuiltin(t *testing.T) {
	t.Parallel()

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
			t.Parallel()

			if got := IsImmutableReturningBuiltin(tt.method); got != tt.expected {
				t.Errorf("IsImmutableReturningBuiltin(%q) = %v, want %v", tt.method, got, tt.expected)
			}
		})
	}
}

func TestIsGormDB(t *testing.T) {
	t.Parallel()

	t.Run("nil type", func(t *testing.T) {
		t.Parallel()

		if IsGormDB(nil) {
			t.Error("IsGormDB(nil) should return false")
		}
	})

	t.Run("non-pointer type", func(t *testing.T) {
		t.Parallel()

		basicType := types.Typ[types.Int]
		if IsGormDB(basicType) {
			t.Error("IsGormDB(int) should return false")
		}
	})

	t.Run("pointer to basic type", func(t *testing.T) {
		t.Parallel()

		ptrToInt := types.NewPointer(types.Typ[types.Int])
		if IsGormDB(ptrToInt) {
			t.Error("IsGormDB(*int) should return false")
		}
	})

	t.Run("pointer to unnamed struct", func(t *testing.T) {
		t.Parallel()

		structType := types.NewStruct(nil, nil)
		ptrToStruct := types.NewPointer(structType)
		if IsGormDB(ptrToStruct) {
			t.Error("IsGormDB(*struct{}) should return false")
		}
	})

	t.Run("*gorm.DB", func(t *testing.T) {
		t.Parallel()

		// Create gorm package and DB type programmatically
		gormPkg := types.NewPackage("gorm.io/gorm", "gorm")
		dbTypeName := types.NewTypeName(0, gormPkg, "DB", nil)
		dbStruct := types.NewStruct(nil, nil)
		dbType := types.NewNamed(dbTypeName, dbStruct, nil)
		gormPkg.Scope().Insert(dbTypeName)

		dbPtrType := types.NewPointer(dbType)
		if !IsGormDB(dbPtrType) {
			t.Error("IsGormDB(*gorm.DB) should return true")
		}
	})

	t.Run("gorm.DB (non-pointer)", func(t *testing.T) {
		t.Parallel()

		gormPkg := types.NewPackage("gorm.io/gorm", "gorm")
		dbTypeName := types.NewTypeName(0, gormPkg, "DB", nil)
		dbStruct := types.NewStruct(nil, nil)
		dbType := types.NewNamed(dbTypeName, dbStruct, nil)
		gormPkg.Scope().Insert(dbTypeName)

		if !IsGormDB(dbType) {
			t.Error("IsGormDB(gorm.DB) should return true - non-pointer is still dangerous")
		}
	})

	t.Run("wrong package path", func(t *testing.T) {
		t.Parallel()

		fakePkg := types.NewPackage("evil.com/fake-gorm.io/gorm", "gorm")
		fakeTypeName := types.NewTypeName(0, fakePkg, "DB", nil)
		fakeDBType := types.NewNamed(fakeTypeName, types.NewStruct(nil, nil), nil)
		fakePkg.Scope().Insert(fakeTypeName)

		if IsGormDB(types.NewPointer(fakeDBType)) {
			t.Error("IsGormDB(*fake/gorm.DB) should return false")
		}
	})

	t.Run("**gorm.DB (double pointer)", func(t *testing.T) {
		t.Parallel()

		gormPkg := types.NewPackage("gorm.io/gorm", "gorm")
		dbTypeName := types.NewTypeName(0, gormPkg, "DB", nil)
		dbStruct := types.NewStruct(nil, nil)
		dbType := types.NewNamed(dbTypeName, dbStruct, nil)
		gormPkg.Scope().Insert(dbTypeName)

		dbPtrType := types.NewPointer(dbType)
		dbDoublePtrType := types.NewPointer(dbPtrType)
		if IsGormDB(dbDoublePtrType) {
			t.Error("IsGormDB(**gorm.DB) should return false - use ClosureCapturesGormDB for nested pointers")
		}
	})

	t.Run("interface{}", func(t *testing.T) {
		t.Parallel()

		emptyInterface := types.NewInterfaceType(nil, nil).Complete()
		if IsGormDB(emptyInterface) {
			t.Error("IsGormDB(interface{}) should return false - use containsGormDB for interface checks")
		}
	})
}

func TestIsGormDBNamed(t *testing.T) {
	t.Parallel()

	t.Run("nil type", func(t *testing.T) {
		t.Parallel()

		if isGormDBNamed(nil) {
			t.Error("isGormDBNamed(nil) should return false")
		}
	})

	t.Run("basic type", func(t *testing.T) {
		t.Parallel()

		if isGormDBNamed(types.Typ[types.Int]) {
			t.Error("isGormDBNamed(int) should return false")
		}
	})

	t.Run("named type from wrong package", func(t *testing.T) {
		t.Parallel()

		pkg := types.NewPackage("other/package", "other")
		obj := types.NewTypeName(0, pkg, "DB", nil)
		named := types.NewNamed(obj, types.NewStruct(nil, nil), nil)

		if isGormDBNamed(named) {
			t.Error("isGormDBNamed(other.DB) should return false")
		}
	})

	t.Run("named type with nil pkg", func(t *testing.T) {
		t.Parallel()

		objNilPkg := types.NewTypeName(0, nil, "DB", nil)
		namedNilPkg := types.NewNamed(objNilPkg, types.NewStruct(nil, nil), nil)

		if isGormDBNamed(namedNilPkg) {
			t.Error("isGormDBNamed with nil pkg should return false")
		}
	})
}
