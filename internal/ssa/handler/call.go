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
	"github.com/mpyw/gormreuse/internal/ssa/pollutionsource"
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

	// NeedsImmutableParam holds the functions marked //gormreuse:immutable-param
	// that genuinely rely on immutability (they branch a *gorm.DB parameter). A
	// call passing a mutable *gorm.DB to such a function violates the contract
	// (Phase 1b stage 2b). Functions whose annotation is redundant (never branch
	// a param) are excluded, so they impose no caller-side obligation.
	NeedsImmutableParam map[*ssa.Function]bool

	// PosOverride, when valid, replaces the source position recorded for uses
	// inside this function. It is set when analyzing a closure that is invoked
	// at a single known call site, so the closure's captured-value uses are
	// ordered by the call-site (execution) position rather than the closure's
	// (earlier) body position — the define-early/call-late case of #68.
	PosOverride token.Pos
}

// pos returns the effective source position to record for a use: the
// PosOverride when set (closure analyzed at its call site), otherwise raw.
func (c *Context) pos(raw token.Pos) token.Pos {
	if c.PosOverride.IsValid() {
		return c.PosOverride
	}
	return raw
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
// Returns true if the call result flows into a Phi node, Store to Alloc,
// or is part of a chain where the final result is assigned.
//
// Assignment patterns (don't pollute):
//   - q = q.Where("x")  → Phi node or Store to Alloc
//   - for { q = q.Where() } → loop-header Phi
//   - q = q.Where("x").Where("y") → chain where final result is assigned
//
// Non-assignment patterns (pollute):
//   - q.Find(nil) → direct use (finisher)
//   - q.Where("x").Find(nil) → chained use where final result is NOT assigned
func isAssignment(call *ssa.Call, ctx *Context) bool {
	return isAssignmentRecursive(call, make(map[*ssa.Call]bool))
}

// isAssignmentRecursive checks if a call result eventually flows into an assignment.
// Uses visited map to avoid infinite recursion in case of cycles.
func isAssignmentRecursive(call *ssa.Call, visited map[*ssa.Call]bool) bool {
	if visited[call] {
		return false
	}
	visited[call] = true

	if call.Referrers() == nil {
		return false
	}

	for _, user := range *call.Referrers() {
		// Phi node: control flow merge creates new SSA value (assignment)
		if _, ok := user.(*ssa.Phi); ok {
			return true
		}

		// Store to Alloc: direct variable assignment (q = ...)
		if store, ok := user.(*ssa.Store); ok {
			if _, ok := store.Addr.(*ssa.Alloc); ok {
				return true
			}
		}

		// Chain intermediate: check if the next call in chain eventually becomes assignment
		// Example: q.Where("x").Where("y") - Where("x") is assignment only if Where("y") is
		if nextCall, ok := user.(*ssa.Call); ok {
			if isChainedGormMethodCall(call, nextCall) {
				// Recursively check if the next call is assignment
				if isAssignmentRecursive(nextCall, visited) {
					return true
				}
			}
		}
	}

	return false
}

// isChainedGormMethodCall checks if nextCall is a gorm method call that uses
// call's result as receiver (i.e., they form a method chain).
func isChainedGormMethodCall(call *ssa.Call, nextCall *ssa.Call) bool {
	// Check if nextCall is a gorm method call
	callee := nextCall.Call.StaticCallee()
	if callee == nil {
		return false
	}

	sig := callee.Signature
	if sig == nil || sig.Recv() == nil {
		return false
	}

	if !typeutil.IsGormDB(sig.Recv().Type()) {
		return false
	}

	// Check if call's result is nextCall's receiver (first argument)
	if len(nextCall.Call.Args) == 0 {
		return false
	}

	return nextCall.Call.Args[0] == call
}

// Handle processes a Call instruction and tracks *gorm.DB pollution.
//
// Processing order:
//  1. Check function calls with *gorm.DB args (mark as polluted)
//  2. Handle bound method calls (find := q.Find; find(nil))
//  3. Process gorm method calls: pure/assignment/actual use
//  4. Check all Phi roots for conditional merges
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
	// Record usage (violations detected later in DetectViolations).
	// pos is the effective position (call-site override inside an invoked
	// closure, else the call's own position) — see Context.pos / #68.
	pos := ctx.pos(call.Pos())
	if isImmutableReturning {
		// Pure methods check for pollution but don't pollute
		ctx.Tracker.RecordPureUse(root, call.Block(), pos)
	} else if isAssignment(call, ctx) {
		// Assignment creates new root - record but doesn't pollute
		ctx.Tracker.RecordAssignment(root, call.Block(), pos)
	} else {
		// Actual use - pollutes the root
		ctx.Tracker.ProcessBranch(root, call.Block(), pos)

		// Loop with external root - immediate violation (only for non-pure methods)
		if isInLoop && ctx.CFG.IsDefinedOutsideLoop(root, ctx.LoopInfo) {
			ctx.Tracker.AddViolationWithRoot(pos, root)
		}
	}

	// Check ALL possible roots for phi nodes
	allRoots := ctx.RootTracer.FindAllMutableRoots(recv, ctx.LoopInfo)
	for _, r := range allRoots {
		if r == root {
			continue
		}
		if ctx.Tracker.IsPollutedAt(r, call.Block()) {
			ctx.Tracker.AddViolationWithRoot(pos, r)
		}
	}
}

