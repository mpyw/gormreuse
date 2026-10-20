package tracer

import (
	"strings"
	"testing"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/directive"
	"github.com/mpyw/gormreuse/internal/ssa/callsite"
	"github.com/mpyw/gormreuse/internal/ssa/cfg"
)

func TestIsImmutableReturningBuiltin(t *testing.T) {
	t.Parallel()
	fixtures, all := loadFixtureProgram(t)
	tr := NewRootTracer(nil, nil, nil, nil, nil, nil, callsite.FuncVars{})

	session := fixtureGormMethod(all, "Session")
	if session == nil {
		t.Fatal("could not find (*gorm.DB).Session in the program")
	}
	if !tr.isImmutableReturningBuiltin(session) {
		t.Error("Session should be an immutable-returning builtin")
	}

	// A gorm method that is NOT immutable-returning (Where).
	if where := fixtureGormMethod(all, "Where"); where != nil && tr.isImmutableReturningBuiltin(where) {
		t.Error("Where is not immutable-returning")
	}

	// A user fixture function that merely shares no builtin name.
	if fn := fixtures["nonPureHelper"]; fn != nil && tr.isImmutableReturningBuiltin(fn) {
		t.Error("nonPureHelper is not a builtin")
	}
	if tr.isImmutableReturningBuiltin(nil) {
		t.Error("nil is not a builtin")
	}
}

func TestFindMutableRootScopesParam(t *testing.T) {
	t.Parallel()
	fixtures, _ := loadFixtureProgram(t)
	loops := cfg.NewAnalyzer()

	named := fixtures["namedScope"]
	ordinary := fixtures["ordinaryHelperParamBranches"]
	if named == nil || ordinary == nil {
		t.Fatal("required fixtures missing")
	}

	// With namedScope registered as a Scopes callback, its *gorm.DB parameter is
	// a mutable root.
	scopes := map[*ssa.Function]bool{named: true}
	tr := NewRootTracer(nil, nil, nil, nil, scopes, nil, callsite.FuncVars{})

	if !tr.IsScopesCallbackFunc(named) {
		t.Error("namedScope should be recognized as a Scopes callback function")
	}
	if tr.IsScopesCallbackFunc(ordinary) {
		t.Error("ordinary helper should not be recognized as a Scopes callback function")
	}

	namedParam := named.Params[0]
	if root := tr.FindMutableRoot(namedParam, loops.DetectLoops(named)); root != namedParam {
		t.Errorf("Scopes callback parameter should be its own mutable root, got %v", root)
	}
	if roots := tr.FindAllMutableRoots(namedParam, loops.DetectLoops(named)); len(roots) != 1 || roots[0] != namedParam {
		t.Errorf("FindAllMutableRoots should return the Scopes param, got %v", roots)
	}

	// Phase 1b (#61): an ordinary *gorm.DB parameter is now a mutable root by
	// default, even without any Scopes registration.
	ordParam := ordinary.Params[0]
	if root := tr.FindMutableRoot(ordParam, loops.DetectLoops(ordinary)); root != ordParam {
		t.Errorf("Phase 1b: ordinary parameter should be a mutable root, got %v", root)
	}
	trPlain := NewRootTracer(nil, nil, nil, nil, nil, nil, callsite.FuncVars{})
	if root := trPlain.FindMutableRoot(namedParam, loops.DetectLoops(named)); root != namedParam {
		t.Errorf("Phase 1b: unregistered parameter should be a mutable root, got %v", root)
	}

	// A Transaction callback's tx parameter is exempt (fresh forkable handle):
	// registering the helper as a transaction callback makes its param immutable.
	trTx := NewRootTracer(nil, nil, nil, nil, nil, map[*ssa.Function]bool{ordinary: true}, callsite.FuncVars{})
	if root := trTx.FindMutableRoot(ordParam, loops.DetectLoops(ordinary)); root != nil {
		t.Errorf("Transaction callback parameter should be immutable (nil root), got %v", root)
	}
	if roots := trTx.FindAllMutableRoots(ordParam, loops.DetectLoops(ordinary)); len(roots) != 0 {
		t.Errorf("Transaction callback parameter should yield no roots, got %v", roots)
	}
}

// TestRootBlock checks the defining block of each root kind (#250).
func TestRootBlock(t *testing.T) {
	t.Parallel()
	fixtures, _ := loadFixtureProgram(t)
	fn := fixtures["valueStructCalledInLoop"]
	if fn == nil {
		t.Fatal("valueStructCalledInLoop fixture missing")
	}
	var call *ssa.Call
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			if c, ok := instr.(*ssa.Call); ok && call == nil {
				call = c
			}
		}
	}
	if call == nil {
		t.Fatal("no call in valueStructCalledInLoop")
	}
	if got := RootBlock(call); got != call.Block() {
		t.Errorf("RootBlock(call) = %v, want %v", got, call.Block())
	}
	if got := RootBlock(&FieldRoot{Call: call}); got != call.Block() {
		t.Errorf("RootBlock(FieldRoot) = %v, want %v", got, call.Block())
	}
	if got := RootBlock(fn.Params[0]); got != fn.Blocks[0] {
		t.Errorf("RootBlock(param) = %v, want the entry block", got)
	}
	if got := RootBlock(ssa.NewConst(nil, call.Type())); got != nil {
		t.Errorf("RootBlock(const) = %v, want nil", got)
	}
	if got := RootBlock(&ssa.Parameter{}); got != nil {
		t.Errorf("RootBlock(param without a function) = %v, want nil", got)
	}
}

