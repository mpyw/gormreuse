package directive

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

func TestIsIgnoreDirective(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		expected bool
	}{
		{"exact match", "//gormreuse:ignore", true},
		{"with space", "// gormreuse:ignore", true},
		{"with extra spaces", "//  gormreuse:ignore", true},
		{"with comment", "//gormreuse:ignore // reason", true},
		{"wrong directive", "//gormreuse:pure", false},
		{"random comment", "// some comment", false},
		{"empty", "//", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsIgnoreDirective(tt.text); got != tt.expected {
				t.Errorf("IsIgnoreDirective(%q) = %v, want %v", tt.text, got, tt.expected)
			}
		})
	}
}

func TestIsPureDirective(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		expected bool
	}{
		{"exact match", "//gormreuse:pure", true},
		{"with space", "// gormreuse:pure", true},
		{"with extra spaces", "//  gormreuse:pure", true},
		{"wrong directive", "//gormreuse:ignore", false},
		{"random comment", "// some comment", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsPureDirective(tt.text); got != tt.expected {
				t.Errorf("IsPureDirective(%q) = %v, want %v", tt.text, got, tt.expected)
			}
		})
	}
}

func TestIgnoreMapShouldIgnore(t *testing.T) {
	m := make(IgnoreMap)
	m[10] = &ignoreEntry{pos: token.Pos(100), used: false}
	m[20] = &ignoreEntry{pos: token.Pos(200), used: false}

	// Test same line
	if !m.ShouldIgnore(10) {
		t.Error("ShouldIgnore(10) should return true (same line)")
	}
	if !m[10].used {
		t.Error("Entry at line 10 should be marked as used")
	}

	// Test next line
	if !m.ShouldIgnore(21) {
		t.Error("ShouldIgnore(21) should return true (previous line has ignore)")
	}
	if !m[20].used {
		t.Error("Entry at line 20 should be marked as used")
	}

	// Test non-ignored line
	if m.ShouldIgnore(5) {
		t.Error("ShouldIgnore(5) should return false")
	}
}

func TestIgnoreMapFileLevel(t *testing.T) {
	m := make(IgnoreMap)
	m[-1] = &ignoreEntry{pos: token.Pos(1), used: false}

	// File-level ignore should affect all lines
	if !m.ShouldIgnore(100) {
		t.Error("ShouldIgnore(100) should return true with file-level ignore")
	}
	if !m[-1].used {
		t.Error("File-level entry should be marked as used")
	}
}

func TestIgnoreMapGetUnusedIgnores(t *testing.T) {
	m := make(IgnoreMap)
	m[10] = &ignoreEntry{pos: token.Pos(100), used: false}
	m[20] = &ignoreEntry{pos: token.Pos(200), used: true}
	m[-1] = &ignoreEntry{pos: token.Pos(1), used: false} // File-level should be skipped

	unused := m.GetUnusedIgnores()
	if len(unused) != 1 {
		t.Errorf("Expected 1 unused ignore, got %d", len(unused))
	}
	if len(unused) > 0 && unused[0] != token.Pos(100) {
		t.Errorf("Expected pos 100, got %v", unused[0])
	}
}

func TestIgnoreMapMarkUsed(t *testing.T) {
	m := make(IgnoreMap)
	m[10] = &ignoreEntry{pos: token.Pos(100), used: false}

	m.MarkUsed(10)
	if !m[10].used {
		t.Error("Entry at line 10 should be marked as used")
	}

	// MarkUsed on non-existent line should not panic
	m.MarkUsed(999)
}

func TestBuildIgnoreMap(t *testing.T) {
	src := `// gormreuse:ignore
package test

// gormreuse:ignore
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
	src := `// gormreuse:ignore
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
				if _, ok := m[-1]; !ok {
					t.Error("Expected file-level ignore (-1) in map")
				}
			}
		}
	}
}

func TestBuildFunctionIgnoreSet(t *testing.T) {
	src := `package test

// gormreuse:ignore
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

func TestBuildPureFunctionSet(t *testing.T) {
	src := `package test

type Receiver struct{}
type GenericReceiver[T any] struct{}

// 1. Regular function
// gormreuse:pure
func pureFunc() {}

// 2. Value receiver method
// gormreuse:pure
func (r Receiver) pureValueMethod() {}

// 3. Pointer receiver method
// gormreuse:pure
func (r *Receiver) purePointerMethod() {}

// 4. Generic function
// gormreuse:pure
func pureGenericFunc[T any]() {}

// 5. Generic value receiver method
// gormreuse:pure
func (r GenericReceiver[T]) pureGenericValueMethod() {}

// 6. Generic pointer receiver method
// gormreuse:pure
func (r *GenericReceiver[T]) pureGenericPointerMethod() {}

func notPure() {}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "test.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("Failed to parse: %v", err)
	}

	set := BuildPureFunctionSet(file, "test/pkg")
	if len(set) != 6 {
		t.Errorf("Expected 6 pure functions, got %d", len(set))
	}

	tests := []struct {
		name string
		key  PureFuncKey
	}{
		{"regular function", PureFuncKey{PkgPath: "test/pkg", FuncName: "pureFunc"}},
		{"value receiver method", PureFuncKey{PkgPath: "test/pkg", ReceiverType: "Receiver", FuncName: "pureValueMethod"}},
		{"pointer receiver method", PureFuncKey{PkgPath: "test/pkg", ReceiverType: "Receiver", FuncName: "purePointerMethod"}},
		{"generic function", PureFuncKey{PkgPath: "test/pkg", FuncName: "pureGenericFunc"}},
		{"generic value receiver method", PureFuncKey{PkgPath: "test/pkg", ReceiverType: "GenericReceiver", FuncName: "pureGenericValueMethod"}},
		{"generic pointer receiver method", PureFuncKey{PkgPath: "test/pkg", ReceiverType: "GenericReceiver", FuncName: "pureGenericPointerMethod"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, ok := set[tt.key]; !ok {
				t.Errorf("Expected %s in set (key: %+v)", tt.name, tt.key)
			}
		})
	}
}

func TestExprToString(t *testing.T) {
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
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "test.go", tt.src, 0)
			if err != nil {
				t.Fatalf("Failed to parse: %v", err)
			}

			for _, decl := range file.Decls {
				if fn, ok := decl.(*ast.FuncDecl); ok {
					if fn.Recv != nil && len(fn.Recv.List) > 0 {
						got := exprToString(fn.Recv.List[0].Type)
						if got != tt.expected {
							t.Errorf("exprToString() = %q, want %q", got, tt.expected)
						}
					}
				}
			}
		})
	}
}

func TestExprToStringUnknown(t *testing.T) {
	// Test with an expression type that returns empty string
	// ArrayType is not handled by exprToString
	src := `package test; func (r [2]int) m() {}`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "test.go", src, 0)
	if err != nil {
		t.Fatalf("Failed to parse: %v", err)
	}

	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok {
			if fn.Recv != nil && len(fn.Recv.List) > 0 {
				got := exprToString(fn.Recv.List[0].Type)
				if got != "" {
					t.Errorf("exprToString(ArrayType) = %q, want empty string", got)
				}
			}
		}
	}
}
