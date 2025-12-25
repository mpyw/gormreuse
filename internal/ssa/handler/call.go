// Package handler provides SSA instruction handlers for gormreuse.
//
// # Overview
//
// This package provides handlers for different SSA instruction types that
// can involve *gorm.DB values. Each handler checks for pollution and records
// usage for later violation detection.
//
// # Instruction Types Handled
//
//	┌─────────────────────────────────────────────────────────────────────────┐
//	│  Instruction Type  │  Handler          │  Purpose                       │
//	├─────────────────────────────────────────────────────────────────────────┤
//	│  *ssa.Call         │  CallHandler      │  Method calls, function calls  │
//	│  *ssa.Go           │  GoHandler        │  go func() { ... }             │
//	│  *ssa.Defer        │  DeferHandler     │  defer func() { ... }          │
//	│  *ssa.Send         │  SendHandler      │  ch <- db (channel send)       │
//	│  *ssa.Store        │  StoreHandler     │  slice[i] = db (slice elem)    │
//	│  *ssa.MapUpdate    │  MapUpdateHandler │  map[k] = db (map storage)     │
//	│  *ssa.MakeInterface│  MakeInterfaceHandler │ interface{}(db)            │
//	└─────────────────────────────────────────────────────────────────────────┘
//
// # Type Switch Dispatch
//
// The Dispatch function uses type switch for O(1) dispatch to handlers:
//
//	switch i := instr.(type) {
//	case *ssa.Call:    (&CallHandler{}).Handle(i, ctx)
//	case *ssa.Go:      (&GoHandler{}).Handle(i, ctx)
//	...
//	}
//
// This is more efficient than reflection-based dispatch for a small number
// of known types.
package handler

import (
	"go/token"
	"strings"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/ssa/cfg"
	"github.com/mpyw/gormreuse/internal/ssa/pollution"
	"github.com/mpyw/gormreuse/internal/ssa/tracer"
	"github.com/mpyw/gormreuse/internal/typeutil"
)

// Context provides shared context for instruction handlers.
//
// This struct is created per-function and passed to all handlers during
// the instruction processing pass.
type Context struct {
	Tracker    *pollution.Tracker // Records usage and detects violations
	RootTracer *tracer.RootTracer // Traces values to mutable roots
	CFG        *cfg.Analyzer      // Control flow graph analysis
	LoopInfo   *cfg.LoopInfo      // Loop detection results for current function
	CurrentFn  *ssa.Function      // The function being analyzed
}

// CallHandler handles *ssa.Call instructions.
//
// This is the most complex handler, covering:
//   - Direct gorm method calls: q.Find(nil)
//   - Bound method calls: find := q.Find; find(nil)
//   - Function calls with *gorm.DB arguments
//
// # Uniform Processing
//
// Every gorm chain method use is processed uniformly:
//   - First use from a root: OK, marks root polluted
//   - Second+ use from same root: VIOLATION at that call site
//
// There's no distinction between "terminal" and "non-terminal" methods.
// All uses are recorded, and violations are detected via CFG reachability.
type CallHandler struct{}

