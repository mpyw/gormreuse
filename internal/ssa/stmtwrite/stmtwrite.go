// Package stmtwrite answers one question for the pure contract and for the
// inferred immutable-param (#405): may code write into the memory that holds
// the query of a *gorm.DB argument?
//
// That memory is the argument's gorm.DB and gorm.Statement, and the maps and
// slices the Statement holds, such as Clauses and Selects. The walk starts
// from a value that is, or is read from, that memory, and follows it forward:
//
//   - A store into it, a map update of it, or delete, clear or copy into it is
//     a Certain write. So is an append to a slice of it, as in s[:0], which
//     overwrites the elements after the slice in place.
//   - Handing a map, a slice or an address of it to a call that is not a
//     builtin or a known reader, or storing, sending or putting it in a map
//     anywhere but a local variable, is a Maybe write: other code may write
//     it.
//   - A scalar, a string or an interface read from it is a copy. Writes to it
//     change nothing. A boxed value is followed only into a type assertion,
//     since a value read from an interface field of the Statement, such as
//     Context, Model or Dest, is the application's own value.
//   - A pointer read from it is followed only when it points to a gorm.DB or
//     a gorm.Statement. Another pointer, such as Schema, points to memory the
//     query does not hold.
//   - A gorm.DB or a gorm.Statement copied into a local variable, as in
//     `c := *db.Statement`, is the function's own. Its maps, slices and
//     handles are still shared, unless a store into the same field of the
//     copy comes first on every path.
//
// The Error, RowsAffected and Config fields of a gorm.DB are left out. The
// first two hold the outcome of a query, as AddError does. Config is shared
// by every handle of the connection and holds no clause.
package stmtwrite

import (
	"go/types"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/ssa/callsite"
	"github.com/mpyw/gormreuse/internal/typeutil"
)

// Write is how surely an instruction writes the argument's query memory.
type Write int

const (
	// None is no write.
	None Write = iota
	// Maybe is a hand-off to code that may write it.
	Maybe
	// Certain is a write.
	Certain
)

// kind is what a followed value is.
type kind int

const (
	// handle is a *gorm.DB or *gorm.Statement whose memory is the argument's.
	handle kind = iota
	// addr is the address of a part of that memory.
	addr
	// ref is a map or a slice that shares that memory.
	ref
	// copied is a gorm.DB or gorm.Statement value copied from it. Its maps,
	// slices and handles are shared.
	copied
	// boxed is an interface that holds one of the above.
	boxed
	// variable is a part of a local variable that holds one of the above.
	// held is the kind of what it holds. A variable that holds a copied
	// value is a local copy (see walker.replacedBefore).
	variable
)

// slot is a part of a local variable: the variable's Alloc and the path of
// field and element indexes to the part.
type slot struct {
	root ssa.Value
	path string
}

// step is a value the walk reached, with what it is. A variable step has
// the part of the variable in sl instead of a value.
type step struct {
	v    ssa.Value
	sl   slot
	k    kind
	held kind
	// origin is the store that made a local copy, for variable steps that
	// hold a copied value. A store into the copy after it replaces a part.
	origin *ssa.Store
}

// walker walks values forward and records the writes it finds.
type walker struct {
	writes map[ssa.Instruction]Write
	seen   map[step]bool
	work   []step
}

func newWalker() *walker {
	return &walker{writes: map[ssa.Instruction]Write{}, seen: map[step]bool{}}
}

// FromValue reports the strongest write that code may make through v, a
// value read from the argument's query memory, such as a load of a field of
// its Statement.
func FromValue(v ssa.Value) Write {
	w := newWalker()
	w.pushRead(v)
	w.run()
	strongest := None
	for _, write := range w.writes {
		strongest = max(strongest, write)
	}
	return strongest
}

// FromHandles returns the instructions that write, or may write, the query
// memory of values that are the argument's: a *gorm.DB or *gorm.Statement, or
// a gorm.DB or gorm.Statement passed by value, whose maps, slices and handles
// are the argument's.
func FromHandles(values []ssa.Value) map[ssa.Instruction]Write {
	w := newWalker()
	for _, v := range values {
		w.pushRead(v)
	}
	w.run()
	return w.writes
}

func (w *walker) push(s step) {
	if !w.seen[s] {
		w.seen[s] = true
		w.work = append(w.work, s)
	}
}

// pushRead follows v, a value read from followed memory, by its type.
func (w *walker) pushRead(v ssa.Value) {
	if k, ok := kindOf(v.Type()); ok {
		w.push(step{v: v, k: k})
	}
}

func (w *walker) record(instr ssa.Instruction, write Write) {
	w.writes[instr] = max(w.writes[instr], write)
}

