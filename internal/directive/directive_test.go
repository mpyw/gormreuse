package directive

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"slices"
	"testing"
)

func TestIsIgnoreDirective(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		text     string
		expected bool
	}{
		{"exact match", "//gormreuse:ignore", true},
		{"with space", "// gormreuse:ignore", false},
		{"with extra spaces", "//  gormreuse:ignore", false},
		{"with comment", "//gormreuse:ignore // reason", true},
		{"wrong directive", "//gormreuse:pure", false},
		{"random comment", "// some comment", false},
		{"empty", "//", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := IsIgnoreDirective(tt.text); got != tt.expected {
				t.Errorf("IsIgnoreDirective(%q) = %v, want %v", tt.text, got, tt.expected)
			}
		})
	}
}

func TestIsPureDirective(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		text     string
		expected bool
	}{
		{"exact match", "//gormreuse:pure", true},
		{"with space", "// gormreuse:pure", false},
		{"with extra spaces", "//  gormreuse:pure", false},
		{"wrong directive", "//gormreuse:ignore", false},
		{"random comment", "// some comment", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := IsPureDirective(tt.text); got != tt.expected {
				t.Errorf("IsPureDirective(%q) = %v, want %v", tt.text, got, tt.expected)
			}
		})
	}
}

func TestIsImmutableParamDirective(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		text     string
		expected bool
	}{
		{"exact match", "//gormreuse:immutable-param", true},
		{"with space", "// gormreuse:immutable-param", false},
		{"block comment", "/*gormreuse:immutable-param*/", false},
		{"combined with pure", "//gormreuse:pure,immutable-param", true},
		{"combined with immutable-return", "//gormreuse:immutable-return,immutable-param", true},
		{"trailing comment", "//gormreuse:immutable-param // callers pass root handles", true},
		{"not a prefix of immutable-return", "//gormreuse:immutable-return", false},
		{"wrong directive", "//gormreuse:pure", false},
		{"random comment", "// some comment", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := IsImmutableParamDirective(tt.text); got != tt.expected {
				t.Errorf("IsImmutableParamDirective(%q) = %v, want %v", tt.text, got, tt.expected)
			}
		})
	}
}

func TestIgnoreMapShouldIgnore(t *testing.T) {
	t.Parallel()

	t.Run("same line", func(t *testing.T) {
		t.Parallel()

		m := make(IgnoreMap)
		m[10] = &ignoreEntry{pos: token.Pos(100), used: false}

		if !m.ShouldIgnore(10) {
			t.Error("ShouldIgnore(10) should return true (same line)")
		}
	})

	t.Run("next line", func(t *testing.T) {
		t.Parallel()

		m := make(IgnoreMap)
		m[20] = &ignoreEntry{pos: token.Pos(200), used: false}

		if !m.ShouldIgnore(21) {
			t.Error("ShouldIgnore(21) should return true (previous line has ignore)")
		}
	})

	t.Run("non-ignored line", func(t *testing.T) {
		t.Parallel()

		m := make(IgnoreMap)
		m[10] = &ignoreEntry{pos: token.Pos(100), used: false}

		if m.ShouldIgnore(5) {
			t.Error("ShouldIgnore(5) should return false")
		}
	})
}

func TestIgnoreMapFileLevel(t *testing.T) {
	t.Parallel()

	m := make(IgnoreMap)
	m[-1] = &ignoreEntry{pos: token.Pos(1), used: true}

	// File-level ignore should affect all lines
	if !m.ShouldIgnore(100) {
		t.Error("ShouldIgnore(100) should return true with file-level ignore")
	}
}

func TestIgnoreMapGetUnusedIgnores(t *testing.T) {
	t.Parallel()

	m := make(IgnoreMap)
	m[10] = &ignoreEntry{pos: token.Pos(100), used: false}
	m[20] = &ignoreEntry{pos: token.Pos(200), used: false}

	// Mark line 20 as used by calling ShouldIgnore
	m.ShouldIgnore(20)

	unused := m.GetUnusedIgnores()
	if len(unused) != 1 {
		t.Errorf("Expected 1 unused ignore, got %d", len(unused))
	}
	if len(unused) > 0 && unused[0] != token.Pos(100) {
		t.Errorf("Expected pos 100, got %v", unused[0])
	}
}

