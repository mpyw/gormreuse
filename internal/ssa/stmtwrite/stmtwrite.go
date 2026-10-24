// Package stmtwrite answers one question for the pure contract and for the
// inferred immutable-param (#405): does a function's body write the query of
// its *gorm.DB parameter through a direct path?
//
// A direct path starts at a *gorm.DB parameter itself and goes only through
// its fields and the maps and slices its Statement holds. These are writes:
//
//   - `*db = v` and `db.Statement = v`;
//   - `db.Statement.F = v`, also into a field of a field;
//   - `db.Statement.M[k] = v`, `delete(db.Statement.M, k)` and
//     `clear(db.Statement.M)`;
//   - `db.Statement.S[i] = v`, `copy(db.Statement.S, …)` and
//     `clear(db.Statement.S)`.
//
// The parameter may be read in a closure, a go or a defer statement in the
// body, through the captured variable that holds it, when nothing assigns
// that variable again. A local variable that go/ssa keeps in a register, as
// in `st := db.Statement; st.Distinct = true`, is the same value, so it is a
// direct path too.
//
// A handle read from the memory is not followed: `db.Statement.DB` is the
// parent of a Session or Debug copy that shares the Statement, not the
// argument. Any other path is not followed either, and is a known
// limitation: a *gorm.Statement parameter, which may be a fresh Statement
// that no argument holds, a variable
// that is assigned again, captured or has its address taken, a copy such as
// `c := *db.Statement`, a Session or Debug result, a parameter that holds a
// *gorm.DB (a **gorm.DB, a slice, a map or a struct), an append result, a
// map lookup, a conversion, a merge, or a call. Each of these may write
// memory that the argument does not hold, so a report of it may be false.
//
// The Error, RowsAffected and Config fields of a gorm.DB are left out. The
// first two hold the outcome of a query, as AddError does. Config holds no
// clause, though a write to it changes how the caller's later queries run.
package stmtwrite

import (
	"go/types"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/typeutil"
)

// kind is what a followed value is.
type kind int

const (
	// handle is the parameter, or its Statement.
	handle kind = iota
	// addr is the address of a field or an element of that memory.
	addr
	// mapRef is a map that the memory holds.
	mapRef
	// sliceRef is a slice that the memory holds.
	sliceRef
)

type step struct {
	v ssa.Value
	k kind
}

// walker walks values forward and records the writes it finds.
type walker struct {
	writes map[ssa.Instruction]bool
	seen   map[step]bool
	work   []step
}

// Writes returns the instructions that write, through a direct path, the
// query memory of seeds (see Seeds).
func Writes(seeds map[ssa.Value]*ssa.Parameter) map[ssa.Instruction]bool {
	w := &walker{writes: map[ssa.Instruction]bool{}, seen: map[step]bool{}}
	for h := range seeds {
		w.push(step{v: h, k: handle})
	}
	w.run()
	return w.writes
}

// Seeds returns the values that are surely a *gorm.DB parameter of fn, each
// mapped to that parameter: the parameter and the values directHandles gives
// for it. A method's receiver is not an argument.
func Seeds(fn *ssa.Function) map[ssa.Value]*ssa.Parameter {
	seeds := make(map[ssa.Value]*ssa.Parameter)
	for i, p := range fn.Params {
		if i == 0 && fn.Signature.Recv() != nil {
			continue
		}
		if _, ptr := types.Unalias(p.Type()).(*types.Pointer); ptr && typeutil.IsGormDB(p.Type()) { // rejects **gorm.DB
			for _, h := range directHandles(p) {
				seeds[h] = p
			}
		}
	}
	return seeds
}

// WrittenParam returns the parameter whose memory addr, the address of a
// write that Writes found, is part of: the seed the address is reached from
// through fields, elements and loads, the steps of a direct path.
func WrittenParam(addr ssa.Value, seeds map[ssa.Value]*ssa.Parameter) *ssa.Parameter {
	for seeds[addr] == nil {
		switch a := addr.(type) {
		case *ssa.FieldAddr:
			addr = a.X
		case *ssa.IndexAddr:
			addr = a.X
		default:
			addr = a.(*ssa.UnOp).X // a load of a field
		}
	}
	return seeds[addr]
}

