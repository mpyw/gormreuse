package pollutionsource_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"

	"github.com/mpyw/gormreuse/internal/ssa/pollutionsource"
)

// testdataDir returns the module-root testdata directory (GOPATH root for the
// gorm stub and the gormreuse fixture package), computed relative to this file.
func testdataDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// internal/ssa/pollutionsource -> module root is three levels up.
	root := filepath.Join(filepath.Dir(file), "..", "..", "..")
	return filepath.Join(root, "testdata")
}

// loadFixtureFuncs builds SSA for the testdata/gormreuse fixture package and
// returns its functions keyed by name.
func loadFixtureFuncs(t *testing.T) map[string]*ssa.Function {
	t.Helper()
	td := testdataDir(t)
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

// leakKindsIn returns the set of leak kinds pollutionsource.Leak reports across
// all instructions of fn.
func leakKindsIn(fn *ssa.Function) map[pollutionsource.Kind]bool {
	kinds := make(map[pollutionsource.Kind]bool)
	for _, b := range fn.Blocks {
		for _, instr := range b.Instrs {
			if v, k := pollutionsource.Leak(instr); k != pollutionsource.KindNone && v != nil {
				kinds[k] = true
			}
		}
	}
	return kinds
}

// TestLeakEnumeratesSources is the single source of truth check: every non-call
// pollution source the fixture package exercises is classified by Leak, and
// non-leaking / read-only-exempt functions produce nothing (issue #66).
func TestLeakEnumeratesSources(t *testing.T) {
	t.Parallel()
	funcs := loadFixtureFuncs(t)

	tests := []struct {
		fn   string
		want pollutionsource.Kind // KindNone means "no leak of any kind"
	}{
		{"pureLeaksViaChanSend", pollutionsource.KindChannelSend},
		{"pureLeaksViaSliceStore", pollutionsource.KindSliceStore},
		{"pureLeaksViaMapStore", pollutionsource.KindMapStore},
		// Read-only variadic stdlib packing (fmt.Println) must NOT be a leak.
		{"pureLogsArgReadOnly", pollutionsource.KindNone},
		// A function that never lets its argument escape.
		{"pureDoesNothing", pollutionsource.KindNone},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.fn, func(t *testing.T) {
			t.Parallel()
			fn, ok := funcs[tc.fn]
			if !ok {
				t.Fatalf("fixture function %q not found", tc.fn)
			}
			got := leakKindsIn(fn)
			if tc.want == pollutionsource.KindNone {
				if len(got) != 0 {
					t.Errorf("%s: expected no leak, got kinds %v", tc.fn, got)
				}
				return
			}
			if !got[tc.want] {
				t.Errorf("%s: expected leak kind %v, got %v", tc.fn, tc.want, got)
			}
		})
	}
}

// TestUnwrapGormDB covers the interface-box unwrapping used by every source.
func TestUnwrapGormDB(t *testing.T) {
	t.Parallel()
	funcs := loadFixtureFuncs(t)

	// pureLeaksViaInterfaceArg boxes its *gorm.DB parameter into interface{}
	// via a MakeInterface before the call, so the program contains at least one
	// MakeInterface wrapping a *gorm.DB that UnwrapGormDB must see through.
	fn, ok := funcs["pureLeaksViaInterfaceArg"]
	if !ok {
		t.Fatal("pureLeaksViaInterfaceArg not found")
	}
	var sawBoxedGormDB bool
	for _, b := range fn.Blocks {
		for _, instr := range b.Instrs {
			mi, ok := instr.(*ssa.MakeInterface)
			if !ok {
				continue
			}
			if _, isGorm := pollutionsource.UnwrapGormDB(mi); isGorm {
				sawBoxedGormDB = true
			}
		}
	}
	if !sawBoxedGormDB {
		t.Error("UnwrapGormDB did not see through a MakeInterface-boxed *gorm.DB")
	}
}