func TestIgnoreMapGetUnusedIgnoresWithFileLevel(t *testing.T) {
	t.Parallel()

	m := make(IgnoreMap)
	m[10] = &ignoreEntry{pos: token.Pos(100), used: false}
	m[-1] = &ignoreEntry{pos: token.Pos(1), used: true} // File-level ignore

	// When file-level ignore is present, line-level ignores are not used
	// because file-level takes precedence
	unused := m.GetUnusedIgnores()
	if len(unused) != 1 {
		t.Errorf("Expected 1 unused ignore, got %d", len(unused))
	}
	if len(unused) > 0 && unused[0] != token.Pos(100) {
		t.Errorf("Expected pos 100, got %v", unused[0])
	}
}

func TestIgnoreMapMarkUsed(t *testing.T) {
	t.Parallel()

	t.Run("mark existing entry", func(t *testing.T) {
		t.Parallel()

		m := make(IgnoreMap)
		m[10] = &ignoreEntry{pos: token.Pos(100), used: false}

		m.MarkUsed(10)
		unused := m.GetUnusedIgnores()
		if len(unused) != 0 {
			t.Error("Entry at line 10 should be marked as used")
		}
	})

	t.Run("mark non-existent line should not panic", func(t *testing.T) {
		t.Parallel()

		m := make(IgnoreMap)
		m.MarkUsed(999)
	})
}

func TestBuildIgnoreMap(t *testing.T) {
	t.Parallel()

	src := `//gormreuse:ignore
package test

//gormreuse:ignore
func foo() {}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "test.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("Failed to parse: %v", err)
	}

	m := BuildIgnoreMap(fset, file)
	if len(m) == 0 {
		t.Error("Expected non-empty ignore map")
	}
}

func TestBuildIgnoreMapWithDocComment(t *testing.T) {
	t.Parallel()

	src := `//gormreuse:ignore
// Package test is a test package.
package test

func foo() {}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "test.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("Failed to parse: %v", err)
	}

	m := BuildIgnoreMap(fset, file)

	// Check that file has doc and it contains ignore
	if file.Doc != nil {
		for _, c := range file.Doc.List {
			if IsIgnoreDirective(c.Text) {
				// File-level ignore should be present
				if !m.ShouldIgnore(1) {
					t.Error("Expected file-level ignore to affect line 1")
				}
			}
		}
	}
}

func TestBuildFunctionIgnoreSet(t *testing.T) {
	t.Parallel()

	src := `package test

//gormreuse:ignore
func ignored() {}

func notIgnored() {}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "test.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("Failed to parse: %v", err)
	}

	set := BuildFunctionIgnoreSet(fset, file)
	if len(set) != 1 {
		t.Errorf("Expected 1 ignored function, got %d", len(set))
	}
}

func TestBuildPureFuncSet(t *testing.T) {
	t.Parallel()

	src := `package test

type Receiver struct{}
type GenericReceiver[T any] struct{}

// 1. Regular function
//gormreuse:pure
func pureFunc() {}

// 2. Value receiver method
//gormreuse:pure
func (r Receiver) pureValueMethod() {}

// 3. Pointer receiver method
//gormreuse:pure
func (r *Receiver) purePointerMethod() {}

// 4. Generic function
//gormreuse:pure
func pureGenericFunc[T any]() {}

// 5. Generic value receiver method
//gormreuse:pure
func (r GenericReceiver[T]) pureGenericValueMethod() {}

// 6. Generic pointer receiver method
//gormreuse:pure
func (r *GenericReceiver[T]) pureGenericPointerMethod() {}

func notPure() {}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "test.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("Failed to parse: %v", err)
	}

	set := BuildPureFuncSet(file, "test/pkg")
	if len(set) != 6 {
		t.Errorf("Expected 6 pure functions, got %d", len(set))
	}

	tests := []struct {
		name string
		key  FuncKey
	}{
		{"regular function", FuncKey{PkgPath: "test/pkg", FuncName: "pureFunc"}},
		{"value receiver method", FuncKey{PkgPath: "test/pkg", ReceiverType: "Receiver", FuncName: "pureValueMethod"}},
		{"pointer receiver method", FuncKey{PkgPath: "test/pkg", ReceiverType: "Receiver", FuncName: "purePointerMethod"}},
		{"generic function", FuncKey{PkgPath: "test/pkg", FuncName: "pureGenericFunc"}},
		{"generic value receiver method", FuncKey{PkgPath: "test/pkg", ReceiverType: "GenericReceiver", FuncName: "pureGenericValueMethod"}},
		{"generic pointer receiver method", FuncKey{PkgPath: "test/pkg", ReceiverType: "GenericReceiver", FuncName: "pureGenericPointerMethod"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if _, ok := set[tt.key]; !ok {
				t.Errorf("Expected %s in set (key: %+v)", tt.name, tt.key)
			}
		})
	}
}

