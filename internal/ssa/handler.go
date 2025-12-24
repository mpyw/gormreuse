package ssa

import (
	"go/token"
	"strings"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/typeutil"
)

// =============================================================================
// Instruction Handlers
//
// Each handler processes a specific type of SSA instruction.
// DispatchInstruction routes instructions to the appropriate handler using type switch.
// =============================================================================

// HandlerContext provides shared context for instruction handlers.
type HandlerContext struct {
	Tracker     *PollutionTracker
	RootTracer  *RootTracer
	CFGAnalyzer *CFGAnalyzer
	LoopInfo    *LoopInfo
	CurrentFn   *ssa.Function
}

// checkAndMarkPollution checks for pollution and marks terminal calls as polluting.
// Used by CallHandler for direct and bound method calls.
func (ctx *HandlerContext) checkAndMarkPollution(
	root ssa.Value,
	currentBlock *ssa.BasicBlock,
	pos token.Pos,
	isPureBuiltin bool,
	isTerminal bool,
	isInLoop bool,
) {
	if ctx.Tracker.IsPollutedInBlock(root, currentBlock) {
		ctx.Tracker.AddViolation(root, pos)
		return
	}
	if !isPureBuiltin && isTerminal {
		ctx.Tracker.MarkPolluted(root, currentBlock, pos)
		if isInLoop && ctx.CFGAnalyzer.IsDefinedOutsideLoop(root, ctx.LoopInfo) {
			ctx.Tracker.AddViolation(root, pos)
		}
	}
}

// pollutionChecker is a function that checks if a value is polluted.
type pollutionChecker func(root ssa.Value) bool

// processGormDBCallCommon processes a call with *gorm.DB arguments.
// Used by GoHandler and DeferHandler to share common pollution-check logic.
func processGormDBCallCommon(
	callCommon *ssa.CallCommon,
	pos token.Pos,
	ctx *HandlerContext,
	isPolluted pollutionChecker,
) {
	callee := callCommon.StaticCallee()
	if callee == nil {
		return
	}

	sig := callee.Signature

	// Check if it's a method call on *gorm.DB
	if sig != nil && sig.Recv() != nil && typeutil.IsGormDB(sig.Recv().Type()) {
		if len(callCommon.Args) == 0 {
			return
		}
		recv := callCommon.Args[0]

		root := ctx.RootTracer.FindMutableRoot(recv)
		if root == nil {
			return
		}

		if isPolluted(root) {
			ctx.Tracker.AddViolation(root, pos)
		}
		return
	}

	// Check each argument for *gorm.DB pollution
	for _, arg := range callCommon.Args {
		if !typeutil.IsGormDB(arg.Type()) {
			continue
		}

		root := ctx.RootTracer.FindMutableRoot(arg)
		if root == nil {
			continue
		}

		if isPolluted(root) {
			ctx.Tracker.AddViolation(root, pos)
		}
	}
}

// =============================================================================
// Handler Implementations
// =============================================================================

// CallHandler handles *ssa.Call instructions.
//
// This is the primary handler for detecting *gorm.DB reuse violations.
// It processes method calls on *gorm.DB and function calls that accept *gorm.DB.
type CallHandler struct{}

