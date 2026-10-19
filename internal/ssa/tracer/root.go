// Package tracer provides SSA value tracing for gormreuse.
//
// # Overview
//
// This package traces SSA values backward through the SSA graph to find the
// "mutable root" - the origin of a *gorm.DB chain that determines pollution state.
//
// # Tracing Model
//
// The tracer follows SSA values backward through:
//
//	┌─────────────────────────────────────────────────────────────────────────┐
//	│                         Tracing Directions                               │
//	│                                                                          │
//	│   Source (origin)                              Target (usage site)       │
//	│      │                                              │                    │
//	│      │  ┌────────────┐   ┌────────────┐            │                    │
//	│      └──│  gorm.DB   │──▶│  .Where()  │──▶ ... ────┘                    │
//	│         │  method    │   │  method    │                                 │
//	│         │  call      │   │  call      │                                 │
//	│         └────────────┘   └────────────┘                                 │
//	│              ▲                                                          │
//	│              │                                                          │
//	│         "Mutable Root"                                                  │
//	│         (This call result is the pollution tracking point)              │
//	└─────────────────────────────────────────────────────────────────────────┘
//
// # Key Semantic: "Variable Assignment Creates New Root"
//
// When a gorm chain result is stored in a variable, that variable becomes a
// NEW mutable root. This prevents pollution from propagating through assignments:
//
//	base := db.Where("x")     // base is mutable root #1
//	base.Find(nil)            // marks base (root #1) polluted
//	q := base.Where("y")      // VIOLATION (base polluted) AND q becomes root #2
//	q.Count(nil)              // OK (q is fresh root #2, not polluted)
//
// # Tracing Through SSA Constructs
//
//	┌───────────────────────────────────────────────────────────────────────┐
//	│  SSA Construct          │  Tracing Behavior                          │
//	├───────────────────────────────────────────────────────────────────────┤
//	│  *ssa.Call (gorm)       │  STOP - call result is the mutable root    │
//	│  *ssa.Call (IIFE)       │  Trace through closure returns             │
//	│  *ssa.Phi               │  Trace all edges (conditional merge)       │
//	│  *ssa.UnOp (deref)      │  Trace the pointer being dereferenced      │
//	│  *ssa.Alloc             │  Find Store instructions to this alloc     │
//	│  *ssa.FreeVar           │  Find binding in parent's MakeClosure      │
//	│  *ssa.FieldAddr         │  Find Store to this field                  │
//	│  *ssa.Global            │  Find Store to it in the same function     │
//	│                         │  (none: immutable, #183)                   │
//	│  *ssa.Parameter         │  ROOT - a *gorm.DB parameter is mutable by │
//	│                         │  default (caller may pass clone==0, #61)   │
//	│  *ssa.Parameter (exempt)│  STOP - immutable when the fn is annotated │
//	│                         │  //gormreuse:immutable-param or is a       │
//	│                         │  Transaction callback (forkable, clone>0)  │
//	│  *ssa.Const (nil)       │  STOP - nil is ignored                     │
//	│  Builtin pure call      │  STOP - returns immutable (nil root)       │
//	│  immutable-return call  │  STOP - returns immutable (nil root)       │
//	│  User-defined pure call │  Returns call as root (may be mutable),    │
//	│                         │  or the argument's root when the result    │
//	│                         │  may be that argument (#188)               │
//	│  Dynamic/interface call │  Returns call as root (callee unknown)     │
//	│  *ssa.Extract           │  Trace the tuple: a tuple-result call is   │
//	│                         │  the root of its *gorm.DB elements         │
//	└───────────────────────────────────────────────────────────────────────┘
package tracer

import (
	"go/token"
	"go/types"
	"maps"
	"slices"
	"strings"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/directive"
	"github.com/mpyw/gormreuse/internal/facts"
	"github.com/mpyw/gormreuse/internal/ssa/cfg"
	"github.com/mpyw/gormreuse/internal/ssa/pollutionsource"
	"github.com/mpyw/gormreuse/internal/typeutil"
)

// RootTracer traces SSA values to find mutable *gorm.DB roots.
//
// # Core Responsibility
//
// Given a receiver value of a gorm method call, trace backward to find
// the "mutable root" that pollution tracking should use.
//
// # Variable Assignment Semantics
//
// Variable assignment (Store to Alloc) creates a NEW mutable root,
// which prevents violations from propagating through assigned variables:
//
//	base := db.Where("x")     // base is mutable root
//	base.Find(nil)            // marks base polluted
//	q := base.Where("y")      // VIOLATION! AND q becomes NEW root
//	q.Count(nil)              // OK (q is fresh, not polluted)
//
// # Immutable Sources
//
// The following are treated as immutable sources (return nil root):
//   - Function parameters: `func foo(db *gorm.DB)` - db is immutable
//   - Builtin pure function results: `db.Session(...)` returns immutable
//   - Const nil values
//
// Note: User-defined pure functions (//gormreuse:pure) are NOT immutable sources.
// They may return mutable values - only builtin pure methods guarantee immutable returns.
type RootTracer struct {
	pureFuncs            *directive.DirectiveFuncSet // User-defined pure functions
	immutableReturnFuncs *directive.DirectiveFuncSet // Functions returning immutable *gorm.DB
	immutableParamFuncs  *directive.DirectiveFuncSet // Functions whose *gorm.DB params are immutable (opt out of Phase 1b)
	pure                 *PureVerdicts               // What validating the pure functions found
	onlyParams           map[*ssa.Function]bool      // Memo of returnsOnlyParams
	scopesCallbacks      map[*ssa.Function]bool      // Scopes/Preload callbacks (params are mutable roots)
	immutableCallbacks   map[*ssa.Function]bool      // Transaction/FindInBatches callbacks (fresh tx)
	fieldRoots           map[fieldRootKey]*FieldRoot // Interned FieldRoots (one per call and field path)
	funcVars             FuncVars                    // Package-level variables that hold one function (#183)
}

// PureVerdicts is what validating the //gormreuse:pure functions found. The
// purity package fills it before the reuse analysis runs.
type PureVerdicts struct {
	// Failed lists the pure functions whose bodies definitively break the
	// contract (pollute or leak an argument). They must NOT be trusted as pure
	// at call sites, since that would hide reuse of the value they leak (#66).
	Failed map[*ssa.Function]bool

	// Returned lists, for each pure function that keeps the contract, the
	// indexes into fn.Params of the parameters its result may be (#188). A
	// pure function does not pollute its argument, but `return db` hands the
	// same *gorm.DB back, so a branch from the result is a branch from the
	// argument.
	Returned map[*ssa.Function][]int

	// Imported holds the verdicts that other packages exported for their pure
	// functions (#195). Failed and Returned hold only this package's.
	Imported *facts.Table
}

// HasFailed reports whether fn is a pure function that broke its contract. The
// verdicts are keyed by the declared function, and fn may be a generic instance
// or a bound method wrapper of it (#180).
func (p *PureVerdicts) HasFailed(fn *ssa.Function) bool {
	if p == nil {
		return false
	}
	declared := directive.DeclaredFunc(fn)
	return p.Failed[declared] || p.Imported.Of(declared).PureFailed
}

// ReturnedArgs returns the arguments of a call to fn (args, as in
// CallCommon.Args) that its result may be. A bound method wrapper (f := x.M;
// f(a)) takes no receiver argument, so the indexes are shifted by the
// parameters it lacks; the receiver itself is never in Returned. The count of
// parameters comes from the signature, since a function of another package
// has no Params.
func (p *PureVerdicts) ReturnedArgs(fn *ssa.Function, args []ssa.Value) []ssa.Value {
	if p == nil {
		return nil
	}
	declared := directive.DeclaredFunc(fn)
	indexes := p.Returned[declared]
	if len(indexes) == 0 {
		indexes = p.Imported.Of(declared).Returned
	}
	if len(indexes) == 0 {
		return nil
	}
	nparams := declared.Signature.Params().Len()
	if declared.Signature.Recv() != nil {
		nparams++
	}
	shift := nparams - len(args)
	var returned []ssa.Value
	for _, i := range indexes {
		if j := i - shift; j >= 0 && j < len(args) {
			returned = append(returned, args[j])
		}
	}
	return returned
}

// New creates a new RootTracer.
//
// pure holds the verdicts of validating the //gormreuse:pure functions earlier
// this pass: the ones that failed are not trusted as pure, and the result of a
// trusted one is traced to the arguments it may return. It may be nil.
//
// scopesCallbacks lists function literals passed to gorm's Scopes/Preload, whose
// *gorm.DB parameter receives a mid-chain (clone==0) value at runtime and is
// therefore itself a mutable root — reuse inside the callback interferes (#60).
// It may be nil.
//
// immutableParamFuncs lists functions annotated //gormreuse:immutable-param,
// whose *gorm.DB parameters opt out of the Phase 1b mutable-by-default treatment
// (issue #61). immutableCallbacks lists function literals passed to gorm's
// Transaction/FindInBatches, whose tx parameter is a fresh forkable (clone>0)
// handle and is therefore immutable. Both may be nil.
//
// funcVars resolves a call through a package-level variable that holds one
// function to that function, so its directives apply (#183). It may be nil.
func New(pureFuncs, immutableReturnFuncs, immutableParamFuncs *directive.DirectiveFuncSet, pure *PureVerdicts, scopesCallbacks, immutableCallbacks map[*ssa.Function]bool, funcVars FuncVars) *RootTracer {
	return &RootTracer{
		pureFuncs:            pureFuncs,
		immutableReturnFuncs: immutableReturnFuncs,
		immutableParamFuncs:  immutableParamFuncs,
		pure:                 pure,
		scopesCallbacks:      scopesCallbacks,
		immutableCallbacks:   immutableCallbacks,
		funcVars:             funcVars,
		onlyParams:           make(map[*ssa.Function]bool),
		fieldRoots:           make(map[fieldRootKey]*FieldRoot),
	}
}

// FindMutableRoot finds the mutable root for a receiver value.
//
// Returns nil if the value traces back to an immutable source (parameter,
// pure function result, or nil constant).
//
// # Key Semantic
//
// We stop at gorm call results, NOT at variable origins. This means each
// gorm call result that is "directly used" is its own root:
//
//	a := db.Where("x")        // Trace stops here: a's root is this call
//	b := a.Where("y")         // Trace stops here: b's root is this call
//	                          // (not traced back to a's call)
//
// # Example Trace
//
//	q := db.Where("x")        // q is root
//	q.Find(nil)               // receiver=q → trace → root=q's defining call
func (t *RootTracer) FindMutableRoot(recv ssa.Value, loopInfo *cfg.LoopInfo) ssa.Value {
	return t.trace(recv, make(map[ssa.Value]bool), loopInfo)
}

// FindAllMutableRoots finds ALL possible mutable roots (for Phi nodes).
//
// Unlike FindMutableRoot which returns the first root found, this function
// collects all possible roots from all Phi edges. This is needed for
// conditional code where different branches may have different roots:
//
//	var q *gorm.DB
//	if cond {
//	    q = db.Where("a")     // root #1
//	} else {
//	    q = db.Where("b")     // root #2
//	}
//	q.Find(nil)               // Phi node: needs to check BOTH roots
func (t *RootTracer) FindAllMutableRoots(v ssa.Value, loopInfo *cfg.LoopInfo) []ssa.Value {
	return t.traceAll(v, make(map[ssa.Value]bool), loopInfo)
}

// IsPureFunction checks if a function is marked as pure (doesn't pollute arguments).
//
// A function is pure if:
//   - It's a builtin pure method (Session, WithContext, Debug, Open, Begin, Transaction)
//   - It's marked with //gormreuse:pure directive
//
// Pure functions don't pollute their *gorm.DB arguments.
// Note: User-defined pure functions may return mutable values - only builtin pure
// methods are guaranteed to return immutable values.
func (t *RootTracer) IsPureFunction(fn *ssa.Function) bool {
	if fn == nil {
		return false
	}
	if t.isImmutableReturningBuiltin(fn) {
		return true
	}
	// A //gormreuse:pure function that failed its own contract validation is
	// NOT trusted here: its callers must see the leak it hides.
	if t.pure.HasFailed(fn) {
		return false
	}
	return t.pureFuncs.Contains(fn)
}

// Callee returns the function that common calls: its static callee, or the
// function a package-level variable holds (see FuncVars). It returns nil when
// neither is known.
func (t *RootTracer) Callee(common *ssa.CallCommon) *ssa.Function {
	return t.funcVars.Callee(common)
}

// IsScopesCallbackFunc reports whether fn is a Scopes/Preload callback. The
// analyzer uses this to recurse into such callbacks even when they capture no
// *gorm.DB (they operate on their parameter, not a captured variable), so reuse
// of the mutable parameter inside them is still detected (#60).
func (t *RootTracer) IsScopesCallbackFunc(fn *ssa.Function) bool {
	return t.scopesCallbacks[fn]
}

// isImmutableReturningBuiltin checks if a function is a builtin method that returns immutable *gorm.DB.
// Builtin methods (Session, WithContext, Debug, etc.) return immutable *gorm.DB.
// This is used for tracing - only builtin methods have immutable return values.
//
// The name lookup is gated on the function actually belonging to gorm.io/gorm
// (see isGormBuiltinFunc). Without this gate, any user-defined function sharing a
// builtin name — a method named Session, a helper named Open — would be silently
// trusted as immutable-returning (and, via IsPureFunction, as pure), disabling
// reuse detection around it.
func (t *RootTracer) isImmutableReturningBuiltin(fn *ssa.Function) bool {
	if fn == nil {
		return false
	}
	if !typeutil.IsImmutableReturningBuiltin(fn.Name()) {
		return false
	}
	return isGormBuiltinFunc(fn)
}

// isGormBuiltinFunc reports whether fn is genuinely defined by gorm.io/gorm:
// either a method whose receiver is gorm.DB (Session, WithContext, Debug, Begin,
// Transaction), or a package-level function in the gorm.io/gorm package
// (gorm.Open). User-defined functions that merely share a builtin name return false.
func isGormBuiltinFunc(fn *ssa.Function) bool {
	if sig := fn.Signature; sig != nil && sig.Recv() != nil {
		return typeutil.IsGormDB(sig.Recv().Type())
	}
	if obj := fn.Object(); obj != nil {
		return typeutil.IsGormPackage(obj.Pkg())
	}
	return false
}

