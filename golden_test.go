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
	"strings"
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
// e2e/internal/fixsql/before and checks that each of its files other than
// doc.go has a counterpart in e2e/internal/fixsql/after (see
// e2eGeneratedName) holding the result. The E2E tests run both packages against real GORM and
// compare the SQL they emit, so they check the fixes gormreuse suggests now
// (#199). A topic's cases go in a file of their own, so fixtures added
// together do not conflict. Its name matches goldentest.UpdateCommand, which
// rewrites the files like the other fixtures, and deletes a generated file
// whose source is gone.
func TestGoldenFilesE2E(t *testing.T) {
	t.Parallel()
	beforeDir := filepath.Join("e2e", "internal", "fixsql", "before")
	afterDir := filepath.Join("e2e", "internal", "fixsql", "after")
	names, err := e2eFixtureNames(beforeDir)
	if err != nil {
		t.Fatalf("list fixtures: %v", err)
	}

	// The fixtures import gorm.io/gorm, which the analysis loads from the stub
	// in testdata. The package is analyzed once, with every file.
	gopath := t.TempDir()
	if err := os.CopyFS(filepath.Join(gopath, "src", "gorm.io", "gorm"),
		os.DirFS(filepath.Join(analysistest.TestData(), "src", "gorm.io", "gorm"))); err != nil {
		t.Fatalf("copy gorm stub: %v", err)
	}
	pkgDir := filepath.Join(gopath, "src", "before")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, name := range names {
		src, err := os.ReadFile(filepath.Join(beforeDir, name))
		if err != nil {
			t.Fatalf("read fixture: %v", err)
		}
		if err := os.WriteFile(filepath.Join(pkgDir, name), src, 0o644); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
	}
	run, err := goldentest.Analyze(gopath, "before", gormreuse.Analyzer)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}

	for _, name := range names {
		src, fixed, err := run.ApplyFixes(filepath.Join(pkgDir, name))
		if err != nil {
			t.Fatalf("apply fixes to %s: %v", name, err)
		}
		if bytes.Equal(src, fixed) {
			t.Errorf("the suggested fixes did not change %s", name)
		}
		header := "// Code generated from ../before/" + name + " by TestGoldenFilesE2E in the main module. DO NOT EDIT.\n\n"
		want := append([]byte(header), bytes.Replace(fixed, []byte("package before"), []byte("package after"), 1)...)
		afterPath := filepath.Join(afterDir, e2eGeneratedName(name))
		if goldentest.Updating() {
			if err := os.WriteFile(afterPath, want, 0o644); err != nil {
				t.Fatalf("write %s: %v", afterPath, err)
			}
			continue
		}
		got, err := os.ReadFile(afterPath)
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s is missing or out of date.\nRun: %s\nThen review and commit the changes.", afterPath, goldentest.UpdateCommand)
		}
	}

	generated, err := e2eFixtureNames(afterDir)
	if err != nil {
		t.Fatalf("list generated files: %v", err)
	}
	for _, name := range generated {
		if slices.ContainsFunc(names, func(src string) bool { return e2eGeneratedName(src) == name }) {
			continue
		}
		afterPath := filepath.Join(afterDir, name)
		if !goldentest.Updating() {
			t.Errorf("%s has no source in %s.\nRun: %s\nThen review and commit the changes.", afterPath, beforeDir, goldentest.UpdateCommand)
			continue
		}
		if err := os.Remove(afterPath); err != nil {
			t.Fatalf("remove %s: %v", afterPath, err)
		}
	}
}

// e2eGeneratedName is the name in fixsql/after of the file generated from
// the file name of fixsql/before: after.go for before.go, the same name for
// any other.
func e2eGeneratedName(name string) string {
	if name == "before.go" {
		return "after.go"
	}
	return name
}

// e2eFixtureNames returns the base names of the .go files in dir other than
// doc.go, which holds only the package comment.
func e2eFixtureNames(dir string) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, err
	}
	var names []string
	for _, m := range matches {
		if name := filepath.Base(m); name != "doc.go" && !strings.HasSuffix(name, "_test.go") {
			names = append(names, name)
		}
	}
	return names, nil
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

// TestEveryDiagnosticCarriesFix requires every diagnostic in
// fix_every_diagnostic.go to carry the fix of its root, the same fix on each.
// An editor offers a quick fix only on the diagnostic that carries it (#246).
// A root with an ignored diagnostic gets no fix (#312).
func TestEveryDiagnosticCarriesFix(t *testing.T) {
	t.Parallel()
	results := analysistest.Run(goldentest.NoopT{}, analysistest.TestData(), gormreuse.Analyzer, "gormreuse")
	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("analysis failed: %v", results)
	}
	fset := results[0].Pass.Fset
	fixes := map[string][]string{} // function name -> rendered fix of each diagnostic
	for _, diag := range results[0].Diagnostics {
		at := fset.Position(diag.Pos)
		if filepath.Base(at.Filename) != "fix_every_diagnostic.go" {
			continue
		}
		if len(diag.SuggestedFixes) == 0 {
			t.Errorf("%s: the diagnostic has no fix", at)
			continue
		}
		var b strings.Builder
		for _, e := range diag.SuggestedFixes[0].TextEdits {
			fmt.Fprintf(&b, "%d-%d %q;", e.Pos, e.End, e.NewText)
		}
		fn := enclosingFuncName(results[0].Pass.Files, diag.Pos)
		fixes[fn] = append(fixes[fn], b.String())
	}
	want := map[string]int{"fixEveryReuse": 2}
	for fn, n := range want {
		got := fixes[fn]
		if len(got) != n {
			t.Errorf("%s: %d diagnostics with a fix, want %d", fn, len(got), n)
			continue
		}
		for _, f := range got[1:] {
			if f != got[0] {
				t.Errorf("%s: the diagnostics carry different fixes:\n%s\n%s", fn, got[0], f)
			}
		}
	}
}

// enclosingFuncName returns the name of the function declaration in files that
// contains pos, or "" if none does.
func enclosingFuncName(files []*ast.File, pos token.Pos) string {
	for _, f := range files {
		for _, decl := range f.Decls {
			if fd, ok := decl.(*ast.FuncDecl); ok && fd.Pos() <= pos && pos < fd.End() {
				return fd.Name.Name
			}
		}
	}
	return ""
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
