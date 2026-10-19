package tracer

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"

	"github.com/mpyw/gormreuse/internal/directive"
	"github.com/mpyw/gormreuse/internal/ssa/cfg"
	"github.com/mpyw/gormreuse/internal/typeutil"
)

// loadProgram builds SSA for the testdata/gormreuse fixture package (plus the
// gorm stub it imports) and returns the fixture functions keyed by name along
// with every function in the program (so gorm builtins like (*DB).Session can
// be located). tracer needs real *gorm.DB types, so it loads the GOPATH fixture
// package rather than an inline plain-Go snippet.
func loadProgram(t *testing.T) (fixtureFuncs map[string]*ssa.Function, allFuncs map[*ssa.Function]bool) {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	td := filepath.Join(filepath.Dir(file), "..", "..", "..", "testdata")

	cfgpkg := &packages.Config{
		Mode: packages.LoadAllSyntax,
		Dir:  td,
		Env:  append(os.Environ(), "GOPATH="+td, "GO111MODULE=off", "GOFLAGS="),
	}
	pkgs, err := packages.Load(cfgpkg, "gormreuse")
	if err != nil {
		t.Fatalf("packages.Load: %v", err)
	}
	if packages.PrintErrors(pkgs) > 0 {
		t.Fatal("packages had errors")
	}

	prog, ssaPkgs := ssautil.Packages(pkgs, ssa.BuilderMode(0))
	prog.Build()

	fixtureFuncs = make(map[string]*ssa.Function)
	for _, p := range ssaPkgs {
		if p == nil {
			continue
		}
		for _, m := range p.Members {
			if fn, ok := m.(*ssa.Function); ok {
				fixtureFuncs[fn.Name()] = fn
			}
		}
	}
	allFuncs = ssautil.AllFunctions(prog)
	if len(fixtureFuncs) == 0 || len(allFuncs) == 0 {
		t.Fatal("no SSA functions loaded")
	}
	return fixtureFuncs, allFuncs
}

// gormMethod finds the gorm.DB method with the given name in the program.
func gormMethod(allFuncs map[*ssa.Function]bool, name string) *ssa.Function {
	for fn := range allFuncs {
		if fn.Name() != name {
			continue
		}
		if sig := fn.Signature; sig != nil && sig.Recv() != nil && typeutil.IsGormDB(sig.Recv().Type()) {
			return fn
		}
	}
	return nil
}