func (w *walker) run() {
	for len(w.work) > 0 {
		s := w.work[len(w.work)-1]
		w.work = w.work[:len(w.work)-1]
		switch s.k {
		case variable:
			w.visitVariable(s)
		case boxed:
			for _, r := range *s.v.Referrers() {
				w.visitBoxed(r)
			}
		default:
			for _, r := range *s.v.Referrers() {
				w.visit(s, r)
			}
		}
	}
}

// visit follows one use r of a followed value s that is not a variable or
// an interface.
func (w *walker) visit(s step, r ssa.Instruction) {
	v := s.v
	switch r := r.(type) {
	case *ssa.Store:
		switch {
		case r.Addr == v:
			w.record(r, Certain)
		case s.k == handle:
			// A handle stored elsewhere is an escape of the *gorm.DB,
			// which the callers check on their own. A local variable that
			// holds it still reaches its memory.
			if root, path, ok := localSlot(r.Addr); ok {
				w.push(step{sl: slot{root, path}, k: variable, held: handle})
			}
		default:
			w.store(s, r)
		}
	case *ssa.MapUpdate:
		switch {
		case r.Map == v:
			w.record(r, Certain)
		case s.k != handle:
			w.record(r, Maybe)
		}
	case *ssa.Send:
		if s.k != handle {
			w.record(r, Maybe)
		}
	case *ssa.FieldAddr:
		if !isGormDBStateField(r.X.Type(), r.Field) {
			w.push(step{v: r, k: addr})
		}
	case *ssa.IndexAddr:
		w.push(step{v: r, k: addr})
	case *ssa.Field, *ssa.UnOp:
		// A field of a copied value, or a load through a handle or an
		// address. A followed value is never a channel, so an UnOp of it
		// is a load.
		w.pushRead(r.(ssa.Value))
	case *ssa.Lookup:
		w.visitLookup(r)
	case *ssa.Slice:
		w.push(step{v: r, k: ref})
	case *ssa.SliceToArrayPointer:
		w.push(step{v: r, k: addr})
	case *ssa.ChangeType, *ssa.Phi:
		w.push(step{v: r.(ssa.Value), k: s.k})
	case *ssa.MakeInterface:
		w.push(step{v: r, k: boxed})
	case *ssa.Range:
		w.visitRange(r)
	case ssa.CallInstruction:
		w.visitCall(s, r)
	}
}

// visitLookup follows a value read from a followed map. A comma-ok lookup
// gives it in the first value of its tuple.
func (w *walker) visitLookup(l *ssa.Lookup) {
	if !l.CommaOk {
		w.pushRead(l)
		return
	}
	for _, use := range *l.Referrers() {
		if e, ok := use.(*ssa.Extract); ok && e.Index == 0 {
			w.pushRead(e)
		}
	}
}

// visitRange follows the keys and values that a range over a followed map
// reads.
func (w *walker) visitRange(r *ssa.Range) {
	for _, use := range *r.Referrers() {
		if next, ok := use.(*ssa.Next); ok {
			for _, e := range *next.Referrers() {
				if e, ok := e.(*ssa.Extract); ok && e.Index > 0 {
					w.pushRead(e)
				}
			}
		}
	}
}

// visitBoxed follows an interface that holds a followed value: only into a
// type assertion that takes it out again, and into a merge.
func (w *walker) visitBoxed(r ssa.Instruction) {
	switch r := r.(type) {
	case *ssa.TypeAssert:
		if !r.CommaOk {
			w.pushAsserted(r)
			return
		}
		for _, use := range *r.Referrers() {
			if e, ok := use.(*ssa.Extract); ok && e.Index == 0 {
				w.pushAsserted(e)
			}
		}
	case *ssa.ChangeInterface, *ssa.Phi:
		w.push(step{v: r.(ssa.Value), k: boxed})
	}
}

func (w *walker) pushAsserted(v ssa.Value) {
	if types.IsInterface(v.Type()) {
		w.push(step{v: v, k: boxed})
		return
	}
	w.pushRead(v)
}

// store follows a followed value, other than a handle, stored into memory: a
// local variable holds it, and any other memory hands it on.
func (w *walker) store(s step, st *ssa.Store) {
	root, path, ok := localSlot(st.Addr)
	if !ok {
		w.record(st, Maybe)
		return
	}
	var origin *ssa.Store
	if s.k == copied {
		origin = st
	}
	w.push(step{sl: slot{root, path}, k: variable, held: s.k, origin: origin})
}

