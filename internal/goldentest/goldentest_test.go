package goldentest

import (
	"bytes"
	"errors"
	"go/ast"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/tools/go/analysis"
)

// replaceOld suggests replacing every "old" string literal with "new", so the
// tests get a fix without depending on the gormreuse analyzer.
var replaceOld = &analysis.Analyzer{
	Name: "replaceold",
	Doc:  "replaces the string literal \"old\" with \"new\"",
	Run: func(pass *analysis.Pass) (any, error) {
		for _, f := range pass.Files {
			ast.Inspect(f, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING || lit.Value != `"old"` {
					return true
				}
				pass.Report(analysis.Diagnostic{
					Pos:     lit.Pos(),
					Message: "old literal",
					SuggestedFixes: []analysis.SuggestedFix{{
						Message:   "use new",
						TextEdits: []analysis.TextEdit{{Pos: lit.Pos(), End: lit.End(), NewText: []byte(`"new"`)}},
					}},
				})
				return true
			})
		}
		return nil, nil
	},
}

// writeFixturePackage creates a GOPATH-style tree holding package p with one
// file p.go, and returns the tree root and the file's path.
func writeFixturePackage(t *testing.T, src string) (testdata, srcPath string) {
	t.Helper()
	testdata = t.TempDir()
	dir := filepath.Join(testdata, "src", "p")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	srcPath = filepath.Join(dir, "p.go")
	if err := os.WriteFile(srcPath, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return testdata, srcPath
}

func TestUpdating(t *testing.T) {
	t.Setenv(updateEnv, "")
	if Updating() {
		t.Errorf("Updating() = true with %s empty", updateEnv)
	}
	t.Setenv(updateEnv, "1")
	if !Updating() {
		t.Errorf("Updating() = false with %s=1", updateEnv)
	}
}

func TestSyncFixture(t *testing.T) {
	testdata, srcPath := writeFixturePackage(t, "package p\n\nvar s = \"old\"\n")
	golden, diff := srcPath+".golden", srcPath+".diff"

	stale, err := SyncFixture(testdata, "p", srcPath, replaceOld, false)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{golden, diff}; !slices.Equal(stale, want) {
		t.Fatalf("missing fixtures: stale = %v, want %v", stale, want)
	}
	if _, err := os.Stat(golden); !os.IsNotExist(err) {
		t.Fatalf("check mode wrote %s", golden)
	}

	if stale, err := SyncFixture(testdata, "p", srcPath, replaceOld, true); err != nil || stale != nil {
		t.Fatalf("update: stale = %v, err = %v", stale, err)
	}
	gotGolden, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if want := "package p\n\nvar s = \"new\"\n"; string(gotGolden) != want {
		t.Errorf("golden = %q, want %q", gotGolden, want)
	}
	gotDiff, err := os.ReadFile(diff)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--- p.go\t1970-01-01 00:00:00", "+++ p.go.golden\t1970-01-01 00:00:00", "-var s = \"old\"", "+var s = \"new\""} {
		if !bytes.Contains(gotDiff, []byte(want)) {
			t.Errorf("diff lacks %q:\n%s", want, gotDiff)
		}
	}

	if stale, err := SyncFixture(testdata, "p", srcPath, replaceOld, false); err != nil || stale != nil {
		t.Fatalf("after update: stale = %v, err = %v", stale, err)
	}

	if err := os.WriteFile(diff, []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	stale, err = SyncFixture(testdata, "p", srcPath, replaceOld, false)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{diff}; !slices.Equal(stale, want) {
		t.Errorf("stale diff: stale = %v, want %v", stale, want)
	}
}

func TestSyncFixtureWithoutFix(t *testing.T) {
	src := "package p\n\nvar s = \"other\"\n"
	testdata, srcPath := writeFixturePackage(t, src)

	if _, err := SyncFixture(testdata, "p", srcPath, replaceOld, true); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(srcPath + ".golden"); string(got) != src {
		t.Errorf("golden = %q, want the unchanged source", got)
	}
	if got, err := os.ReadFile(srcPath + ".diff"); err != nil || len(got) != 0 {
		t.Errorf("diff = %q, err = %v, want an empty file", got, err)
	}
}

func TestSyncFixtureErrors(t *testing.T) {
	testdata, srcPath := writeFixturePackage(t, "package p\n\nvar s = \"old\"\n")

	if _, err := SyncFixture(testdata, "p", srcPath+".missing", replaceOld, false); err == nil {
		t.Error("missing source: want an error")
	}

	// A directory in place of the .golden file makes the write fail.
	if err := os.Mkdir(srcPath+".golden", 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := SyncFixture(testdata, "p", srcPath, replaceOld, true); err == nil {
		t.Error("unwritable .golden: want an error")
	}

	failing := &analysis.Analyzer{
		Name: "failing",
		Doc:  "always fails",
		Run:  func(*analysis.Pass) (any, error) { return nil, errors.New("boom") },
	}
	if _, err := SyncFixture(testdata, "p", srcPath, failing, false); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("failing analyzer: err = %v, want one containing boom", err)
	}

	t.Run("diff fails", func(t *testing.T) {
		writeFakeDiff(t, "exit 2")
		if _, err := SyncFixture(testdata, "p", srcPath, replaceOld, false); err == nil {
			t.Error("failing diff: want an error")
		}
	})

	t.Run("no go on PATH", func(t *testing.T) {
		t.Setenv("PATH", "")
		if _, err := SyncFixture(testdata, "p", srcPath, replaceOld, false); err == nil {
			t.Error("package not loaded: want an error")
		}
	})
}

// TestApplyFixesSeveralEdits checks that edits apply from the highest offset
// down and that edits to another file of the package are left out.
func TestApplyFixesSeveralEdits(t *testing.T) {
	testdata, srcPath := writeFixturePackage(t, "package p\n\nvar a, b = \"old\", \"old\"\n")
	if err := os.WriteFile(filepath.Join(filepath.Dir(srcPath), "q.go"), []byte("package p\n\nvar c = \"old\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, fixed, err := ApplyFixes(testdata, "p", srcPath, replaceOld)
	if err != nil {
		t.Fatal(err)
	}
	if want := "package p\n\nvar a, b = \"new\", \"new\"\n"; string(fixed) != want {
		t.Errorf("fixed = %q, want %q", fixed, want)
	}
}

// TestApplyFixesSharedEdit checks that an edit two diagnostics' fixes both
// carry, such as one import, is applied once.
func TestApplyFixesSharedEdit(t *testing.T) {
	commentEach := &analysis.Analyzer{
		Name: "commenteach",
		Doc:  "suggests the same comment above the package clause for each var",
		Run: func(pass *analysis.Pass) (any, error) {
			for _, f := range pass.Files {
				for _, decl := range f.Decls {
					pass.Report(analysis.Diagnostic{
						Pos:     decl.Pos(),
						Message: "var",
						SuggestedFixes: []analysis.SuggestedFix{{
							Message: "comment",
							TextEdits: []analysis.TextEdit{
								{Pos: f.Package, End: f.Package, NewText: []byte("// c\n")},
								{Pos: decl.Pos(), End: decl.Pos(), NewText: []byte("// v\n")},
							},
						}},
					})
				}
			}
			return nil, nil
		},
	}
	testdata, srcPath := writeFixturePackage(t, "package p\n\nvar a int\n\nvar b int\n")
	_, fixed, err := ApplyFixes(testdata, "p", srcPath, commentEach)
	if err != nil {
		t.Fatal(err)
	}
	if want := "// c\npackage p\n\n// v\nvar a int\n\n// v\nvar b int\n"; string(fixed) != want {
		t.Errorf("fixed = %q, want %q", fixed, want)
	}
}

func TestSyncOrphans(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.go", "a.go.golden", "a.go.diff", "generated.go", "generated.go.golden", "gone.go.diff"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{filepath.Join(dir, "generated.go.golden"), filepath.Join(dir, "gone.go.diff")}

	orphans, err := SyncOrphans(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(orphans, want) {
		t.Fatalf("orphans = %v, want %v", orphans, want)
	}
	for _, path := range want {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("check mode removed %s", path)
		}
	}

	if orphans, err := SyncOrphans(dir, true); err != nil || orphans != nil {
		t.Fatalf("update: orphans = %v, err = %v", orphans, err)
	}
	for _, path := range want {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("update kept %s", path)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "a.go.golden")); err != nil {
		t.Errorf("update removed a fixture's golden: %v", err)
	}
}

func TestSyncOrphansBadPattern(t *testing.T) {
	if _, err := SyncOrphans(filepath.Join(t.TempDir(), "["), false); err == nil {
		t.Error("malformed glob pattern: want an error")
	}
}

func TestSyncOrphansRemoveError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gone.go.diff"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// Removing an entry needs write permission on its directory.
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if _, err := SyncOrphans(dir, true); err == nil {
		t.Error("read-only directory: want an error")
	}
}

// writeFakeDiff puts a `diff` script with the given body first on PATH.
func writeFakeDiff(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "diff"), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestGenerateDiffErrors(t *testing.T) {
	original, fixed := []byte("a\n"), []byte("b\n")

	tests := []struct {
		name  string
		setup func(t *testing.T)
		want  string
	}{
		{"diff missing", func(t *testing.T) { t.Setenv("PATH", "") }, "executable file not found"},
		{"diff fails", func(t *testing.T) { writeFakeDiff(t, "echo broken >&2; exit 2") }, "broken"},
		{"no output", func(t *testing.T) { writeFakeDiff(t, "exit 1") }, "no output"},
		{"no temp dir", func(t *testing.T) { t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing")) }, "missing"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.setup(t)
			_, err := generateDiff("x.go", original, fixed)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want one containing %q", err, tt.want)
			}
		})
	}

	if got, err := generateDiff("x.go", original, original); got != nil || err != nil {
		t.Errorf("equal inputs: got %q, err = %v, want nil, nil", got, err)
	}
}