func TestIsImmutableReturningBuiltin(t *testing.T) {
	t.Parallel()
	fixtures, all := loadProgram(t)
	tr := New(nil, nil, nil, nil, nil, nil, nil)

	session := gormMethod(all, "Session")
	if session == nil {
		t.Fatal("could not find (*gorm.DB).Session in the program")
	}
	if !tr.isImmutableReturningBuiltin(session) {
		t.Error("Session should be an immutable-returning builtin")
	}

	// A gorm method that is NOT immutable-returning (Where).
	if where := gormMethod(all, "Where"); where != nil && tr.isImmutableReturningBuiltin(where) {
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
	fixtures, all := loadProgram(t)
	// Syntax-backed pure set resolves //gormreuse:pure via each function's AST.
	tr := New(directive.NewPureFuncSet(nil, nil), nil, nil, nil, nil, nil, nil)

	if session := gormMethod(all, "Session"); session != nil && !tr.IsPureFunction(session) {
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

func TestClosureCapturesGormDB(t *testing.T) {
	t.Parallel()
	_, all := loadProgram(t)

	var sawCapturing bool
	for fn := range all {
		for _, b := range fn.Blocks {
			for _, instr := range b.Instrs {
				if mc, ok := instr.(*ssa.MakeClosure); ok && ClosureCapturesGormDB(mc) {
					sawCapturing = true
				}
			}
		}
	}
	if !sawCapturing {
		t.Error("expected at least one gorm-capturing closure among the fixtures")
	}
}

func TestCollectScopesCallbacks(t *testing.T) {
	t.Parallel()
	fixtures, _ := loadProgram(t)

	var srcFuncs []*ssa.Function
	for _, fn := range fixtures {
		srcFuncs = append(srcFuncs, fn)
	}
	set := CollectScopesCallbacks(srcFuncs)

	// The named function passed to Scopes must be collected.
	named := fixtures["namedScope"]
	if named == nil {
		t.Fatal("namedScope fixture missing")
	}
	if !set[named] {
		t.Error("namedScope (passed to db.Scopes) should be collected")
	}

	// An ordinary helper never handed to Scopes/Preload must NOT be collected.
	if ord := fixtures["ordinaryHelperParamBranches"]; ord != nil && set[ord] {
		t.Error("ordinary helper should not be collected as a Scopes callback")
	}

	// The inline Scopes/Preload closures should be collected too.
	var sawInlineClosure bool
	for fn := range set {
		if strings.HasPrefix(fn.Name(), "scopesCallbackReuse$") || strings.HasPrefix(fn.Name(), "preloadCallbackReuse$") {
			sawInlineClosure = true
		}
	}
	if !sawInlineClosure {
		t.Error("expected inline Scopes/Preload closures to be collected")
	}
}

// TestCollectScopesOnlyCallbacks covers the indirect callback shapes of #196
// (fixtures in scopes_session_warning.go) and the Preload exclusion.
func TestCollectScopesOnlyCallbacks(t *testing.T) {
	t.Parallel()
	fixtures, allFuncs := loadProgram(t)

	// Like buildssa's SrcFuncs, leave out the synthetic package initializer: the
	// resolver must find package-level initializers by itself.
	var srcFuncs []*ssa.Function
	for name, fn := range fixtures {
		if name != "init" {
			srcFuncs = append(srcFuncs, fn)
		}
	}
	set := CollectScopesOnlyCallbacks(srcFuncs)
	names := make(map[string]bool)
	for fn := range set {
		names[fn.String()] = true
		// Package-level closures are numbered across files; name this one by its variable.
		if strings.HasPrefix(fn.Name(), "init$") && strings.HasSuffix(fn.Prog.Fset.Position(fn.Pos()).Filename, "scopes_session_warning.go") {
			names["packageSessionScope"] = true
		}
	}

	for _, want := range []string{
		"gormreuse.namedSessionScope",                       // named function
		"(gormreuse.sessionScopeRepo).scope",                // method value target
		"gormreuse.scopesAppendSliceSessionWarns$1",         // append-built slice
		"packageSessionScope",                               // package-level closure
		"gormreuse.scopesCallbackSessionWarns$1",            // inline closure
		"gormreuse.scopesCallbackConditionalSessionWarns$1", // closure with captures
	} {
		if !names[want] {
			t.Errorf("%s should be collected as a Scopes callback", want)
		}
	}
	for _, unwanted := range []string{
		"gormreuse.preloadCallbackSessionClean$1", // Preload, not Scopes
		"gormreuse.notScopesSessionClean",         // never passed to Scopes
		"gormreuse.sessionScopeHelper",            // called by a callback, not passed
	} {
		if names[unwanted] {
			t.Errorf("%s should not be collected as a Scopes callback", unwanted)
		}
	}

	// The Preload callback is still in the Scopes+Preload set.
	var preload *ssa.Function
	for fn := range allFuncs {
		if fn.String() == "gormreuse.preloadCallbackSessionClean$1" {
			preload = fn
		}
	}
	if preload == nil || !CollectScopesCallbacks(srcFuncs)[preload] {
		t.Error("preloadCallbackSessionClean$1 should be in CollectScopesCallbacks")
	}
}

func TestFindMutableRootScopesParam(t *testing.T) {
	t.Parallel()
	fixtures, _ := loadProgram(t)
	loops := cfg.New()

	named := fixtures["namedScope"]
	ordinary := fixtures["ordinaryHelperParamBranches"]
	if named == nil || ordinary == nil {
		t.Fatal("required fixtures missing")
	}

	// With namedScope registered as a Scopes callback, its *gorm.DB parameter is
	// a mutable root.
	scopes := map[*ssa.Function]bool{named: true}
	tr := New(nil, nil, nil, nil, scopes, nil, nil)

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
	trPlain := New(nil, nil, nil, nil, nil, nil, nil)
	if root := trPlain.FindMutableRoot(namedParam, loops.DetectLoops(named)); root != namedParam {
		t.Errorf("Phase 1b: unregistered parameter should be a mutable root, got %v", root)
	}

	// A Transaction callback's tx parameter is exempt (fresh forkable handle):
	// registering the helper as a transaction callback makes its param immutable.
	trTx := New(nil, nil, nil, nil, nil, map[*ssa.Function]bool{ordinary: true}, nil)
	if root := trTx.FindMutableRoot(ordParam, loops.DetectLoops(ordinary)); root != nil {
		t.Errorf("Transaction callback parameter should be immutable (nil root), got %v", root)
	}
	if roots := trTx.FindAllMutableRoots(ordParam, loops.DetectLoops(ordinary)); len(roots) != 0 {
		t.Errorf("Transaction callback parameter should yield no roots, got %v", roots)
	}
}

// gormCallReceiver returns the receiver of the first call to the named gorm
// method in fn.
func gormCallReceiver(fn *ssa.Function, method string) ssa.Value {
	for _, b := range fn.Blocks {
		for _, instr := range b.Instrs {
			call, ok := instr.(*ssa.Call)
			if !ok {
				continue
			}
			callee := call.Common().StaticCallee()
			if callee == nil || callee.Name() != method || len(call.Call.Args) == 0 {
				continue
			}
			return call.Call.Args[0]
		}
	}
	return nil
}

// TestFindMutableRootThroughStructField pins the field-path matching in
// fieldStoredValues. The SSA builder stages a composite literal in a separate
// temporary and copies the finished aggregate into the destination, and it emits
// a fresh FieldAddr chain for every access, so a field read shares neither the
// store's base value nor its address chain. Matching the whole field path (and
// following the copy) is what keeps the value stored under a promoted field key
// (Go 1.27) reachable.
func TestFindMutableRootThroughStructField(t *testing.T) {
	t.Parallel()
	fixtures, _ := loadProgram(t)
	loops := cfg.New()
	tr := New(nil, nil, nil, nil, nil, nil, nil)

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
		recv := gormCallReceiver(fn, "Count")
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
	fixtures, _ := loadProgram(t)
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
	if got := fieldStoredValues(fa, loads[0]); len(got) != 0 {
		t.Errorf("the load before the store should see no store, got %v", got)
	}
	if got := fieldStoredValues(loads[1].X.(*ssa.FieldAddr), loads[1]); len(got) != 1 || got[0] != tenant {
		t.Errorf("the load after the store should see the Where(\"tenant\") result, got %v", got)
	}
	if got := fieldStoredValues(fa, nil); len(got) != 1 || got[0] != tenant {
		t.Errorf("without a load every store should be seen, got %v", got)
	}
}

// TestFieldRoot pins the FieldRoot of a field of a struct a call returned
// (#181): every read of the same field of the same call yields one interned
// root, and different fields yield different roots.
func TestFieldRoot(t *testing.T) {
	t.Parallel()
	fixtures, _ := loadProgram(t)
	loops := cfg.New()
	tr := New(nil, nil, nil, nil, nil, nil, nil)

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
}
