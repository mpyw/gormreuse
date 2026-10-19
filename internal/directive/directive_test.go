package directive

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"testing"

	"golang.org/x/tools/go/ast/inspector"
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

			if got := isIgnoreDirective(tt.text); got != tt.expected {
				t.Errorf("isIgnoreDirective(%q) = %v, want %v", tt.text, got, tt.expected)
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

			if got := isPureDirective(tt.text); got != tt.expected {
				t.Errorf("isPureDirective(%q) = %v, want %v", tt.text, got, tt.expected)
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
			if got := isImmutableParamDirective(tt.text); got != tt.expected {
				t.Errorf("isImmutableParamDirective(%q) = %v, want %v", tt.text, got, tt.expected)
			}
		})
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

	cur, _ := inspector.New([]*ast.File{file}).Root().FirstChild()
	set := BuildPureFuncSet(cur, "test/pkg")
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

func TestParseDirective(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		text string
		want []string
	}{
		{"canonical", "//gormreuse:pure", []string{"pure"}},
		{"comma list", "//gormreuse:pure,immutable-return", []string{"pure", "immutable-return"}},
		{"space after comma", "//gormreuse:pure, immutable-return", nil},
		{"space before comma", "//gormreuse:pure ,immutable-return", nil},
		{"trailing comma", "//gormreuse:pure,", nil},
		{"leading comma", "//gormreuse:,pure", nil},
		{"empty part", "//gormreuse:pure,,immutable-return", nil},
		{"words after the name", "//gormreuse:ignore reason here", nil},
		{"dash reason", "//gormreuse:ignore - reason", nil},
		{"unknown name", "//gormreuse:ignor", nil},
		{"unknown part of a list", "//gormreuse:ignore,ignor", nil},
		{"unknown part of a pure list", "//gormreuse:pure,imutable-return", nil},
		{"unclosed immutable-input", "//gormreuse:immutable-input(fn", nil},
		{"reason without space after slashes", "//gormreuse:ignore //reason", []string{"ignore"}},
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
		{"pure in spaced comma list", "//gormreuse:pure, immutable-return", isPureDirective, false},
		{"immutable-return in spaced comma list", "//gormreuse:pure, immutable-return", isImmutableReturnDirective, false},
		{"pure next to immutable-input", "//gormreuse:immutable-input(fn),pure // reason", isPureDirective, true},
		{"ignore with block form", "/*gormreuse:ignore*/", isIgnoreDirective, false},
		{"ignore with space after marker", "// gormreuse:ignore", isIgnoreDirective, false},
		{"name in trailing reason does not count", "//gormreuse:ignore // not pure", isPureDirective, false},
		{"lookalike tool", "//gormreusex:pure", isPureDirective, false},
		{"name prefix does not count", "//gormreuse:pure-ish", isPureDirective, false},
		{"pure next to a typo", "//gormreuse:pure,imutable-return", isPureDirective, false},
		{"ignore next to a typo", "//gormreuse:ignore,ignor", isIgnoreDirective, false},
		{"immutable-return next to a typo", "//gormreuse:immutable-return,pur", isImmutableReturnDirective, false},
		{"immutable-param next to a typo", "//gormreuse:immutable-param,pur", isImmutableParamDirective, false},
		{"ignore with dash reason", "//gormreuse:ignore - reason", isIgnoreDirective, false},
		{"ignore with reason without space", "//gormreuse:ignore//reason", isIgnoreDirective, true},
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

func TestReadDirective(t *testing.T) {
	t.Parallel()

	const (
		malformed = malformedDirectiveMessage
		ignoreArg = "gormreuse:ignore takes no argument; write a reason after //"
		unknown   = " (want ignore, pure, immutable-return, immutable-param or immutable-input(name))"
		inputFmt  = " directive: write it as immutable-input(name)"
	)
	tests := []struct {
		name    string
		text    string
		parts   []string
		problem string
	}{
		// Every name gormreuse reads.
		{"ignore", "//gormreuse:ignore", []string{"ignore"}, ""},
		{"pure", "//gormreuse:pure", []string{"pure"}, ""},
		{"immutable-return", "//gormreuse:immutable-return", []string{"immutable-return"}, ""},
		{"immutable-param", "//gormreuse:immutable-param", []string{"immutable-param"}, ""},
		{"immutable-input", "//gormreuse:immutable-input(cb)", []string{"immutable-input(cb)"}, ""},
		{"comma list", "//gormreuse:pure,immutable-return", []string{"pure", "immutable-return"}, ""},
		{"list with immutable-input", "//gormreuse:immutable-input(a),immutable-input(b),pure", []string{"immutable-input(a)", "immutable-input(b)", "pure"}, ""},
		{"trailing whitespace", "//gormreuse:ignore \t", []string{"ignore"}, ""},

		// A reason goes after "//".
		{"reason", "//gormreuse:ignore // reason", []string{"ignore"}, ""},
		{"reason without space after slashes", "//gormreuse:ignore //reason", []string{"ignore"}, ""},
		{"reason without space before slashes", "//gormreuse:ignore//reason", []string{"ignore"}, ""},
		{"want on the line", "//gormreuse:ignore // want `x`", []string{"ignore"}, ""},
		{"reason after a list", "//gormreuse:pure,immutable-return // note", []string{"pure", "immutable-return"}, ""},
		{"reason after immutable-input", "//gormreuse:immutable-input(cb) // note", []string{"immutable-input(cb)"}, ""},
		{"directive only in the reason", "//gormreuse:ignore // gormreuse:pure", []string{"ignore"}, ""},

		// Text after the name that is not a "//" reason.
		{"dash reason", "//gormreuse:ignore - reason", nil, ignoreArg},
		{"words", "//gormreuse:ignore intentional reuse", nil, ignoreArg},
		{"dash reason then want", "//gormreuse:ignore - reason // want `x`", nil, ignoreArg},
		{"dash reason on a list", "//gormreuse:pure,immutable-return - note", nil, "gormreuse:pure,immutable-return takes no argument; write a reason after //"},
		{"space before comma", "//gormreuse:pure ,immutable-return", nil, "gormreuse:pure takes no argument; write a reason after //"},

		// Unknown names, alone or in a list.
		{"misspelled ignore", "//gormreuse:ignor", nil, "unknown directive gormreuse:ignor" + unknown},
		{"misspelled pure", "//gormreuse:pur", nil, "unknown directive gormreuse:pur" + unknown},
		{"misspelled immutable-input", "//gormreuse:imutable-input(cb)", nil, "unknown directive gormreuse:imutable-input(cb)" + unknown},
		{"immutable-input without parentheses", "//gormreuse:immutable-input", nil, "unknown directive gormreuse:immutable-input" + unknown},
		{"name prefix", "//gormreuse:pure-ish", nil, "unknown directive gormreuse:pure-ish" + unknown},
		{"typo in an ignore list", "//gormreuse:ignore,ignor", nil, "unknown directive gormreuse:ignor" + unknown},
		{"typo in a pure list", "//gormreuse:pure,imutable-return", nil, "unknown directive gormreuse:imutable-return" + unknown},
		{"typo first in a list", "//gormreuse:immutable-return,pur", nil, "unknown directive gormreuse:pur" + unknown},
		{"unknown name with words", "//gormreuse:ignor intentional", nil, "unknown directive gormreuse:ignor" + unknown},
		{"unknown name with reason", "//gormreuse:ignor // reason", nil, "unknown directive gormreuse:ignor" + unknown},

		// Malformed immutable-input(name).
		{"unclosed immutable-input", "//gormreuse:immutable-input(cb", nil, "malformed gormreuse:immutable-input(cb" + inputFmt},
		{"empty immutable-input", "//gormreuse:immutable-input()", nil, "malformed gormreuse:immutable-input()" + inputFmt},
		{"space inside immutable-input", "//gormreuse:immutable-input( cb )", nil, "malformed gormreuse:immutable-input(" + inputFmt},
		{"unclosed immutable-input in a list", "//gormreuse:pure,immutable-input(cb", nil, "malformed gormreuse:immutable-input(cb" + inputFmt},

		// Not in the directive form.
		{"space after marker", "// gormreuse:pure", nil, malformed},
		{"space after colon", "//gormreuse: pure", nil, malformed},
		{"block form", "/*gormreuse:pure*/", nil, malformed},
		{"uppercase name", "//gormreuse:Pure", nil, malformed},
		{"no name", "//gormreuse:", nil, malformed},
		{"no name with reason", "//gormreuse: // reason", nil, malformed},
		{"space after comma", "//gormreuse:pure, immutable-return", nil, malformed},
		{"trailing comma", "//gormreuse:pure,", nil, malformed},
		{"empty part", "//gormreuse:pure,,immutable-return", nil, malformed},

		// Not addressed to gormreuse.
		{"lookalike tool", "//gormreusex:pure", nil, ""},
		{"lookalike tool with space", "// gormreusex:pure", nil, ""},
		{"other tool", "//nolint:gormreuse", nil, ""},
		{"directive after another tool", "//nolint:foo //gormreuse:ignore", nil, ""},
		{"prose", "// see gormreuse:pure for details", nil, ""},
		{"random comment", "// some comment", nil, ""},
		{"empty", "//", nil, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			parts, problem := readDirective(tt.text)
			if !slices.Equal(parts, tt.parts) || problem != tt.problem {
				t.Errorf("readDirective(%q) = %q, %q; want %q, %q", tt.text, parts, problem, tt.parts, tt.problem)
			}
		})
	}
}

func TestFindDirectiveProblems(t *testing.T) {
	t.Parallel()

	src := `package test

// gormreuse:pure
func a() {}

//gormreuse:pure
func b() {}

//gormreuse:pure,imutable-return
func c() {
	_ = 1 /*gormreuse:ignore*/
	_ = 2 //gormreuse:ignore - reason
	_ = 3 //gormreuse:ignore // reason
}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "test.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("Failed to parse: %v", err)
	}

	var got []string
	for _, p := range FindDirectiveProblems(file) {
		got = append(got, fmt.Sprintf("%d: %s", fset.Position(p.Pos).Line, p.Message))
	}
	want := []string{
		"3: " + malformedDirectiveMessage,
		"9: unknown directive gormreuse:imutable-return (want ignore, pure, immutable-return, immutable-param or immutable-input(name))",
		"11: " + malformedDirectiveMessage,
		"12: gormreuse:ignore takes no argument; write a reason after //",
	}
	if !slices.Equal(got, want) {
		t.Errorf("FindDirectiveProblems = %q, want %q", got, want)
	}
}