// isAssignment checks if a call result is assigned to create a new root.
// Returns true if the call is used in an assignment (Store to Alloc or Phi).
//
// KEY INSIGHT: All Phi nodes (loop-header, loop-internal, and non-loop) represent assignments:
//   - They merge control flow and create new SSA values
//   - Assignment does NOT pollute the original root
//   - Only actual method calls (non-assignment) pollute
//
// Example patterns:
//
//	q = q.Where("x")  // Assignment - no pollution
//	q.Find(nil)       // Actual use - pollutes q
//
// Loop patterns:
//
//	for { q = q.Where() }         // Loop-header Phi - assignment, no pollution
//	for { if x { q = q.Where() }} // Loop-internal Phi - assignment, no pollution
//	for { q.Find(nil) }           // Actual use in loop - pollutes q
//
// This works recursively for arbitrary nesting depths (for-if-if-if-for-for-if-if etc.)
// because SSA naturally represents all control flow as Phi nodes.
func isAssignment(call *ssa.Call, ctx *Context) bool {
	if call.Referrers() == nil {
		return false
	}

	for _, user := range *call.Referrers() {
		// =====================================================================
		// Pattern 1: Phi Node (Control Flow Merge) - ALWAYS Assignment
		// =====================================================================
		//
		// Why ALL Phi nodes represent assignments:
		//
		// In SSA, a Phi node merges values from different control flow paths.
		// The Phi node itself CREATES a new SSA value (the merge result).
		// This merge is semantically equivalent to an assignment.
		//
		// Example 1: Simple if-else
		//
		//   Go code:
		//     q := db.Where("base")
		//     if cond {
		//         q = q.Where("a")  // ← Call: t2 = t1.Where("a")
		//     }
		//     q.Find(nil)  // ← Uses merged value
		//
		//   SSA:
		//     Block 0:
		//       t1 = db.Where("base")
		//     Block 1 (if.then):
		//       t2 = t1.Where("a")     ← This call
		//                  ↓
		//     Block 2 (if.done):
		//       t3 = phi [no-if: t1, if: t2]  ← t2 is used by Phi
		//                              ^^
		//                              t2 is in an ASSIGNMENT context
		//       t4 = t3.Find(nil)
		//
		//   Because t2 is used by a Phi node to create t3 (a new variable),
		//   the call t2 = t1.Where("a") is an ASSIGNMENT, not a polluting use.
		//
		// Example 2: Loop with conditional update
		//
		//   Go code:
		//     for _, item := range items {
		//         if item%2 == 0 {
		//             q = q.Where("even")  // ← Call: t5 = t3.Where("even")
		//         }
		//     }
		//
		//   SSA:
		//     Block 1 (loop header):
		//       t3 = phi [entry: t1, back: t6]
		//     Block 2 (if.then):
		//       t5 = t3.Where("even")     ← This call
		//                  ↓
		//     Block 3 (if.done):
		//       t6 = phi [no-if: t3, if: t5]  ← t5 is used by Phi
		//                              ^^
		//                              t5 is in an ASSIGNMENT context
		//
		//   Again, t5 is used to create t6 via Phi, so it's an assignment.
		//
		// Key Insight:
		//   - Phi node = SSA's way of representing variable assignment in control flow
		//   - If a call result flows into a Phi, it's being "assigned" to the merged variable
		//   - Therefore, the call itself is an assignment, NOT a polluting use
		//
		if _, ok := user.(*ssa.Phi); ok {
			return true
		}

		// =====================================================================
		// Pattern 2: Store to Alloc (Direct Variable Assignment)
		// =====================================================================
		//
		// What is Store to Alloc?
		//
		// In SSA, local variables are represented as:
		//   1. Alloc: Allocates memory for the variable
		//   2. Store: Writes a value to that memory location
		//
		// Example:
		//
		//   Go code:
		//     var q *gorm.DB
		//     q = db.Where("x")  // ← This is a Store to Alloc
		//     q.Find(nil)
		//
		//   SSA:
		//     Block 0:
		//       t1 = Alloc *gorm.DB        ← Allocate space for variable 'q'
		//       t2 = db.Where("x")         ← This call
		//       Store t1 <- t2             ← Store t2 into t1 (assignment!)
		//                   ^^                           ^^
		//                   call result                  Alloc
		//       t3 = UnOp * t1             ← Load from t1
		//       t4 = t3.Find(nil)
		//
		// Pattern breakdown:
		//   - store.Addr is t1 (*ssa.Alloc) - the variable location
		//   - store.Val is t2 (the call result) - the value being stored
		//   - This represents: q = db.Where("x")
		//
		// Why this is an assignment:
		//   - The call result (t2) is being stored into a variable (t1)
		//   - This is a direct variable assignment in Go source code
		//   - The call is not being used directly (e.g., not chained immediately)
		//   - Therefore, it's an assignment, NOT a polluting use
		//
		// What qualifies:
		//   - store.Addr must be *ssa.Alloc (not *ssa.FieldAddr, *ssa.IndexAddr, etc.)
		//   - Because only Alloc represents local variable storage
		//   - FieldAddr would be: struct.field = value
		//   - IndexAddr would be: array[i] = value
		//   - We only consider local variable assignment as non-polluting
		//
		if store, ok := user.(*ssa.Store); ok {
			if _, ok := store.Addr.(*ssa.Alloc); ok {
				return true
			}
		}
	}

	return false
}

