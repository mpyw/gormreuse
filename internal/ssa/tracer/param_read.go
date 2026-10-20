package tracer

import (
	"go/token"
	"go/types"
	"slices"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/directive"
	"github.com/mpyw/gormreuse/internal/ssa/callsite"
	"github.com/mpyw/gormreuse/internal/ssa/cfg"
)

// ParamReadValues returns the values that read selects from the argument of
// the call at site, as reuse detection reads the same selector written at the
// call: the stores into the field that reach the call, or the FieldRoot of a
// struct that a call returned.
func (t *RootTracer) ParamReadValues(site callsite.Site, read directive.ParamRead) []ssa.Value {
	return t.stores.paramReadSelect(site.ParamArg(read.Param), read.Path, site.Instr)
}

// ParamReadSelected returns the values that the selector path reads from x
// at at, as ParamReadValues reads it from an argument. The inference of
// immutable-return reads the fields of a struct a function returns with it.
func (t *RootTracer) ParamReadSelected(x ssa.Value, path []int, at ssa.Instruction) []ssa.Value {
	return t.stores.paramReadSelect(x, path, at)
}

// paramReadCrossesPointer reports whether the path of r passes through a
// pointer field. Reuse detection makes no such FieldRoot: only a call's
// struct and the structs it holds by value are its fields. A read of a
// getter makes one, and an immutable-return struct result, inferred from
// those fields, says nothing about it.
//
//declscope:shared // root.go's internFieldRoot keeps such a FieldRoot a root
func paramReadCrossesPointer(r *FieldRoot) bool {
	t := r.Call.Type()
	for _, i := range r.Path[:len(r.Path)-1] {
		t = paramReadField(t, i).Type()
		if paramReadIsPointer(t) {
			return true
		}
	}
	return false
}

// traceParamReads is trace for a call of a callee with reads: the first root
// of the values the reads select at the call.
//
//declscope:shared // root.go's traceCall follows a getter's call with it
func (t *RootTracer) traceParamReads(site callsite.Site, reads []directive.ParamRead, visited map[ssa.Value]bool, loopInfo *cfg.LoopInfo) ssa.Value {
	for _, read := range reads {
		for _, v := range t.ParamReadValues(site, read) {
			if root := t.trace(v, visited, loopInfo); root != nil {
				return root
			}
		}
	}
	return nil
}

// traceAllParamReads is traceAll for a call of a callee with reads.
//
//declscope:shared // root.go's traceAllUnvisited follows a getter's call with it
func (t *RootTracer) traceAllParamReads(site callsite.Site, reads []directive.ParamRead, search *allRootsSearch, loopInfo *cfg.LoopInfo) []ssa.Value {
	var roots []ssa.Value
	for _, read := range reads {
		for _, v := range t.ParamReadValues(site, read) {
			roots = append(roots, t.traceAll(v, search, loopInfo)...)
		}
	}
	return roots
}

// paramReadSelect returns the values that the selector path reads from x at
// at. Fields that x holds by value are one address, read through the stores
// that reach at as a FieldAddr chain is. A pointer field ends that address:
// the rest of the path is selected from each value stored into it.
func (m *storeMemo) paramReadSelect(x ssa.Value, path []int, at ssa.Instruction) []ssa.Value {
	if len(path) == 0 {
		return []ssa.Value{x}
	}
	n := 1
	for ft := paramReadField(x.Type(), path[0]).Type(); n < len(path) && !paramReadIsPointer(ft); n++ {
		ft = paramReadField(ft, path[n]).Type()
	}
	var vals []ssa.Value
	if root, ok := x.(*FieldRoot); ok {
		// The field of a struct that a call returned is the call's FieldRoot.
		vals = []ssa.Value{&FieldRoot{Call: root.Call, Path: append(slices.Clone(root.Path), path[:n]...)}}
	} else if paramReadIsPointer(x.Type()) {
		vals = m.paramReadPointee(x, path[:n], at, make(map[ssa.Value]bool))
	} else {
		vals = m.structFieldStoredValues(x, slices.Clone(path[:n]), make(map[ssa.Value]bool))
	}
	if n == len(path) {
		return vals
	}
	var selected []ssa.Value
	for _, v := range vals {
		selected = append(selected, m.paramReadSelect(v, path[n:], at)...)
	}
	return selected
}

// paramReadPointee returns the values stored into the field that path
// selects from what the pointer x points to, as read at at. Two forms of x
// are not the address the stores are made through (#376):
//
//   - The address of a field, such as &o.H for o.Get() with lib.H embedded
//     in o, is its base with the field's path in front, as a FieldAddr chain
//     written in place is.
//   - A pointer read from a field, such as the embedded *lib.H of
//     PO{&lib.H{...}}, also stands for each pointer stored into that field.
//
// seen stops pointer fields that are stored into each other.
func (m *storeMemo) paramReadPointee(x ssa.Value, path []int, at ssa.Instruction, seen map[ssa.Value]bool) []ssa.Value {
	if seen[x] {
		return nil
	}
	seen[x] = true
	root, prefix := x, []int(nil)
	if fa, ok := x.(*ssa.FieldAddr); ok {
		root, prefix = fieldAddrPath(fa)
	}
	vals := m.fieldStoredValuesAt(at.Parent(), root, slices.Concat(prefix, path), at)
	var ptrs []ssa.Value
	switch p := x.(type) {
	case *ssa.Field:
		ptrs = m.fieldValueStoredValues(p)
	case *ssa.UnOp:
		if fa, ok := p.X.(*ssa.FieldAddr); ok && p.Op == token.MUL {
			ptrs = m.fieldAddrStoredValues(fa, p)
		}
	}
	for _, ptr := range ptrs {
		if root, ok := ptr.(*FieldRoot); ok {
			vals = append(vals, &FieldRoot{Call: root.Call, Path: slices.Concat(root.Path, path)})
		} else {
			vals = append(vals, m.paramReadPointee(ptr, path, at, seen)...)
		}
	}
	return vals
}

// paramReadField returns field i of the struct that t is or points to.
func paramReadField(t types.Type, i int) *types.Var {
	return fieldRootStruct(t).Field(i)
}

// paramReadIsPointer reports whether t is a pointer.
func paramReadIsPointer(t types.Type) bool {
	_, ok := t.Underlying().(*types.Pointer)
	return ok
}