// Handle processes a Call instruction for pollution detection.
//
// Example scenarios:
//
//	Scenario 1: Terminal chain method (pollutes the root)
//	  q := db.Where("x")     // q is mutable root
//	  q.Find(nil)            // Terminal call → marks q as polluted
//	  q.Find(nil)            // Violation! q is already polluted
//
//	Scenario 2: Bound method (method value)
//	  q := db.Where("x")
//	  find := q.Find         // MakeClosure with receiver bound
//	  find(nil)              // Call through bound method → pollutes q
//
//	Scenario 3: Helper function pollutes argument
//	  q := db.Where("x")
//	  doSomething(q)         // Non-pure function → marks q as polluted
//
//	Scenario 4: Loop with external root
//	  q := db.Where("x")
//	  for i := range items {
//	      q.Find(nil)        // In loop + root outside → immediate violation
//	  }
func (h *CallHandler) Handle(instr ssa.Instruction, ctx *HandlerContext) {
	call := instr.(*ssa.Call)
	isInLoop := ctx.LoopInfo.IsInLoop(call.Block())

	// Check if this is a function call that takes *gorm.DB as argument (pollutes it)
	h.checkFunctionCallPollution(call, ctx)

	// Check if this is a bound method call (method value)
	if mc, ok := call.Call.Value.(*ssa.MakeClosure); ok {
		h.processBoundMethodCall(call, mc, isInLoop, ctx)
		return
	}

	// Check if this is a method call on *gorm.DB
	callee := call.Call.StaticCallee()
	if !h.isGormDBMethodCall(call) {
		return
	}

	if callee == nil {
		return
	}

	methodName := callee.Name()
	isPureBuiltin := typeutil.IsPureFunctionBuiltin(methodName)
	isTerminal := h.isTerminalCall(call)

	// Skip non-terminal Chain Methods (part of chain construction)
	if !isTerminal && !isPureBuiltin {
		return
	}

	// Get the receiver
	if len(call.Call.Args) == 0 {
		return
	}
	recv := call.Call.Args[0]

	// Find the mutable root being used
	root := ctx.RootTracer.FindMutableRoot(recv)
	if root == nil {
		return // Receiver is immutable
	}

	currentBlock := call.Block()

	// Check for same-block pollution and mark terminal calls
	ctx.checkAndMarkPollution(root, currentBlock, call.Pos(), isPureBuiltin, isTerminal, isInLoop)

	// Check ALL possible roots for pollution (handles Phi nodes from switch/if)
	allRoots := ctx.RootTracer.FindAllMutableRoots(recv)
	for _, r := range allRoots {
		if r == root {
			continue
		}
		if ctx.Tracker.IsPollutedAt(r, currentBlock) {
			ctx.Tracker.AddViolation(r, call.Pos())
		}
	}
}

// processBoundMethodCall handles calls through method values (bound methods).
//
// Example:
//
//	q := db.Where("x")
//	find := q.Find          // MakeClosure: Fn=(*gorm.DB).Find$bound, Bindings=[q]
//	find(&users)            // Call: Value=MakeClosure
//
//	SSA representation:
//	  t0 = db.Where("x")              // q
//	  t1 = make closure (*gorm.DB).Find$bound [t0]  // find
//	  t2 = t1(&users)                 // call through bound method
//
//	Detection:
//	  1. mc.Bindings[0] is the receiver (q)
//	  2. Method name has "$bound" suffix
//	  3. Apply same terminal/pollution logic as direct calls
func (h *CallHandler) processBoundMethodCall(call *ssa.Call, mc *ssa.MakeClosure, isInLoop bool, ctx *HandlerContext) {
	if len(mc.Bindings) == 0 {
		return
	}

	recv := mc.Bindings[0]
	if !typeutil.IsGormDB(recv.Type()) {
		return
	}

	methodName := strings.TrimSuffix(mc.Fn.Name(), "$bound")
	isPureBuiltin := typeutil.IsPureFunctionBuiltin(methodName)
	isTerminal := h.isTerminalCall(call)

	if !isTerminal && !isPureBuiltin {
		return
	}

	root := ctx.RootTracer.FindMutableRoot(recv)
	if root == nil {
		return
	}

	ctx.checkAndMarkPollution(root, call.Block(), call.Pos(), isPureBuiltin, isTerminal, isInLoop)
}

// isBoundMethodCall checks if a MakeClosure is a bound method call (method value).
// Bound methods have names like "Find$bound" and bind the receiver in Bindings[0].
func (h *CallHandler) isBoundMethodCall(mc *ssa.MakeClosure) bool {
	fnName := mc.Fn.Name()
	return strings.HasSuffix(fnName, "$bound") && len(mc.Bindings) > 0 && typeutil.IsGormDB(mc.Bindings[0].Type())
}

// processClosureCallReturnPollution handles when a closure is called and returns *gorm.DB.
// This marks the mutable root as polluted because calling the closure "realizes" the branch.
//
// Example:
//
//	fn := func() *gorm.DB { return q.Where("x") }
//	q2 := fn()           // Calling fn() creates a branch from q
//	q.Where("y")         // Second use of q → violation
//	q2.Where("z")        // Continues q2's chain (OK)
func (h *CallHandler) processClosureCallReturnPollution(call *ssa.Call, mc *ssa.MakeClosure, ctx *HandlerContext) {
	// Only process closures that return *gorm.DB
	if !typeutil.IsGormDB(call.Type()) {
		return
	}

	// Find the closure function
	closureFn, ok := mc.Fn.(*ssa.Function)
	if !ok || closureFn == nil {
		return
	}

	// Trace through the closure's return statements to find mutable roots
	for _, block := range closureFn.Blocks {
		for _, instr := range block.Instrs {
			ret, ok := instr.(*ssa.Return)
			if !ok || len(ret.Results) == 0 {
				continue
			}

			// Find mutable root of the return value
			root := ctx.RootTracer.FindMutableRoot(ret.Results[0])
			if root == nil {
				continue
			}

			// Mark the root as polluted - the closure call creates a branch
			ctx.Tracker.MarkPolluted(root, call.Block(), call.Pos())
		}
	}
}

