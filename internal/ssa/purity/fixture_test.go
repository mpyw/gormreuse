package purity_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

// loadFixtureFuncs builds SSA for the testdata/gormreuse fixture package (which
// imports the gorm stub) and returns its functions keyed by name. purity needs
// real *gorm.DB types, so — unlike cfg — it cannot use a plain-Go inline
// harness and loads the GOPATH fixture package instead.
//
//declscope:shared // validator_test.go and immutable_return_test.go load the fixtures with it
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