// Handle processes a Call instruction and tracks *gorm.DB pollution.
//
// This is the main entry point for analyzing method calls on *gorm.DB values.
// It determines whether a call is:
//  1. An assignment (q = q.Where(...)) - creates new root, doesn't pollute
//  2. A pure method (q.Session(...)) - checks pollution but doesn't pollute
//  3. An actual use (q.Find(...)) - pollutes the root, potential violation
//
// # Processing Steps
//
// ## Step 1: Check if call is inside a loop
//
//	isInLoop := ctx.LoopInfo.IsInLoop(call.Block())
//
// Why: Calls inside loops with external roots are immediately violations,
// because the loop body will execute multiple times, causing multiple uses.
//
// ## Step 2: Check function call pollution
//
//	h.checkFunctionCallPollution(call, ctx)
//
// Why: Non-gorm functions that receive *gorm.DB as arguments might use it.
// We conservatively mark those roots as polluted.
//
// Example:
//
//	func helper(db *gorm.DB) { db.Find(nil) }
//	helper(q)  // ← q is now polluted
//
// ## Step 3: Handle bound method calls (method values)
//
//	if mc, ok := call.Call.Value.(*ssa.MakeClosure); ok {
//	    h.processBoundMethodCall(call, mc, isInLoop, ctx)
//	    return
//	}
//
// Why: Method values like `find := q.Find` create closures with the receiver
// bound. We need to extract the receiver and track it.
//
// Example:
//
//	find := q.Find  // ← MakeClosure with q in Bindings[0]
//	find(nil)       // ← Call with MakeClosure as Value
//
// ## Step 4: Filter to only gorm method calls
//
//	if !h.isGormDBMethodCall(call) {
//	    return
//	}
//
// Why: We only care about methods called on *gorm.DB. Regular function calls
// were already handled in Step 2.
//
// ## Step 5: Extract method information
//
//	callee := call.Call.StaticCallee()
//	methodName := callee.Name()
//	isImmutableReturning := typeutil.IsImmutableReturningBuiltin(methodName)
//	recv := call.Call.Args[0]
//
// Args[0] is the receiver for method calls in SSA.
//
// ## Step 6: Find the mutable root
//
//	root := ctx.RootTracer.FindMutableRoot(recv, ctx.LoopInfo)
//	if root == nil {
//	    return // Immutable source (e.g., from Session())
//	}
//
// Why: We trace back to find the original mutable *gorm.DB that this call
// operates on. If there's no mutable root, the value is immutable and safe.
//
// ## Step 7: Classify and record the usage
//
//	if isImmutableReturning {
//	    ctx.Tracker.RecordPureUse(root, call.Block(), call.Pos())
//	} else if isAssignment(call, ctx) {
//	    ctx.Tracker.RecordAssignment(root, call.Block(), call.Pos())
//	} else {
//	    ctx.Tracker.ProcessBranch(root, call.Block(), call.Pos())
//	    if isInLoop && ctx.CFG.IsDefinedOutsideLoop(root, ctx.LoopInfo) {
//	        ctx.Tracker.AddViolationWithRoot(call.Pos(), root)
//	    }
//	}
//
// Three cases:
//
//	A) Pure method (Session, WithContext, Debug):
//	   - Checks if root is already polluted (violation if yes)
//	   - Doesn't pollute the root itself
//
//	B) Assignment (q = q.Where(...)):
//	   - Records the assignment but doesn't mark root as polluted
//	   - The new value created by the assignment is a separate root
//
//	C) Actual use (q.Find(...)):
//	   - Marks the root as polluted (first branch consumed)
//	   - If in loop with external root: immediate violation
//
// ## Step 8: Check alternative roots from Phi nodes
//
//	allRoots := ctx.RootTracer.FindAllMutableRoots(recv, ctx.LoopInfo)
//	for _, r := range allRoots {
//	    if r == root { continue }
//	    if ctx.Tracker.IsPollutedAt(r, call.Block()) {
//	        ctx.Tracker.AddViolationWithRoot(call.Pos(), r)
//	    }
//	}
//
// Why: When the receiver comes from a Phi node (conditional merge), there
// might be multiple possible roots. We need to check if ANY of them is
// already polluted.
//
// Example:
//
//	var q *gorm.DB
//	if cond {
//	    q = q1  // Polluted
//	} else {
//	    q = q2  // Clean
//	}
//	q.Find(nil)  // ← Must check BOTH q1 and q2
func (h *CallHandler) Handle(call *ssa.Call, ctx *Context) {
	isInLoop := ctx.LoopInfo.IsInLoop(call.Block())

	// Check function call pollution (non-gorm-method calls with *gorm.DB args)
	h.checkFunctionCallPollution(call, ctx)

	// Check bound method calls (method values)
	if mc, ok := call.Call.Value.(*ssa.MakeClosure); ok {
		h.processBoundMethodCall(call, mc, isInLoop, ctx)
		return
	}

	// Check gorm method calls
	if !h.isGormDBMethodCall(call) {
		return
	}

	callee := call.Call.StaticCallee()
	if callee == nil {
		return
	}

	methodName := callee.Name()
	isImmutableReturning := typeutil.IsImmutableReturningBuiltin(methodName)

	// Get receiver
	if len(call.Call.Args) == 0 {
		return
	}
	recv := call.Call.Args[0]

	// Find mutable root
	root := ctx.RootTracer.FindMutableRoot(recv, ctx.LoopInfo)
	if root == nil {
		return // Immutable source
	}

	// KEY CHANGE: Process ALL calls uniformly (no isTerminal skip)
	// Record usage (violations detected later in DetectViolations)
	if isImmutableReturning {
		// Pure methods check for pollution but don't pollute
		ctx.Tracker.RecordPureUse(root, call.Block(), call.Pos())
	} else if isAssignment(call, ctx) {
		// Assignment creates new root - record but doesn't pollute
		ctx.Tracker.RecordAssignment(root, call.Block(), call.Pos())
	} else {
		// Actual use - pollutes the root
		ctx.Tracker.ProcessBranch(root, call.Block(), call.Pos())

		// Loop with external root - immediate violation (only for non-pure methods)
		if isInLoop && ctx.CFG.IsDefinedOutsideLoop(root, ctx.LoopInfo) {
			ctx.Tracker.AddViolationWithRoot(call.Pos(), root)
		}
	}

	// Check ALL possible roots for phi nodes
	allRoots := ctx.RootTracer.FindAllMutableRoots(recv, ctx.LoopInfo)
	for _, r := range allRoots {
		if r == root {
			continue
		}
		if ctx.Tracker.IsPollutedAt(r, call.Block()) {
			ctx.Tracker.AddViolationWithRoot(call.Pos(), r)
		}
	}
}

