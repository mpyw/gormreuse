package gormreuse_test

import (
	"fmt"
	"go/scanner"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// docsExternalNames are the names in code spans of design/implementation.md
// that are not in this repository's Go code: gorm internals, functions of
// gorm's own repository, and placeholders.
var docsExternalNames = map[string]bool{
	// gorm's Statement fields and internal functions.
	"BuildCondition": true, "Dest": true, "Omits": true, "Selects": true, "executeScopes": true,
	"BeginTransaction": true, "CommitOrRollbackTransaction": true, "getInstance": true,
	"AddVar": true, "Settings": true, "joins": true,
	// The standard library.
	"Pool": true,
	// Functions of gorm's repository that the measurements name.
	"GetQueryAndExecTx": true, "OpenTestConnection": true, "SaveAfterAssociations": true, "SetupUpdateReflectValue": true,
	// goreleaser configuration keys.
	"changelog": true, "groups": true,
	// Placeholders in examples.
	"G": true, "GenF": true, "Reset": true, "X": true, "Y": true, "Z": true, "vX": true, "vLAST": true,
	"ci": true, "cj": true, "svc": true,
	// SSA value names in examples.
	"t0": true,
}

// docsExternalFiles are the file names in code spans of
// design/implementation.md that are not in this repository.
var docsExternalFiles = map[string]bool{
	"_cgo_gotypes.go": true, // written by cmd/cgo
	"_test.go":        true, // a file name suffix
	".custom-gcl.yml": true, // a user's golangci-lint build file
	"checksums.txt":   true, // a release asset
	"X.go":            true, // placeholder fixture
	"X.go.golden":     true,
	"X.go.diff":       true,
}

var (
	docsCodeSpan   = regexp.MustCompile("`([^`]+)`")
	docsGoIdent    = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)
	docsFileName   = regexp.MustCompile(`^[\w./-]+\.(go|golden|diff|yml|yaml|sh|md|toml|txt)$`)
	docsIdentChain = regexp.MustCompile(`^\(?\*?([A-Za-z_][\w$]*(?:\.[A-Za-z_][\w$]*)*)\)?(?:\(\))?$`)
	// docsMixedCase matches a word that can only be a Go name: one with a
	// capital after its first letter, such as RunSSA or readDirective.
	docsMixedCase = regexp.MustCompile(`\b[A-Za-z][a-z0-9]+[A-Z][A-Za-z0-9]*\b`)
	docsTreeEntry = regexp.MustCompile(`^((?:│   |    )*)(?:├── |└── )(\S+)\s*(?:#\s*(.*))?$`)
)

// TestImplementationNotesNamesExist fails when a code span in
// design/implementation.md names an identifier or a file that does not exist
// in the repository, so a rename cannot leave the notes behind. A name is
// checked against the identifiers in the code of the repository's Go files.
// A name that only a comment mentions does not count, since a comment can be
// as stale as the notes. String literals count: the analyzer matches gorm's
// names, such as Session fields, by string. So the test catches names that
// are gone, not names that moved. The fenced architecture tree is checked by
// TestImplementationNotesArchitectureTree.
func TestImplementationNotesNamesExist(t *testing.T) {
	doc, err := os.ReadFile("design/implementation.md")
	if err != nil {
		t.Fatal(err)
	}
	idents, files := docsRepositoryNames(t)
	inFence := false
	for i, line := range strings.Split(string(doc), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		for _, m := range docsCodeSpan.FindAllStringSubmatch(line, -1) {
			span := m[1]
			if docsFileName.MatchString(span) {
				if !docsExternalFiles[filepath.Base(span)] && !docsFileExists(files, span) {
					t.Errorf("design/implementation.md:%d: file %s does not exist", i+1, span)
				}
				continue
			}
			chain := docsIdentChain.FindStringSubmatch(span)
			if chain == nil {
				continue
			}
			for part := range strings.SplitSeq(chain[1], ".") {
				part, _, _ = strings.Cut(part, "$") // m$bound, m$thunk
				if part != "" && !idents[part] && !docsExternalNames[part] && !token.IsKeyword(part) {
					t.Errorf("design/implementation.md:%d: %s names %s, which is not in the code", i+1, span, part)
				}
			}
		}
	}
}

