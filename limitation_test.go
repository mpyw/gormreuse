package gormreuse_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// limitationKinds are the kinds of known limitation, as the bold label that
// opens each in the Known Limitations sections of design/implementation.md
// and README.md. The two sections must have the same kinds, and the same
// number of table rows for each kind that is a table.
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
//     limitation, and each table has as many rows as the notes' table. The
//     rows are in the same order. Either file may write a table in Markdown
//     or in HTML, folded in <details> or not.
//   - The README's summary table gives each table kind's case count, and the
//     count matches that kind's table.
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
	for _, name := range limitationNamedFixtures(section) {
		if _, ok := marked[name]; !ok {
			t.Errorf("design/implementation.md: Known Limitations names fixture %s, but no fixture function of that name is marked [LIMITATION]", name)
		}
	}

	readme := limitationSection(t, "README.md")
	for _, kind := range limitationKinds {
		notesRows, inNotes := limitationTableRows(t, "design/implementation.md", section, kind)
		readmeRows, inReadme := limitationTableRows(t, "README.md", readme, kind)
		switch {
		case !inNotes || !inReadme:
			t.Errorf("Known Limitations: %s is in design/implementation.md: %t, in README.md: %t; want both", kind, inNotes, inReadme)
		case notesRows != readmeRows:
			t.Errorf("Known Limitations: %s has %d rows in design/implementation.md and %d in README.md; list each case in both", kind, notesRows, readmeRows)
		}
		if readmeRows > 0 {
			name := strings.TrimPrefix(kind, "**")
			m := regexp.MustCompile(`(?m)^\| ` + regexp.QuoteMeta(name) + ` \| (\d+) \|`).FindStringSubmatch(readme)
			switch {
			case m == nil:
				t.Errorf("README.md: Known Limitations: the summary table has no %s row with a case count", name)
			case m[1] != strconv.Itoa(readmeRows):
				t.Errorf("README.md: Known Limitations: the summary table says %s has %s cases, but its table has %d rows", name, m[1], readmeRows)
			}
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
)

// limitationNamedFixtures returns the fixture names in section: the code
// spans of each table row's second cell (the Fixtures column), and those of a
// "Fixtures: a, b." list.
func limitationNamedFixtures(section string) []string {
	var names []string
	for line := range strings.SplitSeq(section, "\n") {
		if cells := strings.Split(line, " | "); strings.HasPrefix(line, "| ") && len(cells) == 3 {
			for _, m := range limitationCodeSpan.FindAllStringSubmatch(cells[1], -1) {
				names = append(names, m[1])
			}
		}
		for _, list := range limitationFixtureList.FindAllStringSubmatch(line, -1) {
			for _, m := range limitationCodeSpan.FindAllStringSubmatch(list[1], -1) {
				names = append(names, m[1])
			}
		}
	}
	return names
}

// limitationTableRows returns the number of body rows in the table that
// follows the paragraph starting with kind, and whether that paragraph is
// there. A kind without a table has 0 rows. The table may be a Markdown
// table or an HTML <table>, whose body rows are the <tr> lines without a
// <th>. An HTML table without its closing </table> fails the test.
func limitationTableRows(t *testing.T, path, section, kind string) (rows int, found bool) {
	t.Helper()
	i := strings.Index(section, "\n"+kind)
	if i < 0 {
		return 0, false
	}
	lines := strings.Split(section[i+1:], "\n")[1:]
	for len(lines) > 0 && lines[0] == "" {
		lines = lines[1:]
	}
	if len(lines) > 0 && lines[0] == "<table>" {
		for _, line := range lines[1:] {
			switch {
			case line == "</table>":
				return rows, true
			case strings.HasPrefix(line, "<tr>") && !strings.Contains(line, "<th>"):
				rows++
			}
		}
		t.Errorf("%s: Known Limitations: the %s table has no </table>", path, kind)
		return rows, true
	}
	for j, line := range lines {
		if !strings.HasPrefix(line, "|") {
			break
		}
		if j >= 2 { // the header row and the delimiter row
			rows++
		}
	}
	return rows, true
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
