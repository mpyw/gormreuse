package purity_test

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
	"github.com/mpyw/gormreuse/internal/ssa/purity"
)

// loadFixtureFuncs builds SSA for the testdata/gormreuse fixture package (which
// imports the gorm stub) and returns its functions keyed by name. purity needs
// real *gorm.DB types, so — unlike cfg — it cannot use a plain-Go inline
// harness and loads the GOPATH fixture package instead.
func loadFixtureFuncs(t *testing.T) map[string]*ssa.Function {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// internal/ssa/purity -> module root is three levels up.
	td := filepath.Join(filepath.Dir(file), "..", "..", "..", "testdata")

	cfg := &packages.Config{
		Mode: packages.LoadAllSyntax,
		Dir:  td,
		Env:  append(os.Environ(), "GOPATH="+td, "GO111MODULE=off", "GOFLAGS="),
	}
	pkgs, err := packages.Load(cfg, "gormreuse")
	if err != nil {
		t.Fatalf("packages.Load: %v", err)
	}
	if packages.PrintErrors(pkgs) > 0 {
		t.Fatal("packages had errors")
	}

	prog, ssaPkgs := ssautil.Packages(pkgs, ssa.BuilderMode(0))
	prog.Build()

	funcs := make(map[string]*ssa.Function)
	for _, p := range ssaPkgs {
		if p == nil {
			continue
		}
		for _, m := range p.Members {
			if fn, ok := m.(*ssa.Function); ok {
				funcs[fn.Name()] = fn
			}
		}
	}
	if len(funcs) == 0 {
		t.Fatal("no SSA functions loaded from fixture package")
	}
	return funcs
}

func TestValidateFunction(t *testing.T) {
	t.Parallel()
	funcs := loadFixtureFuncs(t)
	// A syntax-backed pure set: Contains resolves //gormreuse:pure via each
	// function's AST, which is enough for the fixtures.
	pureFuncs := directive.NewPureFuncSet(nil, nil)

	tests := []struct {
		fn       string
		want     []string // substrings that must each appear in some violation
		wantLeak bool     // at least one violation has Leak set
		clean    bool     // expect zero violations
	}{
		{fn: "purePollutesByWhere", want: []string{"pollutes", "Where"}},
		{fn: "purePollutesByFind", want: []string{"Find"}},
		{fn: "purePollutesByChain", want: []string{"Where", "Find"}},
		{fn: "pureReturnsWhereResult", want: []string{"Where"}},
		{fn: "purePollutesOneOfMany", want: []string{"Where"}},
		{fn: "pureReturnsNonPureFuncResult", want: []string{"non-pure function", "nonPureHelperReturns"}},
		{fn: "pureLeaksViaChanSend", want: []string{"channel send"}, wantLeak: true},
		{fn: "pureLeaksViaSliceStore", want: []string{"slice/array store"}, wantLeak: true},
		{fn: "pureLeaksViaMapStore", want: []string{"map store"}, wantLeak: true},
		{fn: "pureLeaksViaInterfaceArg", want: []string{"non-pure function", "nonPureTakesAny"}},
		{fn: "pureDoesNothing", clean: true},
		{fn: "pureOnlyCallsSession", clean: true},
		{fn: "purePassesToPure", clean: true},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.fn, func(t *testing.T) {
			t.Parallel()
			fn, ok := funcs[tc.fn]
			if !ok {
				t.Fatalf("fixture function %q not found", tc.fn)
			}
			violations := purity.ValidateFunction(fn, pureFuncs)

			if tc.clean {
				if len(violations) != 0 {
					t.Errorf("%s: expected no violations, got %d: %+v", tc.fn, len(violations), violations)
				}
				return
			}

			if len(violations) == 0 {
				t.Fatalf("%s: expected violations, got none", tc.fn)
			}
			joined := ""
			leak := false
			for _, v := range violations {
				joined += v.Message + "\n"
				if v.Leak {
					leak = true
				}
			}
			for _, sub := range tc.want {
				if !strings.Contains(joined, sub) {
					t.Errorf("%s: violations missing %q; got:\n%s", tc.fn, sub, joined)
				}
			}
			if tc.wantLeak && !leak {
				t.Errorf("%s: expected a Leak violation, got none", tc.fn)
			}
			if !tc.wantLeak && leak {
				t.Errorf("%s: unexpected Leak violation (should be conservative, not definitive escape)", tc.fn)
			}
		})
	}
}

// TestValidateFunctionNil covers the nil/blockless guards.
func TestValidateFunctionNil(t *testing.T) {
	t.Parallel()
	if v := purity.ValidateFunction(nil, directive.NewPureFuncSet(nil, nil)); v != nil {
		t.Errorf("nil function: expected nil, got %+v", v)
	}
	if v := purity.ValidateFunction(&ssa.Function{}, directive.NewPureFuncSet(nil, nil)); v != nil {
		t.Errorf("blockless function: expected nil, got %+v", v)
	}
}