// trace is the core tracing function that finds the mutable root for a value.
//
// Tracing flow:
//
//	┌──────────────┐
//	│  Input value │
//	└──────┬───────┘
//	       │
//	       ▼
//	┌──────────────────┐     yes    ┌─────────────────┐
//	│ Already visited? │───────────▶│ Return nil      │
//	└──────┬───────────┘            │ (cycle detected)│
//	       │ no                     └─────────────────┘
//	       ▼
//	┌──────────────────┐     yes    ┌─────────────────┐
//	│ Immutable source?│───────────▶│ Return nil      │
//	│ (param/const/    │            │ (no mutable     │
//	│  pure call)      │            │  root exists)   │
//	└──────┬───────────┘            └─────────────────┘
//	       │ no
//	       ▼
//	┌──────────────────┐     yes    ┌─────────────────┐
//	│   *ssa.Call?     │───────────▶│ traceCall()     │
//	└──────┬───────────┘            └─────────────────┘
//	       │ no
//	       ▼
//	┌──────────────────┐
//	│  traceNonCall()  │
//	│ (Phi/UnOp/etc.)  │
//	└──────────────────┘
func (t *RootTracer) trace(v ssa.Value, visited map[ssa.Value]bool, loopInfo *cfg.LoopInfo) ssa.Value {
	if v == nil || visited[v] {
		return nil
	}
	visited[v] = true

	// Under Phase 1b a *gorm.DB parameter is a mutable root by default (a caller
	// may pass a mid-chain clone==0 value that this function then branches). This
	// must be checked BEFORE isImmutableSource, which still reports the exempt
	// parameters (immutable-param / Transaction callback) as immutable (#61, #60).
	if p, ok := v.(*ssa.Parameter); ok && t.isMutableParam(p) {
		return p
	}

	// Check if this is an immutable source
	if t.isImmutableSource(v) {
		return nil
	}

	// Handle gorm method calls
	if call, ok := v.(*ssa.Call); ok {
		return t.traceCall(call, visited, loopInfo)
	}

	// Handle non-call values
	return t.traceNonCall(v, visited, loopInfo)
}

// traceCall handles *ssa.Call values during tracing.
//
// Four cases:
//  1. Pure function whose result (alone or in a tuple) may be its argument:
//     trace the argument
//  2. IIFE (Immediately Invoked Function Expression): trace through returns
//  3. Gorm method call: STOP - this call is the mutable root
//  4. Any other call whose result holds a *gorm.DB (alone or in a tuple),
//     including dynamic and interface calls: treat as root unless the callee
//     is known to return immutable
//
// A call through a method value (w := r.Where; w(x)) is handled as the method
// call r.Where(x) it stands for, not as an IIFE of its synthetic wrapper (#178).
//
// Example IIFE tracing:
//
//	_ = func() *gorm.DB { return q.Where("y") }().Find(nil)
//	      │                        │
//	      │                        └─── Inner call is traced
//	      └─── Outer IIFE call triggers traceIIFEReturns
func (t *RootTracer) traceCall(call *ssa.Call, visited map[ssa.Value]bool, loopInfo *cfg.LoopInfo) ssa.Value {
	// The result of a pure function may be its argument (#188).
	if args := t.pureReturnedArgs(call); len(args) > 0 {
		for _, arg := range args {
			if root := t.trace(arg, cloneVisited(visited), loopInfo); root != nil {
				return root
			}
		}
		if callee, _, _ := t.calledFunc(call); t.returnsOnlyParams(callee) {
			return nil
		}
	}

	// Handle closures (IIFE)
	if mc, ok := call.Call.Value.(*ssa.MakeClosure); ok && !isMethodValueCall(call) {
		if closureFn, ok := mc.Fn.(*ssa.Function); ok {
			if root := t.traceIIFEReturns(closureFn, visited, loopInfo); root != nil {
				// If closure result is stored in a variable (Extract instruction),
				// treat each call as independent root.
				// Only trace through IIFE when result is directly chained.
				if isClosureResultStored(call) {
					return call
				}
				return root
			}
		}
	}

	callee, _, _ := t.calledFunc(call)
	if callee != nil && isGormDBMethod(callee) {
		// This is a gorm method call - THIS CALL is the mutable root
		// Don't trace further - each gorm call result is its own root
		// This implements "variable assignment creates new root" semantics
		return call
	}

	// Not a gorm method: a user function, a dynamic call (a func-typed value)
	// or an interface method. If its result carries a *gorm.DB, alone or in a
	// tuple such as (*gorm.DB, error) reached through an Extract, the call is
	// a mutable root (#176). A dynamic or interface callee is unknown, so its
	// result is conservatively mutable too.
	if !typeutil.HasGormDBResult(call.Type()) {
		return nil
	}
	// Builtin or //gormreuse:immutable-return function returns immutable.
	if t.returnsImmutable(callee) {
		return nil
	}
	// User-defined pure or non-pure function: treat call as mutable root
	// (user-defined pure functions may return mutable values)
	return call
}

// traceNonCall handles non-call SSA values during tracing.
// Routes to specialized handlers based on the value type.
func (t *RootTracer) traceNonCall(v ssa.Value, visited map[ssa.Value]bool, loopInfo *cfg.LoopInfo) ssa.Value {
	switch val := v.(type) {
	case *ssa.Phi:
		// Phi: merge point from conditional branches (if/switch)
		return t.tracePhi(val, visited, loopInfo)

	case *ssa.UnOp:
		// UnOp: unary operations including pointer dereference (*ptr)
		return t.traceUnOp(val, visited, loopInfo)

	case *ssa.ChangeType:
		// ChangeType: type conversion (same underlying type)
		return t.trace(val.X, visited, loopInfo)

	case *ssa.MakeInterface:
		// MakeInterface: interface{}(x) boxing — trace through to the boxed value
		// so a value stored in an interface{} and later extracted stays tracked.
		return t.trace(val.X, visited, loopInfo)

	case *ssa.TypeAssert:
		// TypeAssert: i.(*gorm.DB) extraction — trace through to the asserted
		// operand. Combined with MakeInterface above, this keeps an interface
		// round-trip (var i interface{} = q; q2 := i.(*gorm.DB)) connected to q.
		return t.trace(val.X, visited, loopInfo)

	case *ssa.Extract:
		// Extract: extract element from tuple (multi-return)
		return t.trace(val.Tuple, visited, loopInfo)

	case *ssa.FreeVar:
		// FreeVar: captured variable in a closure
		return t.traceFreeVar(val, visited, loopInfo)

	case *ssa.Alloc:
		// Alloc: local variable allocation
		return t.traceAlloc(val, visited, loopInfo)

	case *ssa.Field:
		// Field: a field of a struct held in a register, as promoted methods
		// of an embedded *gorm.DB read it (#181)
		if vals := fieldValueStoredValues(val); len(vals) > 0 {
			return t.trace(vals[0], visited, loopInfo)
		}
		return nil

	case *FieldRoot:
		// FieldRoot: a field of a struct a call returned (#181)
		return t.internFieldRoot(val)

	default:
		return nil
	}
}

// isSwapPhiPair checks if two Phi nodes form a swap pattern.
//
// A swap pattern occurs when two Phi nodes in the same block have edges that are
// swapped versions of each other. This typically happens when variables are swapped
// in conditional code.
//
// # Pattern Detection
//
// Given two Phi nodes:
//
//	phiA = phi [pred0: x, pred1: y, ...]
//	phiB = phi [pred0: y, pred1: x, ...]
//	                ^^^       ^^^
//	                swapped!
//
// The function returns true if there exists at least one pair of edges (i, j) where:
//   - phiA.Edges[i] == phiB.Edges[j]  AND
//   - phiA.Edges[j] == phiB.Edges[i]
//
// # Example 1: Variable Swap in Loop
//
// Go code:
//
//	q1 := db.Where("q1")
//	q2 := db.Where("q2")
//	for _, item := range items {
//	    if item%2 == 0 {
//	        temp := q1
//	        q1 = q2  // Swap
//	        q2 = temp
//	    }
//	    q1 = q1.Where(...)
//	}
//
// SSA representation:
//
//	Block 1 (loop header):
//	  t5 = phi [entry: t1, back: t23] #q1
//	  t6 = phi [entry: t3, back: t17] #q2
//
//	Block 5 (after if, inside loop):
//	  t16 = phi [no-swap: t5, swap: t6] #q1
//	  t17 = phi [no-swap: t6, swap: t5] #q2
//	                      ^^        ^^
//	                      These are swapped!
//
// In this case:
//   - t16 has edges [t5, t6]
//   - t17 has edges [t6, t5]  ← swapped!
//   - isSwapPhiPair(t16, t17) returns true
//
// # Example 2: Conditional Swap (Non-Loop)
//
// Go code:
//
//	q1 := db.Where("q1")
//	q2 := db.Where("q2")
//	if swap {
//	    q1, q2 = q2, q1  // Modern swap syntax
//	}
//	q1.Find(nil)
//
// SSA representation:
//
//	Block 0 (entry):
//	  t1 = db.Where("q1")
//	  t3 = db.Where("q2")
//
//	Block 2 (after if):
//	  t7 = phi [no-swap: t1, swap: t3] #q1
//	  t8 = phi [no-swap: t3, swap: t1] #q2
//	                      ^^        ^^
//	                      Also swapped!
//
// In this case:
//   - t7 has edges [t1, t3]
//   - t8 has edges [t3, t1]  ← swapped!
//   - isSwapPhiPair(t7, t8) returns true
//
// # Why This Matters
//
// Detecting swap patterns is crucial for correct pollution tracking:
//   - In loops with only assignments: swapped variables remain independent
//   - In conditionals with pre-pollution: swap propagates pollution to both
//
// See collectInitialEdgeRoots() for how this is used in pollution detection.
func isSwapPhiPair(phiA, phiB *ssa.Phi) bool {
	if len(phiA.Edges) != len(phiB.Edges) {
		return false
	}

	// Check if there's at least one pair of swapped edges
	for i := range phiA.Edges {
		edgeA := phiA.Edges[i]
		edgeB := phiB.Edges[i]

		// Look for the swapped pattern in other edges
		for j := range phiA.Edges {
			if i == j {
				continue
			}
			otherA := phiA.Edges[j]
			otherB := phiB.Edges[j]

			// Check if (edgeA, edgeB) == (otherB, otherA) - swap pattern
			if edgeA == otherB && edgeB == otherA {
				return true
			}
		}
	}
	return false
}

// findSwapPhiSibling finds a swap-pair sibling of phi in the same block.
//
// When variables are swapped (e.g., q1, q2 = q2, q1), SSA creates two Phi nodes
// in the same block with swapped edges. This function searches for that sibling.
//
// # Search Process
//
// Given a Phi node, this function:
//  1. Iterates through all instructions in the same basic block
//  2. For each Phi instruction found:
//     - Skip if it's the same phi (self)
//     - Check if it forms a swap-pair using isSwapPhiPair()
//  3. Returns the first matching sibling, or nil if none found
//
// # Example: Loop Variable Swap
//
// SSA block structure:
//
//	Block 5 (if.done - after conditional swap):
//	  t16 = phi [no-swap: t5, swap: t6] #q1  ← Input phi
//	  t17 = phi [no-swap: t6, swap: t5] #q2  ← Sibling found!
//	  t18 = phi [...]                        ← Not a swap-pair, skipped
//	  ...
//
// Given t16 as input:
//   - findSwapPhiSibling(t16) returns t17
//   - findSwapPhiSibling(t17) returns t16 (symmetric)
//
// # Why Search in Same Block?
//
// Swap Phi nodes ALWAYS appear in the same basic block because:
//   - They represent the merge point after a conditional branch
//   - Both variables are updated at the exact same control flow point
//   - SSA ensures all Phi nodes for a block are at the block's start
//
// # Usage Pattern
//
// This is used as a quick check to detect swap patterns:
//
//	if findSwapPhiSibling(phi) != nil {
//	    // This phi is part of a swap - apply special handling
//	    ...
//	}
//
// See tracePhi(), traceAll(), and traceAllPointerLoads() for actual usage.
func findSwapPhiSibling(phi *ssa.Phi) *ssa.Phi {
	block := phi.Block()
	for _, instr := range block.Instrs {
		if otherPhi, ok := instr.(*ssa.Phi); ok && otherPhi != phi && isSwapPhiPair(phi, otherPhi) {
			return otherPhi
		}
	}
	return nil
}

// isLoopVariableSwap checks if a Phi node is part of a loop variable swap pattern.
// Returns the loop-header Phis if this is a swap, nil otherwise.
// This is a convenience function that combines swap detection checks.
func isLoopVariableSwap(phi *ssa.Phi, loopInfo *cfg.LoopInfo) []*ssa.Phi {
	if loopInfo == nil {
		return nil
	}
	// Don't apply to loop-header phis themselves
	if loopInfo.IsLoopHeader(phi.Block()) {
		return nil
	}
	// Must have a swap sibling
	if findSwapPhiSibling(phi) == nil {
		return nil
	}
	// Edges must all be loop-header phis
	return getLoopHeaderPhiEdges(phi, loopInfo)
}

// getLoopHeaderPhiEdges checks if all edges of a Phi are loop-header Phis.
// Returns the list of loop-header Phis if all edges qualify, nil otherwise.
func getLoopHeaderPhiEdges(phi *ssa.Phi, loopInfo *cfg.LoopInfo) []*ssa.Phi {
	if loopInfo == nil {
		return nil
	}

	var loopHeaderPhis []*ssa.Phi
	for _, edge := range phi.Edges {
		edgePhi, ok := edge.(*ssa.Phi)
		if !ok || !loopInfo.IsLoopHeader(edgePhi.Block()) {
			return nil
		}
		loopHeaderPhis = append(loopHeaderPhis, edgePhi)
	}
	return loopHeaderPhis
}