// processBoundMethodCall handles method values like: find := q.Find; find(nil)
//
// In SSA, method values are MakeClosure with receiver in Bindings[0] and
// method name suffixed with "$bound". We extract the receiver and track
// pollution like normal method calls.
//
// Example:
//
//	q := db.Where("x")
//	find := q.Find  // MakeClosure(Find$bound, [q])
//	find(nil)       // first use - OK
//	q.Count(nil)    // VIOLATION (q already polluted by find(nil))
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

	// Get ALL possible roots BEFORE recording usage (needed for pollution check)
	allRoots := ctx.RootTracer.FindAllMutableRoots(recv, ctx.LoopInfo)

	pos := ctx.pos(call.Pos())

	// Check if ANY root was already polluted BEFORE this call
	for _, r := range allRoots {
		if ctx.Tracker.IsPollutedAt(r, call.Block()) {
			ctx.Tracker.AddViolationWithRoot(pos, r)
		}
	}

	// Record usage (violations detected later)
	if isImmutableReturning {
		// Pure methods check for pollution but don't pollute
		ctx.Tracker.RecordPureUse(root, call.Block(), pos)
	} else {
		// Non-pure methods pollute the root
		ctx.Tracker.ProcessBranch(root, call.Block(), pos)

		// Loop with external root - immediate violation (only for non-pure methods)
		if isInLoop && ctx.CFG.IsDefinedOutsideLoop(root, ctx.LoopInfo) {
			ctx.Tracker.AddViolationWithRoot(pos, root)
		}
	}
}

// checkFunctionCallPollution marks *gorm.DB args passed to non-gorm functions as polluted.
//
// We conservatively assume non-pure functions may use *gorm.DB arguments.
// Functions marked with //gormreuse:pure are exempt.
//
// Example:
//
//	func helper(db *gorm.DB) { db.Find(nil) }  // might pollute
//	q := db.Where("x")
//	helper(q)      // marks q as polluted
//	q.Count(nil)   // VIOLATION (q already polluted)
//
//	//gormreuse:pure
//	func pureHelper(db *gorm.DB) *gorm.DB { return db }
//	q := db.Where("x")
//	pureHelper(q)  // does NOT pollute
//	q.Count(nil)   // OK (first use)
func (h *CallHandler) checkFunctionCallPollution(call *ssa.Call, ctx *Context) {
	callee := call.Call.StaticCallee()

	// Check if this is a pure function - pure functions don't pollute args
	if callee != nil && ctx.RootTracer.IsPureFunction(callee) {
		return
	}

	// Note: We don't skip gorm methods here because we need to pollute
	// *gorm.DB arguments passed through interface{} (e.g., base.Or(q))
	// The receiver is already handled by recordMutableReceiver.

	// If the function returns *gorm.DB and result is assigned, treat like gorm method assignment.
	// The assignment creates a new mutable root, so we shouldn't ADD pollution to args.
	// However, we still need to CHECK if args are already polluted (to detect reuse).
	// This enables patterns like: q = buildQuery(q, "filter")
	isReassignment := typeutil.IsGormDB(call.Type()) && isAssignment(call, ctx)

	// A method call carries its receiver as Args[0]; //gormreuse:immutable-param
	// governs parameters, not the receiver, so the contract check below skips it.
	recvArg := callee != nil && callee.Signature != nil && callee.Signature.Recv() != nil

	for i, arg := range call.Call.Args {
		// Check if arg is *gorm.DB (directly or wrapped in MakeInterface)
		gormArg, ok := pollutionsource.UnwrapGormDB(arg)
		if !ok {
			continue
		}

		root := ctx.RootTracer.FindMutableRoot(gormArg, ctx.LoopInfo)
		if root == nil {
			continue
		}

		// Caller-side contract check (Phase 1b stage 2b): passing a mutable
		// (root != nil) *gorm.DB to a parameter of a function that relies on
		// immutability — it branches the parameter — is unsafe: the callee's
		// internal branching interferes because the value is not isolated.
		if callee != nil && ctx.NeedsImmutableParam[callee] && (!recvArg || i != 0) {
			ctx.Tracker.AddMessageViolation(ctx.pos(call.Pos()), immutableParamContractMessage(callee))
		}

		if isReassignment {
			// For reassignment pattern: check if already polluted, but don't add pollution.
			// This is similar to how gorm methods handle RecordAssignment.
			ctx.Tracker.RecordAssignment(root, call.Block(), ctx.pos(call.Pos()))
		} else {
			// Mark polluted (function may use the value)
			ctx.Tracker.MarkPolluted(root, call.Block(), ctx.pos(call.Pos()))
		}
	}
}

