// Package stmtwrite answers one question for the pure contract and for the
// inferred immutable-param (#405): does a function's own body write into the
// memory that holds the query of a *gorm.DB argument?
//
// That memory is the argument's gorm.DB and gorm.Statement, and the maps and
// slices the Statement holds, such as Clauses and Selects. The walk starts
// from a value that is, or is read from, that memory, and follows it forward
// through addresses, loads, lookups, slices, merges, local variables and the
// closures that capture them. These are writes:
//
//   - a store into the memory;
//   - a map update of a map it holds;
//   - delete, clear or copy into a map or a slice it holds;
//   - an append to a reslice of a slice it holds whose high bound may be
//     below the length, as in s[:0], which overwrites the elements after it
//     in place.
//
// The memory handed to other code is not followed: an argument of a call, a
// value sent or stored outside a local variable, or a value boxed in an
// interface and passed on. Such code may write it, as json.Unmarshal into
// &db.Statement.Selects or a method of Statement.SQL does, but deciding that
// needs the callee's body, so these are known limitations. A boxed value is
// followed only into a type assertion in the same function.
//
// A scalar, a string or an interface read from the memory is a copy. A
// pointer read from it is followed only when it points to a gorm.DB or a
// gorm.Statement: another pointer, such as Schema, points to memory the query
// does not hold. A gorm.DB or a gorm.Statement copied into a local variable,
// as in `c := *db.Statement`, is the function's own. Its maps, slices and
// handles are still shared, unless a store into the same field of the copy
// comes first on every path.
//
// The Error, RowsAffected and Config fields of a gorm.DB are left out. The
// first two hold the outcome of a query, as AddError does. Config holds no
// clause, though a write to it changes how the caller's later queries run.
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
	writes map[ssa.Instruction]bool
	seen   map[step]bool
	work   []step
	addrs  map[slot][]ssa.Value
}

func newWalker(addrs map[slot][]ssa.Value) *walker {
	return &walker{writes: map[ssa.Instruction]bool{}, seen: map[step]bool{}, addrs: addrs}
}

// Cache keeps the addresses of each part of a local variable that a walk
// reached, for the next walk in the same functions. The inference asks about
// each load of a Statement field once, so the walks of one function share
// the parts of its variables, not their answers.
type Cache struct {
	addrs map[slot][]ssa.Value
}

// NewCache returns an empty Cache.
func NewCache() *Cache {
	return &Cache{addrs: map[slot][]ssa.Value{}}
}

// WritesThrough reports whether the body writes through v, a value read from
// the argument's query memory, such as a load of a field of its Statement.
func (c *Cache) WritesThrough(v ssa.Value) bool {
	w := newWalker(c.addrs)
	w.pushRead(v)
	w.run()
	return len(w.writes) > 0
}

