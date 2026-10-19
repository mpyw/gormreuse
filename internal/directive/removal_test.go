package directive

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strings"
	"testing"

	"golang.org/x/tools/go/analysis"
)

// TestRemovalEdits covers how a fix rewrites a directive comment (#248),
// including the comments it leaves alone. Each source marks the comment to
// edit as the first one that starts with "//gormreuse:".
func TestRemovalEdits(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		src    string
		remove []string
		want   string // The source after the fix, or "" for no fix
	}{
		{"one of a list", "package p\n\n//gormreuse:pure,ignore\nfunc f() {}\n", []string{"ignore"}, "package p\n\n//gormreuse:pure\nfunc f() {}\n"},
		{"whole line", "package p\n\n//gormreuse:ignore\nfunc f() {}\n", []string{"ignore"}, "package p\n\nfunc f() {}\n"},
		{"indented line", "package p\n\nfunc f() {\n\t//gormreuse:ignore\n\t_ = 1\n}\n", []string{"ignore"}, "package p\n\nfunc f() {\n\t_ = 1\n}\n"},
		{"after code", "package p\n\nfunc f() {\n\t_ = 1 \t//gormreuse:ignore\n}\n", []string{"ignore"}, "package p\n\nfunc f() {\n\t_ = 1\n}\n"},
		{"last line", "package p\n\nvar x = 1\n\n//gormreuse:ignore", []string{"ignore"}, "package p\n\nvar x = 1\n\n"},
		{"reason kept", "package p\n\n//gormreuse:ignore // why\nfunc f() {}\n", []string{"ignore"}, "package p\n\n// why\nfunc f() {}\n"},
		{"reason kept in list", "package p\n\n//gormreuse:pure,ignore  //why\nfunc f() {}\n", []string{"ignore"}, "package p\n\n//gormreuse:pure  //why\nfunc f() {}\n"},
		{"two parts", "package p\n\n//gormreuse:pure,immutable-return\nfunc f() {}\n", []string{"pure", "immutable-return"}, "package p\n\nfunc f() {}\n"},
		{"reason addressed to gormreuse", "package p\n\n//gormreuse:ignore //gormreuse:pure\nfunc f() {}\n", []string{"ignore"}, ""},
		{"part not in the list", "package p\n\n//gormreuse:pure\nfunc f() {}\n", []string{"ignore"}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pass, comment := removalTestPass(t, tc.src)
			r := NewRemovals(pass)
			var targets []RemovalTarget
			for _, part := range tc.remove {
				targets = append(targets, RemovalTarget{Comment: comment, Part: part})
			}
			got := removalTestApply(t, r, pass, tc.src, comment, targets)
			if got != tc.want {
				t.Errorf("fixed source = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRemovalWithoutSource covers the fixes withheld when the file cannot be
// read as parsed, and a diagnostic reported with no target.
func TestRemovalWithoutSource(t *testing.T) {
	t.Parallel()
	const src = "package p\n\n//gormreuse:ignore\nfunc f() {}\n"
	tests := []struct {
		name     string
		readFile func(string) ([]byte, error)
		targets  bool
	}{
		{"no reader", nil, true},
		{"read error", func(string) ([]byte, error) { return nil, errors.New("gone") }, true},
		{"changed on disk", func(string) ([]byte, error) { return []byte(src + "\n"), nil }, true},
		{"no target", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pass, comment := removalTestPass(t, src)
			pass.ReadFile = tc.readFile
			var targets []RemovalTarget
			if tc.targets {
				targets = []RemovalTarget{{Comment: comment, Part: "ignore"}}
			}
			if got := removalTestApply(t, NewRemovals(pass), pass, src, comment, targets); got != "" {
				t.Errorf("fixed source = %q, want no fix", got)
			}
		})
	}
}

// TestRemovalUnknownComment covers a target that is not a directive comment
// of the pass's files.
func TestRemovalUnknownComment(t *testing.T) {
	t.Parallel()
	const src = "package p\n\n// a note\nfunc f() {}\n"
	pass, _ := removalTestPass(t, src)
	file := pass.Files[0]
	note := file.Comments[0].List[0].Pos()
	for _, pos := range []token.Pos{note, file.Name.Pos()} {
		if got := removalTestApply(t, NewRemovals(pass), pass, src, pos, []RemovalTarget{{Comment: pos, Part: "ignore"}}); got != "" {
			t.Errorf("fixed source = %q, want no fix", got)
		}
	}
}

// removalTestPass parses src into a pass whose ReadFile returns src, and
// returns the position of its first directive comment.
func removalTestPass(t *testing.T, src string) (*analysis.Pass, token.Pos) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "p.go", src, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	pass := &analysis.Pass{
		Fset:     fset,
		Files:    []*ast.File{file},
		ReadFile: func(string) ([]byte, error) { return []byte(src), nil },
	}
	for _, cg := range file.Comments {
		for _, c := range cg.List {
			if strings.HasPrefix(c.Text, "//gormreuse:") {
				return pass, c.Pos()
			}
		}
	}
	return pass, token.NoPos
}

// removalTestApply reports one diagnostic with targets through r and returns
// src with its fix applied, or "" when it has none.
func removalTestApply(t *testing.T, r *Removals, pass *analysis.Pass, src string, pos token.Pos, targets []RemovalTarget) string {
	t.Helper()
	var diags []analysis.Diagnostic
	pass.Report = func(d analysis.Diagnostic) { diags = append(diags, d) }
	r.Report(pos, "unused", targets...)
	r.Flush()
	if len(diags) != 1 {
		t.Fatalf("got %d diagnostics, want 1", len(diags))
	}
	if len(diags[0].SuggestedFixes) == 0 {
		return ""
	}
	tf := pass.Fset.File(pass.Files[0].Pos())
	out := src
	for _, e := range slices.Backward(diags[0].SuggestedFixes[0].TextEdits) {
		out = out[:tf.Offset(e.Pos)] + string(e.NewText) + out[tf.Offset(e.End):]
	}
	return out
}
