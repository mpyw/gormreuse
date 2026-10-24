package stmtwrite

import (
	"go/token"
	"go/types"
	"testing"
)

// TestKindOfClausePointer checks that a pointer to a value of gorm's clause
// package, as Statement.TableExpr, is followed as the query's memory. The
// gorm stub of the analyzer tests does not import the clause package, so
// this is checked on the types directly.
func TestKindOfClausePointer(t *testing.T) {
	named := func(path, name string) *types.Named {
		pkg := types.NewPackage(path, "p")
		obj := types.NewTypeName(token.NoPos, pkg, name, nil)
		return types.NewNamed(obj, types.NewStruct(nil, nil), nil)
	}
	for _, tt := range []struct {
		name string
		t    types.Type
		want bool
	}{
		{"*clause.Expr", types.NewPointer(named(clausePkg, "Expr")), true},
		{"*schema.Schema", types.NewPointer(named("gorm.io/gorm/schema", "Schema")), false},
		{"*int", types.NewPointer(types.Typ[types.Int]), false},
	} {
		k, ok := kindOf(tt.t)
		if ok != tt.want || ok && k != addr {
			t.Errorf("kindOf(%s) = %v, %v; want addr, %v", tt.name, k, ok, tt.want)
		}
	}
}