func (h *CallHandler) checkFunctionCallPollution(call *ssa.Call, ctx *HandlerContext) {
	callee := call.Call.StaticCallee()

	if callee != nil {
		sig := callee.Signature
		if sig != nil && sig.Recv() != nil && typeutil.IsGormDB(sig.Recv().Type()) {
			return
		}

		if ctx.RootTracer.IsPureFunction(callee) {
			return
		}
	}

	for _, arg := range call.Call.Args {
		if !typeutil.IsGormDB(arg.Type()) {
			continue
		}

		root := ctx.RootTracer.FindMutableRoot(arg)
		if root == nil {
			continue
		}

		ctx.Tracker.MarkPolluted(root, call.Block(), call.Pos())
	}
}

// isTerminalCall determines if a call consumes the chain (vs continues building it).
//
// Example:
//
//	Terminal calls (result not used as receiver):
//	  q.Find(&users)                  // Returns *gorm.DB but not used for chaining
//	  q.Where("x")                    // Result is not used at all
//	  q.Where("x").Find(&users)       // Find is terminal, Where is not
//
//	Non-terminal calls (result used as receiver):
//	  q.Where("x").Where("y")         // First Where's result is receiver of second
//	  q.Joins("...").Preload("...")   // Joins result is receiver of Preload
//
//	Detection logic:
//	  1. If return type is not *gorm.DB → terminal
//	  2. If no referrers → terminal
//	  3. If any referrer uses this as receiver for *gorm.DB method → non-terminal
//	  4. If result is returned from an IIFE that is chained → non-terminal
func (h *CallHandler) isTerminalCall(call *ssa.Call) bool {
	if !typeutil.IsGormDB(call.Type()) {
		return true
	}

	refs := call.Referrers()
	if refs == nil || len(*refs) == 0 {
		return true
	}

	for _, ref := range *refs {
		// Check if this result is used as receiver for another gorm method
		if refCall, ok := ref.(*ssa.Call); ok {
			if h.isGormDBMethodCall(refCall) && len(refCall.Call.Args) > 0 && refCall.Call.Args[0] == call {
				return false
			}
		}

		// Check if this result is returned from an IIFE that is chained
		// (the IIFE result is used in a gorm method call)
		if ret, ok := ref.(*ssa.Return); ok {
			if h.isReturnedFromChainedIIFE(call, ret) {
				return false
			}
		}
	}

	return true
}

// isReturnedFromChainedClosure checks if a call result is returned from a closure
// whose result is eventually used. This handles patterns like:
//
//	IIFE direct chain: _ = func() *gorm.DB { return q.Where("x") }().Find(nil)
//	IIFE stored:       h.field = func() *gorm.DB { return q.Where("x") }(); h.field.Find(nil)
//	Stored closure:    f := func() *gorm.DB { return q.Where("x") }; q2 := f(); q2.Find(nil)
//	Nested:            _ = func() *gorm.DB { return func() *gorm.DB { return q.Where("x") }() }().Find(nil)
//
// The key insight: If a call is RETURNED from a closure that is called somewhere,
// the returned value forms a chain. The terminal use happens at the call site,
// not inside the closure. This is "inline expansion" semantics.
func (h *CallHandler) isReturnedFromChainedIIFE(call *ssa.Call, ret *ssa.Return) bool {
	visited := make(map[*ssa.Function]bool)
	return h.isReturnedFromCalledClosureRecursive(ret.Parent(), visited)
}

// isReturnedFromCalledClosureRecursive checks if a closure function is called somewhere.
// Any call to the closure means the returned value flows out, so the internal call
// is non-terminal (inline expansion semantics).
func (h *CallHandler) isReturnedFromCalledClosureRecursive(fn *ssa.Function, visited map[*ssa.Function]bool) bool {
	if fn == nil || visited[fn] {
		return false
	}
	visited[fn] = true

	// Find all MakeClosures that reference this function
	refs := fn.Referrers()
	if refs == nil {
		return false
	}
	for _, ref := range *refs {
		mc, ok := ref.(*ssa.MakeClosure)
		if !ok {
			continue
		}

		// Check if this closure is called anywhere (IIFE, stored, or nested)
		if h.closureIsCalledRecursive(mc, visited) {
			return true
		}
	}

	return false
}

