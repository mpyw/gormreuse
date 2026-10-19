package gormreuse_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestKnownLimitationsListed fails when a fixture function marked
// [LIMITATION] is not named in the Known Limitations section of
// design/implementation.md, so that list cannot drift from the fixtures.
func TestKnownLimitationsListed(t *testing.T) {
	doc, err := os.ReadFile("design/implementation.md")
	if err != nil {
		t.Fatal(err)
	}
	_, section, ok := strings.Cut(string(doc), "\n## Known Limitations\n")
	if !ok {
		t.Fatal("design/implementation.md has no ## Known Limitations section")
	}
	section, _, _ = strings.Cut(section, "\n## ")

	marked := markedLimitationFuncs(t, filepath.Join("testdata", "src"))
	if len(marked) == 0 {
		t.Fatal("found no [LIMITATION] markers in testdata/src")
	}
	for name, file := range marked {
		if !strings.Contains(section, "`"+name+"`") {
			t.Errorf("%s: %s is marked [LIMITATION] but not listed in the Known Limitations section of design/implementation.md", file, name)
		}
	}
}

// markedLimitationFuncs returns each function under root whose doc comment
// or body holds a [LIMITATION] comment, mapped to its file. A marker outside
// any function belongs to the next function in the file.
func markedLimitationFuncs(t *testing.T, root string) map[string]string {
	t.Helper()
	marked := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return err
		}
		for _, cg := range f.Comments {
			if !strings.Contains(cg.Text(), "[LIMITATION]") {
				continue
			}
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if ok && cg.Pos() < fn.End() {
					marked[fn.Name.Name] = path
					break
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return marked
}
