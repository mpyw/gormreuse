package tracer

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"

	"github.com/mpyw/gormreuse/internal/typeutil"
)

// loadFixtureProgram builds SSA for the testdata/gormreuse fixture package
// (plus the gorm stub it imports) and returns the fixture functions keyed by
// name along with every function in the program (so gorm builtins like (*DB).Session can
// be located). tracer needs real *gorm.DB types, so it loads the GOPATH fixture
// package rather than an inline plain-Go snippet.
//
//declscope:shared // the tests of every file of the package load the fixtures with it
func loadFixtureProgram(t *testing.T) (fixtureFuncs map[string]*ssa.Function, allFuncs map[*ssa.Function]bool) {
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

// loadFixtureSource builds SSA for a package made of src, which may import
// gorm.io/gorm (the testdata stub), and returns its functions by name.
//
//declscope:shared // alias_test.go and root_test.go build shapes the fixtures lack with it
func loadFixtureSource(t *testing.T, src string) map[string]*ssa.Function {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	gopath := t.TempDir()
	stub := filepath.Join(filepath.Dir(file), "..", "..", "..", "testdata", "src", "gorm.io", "gorm")
	if err := os.CopyFS(filepath.Join(gopath, "src", "gorm.io", "gorm"), os.DirFS(stub)); err != nil {
		t.Fatalf("copy gorm stub: %v", err)
	}
	dir := filepath.Join(gopath, "src", "shape")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "shape.go"), []byte(src), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	pkgs, err := packages.Load(&packages.Config{
		Mode: packages.LoadAllSyntax,
		Dir:  gopath,
		Env:  append(os.Environ(), "GOPATH="+gopath, "GO111MODULE=off", "GOPROXY=off", "GOFLAGS="),
	}, "shape")
	if err != nil {
		t.Fatalf("packages.Load: %v", err)
	}
	if packages.PrintErrors(pkgs) > 0 {
		t.Fatal("packages had errors")
	}
	prog, ssaPkgs := ssautil.Packages(pkgs, ssa.BuilderMode(0))
	prog.Build()
	funcs := make(map[string]*ssa.Function)
	for _, m := range ssaPkgs[0].Members {
		if fn, ok := m.(*ssa.Function); ok {
			funcs[fn.Name()] = fn
		}
	}
	return funcs
}

// fixtureGormMethod finds the gorm.DB method with the given name in the
// program.
//
//declscope:shared // root_test.go looks gorm builtins up with it
func fixtureGormMethod(allFuncs map[*ssa.Function]bool, name string) *ssa.Function {
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

// fixtureGormCallReceiver returns the receiver of the first call to the named
// gorm method in fn.
//
//declscope:shared // store_test.go finds the receiver of a fixture call with it
func fixtureGormCallReceiver(fn *ssa.Function, method string) ssa.Value {
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
