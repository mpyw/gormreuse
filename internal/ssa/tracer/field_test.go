package tracer

import (
	"strings"
	"testing"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/ssa/callsite"
	"github.com/mpyw/gormreuse/internal/ssa/cfg"
	"github.com/mpyw/gormreuse/internal/typeutil"
)

// TestFieldRoot pins the FieldRoot of a field of a struct a call returned
// (#181): every read of the same field of the same call yields one interned
// root, and different fields yield different roots.
func TestFieldRoot(t *testing.T) {
	t.Parallel()
	fixtures, _ := loadFixtureProgram(t)
	loops := cfg.NewAnalyzer()
	tr := NewRootTracer(nil, nil, nil, nil, nil, nil, callsite.FuncVars{})

	whereRoots := func(name string) []ssa.Value {
		fn := fixtures[name]
		if fn == nil {
			t.Fatalf("%s fixture missing", name)
		}
		var roots []ssa.Value
		for _, block := range fn.Blocks {
			for _, instr := range block.Instrs {
				call, ok := instr.(*ssa.Call)
				if !ok || call.Common().StaticCallee() == nil || call.Common().StaticCallee().Name() != "Where" {
					continue
				}
				roots = append(roots, tr.FindMutableRoot(call.Call.Args[0], loops.DetectLoops(fn)))
			}
		}
		if len(roots) != 2 {
			t.Fatalf("%s: expected 2 Where calls, got %d", name, len(roots))
		}
		return roots
	}

	same := whereRoots("nestedFieldOfReturnedStruct")
	root, ok := same[0].(*FieldRoot)
	if !ok {
		t.Fatalf("expected a *FieldRoot, got %v (%T)", same[0], same[0])
	}
	if same[1] != same[0] {
		t.Errorf("two reads of one field should share a root: %v, %v", same[0], same[1])
	}
	if got := root.Name(); got != root.Call.Name()+".inner.DB" {
		t.Errorf("Name() = %q", got)
	}
	if got := root.String(); !strings.HasSuffix(got, "makeSrWrapDB().inner.DB") {
		t.Errorf("String() = %q", got)
	}
	if !typeutil.IsGormDB(root.Type()) {
		t.Errorf("Type() = %v, want *gorm.DB", root.Type())
	}
	if root.Parent() != fixtures["nestedFieldOfReturnedStruct"] || root.Referrers() != nil || root.Pos() != root.Call.Pos() {
		t.Errorf("Parent/Referrers/Pos do not describe the call: %v %v %v", root.Parent(), root.Referrers(), root.Pos())
	}

	distinct := whereRoots("nestedFieldsOfReturnedStructDistinct")
	if distinct[0] == nil || distinct[1] == nil || distinct[0] == distinct[1] {
		t.Errorf("two fields should have different roots: %v, %v", distinct[0], distinct[1])
	}

	// A field reached through a pointer the call returned (#250).
	ptr := whereRoots("pointerStructNested")
	ptrRoot, ok := ptr[0].(*FieldRoot)
	if !ok || ptr[1] != ptr[0] {
		t.Fatalf("expected one shared *FieldRoot, got %v (%T), %v", ptr[0], ptr[0], ptr[1])
	}
	if got := ptrRoot.Name(); got != ptrRoot.Call.Name()+".inner.DB" {
		t.Errorf("Name() = %q", got)
	}
	if !typeutil.IsGormDB(ptrRoot.Type()) {
		t.Errorf("Type() = %v, want *gorm.DB", ptrRoot.Type())
	}
}