// TestImplementationNotesArchitectureTree checks the file tree in the
// Architecture section of design/implementation.md in both directions:
//
//   - Every entry exists, and every Go name its comment gives (a mixed-case
//     word, such as RunSSA) is in the code.
//   - A directory whose files the tree lists has all of its files listed: the
//     Go files at the repository root, test files included, and the non-test
//     Go files and the subdirectories with Go files elsewhere. A glob such as
//     *_test.go covers the files it matches.
func TestImplementationNotesArchitectureTree(t *testing.T) {
	doc, err := os.ReadFile("design/implementation.md")
	if err != nil {
		t.Fatal(err)
	}
	_, section, ok := strings.Cut(string(doc), "\n## Architecture\n")
	if !ok {
		t.Fatal("design/implementation.md has no ## Architecture section")
	}
	_, tree, ok := strings.Cut(section, "```\n")
	if !ok {
		t.Fatal("the Architecture section has no fenced tree")
	}
	tree, _, _ = strings.Cut(tree, "```")

	idents, _ := docsRepositoryNames(t)
	listed := map[string][]string{} // directory -> the names listed in it
	var stack []string              // the directory at each depth
	lines := strings.Split(tree, "\n")
	if len(lines) == 0 || lines[0] != "gormreuse/" {
		t.Fatalf("the tree does not start at gormreuse/: %q", lines[0])
	}
	for _, line := range lines[1:] {
		m := docsTreeEntry.FindStringSubmatch(line)
		if m == nil {
			if strings.Trim(line, "│ ") != "" {
				t.Errorf("tree line %q is not an entry", line)
			}
			continue
		}
		depth := len([]rune(m[1])) / 4
		if depth > len(stack) {
			t.Errorf("tree line %q is nested under no directory", line)
			continue
		}
		stack = stack[:depth]
		entry := strings.TrimSuffix(m[2], "/")
		full := path.Join(append(slices.Clone(stack), entry)...)
		parent := path.Dir(full)
		listed[parent] = append(listed[parent], path.Base(full))
		// facts/facts.go lists facts.go in facts.
		for dir := path.Dir(full); dir != "." && dir != path.Join(stack...); dir = path.Dir(dir) {
			listed[path.Dir(dir)] = append(listed[path.Dir(dir)], path.Base(dir))
		}
		if strings.HasSuffix(m[2], "/") {
			stack = append(stack, entry)
		}

		if matches, _ := filepath.Glob(filepath.FromSlash(full)); len(matches) == 0 {
			t.Errorf("tree entry %s does not exist", full)
		}
		for _, name := range docsMixedCase.FindAllString(m[3], -1) {
			if !idents[name] && !docsExternalNames[name] {
				t.Errorf("tree entry %s: its comment names %s, which is not in the code", full, name)
			}
		}
	}

	for dir, names := range listed {
		entries, err := os.ReadDir(filepath.FromSlash(dir))
		if err != nil {
			continue // a glob, reported above if it matches nothing
		}
		for _, e := range entries {
			name := e.Name()
			switch {
			case e.IsDir() && !docsHasGoFiles(t, filepath.Join(dir, name)):
				continue
			case !e.IsDir() && (!strings.HasSuffix(name, ".go") || (dir != "." && strings.HasSuffix(name, "_test.go"))):
				continue
			}
			if !slices.ContainsFunc(names, func(pattern string) bool {
				ok, _ := path.Match(pattern, name)
				return ok
			}) {
				t.Errorf("the Architecture tree lists %s/ but not %s in it", dir, name)
			}
		}
	}
}

