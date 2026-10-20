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
		{"ToSQL reads", "ToSQL", true},
		// State-only methods do not branch either (#307)
		{"AddError does not branch", "AddError", true},
		{"Get does not branch", "Get", true},
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

func TestIsStateOnlyMethod(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"AddError", "DB", "Get", "InstanceGet", "Callback"} {
		if !IsStateOnlyMethod(name) {
			t.Errorf("IsStateOnlyMethod(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"Session", "ToSQL", "Transaction", "Where", "Find", "Migrator", "Set"} {
		if IsStateOnlyMethod(name) {
			t.Errorf("IsStateOnlyMethod(%q) = true, want false", name)
		}
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

func TestHasGormDBResult(t *testing.T) {
	t.Parallel()

	gormPkg := types.NewPackage("gorm.io/gorm", "gorm")
	dbTypeName := types.NewTypeName(0, gormPkg, "DB", nil)
	dbPtr := types.NewPointer(types.NewNamed(dbTypeName, types.NewStruct(nil, nil), nil))
	errType := types.Universe.Lookup("error").Type()
	result := func(ts ...types.Type) *types.Tuple {
		vars := make([]*types.Var, len(ts))
		for i, typ := range ts {
			vars[i] = types.NewVar(0, nil, "", typ)
		}
		return types.NewTuple(vars...)
	}

	tests := []struct {
		name string
		typ  types.Type
		want bool
	}{
		{"*gorm.DB", dbPtr, true},
		{"error", errType, false},
		{"(*gorm.DB, error)", result(dbPtr, errType), true},
		{"(error, *gorm.DB)", result(errType, dbPtr), true},
		{"(int, error)", result(types.Typ[types.Int], errType), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := HasGormDBResult(tt.typ); got != tt.want {
				t.Errorf("HasGormDBResult(%s) = %v, want %v", tt.typ, got, tt.want)
			}
		})
	}
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

func TestIsGormDBAlias(t *testing.T) {
	t.Parallel()

	// go/types always materializes an alias declaration as a *types.Alias node,
	// so both `type Q = *gorm.DB` and `type D = gorm.DB` reach IsGormDB wrapped.
	gormPkg := types.NewPackage("gorm.io/gorm", "gorm")
	dbTypeName := types.NewTypeName(0, gormPkg, "DB", nil)
	dbType := types.NewNamed(dbTypeName, types.NewStruct(nil, nil), nil)
	gormPkg.Scope().Insert(dbTypeName)

	userPkg := types.NewPackage("example.com/app", "app")

	t.Run("alias to gorm.DB", func(t *testing.T) {
		t.Parallel()

		alias := types.NewAlias(types.NewTypeName(0, userPkg, "D", nil), dbType)
		if !IsGormDB(alias) {
			t.Error("IsGormDB(type D = gorm.DB) should return true")
		}
		if !IsGormDB(types.NewPointer(alias)) {
			t.Error("IsGormDB(*D) where D = gorm.DB should return true")
		}
	})

	t.Run("alias to *gorm.DB", func(t *testing.T) {
		t.Parallel()

		alias := types.NewAlias(types.NewTypeName(0, userPkg, "Q", nil), types.NewPointer(dbType))
		if !IsGormDB(alias) {
			t.Error("IsGormDB(type Q = *gorm.DB) should return true")
		}
	})

	t.Run("alias to an unrelated type", func(t *testing.T) {
		t.Parallel()

		alias := types.NewAlias(types.NewTypeName(0, userPkg, "N", nil), types.Typ[types.Int])
		if IsGormDB(alias) {
			t.Error("IsGormDB(type N = int) should return false")
		}
	})
}

func TestContainsGormDB(t *testing.T) {
	t.Parallel()

	// Create packages programmatically to avoid gorm.io/gorm dependency
	// First, create a mock gorm.io/gorm package with DB type
	gormPkg := types.NewPackage("gorm.io/gorm", "gorm")
	dbTypeName := types.NewTypeName(0, gormPkg, "DB", nil)
	dbStruct := types.NewStruct(nil, nil)
	dbType := types.NewNamed(dbTypeName, dbStruct, nil)
	gormPkg.Scope().Insert(dbTypeName)
	dbPtrType := types.NewPointer(dbType)

	// Create test types
	tests := []struct {
		name     string
		typ      types.Type
		expected bool
	}{
		{
			name:     "direct *gorm.DB",
			typ:      dbPtrType,
			expected: true,
		},
		{
			name:     "direct gorm.DB (non-pointer, still dangerous)",
			typ:      dbType,
			expected: true,
		},
		{
			name:     "struct with *gorm.DB field",
			typ:      types.NewStruct([]*types.Var{types.NewField(0, nil, "db", dbPtrType, false)}, nil),
			expected: true,
		},
		{
			name:     "struct without *gorm.DB field",
			typ:      types.NewStruct([]*types.Var{types.NewField(0, nil, "name", types.Typ[types.String], false)}, nil),
			expected: false,
		},
		{
			name:     "slice of *gorm.DB",
			typ:      types.NewSlice(dbPtrType),
			expected: true,
		},
		{
			name:     "array of *gorm.DB",
			typ:      types.NewArray(dbPtrType, 2),
			expected: true,
		},
		{
			name:     "map with *gorm.DB value",
			typ:      types.NewMap(types.Typ[types.String], dbPtrType),
			expected: true,
		},
		{
			name:     "map with *gorm.DB key",
			typ:      types.NewMap(dbPtrType, types.Typ[types.String]),
			expected: true,
		},
		{
			name:     "chan of *gorm.DB",
			typ:      types.NewChan(types.SendRecv, dbPtrType),
			expected: true,
		},
		{
			name:     "pointer to struct with *gorm.DB",
			typ:      types.NewPointer(types.NewStruct([]*types.Var{types.NewField(0, nil, "db", dbPtrType, false)}, nil)),
			expected: true,
		},
		{
			name:     "simple int",
			typ:      types.Typ[types.Int],
			expected: false,
		},
		{
			name:     "slice of int",
			typ:      types.NewSlice(types.Typ[types.Int]),
			expected: false,
		},
		{
			// gap4 (#72): containsGormDB drives ONLY unused-directive detection,
			// which requires a concrete *gorm.DB in the signature. A bare
			// interface is no longer treated as potentially-gorm.
			name:     "empty interface (interface{})",
			typ:      types.NewInterfaceType(nil, nil),
			expected: false,
		},
		{
			name:     "non-empty interface",
			typ:      types.NewInterfaceType([]*types.Func{types.NewFunc(0, nil, "Method", types.NewSignatureType(nil, nil, nil, nil, nil, false))}, nil),
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := containsGormDB(tt.typ)
			if got != tt.expected {
				t.Errorf("containsGormDB(%s) = %v, want %v", tt.name, got, tt.expected)
			}
		})
	}
}

func TestContainsGormDBDefinedType(t *testing.T) {
	t.Parallel()

	// Create gorm package
	gormPkg := types.NewPackage("gorm.io/gorm", "gorm")
	dbTypeName := types.NewTypeName(0, gormPkg, "DB", nil)
	dbStruct := types.NewStruct(nil, nil)
	dbType := types.NewNamed(dbTypeName, dbStruct, nil)
	gormPkg.Scope().Insert(dbTypeName)
	dbPtrType := types.NewPointer(dbType)

	// Create a defined type like `type DefinedDB *gorm.DB`
	testPkg := types.NewPackage("test", "test")
	definedTypeName := types.NewTypeName(0, testPkg, "DefinedDB", nil)
	definedType := types.NewNamed(definedTypeName, dbPtrType, nil)

	if !containsGormDB(definedType) {
		t.Error("containsGormDB(DefinedDB) should return true for type DefinedDB *gorm.DB")
	}
}

func TestContainsGormDBNilType(t *testing.T) {
	t.Parallel()

	// Test nil type
	if containsGormDB(nil) {
		t.Error("containsGormDB(nil) should return false")
	}
}

func TestContainsGormDBCycleDetection(t *testing.T) {
	t.Parallel()

	// Test cycle detection with recursive types using programmatic type creation
	testPkg := types.NewPackage("test", "test")

	// Create a recursive type: type Recursive struct { self *Recursive; name string }
	recursiveTypeName := types.NewTypeName(0, testPkg, "Recursive", nil)
	recursiveStruct := types.NewStruct(nil, nil) // placeholder
	recursiveType := types.NewNamed(recursiveTypeName, recursiveStruct, nil)

	// Now create the actual struct with the pointer to itself
	selfField := types.NewField(0, testPkg, "self", types.NewPointer(recursiveType), false)
	nameField := types.NewField(0, testPkg, "name", types.Typ[types.String], false)
	actualStruct := types.NewStruct([]*types.Var{selfField, nameField}, nil)
	recursiveType.SetUnderlying(actualStruct)

	// Should not panic and should return false (no *gorm.DB)
	got := containsGormDB(recursiveType)
	if got {
		t.Error("containsGormDB(Recursive) should return false")
	}
}
