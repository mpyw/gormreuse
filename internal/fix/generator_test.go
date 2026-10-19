package fix

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"golang.org/x/tools/go/ssa"
)

// TestGormQualifier covers the alias-aware qualifier used when emitting the
// Session fix (issue #71, defect 3).
func TestGormQualifier(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		src  string
		want string
	}{
		{"normal", `package p; import "gorm.io/gorm"; var _ = gorm.DB{}`, "gorm."},
		{"aliased", `package p; import g "gorm.io/gorm"; var _ = g.DB{}`, "g."},
		{"dot", `package p; import . "gorm.io/gorm"; var _ = DB{}`, ""},
		{"blank", `package p; import _ "gorm.io/gorm"`, "gorm."},
		{"not imported", `package p; var x int`, "gorm."},
		{"other alias", `package p; import orm "gorm.io/gorm"; var _ = orm.DB{}`, "orm."},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, err := parser.ParseFile(token.NewFileSet(), "x.go", tc.src, 0)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got := gormQualifier(f); got != tc.want {
				t.Errorf("gormQualifier() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestIsFinisher(t *testing.T) {
	t.Parallel()
	finishers := []string{"Find", "First", "Count", "Create", "Save", "Delete", "Exec", "Transaction", "Scan", "Rows"}
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
// Session edit and no reassignment target, instead of a fix at a wrong place.
// Calls through a method value now have their own position (#178); a Phi kept
// as the root of a loop variable swap still has none.
func TestRootWithoutPosition(t *testing.T) {
	t.Parallel()

	g := &Generator{fset: token.NewFileSet(), files: map[*token.File]*ast.File{}}
	root := &ssa.Const{}

	if edits := g.generateSessionEditsForRoot(root.Pos(), root); edits != nil {
		t.Errorf("generateSessionEditsForRoot() = %v, want nil", edits)
	}
	if v := g.findRootVar(root); v != nil {
		t.Errorf("findRootVar() = %v, want nil", v)
	}
}