func TestExprToString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		src      string
		expected string
	}{
		{
			name:     "simple ident",
			src:      `package test; func (r Foo) m() {}`,
			expected: "Foo",
		},
		{
			name:     "pointer receiver",
			src:      `package test; func (r *Foo) m() {}`,
			expected: "*Foo",
		},
		{
			name:     "selector",
			src:      `package test; func (r pkg.Foo) m() {}`,
			expected: "pkg.Foo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "test.go", tt.src, 0)
			if err != nil {
				t.Fatalf("Failed to parse: %v", err)
			}

			for _, decl := range file.Decls {
				if fn, ok := decl.(*ast.FuncDecl); ok {
					if fn.Recv != nil && len(fn.Recv.List) > 0 {
						got := exprToTypeString(fn.Recv.List[0].Type)
						if got != tt.expected {
							t.Errorf("exprToTypeString() = %q, want %q", got, tt.expected)
						}
					}
				}
			}
		})
	}
}

func TestExprToStringUnknown(t *testing.T) {
	t.Parallel()

	// Test with an expression type that returns empty string
	// ArrayType is not handled by exprToTypeString
	src := `package test; func (r [2]int) m() {}`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "test.go", src, 0)
	if err != nil {
		t.Fatalf("Failed to parse: %v", err)
	}

	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok {
			if fn.Recv != nil && len(fn.Recv.List) > 0 {
				got := exprToTypeString(fn.Recv.List[0].Type)
				if got != "" {
					t.Errorf("exprToTypeString(ArrayType) = %q, want empty string", got)
				}
			}
		}
	}
}

func TestExprToStringGeneric(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		src      string
		expected string
	}{
		{
			name:     "single type parameter",
			src:      `package test; func (r Generic[T]) m() {}`,
			expected: "Generic",
		},
		{
			name:     "single type parameter pointer",
			src:      `package test; func (r *Generic[T]) m() {}`,
			expected: "*Generic",
		},
		{
			name:     "multiple type parameters",
			src:      `package test; func (r Generic[T, U]) m() {}`,
			expected: "Generic",
		},
		{
			name:     "multiple type parameters pointer",
			src:      `package test; func (r *Generic[T, U]) m() {}`,
			expected: "*Generic",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "test.go", tt.src, 0)
			if err != nil {
				t.Fatalf("Failed to parse: %v", err)
			}

			for _, decl := range file.Decls {
				if fn, ok := decl.(*ast.FuncDecl); ok {
					if fn.Recv != nil && len(fn.Recv.List) > 0 {
						got := exprToTypeString(fn.Recv.List[0].Type)
						if got != tt.expected {
							t.Errorf("exprToTypeString() = %q, want %q", got, tt.expected)
						}
					}
				}
			}
		})
	}
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