// processBoundMethodCall handles calls through method values (bound method calls).
//
// # What are Bound Method Calls?
//
// In Go, you can create a "method value" by referencing a method without calling it:
//
//	q := db.Where("x")
//	find := q.Find  // ← Method value: creates closure with q as receiver
//	find(nil)       // ← Bound method call: calls q.Find(nil)
//
// This is called a "bound method call" because the receiver (q) is "bound" to the
// method at the time the method value is created, not when it's called.
//
// # SSA Representation
//
// In SSA, method values are represented using *ssa.MakeClosure:
//
//	Go code:
//	  q := db.Where("x")
//	  find := q.Find  // ← Method value
//	  find(nil)       // ← Call
//
//	SSA:
//	  Block 0:
//	    t1 = db.Where("x")
//	    t2 = MakeClosure(Find$bound, [t1])  ← Method value
//	                                   ^^^^
//	                                   Bindings[0] = receiver
//	    t3 = Call t2(nil)                    ← Bound method call
//	         ^^^^^^^
//	         Call.Value = *ssa.MakeClosure
//
// Key components:
//   - MakeClosure.Fn.Name() = "Find$bound" (method name + "$bound" suffix)
//   - MakeClosure.Bindings[0] = receiver value (q in this example)
//   - Call.Value = the MakeClosure instruction (not a function)
//
// # Why Special Handling?
//
// Normal method calls have the receiver in Args[0]:
//
//	q.Find(nil)  →  Call Find with Args=[q, nil]
//	                               ^^^^^
//	                               receiver in Args[0]
//
// But bound method calls have the receiver in MakeClosure.Bindings[0]:
//
//	find := q.Find  →  MakeClosure(Find$bound, [q])
//	find(nil)       →  Call MakeClosure with Args=[nil]
//	                   ^^^^^^^^^^^^^^^^^
//	                   receiver NOT in Args! Must extract from Bindings[0]
//
// We need to:
//  1. Extract the receiver from mc.Bindings[0]
//  2. Strip the "$bound" suffix from method name
//  3. Track pollution just like normal method calls
//
// # Processing Steps
//
// ## Step 1: Validate bindings exist
//
//	if len(mc.Bindings) == 0 {
//	    return
//	}
//
// Why: MakeClosure always has the receiver in Bindings[0] for method values.
// If there are no bindings, this is not a method value (might be a regular closure).
//
// ## Step 2: Extract receiver from bindings
//
//	recv := mc.Bindings[0]
//	if !typeutil.IsGormDB(recv.Type()) {
//	    return
//	}
//
// Why: For method values, the receiver is stored in Bindings[0].
// We only care about *gorm.DB receivers.
//
// Example:
//
//	find := q.Find  →  MakeClosure(Find$bound, [q])
//	                                           ^^^
//	                                           recv = q
//
// ## Step 3: Extract method name and check if immutable-returning
//
//	methodName := strings.TrimSuffix(mc.Fn.Name(), "$bound")
//	isImmutableReturning := typeutil.IsImmutableReturningBuiltin(methodName)
//
// Why: SSA adds "$bound" suffix to bound method names.
// We need to remove it to identify the actual method (Find, Session, etc.).
//
// Example:
//
//	mc.Fn.Name() = "Find$bound"
//	methodName = "Find"  (after TrimSuffix)
//
// ## Step 4: Find mutable root
//
//	root := ctx.RootTracer.FindMutableRoot(recv, ctx.LoopInfo)
//	if root == nil {
//	    return
//	}
//
// Why: We need to trace back to find the original mutable *gorm.DB that
// the receiver (q) came from. If there's no mutable root, the value is
// immutable and safe.
//
// ## Step 5: Record usage based on method type
//
//	if isImmutableReturning {
//	    ctx.Tracker.RecordPureUse(root, call.Block(), call.Pos())
//	} else {
//	    ctx.Tracker.ProcessBranch(root, call.Block(), call.Pos())
//	    if isInLoop && ctx.CFG.IsDefinedOutsideLoop(root, ctx.LoopInfo) {
//	        ctx.Tracker.AddViolationWithRoot(call.Pos(), root)
//	    }
//	}
//
// Two cases:
//
//	A) Pure method (Session, WithContext, Debug):
//	   - session := q.Session; session(&gorm.Session{})
//	   - Checks if root is already polluted (violation if yes)
//	   - Doesn't pollute the root itself
//
//	B) Non-pure method (Find, Where, Count):
//	   - find := q.Find; find(nil)
//	   - Marks the root as polluted (first branch consumed)
//	   - If in loop with external root: immediate violation
//
// # Complete Example
//
//	Go code:
//	  q := db.Where("x")
//	  find := q.Find  // ← Create method value
//	  find(nil)       // ← Bound method call (first use)
//	  q.Count(nil)    // ← Second use - VIOLATION!
//
//	SSA:
//	  Block 0:
//	    t1 = db.Where("x")
//	    t2 = MakeClosure(Find$bound, [t1])  ← Create method value
//	                                   ^^^^
//	                                   Bindings[0] = t1 (receiver)
//	    t3 = Call t2(nil)                    ← Call bound method
//	         ^^^^^^^
//	         processBoundMethodCall extracts t1 from mc.Bindings[0]
//	         → Marks t1 as polluted (first use)
//	    t4 = t1.Count(nil)                   ← Second use of t1
//	         → VIOLATION! (t1 already polluted)
func (h *CallHandler) processBoundMethodCall(call *ssa.Call, mc *ssa.MakeClosure, isInLoop bool, ctx *Context) {
	if len(mc.Bindings) == 0 {
		return
	}

	recv := mc.Bindings[0]
	if !typeutil.IsGormDB(recv.Type()) {
		return
	}

	methodName := strings.TrimSuffix(mc.Fn.Name(), "$bound")
	isImmutableReturning := typeutil.IsImmutableReturningBuiltin(methodName)

	root := ctx.RootTracer.FindMutableRoot(recv, ctx.LoopInfo)
	if root == nil {
		return
	}

	// Record usage (violations detected later)
	if isImmutableReturning {
		// Pure methods check for pollution but don't pollute
		ctx.Tracker.RecordPureUse(root, call.Block(), call.Pos())
	} else {
		// Non-pure methods pollute the root
		ctx.Tracker.ProcessBranch(root, call.Block(), call.Pos())

		// Loop with external root - immediate violation (only for non-pure methods)
		if isInLoop && ctx.CFG.IsDefinedOutsideLoop(root, ctx.LoopInfo) {
			ctx.Tracker.AddViolationWithRoot(call.Pos(), root)
		}
	}
}

