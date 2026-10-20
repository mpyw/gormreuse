package internal

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/ast/inspector"
	"golang.org/x/tools/go/ssa"
	xssautil "golang.org/x/tools/go/ssa/ssautil"

	"github.com/mpyw/gormreuse/internal/directive"
	ssautil "github.com/mpyw/gormreuse/internal/ssa"
	"github.com/mpyw/gormreuse/internal/ssa/callsite"
)

// TestPanicGuard verifies that a panic in per-function analysis is contained
// (issue #77 item 3) so one pathological function cannot abort the whole vet
// run, that it is reported once at the function instead of dropped silently
// (issue #186), and that GORMREUSE_DEBUG_PANIC re-surfaces it.
func TestPanicGuard(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "p.go", "package p\n\nfunc F() {}\n", 0)
	if err != nil {
		t.Fatal(err)
	}
	pkg, _, err := xssautil.BuildPackage(&types.Config{}, fset, types.NewPackage("example.com/p", "p"), []*ast.File{file}, 0)
	if err != nil {
		t.Fatal(err)
	}
	fn := pkg.Func("F")

	var got []analysis.Diagnostic
	pass := &analysis.Pass{Report: func(d analysis.Diagnostic) { got = append(got, d) }}
	guard := &panicGuard{pass: pass, reported: make(map[*ssa.Function]bool)}

	// Default: the panic is reported, and execution continues.
	ran := false
	guard.run(fn, func() { panic("boom") })
	guard.run(fn, func() { panic("boom again") })
	guard.run(fn, func() { ran = true })
	if !ran {
		t.Fatal("panicGuard did not run work after a prior panic")
	}
	want := "gormreuse: internal error analyzing F: boom; please report this at https://github.com/mpyw/gormreuse/issues"
	if len(got) != 1 || got[0].Message != want || got[0].Pos != fn.Pos() {
		t.Fatalf("got %+v, want one diagnostic at %v: %q", got, fn.Pos(), want)
	}

	// With GORMREUSE_DEBUG_PANIC set, the panic is re-surfaced.
	t.Setenv("GORMREUSE_DEBUG_PANIC", "1")
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Error("expected re-panic under GORMREUSE_DEBUG_PANIC, got none")
			}
		}()
		guard.run(fn, func() { panic("boom") })
	}()
}

// TestReportUnusedIgnoresFix verifies that an unused ignore gets a fix that
// deletes it, except after an internal error, which may have dropped the
// diagnostic the ignore covers, and in a file other than a test file of a
// package with test files, whose test variant may need the ignore (#248).
func TestReportUnusedIgnoresFix(t *testing.T) {
	const src = "package p\n\nfunc F() {\n\t_ = 1 //gormreuse:ignore\n}\n"
	tests := []struct {
		name          string
		filename      string // In a fresh directory
		sibling       string // Another file of the directory, or ""
		internalError bool
		wantFix       bool
	}{
		{"no test files", "p.go", "", false, true},
		{"internal error", "p.go", "", true, false},
		{"test file beside it", "p.go", "p_test.go", false, false},
		{"in a test file", "p_test.go", "", false, true},
		{"unreadable directory", "missing/p.go", "", false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.sibling != "" {
				if err := os.WriteFile(filepath.Join(dir, tc.sibling), []byte("package p\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, filepath.Join(dir, tc.filename), src, parser.ParseComments)
			if err != nil {
				t.Fatal(err)
			}
			ignores := directive.NewIgnores(fset)
			for cur := range inspector.New([]*ast.File{file}).Root().Children() {
				ignores.AddFile(cur)
			}
			var got []analysis.Diagnostic
			pass := &analysis.Pass{
				Fset:     fset,
				Files:    []*ast.File{file},
				Report:   func(d analysis.Diagnostic) { got = append(got, d) },
				ReadFile: func(string) ([]byte, error) { return []byte(src), nil },
			}
			removals := directive.NewRemovals(pass)
			reportUnusedIgnores(removals, ignores, reportIgnoreRemovable(pass, tc.internalError))
			removals.Flush()
			if len(got) != 1 || got[0].Message != "unused gormreuse:ignore directive" {
				t.Fatalf("got %+v, want one unused-ignore diagnostic", got)
			}
			if hasFix := len(got[0].SuggestedFixes) > 0; hasFix != tc.wantFix {
				t.Errorf("diagnostic has a fix: %v, want %v", hasFix, tc.wantFix)
			}
		})
	}
}

// =============================================================================
// Analyzer Tests
// =============================================================================

func TestNewAnalyzer(t *testing.T) {
	t.Parallel()

	pureFuncs := directive.NewPureFuncSet(nil, nil)
	pureFuncs.Add(directive.FuncKey{PkgPath: "test", FuncName: "Pure"})
	immutableReturnFuncs := directive.NewImmutableReturnFuncSet(nil, nil)
	analyzer := ssautil.NewAnalyzer(nil, pureFuncs, immutableReturnFuncs, nil, nil, nil, nil, nil, callsite.FuncVars{}, nil)

	if analyzer == nil {
		t.Error("Expected analyzer to be initialized")
	}
}

func TestNewChecker(t *testing.T) {
	t.Parallel()

	ignores := directive.NewIgnores(nil)
	pureFuncs := directive.NewPureFuncSet(nil, nil)
	immutableReturnFuncs := directive.NewImmutableReturnFuncSet(nil, nil)
	reported := make(map[reportKey]bool)

	chk := newChecker(nil, ignores, pureFuncs, immutableReturnFuncs, nil, nil, nil, nil, nil, callsite.FuncVars{}, reported, nil, nil)

	if chk == nil {
		t.Error("Expected checker to be initialized")
	}
}

func TestAnalyzer_Analyze_NilFunction(t *testing.T) {
	t.Parallel()

	analyzer := ssautil.NewAnalyzer(nil, nil, nil, nil, nil, nil, nil, nil, callsite.FuncVars{}, nil)

	// Should not panic with nil function
	violations := analyzer.Analyze()
	if len(violations) != 0 {
		t.Errorf("Expected 0 violations for nil function, got %d", len(violations))
	}
}

func TestAnalyzer_Analyze_EmptyFunction(t *testing.T) {
	t.Parallel()

	fn := &ssa.Function{}
	analyzer := ssautil.NewAnalyzer(fn, nil, nil, nil, nil, nil, nil, nil, callsite.FuncVars{}, nil)

	violations := analyzer.Analyze()
	if len(violations) != 0 {
		t.Errorf("Expected 0 violations for empty function, got %d", len(violations))
	}
}
