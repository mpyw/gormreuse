package directive

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strings"
	"testing"

	"golang.org/x/tools/go/ast/inspector"
)

func TestIgnoreMapLineIgnore(t *testing.T) {
	t.Parallel()

	t.Run("same line", func(t *testing.T) {
		t.Parallel()

		m := make(ignoreMap)
		m[10] = &ignoreEntry{pos: token.Pos(100), used: false}

		if m.lineIgnore(10) == nil {
			t.Error("lineIgnore(10) should find the entry (same line)")
		}
	})

	t.Run("next line", func(t *testing.T) {
		t.Parallel()

		m := make(ignoreMap)
		m[20] = &ignoreEntry{pos: token.Pos(200), nextLine: true}

		if m.lineIgnore(21) == nil {
			t.Error("lineIgnore(21) should find the entry (previous line has ignore)")
		}
	})

	t.Run("trailing comment does not cover next line", func(t *testing.T) {
		t.Parallel()

		m := make(ignoreMap)
		m[20] = &ignoreEntry{pos: token.Pos(200)}

		if m.lineIgnore(21) != nil {
			t.Error("lineIgnore(21) should be nil (trailing ignore on line 20)")
		}
	})

	t.Run("non-ignored line", func(t *testing.T) {
		t.Parallel()

		m := make(ignoreMap)
		m[10] = &ignoreEntry{pos: token.Pos(100), used: false}

		if m.lineIgnore(5) != nil {
			t.Error("lineIgnore(5) should be nil")
		}
	})
}

func TestIgnoreMapunusedIgnores(t *testing.T) {
	t.Parallel()

	m := make(ignoreMap)
	m[10] = &ignoreEntry{pos: token.Pos(100), used: false}
	m[20] = &ignoreEntry{pos: token.Pos(200), used: true}
	m[-1] = &ignoreEntry{pos: token.Pos(1), used: true} // File-level ignore

	unused := m.unusedIgnores()
	if len(unused) != 1 {
		t.Errorf("Expected 1 unused ignore, got %d", len(unused))
	}
	if len(unused) > 0 && unused[0] != token.Pos(100) {
		t.Errorf("Expected pos 100, got %v", unused[0])
	}
}

// newIgnoresForTest parses src as test.go and returns its ignore set with the
// file set, so a test can look up positions by line.
func newIgnoresForTest(t *testing.T, src string) (*Ignores, *token.File) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "test.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("Failed to parse: %v", err)
	}
	ig := NewIgnores(fset)
	cur, _ := inspector.New([]*ast.File{file}).Root().FirstChild()
	ig.AddFile(cur)
	return ig, fset.File(file.Pos())
}

// TestIgnoresScope checks the scope rule of #190: a line ignore covers its
// statement, a function ignore covers the declaration and its closures.
func TestIgnoresScope(t *testing.T) {
	t.Parallel()

	src := `package test

func f(a int) {
	//gormreuse:ignore
	_ = a +
		a
	_ = a +
		a //gormreuse:ignore
	if a > 0 &&
		a < 9 { //gormreuse:ignore
		_ = a
	}
	_ = func() int {
		return a //gormreuse:ignore
	}() +
		a
}

//gormreuse:ignore
func g(a int) {
	_ = func() int {
		return a
	}
}

//gormreuse:ignore
func unused() {}
`
	ig, tf := newIgnoresForTest(t, src)
	lines := strings.Split(src, "\n")
	at := func(line int) token.Pos { // First non-tab character of the line
		return tf.LineStart(line) + token.Pos(len(lines[line-1])-len(strings.TrimLeft(lines[line-1], "\t")))
	}

	tests := []struct {
		line int
		want bool
		why  string
	}{
		{6, true, "second line of a statement with an ignore above it"},
		{7, true, "first line of a statement with a trailing ignore on its last line"},
		{9, true, "if condition with a trailing ignore after the brace"},
		{11, false, "body of an if whose header has the ignore"},
		{16, false, "statement whose only ignore is inside a nested closure"},
		{22, true, "closure inside a function with a function-level ignore"},
		{20, true, "the declaration line of an ignored function"},
		{3, false, "a line no ignore covers"},
	}
	// Covers answers as Ignore does and marks nothing used (#312).
	for _, tt := range tests {
		if got := ig.Covers(at(tt.line)); got != tt.want {
			t.Errorf("line %d (%s): Covers = %v, want %v", tt.line, tt.why, got, tt.want)
		}
	}
	if unused := ig.UnusedIgnores(); len(unused) != 6 {
		t.Errorf("Covers marked ignores used: %d unused, want all 6", len(unused))
	}

	for _, tt := range tests {
		if got := ig.Ignore(at(tt.line)); got != tt.want {
			t.Errorf("line %d (%s): Ignore = %v, want %v", tt.line, tt.why, got, tt.want)
		}
	}

	var unusedLines []int
	for _, pos := range ig.UnusedIgnores() {
		unusedLines = append(unusedLines, tf.Line(pos))
	}
	if want := []int{14, 26}; !slices.Equal(unusedLines, want) {
		t.Errorf("unused ignores on lines %v, want %v", unusedLines, want)
	}
}

func TestIgnoresFileLevel(t *testing.T) {
	t.Parallel()

	src := `//gormreuse:ignore
// Package test is a test package.
package test

func foo() {}
`
	ig, tf := newIgnoresForTest(t, src)
	if !ig.Ignore(tf.LineStart(5)) {
		t.Error("Expected file-level ignore to cover line 5")
	}
	if unused := ig.UnusedIgnores(); len(unused) != 0 {
		t.Errorf("file-level ignore must never be unused, got %v", unused)
	}
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

	m := buildIgnoreMap(fset, file)
	if len(m) == 0 {
		t.Error("Expected non-empty ignore map")
	}
}

func TestBuildIgnoreMapNextLine(t *testing.T) {
	t.Parallel()

	src := `package test

func foo(a, b int) {
	//gormreuse:ignore
	_ = a
	_ = b //gormreuse:ignore
	_ = a
	if a > b { //gormreuse:ignore
		_ = b
	}
	_ = []int{
		//gormreuse:ignore
		a,
		b, //gormreuse:ignore
		a,
	}
}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "test.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("Failed to parse: %v", err)
	}

	m := buildIgnoreMap(fset, file)
	want := map[int]bool{4: true, 6: false, 8: false, 12: true, 14: false}
	for line, nextLine := range want {
		entry, ok := m[line]
		if !ok {
			t.Errorf("line %d: no ignore entry", line)
			continue
		}
		if entry.nextLine != nextLine {
			t.Errorf("line %d: nextLine = %v, want %v", line, entry.nextLine, nextLine)
		}
	}
}