// collectInitialEdgeRoots collects roots from INITIAL edges (entry edges) of
// loop-header Phis, used to detect pre-loop pollution that can propagate through swaps.
//
// # Why Only Initial Edges?
//
// Loop-header Phis have two types of edges:
//  1. Initial edge (index 0): Value entering the loop from outside
//  2. Back-edge (index 1+): Value from loop body (loop iteration update)
//
// For loop variable swaps, we only care about pollution from BEFORE the loop starts:
//   - If variables are clean entering the loop → they stay independent during iteration
//   - If one is polluted before loop → swap propagates pollution to both
//
// Back-edges represent loop-internal assignments (e.g., q1 = q1.Where(...)) which
// don't pollute the variables themselves - they're just updates.
//
// # The Two Scenarios
//
// ## Scenario 1: Clean Variables Before Loop (No Violation)
//
// Go code:
//
//	q1 := db.Where("q1")  // Clean
//	q2 := db.Where("q2")  // Clean
//	for _, item := range items {
//	    if item%2 == 0 {
//	        q1, q2 = q2, q1  // Swap
//	    }
//	    q1 = q1.Where(...)   // Assignment (not polluting)
//	}
//	q1.Find(nil)  // First actual use - OK
//	q2.Find(nil)  // First actual use - OK
//
// SSA trace for q2.Find():
//
//	Block 1 (loop header):
//	  t5 = phi [entry: t1, back: t23] #q1
//	            initial ^^       ^^^ back-edge (assignment result)
//	  t6 = phi [entry: t3, back: t17] #q2
//	            initial ^^       ^^^ back-edge (swap result)
//
//	Block 5 (swap merge):
//	  t16 = phi [t5, t6]  ← Swap-phi for q1
//	  t17 = phi [t6, t5]  ← Swap-phi for q2
//
// For q2.Find():
//   - FindMutableRoot(t6) → t17 (swap-phi, treated as independent)
//   - FindAllMutableRoots(t6) calls this function:
//     → collectInitialEdgeRoots([t5, t6], t17, ...)
//     → Traces t5's initial edge: t1 (clean, no pollution)
//     → Traces t6's initial edge: t3 (clean, no pollution)
//     → Returns [t1, t3, t17]
//   - Check pollution: t1 (clean), t3 (clean), t17 (clean)
//   - Result: No violation ✓
//
// ## Scenario 2: Pre-Polluted Variable (Violation Detected)
//
// Go code:
//
//	q1 := db.Where("q1")
//	q2 := db.Where("q2")
//	q1.Find(nil)  // ← Pollute q1 BEFORE loop
//	for _, item := range items {
//	    if item%2 == 0 {
//	        q1, q2 = q2, q1  // Swap propagates pollution!
//	    }
//	    q1 = q1.Where(...)  // ← Uses potentially polluted value
//	}
//
// SSA trace for q1.Where() inside loop:
//
//	Block 0:
//	  t1 = db.Where("q1")
//	  t3 = db.Where("q2")
//	  t1.Find(nil)  ← t1 is now polluted
//
//	Block 1 (loop header):
//	  t5 = phi [entry: t1, back: t23]  ← t1 is polluted!
//	            initial ^^
//	  t6 = phi [entry: t3, back: t17]
//	            initial ^^
//
//	Block 5 (swap merge):
//	  t16 = phi [t5, t6]  ← May contain t1 (polluted) or t6 (clean)
//	  t17 = phi [t6, t5]  ← May contain t6 (clean) or t5 (polluted)
//
// For t16.Where() (the assignment in loop):
//   - FindMutableRoot(t16) → t16 (swap-phi)
//   - FindAllMutableRoots(t16) calls this function:
//     → collectInitialEdgeRoots([t5, t6], t16, ...)
//     → Traces t5's initial edge: t1 (POLLUTED! ✗)
//     → Traces t6's initial edge: t3 (clean)
//     → Returns [t1, t3, t16]
//   - Check pollution: t1 is polluted!
//   - Result: Violation detected ✓
//
// # Implementation Details
//
// For each loop-header Phi:
//  1. Get the first edge (index 0) - this is the initial/entry edge
//  2. Skip if nil constant or already visited (cycle detection)
//  3. Recursively trace using traceAll() to find all possible roots
//  4. Collect all roots from all loop-header Phis
//  5. Add the swap-phi itself as a root (for independent tracking)
//
// # Return Value
//
// Returns a list of roots that includes:
//   - All roots from initial edges of all loop-header Phis
//   - The swap-phi itself (for treating it as an independent root in some contexts)
//
// This allows pollution detection to:
//   - Check if ANY initial value was polluted before the loop
//   - Treat the swap-phi as a separate entity for FindMutableRoot()
func (t *RootTracer) collectInitialEdgeRoots(loopHeaderPhis []*ssa.Phi, swapPhi ssa.Value, visited map[ssa.Value]bool, loopInfo *cfg.LoopInfo) []ssa.Value {
	var roots []ssa.Value
	for _, loopPhi := range loopHeaderPhis {
		// Get the initial edge (first edge, not back-edge)
		if len(loopPhi.Edges) > 0 {
			initialEdge := loopPhi.Edges[0]
			if !isNilConst(initialEdge) && !visited[initialEdge] {
				roots = append(roots, t.traceAll(initialEdge, visited, loopInfo)...)
			}
		}
	}
	// Also include the swap-phi itself as a root
	roots = append(roots, swapPhi)
	return roots
}

func (t *RootTracer) tracePhi(phi *ssa.Phi, visited map[ssa.Value]bool, loopInfo *cfg.LoopInfo) ssa.Value {
	// For loop-header Phi nodes, prioritize back-edges over initial edges
	// This ensures we return the loop-internal assignment result, not the pre-loop initial value
	if loopInfo != nil && loopInfo.IsLoopHeader(phi.Block()) {
		phiBlock := phi.Block()
		phiBlockIndex := slices.Index(phi.Parent().Blocks, phiBlock)

		// First pass: try back-edges (predecessors with higher indices)
		if phiBlockIndex >= 0 {
			for i, edge := range phi.Edges {
				if isNilConst(edge) || visited[edge] {
					continue
				}
				// Check if this is a back-edge (predecessor comes after phi block)
				predIndex := slices.Index(phi.Parent().Blocks, phiBlock.Preds[i])
				// Back-edge: predecessor comes after phi block (loops back)
				if predIndex > phiBlockIndex {
					if root := t.trace(edge, visited, loopInfo); root != nil {
						return root
					}
				}
			}

			// Second pass: try forward-edges if no back-edge root found
			for _, edge := range phi.Edges {
				if isNilConst(edge) || visited[edge] {
					continue
				}
				if root := t.trace(edge, visited, loopInfo); root != nil {
					return root
				}
			}
		}
		return nil
	}

	// =========================================================================
	// NON-LOOP-HEADER PHI: Conditional Merge Patterns
	// =========================================================================
	//
	// Non-loop-header Phis represent merges in conditional code (if/else).
	// We need to handle several patterns correctly:
	//
	// ## Pattern 1: Loop Variable Swap (Special Case)
	//
	// When variables are swapped inside a loop:
	//
	//   for _, item := range items {
	//       if item%2 == 0 {
	//           q1, q2 = q2, q1  // ← Creates swap-phi
	//       }
	//       q1 = q1.Where(...)    // ← Uses merged value
	//   }
	//
	// SSA:
	//   Block 1 (loop header):
	//     t5 = phi [entry, back] #q1
	//     t6 = phi [entry, back] #q2
	//   Block 5 (after if):
	//     t16 = phi [no-swap: t5, swap: t6]  ← Swap-phi
	//     t17 = phi [no-swap: t6, swap: t5]  ← Swap-phi sibling
	//
	// For loop variable swaps, we treat the swap-phi itself as the root,
	// keeping q1 and q2 independent during loop iteration.
	//
	// ## Pattern 2: Reassignment to Same Symbol in If (Regular Phi)
	//
	// When a variable is reassigned in only one branch:
	//
	//   q := db.Where("base")
	//   if cond {
	//       q = q.Where("extra")  // ← Reassignment
	//   }
	//   q.Find(nil)  // ← Uses merged value
	//
	// SSA:
	//   Block 0:
	//     t1 = db.Where("base")
	//   Block 1 (if.then):
	//     t2 = t1.Where("extra")
	//   Block 2 (if.done):
	//     t3 = phi [no-if: t1, if: t2] #q
	//                      ^^      ^^
	//                      Different values, but same LINEAGE
	//
	// This is NOT a swap (no sibling), just a regular conditional update.
	// We trace through the phi normally to find the root (t1 or t2's root).
	//
	// ## Pattern 3: Different Symbols Merged (Regular Phi)
	//
	// When different variables are assigned in different branches:
	//
	//   var q *gorm.DB
	//   if cond {
	//       q = db.Where("a")  // ← First definition
	//   } else {
	//       q = db.Where("b")  // ← Second definition
	//   }
	//   q.Find(nil)  // ← Uses merged value
	//
	// SSA:
	//   Block 2 (if.done):
	//     t3 = phi [if: t1, else: t2] #q
	//                   ^^       ^^
	//                   Different roots, need to check ALL
	//
	// This is also NOT a swap, we trace through normally and FindAllMutableRoots
	// will collect all possible roots (t1 and t2) for pollution checking.
	//
	// ## Detection Logic
	//
	// We only apply special swap-phi handling if ALL conditions are met:
	//   1. loopInfo != nil (we're inside a loop context)
	//   2. findSwapPhiSibling(phi) != nil (this phi has a swap sibling)
	//   3. getLoopHeaderPhiEdges(phi) != nil (edges are loop-header phis)
	//
	// If any condition fails, we fall through to regular tracing.
	//
	if isLoopVariableSwap(phi, loopInfo) != nil {
		// Loop variable swap detected - return phi as independent root
		// This prevents cross-contamination between swapped loop variables
		return phi
	}

	// =========================================================================
	// REGULAR TRACING: Standard Phi Handling
	// =========================================================================
	//
	// For all other cases (non-swap phis, or swaps without loop context):
	//   - Trace through all edges to find the first mutable root
	//   - Skip nil constants and already-visited values (cycle detection)
	//   - Return the first root found, or nil if no root exists
	//
	// This handles:
	//   - Simple reassignment in if: q = q.Where("x") in one branch
	//   - Different values merged: if { q = a } else { q = b }
	//   - Conditional swaps outside loops
	//
	for _, edge := range phi.Edges {
		if isNilConst(edge) || visited[edge] {
			continue
		}
		if root := t.trace(edge, visited, loopInfo); root != nil {
			return root
		}
	}
	return nil
}

func (t *RootTracer) traceUnOp(unop *ssa.UnOp, visited map[ssa.Value]bool, loopInfo *cfg.LoopInfo) ssa.Value {
	if unop.Op == token.MUL {
		// Pointer dereference - trace through the pointer
		return t.tracePointerLoad(unop, visited, loopInfo)
	}
	return t.trace(unop.X, visited, loopInfo)
}

func (t *RootTracer) tracePointerLoad(load *ssa.UnOp, visited map[ssa.Value]bool, loopInfo *cfg.LoopInfo) ssa.Value {
	switch p := load.X.(type) {
	case *ssa.FreeVar:
		return t.traceFreeVar(p, visited, loopInfo)
	case *ssa.Alloc:
		return t.traceAlloc(p, visited, loopInfo)
	case *ssa.FieldAddr:
		return t.traceFieldStore(p, load, visited, loopInfo)
	case *ssa.Global:
		// Single-root: trace the first value stored into the global that
		// reaches the load.
		if vals := globalStoredValues(p, load); len(vals) > 0 {
			return t.trace(vals[0], visited, loopInfo)
		}
		return nil
	default:
		return t.trace(p, visited, loopInfo)
	}
}

// traceFreeVar traces a captured variable in a closure back to its binding.
//
// When a closure captures a variable, SSA represents it as:
//
//	Parent function:
//	    t1 = MakeClosure fn [q]    // q is bound to the closure
//	                      ↑
//	                      └─── Bindings[0] = q
//
//	Closure function (fn):
//	    t2 = FreeVar [0]           // References Bindings[0] from parent
//
// This function finds the MakeClosure in the parent and traces the binding.
func (t *RootTracer) traceFreeVar(fv *ssa.FreeVar, visited map[ssa.Value]bool, loopInfo *cfg.LoopInfo) ssa.Value {
	if binding := FreeVarBinding(fv); binding != nil {
		return t.trace(binding, visited, loopInfo)
	}
	return nil
}

// FreeVarBinding resolves the value captured by fv: it locates the MakeClosure
// that creates fv's parent closure and returns the binding at fv's index in the
// captured-variable list. Returns nil if the closure or binding can't be found.
// Shared by traceFreeVar (single root), traceAllFreeVar (all roots), the Scopes
// callback resolver, and scopeswarn.
func FreeVarBinding(fv *ssa.FreeVar) ssa.Value {
	fn := fv.Parent()
	if fn == nil {
		return nil
	}

	// Find the index of this FreeVar in the function's FreeVars list
	idx := slices.Index(fn.FreeVars, fv)
	if idx < 0 {
		return nil
	}

	// Find the MakeClosure instruction in the parent function
	parent := fn.Parent()
	if parent == nil {
		return nil
	}

	for _, block := range parent.Blocks {
		for _, instr := range block.Instrs {
			mc, ok := instr.(*ssa.MakeClosure)
			if !ok {
				continue
			}
			closureFn, ok := mc.Fn.(*ssa.Function)
			if !ok || closureFn != fn {
				continue
			}
			if idx < len(mc.Bindings) {
				return mc.Bindings[idx]
			}
		}
	}
	return nil
}

// traceAlloc traces a local variable (Alloc) by finding Store instructions.
//
// In SSA, local variables are represented as:
//
//	t1 = Alloc *gorm.DB (q)     // Allocate space for variable q
//	Store t1 <value>            // Store a value into q
//	t2 = UnOp * t1              // Load value from q (dereference)
//
// This function finds the Store instruction that writes to the Alloc.
func (t *RootTracer) traceAlloc(alloc *ssa.Alloc, visited map[ssa.Value]bool, loopInfo *cfg.LoopInfo) ssa.Value {
	// Single-root: trace the first value stored into the Alloc.
	if vals := AllocStoredValues(alloc); len(vals) > 0 {
		return t.trace(vals[0], visited, loopInfo)
	}
	return nil
}

