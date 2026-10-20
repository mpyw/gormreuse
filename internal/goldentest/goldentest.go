// Package goldentest holds the shared machinery for generating and checking the
// suggested-fix golden (.golden) and diff (.diff) fixtures. Before this package
// existed, the "run the analyzer, apply its suggested fixes in reverse offset
// order, render a stable unified diff" logic was copied three times (the
// gengolden command, TestGenerateDiffFiles, and TestDiffFilesUpToDate) and the
// fixture list was hardcoded in the tests while gengolden globbed — so
// assignment_patterns.go and export.go silently had no .diff coverage (#77).
//
// TestGoldenFiles is now the only generator and checker. A plain `go test` only
// compares the fixtures and never writes them. It writes them only when the
// updateEnv environment variable is set (#203).
package goldentest

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/analysistest"
)

// updateEnv is the environment variable that switches the fixture tests from
// checking the committed .golden/.diff files to rewriting them.
const updateEnv = "GORMREUSE_UPDATE_GOLDEN"

// UpdateCommand is the command that regenerates every .golden/.diff fixture.
// Failure messages print it so the update path is visible where it is needed.
const UpdateCommand = updateEnv + "=1 go test -run TestGoldenFiles ."

// Updating reports whether updateEnv asks the tests to rewrite the fixtures.
func Updating() bool {
	return os.Getenv(updateEnv) != ""
}

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
	slices.Sort(out)
	return out, nil
}

// Analysis is one run of an analyzer over a package. The fixes for each of the
// package's files come from the same run, so a test that checks every file
// analyzes the package once (#195: with analysis facts, each run also
// analyzes every dependency).
type Analysis struct {
	results []*analysistest.Result
}

// Analyze runs analyzer a over pkg, loaded from testdata.
func Analyze(testdata, pkg string, a *analysis.Analyzer) (*Analysis, error) {
	// NoopT also swallows load failures, which would leave no fixes and so an
	// unchanged file. Report them instead (#203).
	results := analysistest.Run(NoopT{}, testdata, a, pkg)
	if len(results) == 0 {
		return nil, fmt.Errorf("analysistest loaded no package for %q from %s", pkg, testdata)
	}
	for _, result := range results {
		if result.Err != nil {
			return nil, fmt.Errorf("analyzing %s: %w", pkg, result.Err)
		}
	}
	return &Analysis{results: results}, nil
}

// ApplyFixes returns srcPath's original bytes together with the content
// produced by applying every suggested-fix text edit of the run that targets
// srcPath. Edits are matched to the file by their own position and applied
// from the highest offset down so earlier offsets stay valid. Each
// diagnostic's fix is complete on its own (#200), so two fixes may carry the
// same edit, such as one gorm import. That edit is applied once, as the -fix
// driver does.
func (r *Analysis) ApplyFixes(srcPath string) (original, fixed []byte, err error) {
	original, err = os.ReadFile(srcPath)
	if err != nil {
		return nil, nil, err
	}
	results := r.results

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
						start:   result.Pass.Fset.PositionFor(edit.Pos, false).Offset,
						end:     result.Pass.Fset.PositionFor(edit.End, false).Offset,
						newText: string(edit.NewText),
					})
				}
			}
		}
	}

	fixed = bytes.Clone(original)
	// Ties are broken by end and text so that identical edits sit together.
	slices.SortStableFunc(edits, func(a, b offsetEdit) int {
		return cmp.Or(cmp.Compare(b.start, a.start), cmp.Compare(a.end, b.end), cmp.Compare(a.newText, b.newText))
	})
	for _, e := range slices.Compact(edits) {
		fixed = slices.Replace(fixed, e.start, e.end, []byte(e.newText)...)
	}
	return original, fixed, nil
}

