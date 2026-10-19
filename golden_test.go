package gormreuse_test

import (
	"os"
	"path/filepath"
	"testing"

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