// visitCall follows a followed value used by a call.
func (w *walker) visitCall(s step, call ssa.CallInstruction) {
	common := call.Common()
	if b, ok := common.Value.(*ssa.Builtin); ok {
		w.visitBuiltin(s, call, b.Name(), common.Args)
		return
	}
	if s.k == handle {
		// A Session or Debug copy of the handle shares its Statement when
		// gorm does not clone it. Any other call of or with a handle is a
		// use of the *gorm.DB, which the callers check on their own.
		if c, ok := call.(*ssa.Call); ok {
			site := callsite.SiteOfWithoutFuncVars(c)
			if site.IsGormMethod() && site.Recv == s.v && site.SessionEffect().SharesStatement {
				w.push(step{v: c, k: handle})
			}
		}
		return
	}
	if !isReader(common) {
		w.record(call, Maybe)
	}
}

// visitBuiltin follows a followed value passed to a builtin.
func (w *walker) visitBuiltin(s step, call ssa.CallInstruction, name string, args []ssa.Value) {
	if args[0] != s.v {
		return // a source, as of copy or of append(dst, s...), or len(s)
	}
	switch name {
	case "delete", "clear", "copy":
		w.record(call, Certain)
	case "append":
		// An append writes after the elements of the slice when it has
		// room. A slice of the memory, as in s[:0], has the memory's own
		// elements there.
		if _, resliced := s.v.(*ssa.Slice); resliced {
			w.record(call, Certain)
		}
		w.pushRead(call.(*ssa.Call))
	}
}

// visitVariable follows the uses of a part of a local variable that holds a
// followed value: every address of the same part, in the function and in the
// closures that capture the variable.
func (w *walker) visitVariable(s step) {
	for _, a := range slotAddrs(s.sl) {
		for _, r := range *a.Referrers() {
			w.visitSlotUse(s, a, r)
		}
	}
	// A load of the whole variable reads what the part holds, as part of a
	// copy.
	if s.sl.path != "" {
		for _, a := range slotAddrs(slot{root: s.sl.root}) {
			for _, r := range *a.Referrers() {
				if load, ok := r.(*ssa.UnOp); ok {
					w.pushRead(load)
				}
			}
		}
	}
}

// visitSlotUse follows one use r of a, an address of the part s.sl of a local
// variable.
func (w *walker) visitSlotUse(s step, a ssa.Value, r ssa.Instruction) {
	switch r := r.(type) {
	case *ssa.UnOp:
		// An address is only loaded.
		switch {
		case s.held != copied:
			w.push(step{v: r, k: s.held})
		case !w.replacedBefore(s, r):
			w.pushRead(r)
		}
	case *ssa.FieldAddr:
		if s.held == copied {
			w.push(step{sl: slot{s.sl.root, s.sl.path + "." + strconv.Itoa(r.Field)}, k: variable, held: copied, origin: s.origin})
		}
	case ssa.CallInstruction:
		// The address of a local copy handed to a call lets it reach the
		// maps and slices the copy shares.
		if s.held == copied && slices.Contains(r.Common().Args, a) && !isReader(r.Common()) {
			w.record(r, Maybe)
		}
	}
}

// replacedBefore reports whether a store into the part s.sl of a local copy,
// or into a part that holds it, comes after the store that made the copy and
// before load on every path. The load then reads what that store wrote, not
// the copied value.
func (w *walker) replacedBefore(s step, load *ssa.UnOp) bool {
	for path := s.sl.path; ; path = parentPath(path) {
		for _, a := range slotAddrs(slot{s.sl.root, path}) {
			for _, r := range *a.Referrers() {
				st, ok := r.(*ssa.Store)
				if ok && st.Addr == a && st != s.origin && precedes(s.origin, st) && precedes(st, load) {
					return true
				}
			}
		}
		if path == "" {
			return false
		}
	}
}

// precedes reports whether a runs before b on every path to b: both are in
// one function, and a dominates b.
func precedes(a, b ssa.Instruction) bool {
	if a.Parent() != b.Parent() {
		return false
	}
	ab, bb := a.Block(), b.Block()
	if ab != bb {
		return ab.Dominates(bb)
	}
	return slices.Index(ab.Instrs, a) < slices.Index(ab.Instrs, b)
}

