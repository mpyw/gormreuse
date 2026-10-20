package callsite

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

// TestFreeVarBindingOfDeadClosure: a closure created only after a return has
// a body, but SSA removes the block of its MakeClosure, so its FreeVar has no
// binding.
func TestFreeVarBindingOfDeadClosure(t *testing.T) {
	t.Parallel()
	const src = `package p

func dead(x int) int {
	return x
	_ = func() int { return x }
	return 0
}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "p.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	pkg, _, err := ssautil.BuildPackage(&types.Config{Importer: importer.Default()}, fset,
		types.NewPackage("p", "p"), []*ast.File{file}, ssa.SanityCheckFunctions)
	if err != nil {
		t.Fatal(err)
	}
	anon := pkg.Func("dead").AnonFuncs
	if len(anon) != 1 || len(anon[0].FreeVars) != 1 {
		t.Fatalf("want one closure with one FreeVar, got %v", anon)
	}
	if mc := FreeVarMakeClosure(anon[0]); mc != nil {
		t.Errorf("FreeVarMakeClosure = %v, want nil", mc)
	}
	if b := FreeVarBinding(anon[0].FreeVars[0]); b != nil {
		t.Errorf("FreeVarBinding = %v, want nil", b)
	}
}
