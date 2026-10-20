package tracer

import (
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
		vals = m.fieldStoredValuesAt(at.Parent(), x, slices.Clone(path[:n]), at)
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

// paramReadField returns field i of the struct that t is or points to.
func paramReadField(t types.Type, i int) *types.Var {
	return fieldRootStruct(t).Field(i)
}

// paramReadIsPointer reports whether t is a pointer.
func paramReadIsPointer(t types.Type) bool {
	_, ok := t.Underlying().(*types.Pointer)
	return ok
}
