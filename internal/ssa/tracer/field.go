package tracer

import (
	"go/token"
	"go/types"
	"slices"
	"strings"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/ssa/callsite"
	"github.com/mpyw/gormreuse/internal/ssa/cfg"
)

// traceFieldStore traces a struct field access by finding Store instructions.
//
// When a struct field is accessed:
//
//	type Handler struct { db *gorm.DB }
//	h.db.Find(nil)
//
// SSA represents this as:
//
//	t1 = FieldAddr h.db        // Get address of field
//	t2 = UnOp * t1             // Dereference to get value
//	                           // (traced here to find the Store)
//
// This function finds the Store that wrote to the same field and can reach the
// load.
//
//declscope:shared // root.go's tracePointerLoad sends field loads here
func (t *RootTracer) traceFieldStore(fa *ssa.FieldAddr, load *ssa.UnOp, visited map[ssa.Value]bool, loopInfo *cfg.LoopInfo) ssa.Value {
	// Single-root: trace the first value stored into the same field.
	if vals := FieldStoredValues(fa, load); len(vals) > 0 {
		return t.trace(vals[0], visited, loopInfo)
	}
	return nil
}

// fieldBaseAddrs returns the addresses whose stores a field access through root
// can see. Two forms of root do not hold the stores themselves (#181):
//
//   - A FreeVar: a closure reads a captured struct through it, while the stores
//     are in the enclosing function, on the captured Alloc. The binding stands
//     for it.
//   - A load of a pointer variable (`*h`, where `h := &T{...}` is captured or
//     has its address taken, so it lives in an Alloc): every access loads h
//     afresh, so no two accesses share the load. Each pointer stored into the
//     variable stands for it.
//
// Any other root stands for itself.
//
//declscope:shared // store.go searches the stores at each address it returns
func fieldBaseAddrs(root ssa.Value, seen map[ssa.Value]bool) []ssa.Value {
	if seen[root] {
		return nil
	}
	seen[root] = true
	switch r := root.(type) {
	case *ssa.FreeVar:
		if binding := callsite.FreeVarBinding(r); binding != nil {
			return fieldBaseAddrs(binding, seen)
		}
	case *ssa.UnOp:
		if r.Op != token.MUL {
			break
		}
		slot := r.X
		if fv, ok := slot.(*ssa.FreeVar); ok {
			slot = callsite.FreeVarBinding(fv)
		}
		if alloc, ok := slot.(*ssa.Alloc); ok {
			var bases []ssa.Value
			for _, ptr := range AllocStoredValues(alloc) {
				bases = append(bases, fieldBaseAddrs(ptr, seen)...)
			}
			return bases
		}
	}
	return []ssa.Value{root}
}

// FieldRoot is the mutable root of a *gorm.DB field of a struct that a call
// returned, by value (#181) or through a pointer (#250):
//
//	s := mk(db)          // t1 = mk(db); *t0 = t1
//	s.DB.Where("a")      // root: FieldRoot{t1, [DB]}
//	s.DB.Where("b")      // the same FieldRoot: second branch
//
// It extends "a call returning *gorm.DB is a root" to a call returning a struct
// that holds one. The SSA has no value for "field DB of t1", so the tracer
// makes one per (call, field path) and returns the same pointer for each (see
// RootTracer.internFieldRoot). Path indexes the struct the call returns, or
// the one its pointer result points to. Fields read through a parameter or a
// receiver stay untracked.
type FieldRoot struct {
	Call *ssa.Call
	Path []int
}

// fieldRootStruct returns the struct that a FieldRoot path step selects from:
// t itself, or what it points to.
func fieldRootStruct(t types.Type) *types.Struct {
	if ptr, ok := t.Underlying().(*types.Pointer); ok {
		t = ptr.Elem()
	}
	return t.Underlying().(*types.Struct)
}

// Name names the field of the call: "t1.DB".
func (r *FieldRoot) Name() string { return r.Call.Name() + r.pathString() }

// String describes the field of the call: "makeTwoDB().DB".
func (r *FieldRoot) String() string { return r.Call.String() + r.pathString() }

// Type returns the type of the field.
func (r *FieldRoot) Type() types.Type {
	t := r.Call.Type()
	for _, i := range r.Path {
		t = fieldRootStruct(t).Field(i).Type()
	}
	return t
}

// Parent returns the function that makes the call.
func (r *FieldRoot) Parent() *ssa.Function { return r.Call.Parent() }

// Referrers returns nil: no instruction refers to a FieldRoot.
func (r *FieldRoot) Referrers() *[]ssa.Instruction { return nil }

// Pos returns the position of the call, which the diagnostics name as the root.
func (r *FieldRoot) Pos() token.Pos { return r.Call.Pos() }

// Block returns the block of the call, which defines the field: a call inside a
// loop returns a new struct on each iteration (#250). See RootBlock.
func (r *FieldRoot) Block() *ssa.BasicBlock { return r.Call.Block() }

//declscope:shared // root.go's internFieldRoot keys each FieldRoot by it
func (r *FieldRoot) pathString() string {
	var b strings.Builder
	t := r.Call.Type()
	for _, i := range r.Path {
		field := fieldRootStruct(t).Field(i)
		b.WriteString("." + field.Name())
		t = field.Type()
	}
	return b.String()
}

// fieldAddrPath resolves fa to the address it ultimately selects from together
// with the chain of field indices it walks to get there:
//
//	t1 = &h.inner        // FieldAddr
//	t2 = &t1.db          // FieldAddr    → fieldAddrPath(t2) = (h, [0 0])
//
// Matching on the whole path rather than on the immediate base value is what
// makes promoted fields (`outer{db: q}`, where db is embedded, allowed since
// Go 1.27) traceable: the SSA builder emits a fresh `&h.inner` FieldAddr for
// every access, so two accesses to the same nested field are never the same
// base value.
//
//declscope:shared // store.go matches stores by the same field path
func fieldAddrPath(fa *ssa.FieldAddr) (root ssa.Value, path []int) {
	path = []int{fa.Field}
	root = fa.X
	for {
		parent, ok := root.(*ssa.FieldAddr)
		if !ok {
			return root, path
		}
		path = append([]int{parent.Field}, path...)
		root = parent.X
	}
}

// SameField reports whether a and b select the same field path from the same
// address. Two accesses to one field are separate FieldAddr chains, so this
// compares them as FieldStoredValues does.
func SameField(a, b *ssa.FieldAddr) bool {
	rootA, pathA := fieldAddrPath(a)
	rootB, pathB := fieldAddrPath(b)
	return rootA == rootB && slices.Equal(pathA, pathB)
}

// traceAllFieldStores finds ALL possible roots from stores to a struct field.
//
// Unlike traceFieldStore which returns the first Store found, this collects
// all roots from ALL Store instructions that write the same field and reach the
// load. This is needed for conditional code where different branches may store
// different values:
//
//	if cond {
//	    h.db = q1  // Store #1
//	} else {
//	    h.db = q2  // Store #2
//	}
//	h.db.Find(nil)  // Need to check BOTH q1 and q2
//
//declscope:shared // root.go's traceAllPointerLoads sends field loads here
func (t *RootTracer) traceAllFieldStores(fa *ssa.FieldAddr, load *ssa.UnOp, search *allRootsSearch, loopInfo *cfg.LoopInfo) []ssa.Value {
	var roots []ssa.Value
	for _, v := range FieldStoredValues(fa, load) {
		roots = append(roots, t.traceAll(v, search, loopInfo)...)
	}
	return roots
}
