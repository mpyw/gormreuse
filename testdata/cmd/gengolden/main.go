// Command gengolden generates golden files for suggested fixes tests.
package main

import (
	"fmt"
	"go/token"
	"os"
	"path/filepath"
	"sort"

	"golang.org/x/tools/go/analysis/analysistest"
	"github.com/mpyw/gormreuse"
)

func main() {
	testdata := analysistest.TestData()
	srcDir := filepath.Join(testdata, "src", "gormreuse")

	// Get all source files
	files, err := filepath.Glob(filepath.Join(srcDir, "*.go"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	for _, file := range files {
		base := filepath.Base(file)

		// Skip generated and golden files
		if base == "generated.go" || filepath.Ext(base) == ".golden" {
			continue
		}

		fmt.Printf("Generating golden for %s...\n", base)

		if err := generateGoldenFile(testdata, file); err != nil {
			fmt.Printf("  Error: %v\n", err)
			continue
		}

		fmt.Printf("  Created %s.golden\n", base)
	}
}

func generateGoldenFile(testdata, srcPath string) error {
	// Read original content
	content, err := os.ReadFile(srcPath)
	if err != nil {
		return err
	}

	// Run analyzer
	results := analysistest.Run(&noopT{}, testdata, gormreuse.Analyzer, "gormreuse")

	// Collect all text edits for this file
	type edit struct {
		pos     token.Pos
		end     token.Pos
		newText []byte
	}
	var edits []edit

	srcBaseName := filepath.Base(srcPath)

	for _, result := range results {
		fset := result.Pass.Fset

		for _, diag := range result.Diagnostics {
			// Check if this diagnostic is for our file
			pos := fset.Position(diag.Pos)
			diagFile := filepath.Base(pos.Filename)
			if diagFile != srcBaseName {
				continue
			}

			// Collect all text edits from suggested fixes
			for _, fix := range diag.SuggestedFixes {
				for _, textEdit := range fix.TextEdits {
					edits = append(edits, edit{textEdit.Pos, textEdit.End, textEdit.NewText})
				}
			}
		}
	}

	// Now convert to offsets using the correct fileset
	if len(edits) == 0 {
		// No fixes, just copy
		goldenPath := srcPath + ".golden"
		return os.WriteFile(goldenPath, content, 0644)
	}

	// Get the fileset from the first result
	var fset *token.FileSet
	for _, result := range results {
		fset = result.Pass.Fset
		break
	}

	type offsetEdit struct {
		start   int
		end     int
		newText []byte
	}
	var offsetEdits []offsetEdit

	for _, e := range edits {
		start := fset.Position(e.pos).Offset
		end := fset.Position(e.end).Offset
		offsetEdits = append(offsetEdits, offsetEdit{start, end, e.newText})
	}

	// Apply edits (in reverse order to maintain offsets)
	sort.Slice(offsetEdits, func(i, j int) bool {
		return offsetEdits[i].start > offsetEdits[j].start
	})

	result := content
	for _, e := range offsetEdits {
		result = append(result[:e.start], append(e.newText, result[e.end:]...)...)
	}

	// Write golden file
	goldenPath := srcPath + ".golden"
	return os.WriteFile(goldenPath, result, 0644)
}

type noopT struct{}

func (t *noopT) Errorf(format string, args ...interface{}) {}
func (t *noopT) Fatal(args ...interface{})                 {}
func (t *noopT) Fatalf(format string, args ...interface{}) {}
func (t *noopT) Helper()                                    {}
func (t *noopT) Log(args ...interface{})                    {}
func (t *noopT) Logf(format string, args ...interface{})   {}
