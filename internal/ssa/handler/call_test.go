package handler

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

// loadFixtureCalls builds SSA for the testdata/gormreuse fixture package and
// returns every gorm-method *ssa.Call across all its functions. handler's
// helpers key on real *gorm.DB types, so it loads the GOPATH fixture package.
func loadFixtureCalls(t *testing.T) []*ssa.Call {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
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

	prog, _ := ssautil.Packages(pkgs, ssa.BuilderMode(0))
	prog.Build()

	var calls []*ssa.Call
	for fn := range ssautil.AllFunctions(prog) {
		for _, b := range fn.Blocks {
			for _, instr := range b.Instrs {
				call, ok := instr.(*ssa.Call)
				if !ok {
					continue
				}
				callee := call.Call.StaticCallee()
				if callee == nil {
					continue
				}
				if sig := callee.Signature; sig != nil && sig.Recv() != nil && typeutil.IsGormDB(sig.Recv().Type()) {
					calls = append(calls, call)
				}
			}
		}
	}
	if len(calls) == 0 {
		t.Fatal("no gorm method calls found in fixtures")
	}
	return calls
}

func TestIsChainedGormMethodCall(t *testing.T) {
	t.Parallel()
	calls := loadFixtureCalls(t)

	// Invariant: a gorm method call whose receiver (Args[0]) is itself a gorm
	// call forms a chain, and isChainedGormMethodCall must recognize it.
	var chains int
	for _, b := range calls {
		if len(b.Call.Args) == 0 {
			continue
		}
		recv, ok := b.Call.Args[0].(*ssa.Call)
		if !ok {
			continue
		}
		if !isChainedGormMethodCall(recv, b) {
			t.Errorf("expected chain recognized: %v -> %v", recv, b)
		}
		chains++
	}
	if chains == 0 {
		t.Fatal("no chained gorm calls found in fixtures (expected some, e.g. q.Where().Find())")
	}

	// A call is never chained onto an unrelated call that is not its receiver.
	if len(calls) >= 2 {
		a, b := calls[0], calls[1]
		if len(b.Call.Args) > 0 && b.Call.Args[0] != a && isChainedGormMethodCall(a, b) {
			t.Error("unrelated calls must not be reported as a chain")
		}
	}
}

func TestIsAssignment(t *testing.T) {
	t.Parallel()
	calls := loadFixtureCalls(t)

	// The fixtures contain both reassignments (q = q.Where(...); result flows to
	// a Phi or Store-to-Alloc → assignment) and terminal uses (q.Find(nil);
	// result discarded → not an assignment), so both branches must be exercised.
	var sawTrue, sawFalse bool
	for _, c := range calls {
		if isAssignment(c, nil) { // ctx is unused by isAssignment
			sawTrue = true
		} else {
			sawFalse = true
		}
	}
	if !sawTrue {
		t.Error("expected at least one call classified as an assignment")
	}
	if !sawFalse {
		t.Error("expected at least one call classified as a non-assignment")
	}
}

func TestIsGormDBMethodCall(t *testing.T) {
	t.Parallel()
	h := &CallHandler{}
	// Every call collected by loadFixtureCalls is, by construction, a gorm
	// method call; isGormDBMethodCall must agree.
	for _, c := range loadFixtureCalls(t) {
		if !h.isGormDBMethodCall(c) {
			t.Errorf("collected call should be a gorm method call: %v", c)
		}
	}
}
