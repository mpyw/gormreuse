package callback

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"

	"github.com/mpyw/gormreuse/internal/ssa/callsite"
)

// loadCollectProgram builds SSA for the testdata/gormreuse fixture package (plus the
// gorm stub it imports) and returns the fixture functions keyed by name along
// with every function in the program (so a callback that is not a package member can
// be located). The collectors need real *gorm.DB types, so it loads the GOPATH
// fixture package rather than an inline plain-Go snippet.
func loadCollectProgram(t *testing.T) (fixtureFuncs map[string]*ssa.Function, allFuncs map[*ssa.Function]bool) {
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

func TestCollectScopesCallbacks(t *testing.T) {
	t.Parallel()
	fixtures, _ := loadCollectProgram(t)

	var srcFuncs []*ssa.Function
	for _, fn := range fixtures {
		srcFuncs = append(srcFuncs, fn)
	}
	set := CollectScopesCallbacks(srcFuncs, callsite.FuncVars{})

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
	fixtures, allFuncs := loadCollectProgram(t)

	// Like buildssa's SrcFuncs, leave out the synthetic package initializer: the
	// resolver must find package-level initializers by itself.
	var srcFuncs []*ssa.Function
	for name, fn := range fixtures {
		if name != "init" {
			srcFuncs = append(srcFuncs, fn)
		}
	}
	set := CollectScopesOnlyCallbacks(srcFuncs, callsite.FuncVars{})
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
	if preload == nil || !CollectScopesCallbacks(srcFuncs, callsite.FuncVars{})[preload] {
		t.Error("preloadCallbackSessionClean$1 should be in CollectScopesCallbacks")
	}
}

// TestCollectImmutableInputCallbacksNil checks that a missing set or target
// adds nothing.
func TestCollectImmutableInputCallbacksNil(t *testing.T) {
	t.Parallel()
	fixtures, _ := loadCollectProgram(t)
	var srcFuncs []*ssa.Function
	for _, fn := range fixtures {
		srcFuncs = append(srcFuncs, fn)
	}
	into := make(map[*ssa.Function]bool)
	CollectImmutableInputCallbacks(srcFuncs, callsite.FuncVars{}, nil, into)
	CollectImmutableInputCallbacks(srcFuncs, callsite.FuncVars{}, nil, nil)
	if len(into) != 0 {
		t.Errorf("want nothing collected, got %v", into)
	}
}