// closureIsCalledRecursive checks if a closure is called anywhere.
// Handles IIFE (direct call), stored closures (store + load + call),
// and nested closures (returned from another closure that is called).
func (h *CallHandler) closureIsCalledRecursive(mc *ssa.MakeClosure, visited map[*ssa.Function]bool) bool {
	mcRefs := mc.Referrers()
	if mcRefs == nil {
		return false
	}

	for _, mcRef := range *mcRefs {
		// Case 1: IIFE - MakeClosure is directly called
		if closureCall, ok := mcRef.(*ssa.Call); ok && closureCall.Call.Value == mc {
			return true
		}

		// Case 2: Stored closure - MakeClosure stored, then loaded and called
		if store, ok := mcRef.(*ssa.Store); ok {
			if h.storedValueIsCalledRecursive(store.Addr, visited) {
				return true
			}
		}

		// Case 3: Returned from another closure - check if that closure is called
		if _, ok := mcRef.(*ssa.Return); ok {
			parentFn := mc.Parent()
			if parentFn != nil && h.isReturnedFromCalledClosureRecursive(parentFn, visited) {
				return true
			}
		}
	}

	return false
}

// storedValueIsCalledRecursive checks if a stored value is loaded and called.
func (h *CallHandler) storedValueIsCalledRecursive(addr ssa.Value, visited map[*ssa.Function]bool) bool {
	addrRefs := addr.Referrers()
	if addrRefs == nil {
		return false
	}
	for _, addrRef := range *addrRefs {
		// Look for UnOp (pointer load)
		if unop, ok := addrRef.(*ssa.UnOp); ok {
			unopRefs := unop.Referrers()
			if unopRefs == nil {
				continue
			}
			for _, unopRef := range *unopRefs {
				if closureCall, ok := unopRef.(*ssa.Call); ok && closureCall.Call.Value == unop {
					return true
				}
			}
		}
	}
	return false
}

// isReturnedFromChainedIIFERecursive recursively checks if a function's return value
// eventually flows to a gorm chain, handling nested closures.
// DEPRECATED: Use isReturnedFromCalledClosureRecursive instead.
func (h *CallHandler) isReturnedFromChainedIIFERecursive(fn *ssa.Function, visited map[*ssa.Function]bool) bool {
	if fn == nil || visited[fn] {
		return false
	}
	visited[fn] = true

	// Find all MakeClosures that reference this function
	refs := fn.Referrers()
	if refs == nil {
		return false
	}
	for _, ref := range *refs {
		mc, ok := ref.(*ssa.MakeClosure)
		if !ok {
			continue
		}

		// Check if any call to this closure has its result used in a gorm chain
		// or returned from another closure that is chained
		if h.closureCallResultUsedInChainRecursive(mc, visited) {
			return true
		}
	}

	return false
}

// closureCallResultUsedInChain checks if any call to a closure has its result
// used as receiver in a gorm method chain.
func (h *CallHandler) closureCallResultUsedInChain(mc *ssa.MakeClosure) bool {
	return h.closureCallResultUsedInChainRecursive(mc, make(map[*ssa.Function]bool))
}

// closureCallResultUsedInChainRecursive recursively checks closure call results.
func (h *CallHandler) closureCallResultUsedInChainRecursive(mc *ssa.MakeClosure, visited map[*ssa.Function]bool) bool {
	mcRefs := mc.Referrers()
	if mcRefs == nil {
		return false
	}

	for _, mcRef := range *mcRefs {
		// Case 1: IIFE - MakeClosure is directly called
		if closureCall, ok := mcRef.(*ssa.Call); ok && closureCall.Call.Value == mc {
			// Check if result is used directly in a gorm chain
			if h.callResultUsedInGormChain(closureCall) {
				return true
			}
			// Check if result is returned from another closure that is chained
			if h.callResultReturnedAndChained(closureCall, visited) {
				return true
			}
		}

		// Case 2: Stored closure - MakeClosure stored, then loaded and called
		if store, ok := mcRef.(*ssa.Store); ok {
			if h.storedClosureUsedInChainRecursive(store.Addr, visited) {
				return true
			}
		}
	}

	return false
}

