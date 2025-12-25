package gormreuse_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
		"file_level_ignore.go",
		"file_level_ignore_doc.go",
		"directive_validation.go",
		"nested_chaos.go",
		"fix_constraints.go",
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
			results := analysistest.Run(&noopT{T: t}, testdata, gormreuse.Analyzer, "gormreuse")

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
			defer func() { _ = os.Remove(tmpOriginal) }()
			defer func() { _ = os.Remove(tmpFixed) }()

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

			// Replace temp filenames and timestamps with placeholders
			diffBytes := diffOutput.Bytes()
			// Replace temp paths with actual filenames in the header
			diffBytes = bytes.ReplaceAll(diffBytes, []byte(tmpOriginal), []byte(filename))
			diffBytes = bytes.ReplaceAll(diffBytes, []byte(tmpFixed), []byte(filename+".golden"))

			// Replace timestamps with placeholder
			// Handles both macOS (no TZ) and Linux (with TZ) formats:
			// macOS: "2025-12-25 19:03:15" -> "1970-01-01 00:00:00"
			// Linux: "2025-12-25 19:03:15.123456789 +0000" -> "1970-01-01 00:00:00"
			timestampRegex := regexp.MustCompile(`\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}(\.\d+)?( [+-]\d{4})?`)
			diffBytes = timestampRegex.ReplaceAll(diffBytes, []byte("1970-01-01 00:00:00"))

			// Write diff file
			if err := os.WriteFile(diffPath, diffBytes, 0644); err != nil {
				t.Fatalf("Failed to write diff file %s: %v", diffPath, err)
			}

			t.Logf("Generated diff file: %s (%d bytes)", diffPath, len(diffBytes))
		})
	}
}

// noopT wraps testing.T to suppress errors during result collection.
// This allows us to run the analyzer without failing on diagnostics,
// so we can collect all suggested fixes for golden file generation.
type noopT struct {
	*testing.T
}

// Override error methods to be no-ops
func (n *noopT) Errorf(format string, args ...interface{}) {}
func (n *noopT) Error(args ...interface{})                 {}
func (n *noopT) Fatal(args ...interface{})                 {}
func (n *noopT) Fatalf(format string, args ...interface{}) {}

func TestDiffFilesUpToDate(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping diff file check in short mode")
	}

	// Run diff generation
	testdata := analysistest.TestData()
	srcDir := filepath.Join(testdata, "src", "gormreuse")

	testFiles := []string{
		"basic.go",
		"advanced.go",
		"evil.go",
		"ignore.go",
		"file_level_ignore.go",
		"file_level_ignore_doc.go",
		"directive_validation.go",
		"nested_chaos.go",
		"fix_constraints.go",
	}

	for _, filename := range testFiles {
		srcPath := filepath.Join(srcDir, filename)
		diffPath := srcPath + ".diff"

		// Read current diff file
		beforeContent, err := os.ReadFile(diffPath)
		if err != nil {
			t.Fatalf("Failed to read existing diff %s: %v", diffPath, err)
		}

		// Generate new diff (reuse TestGenerateDiffFiles logic)
		originalContent, err := os.ReadFile(srcPath)
		if err != nil {
			t.Fatalf("Failed to read %s: %v", srcPath, err)
		}

		results := analysistest.Run(&noopT{T: t}, testdata, gormreuse.Analyzer, "gormreuse")

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

		fixedContent := originalContent
		if len(edits) > 0 {
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

		var diffOutput bytes.Buffer
		if bytes.Equal(originalContent, fixedContent) {
			// No changes - should have empty diff
			if len(beforeContent) != 0 {
				t.Errorf("%s: expected empty diff but found %d bytes", filename, len(beforeContent))
			}
			continue
		}

		tmpOriginal := filepath.Join(os.TempDir(), "check_original_"+filename)
		tmpFixed := filepath.Join(os.TempDir(), "check_fixed_"+filename)
		defer func() { _ = os.Remove(tmpOriginal) }()
		defer func() { _ = os.Remove(tmpFixed) }()

		if err := os.WriteFile(tmpOriginal, originalContent, 0644); err != nil {
			t.Fatalf("Failed to write temp original: %v", err)
		}
		if err := os.WriteFile(tmpFixed, fixedContent, 0644); err != nil {
			t.Fatalf("Failed to write temp fixed: %v", err)
		}

		cmd := exec.Command("diff", "-U", "999999", tmpOriginal, tmpFixed)
		cmd.Stdout = &diffOutput
		cmd.Stderr = &diffOutput
		_ = cmd.Run()

		diffBytes := diffOutput.Bytes()
		diffBytes = bytes.ReplaceAll(diffBytes, []byte(tmpOriginal), []byte(filename))
		diffBytes = bytes.ReplaceAll(diffBytes, []byte(tmpFixed), []byte(filename+".golden"))

		timestampRegex := regexp.MustCompile(`\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}(\.\d+)?( [+-]\d{4})?`)
		diffBytes = timestampRegex.ReplaceAll(diffBytes, []byte("1970-01-01 00:00:00"))

		// Compare with existing diff file
		if !bytes.Equal(beforeContent, diffBytes) {
			// Write current and expected to temp files for diffing
			tmpBefore := filepath.Join(os.TempDir(), "before_"+filename+".diff")
			tmpAfter := filepath.Join(os.TempDir(), "after_"+filename+".diff")
			defer func() { _ = os.Remove(tmpBefore) }()
			defer func() { _ = os.Remove(tmpAfter) }()

			if err := os.WriteFile(tmpBefore, beforeContent, 0644); err != nil {
				t.Fatalf("Failed to write temp before: %v", err)
			}
			if err := os.WriteFile(tmpAfter, diffBytes, 0644); err != nil {
				t.Fatalf("Failed to write temp after: %v", err)
			}

			// Show diff between current and expected
			var metaDiff bytes.Buffer
			metaCmd := exec.Command("diff", "-u", tmpBefore, tmpAfter)
			metaCmd.Stdout = &metaDiff
			metaCmd.Stderr = &metaDiff
			_ = metaCmd.Run()

			t.Errorf("%s.diff is out of date.\nRun: go test -run TestGenerateDiffFiles\nThen commit the changes.\n\nDiff between current and expected:\n%s",
				filename, metaDiff.String())
		}
	}
}