// immutableParamContractMessage builds the diagnostic for passing a mutable
// *gorm.DB to a //gormreuse:immutable-param parameter.
func immutableParamContractMessage(callee *ssa.Function) string {
	name := "immutable-param function"
	if callee != nil {
		name = callee.Name()
	}
	return "mutable *gorm.DB passed to //gormreuse:immutable-param parameter of " + name +
		"; isolate it with .Session(&gorm.Session{}) before passing"
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
	processGormDBCallCommonWith(&g.Call, g.Pos(), block, ctx, func(root ssa.Value) bool {
		return ctx.Tracker.IsPollutedAt(root, block)
	})
}

// DeferHandler handles *ssa.Defer instructions.
type DeferHandler struct{}

// Handle processes a Defer instruction.
// Defer uses IsPollutedAnywhere because it executes at function exit.
func (h *DeferHandler) Handle(d *ssa.Defer, ctx *Context) {
	processGormDBCallCommonWith(&d.Call, d.Pos(), d.Block(), ctx, func(root ssa.Value) bool {
		return ctx.Tracker.IsPollutedAnywhere(root)
	})
}

// SendHandler handles *ssa.Send instructions.
type SendHandler struct{}

// Handle marks *gorm.DB sent to channels as polluted.
// Handles both direct sends and sends through MakeInterface (chan interface{}).
func (h *SendHandler) Handle(send *ssa.Send, ctx *Context) {
	gormVal, kind := pollutionsource.Leak(send)
	if kind == pollutionsource.KindNone {
		return
	}

	root := ctx.RootTracer.FindMutableRoot(gormVal, ctx.LoopInfo)
	if root == nil {
		return
	}

	ctx.Tracker.MarkPolluted(root, send.Block(), ctx.pos(send.Pos()))
}

// StoreHandler handles *ssa.Store instructions.
type StoreHandler struct{}

// Handle marks *gorm.DB stored to slice elements as polluted.
// Handles both direct stores and stores through MakeInterface ([]interface{}).
//
// The read-only variadic stdlib exemption (fmt.Println(q), log.Printf, t.Logf)
// lives in pollutionsource.Leak so the purity validator honors it too.
func (h *StoreHandler) Handle(store *ssa.Store, ctx *Context) {
	gormVal, kind := pollutionsource.Leak(store)
	if kind == pollutionsource.KindNone {
		return
	}

	root := ctx.RootTracer.FindMutableRoot(gormVal, ctx.LoopInfo)
	if root == nil {
		return
	}

	ctx.Tracker.MarkPolluted(root, store.Block(), ctx.pos(store.Pos()))
}

// MapUpdateHandler handles *ssa.MapUpdate instructions.
type MapUpdateHandler struct{}

// Handle marks *gorm.DB stored in maps as polluted.
// Handles both direct stores and stores through MakeInterface (map[K]interface{}).
func (h *MapUpdateHandler) Handle(mapUpdate *ssa.MapUpdate, ctx *Context) {
	gormVal, kind := pollutionsource.Leak(mapUpdate)
	if kind == pollutionsource.KindNone {
		return
	}

	root := ctx.RootTracer.FindMutableRoot(gormVal, ctx.LoopInfo)
	if root == nil {
		return
	}

	ctx.Tracker.MarkPolluted(root, mapUpdate.Block(), ctx.pos(mapUpdate.Pos()))
}

