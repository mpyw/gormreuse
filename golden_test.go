package gormreuse_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

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

	orphans, err := goldentest.Orphans(srcDir)
	if err != nil {
		t.Fatalf("Failed to list orphaned fixtures: %v", err)
	}
	for _, path := range orphans {
		if !update {
			t.Errorf("%s has no source fixture.\nRun: %s\nThen review and commit the changes.", path, goldentest.UpdateCommand)
			continue
		}
		if err := os.Remove(path); err != nil {
			t.Fatalf("Failed to remove %s: %v", path, err)
		}
		t.Logf("Removed orphaned %s", path)
	}

	for _, filename := range fixtures {
		t.Run(filename, func(t *testing.T) {
			t.Parallel()
			srcPath := filepath.Join(srcDir, filename)

			golden, diff, err := goldentest.Render(testdata, "gormreuse", srcPath, gormreuse.Analyzer)
			if err != nil {
				t.Fatalf("Failed to render fixtures for %s: %v", srcPath, err)
			}

			for path, want := range map[string][]byte{srcPath + ".golden": golden, srcPath + ".diff": diff} {
				if update {
					if err := os.WriteFile(path, want, 0o644); err != nil {
						t.Fatalf("Failed to write %s: %v", path, err)
					}
					continue
				}
				got, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(got, want) {
					t.Errorf("%s is missing or out of date.\nRun: %s\nThen review and commit the changes.", filepath.Base(path), goldentest.UpdateCommand)
				}
			}
		})
	}
}
