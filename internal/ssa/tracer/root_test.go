package tracer_test

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
	"github.com/mpyw/gormreuse/internal/ssa/tracer"
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
	tr := tracer.New(nil, nil, nil, nil, nil, nil)

	session := gormMethod(all, "Session")
	if session == nil {
		t.Fatal("could not find (*gorm.DB).Session in the program")
	}
	if !tr.IsImmutableReturningBuiltin(session) {
		t.Error("Session should be an immutable-returning builtin")
	}

	// A gorm method that is NOT immutable-returning (Where).
	if where := gormMethod(all, "Where"); where != nil && tr.IsImmutableReturningBuiltin(where) {
		t.Error("Where is not immutable-returning")
	}

	// A user fixture function that merely shares no builtin name.
	if fn := fixtures["nonPureHelper"]; fn != nil && tr.IsImmutableReturningBuiltin(fn) {
		t.Error("nonPureHelper is not a builtin")
	}
	if tr.IsImmutableReturningBuiltin(nil) {
		t.Error("nil is not a builtin")
	}
}

func TestIsPureFunction(t *testing.T) {
	t.Parallel()
	fixtures, all := loadProgram(t)
	// Syntax-backed pure set resolves //gormreuse:pure via each function's AST.
	tr := tracer.New(directive.NewPureFuncSet(nil, nil), nil, nil, nil, nil, nil)

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
				if mc, ok := instr.(*ssa.MakeClosure); ok && tracer.ClosureCapturesGormDB(mc) {
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
	set := tracer.CollectScopesCallbacks(srcFuncs)

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
	tr := tracer.New(nil, nil, nil, nil, scopes, nil)

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
	trPlain := tracer.New(nil, nil, nil, nil, nil, nil)
	if root := trPlain.FindMutableRoot(namedParam, loops.DetectLoops(named)); root != namedParam {
		t.Errorf("Phase 1b: unregistered parameter should be a mutable root, got %v", root)
	}

	// A Transaction callback's tx parameter is exempt (fresh forkable handle):
	// registering the helper as a transaction callback makes its param immutable.
	trTx := tracer.New(nil, nil, nil, nil, nil, map[*ssa.Function]bool{ordinary: true})
	if root := trTx.FindMutableRoot(ordParam, loops.DetectLoops(ordinary)); root != nil {
		t.Errorf("Transaction callback parameter should be immutable (nil root), got %v", root)
	}
	if roots := trTx.FindAllMutableRoots(ordParam, loops.DetectLoops(ordinary)); len(roots) != 0 {
		t.Errorf("Transaction callback parameter should yield no roots, got %v", roots)
	}
}