// AllocStoredValues returns, in program order, the values stored into alloc.
// Shared by traceAlloc (first), traceAllAllocStores (all), the Scopes callback
// resolver, and scopeswarn.
func AllocStoredValues(alloc *ssa.Alloc) []ssa.Value {
	fn := alloc.Parent()
	if fn == nil {
		return nil
	}
	var vals []ssa.Value
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			if store, ok := instr.(*ssa.Store); ok && store.Addr == alloc {
				vals = append(vals, store.Val)
			}
		}
	}
	return vals
}

// traceFieldStore traces a struct field access by finding Store instructions.
//
// When a struct field is accessed:
//
//	type Handler struct { db *gorm.DB }
//	h.db.Find(nil)
//
// SSA represents this as:
//
//	t1 = FieldAddr h.db        // Get address of field
//	t2 = UnOp * t1             // Dereference to get value
//	                           // (traced here to find the Store)
//
// This function finds the Store that wrote to the same field and can reach the
// load.
func (t *RootTracer) traceFieldStore(fa *ssa.FieldAddr, load *ssa.UnOp, visited map[ssa.Value]bool, loopInfo *cfg.LoopInfo) ssa.Value {
	// Single-root: trace the first value stored into the same field.
	if vals := fieldStoredValues(fa, load); len(vals) > 0 {
		return t.trace(vals[0], visited, loopInfo)
	}
	return nil
}

// fieldStoredValues returns, in program order, the values stored into the same
// struct field as fa. Shared by traceFieldStore (first), traceAllFieldStores
// (all), and the Scopes callback resolver.
//
// With a load, only the stores that can reach it are returned (see
// fieldStores). With a nil load, every store in the function is returned.
func fieldStoredValues(fa *ssa.FieldAddr, load ssa.Instruction) []ssa.Value {
	root, path := fieldAddrPath(fa)
	return fieldStoredValuesAt(fa.Parent(), root, path, load)
}

// fieldValueStoredValues is fieldStoredValues for a field read from a struct
// held in a register rather than through its address (#181):
//
//	t4 = *t0             // e := embedded{q}; e.Where("a")
//	t5 = t4.DB           // Field, not FieldAddr: promoted methods read this way
//
// A Field chain over a load resolves to the address that was loaded, so it
// matches the same stores as a FieldAddr chain. A Field chain over a call that
// returns a struct yields that call's FieldRoot. A struct that a Phi merges
// yields the field of every edge.
func fieldValueStoredValues(f *ssa.Field) []ssa.Value {
	return structFieldValues(f, nil, make(map[ssa.Value]bool))
}

// structFieldValues returns the values of the field that path selects from the
// struct value x. seen stops Phi cycles through loops.
func structFieldValues(x ssa.Value, path []int, seen map[ssa.Value]bool) []ssa.Value {
	for {
		field, ok := x.(*ssa.Field)
		if !ok {
			break
		}
		path = append([]int{field.Field}, path...)
		x = field.X
	}
	switch base := x.(type) {
	case *ssa.Call:
		return []ssa.Value{&FieldRoot{Call: base, Path: path}}
	case *ssa.Phi:
		if seen[base] {
			return nil
		}
		seen[base] = true
		var vals []ssa.Value
		for _, edge := range base.Edges {
			vals = append(vals, structFieldValues(edge, path, seen)...)
		}
		return vals
	case *ssa.UnOp:
		if base.Op != token.MUL {
			return nil
		}
		root, prefix := base.X, []int(nil)
		if fa, ok := base.X.(*ssa.FieldAddr); ok {
			root, prefix = fieldAddrPath(fa)
		}
		return fieldStoredValuesAt(base.Parent(), root, append(prefix, path...), base)
	default:
		return nil
	}
}

// fieldStoredValuesAt returns the values stored into the field that path
// selects from root, as read by load in fn. Each address root may stand for
// (see fieldBaseAddrs) is searched in the function that defines it, or in fn
// for one without a function, such as a global. Only the stores that reach load
// are returned when the address is defined in fn. Stores in an enclosing
// function have no CFG path to a load in a closure, so all of them are returned.
func fieldStoredValuesAt(fn *ssa.Function, root ssa.Value, path []int, load ssa.Instruction) []ssa.Value {
	var vals []ssa.Value
	for _, base := range fieldBaseAddrs(root, make(map[ssa.Value]bool)) {
		baseFn, baseLoad := base.Parent(), load
		if baseFn == nil {
			baseFn = fn
		}
		if baseFn != fn {
			baseLoad = nil
		}
		vals = append(vals, fieldStoredValuesFrom(baseFn, base, path, baseLoad, make(map[fieldRef]bool))...)
	}
	return vals
}

// fieldBaseAddrs returns the addresses whose stores a field access through root
// can see. Two forms of root do not hold the stores themselves (#181):
//
//   - A FreeVar: a closure reads a captured struct through it, while the stores
//     are in the enclosing function, on the captured Alloc. The binding stands
//     for it.
//   - A load of a pointer variable (`*h`, where `h := &T{...}` is captured or
//     has its address taken, so it lives in an Alloc): every access loads h
//     afresh, so no two accesses share the load. Each pointer stored into the
//     variable stands for it.
//
// Any other root stands for itself.
func fieldBaseAddrs(root ssa.Value, seen map[ssa.Value]bool) []ssa.Value {
	if seen[root] {
		return nil
	}
	seen[root] = true
	switch r := root.(type) {
	case *ssa.FreeVar:
		if binding := FreeVarBinding(r); binding != nil {
			return fieldBaseAddrs(binding, seen)
		}
	case *ssa.UnOp:
		if r.Op != token.MUL {
			break
		}
		slot := r.X
		if fv, ok := slot.(*ssa.FreeVar); ok {
			slot = FreeVarBinding(fv)
		}
		if alloc, ok := slot.(*ssa.Alloc); ok {
			var bases []ssa.Value
			for _, ptr := range AllocStoredValues(alloc) {
				bases = append(bases, fieldBaseAddrs(ptr, seen)...)
			}
			return bases
		}
	}
	return []ssa.Value{root}
}

// FieldRoot is the mutable root of a *gorm.DB field of a struct that a call
// returned, by value (#181) or through a pointer (#250):
//
//	s := mk(db)          // t1 = mk(db); *t0 = t1
//	s.DB.Where("a")      // root: FieldRoot{t1, [DB]}
//	s.DB.Where("b")      // the same FieldRoot: second branch
//
// It extends "a call returning *gorm.DB is a root" to a call returning a struct
// that holds one. The SSA has no value for "field DB of t1", so the tracer
// makes one per (call, field path) and returns the same pointer for each (see
// RootTracer.internFieldRoot). Path indexes the struct the call returns, or
// the one its pointer result points to. Fields read through a parameter or a
// receiver stay untracked.
type FieldRoot struct {
	Call *ssa.Call
	Path []int
}

// fieldRootStruct returns the struct that a FieldRoot path step selects from:
// t itself, or what it points to.
func fieldRootStruct(t types.Type) *types.Struct {
	if ptr, ok := t.Underlying().(*types.Pointer); ok {
		t = ptr.Elem()
	}
	return t.Underlying().(*types.Struct)
}

// Name names the field of the call: "t1.DB".
func (r *FieldRoot) Name() string { return r.Call.Name() + r.pathString() }

// String describes the field of the call: "makeTwoDB().DB".
func (r *FieldRoot) String() string { return r.Call.String() + r.pathString() }

// Type returns the type of the field.
func (r *FieldRoot) Type() types.Type {
	t := r.Call.Type()
	for _, i := range r.Path {
		t = fieldRootStruct(t).Field(i).Type()
	}
	return t
}

// Parent returns the function that makes the call.
func (r *FieldRoot) Parent() *ssa.Function { return r.Call.Parent() }

// Referrers returns nil: no instruction refers to a FieldRoot.
func (r *FieldRoot) Referrers() *[]ssa.Instruction { return nil }

// Pos returns the position of the call, which the diagnostics name as the root.
func (r *FieldRoot) Pos() token.Pos { return r.Call.Pos() }

// Block returns the block of the call, which defines the field: a call inside a
// loop returns a new struct on each iteration (#250). See RootBlock.
func (r *FieldRoot) Block() *ssa.BasicBlock { return r.Call.Block() }

// RootBlock returns the block that defines a mutable root, or nil when it has
// none, such as a global. The loop check and the suggested fixes place a root
// by it (#250):
//
//   - an instruction (a call, a Phi) is defined in its own block;
//   - a FieldRoot is defined by its call;
//   - a parameter is defined in its function's entry block. That block is in a
//     loop only for the body of a range-over-func loop, whose parameters are
//     the values of the current iteration.
func RootBlock(v ssa.Value) *ssa.BasicBlock {
	switch r := v.(type) {
	case ssa.Instruction:
		return r.Block()
	case *FieldRoot:
		return r.Block()
	case *ssa.Parameter:
		if fn := r.Parent(); fn != nil && len(fn.Blocks) > 0 {
			return fn.Blocks[0]
		}
	}
	return nil
}

func (r *FieldRoot) pathString() string {
	var b strings.Builder
	t := r.Call.Type()
	for _, i := range r.Path {
		field := fieldRootStruct(t).Field(i)
		b.WriteString("." + field.Name())
		t = field.Type()
	}
	return b.String()
}

type fieldRootKey struct {
	call *ssa.Call
	path string
}

// internFieldRoot returns the one FieldRoot for r's call and path, or nil when
// the call returns immutable values (a builtin or //gormreuse:immutable-return
// function).
func (t *RootTracer) internFieldRoot(r *FieldRoot) ssa.Value {
	if callee, _, _ := t.calledFunc(r.Call); t.returnsImmutable(callee) {
		return nil
	}
	key := fieldRootKey{call: r.Call, path: r.pathString()}
	if existing, ok := t.fieldRoots[key]; ok {
		return existing
	}
	t.fieldRoots[key] = r
	return r
}

// fieldAddrPath resolves fa to the address it ultimately selects from together
// with the chain of field indices it walks to get there:
//
//	t1 = &h.inner        // FieldAddr
//	t2 = &t1.db          // FieldAddr    → fieldAddrPath(t2) = (h, [0 0])
//
// Matching on the whole path rather than on the immediate base value is what
// makes promoted fields (`outer{db: q}`, where db is embedded, allowed since
// Go 1.27) traceable: the SSA builder emits a fresh `&h.inner` FieldAddr for
// every access, so two accesses to the same nested field are never the same
// base value.
func fieldAddrPath(fa *ssa.FieldAddr) (root ssa.Value, path []int) {
	path = []int{fa.Field}
	root = fa.X
	for {
		parent, ok := root.(*ssa.FieldAddr)
		if !ok {
			return root, path
		}
		path = append([]int{parent.Field}, path...)
		root = parent.X
	}
}

// SameField reports whether a and b select the same field path from the same
// address. Two accesses to one field are separate FieldAddr chains, so this
// compares them as fieldStoredValues does.
func SameField(a, b *ssa.FieldAddr) bool {
	rootA, pathA := fieldAddrPath(a)
	rootB, pathB := fieldAddrPath(b)
	return rootA == rootB && slices.Equal(pathA, pathB)
}

// fieldRef identifies one (aggregate, field path, load) search. Every path
// visited during a single fieldStoredValues query is a suffix of the original
// path, so its length identifies it uniquely.
type fieldRef struct {
	root  ssa.Value
	depth int
	load  ssa.Instruction
}

// fieldStoredValuesFrom returns, in program order, the values stored into the
// field that path selects from root.
//
// Besides the direct stores it follows whole-aggregate copies into any prefix of
// path, which is required because the SSA builder does not initialize a
// composite literal in place at its destination: `h := T{f: q}` fills a separate
// `local T (complit)` temporary and copies the finished struct over.
//
//	t2 = local T (h)
//	t3 = local T (complit)
//	t4 = &t3.f
//	*t4 = q              // the store lives under the temporary
//	t5 = *t3             // load the finished literal
//	*t2 = t5             // copy it into h
//
// A FieldAddr on t2 has no matching field store of its own; following
// `*t2 = *t3` back to t3 recovers the store of q, as it was when t3 was
// loaded. seen guards against aggregates that copy into each other.
func fieldStoredValuesFrom(fn *ssa.Function, root ssa.Value, path []int, load ssa.Instruction, seen map[fieldRef]bool) []ssa.Value {
	ref := fieldRef{root: root, depth: len(path), load: load}
	if fn == nil || root == nil || len(path) == 0 || seen[ref] {
		return nil
	}
	seen[ref] = true

	stores, open := reachingFieldStores(fn, root, path, load)
	var vals []ssa.Value
	if call, ok := root.(*ssa.Call); ok && open {
		// A field reached through a pointer that a call returned holds what
		// the call put there until a store replaces it (#250). That value is
		// the call's FieldRoot, as for a struct returned by value.
		vals = append(vals, &FieldRoot{Call: call, Path: slices.Clone(path)})
	}
	for _, store := range stores {
		storePath, _ := fieldStorePath(store, root)
		if len(storePath) == len(path) {
			vals = append(vals, store.Val)
			continue
		}
		// A whole sub-aggregate was copied over a prefix of the path being
		// read; resume the search at the address it was loaded from, for the
		// remainder of the path. A struct a call returned has no stores to
		// follow: its field is a FieldRoot (#181).
		switch agg := store.Val.(type) {
		case *ssa.UnOp:
			if agg.Op == token.MUL {
				vals = append(vals, fieldStoredValuesFrom(fn, agg.X, path[len(storePath):], agg, seen)...)
			}
		case *ssa.Call:
			vals = append(vals, &FieldRoot{Call: agg, Path: slices.Clone(path[len(storePath):])})
		}
	}
	return vals
}

