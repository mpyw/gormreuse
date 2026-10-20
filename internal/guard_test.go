package internal

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"testing"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/ssa"
)

// InjectGuardPanic makes the work of each stage of panics panic, for the
// function it maps the stage to, or for the work that reads the whole package
// when that is "". restore removes the panics. It is for TestPanicInjection in
// guard_injection_test.go, which must not run in parallel with another
// analysis.
func InjectGuardPanic(panics map[string]string) (restore func()) {
	guardPanicInjection = func(s string, f *ssa.Function) {
		fn, ok := panics[s]
		if !ok {
			return
		}
		if f == nil && fn == "" || f != nil && f.Name() == fn {
			panic("injected")
		}
	}
	return func() { guardPanicInjection = nil }
}

// TestPanicGuardRunPackage checks that a panic in the work that reads the
// whole package is reported once at the package clause, that the guard then
// reports it panicked, and that GORMREUSE_DEBUG_PANIC re-surfaces it (#315).
func TestPanicGuardRunPackage(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "p.go", "package p\n", 0)
	if err != nil {
		t.Fatal(err)
	}
	var got []analysis.Diagnostic
	pass := &analysis.Pass{
		Files:  []*ast.File{file},
		Pkg:    types.NewPackage("example.com/p", "p"),
		Report: func(d analysis.Diagnostic) { got = append(got, d) },
	}
	guard := &panicGuard{pass: pass, reported: make(map[*ssa.Function]bool)}

	if !guard.runPackage("collect", func() {}) || guard.panicked() {
		t.Fatal("runPackage reported a panic for work that finished")
	}
	if guard.runPackage("collect", func() { panic("boom") }) {
		t.Fatal("runPackage = true for work that panicked, want false")
	}
	want := "gormreuse: internal error in collect of package example.com/p: boom; please report this at https://github.com/mpyw/gormreuse/issues"
	if len(got) != 1 || got[0].Message != want || got[0].Pos != file.Package {
		t.Fatalf("got %+v, want one diagnostic at %v: %q", got, file.Package, want)
	}
	if !guard.panicked() {
		t.Error("panicked = false after a panic, want true")
	}

	t.Setenv("GORMREUSE_DEBUG_PANIC", "1")
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Error("expected re-panic under GORMREUSE_DEBUG_PANIC, got none")
			}
		}()
		guard.runPackage("collect", func() { panic("boom") })
	}()
}
