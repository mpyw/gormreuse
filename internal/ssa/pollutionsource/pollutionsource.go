// Package pollutionsource is the single source of truth for the SSA
// instruction shapes that let a *gorm.DB value escape ("leak") a scope.
//
// Both the main pollution handler (internal/ssa/handler) and the
// //gormreuse:pure contract validator (internal/ssa/purity) consume this
// package so their notions of "what escapes a value" cannot drift apart.
// Before this package existed the validator inspected only *ssa.Call and
// silently accepted pure functions that leaked their argument via channel
// send, slice/array store, or map store (issue #66).
//
// # What counts as a leak
//
//	┌────────────────┬──────────────────────┬─────────────────────────────┐
//	│ Instruction    │ Source syntax        │ Kind                        │
//	├────────────────┼──────────────────────┼─────────────────────────────┤
//	│ *ssa.Send      │ ch <- db             │ KindChannelSend             │
//	│ *ssa.Store     │ slice[i] = db        │ KindSliceStore              │
//	│ *ssa.MapUpdate │ m[k] = db            │ KindMapStore                │
//	└────────────────┴──────────────────────┴─────────────────────────────┘
//
// Values may be interface-boxed before storage (a []interface{} / map /
// chan of interface{}); Leak unwraps a single MakeInterface box.
//
// # What is deliberately NOT a leak
//
//   - Packing into the varargs array of a known read-only stdlib function
//     (fmt.Println(db), log.Printf("%v", db), t.Logf("%v", db)); these never
//     retain or mutate their arguments. User-defined variadic functions stay
//     conservatively polluting. See isReadOnlyVariadicArg.
//   - Packing into the varargs array of a *gorm.DB method that only reads
//     its *gorm.DB arguments (db.Where("id IN (?)", sub)), when the array
//     goes nowhere else. The main handler records such an argument as a read
//     of its root (#185). See ReadOnlyGormArgs.
//   - Packing into a wrapper that goes only to such a parameter: the varargs
//     of gorm.Expr, a local map, or the Vars of a local clause.Expr literal
//     (#253). See isReadOnlyValue.
//   - A bare *ssa.MakeInterface with no downstream store/use. Interface
//     conversion alone transfers no ownership; the value is only a leak once
//     it is stored or passed somewhere, which the cases above already cover.
//     This matches the main handler's makeInterfaceHandler (a no-op).
package pollutionsource

import (
	"go/token"
	"go/types"
	"slices"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/ssa/callsite"
	"github.com/mpyw/gormreuse/internal/typeutil"
)

// Kind classifies a non-call pollution source.
type Kind int

const (
	// KindNone means the instruction is not a leak.
	KindNone Kind = iota
	// KindChannelSend is `ch <- db`.
	KindChannelSend
	// KindSliceStore is `slice[i] = db` (store to a slice/array element).
	KindSliceStore
	// KindMapStore is `m[k] = db`.
	KindMapStore
)

// UnwrapGormDB extracts the *gorm.DB value from an SSA value that may be
// wrapped in a single MakeInterface. Returns the value and true if it is (or
// wraps) a *gorm.DB, otherwise nil and false.
//
// This is needed because storing *gorm.DB into interface{} containers (slice,
// map, channel) makes SSA box the value first, e.g. []interface{}{q} generates
// MakeInterface(q) -> Store.
func UnwrapGormDB(v ssa.Value) (ssa.Value, bool) {
	if typeutil.IsGormDB(v.Type()) {
		return v, true
	}
	if mi, ok := v.(*ssa.MakeInterface); ok && typeutil.IsGormDB(mi.X.Type()) {
		return mi.X, true
	}
	return nil, false
}

// Leak reports whether instr lets a *gorm.DB escape and, if so, returns the
// unwrapped *gorm.DB value and the kind of source. Returns (nil, KindNone)
// otherwise. Read-only variadic stdlib packing is excluded (see the package
// doc). Callers still decide what a leak means for them (the main handler
// marks the value polluted; the purity validator reports a contract
// violation).
func Leak(instr ssa.Instruction) (ssa.Value, Kind) {
	switch i := instr.(type) {
	case *ssa.Send:
		if v, ok := UnwrapGormDB(i.X); ok {
			return v, KindChannelSend
		}
	case *ssa.Store:
		// Only stores to a slice/array element (IndexAddr) count; stores to an
		// Alloc are ordinary variable assignments handled elsewhere.
		idx, ok := i.Addr.(*ssa.IndexAddr)
		if !ok {
			return nil, KindNone
		}
		v, ok := UnwrapGormDB(i.Val)
		if !ok {
			return nil, KindNone
		}
		if isReadOnlyVariadicArg(idx, i.Val) || isReadOnlyArray(idx.X) {
			return nil, KindNone
		}
		return v, KindSliceStore
	case *ssa.MapUpdate:
		v, ok := UnwrapGormDB(i.Value)
		if !ok {
			return nil, KindNone
		}
		if _, local := i.Map.(*ssa.MakeMap); local && isReadOnlyValue(i.Map) {
			return nil, KindNone
		}
		return v, KindMapStore
	}
	return nil, KindNone
}

