package gormreuse_test

import (
	"bytes"
	"cmp"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/analysistest"
	"golang.org/x/tools/go/packages"

	"github.com/mpyw/gormreuse"
	"github.com/mpyw/gormreuse/internal/goldentest"
)

// TestGoldenFiles checks every committed .golden and .diff fixture under
// testdata/src/gormreuse against what the analyzer's suggested fixes produce
// now. A plain run never writes. Set GORMREUSE_UPDATE_GOLDEN=1 to regenerate
// the fixtures and delete orphaned ones, then review and commit the result
// (#203).
func TestGoldenFiles(t *testing.T) {
	testdata := analysistest.TestData()
	srcDir := filepath.Join(testdata, "src", "gormreuse")
	update := goldentest.Updating()

	fixtures, err := goldentest.Fixtures(srcDir)
	if err != nil {
		t.Fatalf("Failed to list fixtures: %v", err)
	}
	if len(fixtures) == 0 {
		t.Fatalf("No fixtures found in %s", srcDir)
	}

	orphans, err := goldentest.SyncOrphans(srcDir, update)
	if err != nil {
		t.Fatalf("Failed to check orphaned fixtures: %v", err)
	}
	for _, path := range orphans {
		t.Errorf("%s has no source fixture.\nRun: %s\nThen review and commit the changes.", path, goldentest.UpdateCommand)
	}

	for _, filename := range fixtures {
		t.Run(filename, func(t *testing.T) {
			t.Parallel()
			srcPath := filepath.Join(srcDir, filename)

			stale, err := goldentest.SyncFixture(testdata, "gormreuse", srcPath, gormreuse.Analyzer, update)
			if err != nil {
				t.Fatalf("Failed to check fixtures for %s: %v", srcPath, err)
			}
			for _, path := range stale {
				t.Errorf("%s is missing or out of date.\nRun: %s\nThen review and commit the changes.", filepath.Base(path), goldentest.UpdateCommand)
			}
		})
	}
}

// TestGoldenFilesCompile type-checks the gormreuse fixture package with every
// fixture replaced by its .golden, so a suggested fix that rewrites the wrong
// variable into a type error (`t = q.Where(...)` inside t.Run, issue #198)
// fails here. TestGoldenFiles and RunWithSuggestedFixes only compare text.
func TestGoldenFilesCompile(t *testing.T) {
	t.Parallel()
	testdata := analysistest.TestData()
	srcDir := filepath.Join(testdata, "src", "gormreuse")

	fixtures, err := goldentest.Fixtures(srcDir)
	if err != nil {
		t.Fatalf("list fixtures: %v", err)
	}
	if len(fixtures) == 0 {
		t.Fatalf("no fixtures in %s", srcDir)
	}

	gopath := t.TempDir()
	if err := os.CopyFS(filepath.Join(gopath, "src"), os.DirFS(filepath.Join(testdata, "src"))); err != nil {
		t.Fatalf("copy testdata: %v", err)
	}
	pkgDir := filepath.Join(gopath, "src", "gormreuse")
	for _, name := range fixtures {
		golden, err := os.ReadFile(filepath.Join(srcDir, name+".golden"))
		if err != nil {
			t.Fatalf("read golden: %v", err)
		}
		if err := os.WriteFile(filepath.Join(pkgDir, name), golden, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
	}

	pkgs, err := packages.Load(&packages.Config{
		Mode: packages.NeedName | packages.NeedTypes | packages.NeedSyntax | packages.NeedTypesInfo,
		Dir:  pkgDir,
		Env:  append(os.Environ(), "GOPATH="+gopath, "GO111MODULE=off", "GOPROXY=off", "GOFLAGS="),
	}, "gormreuse")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		for _, e := range p.Errors {
			t.Errorf("golden output does not compile: %v", e)
		}
	})
}

