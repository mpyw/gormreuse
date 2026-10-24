// Package stmtwrite answers one question for the pure contract and for the
// inferred immutable-param (#405): does a function's own body surely write
// into the memory that holds the query of a *gorm.DB argument?
//
// That memory is the argument's gorm.DB and gorm.Statement, the maps and
// slices the Statement holds, such as Clauses and Selects, and the clause
// values it points to, such as TableExpr. The walk starts from the argument,
// or from a value read from its memory, and follows it forward through
// addresses, loads, lookups, slices, merges, local variables and the closures
// that capture them. These are writes:
//
//   - a store into the memory;
//   - a map update of a map it holds;
//   - delete, clear or copy into a map or a slice it holds;
//   - an append to a reslice of a slice it holds whose high bound may be
//     below the length, as in s[:0], which overwrites the elements after it
//     in place.
//
// A write counts only when it surely reaches the argument. A merge is
// followed only when every edge it merges is the argument's memory. A local
// variable is followed only while it surely holds the value stored into it:
// its address goes nowhere else, and no other store into it can run before
// the read. A call result is not followed, except a Session or Debug copy
// that shares the Statement. Missing a write is a known limitation, and a
// report of one that may not happen would be a false one.
//
// The memory handed to other code is not followed either: an argument of a
// call, a value sent or stored outside a local variable, or a value boxed in
// an interface and passed on. Such code may write it, as json.Unmarshal into
// &db.Statement.Selects or a method of Statement.SQL does, but deciding that
// needs the callee's body. A boxed value is followed only into a type
// assertion in the same function.
//
// A scalar, a string or an interface read from the memory is a copy. A
// pointer read from it is followed only when it points to a gorm.DB, a
// gorm.Statement or a clause value: another pointer, such as Schema, points to
// memory the query does not hold. A gorm.DB or a gorm.Statement copied into a
// local variable, as in `c := *db.Statement`, is the function's own. Its
// maps, slices and handles are still shared while the variable surely holds
// them.
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

// clausePkg is the import path of gorm's clause package.
const clausePkg = "gorm.io/gorm/clause"

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
	// holder is a parameter, or a part of one, that holds a *gorm.DB: a
	// **gorm.DB, a slice, a map, an array or a struct. A write to it is not
	// a write to a query. The handles read from it are the argument's.
	holder
	// variable is a part of a local variable that holds one of the above.
	// held is the kind of what it holds.
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
	// origin is the store that put the followed value into a variable.
	origin *ssa.Store
}

// walker walks values forward and records the writes it finds.
type walker struct {
	writes map[ssa.Instruction]bool
	seen   map[step]bool
	work   []step
	shared *Cache
}

// Cache keeps what the walks of one run learn about the local variables of
// the functions they reach: the addresses of each part of a variable, and
// whether its address goes anywhere else. The inference asks about each load
// of a Statement field once, so the walks share these, not their answers.
type Cache struct {
	addrs   map[slot][]ssa.Value
	escapes map[ssa.Value]bool
}

// NewCache returns an empty Cache.
func NewCache() *Cache {
	return &Cache{addrs: map[slot][]ssa.Value{}, escapes: map[ssa.Value]bool{}}
}

func (c *Cache) walker() *walker {
	return &walker{writes: map[ssa.Instruction]bool{}, seen: map[step]bool{}, shared: c}
}

// WritesThrough reports whether the body surely writes through v, a value
// read from the argument's query memory, such as a load of a field of its
// Statement.
func (c *Cache) WritesThrough(v ssa.Value) bool {
	w := c.walker()
	w.pushRead(v)
	w.run()
	return len(w.writes) > 0
}