// fieldStorePath returns the field path that store writes below root. ok is
// false when it writes somewhere else. A store to root itself has an empty
// path.
func fieldStorePath(store *ssa.Store, root ssa.Value) (path []int, ok bool) {
	storeRoot := store.Addr
	if storeFA, isFA := store.Addr.(*ssa.FieldAddr); isFA {
		storeRoot, path = fieldAddrPath(storeFA)
	}
	return path, storeRoot == root
}

// writesFieldPath reports whether store writes the field that path selects
// from root: either that field itself, or a whole aggregate over a prefix of
// path.
func writesFieldPath(store *ssa.Store, root ssa.Value, path []int) bool {
	storePath, ok := fieldStorePath(store, root)
	return ok && len(storePath) <= len(path) && slices.Equal(storePath, path[:len(storePath)])
}

// fieldStores returns, in program order, the stores that write the field that
// path selects from root. With a nil load it returns all of them. Otherwise it
// returns only the stores that reach load (#182): on some CFG path from the
// store to load, no other store writes the field.
//
//	t0 = &r.db
//	t1 = *t0                 // load: the store below does not reach it
//	t2 = Where(t1, "tenant")
//	t3 = &r.db
//	*t3 = t2
//	t4 = &r.db
//	t5 = *t4                 // load: reached by *t3 = t2 only
//
// Without this, the load on the right of `r.db = r.db.Where(...)` resolved to
// the value being stored, so the builder call counted as a branch of its own
// result. Only stores to the same (root, field path) are seen. A write through
// another pointer or inside a callee neither reaches the load nor replaces a
// store, as before.
func fieldStores(fn *ssa.Function, root ssa.Value, path []int, load ssa.Instruction) []*ssa.Store {
	stores, _ := reachingFieldStores(fn, root, path, load)
	return stores
}

// reachingFieldStores is fieldStores that also reports whether the field can
// reach load unwritten: on some CFG path to load, no store writes it. A nil
// load is open, since it stands for a read that may run before any store.
//
// When root is a call, which defines the address, the walk stops at the call:
// a store above it on a loop path wrote the struct of an earlier iteration.
func reachingFieldStores(fn *ssa.Function, root ssa.Value, path []int, load ssa.Instruction) (stores []*ssa.Store, open bool) {
	if load == nil {
		var all []*ssa.Store
		for _, block := range fn.Blocks {
			for _, instr := range block.Instrs {
				if store, ok := instr.(*ssa.Store); ok && writesFieldPath(store, root, path) {
					all = append(all, store)
				}
			}
		}
		return all, true
	}

	// lastIn returns the last store in instrs that writes the field.
	lastIn := func(instrs []ssa.Instruction) *ssa.Store {
		for _, instr := range slices.Backward(instrs) {
			if store, ok := instr.(*ssa.Store); ok && writesFieldPath(store, root, path) {
				return store
			}
		}
		return nil
	}

	// def is the call that defines root, and the walk ends there.
	def, _ := root.(*ssa.Call)
	// afterDef returns the instructions of block that can write root's field
	// before control leaves it, and whether def is among them.
	afterDef := func(instrs []ssa.Instruction) ([]ssa.Instruction, bool) {
		if def == nil {
			return instrs, false
		}
		if i := slices.Index(instrs, ssa.Instruction(def)); i >= 0 {
			return instrs[i+1:], true
		}
		return instrs, false
	}

	start := load.Block()
	prefix, defined := afterDef(start.Instrs[:slices.Index(start.Instrs, load)])
	if store := lastIn(prefix); store != nil {
		return []*ssa.Store{store}, false
	}
	if defined || len(start.Preds) == 0 {
		return nil, true
	}

	// Walk the CFG backward. A block that writes the field ends the walk on
	// that path with its last such store. The load's own block is scanned in
	// full when a loop leads back to it, since its later stores then run
	// before the load on the next iteration.
	var found []*ssa.Store
	visitedBlocks := make(map[*ssa.BasicBlock]bool)
	work := slices.Clone(start.Preds)
	for len(work) > 0 {
		block := work[len(work)-1]
		work = work[:len(work)-1]
		if visitedBlocks[block] {
			continue
		}
		visitedBlocks[block] = true
		instrs, defined := afterDef(block.Instrs)
		if store := lastIn(instrs); store != nil {
			found = append(found, store)
			continue
		}
		if defined || len(block.Preds) == 0 {
			open = true
			continue
		}
		work = append(work, block.Preds...)
	}
	// Each block contributes at most one store, so block order is program
	// order.
	slices.SortFunc(found, func(a, b *ssa.Store) int { return a.Block().Index - b.Block().Index })
	return found, open
}

// globalStoredValues returns, in program order, the values stored into the
// package-level variable g that reach load (#183). Only stores in load's own
// function are seen, matched as fieldStores matches a field. A load that no
// such store reaches has no stored value, so it is immutable: a global the
// function only reads usually holds a gorm.Open result, and treating every
// read of it as mutable would report each second use of a shared handle.
//
//	DB = DB.Where("tenant")  // the load on the right sees no store
//	DB.Find(nil)             // sees the store above
func globalStoredValues(g *ssa.Global, load ssa.Instruction) []ssa.Value {
	var vals []ssa.Value
	for _, store := range fieldStores(load.Parent(), g, nil, load) {
		vals = append(vals, store.Val)
	}
	return vals
}

// traceIIFEReturns traces through an immediately invoked function expression.
//
// IIFE pattern:
//
//	_ = func() *gorm.DB {
//	    return q.Where("y")   // ← trace this return value
//	}().Find(nil)
//
// This allows treating the entire IIFE chain as a single branch from q,
// rather than two separate branches (IIFE internal + Find external).
//
// We clone the visited map to avoid polluting the parent's tracking state.
func (t *RootTracer) traceIIFEReturns(fn *ssa.Function, visited map[ssa.Value]bool, loopInfo *cfg.LoopInfo) ssa.Value {
	if fn.Signature == nil {
		return nil
	}
	results := fn.Signature.Results()
	if results == nil || results.Len() == 0 || !typeutil.IsGormDB(results.At(0).Type()) {
		return nil
	}

	// Collect ALL roots from all return statements
	var allRoots []ssa.Value
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			ret, ok := instr.(*ssa.Return)
			if !ok || len(ret.Results) == 0 {
				continue
			}
			// Clone visited map to isolate closure's tracing state
			retVisited := cloneVisited(visited)
			if root := t.trace(ret.Results[0], retVisited, loopInfo); root != nil {
				allRoots = append(allRoots, root)
			}
		}
	}

	if len(allRoots) == 0 {
		return nil
	}

	// If all roots are the same, return that root
	firstRoot := allRoots[0]
	allSame := true
	for _, root := range allRoots[1:] {
		if root != firstRoot {
			allSame = false
			break
		}
	}

	if allSame {
		return firstRoot
	}

	// Multiple different roots - return nil to signal caller should use call itself
	// This happens with conditional returns like:
	//   func() *gorm.DB { if cond { return q1 } else { return q2 } }()
	return nil
}

// traceAllIIFEReturns is the multi-root version of traceIIFEReturns.
// It collects roots from ALL return statements in the closure.
func (t *RootTracer) traceAllIIFEReturns(fn *ssa.Function, visited map[ssa.Value]bool, loopInfo *cfg.LoopInfo) []ssa.Value {
	if fn.Signature == nil {
		return nil
	}
	results := fn.Signature.Results()
	if results == nil || results.Len() == 0 || !typeutil.IsGormDB(results.At(0).Type()) {
		return nil
	}

	var allRoots []ssa.Value
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			ret, ok := instr.(*ssa.Return)
			if !ok || len(ret.Results) == 0 {
				continue
			}
			retVisited := cloneVisited(visited)
			roots := t.traceAll(ret.Results[0], retVisited, loopInfo)
			allRoots = append(allRoots, roots...)
		}
	}
	return allRoots
}

// traceAll finds ALL possible mutable roots, handling Phi nodes specially.
//
// Unlike trace() which returns the first root found, this collects all roots.
// This is essential for Phi nodes where different branches may have different roots:
//
//	var q *gorm.DB
//	if cond {
//	    q = db.Where("a")  // root A
//	} else {
//	    q = db.Where("b")  // root B
//	}
//	q.Find(nil)            // Phi has edges from both branches
//	                       // Need to check pollution of BOTH roots
func (t *RootTracer) traceAll(v ssa.Value, visited map[ssa.Value]bool, loopInfo *cfg.LoopInfo) []ssa.Value {
	if v == nil || visited[v] {
		return nil
	}
	visited[v] = true

	// A *gorm.DB parameter is a mutable root by default (Phase 1b, #61), except
	// the exempt cases; keep this consistent with trace()/FindMutableRoot so
	// Phi-based pollution checks see it too.
	if p, ok := v.(*ssa.Parameter); ok && t.isMutableParam(p) {
		return []ssa.Value{p}
	}

	switch val := v.(type) {
	case *ssa.Phi:
		// =====================================================================
		// PHI NODE: Collect All Possible Roots for Pollution Checking
		// =====================================================================
		//
		// Unlike FindMutableRoot() which returns the PRIMARY root for a value,
		// FindAllMutableRoots() must return ALL possible roots that could
		// contribute pollution. This is critical for conditional code where
		// different branches may have different pollution states.
		//
		// ## Why We Need All Roots
		//
		// Consider:
		//   q1 := db.Where("a")
		//   q2 := db.Where("b")
		//   q1.Find(nil)  // Pollute q1
		//   var q *gorm.DB
		//   if cond {
		//       q = q1  // Branch 1: polluted
		//   } else {
		//       q = q2  // Branch 2: clean
		//   }
		//   q.Count(nil)  // ← Need to check BOTH q1 and q2
		//
		// The phi after the if has edges [q1, q2]. We must check if ANY
		// of these roots is polluted, not just pick one.
		//
		// ## Special Case: Loop Variable Swaps
		//
		// However, loop variable swaps need special handling:
		//
		//   q1 := db.Where("q1")  // Clean
		//   q2 := db.Where("q2")  // Clean
		//   for _, item := range items {
		//       if item%2 == 0 {
		//           q1, q2 = q2, q1  // Swap
		//       }
		//       q1 = q1.Where(...)  // Assignment
		//   }
		//
		// SSA creates swap-phi nodes inside the loop. These should NOT
		// mix pollution between q1 and q2 during iteration, because they're
		// just swapping roles, not creating cross-contamination.
		//
		// BUT we still need to check if q1 or q2 were polluted BEFORE
		// entering the loop, because that pollution would propagate.
		//
		// Solution: For loop variable swaps, return:
		//   - INITIAL edges of loop-header phis (pre-loop values)
		//   - The swap-phi itself (as an independent entity)
		//
		// This allows pollution checking to:
		//   - Detect pre-loop pollution: check if t1 or t3 is polluted
		//   - Treat swap-phi independently: t16 and t17 have separate pollution
		//
		if loopHeaderPhis := isLoopVariableSwap(val, loopInfo); loopHeaderPhis != nil {
			// Loop variable swap detected:
			// Return initial edges (to check pre-loop pollution) + swap-phi itself
			return t.collectInitialEdgeRoots(loopHeaderPhis, val, visited, loopInfo)
		}

		// =====================================================================
		// REGULAR PHI: Collect Roots from All Edges
		// =====================================================================
		//
		// For non-swap phis, or swaps outside loop context:
		//   - Recursively trace ALL edges
		//   - Collect roots from each edge
		//   - Return union of all roots found
		//
		// This handles:
		//   - Conditional assignment: if { q = a } else { q = b }
		//   - Reassignment in one branch: if { q = q.Where(...) }
		//   - Conditional swaps outside loops
		//
		return t.traceAllPhiEdges(val, visited, loopInfo)

	case *ssa.UnOp:
		if val.Op == token.MUL {
			return t.traceAllPointerLoads(val, visited, loopInfo)
		}
		return t.traceAll(val.X, visited, loopInfo)

	case *ssa.Alloc:
		return t.traceAllAllocStores(val, visited, loopInfo)

	case *ssa.FreeVar:
		return t.traceAllFreeVar(val, visited, loopInfo)

	case *ssa.Field:
		var roots []ssa.Value
		for _, stored := range fieldValueStoredValues(val) {
			roots = append(roots, t.traceAll(stored, visited, loopInfo)...)
		}
		return roots

	case *ssa.Call:
		// The result of a pure function may be any argument it returns (#188).
		// Like a Session result, a call that trace() finds immutable may stay
		// in this list: nothing branches it, so it is never polluted.
		var argRoots []ssa.Value
		for _, arg := range t.pureReturnedArgs(val) {
			argRoots = append(argRoots, t.traceAll(arg, cloneVisited(visited), loopInfo)...)
		}
		if len(argRoots) > 0 {
			return argRoots
		}
		// Handle closure calls (IIFE) - collect ALL roots from all returns. A
		// call through a method value is a call of its own (see traceCall).
		if mc, ok := val.Call.Value.(*ssa.MakeClosure); ok && !isMethodValueCall(val) {
			if closureFn, ok := mc.Fn.(*ssa.Function); ok {
				if roots := t.traceAllIIFEReturns(closureFn, visited, loopInfo); len(roots) > 0 {
					// If closure result is stored, treat call itself as root
					if isClosureResultStored(val) {
						return []ssa.Value{val}
					}
					return roots
				}
			}
		}
		// Non-closure call - treat as potential root
		if typeutil.IsGormDB(val.Type()) {
			return []ssa.Value{val}
		}
		return nil

	default:
		// For non-special cases (ChangeType, Extract, etc.), delegate to single-root trace.
		// Clone visited and remove v to allow trace() to process it.
		// This is safe because trace() doesn't call back to traceAll(), so no cycle risk.
		freshVisited := cloneVisited(visited)
		delete(freshVisited, v)
		if root := t.trace(v, freshVisited, loopInfo); root != nil {
			return []ssa.Value{root}
		}
		return nil
	}
}