// syncFixture compares srcPath's committed .golden file (the source with every
// suggested fix applied) and .diff file (see generateDiff) with what analyzer a
// produces now, and returns the paths of those that are missing or stale. With
// update set, it rewrites both files instead and returns nothing stale.
func syncFixture(testdata, pkg, srcPath string, a *analysis.Analyzer, update bool) (stale []string, err error) {
	run, err := Analyze(testdata, pkg, a)
	if err != nil {
		return nil, err
	}
	return run.SyncFixture(srcPath, update)
}

// SyncFixture is the package-level syncFixture for a run that is already done.
func (r *Analysis) SyncFixture(srcPath string, update bool) (stale []string, err error) {
	original, fixed, err := r.ApplyFixes(srcPath)
	if err != nil {
		return nil, err
	}
	diff, err := generateDiff(filepath.Base(srcPath), original, fixed)
	if err != nil {
		return nil, err
	}

	for _, f := range []struct {
		path string
		want []byte
	}{{srcPath + ".golden", fixed}, {srcPath + ".diff", diff}} {
		if update {
			if err := os.WriteFile(f.path, f.want, 0o644); err != nil {
				return nil, err
			}
			continue
		}
		got, err := os.ReadFile(f.path)
		if err != nil || !bytes.Equal(got, f.want) {
			stale = append(stale, f.path)
		}
	}
	return stale, nil
}

// SyncOrphans returns the .golden/.diff files under srcDir whose source fixture
// is gone (or is no longer a fixture, see Fixtures). They would otherwise sit
// in the tree unchecked. With update set, it deletes them instead and returns
// nothing.
func SyncOrphans(srcDir string, update bool) (orphans []string, err error) {
	fixtures, err := Fixtures(srcDir)
	if err != nil {
		return nil, err
	}
	for _, ext := range []string{".golden", ".diff"} {
		matches, err := filepath.Glob(filepath.Join(srcDir, "*.go"+ext))
		if err != nil {
			return nil, err
		}
		for _, m := range matches {
			if slices.Contains(fixtures, strings.TrimSuffix(filepath.Base(m), ext)) {
				continue
			}
			if !update {
				orphans = append(orphans, m)
				continue
			}
			if err := os.Remove(m); err != nil {
				return nil, err
			}
		}
	}
	slices.Sort(orphans)
	return orphans, nil
}

var timestampRegex = regexp.MustCompile(`\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}(\.\d+)?( [+-]\d{4})?`)

// generateDiff renders the committed .diff representation of the fix for
// filename: a full-context unified diff between original and fixed with stable
// filename and timestamp placeholders. It returns nil (an empty diff) when the
// two are equal.
func generateDiff(filename string, original, fixed []byte) ([]byte, error) {
	if bytes.Equal(original, fixed) {
		return nil, nil
	}

	// A private directory per call, so that concurrent test runs (for example
	// in several worktrees) cannot overwrite each other's inputs.
	tmpDir, err := os.MkdirTemp("", "goldentest")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()
	tmpOriginal := filepath.Join(tmpDir, "original_"+filename)
	tmpFixed := filepath.Join(tmpDir, "fixed_"+filename)

	if err := os.WriteFile(tmpOriginal, original, 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(tmpFixed, fixed, 0o644); err != nil {
		return nil, err
	}

	// diff exits 1 when the files differ, which is expected. Any other failure,
	// including diff missing from PATH, must not turn into an empty diff that
	// then compares equal to another empty diff (#203).
	var out, stderr bytes.Buffer
	cmd := exec.Command("diff", "-U", "999999", tmpOriginal, tmpFixed)
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
			return nil, fmt.Errorf("diff %s: %w (stderr: %q)", filename, err, stderr.Bytes())
		}
	}
	if out.Len() == 0 {
		return nil, fmt.Errorf("diff %s: no output for files that differ", filename)
	}

	b := out.Bytes()
	b = bytes.ReplaceAll(b, []byte(tmpOriginal), []byte(filename))
	b = bytes.ReplaceAll(b, []byte(tmpFixed), []byte(filename+".golden"))
	b = timestampRegex.ReplaceAll(b, []byte("1970-01-01 00:00:00"))
	return b, nil
}