func TestParseDirective(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		text string
		want []string
	}{
		{"canonical", "//gormreuse:pure", []string{"pure"}},
		{"comma list", "//gormreuse:pure,immutable-return", []string{"pure", "immutable-return"}},
		{"comma list with spaces", "//gormreuse:pure, immutable-return , immutable-param", []string{"pure", "immutable-return", "immutable-param"}},
		{"trailing reason", "//gormreuse:ignore // reason here", []string{"ignore"}},
		{"trailing reason without space", "//gormreuse:ignore// reason", []string{"ignore"}},
		{"comma list with trailing reason", "//gormreuse:pure,immutable-return // note", []string{"pure", "immutable-return"}},
		{"immutable-input combined", "//gormreuse:pure,immutable-input(fn)", []string{"pure", "immutable-input(fn)"}},
		{"space after marker", "// gormreuse:pure", nil},
		{"tab after marker", "//\tgormreuse:pure", nil},
		{"space after colon", "//gormreuse: pure", nil},
		{"block form", "/*gormreuse:pure*/", nil},
		{"spaced block form", "/* gormreuse:pure */", nil},
		{"uppercase name", "//gormreuse:Pure", nil},
		{"lookalike tool", "//gormreusex:pure", nil},
		{"tool as suffix", "//xgormreuse:pure", nil},
		{"other tool", "//nolint:gormreuse", nil},
		{"no name", "//gormreuse:", nil},
		{"random comment", "// some comment", nil},
		{"empty", "//", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := parseDirective(tt.text); !slices.Equal(got, tt.want) {
				t.Errorf("parseDirective(%q) = %q, want %q", tt.text, got, tt.want)
			}
		})
	}
}

func TestHasDirectiveForms(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		text  string
		check func(string) bool
		want  bool
	}{
		{"pure in spaced comma list", "//gormreuse:pure, immutable-return", IsPureDirective, true},
		{"immutable-return in spaced comma list", "//gormreuse:pure, immutable-return", IsImmutableReturnDirective, true},
		{"pure next to immutable-input", "//gormreuse:immutable-input(fn),pure // reason", IsPureDirective, true},
		{"ignore with block form", "/*gormreuse:ignore*/", IsIgnoreDirective, false},
		{"ignore with space after marker", "// gormreuse:ignore", IsIgnoreDirective, false},
		{"name in trailing reason does not count", "//gormreuse:ignore // not pure", IsPureDirective, false},
		{"lookalike tool", "//gormreusex:pure", IsPureDirective, false},
		{"name prefix does not count", "//gormreuse:pure-ish", IsPureDirective, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := tt.check(tt.text); got != tt.want {
				t.Errorf("check(%q) = %v, want %v", tt.text, got, tt.want)
			}
		})
	}
}

func TestIsMalformedDirective(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		text string
		want bool
	}{
		{"space after marker", "// gormreuse:pure", true},
		{"tab after marker", "//\tgormreuse:pure", true},
		{"space after colon", "//gormreuse: pure", true},
		{"block form", "/*gormreuse:pure*/", true},
		{"spaced block form", "/* gormreuse:pure */", true},
		{"uppercase name", "//gormreuse:Pure", true},
		{"no name", "//gormreuse:", true},
		{"no name in block form", "/*gormreuse:*/", true},
		{"canonical", "//gormreuse:pure", false},
		{"canonical comma list", "//gormreuse:pure,immutable-return", false},
		{"canonical with reason", "//gormreuse:ignore // reason", false},
		{"canonical immutable-input", "//gormreuse:immutable-input(fn)", false},
		{"lookalike tool", "// gormreusex:pure", false},
		{"prose mentioning the tool", "// see gormreuse:pure for details", false},
		{"random comment", "// some comment", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := IsMalformedDirective(tt.text); got != tt.want {
				t.Errorf("IsMalformedDirective(%q) = %v, want %v", tt.text, got, tt.want)
			}
		})
	}
}

func TestFindMalformedDirectives(t *testing.T) {
	t.Parallel()

	src := `package test

// gormreuse:pure
func a() {}

//gormreuse:pure
func b() {}

func c() {
	_ = 1 /*gormreuse:ignore*/
}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "test.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("Failed to parse: %v", err)
	}

	var lines []int
	for _, pos := range FindMalformedDirectives(file) {
		lines = append(lines, fset.Position(pos).Line)
	}
	if want := []int{3, 10}; !slices.Equal(lines, want) {
		t.Errorf("FindMalformedDirectives lines = %v, want %v", lines, want)
	}
}