func (t *RootTracer) traceAllPointerLoads(load *ssa.UnOp, visited map[ssa.Value]bool, loopInfo *cfg.LoopInfo) []ssa.Value {
	switch p := load.X.(type) {
	case *ssa.Alloc:
		return t.traceAllAllocStores(p, visited, loopInfo)
	case *ssa.FieldAddr:
		return t.traceAllFieldStores(p, load, visited, loopInfo)
	case *ssa.Global:
		var roots []ssa.Value
		for _, v := range globalStoredValues(p, load) {
			roots = append(roots, t.traceAll(v, visited, loopInfo)...)
		}
		return roots
	case *ssa.Phi:
		// Check for loop variable swap pattern
		if loopHeaderPhis := isLoopVariableSwap(p, loopInfo); loopHeaderPhis != nil {
			return t.collectInitialEdgeRoots(loopHeaderPhis, p, visited, loopInfo)
		}

		// Regular Phi - traverse all edges
		return t.traceAllPhiEdges(p, visited, loopInfo)
	default:
		return t.traceAll(p, visited, loopInfo)
	}
}

// traceAllPhiEdges collects all mutable roots reachable through a Phi node's
// incoming edges, skipping nil constants and already-visited values. Shared by
// the Phi cases of traceAll and traceAllPointerLoads.
func (t *RootTracer) traceAllPhiEdges(phi *ssa.Phi, visited map[ssa.Value]bool, loopInfo *cfg.LoopInfo) []ssa.Value {
	var roots []ssa.Value
	for _, edge := range phi.Edges {
		if isNilConst(edge) || visited[edge] {
			continue
		}
		roots = append(roots, t.traceAll(edge, visited, loopInfo)...)
	}
	return roots
}

func (t *RootTracer) traceAllAllocStores(alloc *ssa.Alloc, visited map[ssa.Value]bool, loopInfo *cfg.LoopInfo) []ssa.Value {
	var roots []ssa.Value
	for _, v := range AllocStoredValues(alloc) {
		roots = append(roots, t.traceAll(v, visited, loopInfo)...)
	}
	return roots
}

// traceAllFieldStores finds ALL possible roots from stores to a struct field.
//
// Unlike traceFieldStore which returns the first Store found, this collects
// all roots from ALL Store instructions that write the same field and reach the
// load. This is needed for conditional code where different branches may store
// different values:
//
//	if cond {
//	    h.db = q1  // Store #1
//	} else {
//	    h.db = q2  // Store #2
//	}
//	h.db.Find(nil)  // Need to check BOTH q1 and q2
func (t *RootTracer) traceAllFieldStores(fa *ssa.FieldAddr, load *ssa.UnOp, visited map[ssa.Value]bool, loopInfo *cfg.LoopInfo) []ssa.Value {
	var roots []ssa.Value
	for _, v := range fieldStoredValues(fa, load) {
		roots = append(roots, t.traceAll(v, visited, loopInfo)...)
	}
	return roots
}

// traceAllFreeVar finds ALL possible roots from a captured closure variable.
//
// Unlike traceFreeVar which calls single-root trace on the binding, this calls
// traceAll to get all possible roots. This is needed when the binding is a Phi
// (from conditional code):
//
//	var q *gorm.DB
//	if cond {
//	    q = q1  // Branch 1
//	} else {
//	    q = q2  // Branch 2
//	}
//	fn := func() { q.Find(nil) }  // FreeVar captures Phi of [q1, q2]
//	fn()  // Need to check BOTH q1 and q2
func (t *RootTracer) traceAllFreeVar(fv *ssa.FreeVar, visited map[ssa.Value]bool, loopInfo *cfg.LoopInfo) []ssa.Value {
	if binding := FreeVarBinding(fv); binding != nil {
		return t.traceAll(binding, visited, loopInfo)
	}
	return nil
}

// isMutableParam reports whether p should be treated as a mutable root.
//
// Under Phase 1b (#61) a *gorm.DB parameter is a mutable root by default: a
// caller may pass a mid-chain (clone==0) value, and branching it inside the
// callee interferes exactly as branching a local would. Exceptions, treated as
// immutable:
//   - the enclosing function is annotated //gormreuse:immutable-param (the caller
//     is responsible for passing a safe value; verified caller-side elsewhere)
//   - p is the tx parameter of a gorm Transaction/FindInBatches callback — a
//     fresh, forkable (clone>0) handle, so reuse inside the callback is safe
//     (#60 SC103, #62). Connection is NOT exempt: it passes a clone==0 handle
//     (#173)
//
// A Scopes/Preload callback parameter receives a clone==0 value and is ALWAYS
// mutable; it cannot be exempted by //gormreuse:immutable-param.
func (t *RootTracer) isMutableParam(p *ssa.Parameter) bool {
	if !typeutil.IsGormDB(p.Type()) {
		return false
	}
	fn := p.Parent()
	if t.scopesCallbacks[fn] {
		return true
	}
	if t.immutableCallbacks[fn] {
		return false
	}
	if t.immutableParamFuncs != nil && t.immutableParamFuncs.Contains(fn) {
		return false
	}
	return true
}

// isImmutableSource checks if a value is an immutable source (no mutable root).
//
// Immutable sources:
//   - Parameter: reached here only for the EXEMPT parameters — a mutable
//     *gorm.DB parameter is handled earlier by isMutableParam and never reaches
//     this point (see trace/traceAll). Non-gorm parameters and exempt gorm
//     parameters (immutable-param / Transaction callback) are immutable.
//   - Const: constant values (especially nil)
//   - Builtin pure function call: returns immutable *gorm.DB (e.g., Session())
//   - User-defined immutable-return function: marked with //gormreuse:immutable-return
//
// Note: User-defined pure functions (without immutable-return) are NOT immutable sources -
// they may return mutable values.
func (t *RootTracer) isImmutableSource(v ssa.Value) bool {
	switch val := v.(type) {
	case *ssa.Parameter:
		// Function parameters are immutable sources
		// (caller's responsibility to ensure safety)
		return true
	case *ssa.Const:
		// Constants (including nil) are immutable
		return true
	case *ssa.Call:
		// Builtin or //gormreuse:immutable-return call yields an immutable value,
		// except a Session call that may set Initialized: it returns a clone==0
		// handle, so the call is a mutable root like a chain method (#174).
		callee, _, _ := t.calledFunc(val)
		if t.isImmutableReturningBuiltin(callee) {
			return !t.builtinCallEffect(val).ReturnsMutable
		}
		return t.returnsImmutable(callee)
	default:
		return false
	}
}

// IsImmutableCall reports whether v is a call whose result is immutable, by
// the rule the tracer uses for a root: the call itself, the last call of its
// chain, is an immutable-returning gorm builtin or a
// //gormreuse:immutable-return function. So db.Session(s).Where("x") is
// mutable, and so is a Session that may set Initialized.
func (t *RootTracer) IsImmutableCall(v ssa.Value) bool {
	call, ok := v.(*ssa.Call)
	return ok && t.isImmutableSource(call)
}

// FindAliasedRoots returns the mutable roots whose Statement v may read later:
// through a Session or Debug copy (#174), or as a read-only argument of a call
// in v's chain, such as a subquery (#185).
//
// On a clone==0 receiver, Session(&gorm.Session{}) and Debug() return a handle
// that shares the receiver's Statement. Each use of the copy forks its own
// Statement, so it does not change the receiver. But it reads the receiver's
// Statement as it is at that moment. So each use of the copy is a pure use of
// the receiver's root at the use's own position:
//
//	q := db.Where("tenant")       // root
//	countQ := q.Session(&gorm.Session{})
//	q.Where("active").Find(nil)   // changes q's Statement in place
//	countQ.Count(nil)             // reads it: pure use of q's root, after a branch
//
// Copies of copies are followed, so q.Session(...).Debug() still reaches q's
// root. A copy reached through a Phi, a variable, or a closure capture is found
// the same way as a mutable root.
//
// A subquery is kept in the Statement of the chain it is passed to, and GORM
// builds it from the argument when the chain runs. So each use of the chain
// reads the argument's root too:
//
//	x := db.Where("id IN (?)", sub)
//	sub.Where("age > 1").Find(nil) // changes sub's Statement in place
//	x.Find(nil)                    // builds the subquery from it
//
// Every call in v's chain is searched for such arguments, through Session and
// WithContext too, since a cloned Statement still holds the argument. A
// Session copy below another chain call is not an alias: that call forked its
// own Statement.
func (t *RootTracer) FindAliasedRoots(v ssa.Value, loopInfo *cfg.LoopInfo) []AliasedRoot {
	var roots []AliasedRoot
	t.collectAliasedRoots(v, loopInfo, true, make(map[aliasVisit]bool), &roots)
	return roots
}

// AliasedRoot is a mutable root that a value reads, and the call it reads it
// through: the Session or Debug call that took the copy, or, when Argument is
// set, the call that took the root as a read-only argument.
type AliasedRoot struct {
	Root     ssa.Value
	Copy     *ssa.Call
	Argument bool
}

// aliasVisit is a call visited by collectAliasedRoots, and whether Session
// and Debug copies still counted there.
type aliasVisit struct {
	call   *ssa.Call
	copies bool
}

// collectAliasedRoots adds to roots the mutable roots that v reaches through
// Statement-sharing builtin calls, while copies is set, and through read-only
// arguments of the gorm calls in its chain. seen stops cycles through loops.
func (t *RootTracer) collectAliasedRoots(v ssa.Value, loopInfo *cfg.LoopInfo, copies bool, seen map[aliasVisit]bool, roots *[]AliasedRoot) {
	for _, candidate := range t.FindAllMutableRoots(v, loopInfo) {
		call, ok := candidate.(*ssa.Call)
		if !ok || seen[aliasVisit{call, copies}] {
			continue
		}
		// A call through a method value (w := q.Where; w(x, sub)) is read as
		// the method call it stands for (#178).
		callee, recv, args := t.calledFunc(call)
		if callee == nil || !isGormDBMethod(callee) || recv == nil {
			continue
		}
		seen[aliasVisit{call, copies}] = true
		isCopy := copies && t.sharesReceiverStatement(call)
		if isCopy {
			if r := t.FindMutableRoot(recv, loopInfo); r != nil {
				*roots = append(*roots, AliasedRoot{Root: r, Copy: call})
			}
		}
		for _, arg := range pollutionsource.ReadOnlyGormArgs(callee, args) {
			if r := t.FindMutableRoot(arg, loopInfo); r != nil {
				*roots = append(*roots, AliasedRoot{Root: r, Copy: call, Argument: true})
			}
			t.collectAliasedRoots(arg, loopInfo, true, seen, roots)
		}
		t.collectAliasedRoots(recv, loopInfo, isCopy, seen, roots)
	}
}

// sharesReceiverStatement reports whether call is a gorm Session or Debug
// method call whose result may share its receiver's Statement.
func (t *RootTracer) sharesReceiverStatement(call *ssa.Call) bool {
	callee, recv, _ := t.calledFunc(call)
	if !t.isImmutableReturningBuiltin(callee) || recv == nil {
		return false
	}
	return t.builtinCallEffect(call).SharesStatement
}

// builtinCallEffect returns the effect of a call to a gorm builtin, passing
// the *gorm.Session argument of a Session method call. The caller must make
// sure the callee is a gorm builtin.
func (t *RootTracer) builtinCallEffect(call *ssa.Call) typeutil.BuiltinEffect {
	callee, _, args := t.calledFunc(call)
	var sessionConfig ssa.Value
	if len(args) > 0 {
		sessionConfig = args[0]
	}
	return typeutil.ImmutableBuiltinEffect(callee.Name(), sessionConfig)
}

// calledFunc returns the function call runs, its receiver, and its other
// arguments. For a static method call r.m(x) that is m, r and [x]; for a
// static function call f(x) it is f, nil and [x].
//
// A call through a method value (f := r.m; f(x)) is resolved the same way as
// r.m(x): SSA builds f as MakeClosure(m$bound, [r]), so the receiver is the
// closure's binding and the method is the one the synthetic wrapper stands
// for (directive.DeclaredFunc). Without this, the wrapper's own m call would
// be the root of every call through f (#178). A call through a package-level
// variable that holds one function is resolved to it (see FuncVars, #183). The
// callee is nil for any other dynamic call.
func (t *RootTracer) calledFunc(call *ssa.Call) (callee *ssa.Function, recv ssa.Value, args []ssa.Value) {
	if isMethodValueCall(call) {
		mc := call.Call.Value.(*ssa.MakeClosure)
		return directive.DeclaredFunc(mc.Fn.(*ssa.Function)), mc.Bindings[0], call.Call.Args
	}
	callee = t.Callee(&call.Call)
	if callee != nil && callee.Signature.Recv() != nil && len(call.Call.Args) > 0 {
		return callee, call.Call.Args[0], call.Call.Args[1:]
	}
	return callee, nil, call.Call.Args
}

// isMethodValueCall reports whether call calls a method value directly: its
// callee is a MakeClosure over a synthetic "m$bound" wrapper.
func isMethodValueCall(call *ssa.Call) bool {
	mc, ok := call.Call.Value.(*ssa.MakeClosure)
	if !ok || len(mc.Bindings) == 0 {
		return false
	}
	fn, ok := mc.Fn.(*ssa.Function)
	return ok && fn.Synthetic != "" && strings.HasSuffix(fn.Name(), "$bound")
}

// pureReturnedArgs returns the arguments of a call to a trusted
// //gormreuse:pure function that its *gorm.DB result may be (#188), or nil.
//
// The result is traced to these arguments first, so `r := pReturn(q)` makes r
// a branch of q's root. When none of them has a mutable root (they are all
// immutable, or the tracer cannot follow them), the result is immutable only
// if the function returns nothing else (returnsOnlyParams). Otherwise the call
// stays its own root: the function may also return a fresh mutable chain.
//
// A tuple result such as (*gorm.DB, error) follows the same rule (#176): its
// *gorm.DB elements share the call as their root, so they are traced to the
// returned arguments together.
func (t *RootTracer) pureReturnedArgs(call *ssa.Call) []ssa.Value {
	if !typeutil.HasGormDBResult(call.Type()) {
		return nil
	}
	// A call through a method value passes its receiver as a binding, not
	// as an argument; put it back so a pure method returning it is followed.
	callee, recv, args := t.calledFunc(call)
	if recv != nil {
		args = append([]ssa.Value{recv}, args...)
	}
	args = t.pure.ReturnedArgs(callee, args)
	if len(args) == 0 || !t.IsPureFunction(callee) {
		return nil
	}
	return args
}