// TestGoldenFilesE2E applies the suggested fixes to the E2E fixture package
// e2e/internal/fixsql/before and checks that e2e/internal/fixsql/after/after.go
// holds the result. The E2E tests run both packages against real GORM and
// compare the SQL they emit, so they check the fixes gormreuse suggests now
// (#199). Its name matches goldentest.UpdateCommand, which rewrites the file
// like the other fixtures.
func TestGoldenFilesE2E(t *testing.T) {
	t.Parallel()
	const header = "// Code generated from ../before/before.go by TestGoldenFilesE2E in the main module. DO NOT EDIT.\n\n"
	srcPath := filepath.Join("e2e", "internal", "fixsql", "before", "before.go")
	afterPath := filepath.Join("e2e", "internal", "fixsql", "after", "after.go")

	src, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	// The fixture imports gorm.io/gorm, which the analysis loads from the stub
	// in testdata.
	gopath := t.TempDir()
	if err := os.CopyFS(filepath.Join(gopath, "src", "gorm.io", "gorm"),
		os.DirFS(filepath.Join(analysistest.TestData(), "src", "gorm.io", "gorm"))); err != nil {
		t.Fatalf("copy gorm stub: %v", err)
	}
	pkgPath := filepath.Join(gopath, "src", "before", "before.go")
	if err := os.MkdirAll(filepath.Dir(pkgPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(pkgPath, src, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	_, fixed, err := goldentest.ApplyFixes(gopath, "before", pkgPath, gormreuse.Analyzer)
	if err != nil {
		t.Fatalf("apply fixes: %v", err)
	}
	if bytes.Equal(src, fixed) {
		t.Fatalf("the suggested fixes did not change %s", srcPath)
	}
	want := append([]byte(header), bytes.Replace(fixed, []byte("package before"), []byte("package after"), 1)...)

	if goldentest.Updating() {
		if err := os.WriteFile(afterPath, want, 0o644); err != nil {
			t.Fatalf("write %s: %v", afterPath, err)
		}
		return
	}
	got, err := os.ReadFile(afterPath)
	if err != nil || !bytes.Equal(got, want) {
		t.Errorf("%s is missing or out of date.\nRun: %s\nThen review and commit the changes.", afterPath, goldentest.UpdateCommand)
	}
}

// TestEachFixCompiles applies the suggested fix of each diagnostic in the
// fix_*.go fixtures on its own and type-checks the package. An editor applies
// one quick fix at a time, so a fix must not rely on an edit, such as the gorm
// import, that only another diagnostic's fix carries (#200).
// TestGoldenFilesCompile checks all fixes applied together.
func TestEachFixCompiles(t *testing.T) {
	t.Parallel()
	results := analysistest.Run(goldentest.NoopT{}, analysistest.TestData(), gormreuse.Analyzer, "gormreuse")
	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("analysis failed: %v", results)
	}
	pass := results[0].Pass
	importer := importerFunc(func(path string) (*types.Package, error) {
		for _, pkg := range pass.Pkg.Imports() {
			if pkg.Path() == path {
				return pkg, nil
			}
		}
		return nil, fmt.Errorf("%s is not imported by the fixtures", path)
	})

	checked := 0
	for _, diag := range results[0].Diagnostics {
		filename := pass.Fset.File(diag.Pos).Name()
		if !strings.HasPrefix(filepath.Base(filename), "fix_") || len(diag.SuggestedFixes) == 0 {
			continue
		}
		src, err := os.ReadFile(filename)
		if err != nil {
			t.Fatal(err)
		}
		edits := slices.Clone(diag.SuggestedFixes[0].TextEdits)
		slices.SortFunc(edits, func(a, b analysis.TextEdit) int { return cmp.Compare(b.Pos, a.Pos) })
		fixed := src
		for _, e := range edits {
			start, end := pass.Fset.Position(e.Pos).Offset, pass.Fset.Position(e.End).Offset
			fixed = slices.Concat(fixed[:start], e.NewText, fixed[end:])
		}

		fset := token.NewFileSet()
		var files []*ast.File
		for _, f := range pass.Files {
			name := pass.Fset.File(f.Pos()).Name()
			var content any
			if name == filename {
				content = fixed
			}
			parsed, err := parser.ParseFile(fset, name, content, 0)
			if err != nil {
				t.Fatalf("%s: the fix does not parse: %v", pass.Fset.Position(diag.Pos), err)
			}
			files = append(files, parsed)
		}
		conf := types.Config{Importer: importer}
		if _, err := conf.Check("gormreuse", fset, files, nil); err != nil {
			t.Errorf("%s: this diagnostic's fix alone does not compile: %v", pass.Fset.Position(diag.Pos), err)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no diagnostic in the fix_*.go fixtures has a fix")
	}
}

// importerFunc imports a package by calling the function.
type importerFunc func(path string) (*types.Package, error)

func (f importerFunc) Import(path string) (*types.Package, error) { return f(path) }
