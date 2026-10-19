package tracer

import (
	"testing"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/directive"
	"github.com/mpyw/gormreuse/internal/ssa/cfg"
)

func TestIsImmutableReturningBuiltin(t *testing.T) {
	t.Parallel()
	fixtures, all := loadFixtureProgram(t)
	tr := NewRootTracer(nil, nil, nil, nil, nil, nil, nil)

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

func TestIsPureFunction(t *testing.T) {
	t.Parallel()
	fixtures, all := loadFixtureProgram(t)
	// Syntax-backed pure set resolves //gormreuse:pure via each function's AST.
	tr := NewRootTracer(directive.NewPureFuncSet(nil, nil), nil, nil, nil, nil, nil, nil)

	if session := fixtureGormMethod(all, "Session"); session != nil && !tr.IsPureFunction(session) {
		t.Error("Session (immutable builtin) should count as pure")
	}
	if fn := fixtures["pureDoesNothing"]; fn == nil {
		t.Fatal("pureDoesNothing fixture missing")
	} else if !tr.IsPureFunction(fn) {
		t.Error("pureDoesNothing is marked //gormreuse:pure")
	}
	if fn := fixtures["nonPureHelper"]; fn != nil && tr.IsPureFunction(fn) {
		t.Error("nonPureHelper is not pure")
	}
	if tr.IsPureFunction(nil) {
		t.Error("nil is not pure")
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
	tr := NewRootTracer(nil, nil, nil, nil, scopes, nil, nil)

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
	trPlain := NewRootTracer(nil, nil, nil, nil, nil, nil, nil)
	if root := trPlain.FindMutableRoot(namedParam, loops.DetectLoops(named)); root != namedParam {
		t.Errorf("Phase 1b: unregistered parameter should be a mutable root, got %v", root)
	}

	// A Transaction callback's tx parameter is exempt (fresh forkable handle):
	// registering the helper as a transaction callback makes its param immutable.
	trTx := NewRootTracer(nil, nil, nil, nil, nil, map[*ssa.Function]bool{ordinary: true}, nil)
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
