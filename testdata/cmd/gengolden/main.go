// Command gengolden generates golden files for suggested fixes tests.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/mpyw/gormreuse"
	"github.com/mpyw/gormreuse/internal/goldentest"
)

func main() {
	testdata := analysistest.TestData()
	srcDir := filepath.Join(testdata, "src", "gormreuse")

	fixtures, err := goldentest.Fixtures(srcDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	for _, base := range fixtures {
		fmt.Printf("Generating golden for %s...\n", base)

		srcPath := filepath.Join(srcDir, base)
		_, fixed, err := goldentest.ApplyFixes(testdata, "gormreuse", srcPath, gormreuse.Analyzer)
		if err != nil {
			fmt.Printf("  Error: %v\n", err)
			continue
		}
		if err := os.WriteFile(srcPath+".golden", fixed, 0o644); err != nil {
			fmt.Printf("  Error: %v\n", err)
			continue
		}
		fmt.Printf("  Created %s.golden\n", base)
	}
}
