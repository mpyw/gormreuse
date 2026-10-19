package cfg

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

// buildFunc compiles src (a complete Go source file) to SSA and returns the
// named function. It uses plain Go — the cfg package operates purely on SSA
// control flow, so no gorm types are required.
//
//declscope:shared // the tests of every file of the package build their functions with it
func buildFunc(t *testing.T, src, name string) *ssa.Function {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "src.go", src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	pkg := types.NewPackage("p", "p")
	ssaPkg, _, err := ssautil.BuildPackage(
		&types.Config{Importer: importer.Default()},
		fset, pkg, []*ast.File{f}, ssa.BuilderMode(0),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	fn := ssaPkg.Func(name)
	if fn == nil {
		t.Fatalf("function %q not found", name)
	}
	return fn
}