// kindOf returns what a value of type t read from followed memory is, and
// false when nothing written through it reaches that memory.
func kindOf(t types.Type) (kind, bool) {
	switch types.Unalias(t).Underlying().(type) {
	case *types.Map, *types.Slice:
		return ref, true
	case *types.Pointer:
		// Both reject a pointer to a pointer.
		return handle, typeutil.IsGormDB(t) || typeutil.IsGormStatement(t)
	case *types.Struct:
		// Only a gorm.DB or a gorm.Statement value is read from the memory
		// as a struct: the Statement holds no other struct with a map, a
		// slice or a handle in it.
		return copied, typeutil.IsGormDB(t) || typeutil.IsGormStatement(t)
	}
	return 0, false
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

// readers are the functions of the standard library that only read the map
// or slice they are given, by package path and name. A nil set holds every
// function of the package.
var readers = map[string]map[string]bool{
	"strings": nil, // none changes a []string it gets
	"slices": {
		"All": true, "Backward": true, "BinarySearch": true, "BinarySearchFunc": true,
		"Clone": true, "Compare": true, "CompareFunc": true, "Contains": true,
		"ContainsFunc": true, "Equal": true, "EqualFunc": true, "Index": true,
		"IndexFunc": true, "IsSorted": true, "IsSortedFunc": true, "Max": true,
		"MaxFunc": true, "Min": true, "MinFunc": true, "Values": true,
	},
	"maps": {
		"All": true, "Clone": true, "Equal": true, "EqualFunc": true,
		"Keys": true, "Values": true,
	},
}

// isReader reports whether common statically calls a function in readers. A
// function literal has no object.
func isReader(common *ssa.CallCommon) bool {
	fn := common.StaticCallee()
	if fn == nil || fn.Object() == nil {
		return false
	}
	names, ok := readers[fn.Object().Pkg().Path()]
	return ok && (names == nil || names[fn.Object().Name()])
}

// localSlot returns the local variable that addr is the address of, or of a
// part of, and the path to the part: an Alloc, reached through field
// addresses and element addresses of arrays. A captured variable is the Alloc
// that the closure's FreeVar is bound to.
func localSlot(addr ssa.Value) (root ssa.Value, path string, ok bool) {
	for {
		switch a := addr.(type) {
		case *ssa.Alloc:
			return a, path, true
		case *ssa.FreeVar:
			addr = freeVarBinding(a)
		case *ssa.FieldAddr:
			path = "." + strconv.Itoa(a.Field) + path
			addr = a.X
		case *ssa.IndexAddr:
			if _, slice := types.Unalias(a.X.Type()).Underlying().(*types.Slice); slice {
				return nil, "", false // an element of a slice is not the variable's
			}
			path = "[" + indexKey(a.Index) + "]" + path
			addr = a.X
		default:
			return nil, "", false
		}
	}
}

// freeVarBinding returns the value that the MakeClosure of fv's function
// binds to fv. A closure has exactly one MakeClosure.
func freeVarBinding(fv *ssa.FreeVar) ssa.Value {
	fn := fv.Parent()
	mc := (*fn.Referrers())[0].(*ssa.MakeClosure)
	return mc.Bindings[slices.Index(fn.FreeVars, fv)]
}

// indexKey returns the path segment of an element index: the constant, or *
// for an index that is not one.
func indexKey(index ssa.Value) string {
	if c, ok := index.(*ssa.Const); ok {
		return c.Value.ExactString()
	}
	return "*"
}

// slotAddrs returns the addresses of the part sl of a variable: in its
// function and in every closure that captures it. An element at an index
// that is not a constant may be any element.
func slotAddrs(sl slot) []ssa.Value {
	addrs := captures(sl.root)
	for _, seg := range splitPath(sl.path) {
		var next []ssa.Value
		for _, a := range addrs {
			for _, r := range *a.Referrers() {
				switch r := r.(type) {
				case *ssa.FieldAddr:
					if seg == "."+strconv.Itoa(r.Field) {
						next = append(next, r)
					}
				case *ssa.IndexAddr:
					if k := indexKey(r.Index); k == "*" || seg == "[*]" || seg == "["+k+"]" {
						next = append(next, r)
					}
				}
			}
		}
		addrs = next
	}
	return addrs
}

// captures returns v and the FreeVars that closures bind to it, also in
// closures nested in those.
func captures(v ssa.Value) []ssa.Value {
	all := []ssa.Value{v}
	for i := 0; i < len(all); i++ {
		for _, r := range *all[i].Referrers() {
			mc, ok := r.(*ssa.MakeClosure)
			if !ok {
				continue
			}
			fn := mc.Fn.(*ssa.Function)
			for j, b := range mc.Bindings {
				if b == all[i] {
					all = append(all, fn.FreeVars[j])
				}
			}
		}
	}
	return all
}

// splitPath splits a slot path into its segments.
func splitPath(path string) []string {
	var segs []string
	for path != "" {
		end := strings.IndexAny(path[1:], ".[")
		if end < 0 {
			segs = append(segs, path)
			break
		}
		segs = append(segs, path[:end+1])
		path = path[end+1:]
	}
	return segs
}

// parentPath returns the path of the part that holds the part path, which is
// not empty.
func parentPath(path string) string {
	segs := splitPath(path)
	return strings.Join(segs[:len(segs)-1], "")
}
