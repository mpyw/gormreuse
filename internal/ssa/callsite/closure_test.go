package callsite

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"testing"

	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

// closureSource holds one function per case. In each, the closure under test
// is the function's first closure.
const closureSource = `package p

func top() {}

func direct(x int) {
	f := func() { _ = x }
	f()
	f()
}

func merged(x int, c bool) {
	f := func() { _ = x }
	g := func() {}
	h := g
	if c {
		h = f
	}
	h()
}

func captured(x int) {
	f := func() { _ = x }
	f()
	func() { func() { f() }() }()
}

func capturedHere(x int) {
	f := func() { _ = x }
	func() { f = nil }()
	f()
}

func passed(x int) {
	f := func() { _ = x }
	run(f)
	go f()
	defer f()
}

func addressStored(x int) *func() {
	f := func() { _ = x }
	p := struct{ f *func() }{&f}
	set(&f)
	return p.f
}

func storedTwice(x int, c bool) {
	f := func() { _ = x }
	var h func()
	if c {
		h = f
	} else {
		h = f
	}
	func() { h() }()
}

func set(p *func()) {}

func run(f func()) { f() }
`

// closureTestFunc builds closureSource, with DebugRefs as the analyzer's SSA
// has them, and returns the named function and the MakeClosure of its first
// closure.
func closureTestFunc(t *testing.T, name string) (*ssa.Function, *ssa.MakeClosure) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "p.go", closureSource, 0)
	if err != nil {
		t.Fatal(err)
	}
	pkg, _, err := ssautil.BuildPackage(&types.Config{Importer: importer.Default()}, fset,
		types.NewPackage("p", "p"), []*ast.File{file}, ssa.SanityCheckFunctions|ssa.GlobalDebug)
	if err != nil {
		t.Fatal(err)
	}
	fn := pkg.Func(name)
	closure := fn.AnonFuncs[0]
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			if mc, ok := instr.(*ssa.MakeClosure); ok && mc.Fn == ssa.Value(closure) {
				return fn, mc
			}
		}
	}
	t.Fatalf("%s: no MakeClosure of %s", name, closure)
	return nil, nil
}

// TestClosureRuns pins where each closure runs, and which runs are plain
// calls in its own function (#305).
func TestClosureRuns(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// runs is the number of runs, called how many are plain calls, and
		// calls the number of ClosureCalls, 0 for nil.
		runs, called, calls int
	}{
		{"direct", 2, 2, 2},
		{"merged", 1, 1, 1},
		// The call in the nested closure is a plain call, in another function.
		{"captured", 2, 2, 0},
		// A store of nil to the variable is not a run.
		{"capturedHere", 1, 1, 1},
		// An argument, a go and a defer statement.
		{"passed", 3, 0, 0},
		// The variable's address stored into a struct, and passed on.
		{"addressStored", 2, 0, 0},
		// Two stores into one variable, read by one call.
		{"storedTwice", 1, 1, 0},
	}
	for _, tc := range tests {
		_, mc := closureTestFunc(t, tc.name)
		runs := ClosureRuns(mc)
		called := 0
		for _, run := range runs {
			if run.Called {
				called++
			}
		}
		if len(runs) != tc.runs || called != tc.called {
			t.Errorf("%s: %d runs, %d called, want %d and %d", tc.name, len(runs), called, tc.runs, tc.called)
		}
		if got := len(ClosureCalls(mc)); got != tc.calls {
			t.Errorf("%s: %d ClosureCalls, want %d", tc.name, got, tc.calls)
		}
	}

	// A function with no parent has no referrers to follow.
	fn, _ := closureTestFunc(t, "direct")
	if runs := ClosureRuns(fn.Pkg.Func("top")); runs != nil {
		t.Errorf("top: runs = %v, want nil", runs)
	}
}