// ReturnsOnlyParams reports whether every mutable root of the values fn
// returns is one of fn's own parameters. The analyzer exports it for the pure
// functions that may return an argument (#195).
func (t *RootTracer) ReturnsOnlyParams(fn *ssa.Function) bool {
	return t.returnsOnlyParams(fn)
}

// returnsOnlyParams reports whether every mutable root of the values fn
// returns is one of fn's own parameters, so fn returns an immutable value when
// its arguments are immutable. `if c { return db.Session(s).Where("x") };
// return db` also returns a fresh chain, so it does not.
func (t *RootTracer) returnsOnlyParams(fn *ssa.Function) bool {
	// An instance or a bound method wrapper runs the declared body.
	fn = directive.DeclaredFunc(fn)
	if only, ok := t.onlyParams[fn]; ok {
		return only
	}
	// The body of a function of another package is not built, so its package
	// decided (#195).
	if fn.Blocks == nil {
		return t.pure != nil && t.pure.Imported.Of(fn).ReturnsOnlyParams
	}
	// A recursive call may return anything until fn is decided.
	t.onlyParams[fn] = false
	only := true
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			ret, ok := instr.(*ssa.Return)
			if !ok {
				continue
			}
			// Every *gorm.DB result counts, in any position of a tuple (#176).
			for _, res := range ret.Results {
				if !typeutil.IsGormDB(res.Type()) {
					continue
				}
				for _, root := range t.traceAll(res, make(map[ssa.Value]bool), nil) {
					if p, isParam := root.(*ssa.Parameter); !isParam || p.Parent() != fn {
						only = false
					}
				}
			}
		}
	}
	t.onlyParams[fn] = only
	return only
}

// returnsImmutable reports whether a call to callee yields an immutable
// *gorm.DB result — either a gorm builtin (Session, WithContext, Debug, Open,
// Begin, Transaction) or a function marked //gormreuse:immutable-return. Such
// results are not mutable roots and can be reused freely. callee may be nil.
func (t *RootTracer) returnsImmutable(callee *ssa.Function) bool {
	if t.isImmutableReturningBuiltin(callee) {
		return true
	}
	return t.immutableReturnFuncs != nil && t.immutableReturnFuncs.Contains(callee)
}

// =============================================================================
// Helper Functions
// =============================================================================

// ClosureCapturesGormDB checks if a closure captures *gorm.DB values.
//
// Used to determine if a closure needs recursive analysis. A closure
// that doesn't capture *gorm.DB can be skipped for efficiency.
//
// Looks through pointer chains (*gorm.DB, **gorm.DB, ...) and struct fields, so
// a closure capturing a struct that holds a *gorm.DB is analyzed too (#181).
func ClosureCapturesGormDB(mc *ssa.MakeClosure) bool {
	return slices.ContainsFunc(mc.Bindings, func(binding ssa.Value) bool {
		return holdsGormDB(binding.Type(), make(map[types.Type]bool))
	})
}

// CollectScopesCallbacks returns the set of functions passed as callbacks to
// gorm's Scopes / Preload across all given functions and their nested closures.
//
// Such callbacks receive a mid-chain (clone==0) *gorm.DB at runtime, so their
// *gorm.DB parameter is a mutable root and reuse inside them interferes (#60).
// This is deliberately narrow — only functions actually handed to Scopes/Preload
// qualify, NOT every func(*gorm.DB) *gorm.DB (that broader treatment is Phase
// 1b, not 1a).
//
// Scopes(funcs ...func(*DB) *DB) and Preload(query, args ...interface{}) are
// variadic, so the callbacks are packed into a varargs array (the last call
// argument is a slice of it); Preload additionally boxes them in interface{}.
func CollectScopesCallbacks(funcs []*ssa.Function) map[*ssa.Function]bool {
	return collectCallbacksPassedTo(funcs, isScopesOrPreloadMethod)
}

// CollectScopesOnlyCallbacks is CollectScopesCallbacks without Preload: the
// functions passed to gorm's Scopes, resolved the same way. The temporary
// Session-in-Scopes warning (go-gorm/gorm#7592) uses it, so it covers the same
// callbacks as reuse detection (#196).
func CollectScopesOnlyCallbacks(funcs []*ssa.Function) map[*ssa.Function]bool {
	return collectCallbacksPassedTo(funcs, isScopesMethod)
}

// CollectImmutableCallbacks returns the set of function literals passed as the
// callback to gorm's Transaction or FindInBatches across all given functions and
// their nested closures.
//
// Each of these methods hands the callback a FRESH, forkable (clone>0) handle
// (Transaction via Begin, FindInBatches via Session(&Session{NewDB: true})), so — unlike an ordinary Phase
// 1b parameter — the callback's *gorm.DB parameter is NOT a mutable root and
// reuse inside the callback is safe (verified against gorm's clone semantics; see
// the epic's pivotal finding). It must be exempted from the mutable-by-default
// treatment (#60 SC103, #62).
func CollectImmutableCallbacks(funcs []*ssa.Function) map[*ssa.Function]bool {
	set := make(map[*ssa.Function]bool)
	walkCalls(funcs, func(call *ssa.Call) {
		collectImmutableCallbacksFromCall(call, set)
	})
	return set
}

// CollectImmutableInputCallbacks adds, to into, every closure passed as the
// callback argument declared by a //gormreuse:immutable-input(name) function.
// Such a callback receives an immutable *gorm.DB (the declaring function promised
// to hand it one), so — like a Transaction callback — its *gorm.DB parameter is
// exempt from the Phase 1b mutable-by-default treatment (#62 case 2.2).
func CollectImmutableInputCallbacks(funcs []*ssa.Function, set *directive.ImmutableInputSet, into map[*ssa.Function]bool) {
	if set == nil || into == nil {
		return
	}
	walkCalls(funcs, func(call *ssa.Call) {
		callee := call.Call.StaticCallee()
		if callee == nil {
			return
		}
		args := call.Call.Args
		for _, idx := range immutableInputArgIndices(callee, set, len(args)) {
			if fn := callbackFuncValue(args[idx]); fn != nil {
				into[fn] = true
			}
		}
	})
}

// immutableInputArgIndices returns the argument indices of a call to callee
// (with nargs arguments) that are its //gormreuse:immutable-input callbacks.
func immutableInputArgIndices(callee *ssa.Function, set *directive.ImmutableInputSet, nargs int) []int {
	// A method call carries its receiver as Args[0]; ParamIdx counts from the
	// first non-receiver parameter, so shift when the callee has a receiver.
	shift := 0
	if callee.Signature != nil && callee.Signature.Recv() != nil {
		shift = 1
	}
	var idxs []int
	for _, cb := range set.Callbacks(callee) {
		if idx := cb.ParamIdx + shift; idx >= 0 && idx < nargs {
			idxs = append(idxs, idx)
		}
	}
	return idxs
}

// DropReachableCallbacks removes from set every callback whose *gorm.DB
// parameter can receive something other than the fresh value of an immutable
// callback position (#191). set is keyed by the declared function, so without
// this a named function passed to Transaction once would be exempt for every
// caller, including a direct call with a mid-chain value.
//
// A function stays in set only when every reference to it in funcs (and in the
// package initializers) is an argument in an immutable callback position: a
// Transaction/FindInBatches callback or an immutable-input(name) argument,
// directly or as a closure or method value. It is dropped when:
//   - any other reference exists: a direct call (also under go and defer, and
//     recursion), a method expression, a store, a Phi, a capture, a return
//   - it is exported, so another package may call it directly (#195)
//   - it is a method and an interface call, method value or method expression
//     in the package names a method of the same name, which may dispatch to it
func DropReachableCallbacks(funcs []*ssa.Function, inputs *directive.ImmutableInputSet, set map[*ssa.Function]bool) {
	for fn := range set {
		if obj := fn.Object(); obj != nil && obj.Exported() {
			delete(set, fn)
		}
	}
	if len(set) == 0 {
		return
	}
	scan := slices.Clone(funcs)
	seenPkg := make(map[*ssa.Package]bool)
	for _, fn := range funcs {
		if fn.Pkg != nil && !seenPkg[fn.Pkg] {
			seenPkg[fn.Pkg] = true
			if init := fn.Pkg.Func("init"); init != nil {
				scan = append(scan, init)
			}
		}
	}
	var invoked []string
	walkInstrs(scan, func(instr ssa.Instruction) {
		if call, ok := instr.(ssa.CallInstruction); ok && call.Common().IsInvoke() {
			invoked = append(invoked, call.Common().Method.Name())
		}
		for _, op := range instr.Operands(nil) {
			ref, ok := (*op).(*ssa.Function)
			if !ok {
				continue
			}
			if name, ok := interfaceMethodName(ref); ok {
				invoked = append(invoked, name)
			}
			if fn := referencedFunc(ref); set[fn] && !isImmutableCallbackRef(instr, ref, inputs) {
				delete(set, fn)
			}
		}
	})
	for fn := range set {
		if fn.Signature.Recv() != nil && slices.Contains(invoked, fn.Name()) {
			delete(set, fn)
		}
	}
}

// interfaceMethodName returns the method name of a wrapper that calls an
// interface method: a method value (i.m) or method expression (I.m) of an
// interface. Its dispatch happens inside the wrapper, so no invoke call in the
// package shows it.
func interfaceMethodName(fn *ssa.Function) (string, bool) {
	obj, ok := fn.Object().(*types.Func)
	if !ok {
		return "", false
	}
	recv := obj.Signature().Recv()
	if recv == nil || !types.IsInterface(recv.Type()) {
		return "", false
	}
	return obj.Name(), true
}

// referencedFunc maps a function value to the declared function whose body it
// runs: a generic instance or bound method wrapper as DeclaredFunc does, and a
// method expression thunk (T.m) to its method.
func referencedFunc(fn *ssa.Function) *ssa.Function {
	if fn.Synthetic != "" && strings.HasSuffix(fn.Name(), "$thunk") {
		if obj, ok := fn.Object().(*types.Func); ok {
			if method := fn.Prog.FuncValue(obj.Origin()); method != nil {
				return method
			}
		}
	}
	return directive.DeclaredFunc(fn)
}

// isImmutableCallbackRef reports whether instr uses v only to hand it to an
// immutable callback position: v is a call argument in such a position, or it
// is the function of a closure (a capturing literal or a bound method value)
// that is itself used only that way. Any other use, such as an interface
// conversion, a store or a Phi, is not: Transaction and immutable-input take a
// func-typed argument, so such a value never reaches one directly.
func isImmutableCallbackRef(instr ssa.Instruction, v ssa.Value, inputs *directive.ImmutableInputSet) bool {
	switch x := instr.(type) {
	case *ssa.MakeClosure:
		return x.Fn == v && usedOnlyAsImmutableCallback(x, inputs)
	case ssa.CallInstruction:
		common := x.Common()
		if common.Value == v {
			return false
		}
		positions := immutableCallbackArgIndices(common, inputs)
		for i, arg := range common.Args {
			if arg == v && !slices.Contains(positions, i) {
				return false
			}
		}
		return true
	}
	return false
}

// usedOnlyAsImmutableCallback reports whether every use of the closure c hands
// it to an immutable callback position.
func usedOnlyAsImmutableCallback(c *ssa.MakeClosure, inputs *directive.ImmutableInputSet) bool {
	for _, ref := range *c.Referrers() {
		if !isImmutableCallbackRef(ref, c, inputs) {
			return false
		}
	}
	return true
}

// immutableCallbackArgIndices returns the argument indices of a call whose
// function argument receives a fresh *gorm.DB: every function argument with a
// *gorm.DB parameter of Transaction/FindInBatches (as
// collectImmutableCallbacksFromCall collects them), and the immutable-input
// callbacks of a user function.
func immutableCallbackArgIndices(common *ssa.CallCommon, inputs *directive.ImmutableInputSet) []int {
	callee := common.StaticCallee()
	if callee == nil {
		return nil
	}
	if !IsImmutableCallbackMethod(callee) {
		return immutableInputArgIndices(callee, inputs, len(common.Args))
	}
	var idxs []int
	for i, arg := range common.Args {
		if cb := callbackFuncValue(arg); cb != nil && cb.Signature != nil && directive.HasGormDBParameter(cb.Signature) {
			idxs = append(idxs, i)
		}
	}
	return idxs
}

// isNilConst checks if a value is a nil constant.
func isNilConst(v ssa.Value) bool {
	c, ok := v.(*ssa.Const)
	return ok && c.Value == nil
}

// isClosureResultStored checks if a closure call's result is stored (assigned to
// a variable) rather than directly chained in an IIFE pattern.
//
// Returns true (stored) for patterns like:
//   - `q, err := closureFunc()` (flows to Extract → stored)
//   - `q := closureFunc()` (flows to Store/Alloc → stored)
//   - `if cond { q = closureFunc() }` (flows to Phi → stored)
//   - `q := closureFunc().Where("x")` (chain eventually stored → stored)
//
// Returns false (chained IIFE) for patterns like:
//   - `closureFunc().Find(nil)` (chain ends with terminal, never stored)
func isClosureResultStored(call *ssa.Call) bool {
	return isClosureResultStoredRecursive(call, make(map[*ssa.Call]bool))
}