// checkFunctionCallPollution handles non-gorm-method calls that take *gorm.DB arguments.
//
// # Purpose
//
// This function conservatively marks *gorm.DB values as "polluted" when they are
// passed to regular functions (not gorm methods). We can't statically analyze
// what the function does with the value, so we assume it might use it.
//
// # What are "Function Calls" vs "Method Calls"?
//
// Method call: receiver.Method(args)
//
//	q.Find(nil)         ← Method call on *gorm.DB
//	q.Where("x")        ← Method call on *gorm.DB
//
// Function call: function(args)
//
//	helper(q)           ← Function call with *gorm.DB argument
//	doSomething(q, x)   ← Function call with *gorm.DB argument
//
// # Why Mark as Polluted?
//
// When a *gorm.DB value is passed to a function, we don't know what happens inside:
//
//	func helper(db *gorm.DB) {
//	    db.Find(nil)  // ← Function MIGHT use the value
//	}
//
//	q := db.Where("x")
//	helper(q)         // ← q is now "used" by helper
//	q.Count(nil)      // ← Second use - VIOLATION!
//
// We conservatively assume the function pollutes the value unless it's marked
// with //gormreuse:pure directive.
//
// # Pure Functions Exception
//
// Functions marked with //gormreuse:pure don't pollute:
//
//	//gormreuse:pure
//	func helper(db *gorm.DB) *gorm.DB {
//	    return db  // Just returns the value, doesn't use it
//	}
//
//	q := db.Where("x")
//	helper(q)         // ← q is NOT polluted (pure function)
//	q.Count(nil)      // ← First use - OK
//
// # Processing Steps
//
// ## Step 1: Get static callee and check if it's a gorm method
//
//	callee := call.Call.StaticCallee()
//
//	if callee != nil {
//	    sig := callee.Signature
//	    if sig != nil && sig.Recv() != nil && typeutil.IsGormDB(sig.Recv().Type()) {
//	        return // This is a gorm method, not a function call
//	    }
//	    ...
//	}
//
// Why: We need to distinguish between:
//   - q.Find(nil)       ← gorm method (has receiver) - handled in Handle()
//   - helper(q)         ← function call (no receiver) - handled here
//
// StaticCallee returns the function being called (if statically known).
// If sig.Recv() != nil, this is a method call, not a function call.
//
// ## Step 2: Check if function is marked as pure
//
//	if ctx.RootTracer.IsPureFunction(callee) {
//	    return
//	}
//
// Why: Pure functions (marked with //gormreuse:pure) don't pollute arguments.
// The RootTracer maintains a set of pure functions based on directives.
//
// ## Step 3: Check each argument for *gorm.DB type
//
//	for _, arg := range call.Call.Args {
//	    if !typeutil.IsGormDB(arg.Type()) {
//	        continue
//	    }
//	    ...
//	}
//
// Why: We only care about *gorm.DB arguments. Other arguments don't affect
// our pollution tracking.
//
// Example:
//
//	helper(q, 42, "str")  ← Only check 'q' (if it's *gorm.DB)
//
// ## Step 4: Find mutable root for *gorm.DB arguments
//
//	root := ctx.RootTracer.FindMutableRoot(arg, ctx.LoopInfo)
//	if root == nil {
//	    continue
//	}
//
// Why: We need to trace back to find the original mutable *gorm.DB.
// If there's no mutable root, the value is immutable and safe.
//
// ## Step 5: Mark root as polluted
//
//	ctx.Tracker.MarkPolluted(root, call.Block(), call.Pos())
//
// Why: Since we can't analyze what the function does, we conservatively
// assume it uses the *gorm.DB value. This marks the root as "consumed",
// and any subsequent use will be a violation.
//
// # Complete Examples
//
// ## Example 1: Non-pure function pollutes
//
//	Go code:
//	  func helper(db *gorm.DB) { db.Find(nil) }
//
//	  q := db.Where("x")
//	  helper(q)         // ← checkFunctionCallPollution marks q as polluted
//	  q.Count(nil)      // ← VIOLATION! (q already polluted)
//
//	SSA:
//	  Block 0:
//	    t1 = db.Where("x")
//	    t2 = Call helper(t1)
//	         ^^^^^^^^^^^^^^
//	         checkFunctionCallPollution:
//	           - Args[0] = t1 (*gorm.DB)
//	           - root = FindMutableRoot(t1) = t1
//	           - MarkPolluted(t1)  ← t1 is now polluted
//	    t3 = t1.Count(nil)
//	         → VIOLATION! (t1 already polluted)
//
// ## Example 2: Pure function doesn't pollute
//
//	Go code:
//	  //gormreuse:pure
//	  func helper(db *gorm.DB) *gorm.DB { return db }
//
//	  q := db.Where("x")
//	  helper(q)         // ← NOT polluted (pure function)
//	  q.Count(nil)      // ← First use - OK
//
//	SSA:
//	  Block 0:
//	    t1 = db.Where("x")
//	    t2 = Call helper(t1)
//	         ^^^^^^^^^^^^^^
//	         checkFunctionCallPollution:
//	           - IsPureFunction(helper) = true
//	           - return early (don't pollute)
//	    t3 = t1.Count(nil)
//	         → OK (t1 not polluted by helper)
//
// ## Example 3: Multiple *gorm.DB arguments
//
//	Go code:
//	  func combine(db1, db2 *gorm.DB, x int) { ... }
//
//	  q1 := db.Where("a")
//	  q2 := db.Where("b")
//	  combine(q1, q2, 42)  // ← Both q1 and q2 marked as polluted
//
//	SSA:
//	  Block 0:
//	    t1 = db.Where("a")
//	    t2 = db.Where("b")
//	    t3 = Call combine(t1, t2, 42)
//	         ^^^^^^^^^^^^^^^^^^^^
//	         checkFunctionCallPollution:
//	           - Args[0] = t1 (*gorm.DB) → MarkPolluted(t1)
//	           - Args[1] = t2 (*gorm.DB) → MarkPolluted(t2)
//	           - Args[2] = 42 (int) → skip
func (h *CallHandler) checkFunctionCallPollution(call *ssa.Call, ctx *Context) {
	callee := call.Call.StaticCallee()

	if callee != nil {
		sig := callee.Signature
		if sig != nil && sig.Recv() != nil && typeutil.IsGormDB(sig.Recv().Type()) {
			return // This is a gorm method, not a function call
		}

		if ctx.RootTracer.IsPureFunction(callee) {
			return
		}
	}

	for _, arg := range call.Call.Args {
		if !typeutil.IsGormDB(arg.Type()) {
			continue
		}

		root := ctx.RootTracer.FindMutableRoot(arg, ctx.LoopInfo)
		if root == nil {
			continue
		}

		// Mark polluted (function may use the value)
		ctx.Tracker.MarkPolluted(root, call.Block(), call.Pos())
	}
}

