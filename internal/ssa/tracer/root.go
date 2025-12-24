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
//	│  *ssa.Parameter         │  STOP - parameter is immutable source      │
//	│  *ssa.Const (nil)       │  STOP - nil is ignored                     │
//	│  Builtin pure call      │  STOP - returns immutable (nil root)       │
//	│  User-defined pure call │  Returns call as root (may be mutable)     │
//	└───────────────────────────────────────────────────────────────────────┘
package tracer

import (
	"go/token"
	"go/types"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/directive"
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
	pureFuncs *directive.PureFuncSet // User-defined pure functions
}

// New creates a new RootTracer.
func New(pureFuncs *directive.PureFuncSet) *RootTracer {
	return &RootTracer{pureFuncs: pureFuncs}
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
func (t *RootTracer) FindMutableRoot(recv ssa.Value) ssa.Value {
	return t.trace(recv, make(map[ssa.Value]bool))
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
func (t *RootTracer) FindAllMutableRoots(v ssa.Value) []ssa.Value {
	return t.traceAll(v, make(map[ssa.Value]bool))
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
	return typeutil.IsImmutableReturningBuiltin(fn.Name()) || t.pureFuncs.Contains(fn)
}

// IsImmutableReturningBuiltin checks if a function is a builtin method that returns immutable *gorm.DB.
// Builtin methods (Session, WithContext, Debug, etc.) return immutable *gorm.DB.
// This is used for tracing - only builtin methods have immutable return values.
func (t *RootTracer) IsImmutableReturningBuiltin(fn *ssa.Function) bool {
	if fn == nil {
		return false
	}
	return typeutil.IsImmutableReturningBuiltin(fn.Name())
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
func (t *RootTracer) trace(v ssa.Value, visited map[ssa.Value]bool) ssa.Value {
	if v == nil || visited[v] {
		return nil
	}
	visited[v] = true

	// Check if this is an immutable source
	if t.isImmutableSource(v) {
		return nil
	}

	// Handle gorm method calls
	if call, ok := v.(*ssa.Call); ok {
		return t.traceCall(call, visited)
	}

	// Handle non-call values
	return t.traceNonCall(v, visited)
}

// traceCall handles *ssa.Call values during tracing.
//
// Three cases:
//  1. IIFE (Immediately Invoked Function Expression): trace through returns
//  2. Gorm method call: STOP - this call is the mutable root
//  3. Non-gorm function returning *gorm.DB: treat as root if not pure
//
// Example IIFE tracing:
//
//	_ = func() *gorm.DB { return q.Where("y") }().Find(nil)
//	      │                        │
//	      │                        └─── Inner call is traced
//	      └─── Outer IIFE call triggers traceIIFEReturns
func (t *RootTracer) traceCall(call *ssa.Call, visited map[ssa.Value]bool) ssa.Value {
	// Handle closures (IIFE)
	if mc, ok := call.Call.Value.(*ssa.MakeClosure); ok {
		if closureFn, ok := mc.Fn.(*ssa.Function); ok {
			if root := t.traceIIFEReturns(closureFn, visited); root != nil {
				return root
			}
		}
	}

	callee := call.Call.StaticCallee()
	if callee == nil {
		return nil
	}

	sig := callee.Signature
	if sig == nil || sig.Recv() == nil || !typeutil.IsGormDB(sig.Recv().Type()) {
		// Not a gorm method - if it returns *gorm.DB, treat as root
		if typeutil.IsGormDB(call.Type()) {
			if t.IsImmutableReturningBuiltin(callee) {
				return nil // Builtin pure function returns immutable
			}
			// User-defined pure or non-pure function: treat call as mutable root
			// (user-defined pure functions may return mutable values)
			return call
		}
		return nil
	}

	// This is a gorm method call - THIS CALL is the mutable root
	// Don't trace further - each gorm call result is its own root
	// This implements "variable assignment creates new root" semantics
	return call
}

// traceNonCall handles non-call SSA values during tracing.
// Routes to specialized handlers based on the value type.
func (t *RootTracer) traceNonCall(v ssa.Value, visited map[ssa.Value]bool) ssa.Value {
	switch val := v.(type) {
	case *ssa.Phi:
		// Phi: merge point from conditional branches (if/switch)
		return t.tracePhi(val, visited)

	case *ssa.UnOp:
		// UnOp: unary operations including pointer dereference (*ptr)
		return t.traceUnOp(val, visited)

	case *ssa.ChangeType:
		// ChangeType: type conversion (same underlying type)
		return t.trace(val.X, visited)

	case *ssa.Extract:
		// Extract: extract element from tuple (multi-return)
		return t.trace(val.Tuple, visited)

	case *ssa.FreeVar:
		// FreeVar: captured variable in a closure
		return t.traceFreeVar(val, visited)

	case *ssa.Alloc:
		// Alloc: local variable allocation
		return t.traceAlloc(val, visited)

	default:
		return nil
	}
}

func (t *RootTracer) tracePhi(phi *ssa.Phi, visited map[ssa.Value]bool) ssa.Value {
	for _, edge := range phi.Edges {
		if isNilConst(edge) || visited[edge] {
			continue
		}
		if root := t.trace(edge, visited); root != nil {
			return root
		}
	}
	return nil
}

func (t *RootTracer) traceUnOp(unop *ssa.UnOp, visited map[ssa.Value]bool) ssa.Value {
	if unop.Op == token.MUL {
		// Pointer dereference - trace through the pointer
		return t.tracePointerLoad(unop.X, visited)
	}
	return t.trace(unop.X, visited)
}

func (t *RootTracer) tracePointerLoad(ptr ssa.Value, visited map[ssa.Value]bool) ssa.Value {
	switch p := ptr.(type) {
	case *ssa.FreeVar:
		return t.traceFreeVar(p, visited)
	case *ssa.Alloc:
		return t.traceAlloc(p, visited)
	case *ssa.FieldAddr:
		return t.traceFieldStore(p, visited)
	default:
		return t.trace(ptr, visited)
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
func (t *RootTracer) traceFreeVar(fv *ssa.FreeVar, visited map[ssa.Value]bool) ssa.Value {
	fn := fv.Parent()
	if fn == nil {
		return nil
	}

	// Find the index of this FreeVar in the function's FreeVars list
	idx := -1
	for i, v := range fn.FreeVars {
		if v == fv {
			idx = i
			break
		}
	}
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
			// Found the MakeClosure - trace the corresponding binding
			if idx < len(mc.Bindings) {
				return t.trace(mc.Bindings[idx], visited)
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
func (t *RootTracer) traceAlloc(alloc *ssa.Alloc, visited map[ssa.Value]bool) ssa.Value {
	fn := alloc.Parent()
	if fn == nil {
		return nil
	}

	// Find Store instructions that write to this Alloc
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			store, ok := instr.(*ssa.Store)
			if !ok || store.Addr != alloc {
				continue
			}
			return t.trace(store.Val, visited)
		}
	}
	return nil
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
// This function finds the Store that wrote to the same field.
func (t *RootTracer) traceFieldStore(fa *ssa.FieldAddr, visited map[ssa.Value]bool) ssa.Value {
	fn := fa.Parent()
	if fn == nil {
		return nil
	}

	// Find Store instructions that write to the same field
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			store, ok := instr.(*ssa.Store)
			if !ok {
				continue
			}
			storeFA, ok := store.Addr.(*ssa.FieldAddr)
			if !ok || storeFA.X != fa.X || storeFA.Field != fa.Field {
				continue
			}
			return t.trace(store.Val, visited)
		}
	}
	return nil
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
func (t *RootTracer) traceIIFEReturns(fn *ssa.Function, visited map[ssa.Value]bool) ssa.Value {
	if fn.Signature == nil {
		return nil
	}
	results := fn.Signature.Results()
	if results == nil || results.Len() == 0 || !typeutil.IsGormDB(results.At(0).Type()) {
		return nil
	}

	// Find Return instructions and trace their results
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			ret, ok := instr.(*ssa.Return)
			if !ok || len(ret.Results) == 0 {
				continue
			}
			// Clone visited map to isolate closure's tracing state
			retVisited := cloneVisited(visited)
			if root := t.trace(ret.Results[0], retVisited); root != nil {
				return root
			}
		}
	}
	return nil
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
func (t *RootTracer) traceAll(v ssa.Value, visited map[ssa.Value]bool) []ssa.Value {
	if v == nil || visited[v] {
		return nil
	}
	visited[v] = true

	switch val := v.(type) {
	case *ssa.Phi:
		// Collect roots from ALL Phi edges
		var roots []ssa.Value
		for _, edge := range val.Edges {
			if isNilConst(edge) || visited[edge] {
				continue
			}
			roots = append(roots, t.traceAll(edge, visited)...)
		}
		return roots

	case *ssa.UnOp:
		if val.Op == token.MUL {
			return t.traceAllPointerLoads(val.X, visited)
		}
		return t.traceAll(val.X, visited)

	case *ssa.Alloc:
		return t.traceAllAllocStores(val, visited)

	default:
		// For non-special cases, delegate to single-root trace
		freshVisited := cloneVisited(visited)
		delete(freshVisited, v)
		if root := t.trace(v, freshVisited); root != nil {
			return []ssa.Value{root}
		}
		return nil
	}
}

func (t *RootTracer) traceAllPointerLoads(ptr ssa.Value, visited map[ssa.Value]bool) []ssa.Value {
	switch p := ptr.(type) {
	case *ssa.Alloc:
		return t.traceAllAllocStores(p, visited)
	case *ssa.Phi:
		var roots []ssa.Value
		for _, edge := range p.Edges {
			if isNilConst(edge) || visited[edge] {
				continue
			}
			roots = append(roots, t.traceAll(edge, visited)...)
		}
		return roots
	default:
		return t.traceAll(ptr, visited)
	}
}

func (t *RootTracer) traceAllAllocStores(alloc *ssa.Alloc, visited map[ssa.Value]bool) []ssa.Value {
	fn := alloc.Parent()
	if fn == nil {
		return nil
	}

	var roots []ssa.Value
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			store, ok := instr.(*ssa.Store)
			if !ok || store.Addr != alloc {
				continue
			}
			roots = append(roots, t.traceAll(store.Val, visited)...)
		}
	}
	return roots
}

// isImmutableSource checks if a value is an immutable source (no mutable root).
//
// Immutable sources:
//   - Parameter: function arguments are treated as fresh values
//   - Const: constant values (especially nil)
//   - Builtin pure function call: returns immutable *gorm.DB (e.g., Session())
//
// Note: User-defined pure functions are NOT immutable sources - they may return
// mutable values. Only builtin pure methods guarantee immutable return values.
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
		// Only builtin pure function calls return immutable values
		// User-defined pure functions may return mutable values
		callee := val.Call.StaticCallee()
		return t.IsImmutableReturningBuiltin(callee)
	default:
		return false
	}
}

