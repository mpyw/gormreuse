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
//   - A bare *ssa.MakeInterface with no downstream store/use. Interface
//     conversion alone transfers no ownership; the value is only a leak once
//     it is stored or passed somewhere, which the cases above already cover.
//     This matches the main handler's makeInterfaceHandler (a no-op).
package pollutionsource

import (
	"slices"

	"golang.org/x/tools/go/ssa"

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
		if isReadOnlyVariadicArg(idx, i.Val) || isReadOnlyGormVarargs(idx.X) {
			return nil, KindNone
		}
		return v, KindSliceStore
	case *ssa.MapUpdate:
		if v, ok := UnwrapGormDB(i.Value); ok {
			return v, KindMapStore
		}
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
	callee := call.Call.StaticCallee()
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
// to or parsed as a model, so they are not read-only.
var readOnlyArgMethods = map[string]int{
	"Where":        0,
	"Or":           0,
	"Not":          0,
	"Having":       0,
	"Joins":        0,
	"InnerJoins":   0,
	"Table":        0,
	"Select":       0,
	"Distinct":     0,
	"Raw":          0,
	"Exec":         0,
	"Find":         1,
	"First":        1,
	"Last":         1,
	"Take":         1,
	"Delete":       1,
	"Update":       1,
	"UpdateColumn": 1,
}

// ReadOnlyArgStart returns the index of the first argument, not counting the
// receiver, that a call to the *gorm.DB method callee passes to a read-only
// parameter (see readOnlyArgMethods). Every later argument is read-only too.
// It returns false when callee is not such a method. A caller that resolves a
// call through a method value passes the method it stands for.
func ReadOnlyArgStart(callee *ssa.Function) (int, bool) {
	if callee == nil {
		return 0, false
	}
	recv := callee.Signature.Recv()
	if recv == nil || !typeutil.IsGormDB(recv.Type()) {
		return 0, false
	}
	first, ok := readOnlyArgMethods[callee.Name()]
	return first, ok
}

// ReadOnlyGormArgs returns the *gorm.DB values that a call to callee passes
// to a read-only parameter (see ReadOnlyArgStart): directly, boxed in an
// interface, or packed into a varargs array that goes nowhere else
// (isReadOnlyGormVarargs). args are the call's arguments without the
// receiver.
func ReadOnlyGormArgs(callee *ssa.Function, args []ssa.Value) []ssa.Value {
	start, ok := ReadOnlyArgStart(callee)
	if !ok || start > len(args) {
		return nil
	}
	var vals []ssa.Value
	for _, arg := range args[start:] {
		if v, ok := UnwrapGormDB(arg); ok {
			vals = append(vals, v)
			continue
		}
		slice, ok := arg.(*ssa.Slice)
		if !ok || !isReadOnlyGormVarargs(slice.X) {
			continue
		}
		for _, store := range arrayElementStores(slice.X) {
			if v, ok := UnwrapGormDB(store.Val); ok {
				vals = append(vals, v)
			}
		}
	}
	return vals
}

// StaticReadOnlyArgs returns the read-only *gorm.DB arguments of a static
// *gorm.DB method call (see ReadOnlyGormArgs), and the index in common.Args
// of the first read-only argument. ok is false for any other call. A call
// through a method value has no receiver argument, so the main handler keeps
// treating its arguments as branches.
func StaticReadOnlyArgs(common *ssa.CallCommon) (vals []ssa.Value, start int, ok bool) {
	callee := common.StaticCallee()
	if callee == nil || len(common.Args) == 0 {
		return nil, 0, false
	}
	first, ok := ReadOnlyArgStart(callee)
	if !ok {
		return nil, 0, false
	}
	return ReadOnlyGormArgs(callee, common.Args[1:]), first + 1, true
}

// isReadOnlyGormVarargs reports whether array is a local array whose every
// slice is passed only to read-only parameters of *gorm.DB methods: the
// varargs of db.Where("id IN (?)", sub), or a literal spread into them. An
// array that is used any other way may be read elsewhere, so it is not.
func isReadOnlyGormVarargs(array ssa.Value) bool {
	alloc, ok := array.(*ssa.Alloc)
	if !ok {
		return false
	}
	passed := false
	for _, r := range *alloc.Referrers() {
		switch u := r.(type) {
		case *ssa.IndexAddr:
			continue
		case *ssa.Slice:
			if len(*u.Referrers()) == 0 {
				return false
			}
			for _, sr := range *u.Referrers() {
				call, ok := sr.(*ssa.Call)
				if !ok {
					return false
				}
				_, start, ok := StaticReadOnlyArgs(&call.Call)
				if !ok || slices.Index(call.Call.Args, ssa.Value(u)) < start {
					return false
				}
			}
			passed = true
		default:
			return false
		}
	}
	return passed
}

// arrayElementStores returns the stores into the elements of array.
func arrayElementStores(array ssa.Value) []*ssa.Store {
	var stores []*ssa.Store
	for _, r := range *array.Referrers() {
		idx, ok := r.(*ssa.IndexAddr)
		if !ok {
			continue
		}
		for _, ir := range *idx.Referrers() {
			if store, ok := ir.(*ssa.Store); ok && store.Addr == idx {
				stores = append(stores, store)
			}
		}
	}
	return stores
}