// readOnlyVariadicPkgs lists packages whose variadic ...interface{} functions
// are known not to retain or mutate their arguments (formatting/output/logging
// only). Passing a *gorm.DB into them must not be treated as a leak.
var readOnlyVariadicPkgs = map[string]bool{
	"fmt":     true,
	"log":     true,
	"testing": true,
}

// isReadOnlyVariadicArg reports whether the store packs an interface-boxed
// *gorm.DB into the varargs array of a variadic call to a known read-only
// stdlib function (see readOnlyVariadicPkgs). In SSA fmt.Println(q) is:
//
//	t2 = new [1]any (varargs)   // array alloc
//	t3 = &t2[0]                 // idx (IndexAddr)
//	t4 = make any <- *gorm.DB   // val (MakeInterface — interface-boxed)
//	*t3 = t4                    // the Store
//	t5 = slice t2[:]            // sliced ...
//	t6 = fmt.Println(t5...)     // ... and handed to a variadic call
//
// It returns true only when the slot is interface-typed (val is a MakeInterface),
// the array flows into the variadic argument of a call, AND that callee is an
// allow-listed read-only stdlib function. This deliberately excludes:
//   - concrete ...*gorm.DB packs (val is the *gorm.DB directly, not boxed),
//   - user composite slices like []interface{}{q} (no variadic call), and
//   - user-defined variadic ...interface{} functions (may branch the value),
//
// all of which stay conservatively polluting.
func isReadOnlyVariadicArg(idx *ssa.IndexAddr, val ssa.Value) bool {
	if _, ok := val.(*ssa.MakeInterface); !ok {
		return false
	}
	alloc, ok := idx.X.(*ssa.Alloc)
	if !ok || alloc.Referrers() == nil {
		return false
	}
	for _, r := range *alloc.Referrers() {
		slice, ok := r.(*ssa.Slice)
		if !ok || slice.Referrers() == nil {
			continue
		}
		for _, sr := range *slice.Referrers() {
			call, ok := sr.(*ssa.Call)
			if !ok {
				continue
			}
			sig := call.Call.Signature()
			args := call.Call.Args
			if sig != nil && sig.Variadic() && len(args) > 0 && args[len(args)-1] == slice && isReadOnlyStdlibCallee(call) {
				return true
			}
		}
	}
	return false
}

// isReadOnlyStdlibCallee reports whether call targets a function in an
// allow-listed read-only package (see readOnlyVariadicPkgs).
func isReadOnlyStdlibCallee(call *ssa.Call) bool {
	callee := callsite.SiteOf(call, nil).StaticSite().Callee
	if callee == nil || callee.Object() == nil || callee.Object().Pkg() == nil {
		return false
	}
	return readOnlyVariadicPkgs[callee.Object().Pkg().Path()]
}

// readOnlyArgMethods maps each *gorm.DB method that only reads a *gorm.DB
// passed as an argument to the index of its first such parameter, not
// counting the receiver. Every later parameter is read-only too. GORM reads
// such an argument as a subquery, a group condition or a Joins condition,
// from a cloned Statement, so it never changes the argument (#185, checked in
// e2e/internal/subquery_test.go). Find's dest and Delete's value are written
// to or parsed as a model, so they are not read-only. FirstOrInit and
// FirstOrCreate pass their conditions to Find (#253).
var readOnlyArgMethods = map[string]int{
	"Where":         0,
	"Or":            0,
	"Not":           0,
	"Having":        0,
	"Joins":         0,
	"InnerJoins":    0,
	"Table":         0,
	"Select":        0,
	"Distinct":      0,
	"Raw":           0,
	"Exec":          0,
	"Find":          1,
	"First":         1,
	"Last":          1,
	"Take":          1,
	"Delete":        1,
	"Update":        1,
	"UpdateColumn":  1,
	"FirstOrInit":   1,
	"FirstOrCreate": 1,
}

// wrapperFuncs maps each package path and function name of a function that
// wraps its arguments for a query, without reading them, to the index of its
// first wrapped parameter. Every later parameter is wrapped too. GORM binds a
// *gorm.DB inside the result as a subquery, as if it were passed directly
// (#253, checked in e2e/internal/subquery_wrapper_test.go).
var wrapperFuncs = map[[2]string]int{
	{"gorm.io/gorm", "Expr"}:  1,
	{"database/sql", "Named"}: 1,
}

