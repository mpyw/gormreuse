package ssa

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"testing"

	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

// TestPositionedViolation checks that a violation without a position falls
// back to its call, then to the analyzed function (#255).
func TestPositionedViolation(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "p.go", "package p\n\nfunc f() { g() }\n\nfunc g() {}\n", 0)
	if err != nil {
		t.Fatal(err)
	}
	pkg, _, err := ssautil.BuildPackage(&types.Config{Importer: importer.Default()}, fset, types.NewPackage("p", ""), []*ast.File{file}, 0)
	if err != nil {
		t.Fatal(err)
	}
	fn := pkg.Func("f")
	var call *ssa.Call
	for _, instr := range fn.Blocks[0].Instrs {
		if c, ok := instr.(*ssa.Call); ok {
			call = c
		}
	}
	if call == nil || !call.Pos().IsValid() || !fn.Pos().IsValid() {
		t.Fatal("fixture has no positioned call")
	}
	own := token.Pos(1)
	tests := []struct {
		name string
		v    Violation
		want token.Pos
	}{
		{"own position", Violation{Pos: own, Call: call}, own},
		{"call", Violation{Call: call}, call.Pos()},
		{"function", Violation{}, fn.Pos()},
	}
	for _, tt := range tests {
		if got := positionedViolation(tt.v, fn); got != tt.want {
			t.Errorf("%s: positionedViolation = %v, want %v", tt.name, got, tt.want)
		}
	}
}
