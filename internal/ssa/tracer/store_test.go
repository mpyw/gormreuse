package tracer

import (
	"testing"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/ssa/callsite"
	"github.com/mpyw/gormreuse/internal/ssa/cfg"
)

// TestFindMutableRootThroughStructField pins the field-path matching in
// FieldStoredValues. The SSA builder stages a composite literal in a separate
// temporary and copies the finished aggregate into the destination, and it emits
// a fresh FieldAddr chain for every access, so a field read shares neither the
// store's base value nor its address chain. Matching the whole field path (and
// following the copy) is what keeps the value stored under a promoted field key
// (Go 1.27) reachable.
func TestFindMutableRootThroughStructField(t *testing.T) {
	t.Parallel()
	fixtures, _ := loadFixtureProgram(t)
	loops := cfg.NewAnalyzer()
	tr := NewRootTracer(nil, nil, nil, nil, nil, nil, callsite.FuncVars{})

	for _, name := range []string{
		"go127PromotedFieldLiteral",
		"go127NestedFieldLiteral",
		"go127PromotedPointerLiteral",
		"go127PromotedFieldAssign",
		"multiStructField",
	} {
		fn := fixtures[name]
		if fn == nil {
			t.Fatalf("%s fixture missing", name)
		}
		recv := fixtureGormCallReceiver(fn, "Count")
		if recv == nil {
			t.Fatalf("%s: no Count call found", name)
		}
		root := tr.FindMutableRoot(recv, loops.DetectLoops(fn))
		call, ok := root.(*ssa.Call)
		if !ok {
			t.Errorf("%s: expected the Where call as mutable root, got %v (%T)", name, root, root)
			continue
		}
		if callee := call.Common().StaticCallee(); callee == nil || callee.Name() != "Where" {
			t.Errorf("%s: expected the Where call as mutable root, got %v", name, root)
		}
	}
}

// TestFieldStoredValuesReachingLoad pins #182: a field load sees only the stores
// that reach it. In `p.db = p.db.Where("tenant")` the load on the right runs
// before the store, so it sees nothing, while the next load sees that store.
// Without a load, as the Scopes callback resolver asks, every store is seen.
func TestFieldStoredValuesReachingLoad(t *testing.T) {
	t.Parallel()
	fixtures, _ := loadFixtureProgram(t)
	fn := fixtures["fsoParamOnce"]
	if fn == nil {
		t.Fatal("fsoParamOnce fixture missing")
	}

	var loads []*ssa.UnOp
	var tenant ssa.Value
	for _, b := range fn.Blocks {
		for _, instr := range b.Instrs {
			call, ok := instr.(*ssa.Call)
			if !ok || len(call.Call.Args) == 0 {
				continue
			}
			callee := call.Common().StaticCallee()
			if callee == nil || callee.Name() != "Where" {
				continue
			}
			if load, ok := call.Call.Args[0].(*ssa.UnOp); ok {
				loads = append(loads, load)
				if tenant == nil {
					tenant = call
				}
			}
		}
	}
	if len(loads) != 2 {
		t.Fatalf("expected two Where calls on a field load, got %d", len(loads))
	}

	fa := loads[0].X.(*ssa.FieldAddr)
	if got := FieldStoredValues(fa, loads[0]); len(got) != 0 {
		t.Errorf("the load before the store should see no store, got %v", got)
	}
	if got := FieldStoredValues(loads[1].X.(*ssa.FieldAddr), loads[1]); len(got) != 1 || got[0] != tenant {
		t.Errorf("the load after the store should see the Where(\"tenant\") result, got %v", got)
	}
	if got := FieldStoredValues(fa, nil); len(got) != 1 || got[0] != tenant {
		t.Errorf("without a load every store should be seen, got %v", got)
	}
}

// TestObservedFieldStoresUnrelatedLoad pins the fallback of #303: a load that
// no MakeClosure in the function leads to sees every store, as a nil load does.
func TestObservedFieldStoresUnrelatedLoad(t *testing.T) {
	t.Parallel()
	fixtures, _ := loadFixtureProgram(t)
	fn, other := fixtures["cosErrgroupTwo"], fixtures["cosSession"]
	if fn == nil || other == nil {
		t.Fatal("cosErrgroupTwo or cosSession fixture missing")
	}
	var alloc *ssa.Alloc
	for _, instr := range fn.Blocks[0].Instrs {
		if a, ok := instr.(*ssa.Alloc); ok && a.Comment == "db" {
			alloc = a
		}
	}
	if alloc == nil {
		t.Fatal("no Alloc for db in cosErrgroupTwo")
	}
	// Without a memo, and with one asked twice: the second answer is the kept
	// one (#325).
	memo := newStoreMemo()
	for _, m := range []*storeMemo{nil, memo, memo} {
		got, open := m.observedFieldStores(fn, storeTarget{root: alloc}, nil, other.Blocks[0].Instrs[0])
		if len(got) != 2 || !open {
			t.Errorf("an unrelated load should see both stores of db, got %v (open %v)", got, open)
		}
	}
}

