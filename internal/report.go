//declscope:shared // run.go, checker.go and immutable.go report through it
package internal

import (
	"cmp"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/tools/go/analysis"

	"github.com/mpyw/gormreuse/internal/directive"
	"github.com/mpyw/gormreuse/internal/ssa/pollution"
)

// ReportInOrder holds back every diagnostic pass reports until flush, which
// reports them sorted by file, offset and message (#273). Some diagnostics come
// from map ranges, such as the unused-directive ones, and the -json output
// lists diagnostics in report order, unlike the text output, which the driver
// sorts. Sorting them all keeps -json output the same on every run.
//
// Positions are compared by file name and offset, not as token.Pos: the
// loader parses files concurrently, so their order in the file set can vary.
func ReportInOrder(pass *analysis.Pass) (flush func()) {
	report := pass.Report
	var diags []analysis.Diagnostic
	pass.Report = func(d analysis.Diagnostic) { diags = append(diags, d) }
	return func() {
		pass.Report = report
		slices.SortStableFunc(diags, func(a, b analysis.Diagnostic) int {
			pa, pb := pass.Fset.PositionFor(a.Pos, false), pass.Fset.PositionFor(b.Pos, false)
			return cmp.Or(cmp.Compare(pa.Filename, pb.Filename), cmp.Compare(pa.Offset, pb.Offset), cmp.Compare(a.Message, b.Message))
		})
		for _, d := range diags {
			report(d)
		}
	}
}

// reportRemovalFunc reports a diagnostic whose fix deletes targets.
type reportRemovalFunc func(pos token.Pos, message string, targets ...directive.RemovalTarget)

// reportUnusedImmutableInputs reports the immutable-input directives that name
// no usable callback or are attached to no function. The fix deletes the
// unused parts.
func reportUnusedImmutableInputs(set *directive.ImmutableInputSet, report reportRemovalFunc) {
	for _, u := range set.GetUnused() {
		var targets []directive.RemovalTarget
		for _, part := range u.Parts {
			targets = append(targets, directive.RemovalTarget{Comment: u.Pos, Part: part})
		}
		report(u.Pos, u.Reason, targets...)
	}
}

// reportUnusedIgnores reports the ignore directives that suppressed nothing.
// No ignore suppresses these. The fix deletes an ignore when removable
// reports that it may.
func reportUnusedIgnores(removals *directive.Removals, ignores *directive.Ignores, removable func(token.Pos) bool) {
	for _, pos := range ignores.UnusedIgnores() {
		if removable(pos) {
			removals.Report(pos, "unused gormreuse:ignore directive", directive.RemovalTarget{Comment: pos, Part: "ignore"})
		} else {
			removals.Report(pos, "unused gormreuse:ignore directive")
		}
	}
}

// reportIgnoreRemovable returns whether a fix may delete the unused ignore at a
// position (#248). It may not after an internal error, which may have dropped
// the diagnostic the ignore covers. Nor may it in a file other than a test
// file when the package has test files: the test variant of the package also
// analyzes the file, with the test files' calls, and may need the ignore. A
// pass sees one variant only, so the pass without the test files cannot tell.
// A test file is in the test variant alone, which sees the whole package.
func reportIgnoreRemovable(pass *analysis.Pass, internalError bool) func(token.Pos) bool {
	if internalError {
		return func(token.Pos) bool { return false }
	}
	return reportSeesWholePackage(pass)
}

// reportSeesWholePackage returns whether pass sees every file of the package that
// may use the code at a position. It does for a test file, which only the
// test variant analyzes, and for a file of a package without test files. A
// pass of a package with test files may be the variant without them (#248,
// #245).
func reportSeesWholePackage(pass *analysis.Pass) func(token.Pos) bool {
	hasTests := make(map[string]bool) // By directory
	return func(pos token.Pos) bool {
		filename := pass.Fset.PositionFor(pos, false).Filename
		if strings.HasSuffix(filename, "_test.go") {
			return true
		}
		dir := filepath.Dir(filename)
		has, seen := hasTests[dir]
		if !seen {
			has = reportDirHasTests(pass, dir)
			hasTests[dir] = has
		}
		return !has
	}
}

