package fix

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"strings"
	"testing"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/ssa"
)

// TestGormNameAt covers the name the Session fix uses for gorm.io/gorm where
// it inserts a Session: an aliased or dot import (issue #71, defect 3), and a
// name that is blank, shadowed or taken by another package (#200). Each
// source marks the insertion point with "HERE".
func TestGormNameAt(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		src           string
		wantQualifier string
		wantImport    string
	}{
		{"normal", `package p; import "gorm.io/gorm"; var _ = gorm.DB{}; func f() { _ = "HERE" }`, "gorm.", ""},
		{"aliased", `package p; import g "gorm.io/gorm"; var _ = g.DB{}; func f() { _ = "HERE" }`, "g.", ""},
		{"dot", `package p; import . "gorm.io/gorm"; var _ = DB{}; func f() { _ = "HERE" }`, "", ""},
		{"dot shadowed", `package p; import . "gorm.io/gorm"; var _ = DB{}; func f(Session int) { _ = "HERE" }`, "gorm.", "gorm"},
		{"blank", `package p; import _ "gorm.io/gorm"; func f() { _ = "HERE" }`, "gorm.", "gorm"},
		{"not imported", `package p; func f() { _ = "HERE" }`, "gorm.", "gorm"},
		{"shadowed", `package p; import "gorm.io/gorm"; var _ = gorm.DB{}; func f(gorm int) { _ = "HERE" }`, "gormio.", "gormio"},
		{"other package", `package p; import gorm "other"; var _ = gorm.X; func f() { _ = "HERE" }`, "gormio.", "gormio"},
		{"both names taken", `package p; import gorm "other"; var gormio int; func f() { _ = "HERE"; _ = gorm.X }`, "gormio2.", "gormio2"},
		{"declared later", `package p; import "gorm.io/gorm"; var _ = gorm.DB{}; func f() { _ = "HERE"; gorm := 1; _ = gorm }`, "gorm.", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g, file, pos := checkGormNameSource(t, tc.src)
			qualifier, importName := g.gormNameAt(file, []token.Pos{pos})
			if qualifier != tc.wantQualifier || importName != tc.wantImport {
				t.Errorf("gormNameAt() = (%q, %q), want (%q, %q)", qualifier, importName, tc.wantQualifier, tc.wantImport)
			}
		})
	}
}

// checkGormNameSource type-checks src against stubs of gorm.io/gorm and of a
// package "other", and returns a Generator for it, the file, and the position
// of "HERE".
func checkGormNameSource(t *testing.T, src string) (*Generator, *ast.File, token.Pos) {
	t.Helper()
	fset := token.NewFileSet()
	stubs := map[string]string{
		"gorm.io/gorm": `package gorm; type DB struct{}; type Session struct{}`,
		"other":        `package other; var X int`,
	}
	imported := make(map[string]*types.Package)
	for path, stub := range stubs {
		f, err := parser.ParseFile(fset, path+".go", stub, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		pkg, err := new(types.Config).Check(path, fset, []*ast.File{f}, nil)
		if err != nil {
			t.Fatalf("check %s: %v", path, err)
		}
		imported[path] = pkg
	}
	file, err := parser.ParseFile(fset, "x.go", src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	info := &types.Info{
		Defs:      make(map[*ast.Ident]types.Object),
		Uses:      make(map[*ast.Ident]types.Object),
		Implicits: make(map[ast.Node]types.Object),
		Scopes:    make(map[ast.Node]*types.Scope),
	}
	conf := types.Config{Importer: importerFunc(func(path string) (*types.Package, error) { return imported[path], nil })}
	pkg, err := conf.Check("p", fset, []*ast.File{file}, info)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	pos := file.FileStart + token.Pos(strings.Index(src, `"HERE"`))
	return &Generator{pass: &analysis.Pass{Pkg: pkg, TypesInfo: info}}, file, pos
}

type importerFunc func(path string) (*types.Package, error)

func (f importerFunc) Import(path string) (*types.Package, error) { return f(path) }

func TestIsFinisher(t *testing.T) {
	t.Parallel()
	finishers := []string{"Find", "First", "Count", "Create", "Save", "Delete", "Exec", "Transaction", "Scan", "Rows", "Commit", "Rollback", "SavePoint", "RollbackTo", "FindInBatches", "CreateInBatches", "UpdateColumn", "UpdateColumns"}
	for _, m := range finishers {
		if !isFinisher(m) {
			t.Errorf("%q should be a finisher", m)
		}
	}
	nonFinishers := []string{"Where", "Order", "Limit", "Session", "WithContext", "Preload", "Scopes", ""}
	for _, m := range nonFinishers {
		if isFinisher(m) {
			t.Errorf("%q should not be a finisher", m)
		}
	}
}

// TestRootWithoutPosition checks that a root with no source position gets no
// Session position and no reassignment target, instead of a fix at a wrong place.
// Calls through a method value now have their own position (#178); a Phi kept
// as the root of a loop variable swap still has none.
func TestRootWithoutPosition(t *testing.T) {
	t.Parallel()

	g := &Generator{fset: token.NewFileSet(), files: map[*token.File]*ast.File{}}
	root := &ssa.Const{}

	if positions := g.sessionPositionsForRoot(root.Pos(), root, nil); positions != nil {
		t.Errorf("sessionPositionsForRoot() = %v, want nil", positions)
	}
	if v := g.findRootVar(root); v != nil {
		t.Errorf("findRootVar() = %v, want nil", v)
	}
	// A Phi root whose result is used once needs no Session on its edges.
	if positions := g.sessionPositionsForRoot(token.NoPos, &ssa.Phi{}, nil); positions != nil {
		t.Errorf("sessionPositionsForRoot(Phi) = %v, want nil", positions)
	}
}