// MakeInterfaceHandler handles *ssa.MakeInterface instructions.
type MakeInterfaceHandler struct{}

// Handle processes *gorm.DB to interface{} conversion.
// NOTE: Interface conversion itself does NOT pollute the source.
// It's just a type conversion (ownership transfer), similar to assignment.
// The source *gorm.DB is only polluted when actually used via:
// - Type assertion extraction followed by gorm method calls
// - Function calls that receive the interface{} value
// Those are handled by their respective handlers.
func (h *MakeInterfaceHandler) Handle(mi *ssa.MakeInterface, ctx *Context) {
	// No-op: interface conversion doesn't pollute the source
	// The value is just wrapped in interface{}, not used.
}

// pollutionChecker is a function that checks if a root is polluted.
type pollutionChecker func(root ssa.Value) bool

// processGormDBCallCommonWith processes gorm calls with a custom pollution checker.
//
// Used by the defer and goroutine handlers. In addition to CHECKING whether the
// receiver/argument root is already polluted (and reporting a violation if so),
// it RECORDS each use as a branch use. Recording lets a later defer/goroutine
// observe an earlier one, so patterns whose ONLY uses are deferred/spawned —
// e.g. `defer q.Find(nil); defer q.Count(nil)` — are detected. Branch uses are
// excluded from position-ordered detection (see pollution.Tracker.branchUses).
func processGormDBCallCommonWith(callCommon *ssa.CallCommon, pos token.Pos, block *ssa.BasicBlock, ctx *Context, isPolluted pollutionChecker) {
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

		// Check ALL possible roots for phi nodes
		allRoots := ctx.RootTracer.FindAllMutableRoots(recv, ctx.LoopInfo)
		for _, r := range allRoots {
			if r == root {
				continue
			}
			if isPolluted(r) {
				ctx.Tracker.AddViolationWithRoot(pos, r)
			}
		}

		// Record this deferred/spawned use so a later defer/go sees it.
		ctx.Tracker.RecordBranchUse(root, block, pos)
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

		// Check ALL possible roots for phi nodes
		allRoots := ctx.RootTracer.FindAllMutableRoots(arg, ctx.LoopInfo)
		for _, r := range allRoots {
			if r == root {
				continue
			}
			if isPolluted(r) {
				ctx.Tracker.AddViolationWithRoot(pos, r)
			}
		}

		// Record this deferred/spawned use so a later defer/go sees it.
		ctx.Tracker.RecordBranchUse(root, block, pos)
	}
}

// Dispatch routes SSA instructions to their handlers using type switch.
//
// Handler mapping:
//   - *ssa.Call         → CallHandler (method/function calls)
//   - *ssa.Go           → GoHandler (goroutine launches)
//   - *ssa.Send         → SendHandler (channel send: ch <- db)
//   - *ssa.Store        → StoreHandler (slice store: slice[i] = db)
//   - *ssa.MapUpdate    → MapUpdateHandler (map store: m[k] = db)
//   - *ssa.MakeInterface → MakeInterfaceHandler (interface conversion)
//
// Note: *ssa.Defer uses DispatchDefer (different pollution semantics).
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

// DispatchDefer handles defer instructions separately from regular Dispatch.
//
// Defer executes at function exit (not where declared), so it uses
// IsPollutedAnywhere instead of IsPollutedAt to check pollution across
// the entire function.
//
// Example:
//
//	q := db.Where("x")
//	defer q.Find(nil)  // deferred - executes at function exit
//	q.Count(nil)       // executes BEFORE defer, pollutes q
//	// function exits → defer q.Find(nil) → VIOLATION!
func DispatchDefer(d *ssa.Defer, ctx *Context) {
	(&DeferHandler{}).Handle(d, ctx)
}

// DispatchGo handles go instructions separately from regular Dispatch.
//
// Go statements are processed in a second pass to ensure all pollution
// is recorded first. SSA block order may differ from source order, and
// go statements need to see pollution from all paths before checking.
//
// Example:
//
//	var q *gorm.DB
//	if flag {
//	    q = db.Where("a")
//	} else {
//	    q = db.Where("b")
//	    q.Find(nil)  // pollutes q in this branch
//	}
//	go q.Count(nil)  // needs to see pollution from else branch
func DispatchGo(g *ssa.Go, ctx *Context) {
	(&GoHandler{}).Handle(g, ctx)
}
