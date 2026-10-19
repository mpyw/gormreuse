package internal

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"testing"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/ssa"
	xssautil "golang.org/x/tools/go/ssa/ssautil"

	"github.com/mpyw/gormreuse/internal/directive"
	ssautil "github.com/mpyw/gormreuse/internal/ssa"
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

// =============================================================================
// Analyzer Tests
// =============================================================================

func TestNewAnalyzer(t *testing.T) {
	t.Parallel()

	pureFuncs := directive.NewPureFuncSet(nil, nil)
	pureFuncs.Add(directive.FuncKey{PkgPath: "test", FuncName: "Pure"})
	immutableReturnFuncs := directive.NewImmutableReturnFuncSet(nil, nil)
	analyzer := ssautil.NewAnalyzer(nil, pureFuncs, immutableReturnFuncs, nil, nil, nil, nil, nil, nil)

	if analyzer == nil {
		t.Error("Expected analyzer to be initialized")
	}
}

func TestNewChecker(t *testing.T) {
	t.Parallel()

	ignores := directive.NewIgnores(nil)
	pureFuncs := directive.NewPureFuncSet(nil, nil)
	immutableReturnFuncs := directive.NewImmutableReturnFuncSet(nil, nil)
	reported := make(map[token.Pos]bool)
	suggestedEdits := make(map[editKey]bool)

	chk := newChecker(nil, ignores, pureFuncs, immutableReturnFuncs, nil, nil, nil, nil, nil, nil, reported, suggestedEdits, nil)

	if chk == nil {
		t.Error("Expected checker to be initialized")
	}
}

func TestAnalyzer_Analyze_NilFunction(t *testing.T) {
	t.Parallel()

	analyzer := ssautil.NewAnalyzer(nil, nil, nil, nil, nil, nil, nil, nil, nil)

	// Should not panic with nil function
	violations := analyzer.Analyze()
	if len(violations) != 0 {
		t.Errorf("Expected 0 violations for nil function, got %d", len(violations))
	}
}

func TestAnalyzer_Analyze_EmptyFunction(t *testing.T) {
	t.Parallel()

	fn := &ssa.Function{}
	analyzer := ssautil.NewAnalyzer(fn, nil, nil, nil, nil, nil, nil, nil, nil)

	violations := analyzer.Analyze()
	if len(violations) != 0 {
		t.Errorf("Expected 0 violations for empty function, got %d", len(violations))
	}
}
