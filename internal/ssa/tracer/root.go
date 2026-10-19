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
	"maps"
	"slices"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/directive"
	"github.com/mpyw/gormreuse/internal/ssa/callsite"
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
	allRoots             map[allRootsKey][]ssa.Value // Memo of FindAllMutableRoots
	aliases              map[aliasedRootsKey]bool    // Memo of mayAlias
	funcVars             callsite.FuncVars           // Package-level variables that hold one function (#183)
}

// NewRootTracer creates a new RootTracer.
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
func NewRootTracer(pureFuncs, immutableReturnFuncs, immutableParamFuncs *directive.DirectiveFuncSet, pure *PureVerdicts, scopesCallbacks, immutableCallbacks map[*ssa.Function]bool, funcVars callsite.FuncVars) *RootTracer {
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
		allRoots:             make(map[allRootsKey][]ssa.Value),
		aliases:              make(map[aliasedRootsKey]bool),
	}
}

// allRootsKey is a FindAllMutableRoots query.
type allRootsKey struct {
	v        ssa.Value
	loopInfo *cfg.LoopInfo
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
//
// The result depends only on v and loopInfo, so it is computed once per pair
// (#260): every gorm call asks for the roots of its receiver, and
// collectAliasedRoots asks again at each step down the chain. The caller must
// not modify the returned slice.
func (t *RootTracer) FindAllMutableRoots(v ssa.Value, loopInfo *cfg.LoopInfo) []ssa.Value {
	key := allRootsKey{v, loopInfo}
	if roots, ok := t.allRoots[key]; ok {
		return roots
	}
	roots := slices.Clip(t.traceAll(v, make(map[ssa.Value]bool), loopInfo))
	t.allRoots[key] = roots
	return roots
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

// Site reads instr as a callsite.Site, resolving a call through a
// package-level variable that holds one function (#183).
func (t *RootTracer) Site(instr ssa.CallInstruction) callsite.Site {
	return callsite.SiteOf(instr, t.funcVars)
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
// The name lookup is gated on the function being genuinely defined by
// gorm.io/gorm: either a method whose receiver is gorm.DB
// (Session, WithContext, Debug, Begin, Transaction), or a package-level
// function in the gorm.io/gorm package (gorm.Open). Without this gate, any
// user-defined function sharing a builtin name — a method named Session, a
// helper named Open — would be silently trusted as immutable-returning (and,
// via IsPureFunction, as pure), disabling reuse detection around it.
func (t *RootTracer) isImmutableReturningBuiltin(fn *ssa.Function) bool {
	if fn == nil {
		return false
	}
	if !typeutil.IsImmutableReturningBuiltin(fn.Name()) {
		return false
	}
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
//
//declscope:shared // field.go, freevar.go and store.go trace the values they resolve with it
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
	site := t.Site(call)

	// The result of a pure function may be its argument (#188).
	if args := t.pureReturnedArgs(site); len(args) > 0 {
		for _, arg := range args {
			if root := t.trace(arg, maps.Clone(visited), loopInfo); root != nil {
				return root
			}
		}
		if t.returnsOnlyParams(site.Declared) {
			return nil
		}
	}

	// Handle closures (IIFE)
	if site.Kind == callsite.SiteClosure {
		if root := t.traceIIFEReturns(site.Callee, visited, loopInfo); root != nil {
			// If closure result is stored in a variable (Extract instruction),
			// treat each call as independent root.
			// Only trace through IIFE when result is directly chained.
			if isClosureResultStored(call) {
				return call
			}
			return root
		}
	}

	if site.IsGormMethod() {
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
	if t.returnsImmutable(site.Declared) {
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
		return t.traceAllocStore(val, visited, loopInfo)

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
			if !isNilPhiEdge(initialEdge) && !visited[initialEdge] {
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
				if isNilPhiEdge(edge) || visited[edge] {
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
				if isNilPhiEdge(edge) || visited[edge] {
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
	if loopSwapPhis(phi, loopInfo) != nil {
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
		if isNilPhiEdge(edge) || visited[edge] {
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
		return t.traceAllocStore(p, visited, loopInfo)
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

// fieldRootKey is the call and field path that internFieldRoot interns a
// FieldRoot by.
type fieldRootKey struct {
	call *ssa.Call
	path string
}

// internFieldRoot returns the one FieldRoot for r's call and path, or nil when
// the call returns immutable values (a builtin or //gormreuse:immutable-return
// function).
func (t *RootTracer) internFieldRoot(r *FieldRoot) ssa.Value {
	if t.returnsImmutable(t.Site(r.Call).Declared) {
		return nil
	}
	key := fieldRootKey{call: r.Call, path: r.pathString()}
	if existing, ok := t.fieldRoots[key]; ok {
		return existing
	}
	t.fieldRoots[key] = r
	return r
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
			retVisited := maps.Clone(visited)
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
			retVisited := maps.Clone(visited)
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
//
//declscope:shared // field.go, freevar.go and store.go trace the values they resolve with it
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
		if loopHeaderPhis := loopSwapPhis(val, loopInfo); loopHeaderPhis != nil {
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
		site := t.Site(val)
		var argRoots []ssa.Value
		for _, arg := range t.pureReturnedArgs(site) {
			argRoots = append(argRoots, t.traceAll(arg, maps.Clone(visited), loopInfo)...)
		}
		if len(argRoots) > 0 {
			return argRoots
		}
		// Handle closure calls (IIFE) - collect ALL roots from all returns. A
		// call through a method value is a call of its own (see traceCall).
		if site.Kind == callsite.SiteClosure {
			if roots := t.traceAllIIFEReturns(site.Callee, visited, loopInfo); len(roots) > 0 {
				// If closure result is stored, treat call itself as root
				if isClosureResultStored(val) {
					return []ssa.Value{val}
				}
				return roots
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
		freshVisited := maps.Clone(visited)
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
		if loopHeaderPhis := loopSwapPhis(p, loopInfo); loopHeaderPhis != nil {
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
		if isNilPhiEdge(edge) || visited[edge] {
			continue
		}
		roots = append(roots, t.traceAll(edge, visited, loopInfo)...)
	}
	return roots
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
		site := t.Site(val)
		if t.isImmutableReturningBuiltin(site.Declared) {
			return !site.SessionEffect().ReturnsMutable
		}
		return t.returnsImmutable(site.Declared)
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
	t.collectAliasedRoots(v, loopInfo, true, make(map[aliasedRootVisit]bool), &roots)
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

// aliasedRootVisit is a call visited by collectAliasedRoots, and whether Session
// and Debug copies still counted there.
type aliasedRootVisit struct {
	call   *ssa.Call
	copies bool
}

// collectAliasedRoots adds to roots the mutable roots that v reaches through
// Statement-sharing builtin calls, while copies is set, and through read-only
// arguments of the gorm calls in its chain. seen stops cycles through loops.
//
// A search that mayAlias says adds nothing is skipped. Without that, a chain
// of n conditional reassignments takes time cubic in n (#260): each of the n
// calls searches n steps down the chain, and each step goes through the n
// roots of its receiver. Skipping such a search does not change what is added
// or in what order: everything it would reach adds nothing either.
func (t *RootTracer) collectAliasedRoots(v ssa.Value, loopInfo *cfg.LoopInfo, copies bool, seen map[aliasedRootVisit]bool, roots *[]AliasedRoot) {
	if !t.mayAlias(v, loopInfo, copies) {
		return
	}
	for _, candidate := range t.FindAllMutableRoots(v, loopInfo) {
		call, site, ok := t.aliasStep(candidate)
		if !ok || seen[aliasedRootVisit{call, copies}] {
			continue
		}
		seen[aliasedRootVisit{call, copies}] = true
		isCopy := copies && t.sharesReceiverStatement(site)
		if isCopy {
			if r := t.FindMutableRoot(site.Recv, loopInfo); r != nil {
				*roots = append(*roots, AliasedRoot{Root: r, Copy: call})
			}
		}
		for _, arg := range pollutionsource.ReadOnlyGormArgs(site) {
			if r := t.FindMutableRoot(arg, loopInfo); r != nil {
				*roots = append(*roots, AliasedRoot{Root: r, Copy: call, Argument: true})
			}
			t.collectAliasedRoots(arg, loopInfo, true, seen, roots)
		}
		t.collectAliasedRoots(site.Recv, loopInfo, isCopy, seen, roots)
	}
}

// aliasedRootsKey is a collectAliasedRoots search that mayAlias answers.
type aliasedRootsKey struct {
	v        ssa.Value
	loopInfo *cfg.LoopInfo
	copies   bool
}

// mayAlias reports whether collectAliasedRoots(v, loopInfo, copies, ...) may
// add a root. The answer is computed once per search. A search that reaches
// itself again through a loop is assumed to add one.
func (t *RootTracer) mayAlias(v ssa.Value, loopInfo *cfg.LoopInfo, copies bool) bool {
	key := aliasedRootsKey{v, loopInfo, copies}
	if may, ok := t.aliases[key]; ok {
		return may
	}
	t.aliases[key] = true
	may := slices.ContainsFunc(t.FindAllMutableRoots(v, loopInfo), func(candidate ssa.Value) bool {
		_, site, ok := t.aliasStep(candidate)
		if !ok {
			return false
		}
		isCopy := copies && t.sharesReceiverStatement(site)
		if isCopy && t.FindMutableRoot(site.Recv, loopInfo) != nil {
			return true
		}
		for _, arg := range pollutionsource.ReadOnlyGormArgs(site) {
			if t.FindMutableRoot(arg, loopInfo) != nil || t.mayAlias(arg, loopInfo, true) {
				return true
			}
		}
		return t.mayAlias(site.Recv, loopInfo, isCopy)
	})
	t.aliases[key] = may
	return may
}

// aliasStep returns the gorm method call that collectAliasedRoots goes
// through at candidate, and its Site, or false when it does not go through
// candidate. A call through a method value (w := q.Where; w(x, sub)) is read
// as the method call it stands for (#178).
func (t *RootTracer) aliasStep(candidate ssa.Value) (*ssa.Call, callsite.Site, bool) {
	call, ok := candidate.(*ssa.Call)
	if !ok {
		return nil, callsite.Site{}, false
	}
	site := t.Site(call)
	if !site.IsGormMethod() || site.Recv == nil {
		return nil, callsite.Site{}, false
	}
	return call, site, true
}

// sharesReceiverStatement reports whether site is a gorm Session or Debug
// method call whose result may share its receiver's Statement.
func (t *RootTracer) sharesReceiverStatement(site callsite.Site) bool {
	if !t.isImmutableReturningBuiltin(site.Declared) || site.Recv == nil {
		return false
	}
	return site.SessionEffect().SharesStatement
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
func (t *RootTracer) pureReturnedArgs(site callsite.Site) []ssa.Value {
	if !typeutil.HasGormDBResult(site.Instr.(*ssa.Call).Type()) {
		return nil
	}
	// A call through a method value passes its receiver as a binding, not
	// as an argument; put it back so a pure method returning it is followed.
	args := site.Args
	if site.Recv != nil {
		args = append([]ssa.Value{site.Recv}, args...)
	}
	args = t.pure.ReturnedArgs(site.Declared, args)
	if len(args) == 0 || !t.IsPureFunction(site.Declared) {
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
