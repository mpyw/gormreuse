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

// TestNoAdjustedPositions keeps every internal position unadjusted by //line
// directives (#151, #152, #153, #244). It fails on a call to a Position
// method, or to PositionFor with any second argument but false, in non-test
// code under internal/. Only the functions below may use adjusted positions:
// their text goes into messages next to the position the driver prints.
func TestNoAdjustedPositions(t *testing.T) {
	t.Parallel()
	allowed := map[string]bool{
		"ssa/pollution/tracker.go:loc": true,
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
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				switch sel.Sel.Name {
				case "Position":
				case "PositionFor":
					if arg, ok := call.Args[len(call.Args)-1].(*ast.Ident); ok && arg.Name == "false" {
						return true
					}
				default:
					return true
				}
				t.Errorf("%s: %s in %s applies //line directives; use PositionFor(pos, false)",
					fset.PositionFor(call.Pos(), false), sel.Sel.Name, fd.Name.Name)
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
