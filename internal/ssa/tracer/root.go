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
	immutableReturnFuncs *directive.DirectiveFuncSet // Functions returning immutable *gorm.DB
	immutableParamFuncs  *directive.DirectiveFuncSet // Functions whose *gorm.DB params are immutable (opt out of Phase 1b)
	scopesCallbacks      map[*ssa.Function]bool      // Scopes/Preload callbacks (params are mutable roots)
	immutableCallbacks   map[*ssa.Function]bool      // Transaction/FindInBatches callbacks (fresh tx)
	fieldRoots           map[fieldRootKey]*FieldRoot // Interned FieldRoots (one per call and field path)
	allRoots             map[allRootsKey][]ssa.Value // Memo of FindAllMutableRoots
	funcVars             callsite.FuncVars           // Package-level variables that hold one function (#183)

	//declscope:shared // pure.go's IsPureFunction reads it
	pureFuncs *directive.DirectiveFuncSet // User-defined pure functions
	//declscope:shared // pure.go reads the verdicts
	pure *PureVerdicts // What validating the pure functions found
	//declscope:shared // pure.go's pureReturnsOnlyParams fills it
	onlyParams map[*ssa.Function]bool // Memo of pureReturnsOnlyParams
	//declscope:shared // alias.go's mayAlias fills it
	aliases map[aliasedRootsKey]bool // Memo of mayAlias
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

// aliasedRootsKey is a collectAliasedRoots search that mayAlias answers.
//
//declscope:shared // alias.go's mayAlias keys its memo by it
type aliasedRootsKey struct {
	v        ssa.Value
	loopInfo *cfg.LoopInfo
	copies   bool
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

// Site reads instr as a callsite.Site, resolving a call through a
// package-level variable that holds one function (#183).
func (t *RootTracer) Site(instr ssa.CallInstruction) callsite.Site {
	return callsite.SiteOf(instr, t.funcVars)
}

// FuncName names fn in a diagnostic (see callsite.FuncVars.FuncName).
func (t *RootTracer) FuncName(fn *ssa.Function) string {
	return t.funcVars.FuncName(fn)
}

// FuncVars returns the package-level function variables the tracer resolves
// calls through. It may be nil.
func (t *RootTracer) FuncVars() callsite.FuncVars {
	return t.funcVars
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
//
//declscope:shared // pure.go's IsPureFunction and alias.go's aliasSharesStatement trust gorm builtins with it
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
//	      └─── Outer IIFE call triggers traceClosureReturns
func (t *RootTracer) traceCall(call *ssa.Call, visited map[ssa.Value]bool, loopInfo *cfg.LoopInfo) ssa.Value {
	site := t.Site(call)

	// The result of a pure function may be its argument (#188).
	if args := t.pureReturnedArgs(site); len(args) > 0 {
		for _, arg := range args {
			if root := t.trace(arg, maps.Clone(visited), loopInfo); root != nil {
				return root
			}
		}
		if t.pureReturnsOnlyParams(site.Declared) {
			return nil
		}
	}

	// Handle closures (IIFE)
	if site.Kind == callsite.SiteClosure {
		if root := t.traceClosureReturns(site.Callee, visited, loopInfo); root != nil {
			// If closure result is stored in a variable (Extract instruction),
			// treat each call as independent root.
			// Only trace through IIFE when result is directly chained.
			if t.isClosureResultStored(call) {
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
			return t.collectInitialPhiEdgeRoots(loopHeaderPhis, val, visited, loopInfo)
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
			if roots := t.traceAllClosureReturns(site.Callee, visited, loopInfo); len(roots) > 0 {
				// If closure result is stored, treat call itself as root
				if t.isClosureResultStored(val) {
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
			return t.collectInitialPhiEdgeRoots(loopHeaderPhis, p, visited, loopInfo)
		}

		// Regular Phi - traverse all edges
		return t.traceAllPhiEdges(p, visited, loopInfo)
	default:
		return t.traceAll(p, visited, loopInfo)
	}
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
