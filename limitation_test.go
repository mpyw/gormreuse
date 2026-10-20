package gormreuse_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// limitationKinds are the kinds of known limitation, as the bold label that
// opens each in the Known Limitations sections of design/implementation.md
// and README.md. The two sections must have the same kinds, and the same
// table rows for each kind that has a table.
var limitationKinds = []string{
	"**Missed reuse (false negatives)",
	"**Extra reports (false positives)",
	"**Wrong position",
	"**No fix",
}

// TestKnownLimitationsListed keeps the Known Limitations section of
// design/implementation.md in step with the fixtures and the README:
//
//   - Each fixture function marked [LIMITATION] is named in the section.
//   - Each fixture the section names, as a code span in a Fixtures column or
//     after "Fixtures:", is a marked fixture function.
//   - A [LIMITATION] marker belongs to a function: one after the last
//     function of its file fails.
//   - The README's Known Limitations section has the same kinds of
//     limitation, and each kind has a table in both files or in neither.
//     Each README row starts with a <!-- fixtures: a, b --> comment that
//     names the fixtures of the notes' row, and the rows are in the same
//     order. Either file may write a table in Markdown or in HTML, folded in
//     <details> or not, and the notes' table has a Fixtures column.
//   - The README's summary table gives each kind's case count, or "—" for a
//     kind without a table, and the count matches that kind's table.
func TestKnownLimitationsListed(t *testing.T) {
	section := limitationSection(t, "design/implementation.md")
	marked := markedLimitationFuncs(t, filepath.Join("testdata", "src"))
	if len(marked) == 0 {
		t.Fatal("found no [LIMITATION] markers in testdata/src")
	}
	for name, file := range marked {
		if !strings.Contains(section, "`"+name+"`") {
			t.Errorf("%s: %s is marked [LIMITATION] but not listed in the Known Limitations section of design/implementation.md", file, name)
		}
	}

	readme := limitationSection(t, "README.md")
	var named []string
	for _, kind := range limitationKinds {
		notesRows, inNotes := limitationTableRows(t, "design/implementation.md", section, kind)
		readmeRows, inReadme := limitationTableRows(t, "README.md", readme, kind)
		for _, row := range notesRows {
			if row != "" {
				named = append(named, strings.Split(row, ", ")...)
			}
		}
		switch {
		case !inNotes || !inReadme:
			t.Errorf("Known Limitations: %s is in design/implementation.md: %t, in README.md: %t; want both", kind, inNotes, inReadme)
		case !slices.Equal(notesRows, readmeRows):
			t.Errorf("Known Limitations: %s has the rows (by fixtures)\n\t%q in design/implementation.md and\n\t%q in README.md; list each case in both, in the same order", kind, notesRows, readmeRows)
		}
		name := strings.TrimPrefix(kind, "**")
		want := "—"
		if len(readmeRows) > 0 {
			want = strconv.Itoa(len(readmeRows))
		}
		m := regexp.MustCompile(`(?m)^\| ` + regexp.QuoteMeta(name) + ` \| ([^ |]+) \|`).FindStringSubmatch(readme)
		switch {
		case m == nil:
			t.Errorf("README.md: Known Limitations: the summary table has no %s row", name)
		case m[1] != want:
			t.Errorf("README.md: Known Limitations: the summary table gives %s %s cases, but its table has %d rows; want %s", name, m[1], len(readmeRows), want)
		}
	}
	for _, list := range limitationFixtureList.FindAllStringSubmatch(section, -1) {
		for _, m := range limitationCodeSpan.FindAllStringSubmatch(list[1], -1) {
			named = append(named, m[1])
		}
	}
	for _, name := range named {
		if _, ok := marked[name]; !ok {
			t.Errorf("design/implementation.md: Known Limitations names fixture %s, but no fixture function of that name is marked [LIMITATION]", name)
		}
	}
}

// limitationSection returns the ## Known Limitations section of a Markdown
// file.
func limitationSection(t *testing.T, path string) string {
	t.Helper()
	doc, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, section, ok := strings.Cut(string(doc), "\n## Known Limitations\n")
	if !ok {
		t.Fatalf("%s has no ## Known Limitations section", path)
	}
	section, _, _ = strings.Cut(section, "\n## ")
	return section
}

var (
	limitationFixtureList = regexp.MustCompile(`Fixtures?: ([^.]+)\.`)
	limitationCodeSpan    = regexp.MustCompile("`([A-Za-z_][A-Za-z0-9_]*)`")
	limitationRowFixtures = regexp.MustCompile(`<!-- fixtures: ([A-Za-z0-9_, ]+) -->`)
	limitationCellBreak   = regexp.MustCompile(`\\.|\|`)
)

