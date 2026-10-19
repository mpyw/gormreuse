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
	"runtime"
	"slices"
	"sync"
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

	// One run gives the fixes of every fixture. A run per fixture repeated
	// the analysis of the package and of all its dependencies (#195).
	run, err := goldentest.Analyze(testdata, "gormreuse", gormreuse.Analyzer)
	if err != nil {
		t.Fatalf("Failed to analyze the fixtures: %v", err)
	}
	for _, filename := range fixtures {
		t.Run(filename, func(t *testing.T) {
			t.Parallel()
			srcPath := filepath.Join(srcDir, filename)

			stale, err := run.SyncFixture(srcPath, update)
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
// gormreuse fixtures on its own and type-checks the package. An editor applies
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

	// The unchanged files are parsed once and shared by every check. go/types
	// does not modify the syntax it checks, so the checks run in parallel.
	fset := token.NewFileSet()
	var unchanged []*ast.File
	for _, f := range pass.Files {
		parsed, err := parser.ParseFile(fset, pass.Fset.File(f.Pos()).Name(), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		unchanged = append(unchanged, parsed)
	}

	var wg sync.WaitGroup
	sem := make(chan struct{}, runtime.GOMAXPROCS(0))
	checked := 0
	for _, diag := range results[0].Diagnostics {
		if len(diag.SuggestedFixes) == 0 {
			continue
		}
		checked++
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			at := pass.Fset.Position(diag.Pos)
			files, err := eachFixApply(fset, pass.Fset, unchanged, diag.SuggestedFixes[0].TextEdits)
			if err != nil {
				t.Errorf("%s: the fix does not parse: %v", at, err)
				return
			}
			conf := types.Config{Importer: importer}
			if _, err := conf.Check("gormreuse", fset, files, nil); err != nil {
				t.Errorf("%s: this diagnostic's fix alone does not compile: %v", at, err)
			}
		})
	}
	wg.Wait()
	if checked == 0 {
		t.Fatal("no diagnostic in the fixtures has a fix")
	}
}

// eachFixApply returns the files of unchanged, parsed into fset, with edits
// applied. editFset holds the edits' positions. A fix may edit a file other
// than the diagnostic's, so each edit is applied to the file it targets.
func eachFixApply(fset, editFset *token.FileSet, unchanged []*ast.File, edits []analysis.TextEdit) ([]*ast.File, error) {
	edits = slices.Clone(edits)
	slices.SortFunc(edits, func(a, b analysis.TextEdit) int { return cmp.Compare(b.Pos, a.Pos) })
	fixedFiles := map[string][]byte{}
	for _, e := range edits {
		name := editFset.File(e.Pos).Name()
		fixed, ok := fixedFiles[name]
		if !ok {
			var err error
			if fixed, err = os.ReadFile(name); err != nil {
				return nil, err
			}
		}
		start, end := editFset.Position(e.Pos).Offset, editFset.Position(e.End).Offset
		fixedFiles[name] = slices.Concat(fixed[:start], e.NewText, fixed[end:])
	}

	files := slices.Clone(unchanged)
	for i, f := range files {
		fixed, ok := fixedFiles[fset.File(f.Pos()).Name()]
		if !ok {
			continue
		}
		parsed, err := parser.ParseFile(fset, fset.File(f.Pos()).Name(), fixed, 0)
		if err != nil {
			return nil, err
		}
		files[i] = parsed
	}
	return files, nil
}

// importerFunc imports a package by calling the function.
type importerFunc func(path string) (*types.Package, error)

func (f importerFunc) Import(path string) (*types.Package, error) { return f(path) }
