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
//   - A bare *ssa.MakeInterface with no downstream store/use. Interface
//     conversion alone transfers no ownership; the value is only a leak once
//     it is stored or passed somewhere, which the cases above already cover.
//     This matches the main handler's MakeInterfaceHandler (a no-op).
package pollutionsource

import (
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
		if isReadOnlyVariadicArg(idx, i.Val) {
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
