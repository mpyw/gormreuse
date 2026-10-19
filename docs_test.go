package gormreuse_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// docsExternalNames are the names in code spans of design/implementation.md
// that are not in this repository's Go code: gorm internals, functions of
// gorm's own repository, and placeholders.
var docsExternalNames = map[string]bool{
	// gorm's Statement fields and internal functions.
	"BuildCondition": true, "Dest": true, "Omits": true, "Selects": true, "executeScopes": true,
	// Functions of gorm's repository that the measurements name.
	"GetQueryAndExecTx": true, "OpenTestConnection": true, "SaveAfterAssociations": true, "SetupUpdateReflectValue": true,
	// goreleaser configuration keys.
	"changelog": true, "groups": true,
	// Placeholders in examples.
	"G": true, "GenF": true, "Reset": true, "X": true, "Y": true, "Z": true, "vX": true, "vLAST": true,
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
)

// TestImplementationNotesNamesExist fails when a code span in
// design/implementation.md names an identifier or a file that does not exist
// in the repository, so a rename cannot leave the notes behind. A name is
// checked against every identifier in the repository's Go files, comments
// included, so it catches names that are gone, not names that moved.
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
				if part != "" && !idents[part] && !docsExternalNames[part] {
					t.Errorf("design/implementation.md:%d: %s names %s, which is not in the code", i+1, span, part)
				}
			}
		}
	}
}

// docsRepositoryNames returns every identifier in the repository's Go files,
// and every file path relative to the repository root.
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
			for _, id := range docsGoIdent.FindAllString(string(src), -1) {
				idents[id] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return idents, files
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
// different versions, so a release that bumps them cannot miss one (see
// Releasing in design/implementation.md).
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
}
