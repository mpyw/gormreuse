// Package goldentest holds the shared machinery for generating and checking the
// suggested-fix golden (.golden) and diff (.diff) fixtures. Before this package
// existed, the "run the analyzer, apply its suggested fixes in reverse offset
// order, render a stable unified diff" logic was copied three times (the
// gengolden command, TestGenerateDiffFiles, and TestDiffFilesUpToDate) and the
// fixture list was hardcoded in the tests while gengolden globbed — so
// assignment_patterns.go and export.go silently had no .diff coverage (#77).
package goldentest

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/analysistest"
)

// NoopT implements the analysistest.Testing interface, swallowing every failure
// so the analyzer can be run purely to collect its suggested fixes (the
// intentional diagnostics in the fixtures are not test failures here).
type NoopT struct{}

func (NoopT) Errorf(string, ...any) {}
func (NoopT) Error(...any)          {}
func (NoopT) Fatal(...any)          {}
func (NoopT) Fatalf(string, ...any) {}
func (NoopT) Helper()               {}
func (NoopT) Log(...any)            {}
func (NoopT) Logf(string, ...any)   {}

// Fixtures returns the base names of the .go fixture files under srcDir that are
// eligible for golden/diff generation, excluding generated.go, *_test.go, and
// the generated .golden/.diff artifacts (which do not match *.go anyway).
func Fixtures(srcDir string) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(srcDir, "*.go"))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, m := range matches {
		base := filepath.Base(m)
		if base == "generated.go" || strings.HasSuffix(base, "_test.go") {
			continue
		}
		out = append(out, base)
	}
	sort.Strings(out)
	return out, nil
}

// ApplyFixes runs analyzer a over pkg (loaded from testdata) and returns
// srcPath's original bytes together with the content produced by applying every
// suggested-fix text edit targeting srcPath. Edits are matched to the file by
// their own position and applied from the highest offset down so earlier offsets
// stay valid.
func ApplyFixes(testdata, pkg, srcPath string, a *analysis.Analyzer) (original, fixed []byte, err error) {
	original, err = os.ReadFile(srcPath)
	if err != nil {
		return nil, nil, err
	}

	results := analysistest.Run(NoopT{}, testdata, a, pkg)

	type offsetEdit struct {
		start, end int
		newText    string
	}
	var edits []offsetEdit
	for _, result := range results {
		for _, diag := range result.Diagnostics {
			for _, fix := range diag.SuggestedFixes {
				for _, edit := range fix.TextEdits {
					if result.Pass.Fset.File(edit.Pos).Name() != srcPath {
						continue
					}
					edits = append(edits, offsetEdit{
						start:   result.Pass.Fset.Position(edit.Pos).Offset,
						end:     result.Pass.Fset.Position(edit.End).Offset,
						newText: string(edit.NewText),
					})
				}
			}
		}
	}

	fixed = append([]byte(nil), original...)
	if len(edits) > 0 {
		sort.Slice(edits, func(i, j int) bool { return edits[i].start > edits[j].start })
		for _, e := range edits {
			fixed = append(fixed[:e.start], append([]byte(e.newText), fixed[e.end:]...)...)
		}
	}
	return original, fixed, nil
}

var timestampRegex = regexp.MustCompile(`\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}(\.\d+)?( [+-]\d{4})?`)

// GenerateDiff renders the committed .diff representation of the fix for
// filename: a full-context unified diff between original and fixed with stable
// filename and timestamp placeholders. It returns nil (an empty diff) when the
// two are equal.
func GenerateDiff(filename string, original, fixed []byte) ([]byte, error) {
	if bytes.Equal(original, fixed) {
		return nil, nil
	}

	tmpOriginal := filepath.Join(os.TempDir(), "goldentest_original_"+filename)
	tmpFixed := filepath.Join(os.TempDir(), "goldentest_fixed_"+filename)
	defer func() { _ = os.Remove(tmpOriginal) }()
	defer func() { _ = os.Remove(tmpFixed) }()

	if err := os.WriteFile(tmpOriginal, original, 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(tmpFixed, fixed, 0o644); err != nil {
		return nil, err
	}

	// diff exits 1 when the files differ, which is expected; only the output
	// matters.
	var out bytes.Buffer
	cmd := exec.Command("diff", "-U", "999999", tmpOriginal, tmpFixed)
	cmd.Stdout = &out
	cmd.Stderr = &out
	_ = cmd.Run()

	b := out.Bytes()
	b = bytes.ReplaceAll(b, []byte(tmpOriginal), []byte(filename))
	b = bytes.ReplaceAll(b, []byte(tmpFixed), []byte(filename+".golden"))
	b = timestampRegex.ReplaceAll(b, []byte("1970-01-01 00:00:00"))
	return b, nil
}