// TestMakeClosureInStoreFuncNested pins #303's nesting: a closure nested in a
// spawned one is read at the outer closure's MakeClosure.
func TestMakeClosureInStoreFuncNested(t *testing.T) {
	t.Parallel()
	fixtures, _ := loadFixtureProgram(t)
	fn := fixtures["cosFieldNested"]
	if fn == nil || len(fn.AnonFuncs) != 1 || len(fn.AnonFuncs[0].AnonFuncs) != 1 {
		t.Fatal("cosFieldNested fixture missing or reshaped")
	}
	outer := fn.AnonFuncs[0]
	want := callsite.FreeVarMakeClosure(outer)
	if got := makeClosureInStoreFunc(fn, outer.AnonFuncs[0]); got == nil || got != want {
		t.Errorf("makeClosureInStoreFunc = %v, want %v", got, want)
	}
	if got := makeClosureInStoreFunc(fixtures["cosSession"], outer.AnonFuncs[0]); got != nil {
		t.Errorf("a closure not nested in the function should give nil, got %v", got)
	}
}

// storeRangeBody returns the Alloc that comment names in fn and the
// MakeClosure of fn's range-over-func body.
func storeRangeBody(t *testing.T, fn *ssa.Function, comment string) (*ssa.Alloc, *ssa.MakeClosure) {
	t.Helper()
	var alloc *ssa.Alloc
	var body *ssa.MakeClosure
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			switch x := instr.(type) {
			case *ssa.Alloc:
				if x.Comment == comment {
					alloc = x
				}
			case *ssa.MakeClosure:
				if cfg.IsRangeFuncLoopBody(x.Fn.(*ssa.Function)) {
					body = x
				}
			}
		}
	}
	if alloc == nil || body == nil {
		t.Fatalf("%s: no Alloc for %s or no range-over-func body", fn.Name(), comment)
	}
	return alloc, body
}

// TestReachingFieldStoresAfterRangeBody pins #341 for a read in the block of
// the body's MakeClosure, after it: the body's store does not end the search,
// so the stores of both branches of the if before the loop are seen too,
// first. The iterator call stands for such a read.
func TestReachingFieldStoresAfterRangeBody(t *testing.T) {
	t.Parallel()
	fixtures, _ := loadFixtureProgram(t)
	fn := fixtures["rangeFuncBodyStoreAfterIf"]
	if fn == nil {
		t.Fatal("rangeFuncBodyStoreAfterIf fixture missing")
	}
	alloc, mc := storeRangeBody(t, fn, "q")
	var iterate *ssa.Call
	for _, ref := range *mc.Referrers() {
		if call, ok := ref.(*ssa.Call); ok {
			iterate = call
		}
	}
	if iterate == nil {
		t.Fatal("no iterator call of the body")
	}
	got, open := newStoreMemo().reachingFieldStores(fn, storeTarget{root: alloc}, nil, iterate)
	if len(got) != 3 || open || got[0].Parent() != fn || got[1].Parent() != fn || got[2].Parent() == fn {
		t.Errorf("want the two stores before the loop, then the body's, got %v (open %v)", got, open)
	}
}

// TestFieldStoredValuesRangeBody pins #341 without a memo, as the callback
// resolver asks: every store of the field is seen, the body's included.
func TestFieldStoredValuesRangeBody(t *testing.T) {
	t.Parallel()
	fixtures, _ := loadFixtureProgram(t)
	fn := fixtures["rangeFuncBodyStoreField"]
	if fn == nil {
		t.Fatal("rangeFuncBodyStoreField fixture missing")
	}
	var fa *ssa.FieldAddr
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			if x, ok := instr.(*ssa.FieldAddr); ok {
				fa = x
			}
		}
	}
	if fa == nil {
		t.Fatal("no field address in rangeFuncBodyStoreField")
	}
	if got := FieldStoredValues(fa, nil); len(got) != 2 {
		t.Errorf("want the literal's store and the body's, got %v", got)
	}
}