// TestFindAllMutableRootsPureArgumentAlreadyTraced checks that a pure call
// whose argument the search has already traced stands for the argument's
// roots, and is not a root of its own (#297). In helperSelfReassignment,
// `if c { q = mergeHelper(q) }` merges q with mergeHelper(q), and the search
// reaches q before the call.
func TestFindAllMutableRootsPureArgumentAlreadyTraced(t *testing.T) {
	t.Parallel()
	fixtures, _ := loadFixtureProgram(t)
	fn := fixtures["helperSelfReassignment"]
	helper := fixtures["mergeHelper"]
	if fn == nil || helper == nil {
		t.Fatal("required fixtures missing")
	}
	// Trust mergeHelper as a pure function that returns its argument.
	pureFuncs := directive.NewPureFuncSet(nil, nil)
	pureFuncs.Infer(helper)
	verdicts := &PureVerdicts{Returned: map[*ssa.Function][]int{helper: {0}}}
	tr := NewRootTracer(pureFuncs, nil, nil, verdicts, nil, nil, callsite.FuncVars{})

	var where, merged ssa.Value
	for _, b := range fn.Blocks {
		for _, instr := range b.Instrs {
			call, ok := instr.(*ssa.Call)
			if !ok {
				continue
			}
			switch callee := call.Common().StaticCallee(); {
			case callee != nil && callee.Name() == "Where" && where == nil:
				where = call
			case callee == helper && merged == nil:
				merged = call
			}
		}
	}
	if where == nil || merged == nil {
		t.Fatal("fixture shape changed")
	}
	var phi *ssa.Phi
	for _, ref := range *merged.Referrers() {
		if p, ok := ref.(*ssa.Phi); ok {
			phi = p
		}
	}
	if phi == nil || len(phi.Edges) != 2 || phi.Edges[0] != where || phi.Edges[1] != merged {
		t.Fatalf("want a Phi of [q, mergeHelper(q)], got %v", phi)
	}
	roots := tr.FindAllMutableRoots(phi, cfg.NewAnalyzer().DetectLoops(fn))
	if len(roots) != 1 || roots[0] != where {
		t.Errorf("want the roots of q only, got %v", roots)
	}
}

// rootLoopShapes are the shapes the root memo tests need: a receiver with
// more roots than rootSetMinLen, and a closure without loops in a function
// with one.
var rootLoopShapes = `package shape

import "gorm.io/gorm"

func manyLoops(db *gorm.DB, xs []string) {
	q := db.Model(nil)
` + strings.Repeat(`	for _, x := range xs {
		q = q.Where(x)
	}
`, rootSetMinLen) + `	q.Find(nil)
}

func closureInLoop(db *gorm.DB, xs []string) {
	q := db.Model(nil)
	for _, x := range xs {
		q = q.Where(x)
	}
	func() {
		q.Find(nil)
	}()
}
`

// TestHasMutableRoot checks HasMutableRoot against the list it stands for,
// on a list long enough to be kept as a set and on a short one (#300).
func TestHasMutableRoot(t *testing.T) {
	t.Parallel()
	fixtures, _ := loadFixtureProgram(t)
	shapes := loadFixtureSource(t, rootLoopShapes)
	loops := cfg.NewAnalyzer()

	for _, c := range []struct {
		fn  *ssa.Function
		set bool
	}{
		{shapes["manyLoops"], true},
		{fixtures["loopSelfReassignment"], false},
	} {
		tr := NewRootTracer(nil, nil, nil, nil, nil, nil, callsite.FuncVars{})
		loopInfo := loops.DetectLoops(c.fn)
		recv := fixtureGormCallReceiver(c.fn, "Find")
		roots := tr.FindAllMutableRoots(recv, loopInfo)
		for _, r := range roots {
			if !tr.HasMutableRoot(recv, loopInfo, r) {
				t.Errorf("%s: root %v not found", c.fn.Name(), r)
			}
		}
		if tr.HasMutableRoot(recv, loopInfo, recv) {
			t.Errorf("%s: the receiver Phi is not one of its roots", c.fn.Name())
		}
		if _, ok := tr.rootSets[allRootsKey{recv, loopInfo}]; ok != c.set {
			t.Errorf("%s: %d roots kept as a set = %t, want %t", c.fn.Name(), len(roots), ok, c.set)
		}
	}
}

// TestFindAllMutableRootsLoopInfoKey checks that a loopInfo without a loop
// header shares the entry of nil, and one with headers keeps its own (#300).
func TestFindAllMutableRootsLoopInfoKey(t *testing.T) {
	t.Parallel()
	shapes := loadFixtureSource(t, rootLoopShapes)
	loops := cfg.NewAnalyzer()
	parent := shapes["closureInLoop"]
	closure := parent.AnonFuncs[0]
	closureLoops := loops.DetectLoops(closure)
	if closureLoops.HasLoopHeaders() {
		t.Fatal("the closure has no loop")
	}
	if rootsLoopInfo(closureLoops) != nil {
		t.Error("a loopInfo without headers should key as nil")
	}
	parentLoops := loops.DetectLoops(parent)
	if rootsLoopInfo(parentLoops) != parentLoops {
		t.Error("a loopInfo with headers should key as itself")
	}

	tr := NewRootTracer(nil, nil, nil, nil, nil, nil, callsite.FuncVars{})
	q := fixtureGormCallReceiver(closure, "Find")
	want := tr.FindAllMutableRoots(q, nil)
	got := tr.FindAllMutableRoots(q, closureLoops)
	if len(got) == 0 || &got[0] != &want[0] {
		t.Errorf("the closure's query was traced again: %v, want the entry %v", got, want)
	}
	if len(tr.allRoots) != 1 {
		t.Errorf("%d entries, want 1", len(tr.allRoots))
	}
	tr.FindAllMutableRoots(q, parentLoops)
	if len(tr.allRoots) != 2 {
		t.Errorf("%d entries, want 2", len(tr.allRoots))
	}
}
