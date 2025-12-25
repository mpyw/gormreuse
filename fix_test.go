package gormreuse_test

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/mpyw/gormreuse"
)

func TestFixBasic(t *testing.T) {
	testdata := analysistest.TestData()

	// Create a temporary directory with only fix_basic.go
	tmpDir := t.TempDir()
	srcDir := filepath.Join(tmpDir, "src", "gormreuse")
	if err := os.MkdirAll(srcDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Copy fix_basic.go
	fixBasicSrc := filepath.Join(testdata, "src", "gormreuse", "fix_basic.go")
	fixBasicDst := filepath.Join(srcDir, "fix_basic.go")
	data, err := os.ReadFile(fixBasicSrc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixBasicDst, data, 0644); err != nil {
		t.Fatal(err)
	}

	// Copy fix_basic.go.golden
	goldenSrc := filepath.Join(testdata, "src", "gormreuse", "fix_basic.go.golden")
	goldenDst := filepath.Join(srcDir, "fix_basic.go.golden")
	data, err = os.ReadFile(goldenSrc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(goldenDst, data, 0644); err != nil {
		t.Fatal(err)
	}

	// Copy gorm stub
	gormSrcDir := filepath.Join(testdata, "src", "gorm.io", "gorm")
	gormDstDir := filepath.Join(tmpDir, "src", "gorm.io", "gorm")
	if err := os.MkdirAll(gormDstDir, 0755); err != nil {
		t.Fatal(err)
	}

	gormFiles, err := os.ReadDir(gormSrcDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range gormFiles {
		if f.IsDir() {
			continue
		}
		src := filepath.Join(gormSrcDir, f.Name())
		dst := filepath.Join(gormDstDir, f.Name())
		data, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, data, 0644); err != nil {
			t.Fatal(err)
		}
	}

	// Run test with suggested fixes
	analysistest.RunWithSuggestedFixes(t, tmpDir, gormreuse.Analyzer, "gormreuse")
}