func (h *CallHandler) isGormDBMethodCall(call *ssa.Call) bool {
	callee := call.Call.StaticCallee()
	if callee == nil {
		return false
	}

	sig := callee.Signature
	if sig == nil || sig.Recv() == nil {
		return false
	}

	return typeutil.IsGormDB(sig.Recv().Type())
}

// GoHandler handles *ssa.Go instructions.
type GoHandler struct{}

// Handle processes a Go instruction.
func (h *GoHandler) Handle(g *ssa.Go, ctx *Context) {
	block := g.Block()
	processGormDBCallCommonWith(&g.Call, g.Pos(), ctx, func(root ssa.Value) bool {
		return ctx.Tracker.IsPollutedAt(root, block)
	})
}

// DeferHandler handles *ssa.Defer instructions.
type DeferHandler struct{}

// Handle processes a Defer instruction.
// Defer uses IsPollutedAnywhere because it executes at function exit.
func (h *DeferHandler) Handle(d *ssa.Defer, ctx *Context) {
	processGormDBCallCommonWith(&d.Call, d.Pos(), ctx, func(root ssa.Value) bool {
		return ctx.Tracker.IsPollutedAnywhere(root)
	})
}

// SendHandler handles *ssa.Send instructions.
type SendHandler struct{}

// Handle marks *gorm.DB sent to channels as polluted.
func (h *SendHandler) Handle(send *ssa.Send, ctx *Context) {
	if !typeutil.IsGormDB(send.X.Type()) {
		return
	}

	root := ctx.RootTracer.FindMutableRoot(send.X, ctx.LoopInfo)
	if root == nil {
		return
	}

	ctx.Tracker.MarkPolluted(root, send.Block(), send.Pos())
}

// StoreHandler handles *ssa.Store instructions.
type StoreHandler struct{}

// Handle marks *gorm.DB stored to slice elements as polluted.
func (h *StoreHandler) Handle(store *ssa.Store, ctx *Context) {
	if !typeutil.IsGormDB(store.Val.Type()) {
		return
	}

	// Only stores to IndexAddr (slice element)
	if _, ok := store.Addr.(*ssa.IndexAddr); !ok {
		return
	}

	root := ctx.RootTracer.FindMutableRoot(store.Val, ctx.LoopInfo)
	if root == nil {
		return
	}

	ctx.Tracker.MarkPolluted(root, store.Block(), store.Pos())
}

// MapUpdateHandler handles *ssa.MapUpdate instructions.
type MapUpdateHandler struct{}

// Handle marks *gorm.DB stored in maps as polluted.
func (h *MapUpdateHandler) Handle(mapUpdate *ssa.MapUpdate, ctx *Context) {
	if !typeutil.IsGormDB(mapUpdate.Value.Type()) {
		return
	}

	root := ctx.RootTracer.FindMutableRoot(mapUpdate.Value, ctx.LoopInfo)
	if root == nil {
		return
	}

	ctx.Tracker.MarkPolluted(root, mapUpdate.Block(), mapUpdate.Pos())
}

// MakeInterfaceHandler handles *ssa.MakeInterface instructions.
type MakeInterfaceHandler struct{}

// Handle marks *gorm.DB converted to interface as polluted.
func (h *MakeInterfaceHandler) Handle(mi *ssa.MakeInterface, ctx *Context) {
	if !typeutil.IsGormDB(mi.X.Type()) {
		return
	}

	root := ctx.RootTracer.FindMutableRoot(mi.X, ctx.LoopInfo)
	if root == nil {
		return
	}

	ctx.Tracker.MarkPolluted(root, mi.Block(), mi.Pos())
}

// pollutionChecker is a function that checks if a root is polluted.
type pollutionChecker func(root ssa.Value) bool

// processGormDBCallCommonWith processes gorm calls with a custom pollution checker.
func processGormDBCallCommonWith(callCommon *ssa.CallCommon, pos token.Pos, ctx *Context, isPolluted pollutionChecker) {
	callee := callCommon.StaticCallee()
	if callee == nil {
		return
	}

	sig := callee.Signature

	// Method call on *gorm.DB
	if sig != nil && sig.Recv() != nil && typeutil.IsGormDB(sig.Recv().Type()) {
		if len(callCommon.Args) == 0 {
			return
		}
		recv := callCommon.Args[0]

		root := ctx.RootTracer.FindMutableRoot(recv, ctx.LoopInfo)
		if root == nil {
			return
		}

		if isPolluted(root) {
			ctx.Tracker.AddViolationWithRoot(pos, root)
		}
		return
	}

	// Function call with *gorm.DB arguments
	for _, arg := range callCommon.Args {
		if !typeutil.IsGormDB(arg.Type()) {
			continue
		}

		root := ctx.RootTracer.FindMutableRoot(arg, ctx.LoopInfo)
		if root == nil {
			continue
		}

		if isPolluted(root) {
			ctx.Tracker.AddViolationWithRoot(pos, root)
		}
	}
}