// Writes returns the instructions that surely write the query memory of the
// parameters params: each a *gorm.DB or *gorm.Statement, a gorm.DB or
// gorm.Statement passed by value, or a value that holds a *gorm.DB.
func Writes(params []ssa.Value) map[ssa.Instruction]bool {
	w := NewCache().walker()
	for _, p := range params {
		w.pushHeld(p)
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

// pushHeld follows v, a parameter or a value read from a holder: a handle or
// a gorm value as such, and anything else that holds a *gorm.DB as a holder.
func (w *walker) pushHeld(v ssa.Value) {
	t := v.Type()
	switch {
	case typeutil.IsGormDB(t) || typeutil.IsGormStatement(t):
		w.pushRead(v)
	case holdsGormDB(t, map[types.Type]bool{}):
		w.push(step{v: v, k: holder})
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
		case holder:
			for _, r := range *s.v.Referrers() {
				w.visitHolder(s, r)
			}
		default:
			for _, r := range *s.v.Referrers() {
				w.visit(s, r)
			}
		}
	}
}

// visit follows one use r of a followed value s that is the argument's
// memory or a copy of a part of it.
func (w *walker) visit(s step, r ssa.Instruction) {
	v := s.v
	switch r := r.(type) {
	case *ssa.Store:
		if r.Addr == v {
			w.writes[r] = true
			return
		}
		w.storeLocal(s, r)
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
			w.visitLookup(r, w.pushRead)
		}
	case *ssa.Slice:
		w.push(step{v: r, k: ref}) // the bounds are integers, never followed
	case *ssa.SliceToArrayPointer:
		w.push(step{v: r, k: addr})
	case *ssa.ChangeType:
		w.push(step{v: r, k: s.k})
	case *ssa.Phi:
		w.pushPhi(r, s.k)
	case *ssa.MakeInterface:
		w.push(step{v: r, k: boxed})
	case *ssa.Range:
		w.visitRange(r, w.pushRead)
	case ssa.CallInstruction:
		w.visitCall(s, r)
	}
}

// visitHolder follows one use r of a holder s: only the reads that take a
// part of it, and a local variable that holds it. A write to the holder
// writes no query.
func (w *walker) visitHolder(s step, r ssa.Instruction) {
	v := s.v
	switch r := r.(type) {
	case *ssa.Store:
		if r.Val == v {
			w.storeLocal(s, r)
		}
	case *ssa.FieldAddr, *ssa.IndexAddr, *ssa.Field, *ssa.Index, *ssa.UnOp, *ssa.Slice, *ssa.ChangeType:
		w.pushHeld(r.(ssa.Value))
	case *ssa.Lookup:
		if r.X == v {
			w.visitLookup(r, w.pushHeld)
		}
	case *ssa.Phi:
		w.pushPhi(r, holder)
	case *ssa.Range:
		w.visitRange(r, w.pushHeld)
	}
}

// storeLocal follows a followed value stored into a local variable. A store
// into any other memory hands the value on, which is not followed.
func (w *walker) storeLocal(s step, st *ssa.Store) {
	if root, path, ok := localSlot(st.Addr); ok {
		w.push(step{sl: slot{root, path}, k: variable, held: s.k, origin: st})
	}
}

// pushPhi follows phi as a value of kind k when every edge of it is: one the
// walk follows as such, nil, or one that the phi itself makes, as a loop
// does. A merge with any other value may not be the argument's.
func (w *walker) pushPhi(phi *ssa.Phi, k kind) {
	for _, e := range phi.Edges {
		if _, isConst := e.(*ssa.Const); isConst || w.seen[step{v: e, k: k}] || derivesFrom(e, phi) {
			continue
		}
		return // pushPhi runs again when the walk reaches the edge
	}
	w.push(step{v: phi, k: k})
}

// derivesFrom reports whether v is made from phi by slicing, conversions,
// merges or an append to it, as a loop that steps a slice does.
func derivesFrom(v ssa.Value, phi *ssa.Phi) bool {
	seen := map[ssa.Value]bool{}
	var from func(v ssa.Value) bool
	from = func(v ssa.Value) bool {
		if v == ssa.Value(phi) {
			return true
		}
		if seen[v] {
			return false
		}
		seen[v] = true
		switch v := v.(type) {
		case *ssa.Slice:
			return from(v.X)
		case *ssa.ChangeType:
			return from(v.X)
		case *ssa.Phi:
			return slices.ContainsFunc(v.Edges, from)
		}
		return false
	}
	return from(v)
}

