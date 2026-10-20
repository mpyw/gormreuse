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
	"github.com/mpyw/gormreuse/internal/effort"
	"github.com/mpyw/gormreuse/internal/ssa/callsite"
	"github.com/mpyw/gormreuse/internal/ssa/cfg"
	"github.com/mpyw/gormreuse/internal/ssa/origin"
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
	immutableReturnFuncs *directive.DirectiveFuncSet        // Functions returning immutable *gorm.DB
	immutableParamFuncs  *directive.DirectiveFuncSet        // Functions whose *gorm.DB params are immutable (opt out of Phase 1b)
	scopesCallbacks      map[*ssa.Function]bool             // Scopes/Preload callbacks (params are mutable roots)
	immutableCallbacks   map[*ssa.Function]bool             // Transaction/FindInBatches callbacks (fresh tx)
	fieldRoots           map[fieldRootKey]*FieldRoot        // Interned FieldRoots (one per call and field path)
	roots                map[allRootsKey]ssa.Value          // Memo of FindMutableRoot
	allRoots             map[allRootsKey][]ssa.Value        // Memo of FindAllMutableRoots
	rootSets             map[allRootsKey]map[ssa.Value]bool // Memo of HasMutableRoot, for long root lists
	funcVars             callsite.FuncVars                  // Package-level variables that hold one function (#183)
	distrustedReturn     func(*ssa.Function) bool           // immutable-return functions not trusted (#314); nil trusts all
	distrustedCallback   func(*ssa.Function) bool           // immutableCallbacks not exempted (#314); nil exempts all

	//declscope:shared // pure.go's IsPureFunction reads it
	pureFuncs *directive.DirectiveFuncSet // User-defined pure functions
	//declscope:shared // pure.go reads the verdicts
	pure *PureVerdicts // What validating the pure functions found
	//declscope:shared // pure.go's pureReturnsOnlyParams fills it
	onlyParams map[*ssa.Function]bool // Memo of pureReturnsOnlyParams
	//declscope:shared // alias.go's mayAlias fills it
	aliases map[aliasedRootsKey]bool // Memo of mayAlias
	//declscope:shared // alias.go's mayAliasVisit fills it
	aliasSteps map[aliasedRootsKey]bool // Memo of mayAliasStep, keyed by candidate root (#350)
	//declscope:shared // store.go and field.go resolve loads through it
	stores *storeMemo // Memo of the stores that reads see (#325)
	//declscope:shared // closure.go's closureRunsRepeatedly asks it
	cfgs *cfg.Analyzer // CFG cache for the Repeats of closures
	//declscope:shared // closure.go's closureReturnsImmutable fills it
	immutableClosures map[*ssa.Function]bool // Memo of closureReturnsImmutable (#308)
	//declscope:shared // pointee.go keeps its PointeeRoots and searches in it
	pointees pointeeMemo // What calls may store through a variable's address (#345)
	//declscope:shared // elem.go keeps its ElemRoots and containers in it
	elems elemMemo // The roots of reads from containers (#374)
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
		roots:                make(map[allRootsKey]ssa.Value),
		allRoots:             make(map[allRootsKey][]ssa.Value),
		rootSets:             make(map[allRootsKey]map[ssa.Value]bool),
		aliases:              make(map[aliasedRootsKey]bool),
		aliasSteps:           make(map[aliasedRootsKey]bool),
		stores:               newStoreMemo(),
		cfgs:                 cfg.NewAnalyzer(),
		immutableClosures:    make(map[*ssa.Function]bool),
	}
}