// Dispatch dispatches an instruction to the appropriate handler.
//
// # Purpose
//
// This function routes SSA instructions to their specialized handlers based on
// instruction type. It uses a type switch for O(1) dispatch efficiency.
//
// # Why Type Switch?
//
// SSA instructions come in many types (*ssa.Call, *ssa.Go, *ssa.Defer, etc.).
// We need to route each instruction to the appropriate handler.
//
// Two common approaches:
//
//	A) Type switch (used here):
//	   - O(1) dispatch (constant time)
//	   - Compiler optimizes to jump table
//	   - Easy to read and maintain
//	   - Best for small number of known types
//
//	B) Reflection-based dispatch (NOT used):
//	   - O(n) dispatch (linear time)
//	   - Requires runtime type inspection
//	   - More flexible but slower
//	   - Better for plugin-style extensibility
//
// Since we have a fixed, small set of instruction types (7 handlers), type
// switch is the most efficient and idiomatic Go approach.
//
// # Handler Dispatch Table
//
//	┌─────────────────────────────────────────────────────────────────────────┐
//	│  Instruction Type  │  Handler              │  What it detects          │
//	├─────────────────────────────────────────────────────────────────────────┤
//	│  *ssa.Call         │  CallHandler          │  Method/function calls    │
//	│  *ssa.Go           │  GoHandler            │  Goroutine launches       │
//	│  *ssa.Send         │  SendHandler          │  Channel send             │
//	│  *ssa.Store        │  StoreHandler         │  Slice element store      │
//	│  *ssa.MapUpdate    │  MapUpdateHandler     │  Map value store          │
//	│  *ssa.MakeInterface│  MakeInterfaceHandler │  Interface conversion     │
//	└─────────────────────────────────────────────────────────────────────────┘
//
// Note: *ssa.Defer is NOT handled here - see DispatchDefer below.
//
// # Why Each Instruction Type?
//
// ## *ssa.Call - Method and function calls
//
//	q.Find(nil)         ← Most important! Direct gorm method calls
//	helper(q)           ← Function calls that might use *gorm.DB
//	find := q.Find      ← Method value creation (MakeClosure)
//	find(nil)           ← Bound method call
//
// ## *ssa.Go - Goroutine launches
//
//	go func() { q.Find(nil) }()  ← Async execution, potential race
//	go q.Find(nil)               ← Direct goroutine with method call
//
// Why track: Goroutines can cause concurrent access to mutable *gorm.DB.
//
// ## *ssa.Send - Channel send
//
//	ch <- q             ← Sends *gorm.DB to channel
//
// Why track: Once sent to a channel, the value might be used elsewhere
// (another goroutine), so we conservatively mark it as polluted.
//
// ## *ssa.Store - Slice element store
//
//	slice[0] = q        ← Stores *gorm.DB to slice element
//
// Why track: Slice elements can be accessed from multiple places, so we
// conservatively mark the value as polluted.
//
// Note: Store to Alloc (local variable) is handled in isAssignment, not here.
//
// ## *ssa.MapUpdate - Map value store
//
//	m["key"] = q        ← Stores *gorm.DB to map
//
// Why track: Map values can be accessed from multiple places, so we
// conservatively mark the value as polluted.
//
// ## *ssa.MakeInterface - Interface conversion
//
//	var i interface{} = q   ← Converts *gorm.DB to interface{}
//
// Why track: Once converted to interface{}, the value might be type-asserted
// and used elsewhere, so we conservatively mark it as polluted.
//
// # Processing Flow
//
//	SSA instruction stream:
//	  t1 = db.Where("x")      ← *ssa.Call    → CallHandler
//	  t2 = t1.Find            ← (implicit MakeClosure in CallHandler)
//	  t3 = Call t2(nil)       ← *ssa.Call    → CallHandler (bound method)
//	  go t1.Count(nil)        ← *ssa.Go      → GoHandler
//	  ch <- t1                ← *ssa.Send    → SendHandler
//	  slice[0] = t1           ← *ssa.Store   → StoreHandler
//	  m["k"] = t1             ← *ssa.MapUpdate → MapUpdateHandler
//	  var i interface{} = t1  ← *ssa.MakeInterface → MakeInterfaceHandler
//
// Each handler:
//  1. Extracts the *gorm.DB value from the instruction
//  2. Finds the mutable root via ctx.RootTracer.FindMutableRoot()
//  3. Marks the root as polluted via ctx.Tracker (if appropriate)
//  4. Records violations if the root was already polluted
//
// # Why No Default Case?
//
// The type switch has no default case. This is intentional:
//   - We ONLY care about the 6 instruction types listed above
//   - All other instruction types are ignored (not relevant to *gorm.DB tracking)
//   - Adding a default case would just add unnecessary overhead
//
// # Example: Complete Instruction Processing
//
//	Go code:
//	  q := db.Where("x")
//	  q.Find(nil)
//	  q.Count(nil)
//
//	SSA:
//	  Block 0:
//	    t1 = db.Where("x")          ← *ssa.Call
//	         Dispatch → CallHandler.Handle()
//	           - isAssignment = false (no Phi, no Store to Alloc)
//	           - ProcessBranch(t1) → marks t1 as polluted (first use)
//
//	    t2 = t1.Find(nil)           ← *ssa.Call
//	         Dispatch → CallHandler.Handle()
//	           - Already handled (return type not *gorm.DB)
//
//	    t3 = t1.Count(nil)          ← *ssa.Call
//	         Dispatch → CallHandler.Handle()
//	           - root = FindMutableRoot(t1) = t1
//	           - IsPollutedAt(t1) = true
//	           - AddViolation() → VIOLATION!
func Dispatch(instr ssa.Instruction, ctx *Context) {
	switch i := instr.(type) {
	case *ssa.Call:
		(&CallHandler{}).Handle(i, ctx)
	case *ssa.Go:
		(&GoHandler{}).Handle(i, ctx)
	case *ssa.Send:
		(&SendHandler{}).Handle(i, ctx)
	case *ssa.Store:
		(&StoreHandler{}).Handle(i, ctx)
	case *ssa.MapUpdate:
		(&MapUpdateHandler{}).Handle(i, ctx)
	case *ssa.MakeInterface:
		(&MakeInterfaceHandler{}).Handle(i, ctx)
	}
}