// limitationTableRows returns the body rows of the first table after the
// paragraph starting with kind and before the next kind, each as the
// comma-separated names of its fixtures, and whether that paragraph is there.
// A kind without a table has no rows. A row's fixtures are the code spans of
// its Fixtures cell when the table has a Fixtures column, and the names in
// its <!-- fixtures: ... --> comment when it does not. The table may be a
// Markdown table or an HTML <table>, whose body rows are the <tr> lines
// without a <th>. A table without rows, an HTML table without its closing
// </table>, a Markdown row with a different number of cells than the header,
// and a row without fixtures fail the test.
func limitationTableRows(t *testing.T, path, section, kind string) (rows []string, found bool) {
	t.Helper()
	i := strings.Index(section, "\n"+kind)
	if i < 0 {
		return nil, false
	}
	lines := strings.Split(section[i+1:], "\n")[1:]
	for j, line := range lines {
		if slices.ContainsFunc(limitationKinds, func(k string) bool { return strings.HasPrefix(line, k) }) {
			lines = lines[:j]
			break
		}
	}
	start := slices.IndexFunc(lines, func(line string) bool { return line == "<table>" || strings.HasPrefix(line, "|") })
	if start < 0 {
		return nil, true
	}
	lines = lines[start:]
	addRow := func(row, fixtures string) {
		if fixtures == "" {
			t.Errorf("%s: Known Limitations: the %s row %q names no fixtures", path, kind, row)
		}
		rows = append(rows, fixtures)
	}
	if lines[0] == "<table>" {
		end := slices.Index(lines, "</table>")
		if end < 0 {
			t.Errorf("%s: Known Limitations: the %s table has no </table>", path, kind)
			end = len(lines)
		}
		for _, line := range lines[1:end] {
			if strings.HasPrefix(line, "<tr>") && !strings.Contains(line, "<th>") {
				addRow(line, rowFixtureComment(line))
			}
		}
	} else {
		header := limitationCells(lines[0])
		fixturesColumn := slices.Index(header, "Fixtures")
		for _, line := range lines[min(2, len(lines)):] { // after the header row and the delimiter row
			if !strings.HasPrefix(line, "|") {
				break
			}
			cells := limitationCells(line)
			switch {
			case len(cells) != len(header):
				t.Errorf("%s: Known Limitations: the %s row %q has %d cells, but the header has %d; escape a | in a cell as \\|", path, kind, line, len(cells), len(header))
				rows = append(rows, "")
			case fixturesColumn >= 0:
				var names []string
				for _, m := range limitationCodeSpan.FindAllStringSubmatch(cells[fixturesColumn], -1) {
					names = append(names, m[1])
				}
				addRow(line, strings.Join(names, ", "))
			default:
				addRow(line, rowFixtureComment(line))
			}
		}
	}
	if len(rows) == 0 {
		t.Errorf("%s: Known Limitations: the %s table has no rows", path, kind)
	}
	return rows, true
}

// rowFixtureComment returns the names in the <!-- fixtures: a, b --> comment
// of a table row, or "" when it has none.
func rowFixtureComment(row string) string {
	m := limitationRowFixtures.FindStringSubmatch(row)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(m[1])
}

// limitationCells returns the cells of a Markdown table row, split at each
// | that is not escaped as \|.
func limitationCells(row string) []string {
	var cells []string
	last := 0
	for _, loc := range limitationCellBreak.FindAllStringIndex(row, -1) {
		if row[loc[0]] == '|' {
			cells = append(cells, strings.TrimSpace(row[last:loc[0]]))
			last = loc[1]
		}
	}
	cells = append(cells, strings.TrimSpace(row[last:]))
	// The leading and trailing pipes leave an empty cell at each end.
	return cells[1 : len(cells)-1]
}

// markedLimitationFuncs returns each function under root whose doc comment
// or body holds a [LIMITATION] comment, mapped to its file. A marker outside
// any function belongs to the next function in the file. A marker after the
// last function belongs to none, and fails the test.
func markedLimitationFuncs(t *testing.T, root string) map[string]string {
	t.Helper()
	marked := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return err
		}
		for _, cg := range f.Comments {
			if !strings.Contains(cg.Text(), "[LIMITATION]") {
				continue
			}
			owner := ""
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if ok && cg.Pos() < fn.End() {
					owner = fn.Name.Name
					break
				}
			}
			if owner == "" {
				t.Errorf("%s: the [LIMITATION] marker is after the last function, so it marks no fixture", fset.Position(cg.Pos()))
				continue
			}
			marked[owner] = path
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return marked
}
