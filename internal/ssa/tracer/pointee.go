package tracer

import (
	"go/token"
	"go/types"
	"slices"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/ssa/cfg"
	"github.com/mpyw/gormreuse/internal/typeutil"
)

// PointeeRoot is the *gorm.DB that a call may store into a local variable
// whose address it takes, as scanInto does in (#345):
//
//	var q *gorm.DB
//	scanInto(&q, db)   // may run *p = db.Where("x")
//	q.Find(&a)
//	q.Count(&n)        // the second use of what scanInto stored
//
// A read of the variable that the call reaches, with no store in between, may
// see it. The call may also leave the variable as it was, so the read sees
// the values stored before the call as well. The call is not a *gorm.DB, so
// no fix appends Session() to it.
type PointeeRoot struct {
	Call *ssa.Call
	Var  *ssa.Alloc
}

// Name names the variable after the call: "*t0@t3".
func (r *PointeeRoot) Name() string { return "*" + r.Var.Name() + "@" + r.Call.Name() }

// String describes what the call stored: "*q after scanInto(t0, db)".
func (r *PointeeRoot) String() string { return "*" + r.Var.Comment + " after " + r.Call.String() }

// Type returns the type of the variable, *gorm.DB.
func (r *PointeeRoot) Type() types.Type { return r.Var.Type().(*types.Pointer).Elem() }

// Parent returns the function that makes the call.
func (r *PointeeRoot) Parent() *ssa.Function { return r.Call.Parent() }

// Referrers returns nil: no instruction refers to a PointeeRoot.
func (r *PointeeRoot) Referrers() *[]ssa.Instruction { return nil }

// Pos returns the position of the call, which the diagnostics name as the root.
func (r *PointeeRoot) Pos() token.Pos { return r.Call.Pos() }

// Block returns the block of the call, which defines the value. See RootBlock.
func (r *PointeeRoot) Block() *ssa.BasicBlock { return r.Call.Block() }

// pointeeKey is the call and variable that pointeeRootOf interns a
// PointeeRoot by.
type pointeeKey struct {
	call *ssa.Call
	v    *ssa.Alloc
}

// pointeeBlockKey is a search for the calls that reach the start of block.
type pointeeBlockKey struct {
	v     *ssa.Alloc
	block *ssa.BasicBlock
}

// pointeeMemo keeps, per variable, the calls that take its address, and per
// block, the calls that reach its start. Reads of one variable then share one
// walk per block, as storeMemo's do.
//
//declscope:shared // root.go's RootTracer holds one
type pointeeMemo struct {
	//declscope:private
	roots map[pointeeKey]*PointeeRoot
	//declscope:private
	calls map[*ssa.Alloc][]*ssa.Call
	//declscope:private
	blocks map[pointeeBlockKey][]*ssa.Call
}

// pointeeRootsAt returns the PointeeRoots of v that a read of it at load may
// see: those of the calls that take v's address and reach load with no store
// into v in between. A nil load, or one in another function such as a closure
// that captures v, sees every such call. A call to a //gormreuse:pure function
// stores nothing.
//
//declscope:shared // store.go's traceAllocStore and traceAllAllocStores add them to what a read of a variable sees
func (t *RootTracer) pointeeRootsAt(v *ssa.Alloc, load ssa.Instruction) []ssa.Value {
	calls := t.pointeeCalls(v)
	if len(calls) == 0 {
		return nil
	}
	if load != nil && load.Parent() == v.Parent() {
		calls = t.pointeeCallsReaching(v, load)
	}
	roots := make([]ssa.Value, 0, len(calls))
	for _, call := range calls {
		roots = append(roots, t.pointeeRootOf(call, v))
	}
	return roots
}

