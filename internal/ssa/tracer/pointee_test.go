package tracer

import (
	"strings"
	"testing"

	"github.com/mpyw/gormreuse/internal/ssa/callsite"
	"github.com/mpyw/gormreuse/internal/typeutil"
)

const pointeeSource = `package shape

import "gorm.io/gorm"

func scanInto(p **gorm.DB, db *gorm.DB) { *p = db.Where("x") }

func rebind(db *gorm.DB) {
	var q *gorm.DB
	scanInto(&q, db)
	q.Find(nil)
}

func rebindThenStore(db *gorm.DB) {
	var q *gorm.DB
	scanInto(&q, db)
	q = db.Where("y")
	q.Find(nil)
}
`

// TestPointeeRoot pins the PointeeRoot of a call that takes a variable's
// address (#345): the read after the call sees it, a store after the call
// hides it, and it describes the call and the variable.
func TestPointeeRoot(t *testing.T) {
	t.Parallel()
	funcs := loadFixtureSource(t, pointeeSource)
	tr := NewRootTracer(nil, nil, nil, nil, nil, nil, callsite.FuncVars{})

	fn := funcs["rebind"]
	root, ok := tr.FindMutableRoot(fixtureGormCallReceiver(fn, "Find"), nil).(*PointeeRoot)
	if !ok {
		t.Fatalf("the read after scanInto(&q, db) should be a *PointeeRoot")
	}
	if root.Call.Common().StaticCallee() != funcs["scanInto"] {
		t.Errorf("Call = %v, want the scanInto call", root.Call)
	}
	if got := root.Name(); got != "*"+root.Var.Name()+"@"+root.Call.Name() {
		t.Errorf("Name() = %q", got)
	}
	if got := root.String(); !strings.HasPrefix(got, "*q after ") {
		t.Errorf("String() = %q", got)
	}
	if !typeutil.IsGormDB(root.Type()) {
		t.Errorf("Type() = %v, want *gorm.DB", root.Type())
	}
	if root.Parent() != fn || root.Referrers() != nil || root.Pos() != root.Call.Pos() || RootBlock(root) != root.Call.Block() {
		t.Errorf("Parent, Referrers, Pos or Block does not describe the call")
	}
	if again := tr.FindAllMutableRoots(fixtureGormCallReceiver(fn, "Find"), nil); len(again) != 1 || again[0] != root {
		t.Errorf("FindAllMutableRoots = %v, want the one interned root", again)
	}

	stored := tr.FindAllMutableRoots(fixtureGormCallReceiver(funcs["rebindThenStore"], "Find"), nil)
	for _, r := range stored {
		if _, ok := r.(*PointeeRoot); ok {
			t.Errorf("a store after the call should hide its PointeeRoot, got %v", stored)
		}
	}

	if roots := tr.PointeeRoots(fn.Params[0], fn.Blocks[0].Instrs[0], nil); roots != nil {
		t.Errorf("PointeeRoots of a non-variable = %v, want nil", roots)
	}
}
