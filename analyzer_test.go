package gormreuse_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/mpyw/gormreuse"
)

func TestAnalyzer(t *testing.T) {
	testdata := analysistest.TestData()
	analysistest.Run(t, testdata, gormreuse.Analyzer, "gormreuse")
}

func TestFileFilter(t *testing.T) {
	testdata := analysistest.TestData()
	// Tests that generated files are skipped
	analysistest.Run(t, testdata, gormreuse.Analyzer, "filefilter")
}

func TestSuggestedFixes(t *testing.T) {
	testdata := analysistest.TestData()
	analysistest.RunWithSuggestedFixes(t, testdata, gormreuse.Analyzer, "gormreuse")
}

func TestGenerateDiffFiles(t *testing.T) {
	testdata := analysistest.TestData()
	srcDir := filepath.Join(testdata, "src", "gormreuse")

	testFiles := []string{
		"basic.go",
		"advanced.go",
		"evil.go",
		"ignore.go",
		"directive_validation.go",
	}

	for _, filename := range testFiles {
		t.Run(filename, func(t *testing.T) {
			srcPath := filepath.Join(srcDir, filename)
			diffPath := srcPath + ".diff"

			// Read original file
			originalContent, err := os.ReadFile(srcPath)
			if err != nil {
				t.Fatalf("Failed to read %s: %v", srcPath, err)
			}

			// Run analyzer and collect suggested fixes
			results := analysistest.Run(&noopT{t: t}, testdata, gormreuse.Analyzer, "gormreuse")

			// Collect all text edits for this file
			type offsetEdit struct {
				start, end int
				newText    string
			}
			var edits []offsetEdit

			for _, result := range results {
				for _, diag := range result.Diagnostics {
					for _, fix := range diag.SuggestedFixes {
						for _, edit := range fix.TextEdits {
							if result.Pass.Fset.File(edit.Pos).Name() == srcPath {
								edits = append(edits, offsetEdit{
									start:   result.Pass.Fset.Position(edit.Pos).Offset,
									end:     result.Pass.Fset.Position(edit.End).Offset,
									newText: string(edit.NewText),
								})
							}
						}
					}
				}
			}

			// Apply edits to generate fixed content
			fixedContent := originalContent
			if len(edits) > 0 {
				// Sort edits by offset in reverse order to apply from end to start
				sort.Slice(edits, func(i, j int) bool {
					return edits[i].start > edits[j].start
				})

				for _, edit := range edits {
					fixedContent = append(
						fixedContent[:edit.start],
						append([]byte(edit.newText), fixedContent[edit.end:]...)...,
					)
				}
			}

			// Generate unified diff
			var diffOutput bytes.Buffer
			if bytes.Equal(originalContent, fixedContent) {
				// No changes - write empty diff file
				if err := os.WriteFile(diffPath, []byte{}, 0644); err != nil {
					t.Fatalf("Failed to write empty diff file %s: %v", diffPath, err)
				}
				t.Logf("Generated empty diff file: %s", diffPath)
				return
			}

			// Write temporary files for diff
			tmpOriginal := filepath.Join(os.TempDir(), "original_"+filename)
			tmpFixed := filepath.Join(os.TempDir(), "fixed_"+filename)
			defer os.Remove(tmpOriginal)
			defer os.Remove(tmpFixed)

			if err := os.WriteFile(tmpOriginal, originalContent, 0644); err != nil {
				t.Fatalf("Failed to write temp original: %v", err)
			}
			if err := os.WriteFile(tmpFixed, fixedContent, 0644); err != nil {
				t.Fatalf("Failed to write temp fixed: %v", err)
			}

			// Run diff -u with full context (show entire file)
			cmd := exec.Command("diff", "-U", "999999", tmpOriginal, tmpFixed)
			cmd.Stdout = &diffOutput
			cmd.Stderr = &diffOutput
			// diff returns exit code 1 when files differ, which is expected
			_ = cmd.Run()

			// Replace temp filenames with actual filenames in diff header
			diffBytes := diffOutput.Bytes()
			// Replace temp paths with actual filenames in the header
			diffBytes = bytes.ReplaceAll(diffBytes, []byte(tmpOriginal), []byte(filename))
			diffBytes = bytes.ReplaceAll(diffBytes, []byte(tmpFixed), []byte(filename+".golden"))

			// Write diff file
			if err := os.WriteFile(diffPath, diffBytes, 0644); err != nil {
				t.Fatalf("Failed to write diff file %s: %v", diffPath, err)
			}

			t.Logf("Generated diff file: %s (%d bytes)", diffPath, len(diffBytes))
		})
	}
}

// noopT is a testing.T that doesn't fail on errors (for collecting results)
type noopT struct {
	t *testing.T
}

func (n *noopT) Errorf(format string, args ...interface{}) {
	// Collect diagnostics without failing
}

func (n *noopT) Fatalf(format string, args ...interface{}) {
	n.t.Logf("(suppressed) "+format, args...)
}

func (n *noopT) Helper() {}

func (n *noopT) Logf(format string, args ...interface{}) {
	// Suppress logs during collection
}

// Implement other required methods
func (n *noopT) Error(args ...interface{})                 {}
func (n *noopT) Fatal(args ...interface{})                 {}
func (n *noopT) Skip(args ...interface{})                  {}
func (n *noopT) Skipf(format string, args ...interface{})  {}
func (n *noopT) SkipNow()                                  {}
func (n *noopT) Skipped() bool                             { return false }
func (n *noopT) Failed() bool                              { return false }
func (n *noopT) Name() string                              { return "noop" }
func (n *noopT) Parallel()                                 {}
func (n *noopT) Run(name string, f func(*testing.T)) bool { return true }
func (n *noopT) Setenv(key, value string)                  {}
func (n *noopT) TempDir() string                           { return "" }
func (n *noopT) Cleanup(func())                            {}