// readOnlyArgStart returns the index in site.Args of the first argument that
// a call to a *gorm.DB method passes to a read-only parameter (see
// readOnlyArgMethods). Every later argument is read-only too. It returns false
// when site is not a call to such a method.
func readOnlyArgStart(site callsite.Site) (int, bool) {
	if !site.IsGormMethod() {
		return 0, false
	}
	first, ok := readOnlyArgMethods[site.Method()]
	return first, ok
}

// wrapperArgStart returns the index in site.Args of the first argument that a
// call to a wrapper function wraps (see wrapperFuncs). It returns false when
// site is not a call to such a function.
func wrapperArgStart(site callsite.Site) (int, bool) {
	fn := site.Declared
	if fn == nil || fn.Signature.Recv() != nil || fn.Object() == nil || fn.Object().Pkg() == nil {
		return 0, false
	}
	first, ok := wrapperFuncs[[2]string{fn.Object().Pkg().Path(), fn.Object().Name()}]
	return first, ok
}

// IsReadOnlyWrapperCall reports whether call, read as site, is a call to a
// wrapper function (gorm.Expr, sql.Named) whose result goes only to read-only
// arguments of *gorm.DB methods, directly or through other wrappers. Its
// *gorm.DB arguments are then only read, by the method that takes the result
// (ReadOnlyGormArgs).
func IsReadOnlyWrapperCall(call *ssa.Call, site callsite.Site) bool {
	_, ok := wrapperArgStart(site)
	return ok && isReadOnlyValue(call)
}

// ReadOnlyGormArgs returns the *gorm.DB values that the call passes to a
// read-only parameter (see readOnlyArgStart): directly, boxed in an
// interface, packed into a varargs array that goes nowhere else, or inside a
// wrapper that goes nowhere else (wrappedGormDBs).
func ReadOnlyGormArgs(site callsite.Site) []ssa.Value {
	start, ok := readOnlyArgStart(site)
	if !ok || start > len(site.Args) {
		return nil
	}
	var vals []ssa.Value
	for _, arg := range site.Args[start:] {
		vals = append(vals, wrappedGormDBs(arg)...)
	}
	return vals
}

// CallReadOnlyArgs returns the read-only *gorm.DB arguments of a call to a
// *gorm.DB method (see ReadOnlyGormArgs), and the index in site.Args of the
// first read-only argument. ok is false for any other call. A call through a
// method value (w := db.Where; w("id IN (?)", sub)) is read as the method call
// it stands for (#178).
func CallReadOnlyArgs(site callsite.Site) (vals []ssa.Value, start int, ok bool) {
	start, ok = readOnlyArgStart(site)
	if !ok {
		return nil, 0, false
	}
	return ReadOnlyGormArgs(site), start, true
}

// wrappedGormDBs returns the *gorm.DB values that v, passed to a read-only
// parameter, holds: v itself, or the values inside a wrapper whose every use
// is read-only (isReadOnlyValue). The wrappers are an interface box, a local
// array sliced into varargs or into a []any literal, a call to a wrapper
// function (gorm.Expr, sql.Named), a local map, and a local clause.Expr
// literal or a pointer to one.
func wrappedGormDBs(v ssa.Value) []ssa.Value {
	if db, ok := UnwrapGormDB(v); ok {
		return []ssa.Value{db}
	}
	var vals []ssa.Value
	switch w := v.(type) {
	case *ssa.MakeInterface:
		return wrappedGormDBs(w.X)
	case *ssa.Slice:
		if isReadOnlyArray(w.X) {
			for _, store := range addrStores(w.X) {
				vals = append(vals, wrappedGormDBs(store.Val)...)
			}
		}
	case *ssa.Call:
		site := callsite.SiteOf(w, nil)
		if start, ok := wrapperArgStart(site); ok && start <= len(site.Args) && isReadOnlyValue(w) {
			for _, arg := range site.Args[start:] {
				vals = append(vals, wrappedGormDBs(arg)...)
			}
		}
	case *ssa.MakeMap:
		if isReadOnlyValue(w) {
			for _, r := range *w.Referrers() {
				if update, ok := r.(*ssa.MapUpdate); ok {
					vals = append(vals, wrappedGormDBs(update.Value)...)
				}
			}
		}
	case *ssa.UnOp:
		if isClauseExprAlloc(w.X) && isReadOnlyValue(w.X) {
			for _, store := range addrStores(w.X) {
				vals = append(vals, wrappedGormDBs(store.Val)...)
			}
		}
	case *ssa.Alloc:
		if isClauseExprAlloc(w) && isReadOnlyValue(w) {
			for _, store := range addrStores(w) {
				vals = append(vals, wrappedGormDBs(store.Val)...)
			}
		}
	}
	return vals
}

