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

// funcVarSource holds one function per case. In each, the call under test is
// the last call through a variable: a load of a local or a package-level
// variable.
const funcVarSource = `package p

type R struct{}

func (R) m() {}

type I interface{ m() }

func top() {}

var global = func() {}

func closure(x int) {
	f := func() { _ = x }
	func() { f() }()
}

func bound(r R) {
	f := r.m
	func() { f() }()
}

func static() {
	f := top
	f()
	func() { f() }()
}

func nested() {
	f := top
	func() { func() { f() }() }()
}

func chained() {
	f := top
	g := f
	func() { _ = f; g() }()
}

func viaGlobal() {
	f := global
	func() { f() }()
}

func storedTwice(c bool) {
	f := top
	if c {
		f = func() {}
	}
	func() { f() }()
}

func storedInClosure() {
	var f func()
	func() { f = top }()
	f()
}

func storedInNestedClosure() {
	f := top
	func() { func() { f = func() {} }() }()
	f()
}

func sink(*func()) {}

func escaped() {
	f := top
	sink(&f)
	func() { f() }()
}

func addressStored(q **func()) {
	f := top
	*q = &f
	func() { f() }()
}

func cycle() {
	var f func()
	f = f
	func() { f() }()
}

func param(f func()) {
	func() { f() }()
	f = nil
}

func received(ch chan func()) {
	(<-ch)()
}

func invoke(i I) {
	func() { i.m() }()
}
`

// funcVarCallOf returns the Site of the last call in function name, or in a
// closure it declares, that is not a call of a closure literal in place.
func funcVarCallOf(t *testing.T, pkg *ssa.Package, vars FuncVars, name string) Site {
	t.Helper()
	var last ssa.CallInstruction
	WalkInstrs([]*ssa.Function{pkg.Func(name)}, func(instr ssa.Instruction) {
		if call, ok := instr.(ssa.CallInstruction); ok {
			if _, literal := call.Common().Value.(*ssa.MakeClosure); !literal {
				if fn, ok := call.Common().Value.(*ssa.Function); !ok || fn.Parent() == nil {
					last = call
				}
			}
		}
	})
	if last == nil {
		t.Fatalf("no call in %s", name)
	}
	return SiteOf(last, vars)
}

func TestSiteOfLocalFuncVar(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "p.go", funcVarSource, 0)
	if err != nil {
		t.Fatal(err)
	}
	pkg, _, err := ssautil.BuildPackage(&types.Config{Importer: importer.Default()}, fset,
		types.NewPackage("p", "p"), []*ast.File{file}, ssa.SanityCheckFunctions)
	if err != nil {
		t.Fatal(err)
	}
	var funcs []*ssa.Function
	for _, m := range pkg.Members {
		if fn, ok := m.(*ssa.Function); ok {
			funcs = append(funcs, fn)
		}
	}
	vars := CollectFuncVars(funcs)

	tests := []struct {
		fn       string
		kind     SiteKind
		declared string // "" for no callee
	}{
		{fn: "closure", kind: SiteClosure, declared: "closure$1"},
		{fn: "bound", kind: SiteBound, declared: "m"},
		{fn: "static", kind: SiteStatic, declared: "top"},
		{fn: "nested", kind: SiteStatic, declared: "top"},
		{fn: "chained", kind: SiteStatic, declared: "top"},
		{fn: "viaGlobal", kind: SiteFuncVar, declared: "init$1"},
		{fn: "storedTwice", kind: SiteDynamic},
		{fn: "storedInClosure", kind: SiteDynamic},
		{fn: "storedInNestedClosure", kind: SiteDynamic},
		{fn: "escaped", kind: SiteDynamic},
		{fn: "addressStored", kind: SiteDynamic},
		{fn: "cycle", kind: SiteDynamic},
		{fn: "param", kind: SiteDynamic},
		{fn: "received", kind: SiteDynamic},
		{fn: "invoke", kind: SiteInvoke},
	}
	for _, tt := range tests {
		site := funcVarCallOf(t, pkg, vars, tt.fn)
		if site.Kind != tt.kind {
			t.Errorf("%s: Kind = %v, want %v", tt.fn, site.Kind, tt.kind)
		}
		got := ""
		if site.Declared != nil {
			got = site.Declared.Name()
		}
		if got != tt.declared {
			t.Errorf("%s: Declared = %q, want %q", tt.fn, got, tt.declared)
		}
	}
}
