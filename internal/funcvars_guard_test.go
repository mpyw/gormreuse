package internal

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoSiteWithoutFuncVarsByAccident keeps every call-site read resolving
// calls through package-level function variables (#183, #293). It fails on a
// call to SiteOf or EachSite with a nil FuncVars literal in non-test code
// under internal/. A read that cannot reach the FuncVars says so by calling
// callsite.SiteOfWithoutFuncVars, which is the only function allowed below.
func TestNoSiteWithoutFuncVarsByAccident(t *testing.T) {
	t.Parallel()
	allowed := map[string]bool{
		"ssa/callsite/site.go:SiteOfWithoutFuncVars": true,
	}
	fset := token.NewFileSet()
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		for _, decl := range file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || allowed[filepath.ToSlash(path)+":"+fd.Name.Name] {
				continue
			}
			ast.Inspect(fd, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) < 2 {
					return true
				}
				var name string
				switch fun := call.Fun.(type) {
				case *ast.Ident:
					name = fun.Name
				case *ast.SelectorExpr:
					name = fun.Sel.Name
				}
				if arg, ok := call.Args[1].(*ast.Ident); !ok || arg.Name != "nil" || name != "SiteOf" && name != "EachSite" {
					return true
				}
				t.Errorf("%s: %s in %s gets nil FuncVars; pass the package's FuncVars or call SiteOfWithoutFuncVars",
					fset.PositionFor(call.Pos(), false), name, fd.Name.Name)
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