// isReadOnlyValue reports whether v is used, and every use of v only hands it
// to a read-only parameter of a *gorm.DB method (readOnlyArgStart), directly
// or through wrappers (see wrappedGormDBs) that are used the same way. Any
// other use may read the *gorm.DB it holds elsewhere, so it is not read-only.
func isReadOnlyValue(v ssa.Value) bool {
	passed := false
	for _, r := range *v.Referrers() {
		switch {
		case fillsLocal(v, r):
		case isReadOnlyUse(v, r):
			passed = true
		default:
			return false
		}
	}
	return passed
}

// fillsLocal reports whether instr, a referrer of v, only fills v, a local
// array, map or clause.Expr: a store into it, or the address of an element or
// field that is only stored to.
func fillsLocal(v ssa.Value, instr ssa.Instruction) bool {
	switch u := instr.(type) {
	case *ssa.IndexAddr:
		return onlyStoredTo(u)
	case *ssa.FieldAddr:
		return onlyStoredTo(u)
	case *ssa.Store:
		return u.Addr == v
	case *ssa.MapUpdate:
		return u.Map == v
	}
	return false
}

// isReadOnlyUse reports whether instr, a referrer of v, only hands v to a
// read-only parameter or to a wrapper that isReadOnlyValue (see
// isReadOnlyValue).
func isReadOnlyUse(v ssa.Value, instr ssa.Instruction) bool {
	switch u := instr.(type) {
	case *ssa.MakeInterface:
		return isReadOnlyValue(u)
	case ssa.CallInstruction:
		// A go or defer statement reads the argument when the call runs, as
		// a plain call does. Only a plain call has a result to wrap it in.
		site := callsite.SiteOf(u, nil)
		if first, ok := readOnlyArgStart(site); ok {
			return slices.Index(site.Args, v) >= first
		}
		call, isCall := u.(*ssa.Call)
		first, ok := wrapperArgStart(site)
		return isCall && ok && slices.Index(site.Args, v) >= first && isReadOnlyValue(call)
	case *ssa.Store:
		// v is u.Val: a store into v is fillsLocal.
		switch addr := u.Addr.(type) {
		case *ssa.IndexAddr:
			return isReadOnlyArray(addr.X)
		case *ssa.FieldAddr:
			return isClauseExprAlloc(addr.X) && isReadOnlyValue(addr.X)
		}
	case *ssa.MapUpdate:
		_, local := u.Map.(*ssa.MakeMap)
		return u.Value == v && local && isReadOnlyValue(u.Map)
	case *ssa.Slice:
		// v is a local array (isReadOnlyArray checks it).
		return isReadOnlyValue(u)
	case *ssa.UnOp:
		// v is a local clause.Expr: its value is used like v.
		return u.Op == token.MUL && isClauseExprAlloc(v) && isReadOnlyValue(u)
	}
	return false
}

// isReadOnlyArray reports whether array is a local array that isReadOnlyValue:
// one whose every slice is passed only to read-only parameters, such as the
// varargs of db.Where("id IN (?)", sub) or a literal spread into them.
func isReadOnlyArray(array ssa.Value) bool {
	_, ok := array.(*ssa.Alloc)
	return ok && isReadOnlyValue(array)
}

// isClauseExprAlloc reports whether v is a local clause.Expr: the address
// that a clause.Expr literal is built in.
func isClauseExprAlloc(v ssa.Value) bool {
	alloc, ok := v.(*ssa.Alloc)
	if !ok {
		return false
	}
	named, ok := types.Unalias(alloc.Type().(*types.Pointer).Elem()).(*types.Named)
	if !ok {
		return false
	}
	obj := named.Obj()
	return obj.Pkg() != nil && obj.Pkg().Path() == "gorm.io/gorm/clause" && obj.Name() == "Expr"
}

// onlyStoredTo reports whether every use of the address addr stores into it.
func onlyStoredTo(addr ssa.Value) bool {
	return !slices.ContainsFunc(*addr.Referrers(), func(r ssa.Instruction) bool {
		store, ok := r.(*ssa.Store)
		return !ok || store.Addr != addr
	})
}

// addrStores returns the stores into the elements or fields of addr, a local
// array or struct.
func addrStores(addr ssa.Value) []*ssa.Store {
	var stores []*ssa.Store
	for _, r := range *addr.Referrers() {
		var elem ssa.Value
		switch r := r.(type) {
		case *ssa.IndexAddr:
			elem = r
		case *ssa.FieldAddr:
			elem = r
		default:
			continue
		}
		for _, er := range *elem.Referrers() {
			if store, ok := er.(*ssa.Store); ok && store.Addr == elem {
				stores = append(stores, store)
			}
		}
	}
	return stores
}
