package callback

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"testing"

	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"

	"github.com/mpyw/gormreuse/internal/ssa/callsite"
)

// holderSrc stores a, once, into each kind of holder, and reads each holder
// back in a function named read*.
const holderSrc = `package p

import "unsafe"

func a() {}
func b() {}

type box struct{ f func() }
type val struct{ f func() }
type conv struct{ f func() }
type unsafeConv struct{ f func() }
type conv2 struct{ f func() }
type twice struct{ f func() }
type addr struct{ f func() }
type Exp struct{ F func() }

var g = a
var g2 = a

func setG2() { g2 = b }

func mkBox() *box     { return &box{f: a} }
func mkVal() val      { return val{f: a} }
func mkConv() *conv   { return &conv{f: a} }
func mkUnsafeConv() *unsafeConv { return &unsafeConv{f: a} }
func mkTwiceA() twice { return twice{f: a} }
func mkTwiceB() twice { return twice{f: b} }
func mkAddr() *addr   { return &addr{f: a} }
func mkExp() *Exp     { return &Exp{F: a} }

func toConv2(c conv) conv2         { return conv2(c) }
func toPointer(c *unsafeConv) unsafe.Pointer { return unsafe.Pointer(c) }
func takeAddr(x *addr) *func()     { return &x.f }

func readBoxByValue(x box) func() { return x.f }
func readVal() func()             { return mkVal().f }
func readTwiceOfCall() func()     { return mkTwiceA().f }
func readBoxByPtr(x *box) func()  { return x.f }
func readConv(x *conv) func()     { return x.f }
func readUnsafeConv(x *unsafeConv) func() { return x.f }
func readTwice(x twice) func()    { return x.f }
func readAddr(x *addr) func()     { return x.f }
func readExp(x *Exp) func()       { return x.F }
func readG() func()               { return g }
func readG2() func()              { return g2 }
func readNeg(x int) int           { return -x }
`

// buildHolderProgram builds holderSrc in debug mode, so that DebugRefs refer
// to the addresses of the fields, and returns its functions with the package
// initializer, and the members by name.
func buildHolderProgram(t *testing.T) (funcs []*ssa.Function, members map[string]*ssa.Function) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "p.go", holderSrc, 0)
	if err != nil {
		t.Fatal(err)
	}
	pkg, _, err := ssautil.BuildPackage(&types.Config{Importer: importer.Default()}, fset,
		types.NewPackage("p", "p"), []*ast.File{file}, ssa.GlobalDebug)
	if err != nil {
		t.Fatal(err)
	}
	members = make(map[string]*ssa.Function)
	for name, m := range pkg.Members {
		if fn, ok := m.(*ssa.Function); ok {
			members[name] = fn
			funcs = append(funcs, fn)
		}
	}
	return funcs, members
}

// holderReturned returns the value fn returns.
func holderReturned(t *testing.T, fn *ssa.Function) ssa.Value {
	t.Helper()
	for _, b := range fn.Blocks {
		for _, instr := range b.Instrs {
			if ret, ok := instr.(*ssa.Return); ok && len(ret.Results) == 1 {
				return ret.Results[0]
			}
		}
	}
	t.Fatalf("%s returns nothing", fn.Name())
	return nil
}

// TestHolderHeldValue checks which reads resolve to the stored function.
func TestHolderHeldValue(t *testing.T) {
	t.Parallel()
	funcs, members := buildHolderProgram(t)
	h := collectHolders(funcs, callsite.CollectFuncVars(funcs))
	tests := []struct {
		read string
		want *ssa.Function
	}{
		{"readBoxByValue", members["a"]}, // a value parameter, spilled to a local
		{"readBoxByPtr", members["a"]},   // a load of the field's address
		{"readVal", members["a"]},        // the field of a struct in a register
		{"readTwiceOfCall", nil},         // the same, stored twice
		{"readG", members["a"]},          // a variable stored once
		{"readConv", nil},                // converted to another struct type
		{"readUnsafeConv", nil},          // converted to unsafe.Pointer
		{"readTwice", nil},               // stored twice
		{"readAddr", nil},                // its address is taken
		{"readExp", nil},                 // exported
		{"readG2", nil},                  // a variable stored twice
		{"readNeg", nil},                 // not a load
	}
	for _, tt := range tests {
		got := h.heldValue(holderReturned(t, members[tt.read]))
		if tt.want == nil && got != nil || tt.want != nil && got != ssa.Value(tt.want) {
			t.Errorf("%s: held value = %v, want %v", tt.read, got, tt.want)
		}
	}
	var none *holders
	if got := none.heldValue(holderReturned(t, members["readG"])); got != nil {
		t.Errorf("nil holders: held value = %v, want nil", got)
	}
}

// TestHolderFeedsOnlyCallbacks checks the addresses that are not holders,
// a holder read another way, and a cycle.
func TestHolderFeedsOnlyCallbacks(t *testing.T) {
	t.Parallel()
	funcs, members := buildHolderProgram(t)
	h := collectHolders(funcs, callsite.CollectFuncVars(funcs))
	byPtr := holderReturned(t, members["readBoxByPtr"]).(*ssa.UnOp)
	readG := holderReturned(t, members["readG"]).(*ssa.UnOp)
	readConv := holderReturned(t, members["readConv"]).(*ssa.UnOp)
	readG2 := holderReturned(t, members["readG2"]).(*ssa.UnOp)

	// Each holder is read by a return, which is not a callback position.
	if h.feedsOnlyCallbacks(byPtr.X, nil) {
		t.Error("a field read by a return should not feed only callbacks")
	}
	if h.feedsOnlyCallbacks(readG.X, nil) {
		t.Error("a variable read by a return should not feed only callbacks")
	}
	if h.feedsOnlyCallbacks(readConv.X, nil) {
		t.Error("a field that is not held should not feed only callbacks")
	}
	if h.feedsOnlyCallbacks(readG2.X, nil) {
		t.Error("a variable that is not held should not feed only callbacks")
	}
	if h.feedsOnlyCallbacks(byPtr, nil) {
		t.Error("a value that is not an address should not feed only callbacks")
	}
	h.checking[readG.X] = true
	if h.feedsOnlyCallbacks(readG.X, nil) {
		t.Error("a holder being checked should not feed only callbacks")
	}
}