// Writes returns the instructions that write the query memory of values that
// are the argument's: a *gorm.DB or *gorm.Statement, or a gorm.DB or
// gorm.Statement passed by value, whose maps, slices and handles are the
// argument's.
func Writes(values []ssa.Value) map[ssa.Instruction]bool {
	w := newWalker(map[slot][]ssa.Value{})
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
		if r.Addr == v {
			w.writes[r] = true
			return
		}
		// A local variable that holds the value still reaches the memory.
		// Other memory hands it on, which is not followed.
		if root, path, ok := localSlot(r.Addr); ok {
			var origin *ssa.Store
			if s.k == copied {
				origin = r
			}
			w.push(step{sl: slot{root, path}, k: variable, held: s.k, origin: origin})
		}
	case *ssa.MapUpdate:
		if r.Map == v {
			w.writes[r] = true
		}
	case *ssa.FieldAddr:
		if !isGormDBStateField(r.X.Type(), r.Field) {
			w.push(step{v: r, k: addr})
		}
	case *ssa.IndexAddr:
		w.push(step{v: r, k: addr}) // the index is an integer, never followed
	case *ssa.Field, *ssa.UnOp:
		// A field of a copied value, or a load through a handle or an
		// address. A followed value is never a channel, so an UnOp of it
		// is a load.
		w.pushRead(r.(ssa.Value))
	case *ssa.Lookup:
		if r.X == v { // not a lookup keyed by the value
			w.visitLookup(r)
		}
	case *ssa.Slice:
		w.push(step{v: r, k: ref}) // the bounds are integers, never followed
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

// visitCall follows a followed value used by a call. Only builtins, and the
// Session-family copies of a handle, are followed: any other call is a
// hand-off.
func (w *walker) visitCall(s step, call ssa.CallInstruction) {
	common := call.Common()
	if b, ok := common.Value.(*ssa.Builtin); ok {
		w.visitBuiltin(s, call, b.Name(), common.Args)
		return
	}
	// A Session or Debug copy of the handle shares its Statement when gorm
	// does not clone it.
	if c, ok := call.(*ssa.Call); ok && s.k == handle {
		site := callsite.SiteOfWithoutFuncVars(c)
		if site.IsGormMethod() && site.Recv == s.v && site.SessionEffect().SharesStatement {
			w.push(step{v: c, k: handle})
		}
	}
}

// visitBuiltin follows a followed value passed to a builtin.
func (w *walker) visitBuiltin(s step, call ssa.CallInstruction, name string, args []ssa.Value) {
	if args[0] != s.v {
		return // a source, as of copy or of append(dst, s...), or len(s)
	}
	switch name {
	case "delete", "clear", "copy":
		w.writes[call] = true
	case "append":
		if overwrites(s.v) {
			w.writes[call] = true
		}
		w.pushRead(call.(*ssa.Call))
	}
}

// overwrites reports whether an append to v may write over elements that the
// slice v was sliced from holds. An append writes after the elements of v
// when it has room. That room holds the original elements only when v is a
// reslice whose high bound may be below the original length, as s[:0] or
// s[:n]. A full reslice (s[1:], s[:len(s)]) or one with a max bound (s[:0:0],
// s[:len(s):len(s)]) leaves no such elements in the room.
func overwrites(v ssa.Value) bool {
	s, ok := v.(*ssa.Slice)
	if !ok || s.High == nil || s.Max != nil {
		return false
	}
	call, ok := s.High.(*ssa.Call)
	if !ok {
		return true
	}
	b, ok := call.Call.Value.(*ssa.Builtin)
	return !ok || b.Name() != "len" || !sameValue(call.Call.Args[0], s.X)
}

// sameValue reports whether a and b are the same value, or loads of the
// same field path from the same value.
func sameValue(a, b ssa.Value) bool {
	if a == b {
		return true
	}
	switch a := a.(type) {
	case *ssa.UnOp:
		b, ok := b.(*ssa.UnOp)
		return ok && a.Op == b.Op && sameValue(a.X, b.X)
	case *ssa.FieldAddr:
		b, ok := b.(*ssa.FieldAddr)
		return ok && a.Field == b.Field && sameValue(a.X, b.X)
	}
	return false
}

// visitVariable follows the uses of a part of a local variable that holds a
// followed value: every address of the same part, in the function and in the
// closures that capture the variable.
func (w *walker) visitVariable(s step) {
	for _, a := range w.slotAddrs(s.sl) {
		for _, r := range *a.Referrers() {
			w.visitSlotUse(s, r)
		}
	}
	// A load of the whole variable reads what the part holds, as part of a
	// copy.
	if s.sl.path != "" {
		for _, a := range w.slotAddrs(slot{root: s.sl.root}) {
			for _, r := range *a.Referrers() {
				if load, ok := r.(*ssa.UnOp); ok {
					w.pushRead(load)
				}
			}
		}
	}
}

// visitSlotUse follows one use r of an address of the part s.sl of a local
// variable.
func (w *walker) visitSlotUse(s step, r ssa.Instruction) {
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
	}
}

// replacedBefore reports whether a store into the part s.sl of a local copy,
// or into a part that holds it, comes after the store that made the copy and
// before load on every path. The load then reads what that store wrote, not
// the copied value.
func (w *walker) replacedBefore(s step, load *ssa.UnOp) bool {
	for path := s.sl.path; ; path = parentPath(path) {
		for _, a := range w.slotAddrs(slot{s.sl.root, path}) {
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
		// as a struct with a map, a slice or a handle in it.
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
			addr = callsite.FreeVarBinding(a)
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
			// Also a FreeVar whose MakeClosure SSA removed, for which
			// FreeVarBinding gives nil.
			return nil, "", false
		}
	}
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
func (w *walker) slotAddrs(sl slot) []ssa.Value {
	if addrs, done := w.addrs[sl]; done {
		return addrs
	}
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
	w.addrs[sl] = addrs
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