// Distrusting returns a tracer configured like t, with empty memos, that does
// not trust the immutable-return functions that returns reports, and does not
// exempt the parameter of the immutable callbacks that callbacks reports. It
// tells which contracts rely on a broken one (#314). The predicates may grow
// between calls, so the memos are not shared.
func (t *RootTracer) Distrusting(returns, callbacks func(*ssa.Function) bool) *RootTracer {
	d := NewRootTracer(t.pureFuncs, t.immutableReturnFuncs, t.immutableParamFuncs, t.pure, t.scopesCallbacks, t.immutableCallbacks, t.funcVars)
	d.distrustedReturn = returns
	d.distrustedCallback = callbacks
	return d
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
//
// Like FindAllMutableRoots, the result depends only on recv and loopInfo, so
// it is computed once per pair (#350): the handlers, the alias search and the
// fixes each ask for the root of the same receivers, and a read of a field
// reassigned n times walks its n stores each time.
func (t *RootTracer) FindMutableRoot(recv ssa.Value, loopInfo *cfg.LoopInfo) ssa.Value {
	key := allRootsKey{recv, loopInfo}
	if root, ok := t.roots[key]; ok {
		return root
	}
	root := t.trace(recv, make(map[ssa.Value]bool), loopInfo)
	t.roots[key] = root
	return root
}

// MutableRoots returns the mutable roots that v may hold: first the one it
// resolves to (FindMutableRoot), then the others that FindAllMutableRoots
// finds through a merge. It is nil when v has no mutable root.
//
// It is also nil when FindMutableRoot finds no root but FindAllMutableRoots
// does. That list holds what a merge may hold, immutable values included, and
// the roots inside a closure that an immutable-return directive vouches for,
// which FindMutableRoot trusts. So FindMutableRoot decides whether v is
// mutable; it tries every value a merge or a variable in memory may hold
// (#340).
func (t *RootTracer) MutableRoots(v ssa.Value, loopInfo *cfg.LoopInfo) []ssa.Value {
	root := t.FindMutableRoot(v, loopInfo)
	if root == nil {
		return nil
	}
	roots := []ssa.Value{root}
	for _, r := range t.FindAllMutableRoots(v, loopInfo) {
		if r != root {
			roots = append(roots, r)
		}
	}
	return roots
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
//
// A loopInfo with no loop header gives the same result as nil, so such
// queries share one entry (see rootsLoopInfo).
func (t *RootTracer) FindAllMutableRoots(v ssa.Value, loopInfo *cfg.LoopInfo) []ssa.Value {
	loopInfo = rootsLoopInfo(loopInfo)
	key := allRootsKey{v, loopInfo}
	if roots, ok := t.allRoots[key]; ok {
		return roots
	}
	roots := slices.Clip(t.traceAll(v, newAllRootsSearch(make(map[ssa.Value]bool)), loopInfo))
	t.allRoots[key] = roots
	return roots
}

// rootsLoopInfo returns the loopInfo that a search with loopInfo can use in
// its place: nil when loopInfo has no loop header.
//
// The tracer reads a loopInfo only through IsLoopHeader. With no header every
// answer is false, and each reader then does what it does for nil:
// tracePhi skips the back-edge order, loopSwapPhis finds no loop-header edge,
// and the rest only pass loopInfo on. A loopInfo describes one function, so a
// closure without loops traced a value of its parent under its own loopInfo
// and missed the entry the parent's analysis made, once per closure (#300).
// Its search is now shared with every other one that sees no loop header.
// A loopInfo that has headers still keys its own entry: its function may be
// reached from v, such as the body of a closure that v calls.
//
//declscope:shared // alias.go's FindAliasedRoots keys its searches by it too
func rootsLoopInfo(loopInfo *cfg.LoopInfo) *cfg.LoopInfo {
	if !loopInfo.HasLoopHeaders() {
		return nil
	}
	return loopInfo
}

// rootSetMinLen is the shortest FindAllMutableRoots list that HasMutableRoot
// keeps as a set. A shorter one is searched in place.
const rootSetMinLen = 16

// HasMutableRoot reports whether root is one of FindAllMutableRoots(v,
// loopInfo). A long list is kept as a set, once per pair: isAssignment asks it
// about each root of a call's receiver, and after n loops that rebuild one
// query the receiver has n roots, so searching the list took time cubic in n
// (#300).
func (t *RootTracer) HasMutableRoot(v ssa.Value, loopInfo *cfg.LoopInfo, root ssa.Value) bool {
	loopInfo = rootsLoopInfo(loopInfo)
	key := allRootsKey{v, loopInfo}
	if set, ok := t.rootSets[key]; ok {
		return set[root]
	}
	roots := t.FindAllMutableRoots(v, loopInfo)
	if len(roots) < rootSetMinLen {
		return slices.Contains(roots, root)
	}
	set := make(map[ssa.Value]bool, len(roots))
	for _, r := range roots {
		set[r] = true
	}
	t.rootSets[key] = set
	return set[root]
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
	effort.Step()

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

	// The result of a pure function may be its argument (#188). The argument
	// shares visited with the other edges: a value its search reached and left
	// without a root has none from any other edge either (#297).
	if args := t.pureReturnedArgs(site); len(args) > 0 {
		for _, arg := range args {
			if root := t.trace(arg, visited, loopInfo); root != nil {
				return root
			}
		}
		if pureReturnsReceiver(site) || t.pureReturnsOnlyParams(site.Declared) {
			return nil
		}
	}

	// An inferred getter, a closure too, returns what its argument holds
	// (#342). Everything else it returns is immutable.
	if reads := t.paramReads(site.Declared); len(reads) > 0 && typeutil.HasGormDBResult(call.Type()) {
		return t.traceParamReads(site, reads, visited, loopInfo)
	}

	// Handle closures (IIFE)
	if site.Kind == callsite.SiteClosure {
		if root := t.traceClosureReturns(site.Callee, visited, loopInfo); root != nil {
			// If closure result is stored in a variable (Extract instruction),
			// treat each call as independent root.
			// Only trace through IIFE when result is directly chained.
			// So is each call of a closure that runs more than once and
			// builds a fresh chain on each run (#251).
			if t.isClosureResultStored(call) || t.closureCallOwnsRoots(call, []ssa.Value{root}, visited, loopInfo) {
				return call
			}
			return root
		}
		// No return of the closure has a root, so neither has the call
		// (#308). A nil root alone may also mean several roots.
		if t.closureReturnsImmutable(site.Callee) {
			return nil
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
	// A read from a container: a slice or array element, a map value, a
	// receive, or a load through a pointer parameter (#374)
	if root, ok := t.elemRoot(v); ok {
		return root
	}
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
		return t.traceAllocStore(val, nil, visited, loopInfo)

	case *ssa.Field:
		// Field: a field of a struct held in a register, as promoted methods
		// of an embedded *gorm.DB read it (#181)
		if vals := t.stores.fieldValueStoredValues(val); len(vals) > 0 {
			return t.traceStores(vals, visited, loopInfo)
		}
		return nil

	case *FieldRoot:
		// FieldRoot: a field of a struct a call returned (#181)
		return t.internFieldRoot(val)

	case *PointeeRoot:
		// PointeeRoot: what a call may store through a variable's address
		// (#345)
		return val

	case *ElemRoot:
		// ElemRoot: a read from a container (#374)
		return val

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
		return t.traceFreeVarLoad(p, load, visited, loopInfo)
	case *ssa.Alloc:
		return t.traceAllocStore(p, load, visited, loopInfo)
	case *ssa.FieldAddr:
		return t.traceFieldStore(p, load, visited, loopInfo)
	case *ssa.Global:
		// Single-root: the first value stored into the global that reaches
		// the load and has a root (#340).
		if vals := t.stores.globalStoredValues(p, load); len(vals) > 0 {
			return t.traceStores(vals, visited, loopInfo)
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
	case *PointeeRoot:
		return r.Block()
	case *ElemRoot:
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
	if t.returnsImmutable(t.Site(r.Call).Declared) && !paramReadCrossesPointer(r) {
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
// A value already visited in the search adds nothing: either it is still
// being traced (a cycle), or its roots are in the result already.
//
//declscope:shared // field.go, freevar.go and store.go trace the values they resolve with it
func (t *RootTracer) traceAll(v ssa.Value, search *allRootsSearch, loopInfo *cfg.LoopInfo) []ssa.Value {
	if v == nil || search.visited[v] {
		return nil
	}
	search.visited[v] = true
	effort.Step()
	roots := t.traceAllUnvisited(v, search, loopInfo)
	if _, ok := search.rooted[v]; !ok {
		search.rooted[v] = len(roots) > 0
	}
	return roots
}

// allRootsSearch is the state of one traceAll search.
//
//declscope:shared // closure.go, field.go, freevar.go, phi.go, pure.go and store.go pass it along traceAll
type allRootsSearch struct {
	// visited holds the values the search has entered.
	visited map[ssa.Value]bool

	// rooted holds the values the search has finished, and whether each has a
	// root in the result. A value in visited but not here is still being
	// traced.
	//
	//declscope:private
	rooted map[ssa.Value]bool
}

// newAllRootsSearch starts a traceAll search that treats the values in visited
// as already traced.
//
//declscope:shared // closure.go and pure.go start their own searches
func newAllRootsSearch(visited map[ssa.Value]bool) *allRootsSearch {
	return &allRootsSearch{visited: visited, rooted: make(map[ssa.Value]bool)}
}

// traceAllUnvisited is traceAll for a value the search enters for the first
// time.
func (t *RootTracer) traceAllUnvisited(v ssa.Value, search *allRootsSearch, loopInfo *cfg.LoopInfo) []ssa.Value {
	// A *gorm.DB parameter is a mutable root by default (Phase 1b, #61), except
	// the exempt cases; keep this consistent with trace()/FindMutableRoot so
	// Phi-based pollution checks see it too.
	if p, ok := v.(*ssa.Parameter); ok && t.isMutableParam(p) {
		return []ssa.Value{p}
	}
	if root, ok := t.elemRoot(v); ok {
		if root == nil {
			return nil
		}
		return []ssa.Value{root}
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
			return t.collectInitialPhiEdgeRoots(loopHeaderPhis, val, search, loopInfo)
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
		return t.traceAllPhiEdges(val, search, loopInfo)

	case *ssa.UnOp:
		if val.Op == token.MUL {
			return t.traceAllPointerLoads(val, search, loopInfo)
		}
		return t.traceAll(val.X, search, loopInfo)

	case *ssa.Alloc:
		return t.traceAllAllocStores(val, nil, search, loopInfo)

	case *ssa.FreeVar:
		return t.traceAllFreeVar(val, search, loopInfo)

	case *ssa.Field:
		var roots []ssa.Value
		for _, stored := range t.stores.fieldValueStoredValues(val) {
			roots = append(roots, t.traceAll(stored, search, loopInfo)...)
		}
		return roots

	case *ssa.Call:
		// The result of a pure function may be any argument it returns (#188).
		// Like a Session result, a call that trace() finds immutable may stay
		// in this list: nothing branches it, so it is never polluted.
		//
		// The arguments share the search. An argument the search has already
		// finished with a root adds nothing, since its roots are in the result,
		// and the call is not a root of its own. Tracing each argument in a copy
		// of visited instead walked the same values again from every other
		// edge of a Phi, which doubled the work at each merge (#297).
		site := t.Site(val)
		var argRoots []ssa.Value
		rooted := false
		for _, arg := range t.pureReturnedArgs(site) {
			if search.rooted[arg] {
				rooted = true
				continue
			}
			argRoots = append(argRoots, t.traceAll(arg, search, loopInfo)...)
		}
		if rooted {
			search.rooted[val] = true
		}
		if len(argRoots) > 0 || rooted || pureReturnsReceiver(site) {
			return argRoots
		}
		// An inferred getter returns what its argument holds (#342).
		if reads := t.paramReads(site.Declared); len(reads) > 0 && typeutil.IsGormDB(val.Type()) {
			return t.traceAllParamReads(site, reads, search, loopInfo)
		}
		// Handle closure calls (IIFE) - collect ALL roots from all returns. A
		// call through a method value is a call of its own (see traceCall).
		if site.Kind == callsite.SiteClosure {
			if roots := t.traceAllClosureReturns(site.Callee, search, loopInfo); len(roots) > 0 {
				// If closure result is stored, or each call builds a fresh
				// chain (#251), treat call itself as root
				if t.isClosureResultStored(val) || t.closureCallOwnsRoots(val, roots, search.visited, loopInfo) {
					return []ssa.Value{val}
				}
				return roots
			}
			// Values this search visited before trace to nothing here,
			// whatever their roots, so only a search of its own tells
			// that the closure returns no mutable value (#308).
			if t.closureReturnsImmutable(site.Callee) {
				return nil
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
		freshVisited := maps.Clone(search.visited)
		delete(freshVisited, v)
		if root := t.trace(v, freshVisited, loopInfo); root != nil {
			return []ssa.Value{root}
		}
		return nil
	}
}

func (t *RootTracer) traceAllPointerLoads(load *ssa.UnOp, search *allRootsSearch, loopInfo *cfg.LoopInfo) []ssa.Value {
	switch p := load.X.(type) {
	case *ssa.Alloc:
		return t.traceAllAllocStores(p, load, search, loopInfo)
	case *ssa.FreeVar:
		return t.traceAllFreeVarLoads(p, load, search, loopInfo)
	case *ssa.FieldAddr:
		return t.traceAllFieldStores(p, load, search, loopInfo)
	case *ssa.Global:
		var roots []ssa.Value
		for _, v := range t.stores.globalStoredValues(p, load) {
			roots = append(roots, t.traceAll(v, search, loopInfo)...)
		}
		return roots
	case *ssa.Phi:
		// Check for loop variable swap pattern
		if loopHeaderPhis := loopSwapPhis(p, loopInfo); loopHeaderPhis != nil {
			return t.collectInitialPhiEdgeRoots(loopHeaderPhis, p, search, loopInfo)
		}

		// Regular Phi - traverse all edges
		return t.traceAllPhiEdges(p, search, loopInfo)
	default:
		return t.traceAll(p, search, loopInfo)
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
	if t.isExemptCallback(fn) {
		return false
	}
	if t.immutableParamFuncs != nil && t.immutableParamFuncs.Contains(fn) {
		return false
	}
	return true
}

// isExemptCallback reports whether the *gorm.DB parameter of fn is a fresh
// handle: fn is a Transaction, FindInBatches or immutable-input callback that
// is not distrusted, and not a Scopes or Connection callback.
//
//declscope:shared // prove.go proves a callback's parameter with it
func (t *RootTracer) isExemptCallback(fn *ssa.Function) bool {
	return !t.scopesCallbacks[fn] && t.immutableCallbacks[fn] && (t.distrustedCallback == nil || !t.distrustedCallback(fn))
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
//
//declscope:shared // prove.go proves a call with it
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
// An inferred function that may return what its argument holds is immutable
// only where the argument is (paramReads), so it is not one.
func (t *RootTracer) returnsImmutable(callee *ssa.Function) bool {
	if t.isImmutableReturningBuiltin(callee) {
		return true
	}
	if t.distrustedReturn != nil && callee != nil && t.distrustedReturn(origin.Declared(callee)) {
		return false
	}
	return t.immutableReturnFuncs != nil && t.immutableReturnFuncs.Contains(callee) && len(t.paramReads(callee)) == 0
}

// paramReads returns the reads of its parameters that a trusted
// immutable-return callee may return (#342), or nil when its results are
// immutable at every call. A getter such as
//
//	func (h *Holder) DB() *gorm.DB { return h.db }
//
// is inferred immutable-return because a field read through a receiver is
// immutable where it is read. That holds only where the caller's struct holds
// an immutable value, so a call of it is traced as the selector h.db written
// at the call: `h := &Holder{db: q.Where("x")}; h.DB()` has q.Where's root.
func (t *RootTracer) paramReads(callee *ssa.Function) []directive.ParamRead {
	if callee == nil || t.immutableReturnFuncs == nil || t.isImmutableReturningBuiltin(callee) {
		return nil
	}
	if t.distrustedReturn != nil && t.distrustedReturn(origin.Declared(callee)) {
		return nil
	}
	return t.immutableReturnFuncs.ParamReads(callee)
}

// HasParamReads reports whether a call of callee is traced as the reads of
// its parameters that it may return (see paramReads), as for a getter.
func (t *RootTracer) HasParamReads(callee *ssa.Function) bool {
	return len(t.paramReads(callee)) > 0
}