func isClosureResultStoredRecursive(call *ssa.Call, visited map[*ssa.Call]bool) bool {
	if visited[call] {
		return false
	}
	visited[call] = true

	refs := call.Referrers()
	if refs == nil {
		return false
	}

	for _, ref := range *refs {
		switch user := ref.(type) {
		case *ssa.Extract:
			// Multi-return extraction → stored
			return true

		case *ssa.Phi:
			// Flows to Phi → stored (conditional assignment)
			return true

		case *ssa.Store:
			// Store to Alloc → stored (variable assignment)
			if _, ok := user.Addr.(*ssa.Alloc); ok {
				return true
			}

		case *ssa.MakeInterface:
			// Value converted to interface{} (e.g., for variadic args) → stored
			// This happens when passing *gorm.DB to methods with interface{} args
			return true

		case *ssa.Call:
			callee := user.Call.StaticCallee()
			if callee == nil {
				continue
			}
			sig := callee.Signature
			if sig == nil || sig.Recv() == nil || !typeutil.IsGormDB(sig.Recv().Type()) {
				continue
			}

			// Check if our result is receiver of this gorm method
			if len(user.Call.Args) > 0 && user.Call.Args[0] == call {
				// Our result is receiver - check if THAT call is stored
				if isClosureResultStoredRecursive(user, visited) {
					return true
				}
			} else {
				// Our result is passed as argument (e.g., condition.Or(ourResult))
				// This is effectively "stored" - the value is consumed by another call
				return true
			}
		}
	}

	return false
}

// cloneVisited creates a copy of the visited map.
// Used to isolate tracing state when entering closures.
func cloneVisited(visited map[ssa.Value]bool) map[ssa.Value]bool {
	clone := make(map[ssa.Value]bool, len(visited))
	maps.Copy(clone, visited)
	return clone
}

// walkCalls visits every *ssa.Call in funcs and their nested anonymous functions
// exactly once. Shared by the Scopes/Preload and Transaction callback collectors.
func walkCalls(funcs []*ssa.Function, visit func(*ssa.Call)) {
	walkInstrs(funcs, func(instr ssa.Instruction) {
		if call, ok := instr.(*ssa.Call); ok {
			visit(call)
		}
	})
}

// walkInstrs visits every instruction in funcs and their nested anonymous
// functions, visiting each function exactly once.
func walkInstrs(funcs []*ssa.Function, visit func(ssa.Instruction)) {
	visited := make(map[*ssa.Function]bool)
	var rec func(fn *ssa.Function)
	rec = func(fn *ssa.Function) {
		if fn == nil || visited[fn] {
			return
		}
		visited[fn] = true
		for _, block := range fn.Blocks {
			for _, instr := range block.Instrs {
				visit(instr)
			}
		}
		for _, af := range fn.AnonFuncs {
			rec(af)
		}
	}
	for _, fn := range funcs {
		rec(fn)
	}
}

// FuncVars maps each package-level variable that always holds one function to
// that function, such as `var helper = func(db *gorm.DB) { ... }` (#183). A
// call through such a variable has no static callee, so without this map it
// would carry none of the directives written on the closure.
//
// A variable is in the map only when the package stores to it exactly once, the
// stored value is a function, its address is used only to load and store it,
// and it is unexported. Any other variable may hold another function when it
// is called: a second assignment, a write through a pointer, or, for an
// exported one, an assignment in another package.
type FuncVars map[*ssa.Global]*ssa.Function

// CollectFuncVars builds the FuncVars of the package that funcs belong to.
// funcs must hold every function of the package with a body, including the
// package initializer, which runs package-level `var x = ...` initializers.
// Nested anonymous functions are visited too.
func CollectFuncVars(funcs []*ssa.Function) FuncVars {
	stored := make(map[*ssa.Global][]ssa.Value)
	escaped := make(map[*ssa.Global]bool)
	walkInstrs(funcs, func(instr ssa.Instruction) {
		switch x := instr.(type) {
		case *ssa.Store:
			if g, ok := x.Addr.(*ssa.Global); ok {
				stored[g] = append(stored[g], x.Val)
			}
			if g, ok := x.Val.(*ssa.Global); ok {
				escaped[g] = true
			}
			return
		case *ssa.UnOp:
			if x.Op == token.MUL {
				return
			}
		case *ssa.DebugRef:
			return
		}
		for _, op := range instr.Operands(nil) {
			if g, ok := (*op).(*ssa.Global); ok {
				escaped[g] = true
			}
		}
	})

	vars := make(FuncVars)
	for g, vals := range stored {
		if escaped[g] || len(vals) != 1 || g.Object() == nil || g.Object().Exported() {
			continue
		}
		if fn, ok := vals[0].(*ssa.Function); ok {
			vars[g] = fn
		}
	}
	return vars
}

// Callee returns the function that common calls: its static callee, or the
// function held by the package-level variable it loads. It returns nil when
// neither is known.
func (vars FuncVars) Callee(common *ssa.CallCommon) *ssa.Function {
	if fn := common.StaticCallee(); fn != nil {
		return fn
	}
	if load, ok := common.Value.(*ssa.UnOp); ok && load.Op == token.MUL {
		if g, ok := load.X.(*ssa.Global); ok {
			return vars[g]
		}
	}
	return nil
}

// collectCallbacksPassedTo returns every function passed as a variadic
// callback to a call whose static callee satisfies match, resolved by a
// scopesCallbackResolver.
func collectCallbacksPassedTo(funcs []*ssa.Function, match func(*ssa.Function) bool) map[*ssa.Function]bool {
	set := make(map[*ssa.Function]bool)
	r := &scopesCallbackResolver{funcs: funcs, set: set, seen: make(map[ssa.Value]bool)}
	walkCalls(funcs, func(call *ssa.Call) {
		callee := call.Call.StaticCallee()
		if callee == nil || !match(callee) {
			return
		}
		// The variadic callbacks arrive as a slice in the last argument.
		if args := call.Call.Args; len(args) > 0 {
			r.addSliceElems(args[len(args)-1])
		}
	})
	return set
}

// scopesCallbackResolver resolves the slice handed to Scopes/Preload back to
// the functions it can hold (#196). It follows:
//
//   - the varargs array (`Scopes(a, b)`) and slice literals
//   - `append` (`s = append(s, f)`; `Scopes(s...)`) and Phi edges
//   - local, captured and package-level variables, and struct fields
//   - method values (`Scopes(r.scope)`): both the $bound wrapper and the method
//
// It stays a may-hold set: an unresolvable source (a parameter, an interface
// call) adds nothing.
type scopesCallbackResolver struct {
	funcs        []*ssa.Function
	set          map[*ssa.Function]bool
	seen         map[ssa.Value]bool
	globalStores map[*ssa.Global][]ssa.Value // built on first use
}

// addSliceElems adds the functions that the slice (or array pointer) v may hold.
func (r *scopesCallbackResolver) addSliceElems(v ssa.Value) {
	if v == nil || r.seen[v] {
		return
	}
	r.seen[v] = true

	// Element stores: `arr[i] = f` for varargs and literals, `s[i] = f` for make.
	if refs := v.Referrers(); refs != nil {
		for _, ref := range *refs {
			idx, ok := ref.(*ssa.IndexAddr)
			if !ok || idx.X != v || idx.Referrers() == nil {
				continue
			}
			for _, ir := range *idx.Referrers() {
				if store, ok := ir.(*ssa.Store); ok && store.Addr == idx {
					r.addFunc(store.Val)
				}
			}
		}
	}

	switch x := v.(type) {
	case *ssa.Slice:
		r.addSliceElems(x.X)
	case *ssa.Phi:
		for _, e := range x.Edges {
			r.addSliceElems(e)
		}
	case *ssa.Call:
		if b, ok := x.Call.Value.(*ssa.Builtin); ok && b.Name() == "append" {
			for _, a := range x.Call.Args {
				r.addSliceElems(a)
			}
		}
	case *ssa.UnOp:
		if x.Op == token.MUL {
			for _, sv := range r.storedValues(x.X) {
				r.addSliceElems(sv)
			}
		}
	case *ssa.FreeVar:
		r.addSliceElems(FreeVarBinding(x))
	case *ssa.ChangeType:
		r.addSliceElems(x.X)
	}
}

// addFunc adds the functions that the func value v may be.
func (r *scopesCallbackResolver) addFunc(v ssa.Value) {
	if v == nil || r.seen[v] {
		return
	}
	r.seen[v] = true

	switch x := v.(type) {
	case *ssa.Function:
		r.set[directive.DeclaredFunc(x)] = true
	case *ssa.MakeClosure:
		fn, ok := x.Fn.(*ssa.Function)
		if !ok {
			return
		}
		r.set[fn] = true
		// A method value is a closure over a synthetic $bound wrapper; the
		// declared method receives the *gorm.DB, so it is a callback too (#180).
		r.set[directive.DeclaredFunc(fn)] = true
	case *ssa.MakeInterface:
		r.addFunc(x.X)
	case *ssa.ChangeType:
		r.addFunc(x.X)
	case *ssa.Phi:
		for _, e := range x.Edges {
			r.addFunc(e)
		}
	case *ssa.UnOp:
		if x.Op == token.MUL {
			for _, sv := range r.storedValues(x.X) {
				r.addFunc(sv)
			}
		}
	case *ssa.FreeVar:
		r.addFunc(FreeVarBinding(x))
	}
}

// storedValues returns the values stored at addr: a local, a captured local, a
// struct field, or a package-level variable.
func (r *scopesCallbackResolver) storedValues(addr ssa.Value) []ssa.Value {
	switch a := addr.(type) {
	case *ssa.Alloc:
		return AllocStoredValues(a)
	case *ssa.FieldAddr:
		// Every store, not only the ones that reach the load: a larger
		// callback set only makes more parameters mutable roots, so the
		// over-approximation stays on the reporting side.
		return fieldStoredValues(a, nil)
	case *ssa.FreeVar:
		return r.storedValues(FreeVarBinding(a))
	case *ssa.Global:
		return r.globalStoredValues(a)
	}
	return nil
}

// globalStoredValues returns the values stored into g by the analyzed functions
// and by the package initializer, which runs package-level `var x = ...`
// initializers but is not a source function.
func (r *scopesCallbackResolver) globalStoredValues(g *ssa.Global) []ssa.Value {
	if r.globalStores == nil {
		r.globalStores = make(map[*ssa.Global][]ssa.Value)
		funcs := slices.Clone(r.funcs)
		if g.Pkg != nil {
			if init := g.Pkg.Func("init"); init != nil {
				funcs = append(funcs, init)
			}
		}
		walkInstrs(funcs, func(instr ssa.Instruction) {
			if store, ok := instr.(*ssa.Store); ok {
				if sg, ok := store.Addr.(*ssa.Global); ok {
					r.globalStores[sg] = append(r.globalStores[sg], store.Val)
				}
			}
		})
	}
	return r.globalStores[g]
}

// isScopesOrPreloadMethod reports whether callee is gorm's Scopes or Preload
// method (gated on a gorm.DB receiver so a same-named user method is excluded).
func isScopesOrPreloadMethod(callee *ssa.Function) bool {
	name := callee.Name()
	if name != "Scopes" && name != "Preload" {
		return false
	}
	return isGormDBMethod(callee)
}

// isScopesMethod reports whether callee is gorm's Scopes method.
func isScopesMethod(callee *ssa.Function) bool {
	return callee.Name() == "Scopes" && isGormDBMethod(callee)
}

// isGormDBMethod reports whether callee is a method with a gorm.DB receiver.
func isGormDBMethod(callee *ssa.Function) bool {
	sig := callee.Signature
	return sig != nil && sig.Recv() != nil && typeutil.IsGormDB(sig.Recv().Type())
}

// collectImmutableCallbacksFromCall adds the callback function passed to a gorm
// Transaction/FindInBatches call to set. The callback sits at a different
// argument position per method (Transaction: first argument; FindInBatches:
// third), so scan for the func-typed argument whose
// signature takes a *gorm.DB rather than hard-coding an index.
func collectImmutableCallbacksFromCall(call *ssa.Call, set map[*ssa.Function]bool) {
	callee := call.Call.StaticCallee()
	if callee == nil || !IsImmutableCallbackMethod(callee) {
		return
	}
	for _, arg := range call.Call.Args {
		cb := callbackFuncValue(arg)
		if cb != nil && cb.Signature != nil && directive.HasGormDBParameter(cb.Signature) {
			set[cb] = true
		}
	}
}

// IsImmutableCallbackMethod reports whether callee is a gorm method that hands a
// fresh (clone>0) *gorm.DB to a callback — Transaction or FindInBatches — gated
// on a gorm.DB receiver so a same-named user method is excluded.
//
// Connection is deliberately absent: it calls fc(db.getInstance()), and
// getInstance yields a clone==0 handle, so its callback's tx accumulates
// conditions like any mid-chain value (#173).
func IsImmutableCallbackMethod(callee *ssa.Function) bool {
	switch callee.Name() {
	case "Transaction", "FindInBatches":
	default:
		return false
	}
	return isGormDBMethod(callee)
}

// callbackFuncValue extracts the concrete function from a value stored into a
// Scopes/Preload varargs slot: a MakeClosure (capturing closure), a bare
// *ssa.Function (named function reference), or either boxed in an interface
// (Preload's args are ...interface{}). A generic instance or a bound method
// wrapper is mapped to the function whose body receives the argument (#180).
func callbackFuncValue(v ssa.Value) *ssa.Function {
	switch x := v.(type) {
	case *ssa.MakeClosure:
		if fn, ok := x.Fn.(*ssa.Function); ok {
			return directive.DeclaredFunc(fn)
		}
	case *ssa.Function:
		return directive.DeclaredFunc(x)
	case *ssa.MakeInterface:
		return callbackFuncValue(x.X)
	}
	return nil
}

// holdsGormDB reports whether a value of type t holds a *gorm.DB the tracer can
// follow: t is *gorm.DB or gorm.DB, a pointer to such a type (**gorm.DB, ...),
// or a struct with such a field, embedded or not (#181). seen stops recursive
// struct types.
//
// Unlike containsGormDB in the directive package, this does NOT check:
//   - interface{} (would cause false positives in SSA analysis)
//   - slices, maps, channels (SSA handles these differently)
func holdsGormDB(t types.Type, seen map[types.Type]bool) bool {
	if t == nil || seen[t] {
		return false
	}
	seen[t] = true

	if typeutil.IsGormDB(t) {
		return true
	}
	switch u := t.Underlying().(type) {
	case *types.Pointer:
		return holdsGormDB(u.Elem(), seen)
	case *types.Struct:
		for field := range u.Fields() {
			if holdsGormDB(field.Type(), seen) {
				return true
			}
		}
	}
	return false
}