// DispatchDefer handles defer instructions separately.
//
// # Why Defer is Special
//
// Defer statements are handled separately from regular instructions because they
// have unique execution semantics that require different pollution checking.
//
// # Regular Instruction vs Defer Execution
//
// ## Regular instructions execute immediately in control flow order:
//
//	Go code:
//	  q := db.Where("x")
//	  q.Find(nil)       // ← Executes HERE (Block 0)
//	  if cond {
//	      q.Count(nil)  // ← Executes HERE (Block 1) - only if cond is true
//	  }
//
//	SSA:
//	  Block 0:
//	    t1 = db.Where("x")
//	    t2 = t1.Find(nil)     ← Executes in Block 0
//	    If cond → Block 1 : Block 2
//	  Block 1:
//	    t3 = t1.Count(nil)    ← Executes in Block 1 (if cond is true)
//
// For regular instructions, we check pollution AT THE CURRENT BLOCK:
//
//	if ctx.Tracker.IsPollutedAt(root, block) { ... }
//	                            ^^^^^^^^^^^^
//	                            Check pollution at specific block
//
// ## Defer executes at function exit, NOT where declared:
//
//	Go code:
//	  q := db.Where("x")
//	  if cond {
//	      defer q.Find(nil)  // ← Deferred (will execute at function exit)
//	  }
//	  q.Count(nil)           // ← Executes HERE (before defer!)
//	  // Function exits
//	  // → defer q.Find(nil) executes NOW
//
//	SSA:
//	  Block 0:
//	    t1 = db.Where("x")
//	    If cond → Block 1 : Block 2
//	  Block 1:
//	    Defer t1.Find(nil)    ← DEFERRED (declared in Block 1)
//	    Goto Block 2
//	  Block 2:
//	    t2 = t1.Count(nil)    ← Executes BEFORE defer
//	    Return
//	    // → Defer t1.Find(nil) executes HERE (after Block 2)
//
// Runtime execution order:
//  1. Block 0: q := db.Where("x")
//  2. Block 1: defer q.Find(nil) (DEFERRED, not executed yet)
//  3. Block 2: q.Count(nil) (executes before defer!)
//  4. Function exit: q.Find(nil) (defer executes NOW)
//
// # Why IsPollutedAnywhere vs IsPollutedAt?
//
// ## Regular instruction - IsPollutedAt(root, block):
//
//	q := db.Where("x")
//	if cond {
//	    q.Find(nil)  // ← Executes in Block 1
//	}
//	q.Count(nil)     // ← Check: IsPollutedAt(q, Block 2)?
//	                 //   → Only checks if q was polluted in blocks
//	                 //     that REACH Block 2 via control flow
//
// ## Defer instruction - IsPollutedAnywhere(root):
//
//	q := db.Where("x")
//	defer q.Find(nil)  // ← Deferred (will execute at function exit)
//	if cond {
//	    q.Count(nil)   // ← Executes BEFORE defer
//	}
//	// Function exits → defer executes
//	// Defer must check if q was polluted in ANY block (even future ones)
//
// Because defer executes AFTER all regular instructions, it needs to check
// if the root is polluted ANYWHERE in the function, not just in blocks that
// reach the defer statement.
//
// # Complete Example: Why Separate Handling is Needed
//
//	Go code:
//	  q := db.Where("x")
//	  defer q.Find(nil)  // ← Deferred (Block 0)
//	  q.Count(nil)       // ← Executes BEFORE defer (Block 0)
//
//	Runtime execution order:
//	  1. q := db.Where("x")
//	  2. defer q.Find(nil) registered (NOT executed yet)
//	  3. q.Count(nil) ← First actual use
//	  4. Function exits
//	  5. q.Find(nil) ← Defer executes (second use) - VIOLATION!
//
//	SSA:
//	  Block 0:
//	    t1 = db.Where("x")
//	    Defer t1.Find(nil)       ← DispatchDefer
//	          ^^^^^^^^^^^^^^^^
//	          DeferHandler uses IsPollutedAnywhere(t1)
//	          → Checks if t1 is polluted in ANY block
//	          → At this point, t1 is NOT polluted yet
//	          → Records this defer for later checking
//	    t2 = t1.Count(nil)       ← Dispatch (regular CallHandler)
//	          ^^^^^^^^^^^^^^^^
//	          ProcessBranch(t1) → marks t1 as polluted
//
//	At analysis time, we detect:
//	  - defer t1.Find(nil) will execute after function
//	  - t1 is polluted by t1.Count(nil) in Block 0
//	  - IsPollutedAnywhere(t1) = true
//	  - VIOLATION! Defer uses polluted root
//
// # Why Not in Regular Dispatch?
//
// Defer needs special handling:
//  1. Different execution timing (function exit vs immediate)
//  2. Different pollution check (IsPollutedAnywhere vs IsPollutedAt)
//  3. Must check against ALL blocks, not just reachable ones
//
// Therefore, defer is dispatched separately via DispatchDefer instead of
// being included in the regular Dispatch type switch.
//
// # Processing Flow
//
//	SSA Analyzer:
//	  for instr in block.Instrs {
//	      if defer, ok := instr.(*ssa.Defer); ok {
//	          DispatchDefer(defer, ctx)  ← Special handling
//	      } else {
//	          Dispatch(instr, ctx)       ← Regular handling
//	      }
//	  }
func DispatchDefer(d *ssa.Defer, ctx *Context) {
	(&DeferHandler{}).Handle(d, ctx)
}