// reportDirHasTests reports whether dir holds a _test.go file, the test files
// of the package of pass among them. A directory that cannot be read counts
// as holding one.
//
//declscope:private
func reportDirHasTests(pass *analysis.Pass, dir string) bool {
	for _, f := range pass.Files {
		if strings.HasSuffix(pass.Fset.PositionFor(f.Pos(), false).Filename, "_test.go") {
			return true
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return true
	}
	return slices.ContainsFunc(entries, func(e os.DirEntry) bool { return strings.HasSuffix(e.Name(), "_test.go") })
}

// reportUnusedDirectiveFuncs reports pure / immutable-return / immutable-param
// directives that matched no valid function. For combined directives (e.g.
// //gormreuse:pure,immutable-return,immutable-param) a directive at a position
// is "used" if ANY of its combined siblings is used, so each set is suppressed
// when another set reports that position as used. The fix deletes the unused
// part of the comment (#248).
func reportUnusedDirectiveFuncs(pureFuncs, immutableReturnFuncs, immutableParamFuncs *directive.DirectiveFuncSet, report reportRemovalFunc) {
	usedByOther := func(pos token.Pos, others ...*directive.DirectiveFuncSet) bool {
		return slices.ContainsFunc(others, func(s *directive.DirectiveFuncSet) bool {
			return s != nil && s.IsUsed(pos)
		})
	}
	reportSet := func(set *directive.DirectiveFuncSet, name string, others ...*directive.DirectiveFuncSet) {
		if set == nil {
			return
		}
		for _, pos := range set.GetUnusedDirectives() {
			if usedByOther(pos, others...) {
				continue
			}
			report(pos, "unused gormreuse:"+name+" directive", directive.RemovalTarget{Comment: pos, Part: name})
		}
	}

	reportSet(pureFuncs, "pure", immutableReturnFuncs, immutableParamFuncs)
	reportSet(immutableReturnFuncs, "immutable-return", pureFuncs, immutableParamFuncs)
	reportSet(immutableParamFuncs, "immutable-param", pureFuncs, immutableReturnFuncs)
}

// reportKey identifies a diagnostic for deduplication. A position has one
// reuse diagnostic, which names one of the roots reused there and carries the
// fixes of all of them, and one diagnostic of each other message. Reporting
// only the first diagnostic at a position hid the others until a fix removed
// it, as for `q = q.Where(r); q.Count(nil); r.Find(nil); q.Find(nil)`, where
// q.Find both reuses q and reads r after r.Find changed it (#247).
type reportKey struct {
	//declscope:private
	pos token.Pos
	// message is "" for the reuse diagnostic.
	//
	//declscope:private
	message string
}

// reportKeyOf returns the reportKey of v.
func reportKeyOf(v pollution.Violation) reportKey {
	if v.Root != nil {
		return reportKey{pos: v.Pos}
	}
	return reportKey{pos: v.Pos, message: v.Message}
}

// reportGroupByPos groups violations by position. Groups keep the order in which
// their position first appears, and violations keep their order in a group.
func reportGroupByPos(violations []pollution.Violation) [][]pollution.Violation {
	var groups [][]pollution.Violation
	index := make(map[token.Pos]int)
	for _, v := range violations {
		i, ok := index[v.Pos]
		if !ok {
			i = len(groups)
			index[v.Pos] = i
			groups = append(groups, nil)
		}
		groups[i] = append(groups[i], v)
	}
	return groups
}

// mergeReportFixes combines fixes into one fix. Its edits are sorted by position,
// an edit that two roots both need appears once, and its message joins the
// distinct messages in order.
func mergeReportFixes(fixes []analysis.SuggestedFix) []analysis.SuggestedFix {
	if len(fixes) <= 1 {
		return fixes
	}
	var messages []string
	var edits []analysis.TextEdit
	for _, f := range fixes {
		if !slices.Contains(messages, f.Message) {
			messages = append(messages, f.Message)
		}
		for _, e := range f.TextEdits {
			if !slices.ContainsFunc(edits, func(x analysis.TextEdit) bool {
				return x.Pos == e.Pos && x.End == e.End && string(x.NewText) == string(e.NewText)
			}) {
				edits = append(edits, e)
			}
		}
	}
	slices.SortStableFunc(edits, func(a, b analysis.TextEdit) int { return cmp.Compare(a.Pos, b.Pos) })
	return []analysis.SuggestedFix{{Message: strings.Join(messages, "; "), TextEdits: edits}}
}
