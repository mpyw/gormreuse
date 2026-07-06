package fix

import (
	"go/parser"
	"go/token"
	"testing"
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
		tc := tc
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

// TestExtractAssignableLHS covers extractAssignableLHSImpl, which reconstructs
// the assignable left-hand side of an expression (used when rewriting a reused
// chain into `x = x.Where(...)`). It is a pure AST walk and uses no Generator
// state, so a zero-value Generator suffices.
func TestExtractAssignableLHS(t *testing.T) {
	t.Parallel()
	g := &Generator{}

	tests := []struct {
		expr string
		want string
	}{
		{"q", "q"},                     // Ident
		{"obj.field", "obj.field"},     // SelectorExpr
		{"a.b.c", "a.b.c"},             // nested SelectorExpr
		{"ptr", "ptr"},                 // Ident
		{"*ptr", "*ptr"},               // StarExpr
		{"*a.b", "*a.b"},               // StarExpr over selector
		{"(x)", "x"},                   // ParenExpr unwraps
		{"*(obj.field)", "*obj.field"}, // Paren + Star + Selector
		{"arr[i]", ""},                 // IndexExpr — deliberately skipped
		{"f()", ""},                    // CallExpr — not assignable
		{"x[1:2]", ""},                 // SliceExpr — not assignable
		{"x.(T)", ""},                  // TypeAssertExpr — not assignable
		{"42", ""},                     // BasicLit
		{"T{}", ""},                    // CompositeLit
		{"a + b", ""},                  // default (BinaryExpr)
		{"obj.f()", ""},                // selector on a call is a call → ""
		{"f().x", ""},                  // SelectorExpr with non-assignable base
		{"*f()", ""},                   // StarExpr with non-assignable base
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.expr, func(t *testing.T) {
			t.Parallel()
			expr, err := parser.ParseExpr(tc.expr)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.expr, err)
			}
			if got := g.extractAssignableLHS(expr); got != tc.want {
				t.Errorf("extractAssignableLHS(%q) = %q, want %q", tc.expr, got, tc.want)
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