// directHandles returns param and the values that are surely param: the
// loads of the variable that holds it when a closure captures it. go/ssa
// keeps such a parameter in a variable of its name, stored in the entry
// block, that the function and its closures load. The variable counts only
// when the parameter is the one value stored into it, and its address goes
// to nothing but loads and closures. A variable of the body that is assigned
// the parameter, as in `var x *gorm.DB; …; x = db`, is not one.
func directHandles(param *ssa.Parameter) []ssa.Value {
	handles := []ssa.Value{param}
	for _, r := range *param.Referrers() {
		st, ok := r.(*ssa.Store)
		if !ok || st.Val != ssa.Value(param) || st.Block() != param.Parent().Blocks[0] {
			continue
		}
		if spill, ok := st.Addr.(*ssa.Alloc); ok && spill.Comment == param.Name() {
			handles = append(handles, spillLoads(spill, st)...)
		}
	}
	return handles
}

// spillLoads returns the loads of the variable spill, in its function and
// in the closures that capture it, when its only store is spilled. It returns
// nil when anything else uses its address.
func spillLoads(spill *ssa.Alloc, spilled *ssa.Store) []ssa.Value {
	var loads []ssa.Value
	vars := []ssa.Value{spill}
	for i := 0; i < len(vars); i++ {
		for _, r := range *vars[i].Referrers() {
			switch r := r.(type) {
			case *ssa.UnOp:
				loads = append(loads, r) // an address is only loaded
			case *ssa.MakeClosure:
				fn := r.Fn.(*ssa.Function)
				for j, b := range r.Bindings {
					if b == vars[i] {
						vars = append(vars, fn.FreeVars[j])
					}
				}
			case *ssa.DebugRef:
			default:
				if r != spilled {
					return nil
				}
			}
		}
	}
	return loads
}

func (w *walker) push(s step) {
	if !w.seen[s] {
		w.seen[s] = true
		w.work = append(w.work, s)
	}
}

// pushLoaded follows v, a value loaded from the memory, by its type: the
// Statement, a map or a slice. A *gorm.DB read from the memory, as
// db.Statement.DB, may be another handle than the argument. Any other value
// is a copy.
func (w *walker) pushLoaded(v ssa.Value) {
	t := v.Type()
	switch types.Unalias(t).Underlying().(type) {
	case *types.Map:
		w.push(step{v: v, k: mapRef})
	case *types.Slice:
		w.push(step{v: v, k: sliceRef})
	case *types.Pointer:
		if typeutil.IsGormStatement(t) { // a pointer field, so not **gorm.Statement
			w.push(step{v: v, k: handle})
		}
	}
}

func (w *walker) run() {
	for len(w.work) > 0 {
		s := w.work[len(w.work)-1]
		w.work = w.work[:len(w.work)-1]
		for _, r := range *s.v.Referrers() {
			w.visit(s, r)
		}
	}
}

// visit follows one use r of a followed value s along a direct path.
func (w *walker) visit(s step, r ssa.Instruction) {
	v := s.v
	switch r := r.(type) {
	case *ssa.Store:
		if r.Addr == v { // a handle or an address
			w.writes[r] = true
		}
	case *ssa.FieldAddr:
		if s.k == handle && isGormDBStateField(r.X.Type(), r.Field) {
			return
		}
		if s.k == handle || s.k == addr {
			w.push(step{v: r, k: addr})
		}
	case *ssa.UnOp:
		if s.k == addr { // a load of a field or an element
			w.pushLoaded(r)
		}
	case *ssa.MapUpdate:
		if r.Map == v {
			w.writes[r] = true
		}
	case *ssa.IndexAddr:
		if s.k == sliceRef {
			w.push(step{v: r, k: addr})
		}
	case ssa.CallInstruction:
		w.visitBuiltin(s, r)
	}
}

// visitBuiltin records a delete, clear or copy into a followed map or slice,
// also in a go or defer statement. The first argument is the one they
// change. Any other call hands the value on, which is not followed.
func (w *walker) visitBuiltin(s step, call ssa.CallInstruction) {
	common := call.Common()
	b, ok := common.Value.(*ssa.Builtin)
	if !ok || s.k != mapRef && s.k != sliceRef || common.Args[0] != s.v {
		return
	}
	switch b.Name() {
	case "delete", "clear", "copy":
		w.writes[call] = true
	}
}

// isGormDBStateField reports whether field i of the struct that ptr points to
// is the Error, RowsAffected or Config field of gorm.DB.
func isGormDBStateField(ptr types.Type, i int) bool {
	if !typeutil.IsGormDB(ptr) {
		return false
	}
	st := ptr.Underlying().(*types.Pointer).Elem().Underlying().(*types.Struct)
	switch st.Field(i).Name() {
	case "Error", "RowsAffected", "Config":
		return true
	}
	return false
}