// =============================================================================
// Helper Functions
// =============================================================================

// isNilConst checks if a value is a nil constant.
func isNilConst(v ssa.Value) bool {
	c, ok := v.(*ssa.Const)
	return ok && c.Value == nil
}

// cloneVisited creates a copy of the visited map.
// Used to isolate tracing state when entering closures.
func cloneVisited(visited map[ssa.Value]bool) map[ssa.Value]bool {
	clone := make(map[ssa.Value]bool, len(visited))
	for k, v := range visited {
		clone[k] = v
	}
	return clone
}

// ClosureCapturesGormDB checks if a closure captures *gorm.DB values.
//
// Used to determine if a closure needs recursive analysis. A closure
// that doesn't capture *gorm.DB can be skipped for efficiency.
//
// Checks both direct *gorm.DB and **gorm.DB (pointer to pointer).
func ClosureCapturesGormDB(mc *ssa.MakeClosure) bool {
	for _, binding := range mc.Bindings {
		t := binding.Type()
		if typeutil.IsGormDB(t) {
			return true
		}
		// Check **gorm.DB (pointer to pointer, from captured variables)
		if ptr, ok := t.(*types.Pointer); ok {
			if typeutil.IsGormDB(ptr.Elem()) {
				return true
			}
		}
	}
	return false
}