// docsHasGoFiles reports whether dir or a directory under it holds a Go file.
func docsHasGoFiles(t *testing.T, dir string) bool {
	t.Helper()
	found := false
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(p, ".go") {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// docsRepositoryNames returns every identifier in the code of the
// repository's Go files, and every file path relative to the repository root.
// Comments are skipped. Words in string literals count, since the analyzer
// matches some of gorm's names by string.
func docsRepositoryNames(t *testing.T) (idents map[string]bool, files []string) {
	t.Helper()
	idents = map[string]bool{}
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != "." && strings.HasPrefix(d.Name(), ".") && d.Name() != ".github" {
				return filepath.SkipDir
			}
			return nil
		}
		files = append(files, filepath.ToSlash(path))
		if strings.HasSuffix(path, ".go") {
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			docsCodeIdents(src, idents)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return idents, files
}

// docsCodeIdents adds to idents the identifiers of src outside comments, and
// the words of its string literals. It needs no valid Go: a fixture may be
// malformed on purpose.
func docsCodeIdents(src []byte, idents map[string]bool) {
	fset := token.NewFileSet()
	var s scanner.Scanner
	s.Init(fset.AddFile("", fset.Base(), len(src)), src, func(token.Position, string) {}, 0)
	for {
		_, tok, lit := s.Scan()
		switch tok {
		case token.EOF:
			return
		case token.IDENT:
			idents[lit] = true
		case token.STRING:
			for _, id := range docsGoIdent.FindAllString(lit, -1) {
				idents[id] = true
			}
		}
	}
}

// docsFileExists reports whether name is the path, or the trailing part of
// the path, of a file in files. The notes often name a file relative to its
// package directory.
func docsFileExists(files []string, name string) bool {
	for _, f := range files {
		if f == name || strings.HasSuffix(f, "/"+name) {
			return true
		}
	}
	return false
}

// TestReadmeVersionPinsAgree fails when the version pins in README.md name
// different versions, or a version other than the latest release, so a
// release cannot miss one and the pins cannot name an unreleased version (see
// Releasing in design/implementation.md). The latest release is the newest
// v* tag. While a release is being prepared, the newest dated CHANGELOG.md
// section names a newer version than the tag, and the pins must name that.
//
// A clone without tags, such as a shallow clone or a source tarball, checks
// only that the pins agree, and skips the comparison with the release. In CI
// (CI or GITHUB_ACTIONS set) a missing tag fails the test instead, so it
// cannot pass there without the check.
func TestReadmeVersionPinsAgree(t *testing.T) {
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	pins := map[string]*regexp.Regexp{
		"mise use":        regexp.MustCompile(`mise use "github:mpyw/gormreuse@(\d+\.\d+\.\d+)"`),
		"mise.toml":       regexp.MustCompile(`"github:mpyw/gormreuse" = "(\d+\.\d+\.\d+)"`),
		"pinned tag":      regexp.MustCompile("`@v(\\d+\\.\\d+\\.\\d+)`"),
		"tarball VERSION": regexp.MustCompile(`export VERSION=(\d+\.\d+\.\d+)`),
		".custom-gcl.yml": regexp.MustCompile(`(?m)^\s+version: v(\d+\.\d+\.\d+)$`),
	}
	versions := map[string]string{}
	for name, re := range pins {
		m := re.FindAllStringSubmatch(string(readme), -1)
		if len(m) != 1 {
			t.Errorf("README.md: want one %s pin, found %d", name, len(m))
			continue
		}
		versions[name] = m[0][1]
	}
	for name, v := range versions {
		if v != versions["mise use"] {
			t.Errorf("README.md: the %s pin is %s, but the mise use pin is %s", name, v, versions["mise use"])
		}
	}

	want := docsReleasedVersion(t)
	for _, name := range slices.Sorted(maps.Keys(versions)) {
		if v := versions[name]; v != want {
			t.Errorf("README.md: the %s pin is %s, want %s, the latest release", name, v, want)
		}
	}
}

// docsNoReleaseTag fails the test in CI and skips it elsewhere: a clone
// without tags cannot tell the latest release.
func docsNoReleaseTag(t *testing.T, reason string) {
	t.Helper()
	msg := reason + "; fetch the tags with git fetch --tags to compare the README pins with the latest release"
	if os.Getenv("CI") != "" || os.Getenv("GITHUB_ACTIONS") != "" {
		t.Fatal(msg)
	}
	t.Skip(msg)
}

// docsReleasedVersion returns the version the README pins must name: the
// newest v* tag, or the version of the newest dated CHANGELOG.md section when
// that is newer (a release being prepared).
func docsReleasedVersion(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "tag", "--list", "v*").Output()
	if err != nil {
		docsNoReleaseTag(t, fmt.Sprintf("git tag: %v", err))
	}
	var latest string
	for tag := range strings.FieldsSeq(string(out)) {
		v := strings.TrimPrefix(tag, "v")
		if docsSemver(v) != nil && (latest == "" || docsNewer(v, latest)) {
			latest = v
		}
	}
	if latest == "" {
		docsNoReleaseTag(t, "no v* release tag found")
	}
	changelog, err := os.ReadFile("CHANGELOG.md")
	if err != nil {
		t.Fatal(err)
	}
	if m := regexp.MustCompile(`(?m)^## (\d+\.\d+\.\d+) \(\d{4}-\d{2}-\d{2}\)$`).FindSubmatch(changelog); m != nil {
		if v := string(m[1]); docsNewer(v, latest) {
			return v
		}
	}
	return latest
}

// docsSemver returns the three numbers of a version such as 0.22.0, or nil.
func docsSemver(v string) []int {
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return nil
	}
	nums := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil
		}
		nums[i] = n
	}
	return nums
}

// docsNewer reports whether version a is newer than version b.
func docsNewer(a, b string) bool {
	return slices.Compare(docsSemver(a), docsSemver(b)) > 0
}