// callResultReturnedAndChained checks if a call result is returned from a closure
// and that closure's call result is eventually used in a gorm chain.
func (h *CallHandler) callResultReturnedAndChained(call *ssa.Call, visited map[*ssa.Function]bool) bool {
	refs := call.Referrers()
	if refs == nil {
		return false
	}
	for _, ref := range *refs {
		if _, ok := ref.(*ssa.Return); ok {
			// This call result is returned - check if the parent function's
			// return value eventually flows to a gorm chain
			parentFn := call.Parent()
			if parentFn != nil && h.isReturnedFromChainedIIFERecursive(parentFn, visited) {
				return true
			}
		}
	}
	return false
}

// callResultUsedInGormChain checks if a call's result is used as receiver for a gorm method.
func (h *CallHandler) callResultUsedInGormChain(call *ssa.Call) bool {
	refs := call.Referrers()
	if refs == nil {
		return false
	}
	for _, ref := range *refs {
		if chainCall, ok := ref.(*ssa.Call); ok {
			if h.isGormDBMethodCall(chainCall) && len(chainCall.Call.Args) > 0 && chainCall.Call.Args[0] == call {
				return true
			}
		}
	}
	return false
}

// storedClosureUsedInChainRecursive checks if a stored closure is called and the result
// eventually used in a gorm chain (handling nested closures).
func (h *CallHandler) storedClosureUsedInChainRecursive(addr ssa.Value, visited map[*ssa.Function]bool) bool {
	// Find loads from this address
	addrRefs := addr.Referrers()
	if addrRefs == nil {
		return false
	}
	for _, addrRef := range *addrRefs {
		// Look for UnOp (pointer load)
		if unop, ok := addrRef.(*ssa.UnOp); ok {
			unopRefs := unop.Referrers()
			if unopRefs == nil {
				continue
			}
			for _, unopRef := range *unopRefs {
				if closureCall, ok := unopRef.(*ssa.Call); ok && closureCall.Call.Value == unop {
					// Check if result is used directly in a gorm chain
					if h.callResultUsedInGormChain(closureCall) {
						return true
					}
					// Check if result is returned from another closure that is chained
					if h.callResultReturnedAndChained(closureCall, visited) {
						return true
					}
				}
			}
		}
	}
	return false
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

// =============================================================================
// GoHandler handles *ssa.Go instructions.
// =============================================================================

// GoHandler handles goroutine spawning instructions.
type GoHandler struct{}

// Handle processes a Go instruction for pollution detection.
func (h *GoHandler) Handle(instr ssa.Instruction, ctx *HandlerContext) {
	g := instr.(*ssa.Go)
	h.processCallCommon(&g.Call, g.Pos(), g.Block(), ctx)
}

func (h *GoHandler) processCallCommon(callCommon *ssa.CallCommon, pos token.Pos, block *ssa.BasicBlock, ctx *HandlerContext) {
	processGormDBCallCommon(callCommon, pos, ctx, func(root ssa.Value) bool {
		return ctx.Tracker.IsPollutedAt(root, block)
	})
}

// =============================================================================
// DeferHandler handles *ssa.Defer instructions.
//
// Deferred calls execute at function exit, so they see pollution from ANY block.
// This is different from regular calls which only see pollution from reachable blocks.
//
// Example:
//
//	func example(db *gorm.DB) {
//	    q := db.Where("x")
//	    defer q.Find(nil)       // Deferred: will execute at function exit
//	    q.Find(nil)             // Pollutes q
//	}                           // defer executes here → violation!
//
// Detection:
//
//	Unlike regular calls, defer checks IsPollutedAnywhere (not IsPollutedAt)
//	because the deferred call executes after all other code in the function.
// =============================================================================

// DeferHandler handles deferred call instructions.
type DeferHandler struct{}

// Handle processes a Defer instruction for pollution detection.
func (h *DeferHandler) Handle(instr ssa.Instruction, ctx *HandlerContext) {
	d := instr.(*ssa.Defer)
	h.processCallCommon(&d.Call, d.Pos(), d.Parent(), ctx)
}

func (h *DeferHandler) processCallCommon(callCommon *ssa.CallCommon, pos token.Pos, fn *ssa.Function, ctx *HandlerContext) {
	processGormDBCallCommon(callCommon, pos, ctx, func(root ssa.Value) bool {
		return ctx.Tracker.IsPollutedAnywhere(root, fn)
	})
}

// =============================================================================
// SendHandler handles *ssa.Send instructions (channel send).
//
// Sending *gorm.DB through a channel is treated as pollution because:
//   - The receiver goroutine might use it concurrently
//   - We can't track usage across goroutines statically
//
// Example:
//
//	q := db.Where("x")
//	ch <- q                 // Send pollutes q
//	q.Find(nil)             // Violation! q was sent to channel
// =============================================================================

// SendHandler handles channel send instructions.
type SendHandler struct{}

// Handle marks *gorm.DB values sent to channels as polluted.
func (h *SendHandler) Handle(instr ssa.Instruction, ctx *HandlerContext) {
	send := instr.(*ssa.Send)

	if !typeutil.IsGormDB(send.X.Type()) {
		return
	}

	root := ctx.RootTracer.FindMutableRoot(send.X)
	if root == nil {
		return
	}

	ctx.Tracker.MarkPolluted(root, send.Block(), send.Pos())
}

// =============================================================================
// StoreHandler handles *ssa.Store instructions.
//
// Stores to slice/array elements are treated as pollution because:
//   - The slice might be shared with other code
//   - We can't track which element is accessed
//
// Example:
//
//	q := db.Where("x")
//	dbs := make([]*gorm.DB, 1)
//	dbs[0] = q              // Store to IndexAddr → pollutes q
//	q.Find(nil)             // Violation! q was stored in slice
//
// Note: Simple stores to local variables (Alloc) are NOT pollution.
//
//	They're handled by SSATracer for value tracking.
// =============================================================================

// StoreHandler handles store instructions.
type StoreHandler struct{}

// Handle marks *gorm.DB values stored to slice elements as polluted.
func (h *StoreHandler) Handle(instr ssa.Instruction, ctx *HandlerContext) {
	store := instr.(*ssa.Store)

	if !typeutil.IsGormDB(store.Val.Type()) {
		return
	}

	// Only process stores to IndexAddr (slice element)
	if _, ok := store.Addr.(*ssa.IndexAddr); !ok {
		return
	}

	root := ctx.RootTracer.FindMutableRoot(store.Val)
	if root == nil {
		return
	}

	ctx.Tracker.MarkPolluted(root, store.Block(), store.Pos())
}

// =============================================================================
// MapUpdateHandler handles *ssa.MapUpdate instructions.
//
// Storing *gorm.DB in a map is treated as pollution because:
//   - The map might be shared with other code
//   - We can't track which key is accessed or when
//
// Example:
//
//	q := db.Where("x")
//	m := make(map[string]*gorm.DB)
//	m["key"] = q            // MapUpdate → pollutes q
//	q.Find(nil)             // Violation! q was stored in map
// =============================================================================

// MapUpdateHandler handles map update instructions.
type MapUpdateHandler struct{}

// Handle marks *gorm.DB values stored in maps as polluted.
func (h *MapUpdateHandler) Handle(instr ssa.Instruction, ctx *HandlerContext) {
	mapUpdate := instr.(*ssa.MapUpdate)

	if !typeutil.IsGormDB(mapUpdate.Value.Type()) {
		return
	}

	root := ctx.RootTracer.FindMutableRoot(mapUpdate.Value)
	if root == nil {
		return
	}

	ctx.Tracker.MarkPolluted(root, mapUpdate.Block(), mapUpdate.Pos())
}

// =============================================================================
// MakeInterfaceHandler handles *ssa.MakeInterface instructions.
//
// Converting *gorm.DB to interface{} is treated as pollution because:
//   - Type assertion might extract it later
//   - We can't track usage through interface types
//
// Example:
//
//	q := db.Where("x")
//	var i interface{} = q   // MakeInterface → pollutes q
//	q.Find(nil)             // Violation! q was wrapped in interface
// =============================================================================

// MakeInterfaceHandler handles interface conversion instructions.
type MakeInterfaceHandler struct{}

// Handle marks *gorm.DB values converted to interfaces as polluted.
func (h *MakeInterfaceHandler) Handle(instr ssa.Instruction, ctx *HandlerContext) {
	mi := instr.(*ssa.MakeInterface)

	if !typeutil.IsGormDB(mi.X.Type()) {
		return
	}

	root := ctx.RootTracer.FindMutableRoot(mi.X)
	if root == nil {
		return
	}

	ctx.Tracker.MarkPolluted(root, mi.Block(), mi.Pos())
}

// DispatchInstruction dispatches an instruction to the appropriate handler.
// Uses type switch for O(1) dispatch instead of O(n) handler iteration.
// Note: Does NOT handle *ssa.Defer (defers are processed in a second pass).
func DispatchInstruction(instr ssa.Instruction, ctx *HandlerContext) {
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