// visitLookup follows a value read from a followed map with push. A comma-ok
// lookup gives it in the first value of its tuple.
func (w *walker) visitLookup(l *ssa.Lookup, push func(ssa.Value)) {
	if !l.CommaOk {
		push(l)
		return
	}
	for _, use := range *l.Referrers() {
		if e, ok := use.(*ssa.Extract); ok && e.Index == 0 {
			push(e)
		}
	}
}

// visitRange follows the keys and values that a range over a followed map
// reads with push.
func (w *walker) visitRange(r *ssa.Range, push func(ssa.Value)) {
	for _, use := range *r.Referrers() {
		if next, ok := use.(*ssa.Next); ok {
			for _, e := range *next.Referrers() {
				if e, ok := e.(*ssa.Extract); ok && e.Index > 0 {
					push(e)
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
	case *ssa.ChangeInterface:
		w.push(step{v: r, k: boxed})
	case *ssa.Phi:
		w.pushPhi(r, boxed)
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
		if !allocates(s.v) {
			w.pushRead(call.(*ssa.Call))
		}
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

// allocates reports whether an append to v always makes a new array: v is a
// reslice with no room, its max bound the same as its low bound, as in
// s[:0:0] or s[i:i:i].
func allocates(v ssa.Value) bool {
	s, ok := v.(*ssa.Slice)
	if !ok || s.Max == nil {
		return false
	}
	if s.Low == nil {
		c, isConst := s.Max.(*ssa.Const)
		return isConst && c.Value.ExactString() == "0"
	}
	return sameValue(s.Low, s.Max)
}

// sameValue reports whether a and b are the same value, equal constants, or
// loads of the same field path from the same value.
func sameValue(a, b ssa.Value) bool {
	if a == b {
		return true
	}
	switch a := a.(type) {
	case *ssa.Const:
		b, ok := b.(*ssa.Const)
		return ok && a.Value != nil && b.Value != nil && a.Value.ExactString() == b.Value.ExactString()
	case *ssa.UnOp:
		b, ok := b.(*ssa.UnOp)
		return ok && a.Op == b.Op && sameValue(a.X, b.X)
	case *ssa.FieldAddr:
		b, ok := b.(*ssa.FieldAddr)
		return ok && a.Field == b.Field && sameValue(a.X, b.X)
	}
	return false
}

// visitVariable follows the reads of a part of a local variable that holds a
// followed value: every address of the same part, in the function and in the
// closures that capture the variable. A read is followed only when the
// variable surely holds the value there (surelyHolds).
func (w *walker) visitVariable(s step) {
	if w.escapes(s.sl.root) {
		return
	}
	for _, a := range w.slotAddrs(s.sl) {
		for _, r := range *a.Referrers() {
			switch r := r.(type) {
			case *ssa.UnOp:
				// An address is only loaded.
				if w.surelyHolds(s, r) {
					w.pushVariable(s, r)
				}
			case *ssa.FieldAddr:
				// A part of a local copy, or of a holder.
				w.push(step{sl: slot{s.sl.root, s.sl.path + "." + strconv.Itoa(r.Field)}, k: variable, held: s.held, origin: s.origin})
			case *ssa.IndexAddr:
				w.push(step{sl: slot{s.sl.root, s.sl.path + "[" + indexKey(r.Index) + "]"}, k: variable, held: s.held, origin: s.origin})
			}
		}
	}
	// A load of the whole variable reads what the part holds, as part of a
	// copy.
	if s.sl.path != "" {
		for _, a := range w.slotAddrs(slot{root: s.sl.root}) {
			for _, r := range *a.Referrers() {
				if load, ok := r.(*ssa.UnOp); ok && w.surelyHolds(s, load) {
					w.pushRead(load)
				}
			}
		}
	}
}

// pushVariable follows load, a read of the part s.sl that surely holds what
// s says.
func (w *walker) pushVariable(s step, load *ssa.UnOp) {
	switch s.held {
	case copied:
		w.pushRead(load) // a part of the copy, by its type
	case holder:
		w.pushHeld(load)
	default:
		w.push(step{v: load, k: s.held})
	}
}

// surelyHolds reports whether load surely reads the value that s.origin
// stored, or a part of it, or the variable's zero value, which writes
// nothing. Every other store into the part s.sl, or into a part that holds
// it, must be in the function of load and unable to run before it on any
// path from the function's entry: a store in a closure may run at any time.
func (w *walker) surelyHolds(s step, load *ssa.UnOp) bool {
	for path := s.sl.path; ; path = parentPath(path) {
		for _, a := range w.slotAddrs(slot{s.sl.root, path}) {
			for _, r := range *a.Referrers() {
				st, ok := r.(*ssa.Store)
				if ok && st.Addr == a && st != s.origin && (st.Parent() != load.Parent() || reaches(st, load)) {
					return false
				}
			}
		}
		if path == "" {
			return true
		}
	}
}

// reaches reports whether a may run before b in their one function.
func reaches(a, b ssa.Instruction) bool {
	ab, bb := a.Block(), b.Block()
	if ab == bb && slices.Index(ab.Instrs, a) < slices.Index(ab.Instrs, b) {
		return true
	}
	seen := map[*ssa.BasicBlock]bool{}
	work := slices.Clone(ab.Succs)
	for len(work) > 0 {
		blk := work[len(work)-1]
		work = work[:len(work)-1]
		if blk == bb {
			return true
		}
		if !seen[blk] {
			seen[blk] = true
			work = append(work, blk.Succs...)
		}
	}
	return false
}

// escapes reports whether the address of the variable root, or of a part of
// it, goes anywhere but loads, stores into it, the addresses of its parts and
// the closures that capture it. Code that gets the address may store into
// the variable, so it no longer surely holds anything.
func (w *walker) escapes(root ssa.Value) bool {
	if escapes, done := w.shared.escapes[root]; done {
		return escapes
	}
	escapes := false
	work := captures(root)
	for len(work) > 0 && !escapes {
		a := work[len(work)-1]
		work = work[:len(work)-1]
		for _, r := range *a.Referrers() {
			switch r := r.(type) {
			case *ssa.FieldAddr, *ssa.IndexAddr:
				work = append(work, r.(ssa.Value))
			case *ssa.Store:
				escapes = escapes || r.Addr != a
			case *ssa.UnOp, *ssa.MakeClosure, *ssa.DebugRef:
			default:
				escapes = true
			}
		}
	}
	w.shared.escapes[root] = escapes
	return escapes
}

// kindOf returns what a value of type t read from followed memory is, and
// false when nothing written through it reaches that memory.
func kindOf(t types.Type) (kind, bool) {
	switch u := types.Unalias(t).Underlying().(type) {
	case *types.Map, *types.Slice:
		return ref, true
	case *types.Pointer:
		if typeutil.IsGormDB(t) || typeutil.IsGormStatement(t) { // both reject **T
			return handle, true
		}
		// A clause value that the Statement points to, as TableExpr.
		named, ok := types.Unalias(u.Elem()).(*types.Named)
		return addr, ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == clausePkg
	case *types.Struct:
		// Only a gorm.DB or a gorm.Statement value is read from the memory
		// as a struct with a map, a slice or a handle in it.
		return copied, typeutil.IsGormDB(t) || typeutil.IsGormStatement(t)
	}
	return 0, false
}

// holdsGormDB reports whether a value of type t holds a *gorm.DB: through
// pointers, struct fields, and the elements of arrays, slices and maps.
func holdsGormDB(t types.Type, seen map[types.Type]bool) bool {
	if typeutil.IsGormDB(t) {
		return true
	}
	if seen[t] {
		return false
	}
	seen[t] = true
	switch u := types.Unalias(t).Underlying().(type) {
	case *types.Pointer:
		return holdsGormDB(u.Elem(), seen)
	case *types.Array:
		return holdsGormDB(u.Elem(), seen)
	case *types.Slice:
		return holdsGormDB(u.Elem(), seen)
	case *types.Map:
		return holdsGormDB(u.Elem(), seen)
	case *types.Struct:
		for f := range u.Fields() {
			if holdsGormDB(f.Type(), seen) {
				return true
			}
		}
	}
	return false
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
	if addrs, done := w.shared.addrs[sl]; done {
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
	w.shared.addrs[sl] = addrs
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