// pointeeCalls returns the calls that take the address of v, a local
// *gorm.DB variable, as an argument.
func (t *RootTracer) pointeeCalls(v *ssa.Alloc) []*ssa.Call {
	m := &t.pointees
	if calls, ok := m.calls[v]; ok {
		return calls
	}
	if m.calls == nil {
		m.calls = map[*ssa.Alloc][]*ssa.Call{}
	}
	var calls []*ssa.Call
	if ptr, ok := v.Type().(*types.Pointer); ok && typeutil.IsGormDB(ptr.Elem()) {
		for _, ref := range *v.Referrers() {
			call, ok := ref.(*ssa.Call)
			if !ok || !slices.Contains(call.Call.Args, ssa.Value(v)) {
				continue
			}
			if declared := t.Site(call).Declared; declared != nil && t.IsPureFunction(declared) {
				continue
			}
			calls = append(calls, call)
		}
	}
	m.calls[v] = calls
	return calls
}

// pointeeCallsReaching returns the calls of pointeeCalls(v) that reach load,
// in v's function, with no store into v in between.
func (t *RootTracer) pointeeCallsReaching(v *ssa.Alloc, load ssa.Instruction) []*ssa.Call {
	start := load.Block()
	if call, done := pointeeLastWrite(v, start.Instrs[:slices.Index(start.Instrs, load)]); done {
		if call == nil {
			return nil
		}
		return []*ssa.Call{call}
	}
	m := &t.pointees
	key := pointeeBlockKey{v, start}
	if calls, ok := m.blocks[key]; ok {
		return calls
	}
	if m.blocks == nil {
		m.blocks = map[pointeeBlockKey][]*ssa.Call{}
	}
	var calls []*ssa.Call
	visited := map[*ssa.BasicBlock]bool{}
	work := slices.Clone(start.Preds)
	for len(work) > 0 {
		block := work[len(work)-1]
		work = work[:len(work)-1]
		if visited[block] {
			continue
		}
		visited[block] = true
		if call, done := pointeeLastWrite(v, block.Instrs); done {
			if call != nil {
				calls = append(calls, call)
			}
			continue
		}
		work = append(work, block.Preds...)
	}
	m.blocks[key] = calls
	return calls
}

// pointeeLastWrite returns the last instruction of instrs that writes v: a
// call that takes its address, or a store, for which call is nil. done is
// false when instrs writes nothing.
func pointeeLastWrite(v *ssa.Alloc, instrs []ssa.Instruction) (call *ssa.Call, done bool) {
	for _, instr := range slices.Backward(instrs) {
		switch instr := instr.(type) {
		case *ssa.Store:
			if instr.Addr == ssa.Value(v) {
				return nil, true
			}
		case *ssa.Call:
			if slices.Contains(instr.Call.Args, ssa.Value(v)) {
				return instr, true
			}
		}
	}
	return nil, false
}

// pointeeRootOf returns the one PointeeRoot of call and v.
func (t *RootTracer) pointeeRootOf(call *ssa.Call, v *ssa.Alloc) *PointeeRoot {
	m := &t.pointees
	key := pointeeKey{call, v}
	if r, ok := m.roots[key]; ok {
		return r
	}
	if m.roots == nil {
		m.roots = map[pointeeKey]*PointeeRoot{}
	}
	r := &PointeeRoot{Call: call, Var: v}
	m.roots[key] = r
	return r
}

// PointeeRoots returns the mutable roots of the *gorm.DB that ptr, the
// address of a local variable, points to where at runs, as in applyP(&q)
// (#345). They are what a read of the variable at at would see. It is nil
// for any other pointer.
func (t *RootTracer) PointeeRoots(ptr ssa.Value, at ssa.Instruction, loopInfo *cfg.LoopInfo) []ssa.Value {
	v, ok := ptr.(*ssa.Alloc)
	if !ok {
		return nil
	}
	if elem, ok := v.Type().(*types.Pointer); !ok || !typeutil.IsGormDB(elem.Elem()) {
		return nil
	}
	root := t.traceAllocStore(v, at, make(map[ssa.Value]bool), loopInfo)
	if root == nil {
		return nil
	}
	roots := []ssa.Value{root}
	for _, r := range t.traceAllAllocStores(v, at, newAllRootsSearch(make(map[ssa.Value]bool)), loopInfo) {
		if r != root {
			roots = append(roots, r)
		}
	}
	return roots
}
