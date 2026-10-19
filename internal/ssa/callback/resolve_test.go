package callback

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"slices"
	"testing"

	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

// resolveSrc holds one function per shape that resolveMayFuncs follows. Each
// passes its callbacks to use, and names them a*, b* or c* after the shape.
const resolveSrc = `package p

type fns []func()

type holder struct{ s []func() }

type named func()

func use(fs ...func()) {}

func a1() {}
func a2() {}
func b1() {}
func b2() {}
func c1() {}
func c2() {}
func d1() {}
func e1() {}
func f1() {}

var global = func() {}

var cond bool

func slicePhi() {
	s := []func(){a1}
	if cond {
		s = []func(){a2}
	}
	use(s...)
}

func capturedSlice() {
	s := []func(){b1}
	func() { use(s...) }()
	s = nil
}

func fieldSlice() {
	h := &holder{s: []func(){b2}}
	use(h.s...)
}

func convertedSlice() {
	var v fns = fns{c1}
	use(([]func())(v)...)
}

func funcPhi() {
	f := c2
	if cond {
		f = d1
	}
	use(f)
}

func convertedFunc() {
	use((func())(named(e1)))
}

func loadedFunc() {
	use(global)
}

func loopSlice() {
	s := []func(){a1}
	for cond {
		s = append(s, a2)
	}
	use(s...)
}

func repeatedFunc() {
	use(a1, a1)
}

func pointerParam(p *func()) {
	use(*p)
}

func capturedFunc() {
	f := f1
	func() { use(f) }()
	f = nil
}
`

// buildResolveProgram builds resolveSrc and returns its source functions and
// the package members by name.
func buildResolveProgram(t *testing.T) (funcs []*ssa.Function, members map[string]*ssa.Function) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "p.go", resolveSrc, 0)
	if err != nil {
		t.Fatal(err)
	}
	pkg, _, err := ssautil.BuildPackage(&types.Config{Importer: importer.Default()}, fset,
		types.NewPackage("p", "p"), []*ast.File{file}, ssa.BuilderMode(0))
	if err != nil {
		t.Fatal(err)
	}
	members = make(map[string]*ssa.Function)
	for name, m := range pkg.Members {
		if fn, ok := m.(*ssa.Function); ok {
			members[name] = fn
			if name != "init" {
				funcs = append(funcs, fn)
			}
		}
	}
	return funcs, members
}

// resolvedUseArgs returns the last argument of every call of use in funcs and
// their closures.
func resolvedUseArgs(funcs []*ssa.Function, use *ssa.Function) []ssa.Value {
	var vals []ssa.Value
	for _, fn := range funcs {
		for _, f := range append([]*ssa.Function{fn}, fn.AnonFuncs...) {
			for _, b := range f.Blocks {
				for _, instr := range b.Instrs {
					if call, ok := instr.(*ssa.Call); ok && call.Call.StaticCallee() == use {
						vals = append(vals, call.Call.Args[len(call.Call.Args)-1])
					}
				}
			}
		}
	}
	return vals
}

// TestResolveMayFuncs covers each shape resolveMayFuncs follows.
func TestResolveMayFuncs(t *testing.T) {
	t.Parallel()
	funcs, members := buildResolveProgram(t)
	set := resolveMayFuncs(funcs, resolvedUseArgs(funcs, members["use"]))

	for _, want := range []string{"a1", "a2", "b1", "b2", "c1", "c2", "d1", "e1", "f1", "init$1"} {
		fn := members[want]
		if want == "init$1" {
			fn = members["init"].AnonFuncs[0]
		}
		if !set[fn] {
			t.Errorf("%s should be resolved", want)
		}
	}
	if set[members["use"]] {
		t.Error("use is called, not passed")
	}
}

// TestResolveExactFunc checks that the exact query follows only the steps
// that keep one function.
func TestResolveExactFunc(t *testing.T) {
	t.Parallel()
	funcs, members := buildResolveProgram(t)
	vals := resolvedUseArgs(slices.DeleteFunc(funcs, func(fn *ssa.Function) bool {
		return fn.Name() != "funcPhi"
	}), members["use"])
	if len(vals) != 1 {
		t.Fatalf("want one call of use in funcPhi, got %d", len(vals))
	}
	// The varargs slice is not a function, and neither is the Phi stored in it.
	if fn := resolveExactFunc(vals[0]); fn != nil {
		t.Errorf("a slice should resolve to nil, got %v", fn)
	}
	if fn := resolveExactFunc(members["a1"]); fn != members["a1"] {
		t.Errorf("a function should resolve to itself, got %v", fn)
	}
	if fn := resolveExactFunc(nil); fn != nil {
		t.Errorf("nil should resolve to nil, got %v", fn)
	}
}
